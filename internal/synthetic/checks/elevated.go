package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// ElevatedConfig carries an instructor-role client for the checks that must
// probe an authenticated admin surface rather than merely prove a student is
// refused.
//
// Why a second identity exists at all: the monitor mints one session JWT per
// cycle, and the production synthetic user is deliberately a STUDENT so that
// admin_list_users_403, admin_audit_403, wiki_index_rbac,
// pod_testing_dashboard_404 and image_upload_rbac all assert a real denial.
// Elevating that single user would silently turn five RBAC checks into
// tautologies — they would still pass, while proving nothing. So instead we
// mint a second cookie for a separate, dedicated instructor row
// (synthetic-instructor) whose OIDC subject cannot be produced by Authentik
// and whose pod/vCPU/RAM quotas are zero, so it can read the admin surface
// and provision nothing.
//
// See WikiIndexRBAC's docstring for the limitation this removes.
type ElevatedConfig struct {
	// Client is the instructor-role client. Elevated checks use THIS
	// client and deliberately ignore the one the Runner passes to Run(),
	// because the Runner's client is the student identity.
	Client *synthetic.Client
}

// errNoElevatedClient is returned rather than falling back to the runner's
// student client. A silent fallback would turn every elevated check into a
// "403 != 200" failure whose message pointed at the endpoint instead of at
// the misconfiguration, which is a materially worse page at 3am.
func (cfg ElevatedConfig) client() (*synthetic.Client, error) {
	if cfg.Client == nil {
		return nil, fmt.Errorf("elevated check has no instructor client configured (SYNTHETIC_INSTRUCTOR_USER_ID unset?)")
	}
	return cfg.Client, nil
}

// Elevated returns the checks that require an instructor-role identity.
//
// The caller registers these only when an instructor client is available;
// cmd/synthetic-api-monitor logs loudly and skips them otherwise, so a
// half-configured deploy shows up as "6 checks" rather than as five
// mysteriously failing ones.
func Elevated(cfg ElevatedConfig) []synthetic.Check {
	return []synthetic.Check{
		imageListContract(cfg),
		isoCatalogReachable(cfg),
		templateWizardState404(cfg),
		adminRunsFilterContract(cfg),
		blueprintVMPlaylistsContract(cfg),
	}
}

// adminRunsFilterContract asserts that /admin/runs honours an unmatched
// triggered_by filter: 200 with an empty array, not 500 and not every run.
//
// This lives here, not in All(), because /admin/runs is instructor-gated. It
// originally shipped on the student client, where it could only ever observe
// the 403 from the RBAC middleware — so it went red the moment it was deployed
// and could never have gone green, no matter how correct the endpoint was.
//
// The two failure modes it exists to catch are only reachable past the gate:
// a nil-deref 500 when the filter matches nothing, and a filter that is parsed
// then silently ignored (which returns every run in the system to a caller who
// asked for one user's). The empty-array assertion is what distinguishes them —
// a 200 alone would pass in both the correct case and the ignored-filter case.
func adminRunsFilterContract(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "admin_runs_filter_contract",
		TitleVal:       "Admin Runs Filter Contract",
		DescriptionVal: "Calls /api/v1/admin/runs as an instructor with an unmatched triggered_by filter and requires 200 plus an empty array. Catches 500s on unmatched filters and filters being parsed but silently ignored.",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			c, err := cfg.client()
			if err != nil {
				return 0, err
			}
			// A UUID that cannot belong to any user, so the correct answer is
			// unambiguously "no runs".
			const phantom = "00000000-0000-0000-0000-000000000000"
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/runs?triggered_by="+phantom, nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

			if resp.StatusCode == http.StatusForbidden {
				return resp.StatusCode, fmt.Errorf(
					"admin/runs returned 403 to the instructor identity — this check is running with the wrong client, or the instructor role lost access to /admin/runs")
			}
			if resp.StatusCode != http.StatusOK {
				return resp.StatusCode, fmt.Errorf("admin/runs with unmatched filter returned %d, want 200: %s", resp.StatusCode, snippet(body))
			}
			var arr []any
			if err := json.Unmarshal(body, &arr); err != nil {
				return resp.StatusCode, fmt.Errorf("admin/runs response is not a JSON array: %w", err)
			}
			if len(arr) != 0 {
				return resp.StatusCode, fmt.Errorf("admin/runs with unmatched filter returned %d items, want 0 — the filter is being ignored and every run is exposed", len(arr))
			}
			return resp.StatusCode, nil
		},
	}
}

