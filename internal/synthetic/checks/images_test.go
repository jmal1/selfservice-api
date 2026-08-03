package checks

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

func TestImageUploadRBAC_PassesOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			w.WriteHeader(http.StatusForbidden)
		},
	})
	status, err := ImageUploadRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
}

// TestImageUploadRBAC_FailsWhenStudentAllowed is the reason this check exists:
// a 201 means the role gate stopped running.
func TestImageUploadRBAC_FailsWhenStudentAllowed(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted} {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) },
		})
		_, err := ImageUploadRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
		if err == nil {
			t.Fatalf("status %d: expected failure when a student is allowed to upload", code)
		}
		if !strings.Contains(err.Error(), "ALLOWED") {
			t.Errorf("status %d: error %q should name the RBAC gate", code, err)
		}
	}
}

// TestImageUploadRBAC_FailsOn404 guards against the check silently becoming a
// tautology if the route is removed or renamed: a 404 is "not 403", but it is
// also "this check now proves nothing", and the message must say so.
func TestImageUploadRBAC_FailsOn404(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){})
	_, err := ImageUploadRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected failure when the route is missing")
	}
	if !strings.Contains(err.Error(), "route is missing") {
		t.Errorf("error %q should distinguish a missing route from a permission failure", err)
	}
}

// TestImageUploadRBAC_SendsWellFormedBody pins the deliberate choice of a
// valid payload. A malformed body would let a 400 stand in for a 403 if the
// middleware ever stopped running, which is the exact regression the check
// exists to detect.
func TestImageUploadRBAC_SendsWellFormedBody(t *testing.T) {
	var got string
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 512)
			n, _ := r.Body.Read(buf)
			got = string(buf[:n])
			w.WriteHeader(http.StatusForbidden)
		},
	})
	if _, err := ImageUploadRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, want := range []string{`"filename"`, `"kind"`, `"size_bytes"`} {
		if !strings.Contains(got, want) {
			t.Errorf("request body %q missing %s", got, want)
		}
	}
}
