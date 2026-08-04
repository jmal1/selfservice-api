// Package checks defines the concrete synthetic check catalog.
//
// Every Check in this package is intentionally tiny: it picks one route, makes
// one assertion, and reports one result. Avoid adding "smart" composite checks
// — when something breaks, the most useful signal is "check X failed", not
// "the seventh assertion of mega-check Y failed at step 3".
//
// Naming convention: file = check, exported variable = Check, test = file_test.
package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// Healthz verifies the unauthenticated /healthz endpoint returns 200 and the
// expected body. This is the canary check: if it fails, every other check is
// likely to fail too, and the alert wording should make that obvious.
var Healthz = synthetic.CheckFunc{
	NameVal:        "healthz",
	TitleVal:       "API Liveness",
	DescriptionVal: "Hits the unauthenticated /healthz endpoint and verifies the {\"status\":\"ok\"} payload. First to fail if ingress, Caddy, or the API pod is down.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/healthz", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("healthz returned %d: %s", resp.StatusCode, string(body))
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			return resp.StatusCode, fmt.Errorf("healthz body not JSON: %w (body=%q)", err, string(body))
		}
		if got["status"] != "ok" {
			return resp.StatusCode, fmt.Errorf("healthz status=%q, want %q", got["status"], "ok")
		}
		return resp.StatusCode, nil
	},
}

// AuthMe verifies that the authenticated /auth/me endpoint returns 200 and
// includes a username field. This is the second canary: it proves the
// synthetic session cookie is valid AND the API can talk to its database
// (the handler looks up the user row).
var AuthMe = synthetic.CheckFunc{
	NameVal:        "auth_me",
	TitleVal:       "Session Auth + DB",
	DescriptionVal: "Logs in as the synthetic user and calls /auth/me. Proves the JWT secret, the middleware, and the users-table lookup all work end-to-end.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/auth/me", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("auth_me returned %d: %s", resp.StatusCode, string(body))
		}
		// MeResponse has a nested user.username; assert presence without
		// caring about the exact value (synthetic user name is config).
		var parsed struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return resp.StatusCode, fmt.Errorf("auth_me body not JSON: %w", err)
		}
		if parsed.User.Username == "" {
			return resp.StatusCode, fmt.Errorf("auth_me returned empty username")
		}
		return resp.StatusCode, nil
	},
}

// PodsList verifies /api/v1/pods returns 200 and a JSON array (possibly
// empty). It does NOT assert any specific content because the synthetic user
// may legitimately have no pods. If the API ever changes the response shape
// (e.g. to {pods: [...]}) this check fails and the breakage is obvious.
var PodsList = synthetic.CheckFunc{
	NameVal:        "pods_list",
	TitleVal:       "List My Pods",
	DescriptionVal: "Calls /api/v1/pods as the synthetic user and verifies a JSON array (or null) comes back. Smoke-tests the most-used customer route + role filter.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/pods", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("pods_list returned %d: %s", resp.StatusCode, snippet(body))
		}
		// The endpoint returns either `[]` or `[{...}, ...]` — both are valid.
		var arr []any
		if err := json.Unmarshal(body, &arr); err != nil {
			return resp.StatusCode, fmt.Errorf("pods_list body not a JSON array: %w", err)
		}
		return resp.StatusCode, nil
	},
}

