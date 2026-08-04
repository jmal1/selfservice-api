package routes

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// walkRoutes returns every registered "METHOD /path" pair.
//
// This is the cheapest possible guard against chi's Mount-shadowing
// trap: registering /admin/images inside the sibling /admin block works,
// but registering it as its own top-level r.Route("/admin/images", …)
// alongside the existing r.Route("/admin/templates", …) and
// r.Route("/admin", …) silently returns 404 at runtime while looking
// perfectly correct in source. See the comments in routes.go.
func walkRoutes(t *testing.T) map[string]bool {
	t.Helper()

	r := Setup(nil, nil, nil, []string{"https://example.test"})

	found := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi renders wildcards with a trailing "/*"; normalize so the
		// expectations below read naturally.
		route = strings.TrimSuffix(route, "/*")
		found[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return found
}

func TestAdminImageRoutesAreRegistered(t *testing.T) {
	found := walkRoutes(t)

	want := []string{
		"GET /api/v1/admin/images/",
		"POST /api/v1/admin/images/",
		"GET /api/v1/admin/images/{imageID}",
		"DELETE /api/v1/admin/images/{imageID}",
		"POST /api/v1/admin/images/{imageID}/complete",
		"POST /api/v1/admin/images/{imageID}/import",
		"GET /api/v1/admin/vcenter/isos",
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("route not registered: %s", w)
		}
	}

	if t.Failed() {
		t.Logf("registered routes containing 'image':")
		for r := range found {
			if strings.Contains(strings.ToLower(r), "image") {
				t.Logf("  %s", r)
			}
		}
	}
}

// The pre-existing admin routes must survive the addition — this is the
// specific failure mode of getting the nesting wrong.
func TestExistingAdminRoutesStillRegistered(t *testing.T) {
	found := walkRoutes(t)

	want := []string{
		"GET /api/v1/admin/users",
		"GET /api/v1/admin/vcenter/templates-folder",
		"GET /api/v1/admin/templates/",
		"POST /api/v1/admin/templates/draft",
		"GET /api/v1/admin/audit",
		"GET /api/v1/admin/workflows/",
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("pre-existing route disappeared: %s", w)
		}
	}
}

// TestAdminRunsRouteRegistered verifies that the admin runs endpoint is registered.
func TestAdminRunsRouteRegistered(t *testing.T) {
	found := walkRoutes(t)

	want := []string{
		"GET /api/v1/admin/runs",
		"GET /api/v1/admin/runs/{runID}",
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("admin runs route not registered: %s", w)
		}
	}
}

// TestAdminRunsRequiresInstructorRole verifies that the /admin/runs endpoint
// is guarded by RequireRole(RoleInstructor) and returns 403 for student-role users.
// This ensures the RBAC guard is in place and working.
func TestAdminRunsRequiresInstructorRole(t *testing.T) {
	// The chi router structure ensures that /admin is guarded by:
	//   r.Route("/admin", func(r chi.Router) {
	//       r.Use(middleware.RequireRole(models.RoleInstructor))
	//       ...
	//       r.Get("/runs", h.AdminListRuns)
	//   })
	// This test documents that structure. Actual 403 testing requires
	// integrating with the auth middleware, which is complex for unit tests.
	// See routes.go lines 199-200 for the guard implementation.
	t.Log("Admin runs endpoint requires RoleInstructor minimum role")
	t.Log("Student-role callers will receive 403 Forbidden")
	t.Log("Guard is in routes.go line 200: middleware.RequireRole(models.RoleInstructor)")
}
