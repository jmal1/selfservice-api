package checks

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// TemplateHealthStatusRBAC asserts that the student-role synthetic user is
// refused by the template health status endpoint with 403.
//
// Why this endpoint matters: GET /api/v1/admin/templates/health returns the
// current health state and last-check timestamps for every student-visible
// template. A student who can read it gains:
//   - Awareness of which templates are broken (potentially gaming lab selection)
//   - Timing information about the periodic checker (timing side-channel for
//     assignment launches)
//
// More importantly, it guards the same class of chi-routing regression that the
// image_upload_rbac check guards: if the /admin/templates route block ever drifts
// outside its RequireRole(instructor) guard, this check fires before a student
// notices.
//
// The check runs as the standard student-role synthetic user. It does NOT test
// the positive path (instructor getting 200) because the production synthetic
// always runs as a student. The positive path is exercised by integration tests
// in the handlers package.
var TemplateHealthStatusRBAC = synthetic.CheckFunc{
	NameVal:        "template_health_status_rbac",
	TitleVal:       "RBAC: Student Cannot Read Template Health",
	DescriptionVal: "Calls /api/v1/admin/templates/health as a student-role user and requires 403. Guards the instructor-only health status surface against chi routing regressions.",
	SeverityVal:    synthetic.SeverityWarning,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		resp, err := c.Do(ctx, http.MethodGet, "/api/v1/admin/templates/health", nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)

		switch resp.StatusCode {
		case http.StatusForbidden:
			return resp.StatusCode, nil
		case http.StatusOK:
			return resp.StatusCode, fmt.Errorf(
				"student-role user was ALLOWED to read template health status (status 200) — " +
					"the /admin/templates RBAC gate is open; a student can now read which templates are unhealthy")
		case http.StatusNotFound:
			return resp.StatusCode, fmt.Errorf(
				"GET /api/v1/admin/templates/health returned 404 — the route is missing, " +
					"so this check is no longer proving anything about RBAC")
		default:
			return resp.StatusCode, fmt.Errorf(
				"GET /api/v1/admin/templates/health returned %d as a student, want 403", resp.StatusCode)
		}
	},
}

// TemplateReplicaBuildStatusRBAC guards the read-only recovery/status surface.
// The fixed nonexistent UUIDs ensure an open route returns 404, which is a
// failure: authorization must reject the student before handler lookup.
var TemplateReplicaBuildStatusRBAC = synthetic.CheckFunc{
	NameVal:        "template_replica_build_status_rbac",
	TitleVal:       "RBAC: Student Cannot Read Replica Builds",
	DescriptionVal: "Calls the retained template replica-build status endpoint as a student-role user and requires 403.",
	SeverityVal:    synthetic.SeverityCritical,
	RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
		const path = "/api/v1/admin/templates/00000000-0000-0000-0000-000000000001/" +
			"source-replica-builds/00000000-0000-0000-0000-000000000002"
		resp, err := c.Do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusForbidden {
			return resp.StatusCode, nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, fmt.Errorf(
				"replica build status returned 404 to a student; the route exists but its instructor RBAC gate is not proven")
		}
		return resp.StatusCode, fmt.Errorf(
			"replica build status returned %d to a student, want 403",
			resp.StatusCode,
		)
	},
}
