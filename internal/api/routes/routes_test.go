package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	jwtlib "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/api/handlers"
	"github.com/jmal1/selfservice-api/internal/auth"
	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
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

func newTestAuthProvider(t *testing.T, queries *database.Queries) *auth.Provider {
	t.Helper()

	var issuerURL string
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,"end_session_endpoint":%q}`,
			issuerURL,
			issuerURL+"/application/o/authorize/",
			issuerURL+"/application/o/token/",
			issuerURL+"/keys",
			issuerURL+"/application/o/selfservice/end-session/",
		)
	}))
	issuerURL = issuer.URL
	t.Cleanup(issuer.Close)

	provider, err := auth.NewProvider(context.Background(), config.OIDCConfig{
		IssuerURL:             issuer.URL,
		ClientID:              "selfservice-client",
		ClientSecret:          "client-secret",
		RedirectURL:           "https://app.example.test/auth/callback",
		Scopes:                []string{"openid", "profile", "email"},
		PostLogoutRedirectURI: "https://app.example.test/login",
	}, queries, []byte(testJWTSecret), slog.Default())
	if err != nil {
		t.Fatalf("build auth provider: %v", err)
	}
	return provider
}

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

func TestReadinessAndConsoleRoutes(t *testing.T) {
	provider := auth.NewTestProvider([]byte(testJWTSecret))
	h := handlers.NewHandler(nil, nil, nil, slog.Default(), []string{"https://example.test"})
	router := Setup(h, provider, nil, []string{"https://example.test"})

	tests := []struct {
		name       string
		method     string
		path       string
		cookie     *http.Cookie
		wantStatus int
		wantBody   string
	}{
		{
			name:       "readyz is live",
			method:     http.MethodGet,
			path:       "/readyz",
			wantStatus: http.StatusOK,
			wantBody:   "\"status\":\"ok\"",
		},
		{
			name:       "pod vm console requires auth",
			method:     http.MethodGet,
			path:       "/api/v1/pods/not-a-uuid/vms/not-a-uuid/console/ws",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "unauthorized",
		},
		{
			name:       "pod vm console validates ids after auth",
			method:     http.MethodGet,
			path:       "/api/v1/pods/not-a-uuid/vms/not-a-uuid/console/ws",
			cookie:     makeSessionCookie(t, models.RoleStudent),
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid pod id",
		},
		{
			name:       "template console forbids student users",
			method:     http.MethodGet,
			path:       "/api/v1/admin/templates/00000000-0000-0000-0000-000000000001/console/ws",
			cookie:     makeSessionCookie(t, models.RoleStudent),
			wantStatus: http.StatusForbidden,
			wantBody:   "forbidden",
		},
		{
			name:       "template console validates template id after auth",
			method:     http.MethodGet,
			path:       "/api/v1/admin/templates/not-a-uuid/console/ws",
			cookie:     makeSessionCookie(t, models.RoleInstructor),
			wantStatus: http.StatusBadRequest,
			wantBody:   "invalid template id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want substring %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestAuthRoutesThroughSetup(t *testing.T) {
	provider := newTestAuthProvider(t, nil)
	router := Setup(nil, provider, nil, []string{"https://example.test"})

	t.Run("login redirect includes Authentik state and cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusTemporaryRedirect {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
		}
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("Location is not a valid URL: %v", err)
		}
		if got := loc.Query().Get("client_id"); got != "selfservice-client" {
			t.Fatalf("client_id = %q, want %q", got, "selfservice-client")
		}
		if got := loc.Query().Get("redirect_uri"); got != "https://app.example.test/auth/callback" {
			t.Fatalf("redirect_uri = %q, want %q", got, "https://app.example.test/auth/callback")
		}
		state := loc.Query().Get("state")
		if state == "" {
			t.Fatal("state missing from Authentik redirect URL")
		}
		cookies := rec.Result().Cookies()
		var stateCookie *http.Cookie
		for i := range cookies {
			if cookies[i].Name == "oauth_state" {
				stateCookie = cookies[i]
				break
			}
		}
		if stateCookie == nil {
			t.Fatal("oauth_state cookie was not set")
		}
		if stateCookie.Value != state {
			t.Fatalf("oauth_state cookie value %q does not match redirect state %q", stateCookie.Value, state)
		}
	})

	t.Run("callback rejects mismatched state", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=wrong-state", nil)
		req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "expected-state"})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		if !strings.Contains(rec.Body.String(), "invalid state") {
			t.Fatalf("body = %q, want invalid state", rec.Body.String())
		}
	})

	t.Run("logout clears the session cookie and returns JSON", func(t *testing.T) {
		claims := auth.SessionClaims{
			RegisteredClaims: jwtlib.RegisteredClaims{
				Subject:   uuid.NewString(),
				IssuedAt:  jwtlib.NewNumericDate(time.Now()),
				ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(8 * time.Hour)),
				Issuer:    "selfservice-api",
			},
			UserID:   "not-a-uuid",
			Username: "logout-user",
			Role:     models.RoleStudent,
		}
		token, err := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims).SignedString([]byte(testJWTSecret))
		if err != nil {
			t.Fatalf("sign JWT: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		req.AddCookie(&http.Cookie{Name: "session", Value: token})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode logout JSON: %v", err)
		}
		if resp["status"] != "logged_out" {
			t.Fatalf("status = %q, want %q", resp["status"], "logged_out")
		}
		logoutURL := resp["logout_url"]
		if logoutURL == "" {
			t.Fatal("logout_url missing from JSON response")
		}
		u, err := url.Parse(logoutURL)
		if err != nil {
			t.Fatalf("logout_url is not a valid URL: %v", err)
		}
		if got := u.Query().Get("post_logout_redirect_uri"); got != "https://app.example.test/login" {
			t.Fatalf("post_logout_redirect_uri = %q, want %q", got, "https://app.example.test/login")
		}
		if got := u.Query().Get("id_token_hint"); got != "" {
			t.Fatalf("id_token_hint = %q, want empty when no persisted session token exists", got)
		}
		var sessionCookie *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == "session" {
				sessionCookie = c
				break
			}
		}
		if sessionCookie == nil {
			t.Fatal("session cookie was not cleared")
		}
		if sessionCookie.Value != "" || sessionCookie.MaxAge != -1 || !sessionCookie.HttpOnly || !sessionCookie.Secure {
			t.Fatalf("session cookie not cleared as expected: %#v", sessionCookie)
		}
	})
}

func TestLogoutRouteDeactivatesSessionAndIncludesIdTokenHint(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run logout persistence tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect database: %v", err)
	}
	defer pool.Close()
	queries := database.NewQueries(pool)

	userID := uuid.New()
	user := &models.User{
		ID:          userID,
		OIDCSub:     "logout-route-" + uuid.NewString(),
		Username:    "logout.route.user",
		Email:       "logout-route@example.test",
		DisplayName: "Logout Route User",
		Role:        models.RoleStudent,
	}
	if err := queries.UpsertUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	stored, err := queries.GetUserBySub(ctx, user.OIDCSub)
	if err != nil {
		t.Fatalf("get user by sub: %v", err)
	}
	if stored == nil {
		t.Fatal("stored user not found")
	}

	sessionID := uuid.New()
	idToken := "logout-id-token-" + uuid.NewString()
	if err := queries.CreateSession(ctx, sessionID, stored.ID, "127.0.0.1", "test-agent", idToken); err != nil {
		t.Fatalf("create session: %v", err)
	}

	jwtToken, err := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, auth.SessionClaims{
		RegisteredClaims: jwtlib.RegisteredClaims{
			Subject:   stored.ID.String(),
			IssuedAt:  jwtlib.NewNumericDate(time.Now()),
			ExpiresAt: jwtlib.NewNumericDate(time.Now().Add(8 * time.Hour)),
			Issuer:    "selfservice-api",
		},
		UserID:    stored.ID.String(),
		Username:  stored.Username,
		Role:      stored.Role,
		SessionID: sessionID.String(),
	}).SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}

	provider := newTestAuthProvider(t, queries)
	router := Setup(nil, provider, queries, []string{"https://app.example.test"})

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: jwtToken})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode logout JSON: %v", err)
	}
	if _, hasRawIDToken := resp["id_token"]; hasRawIDToken {
		t.Fatalf("logout response exposed raw id_token: %#v", resp)
	}
	if _, hasRawHint := resp["id_token_hint"]; hasRawHint {
		t.Fatalf("logout response exposed id_token_hint as a JSON field: %#v", resp)
	}
	logoutURL := resp["logout_url"]
	if logoutURL == "" {
		t.Fatal("logout_url missing from JSON response")
	}
	u, err := url.Parse(logoutURL)
	if err != nil {
		t.Fatalf("logout_url is not a valid URL: %v", err)
	}
	if got := u.Query().Get("id_token_hint"); got != idToken {
		t.Fatalf("id_token_hint = %q, want %q", got, idToken)
	}
	if got := u.Query().Get("post_logout_redirect_uri"); got != "https://app.example.test/login" {
		t.Fatalf("post_logout_redirect_uri = %q, want %q", got, "https://app.example.test/login")
	}

	active, err := queries.IsSessionActive(ctx, sessionID)
	if err != nil {
		t.Fatalf("check session active: %v", err)
	}
	if active {
		t.Fatal("session remains active after logout")
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
