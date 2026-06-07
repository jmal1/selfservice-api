package synthetic

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jmal1/selfservice-api/internal/auth"
)

// MintSessionToken issues a session JWT identical in shape to the one issued
// by Provider.LoginHandler after a successful OIDC login. This is the only
// supported synthetic auth path: the monitor needs the same JWT_SECRET the
// API uses, and the synthetic user must already exist in the users table.
//
// ttl SHOULD be short (a few minutes) — synthetic runs complete in seconds, so
// long-lived tokens add risk without value. The returned token is suitable for
// the `session` cookie consumed by middleware.Auth.
//
// userID, username, and role MUST match the synthetic user's DB row;
// middleware.Auth re-uses claims.UserID directly for DB lookups, so a stale or
// wrong UUID causes a 401 (not a useful error message).
func MintSessionToken(jwtSecret []byte, userID, username, role string, ttl time.Duration) (string, error) {
	if len(jwtSecret) == 0 {
		return "", fmt.Errorf("jwtSecret is empty")
	}
	if userID == "" {
		return "", fmt.Errorf("userID is empty")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("ttl must be positive")
	}

	now := time.Now()
	claims := auth.SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "selfservice-synthetic-api-monitor",
		},
		UserID:   userID,
		Username: username,
		Role:     role,
		// SessionID intentionally left empty: middleware.Auth tolerates an
		// empty SessionID, and we don't want synthetic runs to require a row
		// in the user_sessions table (which would force a cleanup discipline
		// and rate-limit our test cadence).
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(jwtSecret)
	if err != nil {
		return "", fmt.Errorf("sign session token: %w", err)
	}
	return signed, nil
}
