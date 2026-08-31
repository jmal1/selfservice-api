package routes

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/api/handlers"
	"github.com/jmal1/selfservice-api/internal/auth"
	"github.com/jmal1/selfservice-api/internal/models"
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

func TestTemplateAdministrationRoutesAreRegistered(t *testing.T) {
	found := walkRoutes(t)
	want := []string{
		"GET /api/v1/admin/templates/",
		"POST /api/v1/admin/templates/",
		"POST /api/v1/admin/templates/draft",
		"GET /api/v1/admin/templates/guest-os-catalog",
		"GET /api/v1/admin/templates/{templateID}/wizard-state",
		"POST /api/v1/admin/templates/{templateID}/provision",
		"POST /api/v1/admin/templates/{templateID}/preflight",
		"POST /api/v1/admin/templates/{templateID}/generalize",
		"POST /api/v1/admin/templates/{templateID}/publish",
		"POST /api/v1/admin/templates/{templateID}/unpublish",
		"POST /api/v1/admin/templates/{templateID}/cancel",
		"POST /api/v1/admin/templates/{templateID}/retry",
		"POST /api/v1/admin/templates/{templateID}/power",
		"GET /api/v1/admin/templates/{templateID}/resolved-credentials",
		"GET /api/v1/admin/templates/{templateID}/console/ticket",
		"PATCH /api/v1/admin/templates/{templateID}",
		"DELETE /api/v1/admin/templates/{templateID}",
		"POST /api/v1/admin/templates/{templateID}/access",
		"GET /api/v1/admin/templates/{templateID}/dependents",
		"GET /api/v1/admin/templates/{templateID}/playlists",
		"POST /api/v1/admin/templates/{templateID}/playlists",
		"GET /api/v1/admin/templates/{templateID}/source-replicas",
		"POST /api/v1/admin/templates/{templateID}/source-replicas",
		"DELETE /api/v1/admin/templates/{templateID}/source-replicas/{replicaID}",
		"POST /api/v1/admin/templates/{templateID}/source-replica-builds",
		"GET /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}",
		"POST /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}/retry",
		"POST /api/v1/admin/templates/{templateID}/source-replica-builds/{buildID}/cleanup",
		"POST /api/v1/admin/templates/{id}/pin",
		"DELETE /api/v1/admin/templates/{id}/pin",
		"POST /api/v1/admin/templates/reorder",
	}
	for _, route := range want {
		if !found[route] {
			t.Errorf("template administration route not registered: %s", route)
		}
	}
}

