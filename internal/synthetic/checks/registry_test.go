package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// newFakeAPI builds an httptest server that pretends to be the Crucible API
// with route-level handlers parameterized per test.
func newFakeAPI(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthz_Happy(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/healthz": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{"status":"ok"}`))
		},
	})
	status, err := Healthz.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
}

func TestHealthz_FailsOn500(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/healthz": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
	})
	status, err := Healthz.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected failure on 500, got nil")
	}
	if status != 500 {
		t.Errorf("status = %d, want 500 (failure case must still report the real status)", status)
	}
}

func TestHealthz_FailsOnWrongBody(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/healthz": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"status":"degraded"}`))
		},
	})
	_, err := Healthz.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "degraded") {
		t.Fatalf("expected wrong-status error, got %v", err)
	}
}

func TestAuthMe_Happy(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/auth/me": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"user":{"username":"synthetic","role":"student"},"resource_usage":{}}`))
		},
	})
	status, err := AuthMe.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
}

func TestAuthMe_FailsOnEmptyUsername(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/auth/me": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"user":{"username":""}}`))
		},
	})
	_, err := AuthMe.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "empty username") {
		t.Fatalf("expected empty-username error, got %v", err)
	}
}

func TestPodsList_AcceptsEmptyArray(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/pods": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`[]`))
		},
	})
	if _, err := PodsList.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("empty array should pass: %v", err)
	}
}

func TestPodsList_FailsOnObjectResponse(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/pods": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.Write([]byte(`{"pods":[]}`))
		},
	})
	if _, err := PodsList.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err == nil {
		t.Fatal("expected failure on object response (would indicate API contract regression)")
	}
}

func TestAdminListUsers403_PassesOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
	})
	status, err := AdminListUsers403.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil || status != 403 {
		t.Fatalf("403 should pass: status=%d err=%v", status, err)
	}
}

func TestAdminListUsers403_FailsOn200(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/users": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) },
	})
	_, err := AdminListUsers403.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a 200 from an admin endpoint as a student MUST fail — that is the entire point of this check")
	}
}

func TestPodTestingDashboard404_AcceptsBothExpectedCodes(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
				"/api/v1/pods/00000000-0000-0000-0000-000000000000/testing": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(code)
				},
			})
			if _, err := PodTestingDashboard404.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
				t.Fatalf("%d should pass: %v", code, err)
			}
		})
	}
}

func TestPodTestingDashboard404_FailsLoudlyOn500(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/pods/00000000-0000-0000-0000-000000000000/testing": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		},
	})
	_, err := PodTestingDashboard404.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("500 must fail this check, got %v", err)
	}
}

func TestWikiIndexRBAC_PassesOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/wiki/index": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
	})
	status, err := WikiIndexRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil || status != 403 {
		t.Fatalf("403 should pass: status=%d err=%v", status, err)
	}
}

func TestWikiIndexRBAC_FailsOn200(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/wiki/index": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{"seeds":[],"files":[],"total_bytes":0}`))
		},
	})
	_, err := WikiIndexRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a 200 from /wiki/index as a student MUST fail — that is the entire point of this check")
	}
}

func TestAll_StableNames(t *testing.T) {
	// Alert rules reference check names; renames are monitoring breaking
	// changes. If you intentionally rename a check, update the alert YAML in
	// the Grafana provisioning then update this test.
	wantNames := map[string]bool{
		"healthz":                   true,
		"auth_me":                   true,
		"pods_list":                 true,
		"admin_list_users_403":      true,
		"admin_audit_403":           true,
		"pod_testing_dashboard_404": true,
		"wiki_index_rbac":           true,
	}
	for _, c := range All() {
		if !wantNames[c.Name()] {
			t.Errorf("unexpected check name %q (rename or add to wantNames+alert rules)", c.Name())
		}
		delete(wantNames, c.Name())
	}
	for missing := range wantNames {
		t.Errorf("expected check %q not registered", missing)
	}
}

// TestAll_HasFriendlyMetadata guarantees every check is dashboard-ready:
// non-empty Title (for status pills + alert summaries) and Description (for
// status table). A check that ships without either is invisible to humans.
func TestAll_HasFriendlyMetadata(t *testing.T) {
	for _, c := range All() {
		if strings.TrimSpace(c.Title()) == "" {
			t.Errorf("check %q has empty Title()", c.Name())
		}
		if strings.TrimSpace(c.Description()) == "" {
			t.Errorf("check %q has empty Description()", c.Name())
		}
		if len(c.Title()) > 60 {
			t.Errorf("check %q Title() = %q is too long (>60 chars; keep it pill-sized)", c.Name(), c.Title())
		}
	}
}
