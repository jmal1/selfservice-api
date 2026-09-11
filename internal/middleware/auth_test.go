package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// withRole returns a request whose context carries the given role, mirroring
// what the Auth middleware injects after validating a session. Tests use this
// to exercise RequireRole without standing up the full OIDC provider.
func withRole(role string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/anything", nil)
	ctx := context.WithValue(req.Context(), roleKey, role)
	return req.WithContext(ctx)
}

// TestRequireRole_LevelBased locks the level-based semantics the instructor
// admin surface depends on: RequireRole(min) admits any role >= min. If this
// ever regresses to exact-match (or the level ordering is scrambled),
// instructors would either lose the admin surface or students would gain it.
func TestRequireRole_LevelBased(t *testing.T) {
	cases := []struct {
		name       string
		minRole    string
		actualRole string
		wantPass   bool
	}{
		// RequireRole(Instructor) — the gate on the widened /admin surface.
		{"instructor gate admits instructor", models.RoleInstructor, models.RoleInstructor, true},
		{"instructor gate admits admin", models.RoleInstructor, models.RoleAdmin, true},
		{"instructor gate rejects student", models.RoleInstructor, models.RoleStudent, false},

		// RequireRole(Admin) — the gate kept on Audit Log + Sessions.
		{"admin gate admits admin", models.RoleAdmin, models.RoleAdmin, true},
		{"admin gate rejects instructor", models.RoleAdmin, models.RoleInstructor, false},
		{"admin gate rejects student", models.RoleAdmin, models.RoleStudent, false},

		// Unknown / empty roles never satisfy any gate.
		{"empty role rejected by instructor gate", models.RoleInstructor, "", false},
		{"unknown role rejected by admin gate", models.RoleAdmin, "superuser", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})

			rec := httptest.NewRecorder()
			RequireRole(tc.minRole)(next).ServeHTTP(rec, withRole(tc.actualRole))

			if tc.wantPass {
				if !reached {
					t.Fatalf("role %q should pass RequireRole(%q) but was blocked (status %d)",
						tc.actualRole, tc.minRole, rec.Code)
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200 for allowed role, got %d", rec.Code)
				}
			} else {
				if reached {
					t.Fatalf("role %q should be blocked by RequireRole(%q) but reached the handler",
						tc.actualRole, tc.minRole)
				}
				if rec.Code != http.StatusForbidden {
					t.Fatalf("expected 403 for denied role, got %d", rec.Code)
				}
			}
		})
	}
}

// TestHasMinRole documents the exact level ordering. Kept separate from the
// middleware test so a future role addition (e.g. a "ta" tier) forces an
// intentional update here.
func TestHasMinRole(t *testing.T) {
	if !hasMinRole(models.RoleInstructor, models.RoleInstructor) {
		t.Error("instructor should satisfy instructor minimum")
	}
	if !hasMinRole(models.RoleAdmin, models.RoleInstructor) {
		t.Error("admin should satisfy instructor minimum")
	}
	if hasMinRole(models.RoleStudent, models.RoleInstructor) {
		t.Error("student must NOT satisfy instructor minimum")
	}
	if hasMinRole(models.RoleInstructor, models.RoleAdmin) {
		t.Error("instructor must NOT satisfy admin minimum")
	}
}
