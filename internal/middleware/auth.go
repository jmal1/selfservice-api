package middleware

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/auth"
	"github.com/jmal1/selfservice-api/internal/models"
)

type contextKey string

const (
	userIDKey    contextKey = "user_id"
	usernameKey  contextKey = "username"
	roleKey      contextKey = "role"
	sessionIDKey contextKey = "session_id"
)

// Auth returns middleware that validates the session JWT.
func Auth(provider *auth.Provider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := provider.ValidateSession(r)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			// Sliding window: refresh token if past halfway
			provider.RefreshSessionCookie(w, claims)

			uid, err := uuid.Parse(claims.UserID)
			if err != nil {
				http.Error(w, "invalid session", http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), userIDKey, uid)
			ctx = context.WithValue(ctx, usernameKey, claims.Username)
			ctx = context.WithValue(ctx, roleKey, claims.Role)
			ctx = audit.WithUserID(ctx, uid)
			ctx = audit.WithClientIP(ctx, r.RemoteAddr)
			if claims.SessionID != "" {
				if sid, err := uuid.Parse(claims.SessionID); err == nil {
					// Reject a JWT whose server-side session has been
					// deactivated (logout/revocation) even though the JWT
					// itself hasn't expired. Empty SessionID (e.g. synthetic
					// monitor tokens) skips this check by design.
					active, err := provider.SessionIsActive(r.Context(), sid)
					if err != nil {
						http.Error(w, "internal error", http.StatusInternalServerError)
						return
					}
					if !active {
						http.Error(w, "unauthorized", http.StatusUnauthorized)
						return
					}
					ctx = context.WithValue(ctx, sessionIDKey, sid)
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole returns middleware that requires a minimum role level.
func RequireRole(minRole string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := RoleFromContext(r.Context())
			if !HasMinRole(role, minRole) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// HasMinRole reports whether the actual role meets or exceeds the minimum required role.
func HasMinRole(actual, minimum string) bool {
	return hasMinRole(actual, minimum)
}

// UserIDFromContext extracts the user ID from the request context.
func UserIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(userIDKey).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// UsernameFromContext extracts the username from the request context.
func UsernameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(usernameKey).(string); ok {
		return v
	}
	return ""
}

// RoleFromContext extracts the role from the request context.
func RoleFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(roleKey).(string); ok {
		return v
	}
	return ""
}

// SessionIDFromContext extracts the session ID from the request context.
func SessionIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(sessionIDKey).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// WithRole returns a copy of ctx with the given role injected under the same
// key used by the Auth middleware. Use this in tests and internal service calls
// that need to establish a role without going through full JWT validation.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleKey, role)
}

// WithUserID returns a copy of ctx with the given user ID injected under the
// same key used by the Auth middleware. Use this in tests and internal service
// calls that need to establish a user identity without a live JWT.
func WithUserID(ctx context.Context, userID uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// hasMinRole checks if the actual role meets or exceeds the minimum required role.
func hasMinRole(actual, minimum string) bool {
	roleLevel := map[string]int{
		models.RoleStudent:    1,
		models.RoleInstructor: 2,
		models.RoleAdmin:      3,
	}
	return roleLevel[actual] >= roleLevel[minimum]
}