func TestTemplateAdministrationRoutesRequireInstructorRole(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	router := Setup(nil, provider, nil, []string{"*"})

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/admin/templates/"},
		{method: http.MethodPost, path: "/api/v1/admin/templates/"},
		{method: http.MethodPatch, path: "/api/v1/admin/templates/" + uuid.NewString()},
		{method: http.MethodDelete, path: "/api/v1/admin/templates/" + uuid.NewString()},
	} {
		t.Run(tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.AddCookie(makeSessionCookie(t, models.RoleStudent))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("student role: status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestAdministrativeResourceRoutesAreRegistered(t *testing.T) {
	found := walkRoutes(t)
	want := []string{
		"GET /api/v1/admin/audit",
		"GET /api/v1/admin/audit/search",
		"GET /api/v1/admin/sessions",
		"PATCH /api/v1/admin/users/{userID}/quotas",
		"GET /api/v1/admin/jobs",
		"GET /api/v1/admin/vlans",
		"POST /api/v1/admin/vlans",
		"PATCH /api/v1/admin/vlans/{vlanID}",
		"DELETE /api/v1/admin/vlans/{vlanID}",
		"GET /api/v1/admin/blueprints",
		"POST /api/v1/admin/blueprints",
		"PUT /api/v1/admin/blueprints/{blueprintID}",
		"DELETE /api/v1/admin/blueprints/{blueprintID}",
		"POST /api/v1/admin/blueprints/{blueprintID}/access",
		"POST /api/v1/admin/blueprints/{id}/pin",
		"DELETE /api/v1/admin/blueprints/{id}/pin",
		"POST /api/v1/admin/blueprints/reorder",
		"POST /api/v1/admin/blueprints/{blueprintID}/vm-playlists",
		"GET /api/v1/admin/blueprints/{blueprintID}/vm-playlists",
		"DELETE /api/v1/admin/blueprints/{blueprintID}/vm-playlists/{vmSlot}",
		"POST /api/v1/admin/pods/{podID}/extend",
		"POST /api/v1/admin/pods/{podID}/finalize-orphaned-destroy",
		"GET /api/v1/admin/workflows/",
		"POST /api/v1/admin/workflows/",
		"POST /api/v1/admin/workflows/import",
		"GET /api/v1/admin/workflows/export",
		"GET /api/v1/admin/workflows/{workflowID}",
		"PUT /api/v1/admin/workflows/{workflowID}",
		"DELETE /api/v1/admin/workflows/{workflowID}",
		"POST /api/v1/admin/workflows/{workflowID}/submit",
		"POST /api/v1/admin/workflows/{workflowID}/approve",
		"POST /api/v1/admin/workflows/{workflowID}/activate",
		"GET /api/v1/admin/actions/",
		"POST /api/v1/admin/actions/",
		"GET /api/v1/admin/actions/{actionID}",
		"PUT /api/v1/admin/actions/{actionID}",
		"DELETE /api/v1/admin/actions/{actionID}",
		"POST /api/v1/admin/scripts/validate",
		"GET /api/v1/admin/playlists/",
		"POST /api/v1/admin/playlists/",
		"GET /api/v1/admin/playlists/{playlistID}",
		"PUT /api/v1/admin/playlists/{playlistID}",
		"DELETE /api/v1/admin/playlists/{playlistID}",
	}
	for _, route := range want {
		if !found[route] {
			t.Errorf("administrative resource route not registered: %s", route)
		}
	}
}

func TestAdministrativeResourceRoutesRequireInstructorRole(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	router := Setup(nil, provider, nil, []string{"*"})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/admin/jobs"},
		{method: http.MethodPatch, path: "/api/v1/admin/users/" + uuid.NewString() + "/quotas"},
		{method: http.MethodPost, path: "/api/v1/admin/blueprints/reorder"},
		{method: http.MethodPost, path: "/api/v1/admin/pods/" + uuid.NewString() + "/extend"},
		{method: http.MethodPost, path: "/api/v1/admin/workflows/import"},
		{method: http.MethodDelete, path: "/api/v1/admin/actions/" + uuid.NewString()},
		{method: http.MethodPut, path: "/api/v1/admin/playlists/" + uuid.NewString()},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.AddCookie(makeSessionCookie(t, models.RoleStudent))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("student role: status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestAuditAndSessionRoutesRequireAdminRole(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	router := Setup(nil, provider, nil, []string{"*"})
	for _, path := range []string{"/api/v1/admin/audit", "/api/v1/admin/audit/search", "/api/v1/admin/sessions"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(makeSessionCookie(t, models.RoleInstructor))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("instructor role: status = %d, want 403", rec.Code)
			}
		})
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

// Test that blueprint VM playlists routes are registered.
func TestBlueprintVMPlaylistsRoutesRegistered(t *testing.T) {
	found := walkRoutes(t)

	want := []string{
		"GET /api/v1/admin/blueprints/{blueprintID}/vm-playlists",
		"DELETE /api/v1/admin/blueprints/{blueprintID}/vm-playlists/{vmSlot}",
	}
	for _, w := range want {
		if !found[w] {
			t.Errorf("route not registered: %s", w)
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
// returns 403 for a student-role caller.
//
// This test drives a real HTTP request through the chi router built by Setup()
// so that RequireRole(RoleInstructor) — not just the handler method — is
// exercised.  A test that calls h.AdminListRuns directly would pass even if
// the route were accidentally mounted outside the /admin guard block, because
// the handler does not check the caller's role.
//
// auth.NewTestProvider creates a minimal *auth.Provider that can verify JWTs
// signed with a known secret.  The signed JWT carries no SessionID, which
// skips the server-side session-liveness check (see ValidateSession).
// The Handler passed to Setup is nil: RequireRole fires before any handler
// method is invoked for the student case; for the instructor case the nil
// handler panics, chi's Recoverer returns 500, which is still != 403.
const testJWTSecret = "admin-runs-rbac-test-secret-do-not-use-in-prod"

func makeSessionCookie(t *testing.T, role string) *http.Cookie {
	t.Helper()
	claims := auth.SessionClaims{
		RegisteredClaims: jwtlib.RegisteredClaims{
			Subject:   uuid.New().String(),
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
			ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(8 * time.Hour)),
		},
		UserID:   uuid.New().String(),
		Username: "testuser",
		Role:     role,
		// SessionID intentionally empty — skips p.queries.IsSessionActive.
	}
	token := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return &http.Cookie{Name: "session", Value: signed}
}

func TestAdminRunsRequiresInstructorRole(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	// h=nil is safe: RequireRole fires before any handler is called for the
	// student case.  For the instructor case the nil *Handler panics inside
	// AdminListRuns; chi's Recoverer returns 500, which satisfies != 403.
	router := Setup(nil, provider, nil, []string{"*"})

	t.Run("student role is forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/runs", nil)
		req.AddCookie(makeSessionCookie(t, models.RoleStudent))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("student role: status = %d; want 403 Forbidden — "+
				"RequireRole(RoleInstructor) guard on the /admin block must fire", rec.Code)
		}
	})

	t.Run("instructor role passes the guard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/runs", nil)
		req.AddCookie(makeSessionCookie(t, models.RoleInstructor))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		// The nil Handler panics → Recoverer returns 500.
		// What matters: the guard did NOT return 403.
		if rec.Code == http.StatusForbidden {
			t.Errorf("instructor role: got 403 Forbidden; want guard to allow through "+
				"(any non-403 is acceptable here, got %d)", rec.Code)
		}
	})
}

