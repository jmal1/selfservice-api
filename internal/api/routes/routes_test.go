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
