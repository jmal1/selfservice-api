package synthetic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSerializeResults_ContainsRequiredFamilies pins the exposition format:
// alerts subscribe to crucible_synthetic_check_success by name and the
// pushgateway requires a TYPE line for each family. Any change here is a
// monitoring breaking change.
func TestSerializeResults_ContainsRequiredFamilies(t *testing.T) {
	body := string(serializeResults([]Result{
		{Name: "healthz", Severity: SeverityCritical, Success: true, Duration: 123 * time.Millisecond, HTTPStatus: 200},
		{Name: "auth_me", Severity: SeverityWarning, Success: false, Duration: 2 * time.Second, HTTPStatus: 500},
	}))

	required := []string{
		"# TYPE crucible_synthetic_check_success gauge",
		"# TYPE crucible_synthetic_check_duration_seconds gauge",
		"# TYPE crucible_synthetic_check_http_status gauge",
		"# TYPE crucible_synthetic_run_timestamp_seconds gauge",
		`crucible_synthetic_check_success{check="healthz",severity="critical"} 1`,
		`crucible_synthetic_check_success{check="auth_me",severity="warning"} 0`,
		`crucible_synthetic_check_http_status{check="healthz",severity="critical"} 200`,
		`crucible_synthetic_check_http_status{check="auth_me",severity="warning"} 500`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q.\nbody:\n%s", want, body)
		}
	}
}

// TestSerializeResults_EmptyBatch keeps a TYPE+HELP line per family so
// Pushgateway still accepts the push when no checks are configured (a corner
// case that would otherwise produce a 400 and confusing alerts).
func TestSerializeResults_EmptyBatch(t *testing.T) {
	body := string(serializeResults(nil))
	for _, want := range []string{
		"# TYPE crucible_synthetic_check_success gauge",
		"# TYPE crucible_synthetic_run_timestamp_seconds gauge",
		"crucible_synthetic_run_timestamp_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in empty-batch body", want)
		}
	}
}

// TestPushgateway_PushResults_PostsCorrectURLAndContentType exercises the
// happy path against a fake server that mimics Pushgateway's URL contract.
func TestPushgateway_PushResults_PostsCorrectURLAndContentType(t *testing.T) {
	var (
		gotMethod      string
		gotURL         string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotURL = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pg := NewPushgateway(srv.URL, "crucible_synthetic_api", map[string]string{
		"layer": "api",
		"env":   "prod",
	})
	pg.HTTP = srv.Client()

	err := pg.PushResults(context.Background(), []Result{
		{Name: "healthz", Severity: SeverityCritical, Success: true, HTTPStatus: 200, Duration: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	// URL must include the job AND the grouping labels in sorted order:
	// /metrics/job/{job}/env/prod/layer/api  (env < layer alphabetically)
	wantURL := "/metrics/job/crucible_synthetic_api/env/prod/layer/api"
	if gotURL != wantURL {
		t.Errorf("url = %q, want %q", gotURL, wantURL)
	}
	if !strings.HasPrefix(gotContentType, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", gotContentType)
	}
	if !strings.Contains(string(gotBody), "crucible_synthetic_check_success") {
		t.Errorf("body missing expected metric:\n%s", gotBody)
	}
}

// TestPushgateway_PushResults_ErrorsOnNon2xx ensures we surface Pushgateway
// rejections instead of silently dropping them. This is exactly the bug that
// hid behind the manual smoke test until I added a TYPE line.
func TestPushgateway_PushResults_ErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("bad request"))
	}))
	defer srv.Close()
	pg := NewPushgateway(srv.URL, "j", nil)
	pg.HTTP = srv.Client()
	err := pg.PushResults(context.Background(), []Result{{Name: "x", Severity: SeverityWarning}})
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad request") {
		t.Errorf("error = %q, want contains '400' and 'bad request'", err.Error())
	}
}

// TestPushgateway_pushURL_DeterministicOrdering proves the sorted-key
// invariant: any push for the same {job,grouping} must hit the same URL or
// Pushgateway treats them as distinct groupings (stale metrics live forever).
func TestPushgateway_pushURL_DeterministicOrdering(t *testing.T) {
	a := NewPushgateway("http://x", "j", map[string]string{"b": "2", "a": "1", "c": "3"}).pushURL()
	b := NewPushgateway("http://x", "j", map[string]string{"c": "3", "a": "1", "b": "2"}).pushURL()
	if a != b {
		t.Fatalf("URL not deterministic: %q vs %q", a, b)
	}
	if !strings.Contains(a, "/a/1/b/2/c/3") {
		t.Errorf("URL = %q, want sorted segments /a/1/b/2/c/3", a)
	}
}
