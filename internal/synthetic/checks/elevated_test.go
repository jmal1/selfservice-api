package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// byName finds an elevated check by its metric name so tests do not depend on
// the slice order returned by Elevated().
func byName(t *testing.T, all []synthetic.Check, name string) synthetic.Check {
	t.Helper()
	for _, c := range all {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("elevated check %q not found (have %d checks)", name, len(all))
	return nil
}

func elevatedFor(t *testing.T, url, name string) synthetic.Check {
	t.Helper()
	return byName(t, Elevated(ElevatedConfig{Client: synthetic.NewClient(url, "")}), name)
}

// poisonClient points at a server that fails every request with a status no
// elevated check ever accepts. Elevated checks must ignore it entirely — see
// TestElevated_UsesConfiguredClientNotRunners.
//
// It deliberately does NOT 404: template_wizard_state_404 *wants* a 404, so a
// default httptest NotFound handler would let that one check pass against the
// wrong client and quietly hollow out the guard.
func poisonClient(t *testing.T) *synthetic.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "poison: this is the runner's student client", http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	return synthetic.NewClient(srv.URL, "student-cookie-must-not-be-used")
}

// TestElevated_UsesConfiguredClientNotRunners is the load-bearing test for the
// whole design. The Runner passes every check the STUDENT client; elevated
// checks must use the instructor client captured in their config instead. If
// a future refactor "helpfully" switches them back to the passed-in client,
// every elevated check would 403 in production — but unit tests that pass the
// same client for both would still be green. So the two clients here point at
// deliberately different servers.
func TestElevated_UsesConfiguredClientNotRunners(t *testing.T) {
	good := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images":       func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) },
		"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"isos":[]}`)) },
		"/api/v1/admin/templates/00000000-0000-0000-0000-000000000000/wizard-state": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"/api/v1/admin/runs": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) },
		"/api/v1/admin/blueprints/00000000-0000-0000-0000-000000000000/vm-playlists": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	all := Elevated(ElevatedConfig{Client: synthetic.NewClient(good.URL, "instructor-cookie")})
	if len(all) != 5 {
		t.Fatalf("Elevated() returned %d checks, want 5", len(all))
	}
	for _, c := range all {
		if _, err := c.Run(context.Background(), poisonClient(t)); err != nil {
			t.Errorf("%s used the runner's client instead of the configured one: %v", c.Name(), err)
		}
	}
}

// TestElevated_NilClientFailsLoudly pins the deliberate refusal to fall back
// to the runner's student client. A fallback would report "403 want 200",
// pointing an operator at the endpoint rather than at the missing env var.
func TestElevated_NilClientFailsLoudly(t *testing.T) {
	for _, c := range Elevated(ElevatedConfig{}) {
		status, err := c.Run(context.Background(), poisonClient(t))
		if err == nil {
			t.Fatalf("%s: expected an error with no instructor client", c.Name())
		}
		if status != 0 {
			t.Errorf("%s: status = %d, want 0 (no request should have been made)", c.Name(), status)
		}
		if !strings.Contains(err.Error(), "SYNTHETIC_INSTRUCTOR_USER_ID") {
			t.Errorf("%s: error %q should name the env var to set", c.Name(), err)
		}
	}
}

func TestImageListContract_Happy(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `[{"id":"x"}]`} {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
		})
		if _, err := elevatedFor(t, srv.URL, "image_list_contract").Run(context.Background(), nil); err != nil {
			t.Errorf("body %s: %v", body, err)
		}
	}
}

// TestImageListContract_FailsOn503 is the dead-wiring guard: a 503 means the
// object store was never attached in cmd/api-gateway, which looks identical
// to a healthy deploy from every other angle.
func TestImageListContract_FailsOn503(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "image upload not configured", http.StatusServiceUnavailable)
		},
	})
	_, err := elevatedFor(t, srv.URL, "image_list_contract").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected failure on 503")
	}
	if !strings.Contains(err.Error(), "not wired") {
		t.Errorf("error %q should name the wiring failure, not just the status", err)
	}
}