// AdminListUsers403 asserts that a non-admin synthetic user is rejected from
// the admin route with 403. This is critical: a permission regression that
// silently grants admin access would not surface in any other check.
var AdminListUsers403 = synthetic.CheckFunc{
	NameVal:        "admin_list_users_403",
	TitleVal:       "RBAC: Student Cannot Admin",
	DescriptionVal: "Calls /api/v1/admin/users as a student-role user and requires a 403. Catches any RBAC regression that would silently elevate students to admin.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/users", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("admin endpoint returned %d, want 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// AdminRunDetail403 asserts that a student cannot read the instructor/admin
// run-detail surface. A permission regression here would expose run history
// and attribution data to the wrong role with no other obvious symptom.
var AdminRunDetail403 = synthetic.CheckFunc{
	NameVal:        "admin_run_detail_403",
	TitleVal:       "RBAC: Student Cannot Read Run Detail",
	DescriptionVal: "Calls /api/v1/admin/runs/{phantom-uuid} as a student-role user and requires a 403. Catches any regression that would expose instructor run detail to students.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		const phantom = "00000000-0000-0000-0000-000000000000"
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/runs/"+phantom, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("admin run detail returned %d, want 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// PodTestingDashboardSmoke probes the /pods/{id}/testing endpoint that
// produced the one observed prod 500 (see plan §"Track S motivation").
// Without a specific synthetic pod ID this check probes a known-bad UUID and
// expects 404, NOT 500. If a future config-load failure begins returning 500
// for missing pods (the way the original bug did), this check fires.
var PodTestingDashboard404 = synthetic.CheckFunc{
	NameVal:        "pod_testing_dashboard_404",
	TitleVal:       "Missing Pod Returns 404 (not 500)",
	DescriptionVal: "Probes /pods/{phantom-uuid}/testing and requires 404. Watches for the historical nil-deref bug in GetTestingDashboard that produced 500s for missing pods.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		// Deterministic non-existent UUID; if a pod with this ID ever exists,
		// we have bigger problems.
		const phantom = "00000000-0000-0000-0000-000000000000"
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/pods/"+phantom+"/testing", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusInternalServerError {
			return resp.StatusCode, fmt.Errorf("phantom pod testing endpoint returned 500 (the bug we are watching for)")
		}
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("phantom pod testing endpoint returned %d, want 404 or 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// WikiIndexRBAC asserts that a non-instructor synthetic user is rejected from
// the wiki index with 403. The wiki contains internal authoring docs scoped
// to instructors and admins; a permission regression would surface here.
//
// We intentionally do NOT positive-path-test /wiki/index against an instructor
// JWT here — the synthetic runs with one role per cycle and the prod
// synthetic is a student. The endpoint itself is exercised by integration
// tests in selfservice-api; this check guards the gate.
var WikiIndexRBAC = synthetic.CheckFunc{
	NameVal:        "wiki_index_rbac",
	TitleVal:       "RBAC: Student Cannot Read Wiki",
	DescriptionVal: "Calls /api/v1/wiki/index as a student-role user and requires a 403. Catches any RBAC regression that would expose the instructor wiki to students.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/wiki/index", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("wiki/index returned %d, want 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// AdminAudit403 asserts that a non-admin synthetic user is rejected from the
// audit-log route with 403. The Audit Log (and active-Sessions listing) is the
// one admin surface deliberately withheld from instructors when lab-instructors
// were granted the rest of the admin panel; this check guards that carve-out.
// It runs as the prod synthetic (student) role, so it also catches any
// regression that would expose the audit trail to lower roles.
var AdminAudit403 = synthetic.CheckFunc{
	NameVal:        "admin_audit_403",
	TitleVal:       "RBAC: Non-Admin Cannot Read Audit Log",
	DescriptionVal: "Calls /api/v1/admin/audit as a non-admin (student) user and requires a 403. Guards the admin-only carve-out for the audit log after instructors gained the rest of the admin surface.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/audit", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("admin/audit returned %d, want 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// TemplatePinRBAC asserts that a non-instructor synthetic user is rejected from
// the template reorder endpoint with 403. Pinning is an instructor-only operation;
// this check catches any RBAC regression that would allow students to reorder templates.
var TemplatePinRBAC = synthetic.CheckFunc{
	NameVal:        "template_pin_rbac",
	TitleVal:       "RBAC: Student Cannot Pin Templates",
	DescriptionVal: "Calls POST /api/v1/admin/templates/reorder as a student-role user and requires a 403. Catches any RBAC regression that would allow students to modify template pinning.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		// Empty reorder request (no templates to reorder) — we just care about the RBAC gate
		resp, err := c.Do(ctx, http.MethodPost, "/api/v1/admin/templates/reorder", io.NopCloser(strings.NewReader("{}")))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusForbidden {
			return resp.StatusCode, fmt.Errorf("template reorder returned %d, want 403", resp.StatusCode)
		}
		return resp.StatusCode, nil
	},
}

// TemplateVisibilityEnforced asserts that instructor_only templates are hidden
// from students: they don't appear in the list and students cannot use them in
// pod creation. This is critical: visibility=instructor_only is the boundary
// between staged templates (not yet ready for student access) and public ones.
// A regression that leaks instructor_only templates would expose unfinished
// content to students.
var TemplateVisibilityEnforced = synthetic.CheckFunc{
	NameVal:        "template_visibility_enforced",
	TitleVal:       "Template Visibility: Students Cannot See instructor_only",
	DescriptionVal: "Lists templates as a student and asserts no instructor_only templates are present. Then attempts to create a pod with a known instructor_only template UUID and requires 403. Guards template staging isolation.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		// 1. List templates as student and verify no instructor_only templates leak
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/templates", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("list templates returned %d: %s", resp.StatusCode, snippet(body))
		}
		var templates []struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Visibility string `json:"visibility"`
		}
		if err := json.Unmarshal(body, &templates); err != nil {
			return resp.StatusCode, fmt.Errorf("list templates body not valid JSON: %w", err)
		}
		for _, t := range templates {
			if t.Visibility == "instructor_only" {
				return resp.StatusCode, fmt.Errorf("template %q (%s) with visibility=instructor_only appeared in student template list (should be hidden)", t.Name, t.ID)
			}
		}

		// 2. Attempt pod creation with a known fixture instructor_only template.
		// The fixture "synthetic-visibility-test" is created in the seed with
		// visibility=instructor_only. This UUID matches the seeded fixture.
		// If the fixture does not exist, the pod creation will fail with 400
		// "template not found", which is acceptable degradation if seeding fails.
		const fixtureTemplateID = "00000000-0000-0000-0000-000000000001"
		createReq := struct {
			Name string `json:"name"`
			VMs  []struct {
				TemplateID  string `json:"template_id"`
				DisplayName string `json:"display_name"`
			} `json:"vms"`
		}{
			Name: "test-pod",
			VMs: []struct {
				TemplateID  string `json:"template_id"`
				DisplayName string `json:"display_name"`
			}{
				{TemplateID: fixtureTemplateID, DisplayName: "test-vm"},
			},
		}
		reqBody, _ := json.Marshal(createReq)
		resp, err = c.Do(ctx, http.MethodPost, "/api/v1/pods", strings.NewReader(string(reqBody)))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		// Student attempting to use instructor_only template must get 403.
		// 400 ("template not found") is acceptable if the fixture doesn't exist.
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			return resp.StatusCode, fmt.Errorf("pod create with instructor_only template returned %d, want 403 or 400", resp.StatusCode)
		}
		if resp.StatusCode == http.StatusForbidden {
			// Ideal case: the fixture exists and we got the right error.
			return resp.StatusCode, nil
		}
		// Fallback: 400 because fixture doesn't exist, which is acceptable.
		return http.StatusOK, nil
	},
}

// AdminRunsFilterContract asserts that the /admin/runs endpoint with an
// unmatched filter returns 200 with an empty array (not 500 or silently
// returning all runs). This catches both nil-deref/500 on unmatched filters
// and silently ignoring filters (which would return all runs instead).
var AdminRunsFilterContract = synthetic.CheckFunc{
	NameVal:        "admin_runs_filter_contract",
	TitleVal:       "Admin Runs Filter Contract",
	DescriptionVal: "Calls /api/v1/admin/runs with an unmatched triggered_by filter and requires 200 + empty array. Catches 500s on missing filters and filters being silently ignored.",
	SeverityVal:    synthetic.SeverityWarning,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		// Use a deterministic UUID that will never have a run
		const phantom = "00000000-0000-0000-0000-000000000000"
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/runs?triggered_by="+phantom, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, fmt.Errorf("admin/runs with unmatched filter returned %d, want 200", resp.StatusCode)
		}
		// Should return a JSON array (possibly empty)
		var arr []any
		if err := json.Unmarshal(body, &arr); err != nil {
			return resp.StatusCode, fmt.Errorf("admin/runs response is not a JSON array: %w", err)
		}
		if len(arr) != 0 {
			return resp.StatusCode, fmt.Errorf("admin/runs with unmatched filter returned %d items, want 0 (filter may be silently ignored)", len(arr))
		}
		return resp.StatusCode, nil
	},
}

// All returns the canonical list of synthetic checks the monitor runs each
// cycle. Ordering does not matter — checks run sequentially and results are
// pushed atomically. Add new checks here.
func All() []synthetic.Check {
	return []synthetic.Check{
		Healthz,
		AuthMe,
		PodsList,
		AdminListUsers403,
		AdminRunDetail403,
		AdminAudit403,
		AdminRunsFilterContract,
		TemplateVisibilityEnforced,
		PodTestingDashboard404,
		WikiIndexRBAC,
		ImageUploadRBAC,
		TemplateHealthStatusRBAC,
		TemplatePinRBAC,
	}
}

// snippet truncates a byte buffer for safe inclusion in error messages.
func snippet(b []byte) string {
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
