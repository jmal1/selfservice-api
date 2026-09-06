package synthetic

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jmal1/selfservice-api/internal/auth"
)

// MintSessionToken issues a session JWT identical in shape to the one issued
// by Provider.LoginHandler after a successful OIDC login. Prefer
// MintSessionTokenWithSigner when the API uses RS256; the HMAC path remains
// for backward compatibility with SYNTHETIC_JWT_SECRET.
//
// ttl SHOULD be short (a few minutes) — synthetic runs complete in seconds, so
// long-lived tokens add risk without value. The returned token is suitable for
// the `session` cookie consumed by middleware.Auth.
//
// userID, username, and role MUST match the synthetic user's DB row;
// middleware.Auth re-uses claims.UserID directly for DB lookups, so a stale or
// wrong UUID causes a 401 (not a useful error message).
func MintSessionToken(jwtSecret []byte, userID, username, role string, ttl time.Duration) (string, error) {
	signer, err := auth.NewHMACSessionSigner(jwtSecret)
	if err != nil {
		return "", err
	}
	return MintSessionTokenWithSigner(signer, userID, username, role, ttl)
}

// MintSessionTokenWithSigner signs with the same SessionSigner the API uses
// (HS256 or RS256).
func MintSessionTokenWithSigner(signer auth.SessionSigner, userID, username, role string, ttl time.Duration) (string, error) {
	if signer == nil {
		return "", fmt.Errorf("session signer is nil")
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

	signed, err := signer.Sign(claims)
	if err != nil {
		return "", fmt.Errorf("sign session token: %w", err)
	}
	return signed, nil
}
