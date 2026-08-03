package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) },
		"/api/v1/admin/templates/00000000-0000-0000-0000-000000000000/wizard-state": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	all := Elevated(ElevatedConfig{Client: synthetic.NewClient(good.URL, "instructor-cookie")})
	if len(all) != 3 {
		t.Fatalf("Elevated() returned %d checks, want 3", len(all))
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
	srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
		"/api/v1/admin/vcenter/isos": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"files":[]}`)) },
	})
	if _, err := elevatedFor(t, srv.URL, "iso_catalog_reachable").Run(context.Background(), nil); err != nil {
		t.Fatalf("err: %v", err)
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
		"image_list_contract":       true,
		"iso_catalog_reachable":     true,
		"template_wizard_state_404": true,
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