// blueprintVMPlaylistsContract asserts that resolving VM playlists for a
// blueprint that does not exist returns 404, not 500.
//
// Also moved off the student client, and for a subtler reason than the runs
// check: it accepted "404 or 403", and as a student it received 403 on every
// cycle. It therefore passed continuously while proving nothing — the request
// was rejected by the RBAC middleware and never reached the handler, so the
// nil-deref in blueprint resolution that the check exists to detect was
// structurally unreachable.
//
// Past the gate, 403 is no longer an acceptable answer, so it is asserted
// strictly: 404 only.
func blueprintVMPlaylistsContract(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "blueprint_vm_playlists_contract",
		TitleVal:       "Missing Blueprint Playlists Returns 404 (not 500)",
		DescriptionVal: "Probes /admin/blueprints/{phantom-uuid}/vm-playlists as an instructor and requires 404. Watches for nil-deref bugs in blueprint playlist resolution.",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			c, err := cfg.client()
			if err != nil {
				return 0, err
			}
			const phantom = "00000000-0000-0000-0000-000000000000"
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/blueprints/"+phantom+"/vm-playlists", nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

			if resp.StatusCode == http.StatusInternalServerError {
				return resp.StatusCode, fmt.Errorf("phantom blueprint vm-playlists returned 500 (nil-deref in resolution): %s", snippet(body))
			}
			if resp.StatusCode == http.StatusForbidden {
				return resp.StatusCode, fmt.Errorf(
					"phantom blueprint vm-playlists returned 403 to the instructor identity — the request never reached the handler, so this check is proving nothing")
			}
			if resp.StatusCode != http.StatusNotFound {
				return resp.StatusCode, fmt.Errorf("phantom blueprint vm-playlists returned %d, want 404: %s", resp.StatusCode, snippet(body))
			}
			return resp.StatusCode, nil
		},
	}
}

// ElevatedIdentityConfigured is registered UNCONDITIONALLY, whether or not the
// instructor identity exists. It is the guard for this package's own blind
// spot.
//
// Skipping the elevated checks when the identity is missing keeps the monitor
// running, but it makes the resulting coverage loss INVISIBLE. PushResults
// POSTs the whole crucible_synthetic_check_success family each cycle, and
// Pushgateway replaces a family wholesale on POST, so the three elevated
// series do not go stale — they simply cease to exist. Every alert we have is
// of the form `1 - crucible_synthetic_check_success > 0`, which cannot match a
// series that is absent. The board would read a healthy 10/10 while silently
// having stopped testing the entire authenticated admin surface.
//
// A metric that disappears is strictly worse than one that goes red, because
// nothing is watching for its absence. So we emit a series that is always
// present and flips to 0 instead.
//
// The k8s secret key this depends on is reconciled from Vault by External
// Secrets Operator (creationPolicy: Owner). A `kubectl patch` of the Secret
// reports success and is then silently reverted within seconds, so "someone
// patched it back by hand" is a realistic way for this to regress.
//
// Severity is warning, not critical: losing elevated coverage is a monitoring
// regression to fix on a weekday, not a student-facing outage. A fresh
// environment that has never run the bootstrap SQL therefore gets one clearly
// named warning telling it exactly what to configure, rather than three
// confusingly absent checks.
func ElevatedIdentityConfigured(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:  "elevated_identity_configured",
		TitleVal: "Instructor Synthetic Identity Present",
		DescriptionVal: "Reports whether the monitor has an instructor-role identity to run the " +
			"authenticated admin-surface checks with. When this fails the three elevated checks " +
			"(image_list_contract / iso_catalog_reachable / template_wizard_state_404) are NOT " +
			"RUNNING AT ALL and their series are absent from Prometheus entirely -- so no other " +
			"alert can tell you the admin surface stopped being tested. Fix: confirm the " +
			"synthetic-instructor row exists (deploy/sql/synthetic-instructor-user.sql) and that " +
			"Vault holds key instructor-user-id at secret/selfservice/selfservice-synthetic-user " +
			"and that the ExternalSecret lists that key. Never kubectl-patch the Secret directly; " +
			"External Secrets Operator owns it and silently reverts the patch.",
		SeverityVal: synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			if _, err := cfg.client(); err != nil {
				return 0, fmt.Errorf("elevated checks are DISABLED: %w; the admin-surface checks are absent from Prometheus, not failing", err)
			}
			return 0, nil
		},
	}
}

// imageListContract proves the /admin/images read path actually works for
// someone allowed to use it.
//
// The specific failure this is built for is a 503. Both AdminListImages and
// AdminListVCenterISOs return "not configured" 503s when their optional
// dependency was never wired in cmd/api-gateway — the dead-wiring class that
// hit five separate lanes during this feature's development (and once in
// Helm, where values.yaml declared an objectstore block that no template
// consumed). A 503 here is indistinguishable from a healthy deploy to every
// other check in the catalog, because nothing else touches the route with
// credentials that get past the role gate.
func imageListContract(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "image_list_contract",
		TitleVal:       "Image Library Responds",
		DescriptionVal: "Lists /admin/images as an instructor and requires 200 with a JSON array. A 503 here means the object store was never wired into the API process even though the deploy looked healthy.",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			c, err := cfg.client()
			if err != nil {
				return 0, err
			}
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/images", nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

			switch resp.StatusCode {
			case http.StatusOK:
			case http.StatusServiceUnavailable:
				return resp.StatusCode, fmt.Errorf(
					"image library returned 503 — the object store is not wired into the API process (check OBJECTSTORE_* env and WithImageStore in cmd/api-gateway)")
			case http.StatusForbidden:
				return resp.StatusCode, fmt.Errorf(
					"image library returned 403 to the instructor identity — the synthetic instructor user is missing, inactive, or no longer has role=instructor")
			default:
				return resp.StatusCode, fmt.Errorf(
					"image library returned %d, want 200: %s", resp.StatusCode, snippet(body))
			}

			// A nil slice marshals to `null`, which is a legitimate empty
			// library. An object would mean the response shape changed.
			var items []json.RawMessage
			if err := json.Unmarshal(body, &items); err != nil {
				return resp.StatusCode, fmt.Errorf(
					"image library body is not a JSON array: %w (body=%s)", err, snippet(body))
			}
			return resp.StatusCode, nil
		},
	}
}