func TestImageListContract_FailsOnObjectResponse(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"images":[]}`)) },
	})
	if _, err := elevatedFor(t, srv.URL, "image_list_contract").Run(context.Background(), nil); err == nil {
		t.Fatal("expected failure when the response shape changes to an object")
	}
}

func TestImageListContract_FailsOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/images": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) },
	})
	_, err := elevatedFor(t, srv.URL, "image_list_contract").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected failure on 403")
	}
	if !strings.Contains(err.Error(), "instructor") {
		t.Errorf("error %q should point at the instructor identity", err)
	}
}

func TestISOCatalogReachable_Happy(t *testing.T) {
	// Empty isos list: no entries to validate source field on, so this passes.
	for _, body := range []string{
		`{"isos":[]}`,
		`{"isos":null}`,
		`{"isos":[{"source":"datastore","name":"kali.iso","path":"[NAS] ISOs/kali.iso"}]}`,
	} {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) },
		})
		if _, err := elevatedFor(t, srv.URL, "iso_catalog_reachable").Run(context.Background(), nil); err != nil {
			t.Errorf("body %s: %v", body, err)
		}
	}
}

// TestISOCatalogReachable_DistinguishesFailureModes matters because the fix
// differs completely: 502 is a vCenter credential problem, 503 is a wiring
// problem. An operator paged at 3am should not have to guess.
func TestISOCatalogReachable_DistinguishesFailureModes(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{http.StatusBadGateway, "vCenter rejected"},
		{http.StatusServiceUnavailable, "not wired"},
	}
	for _, tc := range cases {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code) },
		})
		_, err := elevatedFor(t, srv.URL, "iso_catalog_reachable").Run(context.Background(), nil)
		if err == nil {
			t.Fatalf("status %d: expected failure", tc.code)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: error %q should contain %q", tc.code, err, tc.want)
		}
	}
}

// TestISOCatalogReachable_FailsWhenSourceFieldMissing verifies that the check
// catches a regression where the merged ISO list drops the 'source' field from
// an entry. Without 'source', the wizard cannot distinguish uploaded from
// datastore ISOs.
func TestISOCatalogReachable_FailsWhenSourceFieldMissing(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"isos":[{"name":"kali.iso","path":"[NAS] ISOs/kali.iso"}]}`))
		},
	})
	_, err := elevatedFor(t, srv.URL, "iso_catalog_reachable").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected failure when 'source' field is absent from an ISO entry")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("error %q should mention the missing 'source' field", err)
	}
}

func TestTemplateWizardState404_PassesOn404(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/templates/00000000-0000-0000-0000-000000000000/wizard-state": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	status, err := elevatedFor(t, srv.URL, "template_wizard_state_404").Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestTemplateWizardState404_FailsLoudlyOn500(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/templates/00000000-0000-0000-0000-000000000000/wizard-state": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "internal error", http.StatusInternalServerError)
		},
	})
	_, err := elevatedFor(t, srv.URL, "template_wizard_state_404").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected failure on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q should name the status", err)
	}
}

// TestTemplateWizardState404_FailsOn403 guards the reason this check needs the
// elevated identity at all: as a student it would return 403 forever and pass
// nothing through to the handler it exists to watch.
func TestTemplateWizardState404_FailsOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/templates/00000000-0000-0000-0000-000000000000/wizard-state": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		},
	})
	_, err := elevatedFor(t, srv.URL, "template_wizard_state_404").Run(context.Background(), nil)
	if err == nil {
		t.Fatal("a 403 must fail: the check would otherwise be a tautology")
	}
	if !strings.Contains(err.Error(), "no longer reaching the handler") {
		t.Errorf("error %q should explain that the check has stopped proving anything", err)
	}
}

// TestElevated_HasFriendlyMetadata mirrors TestAll_HasFriendlyMetadata, which
// only walks All() and therefore never sees the config-constructed checks.
func TestElevated_HasFriendlyMetadata(t *testing.T) {
	for _, c := range Elevated(ElevatedConfig{Client: synthetic.NewClient("http://x", "")}) {
		if c.Title() == "" || c.Description() == "" {
			t.Errorf("%s: missing Title/Description", c.Name())
		}
		if c.Severity() != synthetic.SeverityCritical && c.Severity() != synthetic.SeverityWarning {
			t.Errorf("%s: severity %q is not a routable value", c.Name(), c.Severity())
		}
		for _, bad := range []string{",", `"`, "\n"} {
			if strings.Contains(c.Title()+c.Description(), bad) {
				t.Errorf("%s: metadata contains %q, which corrupts Prometheus exposition labels", c.Name(), bad)
			}
		}
	}
}

