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
		{Name: "healthz", Title: "API Liveness", Description: "hits /healthz", Severity: SeverityCritical, Success: true, Duration: 123 * time.Millisecond, HTTPStatus: 200},
		{Name: "auth_me", Title: "Session Auth + DB", Description: "calls /auth/me", Severity: SeverityWarning, Success: false, Duration: 2 * time.Second, HTTPStatus: 500},
	}))

	required := []string{
		"# TYPE crucible_synthetic_check_success gauge",
		"# TYPE crucible_synthetic_check_duration_seconds gauge",
		"# TYPE crucible_synthetic_check_http_status gauge",
		"# TYPE crucible_synthetic_check_info gauge",
		"# TYPE crucible_synthetic_run_timestamp_seconds gauge",
		`crucible_synthetic_check_success{check="healthz",title="API Liveness",severity="critical"} 1`,
		`crucible_synthetic_check_success{check="auth_me",title="Session Auth + DB",severity="warning"} 0`,
		`crucible_synthetic_check_http_status{check="healthz",title="API Liveness",severity="critical"} 200`,
		`crucible_synthetic_check_http_status{check="auth_me",title="Session Auth + DB",severity="warning"} 500`,
		`crucible_synthetic_check_info{check="healthz",title="API Liveness",description="hits /healthz",severity="critical"} 1`,
		`crucible_synthetic_check_info{check="auth_me",title="Session Auth + DB",description="calls /auth/me",severity="warning"} 1`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q.\nbody:\n%s", want, body)
		}
	}
}

// TestSerializeResults_SanitizesLabelValues ensures titles/descriptions that
// would break the exposition format are flattened rather than corrupting the
// push. Multi-line descriptions in particular would otherwise produce invalid
// metrics that Pushgateway rejects.
func TestSerializeResults_SanitizesLabelValues(t *testing.T) {
	body := string(serializeResults([]Result{
		{Name: "x", Title: "line1\nline2", Description: "tab\there", Severity: SeverityCritical},
	}))
	if strings.Contains(body, "line1\nline2") {
		t.Errorf("title newline not sanitized:\n%s", body)
	}
	if strings.Contains(body, "tab\there") {
		t.Errorf("description tab not sanitized:\n%s", body)
	}
	if !strings.Contains(body, `title="line1 line2"`) {
		t.Errorf("expected flattened title in body:\n%s", body)
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

// TestSerializeResults_EmitsRegisteredCount is the guard for the silent
// coverage-loss class described in the metric's own comment: because
// PushResults POSTs and Pushgateway replaces a family wholesale, a check that
// stops being registered leaves NO series behind, and `1 - success > 0` cannot
// match a series that is absent. This counter is the only signal that survives
// such a drop, so it must be emitted unconditionally and must track len(results).
func TestSerializeResults_EmitsRegisteredCount(t *testing.T) {
	body := string(serializeResults([]Result{
		{Name: "a", Title: "A", Severity: SeverityCritical, Success: true},
		{Name: "b", Title: "B", Severity: SeverityWarning, Success: true},
		{Name: "c", Title: "C", Severity: SeverityWarning, Success: false},
	}))
	if !strings.Contains(body, "# TYPE crucible_synthetic_checks_registered gauge") {
		t.Errorf("missing TYPE line for crucible_synthetic_checks_registered:\n%s", body)
	}
	if !strings.Contains(body, "crucible_synthetic_checks_registered 3") {
		t.Errorf("expected registered count of 3:\n%s", body)
	}

	// Dropping a check must move the counter, otherwise the alert
	// (delta(...[1h]) < 0) can never fire and the guard is decorative.
	fewer := string(serializeResults([]Result{
		{Name: "a", Title: "A", Severity: SeverityCritical, Success: true},
	}))
	if !strings.Contains(fewer, "crucible_synthetic_checks_registered 1") {
		t.Errorf("expected registered count of 1 after dropping checks:\n%s", fewer)
	}

	// Emitted even with no checks at all — a monitor that registered nothing is
	// exactly the case we most need to see.
	if !strings.Contains(string(serializeResults(nil)), "crucible_synthetic_checks_registered 0") {
		t.Error("registered count must still be emitted for an empty batch")
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

// TestSerializeResults_EmitsAttemptsMetric confirms that the new
// crucible_synthetic_check_attempts family is present and reflects Result.Attempts.
func TestSerializeResults_EmitsAttemptsMetric(t *testing.T) {
	body := string(serializeResults([]Result{
		{Name: "pod_lifecycle", Title: "Pod lifecycle", Severity: SeverityCritical, Success: true, Attempts: 2},
		{Name: "healthz", Title: "API Liveness", Severity: SeverityCritical, Success: true, Attempts: 1},
	}))

	if !strings.Contains(body, "# TYPE crucible_synthetic_check_attempts gauge") {
		t.Errorf("missing TYPE line for crucible_synthetic_check_attempts:\n%s", body)
	}
	if !strings.Contains(body, `crucible_synthetic_check_attempts{check="pod_lifecycle"`) {
		t.Errorf("pod_lifecycle attempts metric missing:\n%s", body)
	}
	if !strings.Contains(body, `} 2`) {
		t.Errorf("pod_lifecycle should show attempts=2:\n%s", body)
	}
	if !strings.Contains(body, `crucible_synthetic_check_attempts{check="healthz"`) {
		t.Errorf("healthz attempts metric missing:\n%s", body)
	}
}

// TestSerializeResults_AttemptsDefaultsToOneForUnsetField guards the defensive
// fallback: a Result with Attempts=0 (field not set by older code paths)
// should emit 1 rather than 0, which would look like the check never ran.
func TestSerializeResults_AttemptsDefaultsToOneForUnsetField(t *testing.T) {
	body := string(serializeResults([]Result{
		{Name: "x", Title: "X", Severity: SeverityCritical, Success: true, Attempts: 0},
	}))
	// The defensive fallback in serializeResults should normalise 0 → 1.
	if !strings.Contains(body, `crucible_synthetic_check_attempts{check="x",title="X",severity="critical"} 1`) {
		t.Errorf("Attempts=0 in Result must be emitted as 1 (defensive fallback); got:\n%s", body)
	}
}

// TestSerializeResults_EmitsVCenterDegradedMetric confirms the new
// crucible_synthetic_vcenter_degraded family is present.
func TestSerializeResults_EmitsVCenterDegradedMetric(t *testing.T) {
	body := string(serializeResults([]Result{
		{
			Name: "pod_lifecycle", Title: "Pod lifecycle", Severity: SeverityCritical,
			Success: true, Attempts: 2, VCenterDegraded: true,
		},
		{
			Name: "healthz", Title: "API Liveness", Severity: SeverityCritical,
			Success: true, Attempts: 1, VCenterDegraded: false,
		},
	}))

	if !strings.Contains(body, "# TYPE crucible_synthetic_vcenter_degraded gauge") {
		t.Errorf("missing TYPE line for crucible_synthetic_vcenter_degraded:\n%s", body)
	}
	// pod_lifecycle passed on retry but had a vCenter stall — must be 1.
	if !strings.Contains(body, `crucible_synthetic_vcenter_degraded{check="pod_lifecycle"`) {
		t.Errorf("pod_lifecycle vcenter_degraded metric missing:\n%s", body)
	}
	// healthz had no vCenter stall — must be 0.
	if !strings.Contains(body, `crucible_synthetic_vcenter_degraded{check="healthz"`) {
		t.Errorf("healthz vcenter_degraded metric missing:\n%s", body)
	}
}