// isoCatalogReachable proves the wizard's ISO picker can still browse the
// vCenter datastore, and that the response carries the `source` field that
// the wizard relies on to distinguish pipeline-uploaded ISOs from ones found
// directly on the datastore.
//
// This is the check that would have caught the vCenter credential rotation
// that previously broke datastore browsing. The handler maps a vCenter error
// to 502 and an unwired dependency to 503, so the two failure modes are
// distinguishable from the metric alone.
func isoCatalogReachable(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "iso_catalog_reachable",
		TitleVal:       "ISO Catalog (vCenter Datastore)",
		DescriptionVal: "Browses the vCenter ISO datastore via /admin/vcenter/isos as an instructor. A 502 means vCenter rejected us (usually a rotated credential); a 503 means the lister was never wired. Also verifies the response carries an 'isos' array whose entries include the 'source' field.",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			c, err := cfg.client()
			if err != nil {
				return 0, err
			}
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/vcenter/isos", nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16384))

			switch resp.StatusCode {
			case http.StatusOK:
				// Verify the response shape: the 'isos' key must be present (not 'files'),
				// and any entries must carry the 'source' field. This catches a silent
				// regression where the handler reverts to the old response shape.
				var parsed struct {
					ISOs []map[string]any `json:"isos"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil {
					return resp.StatusCode, fmt.Errorf(
						"ISO catalog returned 200 but body is not valid JSON: %v — body: %s",
						err, snippet(body))
				}
				for i, entry := range parsed.ISOs {
					if _, hasSource := entry["source"]; !hasSource {
						return resp.StatusCode, fmt.Errorf(
							"ISO catalog entry %d is missing the 'source' field — the merged response shape may have reverted: %v",
							i, entry)
					}
				}
				return resp.StatusCode, nil
			case http.StatusBadGateway:
				return resp.StatusCode, fmt.Errorf(
					"ISO catalog returned 502 — vCenter rejected the datastore browse (rotated credential, or the datastore name drifted from NAS-BackupsAndISOS): %s", snippet(body))
			case http.StatusServiceUnavailable:
				return resp.StatusCode, fmt.Errorf(
					"ISO catalog returned 503 — the vCenter ISO lister is not wired into the API process")
			case http.StatusForbidden:
				return resp.StatusCode, fmt.Errorf(
					"ISO catalog returned 403 to the instructor identity — the synthetic instructor user is missing, inactive, or no longer has role=instructor")
			default:
				return resp.StatusCode, fmt.Errorf(
					"ISO catalog returned %d, want 200: %s", resp.StatusCode, snippet(body))
			}
		},
	}
}

// templateWizardState404 asserts a missing template yields 404, never 500.
//
// This is the same nil-deref class as pod_testing_dashboard_404, but that
// check can only be written against a route a student may reach. The wizard
// state endpoint sits behind RequireRole(instructor), so as a student it
// returns 403 before the handler ever runs — a student-token version of this
// check would pass forever while the handler panicked on every real call.
// That is exactly why it needs the elevated identity.
func templateWizardState404(cfg ElevatedConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        "template_wizard_state_404",
		TitleVal:       "Missing Template Returns 404 (not 500)",
		DescriptionVal: "Requests wizard state for a phantom template UUID as an instructor and requires 404. Watches for the nil-deref class that produced 500s for missing pods in GetTestingDashboard.",
		SeverityVal:    synthetic.SeverityCritical,
		RunFn: func(ctx context.Context, _ *synthetic.Client) (int, error) {
			c, err := cfg.client()
			if err != nil {
				return 0, err
			}
			const phantom = "00000000-0000-0000-0000-000000000000"
			resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/templates/"+phantom+"/wizard-state", nil)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

			switch resp.StatusCode {
			case http.StatusNotFound:
				return resp.StatusCode, nil
			case http.StatusInternalServerError:
				return resp.StatusCode, fmt.Errorf(
					"phantom template wizard-state returned 500 (the nil-deref bug this check watches for): %s", snippet(body))
			case http.StatusForbidden:
				return resp.StatusCode, fmt.Errorf(
					"phantom template wizard-state returned 403 — the instructor identity is broken, so this check is no longer reaching the handler it exists to guard")
			default:
				return resp.StatusCode, fmt.Errorf(
					"phantom template wizard-state returned %d, want 404: %s", resp.StatusCode, snippet(body))
			}
		},
	}
}