// TestElevated_StableNames pins the metric names. Alert rules and dashboard
// variables reference them; a rename is a monitoring-breaking change that
// nothing else in the build would catch, because these checks are built by a
// constructor and never appear in All().
func TestElevated_StableNames(t *testing.T) {
	want := map[string]bool{
		"image_list_contract":             true,
		"iso_catalog_reachable":           true,
		"template_wizard_state_404":       true,
		"admin_runs_filter_contract":      true,
		"blueprint_vm_playlists_contract": true,
	}
	for _, c := range Elevated(ElevatedConfig{Client: synthetic.NewClient("http://x", "")}) {
		if !want[c.Name()] {
			t.Errorf("unexpected elevated check name %q (rename or update alert rules + runbooks)", c.Name())
		}
		delete(want, c.Name())
	}
	for missing := range want {
		t.Errorf("expected elevated check %q not returned", missing)
	}
}

// -- elevated_identity_configured ------------------------------------------
//
// This meta-check exists because a check that DISAPPEARS is invisible.
// PushResults POSTs the whole crucible_synthetic_check_success family and
// Pushgateway replaces a family wholesale, so a skipped elevated check leaves
// no series at all -- and `1 - crucible_synthetic_check_success > 0` cannot
// match a series that does not exist. These tests pin both directions.

func TestElevatedIdentityConfigured_PassesWhenClientPresent(t *testing.T) {
	c := ElevatedIdentityConfigured(ElevatedConfig{Client: synthetic.NewClient("http://x", "")})
	if _, err := c.Run(context.Background(), nil); err != nil {
		t.Fatalf("want pass with a configured client, got %v", err)
	}
}

func TestElevatedIdentityConfigured_FailsWhenClientMissing(t *testing.T) {
	c := ElevatedIdentityConfigured(ElevatedConfig{})
	_, err := c.Run(context.Background(), nil)
	if err == nil {
		t.Fatal("want failure when no instructor identity is configured; a green series here " +
			"means the admin-surface checks are absent and NOTHING is alerting on it")
	}
	// The message must say the checks are absent rather than failing: an
	// on-call who reads "3 checks failing" looks at the API, while one who
	// reads "not running" looks at the secret.
	if !strings.Contains(err.Error(), "DISABLED") || !strings.Contains(err.Error(), "absent") {
		t.Errorf("error must distinguish absent-from-failing, got: %v", err)
	}
}

// TestElevatedIdentityConfigured_IsRegisteredUnconditionally guards the actual
// mistake: registering the meta-check inside the same `if instructorClient !=
// nil` branch as the checks it reports on. That would make it vanish in
// exactly the situation it exists to detect -- a monitor for a missing thing
// that goes missing along with it.
func TestElevatedIdentityConfigured_IsRegisteredUnconditionally(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "cmd", "synthetic-api-monitor", "main.go"))
	if err != nil {
		t.Fatalf("read monitor main: %v", err)
	}
	body := string(src)

	call := strings.Index(body, "checks.ElevatedIdentityConfigured(")
	if call < 0 {
		t.Fatal("cmd/synthetic-api-monitor/main.go never calls checks.ElevatedIdentityConfigured; " +
			"losing the instructor identity would silently drop 3 checks with no alert")
	}
	guard := strings.Index(body, "if instructorClient != nil {")
	if guard < 0 {
		t.Fatal("expected the instructorClient guard in main.go; update this test if it was restructured")
	}
	if call > guard {
		t.Error("checks.ElevatedIdentityConfigured must be registered BEFORE (and outside) the " +
			"`if instructorClient != nil` guard. Inside it, the meta-check disappears in precisely " +
			"the case it is meant to report, which is how this blind spot was created originally.")
	}
}

func TestElevatedIdentityConfigured_Metadata(t *testing.T) {
	c := ElevatedIdentityConfigured(ElevatedConfig{})
	if c.Name() != "elevated_identity_configured" {
		t.Errorf("name %q is referenced by runbooks and dashboards; do not rename casually", c.Name())
	}
	// Warning, not critical: lost coverage is a weekday fix, and a fresh
	// environment that has not run the bootstrap SQL must not page anyone.
	if c.Severity() != synthetic.SeverityWarning {
		t.Errorf("severity = %q, want warning", c.Severity())
	}
	if c.Title() == "" || c.Description() == "" {
		t.Error("missing Title/Description")
	}
	for _, bad := range []string{",", "\"", "\n"} {
		if strings.Contains(c.Title()+c.Description(), bad) {
			t.Errorf("metadata contains %q, which corrupts Prometheus exposition labels", bad)
		}
	}
}

