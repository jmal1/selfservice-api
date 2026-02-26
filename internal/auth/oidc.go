package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"

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

// SessionClaims are stored in the JWT session token.
type SessionClaims struct {
	jwt.RegisteredClaims
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Provider handles OIDC authentication with Authentik.
type Provider struct {
	oidcProvider *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	queries      *database.Queries
	jwtSecret    []byte
	logger       *slog.Logger
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

	return &Provider{
		oidcProvider: provider,
		oauth2Config: oauth2Cfg,
		verifier:     verifier,
		queries:      queries,
		jwtSecret:    jwtSecret,
		logger:       logger,
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

	// Debug: log raw id_token shape (first 80 chars + dot count)
	dotCount := strings.Count(rawIDToken, ".")
	preview := rawIDToken
	if len(preview) > 80 {
		preview = preview[:80] + "..."
	}
	p.logger.Info("raw id_token debug", "length", len(rawIDToken), "dots", dotCount, "preview", preview)

	idToken, err := p.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		p.logger.Error("id_token verification failed", "error", err, "raw_token_length", len(rawIDToken), "dot_count", dotCount)
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

	// Issue JWT session token
	sessionToken, err := p.issueSessionToken(dbUser)
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
		MaxAge:   int((15 * time.Minute).Seconds()),
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
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// LogoutHandler clears the session.
func (p *Provider) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:   "session",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
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

func (p *Provider) issueSessionToken(user *models.User) (string, error) {
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			Issuer:    "selfservice-api",
		},
		UserID:   user.ID.String(),
		Username: user.Username,
		Role:     user.Role,
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
