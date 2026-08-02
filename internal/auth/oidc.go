package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

// OIDCClaims represents the claims returned by Authentik.
type OIDCClaims struct {
	Sub           string   `json:"sub"`
	PreferredUser string   `json:"preferred_username"`
	Email         string   `json:"email"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
}

const sessionTTL = 8 * time.Hour

// SessionClaims are stored in the JWT session token.
type SessionClaims struct {
	jwt.RegisteredClaims
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	SessionID string `json:"session_id,omitempty"`
}

// Provider handles OIDC authentication with Authentik.
type Provider struct {
	oidcProvider          *oidc.Provider
	oauth2Config          oauth2.Config
	verifier              *oidc.IDTokenVerifier
	queries               *database.Queries
	jwtSecret             []byte
	logger                *slog.Logger
	endSessionEndpoint    string
	postLogoutRedirectURI string
}

// NewProvider creates a new OIDC authentication provider.
func NewProvider(ctx context.Context, cfg config.OIDCConfig, queries *database.Queries, jwtSecret []byte, logger *slog.Logger) (*Provider, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("create OIDC provider: %w", err)
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       cfg.Scopes,
	}

	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	// Read Authentik's non-standard end_session_endpoint from the discovery
	// document. It's absent from the go-oidc typed endpoints, so pull it out
	// of the raw provider metadata. Absence is tolerated: LogoutHandler falls
	// back to a JSON logout response when this is empty.
	var discovery struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovery); err != nil {
		logger.Warn("failed to read OIDC discovery claims; end_session_endpoint unavailable", "error", err)
	}
	if discovery.EndSessionEndpoint == "" {
		logger.Warn("OIDC discovery has no end_session_endpoint; logout will not terminate the IdP session")
	}

	return &Provider{
		oidcProvider:          provider,
		oauth2Config:          oauth2Cfg,
		verifier:              verifier,
		queries:               queries,
		jwtSecret:             jwtSecret,
		logger:                logger,
		endSessionEndpoint:    discovery.EndSessionEndpoint,
		postLogoutRedirectURI: cfg.PostLogoutRedirectURI,
	}, nil
}

// LoginHandler redirects the user to Authentik for authentication.
func (p *Provider) LoginHandler(w http.ResponseWriter, r *http.Request) {
	state, err := generateState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   300,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, p.oauth2Config.AuthCodeURL(state), http.StatusTemporaryRedirect)
}

// CallbackHandler handles the OIDC callback from Authentik.
func (p *Provider) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Verify state
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	// Exchange code for tokens
	token, err := p.oauth2Config.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		p.logger.Error("token exchange failed", "error", err)
		http.Error(w, "authentication failed", http.StatusInternalServerError)
		return
	}

	// Extract and verify ID token
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusInternalServerError)
		return
	}

	idToken, err := p.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		p.logger.Error("id_token verification failed", "error", err)
		http.Error(w, "token verification failed", http.StatusInternalServerError)
		return
	}

	// Parse claims
	var claims OIDCClaims
	if err := idToken.Claims(&claims); err != nil {
		p.logger.Error("failed to parse claims", "error", err)
		http.Error(w, "failed to parse user info", http.StatusInternalServerError)
		return
	}

	// Map Authentik groups to role
	role := mapGroupsToRole(claims.Groups)

	// Determine quotas for new users
	quotas := models.DefaultQuotas[role]

	// Upsert user in database
	user := &models.User{
		OIDCSub:     claims.Sub,
		Username:    claims.PreferredUser,
		Email:       claims.Email,
		DisplayName: claims.Name,
		Role:        role,
		MaxVCPUs:    quotas.MaxVCPUs,
		MaxRAMMB:    quotas.MaxRAMMB,
		MaxPods:     quotas.MaxPods,
	}

	if err := p.queries.UpsertUser(r.Context(), user); err != nil {
		p.logger.Error("failed to upsert user", "error", err, "username", claims.PreferredUser)
		http.Error(w, "failed to save user", http.StatusInternalServerError)
		return
	}

	// Fetch full user record (to get ID)
	dbUser, err := p.queries.GetUserBySub(r.Context(), claims.Sub)
	if err != nil || dbUser == nil {
		p.logger.Error("failed to fetch user after upsert", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create server-side session
	sessionID := uuid.New()
	if err := p.queries.CreateSession(r.Context(), sessionID, dbUser.ID, r.RemoteAddr, r.UserAgent(), rawIDToken); err != nil {
		p.logger.Error("failed to create session", "error", err)
		// Non-fatal: continue without server-side session tracking
	}

	// Issue JWT session token
	sessionToken, err := p.issueSessionToken(dbUser, sessionID.String())
	if err != nil {
		p.logger.Error("failed to issue session token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Set session cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    sessionToken,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	// Clear state cookie
	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	// Redirect to frontend
	audit.Log(r.Context(), p.queries, "auth.login",
		audit.User(dbUser.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("username", dbUser.Username),
		audit.Detail("role", role),
		audit.Detail("session_id", sessionID.String()),
	)
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// LogoutHandler clears the local session and returns the Authentik
// end-session URL so the browser can terminate the OIDC SSO session too.
//
// The frontend calls POST /auth/logout via fetch, reads `logout_url` from the
// JSON response, and navigates the browser there (a top-level navigation is
// required so Authentik can clear its own SSO cookie and then redirect to the
// configured post_logout_redirect_uri). A fetch alone cannot terminate the IdP
// session, which is why we hand the URL back rather than emitting a 302 that a
// fetch would silently follow without touching the SSO cookie.
//
// When no id_token was persisted or the IdP exposes no end_session_endpoint,
// we fall back to the plain {"status":"logged_out"} response.
func (p *Provider) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	var idToken string

	// Deactivate server-side session if present
	if claims, err := p.ValidateSession(r); err == nil {
		if sid, err := uuid.Parse(claims.SessionID); err == nil {
			if tok, err := p.queries.GetSessionIDToken(r.Context(), sid); err == nil {
				idToken = tok
			}
			_ = p.queries.DeactivateSession(r.Context(), sid)
		}
		if uid, err := uuid.Parse(claims.UserID); err == nil {
			audit.Log(r.Context(), p.queries, "auth.logout",
				audit.User(uid),
				audit.IP(r.RemoteAddr),
				audit.Detail("username", claims.Username),
			)
		}
	}

	// Clear the session cookie with the same attributes it was issued with so
	// browsers reliably overwrite it (a mismatched HttpOnly/Secure/SameSite can
	// leave the original cookie in place).
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	resp := map[string]string{"status": "logged_out"}
	if logoutURL := p.buildEndSessionURL(idToken); logoutURL != "" {
		resp["logout_url"] = logoutURL
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// buildEndSessionURL constructs the Authentik end_session_endpoint URL with the
// id_token_hint and post_logout_redirect_uri query parameters. Returns "" when
// the IdP advertised no end_session_endpoint (the caller then omits it and the
// SSO session is left untouched — best effort).
func (p *Provider) buildEndSessionURL(idToken string) string {
	if p.endSessionEndpoint == "" {
		return ""
	}
	u, err := url.Parse(p.endSessionEndpoint)
	if err != nil {
		p.logger.Error("failed to parse end_session_endpoint", "error", err)
		return ""
	}
	q := u.Query()
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	if p.postLogoutRedirectURI != "" {
		q.Set("post_logout_redirect_uri", p.postLogoutRedirectURI)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// SessionIsActive reports whether the given server-side session is still active.
// The auth middleware uses this so a JWT for a logged-out/revoked session is
// rejected before its 8h expiry.
func (p *Provider) SessionIsActive(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	return p.queries.IsSessionActive(ctx, sessionID)
}

// ValidateSession extracts and validates the session JWT from a request.
func (p *Provider) ValidateSession(r *http.Request) (*SessionClaims, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil, fmt.Errorf("no session cookie")
	}

	token, err := jwt.ParseWithClaims(cookie.Value, &SessionClaims{}, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return p.jwtSecret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid session token: %w", err)
	}

	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

// RefreshSessionCookie re-issues the session JWT if it's past the halfway point of its TTL.
// This creates a sliding window so active users don't get logged out.
func (p *Provider) RefreshSessionCookie(w http.ResponseWriter, claims *SessionClaims) {
	if claims.ExpiresAt == nil || claims.IssuedAt == nil {
		return
	}
	total := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time)
	elapsed := time.Since(claims.IssuedAt.Time)
	if elapsed < total/2 {
		return
	}

	// Re-issue token with fresh TTL
	now := time.Now()
	claims.IssuedAt = jwt.NewNumericDate(now)
	claims.ExpiresAt = jwt.NewNumericDate(now.Add(sessionTTL))
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(p.jwtSecret)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    signed,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (p *Provider) issueSessionToken(user *models.User, sessionID string) (string, error) {
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(sessionTTL)),
			Issuer:    "selfservice-api",
		},
		UserID:    user.ID.String(),
		Username:  user.Username,
		Role:      user.Role,
		SessionID: sessionID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(p.jwtSecret)
}

// mapGroupsToRole maps Authentik group names to portal roles.
// Priority: admin > instructor > student.
func mapGroupsToRole(groups []string) string {
	for _, g := range groups {
		g = strings.ToLower(g)
		if g == "lab-admins" || g == "lab-super-admins" {
			return models.RoleAdmin
		}
	}
	for _, g := range groups {
		if strings.ToLower(g) == "lab-instructors" {
			return models.RoleInstructor
		}
	}
	return models.RoleStudent
}

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