// -- admin_runs_filter_contract --------------------------------------------
//
// This check shipped on the student client and asserted 200 on an
// instructor-gated route, so in production it observed only the RBAC
// middleware's 403: red on the first cycle, and incapable of ever going green.
// These tests pin the contract now that it runs with the instructor identity.

func elevatedCfg(url string) ElevatedConfig {
	return ElevatedConfig{Client: synthetic.NewClient(url, "")}
}

func TestAdminRunsFilterContract_PassesOnEmptyArray(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/runs": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`[]`))
		},
	})
	status, err := adminRunsFilterContract(elevatedCfg(srv.URL)).Run(context.Background(), nil)
	if err != nil || status != 200 {
		t.Fatalf("empty array with 200 should pass: status=%d err=%v", status, err)
	}
}

func TestAdminRunsFilterContract_FailsOn500(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/runs": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
	})
	if _, err := adminRunsFilterContract(elevatedCfg(srv.URL)).Run(context.Background(), nil); err == nil {
		t.Fatal("a 500 from admin/runs filter MUST fail")
	}
}

// The assertion that distinguishes "filter works" from "filter parsed and
// ignored". A 200-only check would pass in both cases, and the ignored-filter
// case leaks every run in the system to a caller who asked for one user's.
func TestAdminRunsFilterContract_FailsOnIgnoredFilter(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/runs": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`[{"id":"fake"}]`))
		},
	})
	if _, err := adminRunsFilterContract(elevatedCfg(srv.URL)).Run(context.Background(), nil); err == nil {
		t.Fatal("a non-empty array when filtering should fail (the filter is being ignored)")
	}
}

// The regression guard for the bug being fixed: if this check is ever moved
// back onto the student client, it sees 403. That must fail loudly, and the
// message must point at the identity rather than at the endpoint.
func TestAdminRunsFilterContract_FailsOn403WithIdentityHint(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/runs": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) },
	})
	_, err := adminRunsFilterContract(elevatedCfg(srv.URL)).Run(context.Background(), nil)
	if err == nil {
		t.Fatal("403 must fail: the check would be running as the wrong identity")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("403 error should point at the identity, not the endpoint; got: %v", err)
	}
}

// -- blueprint_vm_playlists_contract ---------------------------------------
//
// This one is the subtler failure: it accepted "404 or 403" and, as a student,
// received 403 every cycle. It passed continuously while proving nothing --
// the request never reached the handler, so the nil-deref it exists to catch
// was structurally unreachable. Past the gate, 403 is not an acceptable answer.

func TestBlueprintVMPlaylistsContract_PassesOn404(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/blueprints/00000000-0000-0000-0000-000000000000/vm-playlists": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
		},
	})
	status, err := blueprintVMPlaylistsContract(elevatedCfg(srv.URL)).Run(context.Background(), nil)
	if err != nil || status != 404 {
		t.Fatalf("404 should pass: status=%d err=%v", status, err)
	}
}

func TestBlueprintVMPlaylistsContract_FailsOn500(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/blueprints/00000000-0000-0000-0000-000000000000/vm-playlists": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
		},
	})
	if _, err := blueprintVMPlaylistsContract(elevatedCfg(srv.URL)).Run(context.Background(), nil); err == nil {
		t.Fatal("a 500 is the nil-deref this check exists to catch and MUST fail")
	}
}

// The tautology guard. Before this change a 403 counted as a pass, so the
// check was green for its entire life while testing nothing.
func TestBlueprintVMPlaylistsContract_FailsOn403(t *testing.T) {
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/blueprints/00000000-0000-0000-0000-000000000000/vm-playlists": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
		},
	})
	_, err := blueprintVMPlaylistsContract(elevatedCfg(srv.URL)).Run(context.Background(), nil)
	if err == nil {
		t.Fatal("403 must fail: it means the request never reached the handler, so the check proves nothing")
	}
}