func TestStudentGuideRoutesAreRegistered(t *testing.T) {
	found := walkRoutes(t)
	for _, want := range []string{
		"GET /api/v1/student-guide/index",
		"GET /api/v1/student-guide/page",
	} {
		if !found[want] {
			t.Errorf("route not registered: %s", want)
		}
	}
	if found["GET /api/v1/student-guide/bundle.zip"] {
		t.Error("student guide must not expose a bundle ZIP route")
	}
}

func TestProvisioningStatusRouteIsAuthenticated(t *testing.T) {
	found := walkRoutes(t)
	if !found["GET /api/v1/provisioning/status"] {
		t.Fatal("GET /api/v1/provisioning/status is not registered")
	}

	provider := auth.NewTestProvider([]byte(testJWTSecret))
	h := handlers.NewHandler(nil, nil, nil, slog.Default(), nil)
	router := Setup(h, provider, nil, []string{"*"})

	t.Run("unauthenticated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/provisioning/status", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("authenticated student", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/provisioning/status", nil)
		req.AddCookie(makeSessionCookie(t, models.RoleStudent))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestStudentGuideRequiresAuthenticationAndPreservesWikiRBAC(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	h := handlers.NewHandler(nil, nil, nil, slog.Default(), nil)
	router := Setup(h, provider, nil, []string{"*"})

	t.Run("unauthenticated student guide is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/index", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("student can read guide index and page", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/student-guide/index",
			"/api/v1/student-guide/page/docs/student/overview.md",
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(makeSessionCookie(t, models.RoleStudent))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("%s: status = %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("student remains forbidden from instructor wiki", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/wiki/index",
			"/api/v1/wiki/page/AGENTS.md",
			"/api/v1/wiki/bundle.zip",
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.AddCookie(makeSessionCookie(t, models.RoleStudent))
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", path, rec.Code)
			}
		}
	})
}
