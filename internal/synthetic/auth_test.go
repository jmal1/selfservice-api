package synthetic

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jmal1/selfservice-api/internal/auth"
)

// TestMintSessionToken_RoundTrip mints a token and then parses it with the
// same shape the API uses (HMAC + SessionClaims) to prove the token would be
// accepted by middleware.Auth without modification.
func TestMintSessionToken_RoundTrip(t *testing.T) {
	secret := []byte("test-secret-do-not-use-in-prod")
	uid := "11111111-1111-1111-1111-111111111111"
	const username = "synthetic"
	const role = "student"

	signed, err := MintSessionToken(secret, uid, username, role, 5*time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if signed == "" {
		t.Fatal("mint returned empty token")
	}
	// JWTs are always 3 dot-separated base64 segments. A regression here means
	// the underlying library swapped serializers.
	if got := strings.Count(signed, "."); got != 2 {
		t.Fatalf("expected 3 JWT segments (2 dots), got %d in %q", got+1, signed)
	}

	parsed, err := jwt.ParseWithClaims(signed, &auth.SessionClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return secret, nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("parsed token not valid")
	}
	claims, ok := parsed.Claims.(*auth.SessionClaims)
	if !ok {
		t.Fatalf("unexpected claims type %T", parsed.Claims)
	}
	if claims.UserID != uid {
		t.Errorf("UserID = %q, want %q", claims.UserID, uid)
	}
	if claims.Username != username {
		t.Errorf("Username = %q, want %q", claims.Username, username)
	}
	if claims.Role != role {
		t.Errorf("Role = %q, want %q", claims.Role, role)
	}
	if claims.SessionID != "" {
		t.Errorf("SessionID = %q, want empty", claims.SessionID)
	}
	if claims.Issuer != "selfservice-synthetic-api-monitor" {
		t.Errorf("Issuer = %q, want synthetic-api-monitor", claims.Issuer)
	}
}

// TestMintSessionToken_RejectsBadInput catches the four common
// misconfigurations: empty secret, empty user id, zero ttl, negative ttl.
func TestMintSessionToken_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name      string
		secret    []byte
		uid       string
		ttl       time.Duration
		wantError string
	}{
		{"empty secret", []byte{}, "u", time.Minute, "jwt secret is empty"},
		{"empty uid", []byte("s"), "", time.Minute, "userID is empty"},
		{"zero ttl", []byte("s"), "u", 0, "ttl must be positive"},
		{"negative ttl", []byte("s"), "u", -time.Second, "ttl must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MintSessionToken(tc.secret, tc.uid, "u", "student", tc.ttl)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantError)
			}
		})
	}
}

// TestMintSessionToken_WrongSecretFailsVerify ensures that a different secret
// truly rejects the token — this protects against an accidental change to a
// non-HMAC signer that would silently accept any signature.
func TestMintSessionToken_WrongSecretFailsVerify(t *testing.T) {
	signed, err := MintSessionToken([]byte("right"), "u", "synthetic", "student", time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = jwt.ParseWithClaims(signed, &auth.SessionClaims{}, func(t *jwt.Token) (any, error) {
		return []byte("wrong"), nil
	})
	if err == nil {
		t.Fatal("expected verification failure with wrong secret, got nil")
	}
}
