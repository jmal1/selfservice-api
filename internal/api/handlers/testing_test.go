package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

func requestWithRouteParams(t *testing.T, url string, params map[string]string) *http.Request {
	t.Helper()
	ctx := chi.NewRouteContext()
	for key, value := range params {
		ctx.URLParams.Add(key, value)
	}
	return httptest.NewRequest(http.MethodGet, url, nil).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, ctx))
}

func TestValidateTestingRunAccess(t *testing.T) {
	ownerID := uuid.New()
	otherID := uuid.New()

	tests := []struct {
		name       string
		podOwnerID uuid.UUID
		userID     uuid.UUID
		role       string
		want       int
	}{
		{
			name:       "student own allowed",
			podOwnerID: ownerID,
			userID:     ownerID,
			role:       models.RoleStudent,
			want:       0,
		},
		{
			name:       "student other denied",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleStudent,
			want:       http.StatusForbidden,
		},
		{
			name:       "instructor other allowed",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleInstructor,
			want:       0,
		},
		{
			name:       "admin other allowed",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleAdmin,
			want:       0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateTestingRunAccess(tc.podOwnerID, tc.userID, tc.role); got != tc.want {
				t.Fatalf("validateTestingRunAccess() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLifecycleIDValidationReturns400BeforeDBLookup(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil)
	cases := []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{name: "delete vm", call: h.DeleteVM, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid"})},
		{name: "power action", call: h.VMPowerAction, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/start", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid"})},
		{name: "list snapshots", call: h.ListVMSnapshots, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/snapshots", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid"})},
		{name: "create snapshot", call: h.CreateVMSnapshot, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/snapshots", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid"})},
		{name: "revert initial", call: h.RevertToInitial, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/revert", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid"})},
		{name: "revert snapshot", call: h.RevertToSnapshot, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/snapshots/not-a-uuid/revert", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid", "snapID": "not-a-uuid"})},
		{name: "delete snapshot", call: h.DeleteVMSnapshot, req: requestWithRouteParams(t, "/pods/not-a-uuid/vms/not-a-uuid/snapshots/not-a-uuid", map[string]string{"podID": "not-a-uuid", "vmID": "not-a-uuid", "snapID": "not-a-uuid"})},
		{name: "list testing runs", call: h.ListTestingRuns, req: requestWithRouteParams(t, "/pods/not-a-uuid/testing", map[string]string{"podID": "not-a-uuid"})},
		{name: "get testing run", call: h.GetTestingRun, req: requestWithRouteParams(t, "/pods/not-a-uuid/testing/runs/not-a-uuid", map[string]string{"podID": "not-a-uuid", "runID": "not-a-uuid"})},
		{name: "cancel testing run", call: h.CancelTestingRun, req: requestWithRouteParams(t, "/pods/not-a-uuid/testing/runs/not-a-uuid/cancel", map[string]string{"podID": "not-a-uuid", "runID": "not-a-uuid"})},
		{name: "get blueprint", call: h.GetBlueprint, req: requestWithRouteParams(t, "/blueprints/not-a-uuid", map[string]string{"blueprintID": "not-a-uuid"})},
		{name: "get job status", call: h.GetJobStatus, req: requestWithRouteParams(t, "/jobs/not-a-uuid", map[string]string{"jobID": "not-a-uuid"})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, tc.req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
			}
		})
	}
}

func TestTestingRunAccessMatchesOwnerAndInstructorPolicy(t *testing.T) {
	ownerID := uuid.New()
	otherID := uuid.New()
	for _, tc := range []struct {
		name string
		want int
		role string
	}{
		{name: "owner student", want: 0, role: models.RoleStudent},
		{name: "other student", want: http.StatusForbidden, role: models.RoleStudent},
		{name: "other instructor", want: 0, role: models.RoleInstructor},
		{name: "other admin", want: 0, role: models.RoleAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userID := ownerID
			if tc.name == "other student" || tc.name == "other instructor" || tc.name == "other admin" {
				userID = otherID
			}
			if got := validateTestingRunAccess(ownerID, userID, tc.role); got != tc.want {
				t.Fatalf("validateTestingRunAccess() = %d, want %d", got, tc.want)
			}
		})
	}
}
