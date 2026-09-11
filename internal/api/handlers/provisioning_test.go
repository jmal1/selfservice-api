package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

type provisioningMetricsSpy struct {
	routes []string
}

func (s *provisioningMetricsSpy) RecordRejected(route string) {
	s.routes = append(s.routes, route)
}

func disabledProvisioningHandler(metrics provisioningAdmissionMetrics) *Handler {
	return NewHandler(nil, nil, nil, slog.Default(), nil).WithProvisioningAdmission(false, metrics)
}

func TestProvisioningStatusContract(t *testing.T) {
	tests := []struct {
		name    string
		handler *Handler
		want    ProvisioningStatusResponse
	}{
		{
			name:    "default enabled",
			handler: NewHandler(nil, nil, nil, slog.Default(), nil),
			want:    ProvisioningStatusResponse{Enabled: true, Message: ProvisioningAvailableMessage},
		},
		{
			name:    "maintenance disabled",
			handler: disabledProvisioningHandler(nil),
			want:    ProvisioningStatusResponse{Enabled: false, Message: ProvisioningMaintenanceMessage},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.GetProvisioningStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/provisioning/status", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got ProvisioningStatusResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got != tc.want {
				t.Fatalf("response = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProvisioningMutationHandlersRejectBeforeInputOrDB(t *testing.T) {
	spy := &provisioningMetricsSpy{}
	h := disabledProvisioningHandler(spy)
	tests := []struct {
		name  string
		route string
		call  func(http.ResponseWriter, *http.Request)
		label string
	}{
		{"pod create", "/api/v1/pods", h.CreatePod, provisioningRoutePodCreate},
		{"blueprint deploy", "/api/v1/blueprints/not-a-uuid/deploy", h.DeployBlueprint, provisioningRouteBlueprintDeploy},
		{"vm add", "/api/v1/pods/not-a-uuid/vms", h.AddVM, provisioningRouteVMAdd},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader("not-json"))
			chimiddleware.RequestID(http.HandlerFunc(tc.call)).ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; gate did not run before parsing or DB access", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != ProvisioningRetryAfterSeconds {
				t.Errorf("Retry-After = %q, want %q", got, ProvisioningRetryAfterSeconds)
			}
			var body map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if body["error"] != ProvisioningMaintenanceMessage {
				t.Errorf("error = %q, want %q", body["error"], ProvisioningMaintenanceMessage)
			}
			if body["request_id"] == "" {
				t.Error("request_id is empty; maintenance errors must preserve the standard envelope")
			}
		})
	}

	if len(spy.routes) != len(tests) {
		t.Fatalf("recorded routes = %v, want one per rejection", spy.routes)
	}
	for i, tc := range tests {
		if spy.routes[i] != tc.label {
			t.Errorf("route[%d] = %q, want %q", i, spy.routes[i], tc.label)
		}
	}
}

func TestProvisioningMaintenanceDoesNotGateCleanup(t *testing.T) {
	h := disabledProvisioningHandler(nil)
	tests := []struct {
		name string
		path string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"pod delete", "/api/v1/pods/not-a-uuid", h.DeletePod},
		{"vm delete", "/api/v1/pods/not-a-uuid/vms/not-a-uuid", h.DeleteVM},
		{"vm power", "/api/v1/pods/not-a-uuid/vms/not-a-uuid/start", h.VMPowerAction},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, httptest.NewRequest(http.MethodDelete, tc.path, nil))
			if rec.Code == http.StatusServiceUnavailable {
				t.Fatal("maintenance gate blocked a cleanup/power path")
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want normal handler validation 400", rec.Code)
			}
		})
	}
}

func TestProvisioningAdmissionStopsBeforeDownstreamMiddleware(t *testing.T) {
	h := disabledProvisioningHandler(nil)
	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
		panic("downstream database middleware must not run")
	})

	for _, tc := range []struct {
		name string
		path string
	}{
		{"pod create", "/api/v1/pods"},
		{"pod create trailing slash", "/api/v1/pods/"},
		{"blueprint deploy", "/api/v1/blueprints/00000000-0000-0000-0000-000000000001/deploy"},
		{"vm add", "/api/v1/pods/00000000-0000-0000-0000-000000000001/vms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			rec := httptest.NewRecorder()
			h.ProvisioningAdmission(next).ServeHTTP(
				rec,
				httptest.NewRequest(http.MethodPost, tc.path, nil),
			)
			if called {
				t.Fatal("maintenance admission called downstream middleware")
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
		})
	}
}

func TestProvisioningAdmissionPreservesCleanupRoutes(t *testing.T) {
	h := disabledProvisioningHandler(nil)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	h.ProvisioningAdmission(next).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodDelete, "/api/v1/pods/00000000-0000-0000-0000-000000000001", nil),
	)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("cleanup request called=%t status=%d, want downstream 204", called, rec.Code)
	}
}
