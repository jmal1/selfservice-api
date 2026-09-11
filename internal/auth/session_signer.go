package auth

import (
	"crypto/rsa"
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// SessionSigner signs and verifies Crucible session JWTs.
// Production default remains HS256 (JWT_SECRET). Prefer RS256 via
// JWT_PRIVATE_KEY + JWT_PUBLIC_KEY (Vault → ESO) so the shared HMAC is not a
// long-lived password-equivalent that must stay in sync across binaries.
type SessionSigner interface {
	Sign(claims jwt.Claims) (string, error)
	Keyfunc(token *jwt.Token) (any, error)
	Alg() string
}

type hmacSigner struct {
	secret []byte
}

func (s hmacSigner) Sign(claims jwt.Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
}

func (s hmacSigner) Keyfunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
	}
	return s.secret, nil
}

func (s hmacSigner) Alg() string { return "HS256" }

type rsaSigner struct {
	private *rsa.PrivateKey
	public  *rsa.PublicKey
}

func (s rsaSigner) Sign(claims jwt.Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.private)
}

func (s rsaSigner) Keyfunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
		return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
	}
	return s.public, nil
}

func (s rsaSigner) Alg() string { return "RS256" }

// NewSessionSignerFromEnv builds a SessionSigner.
// Prefer JWT_PRIVATE_KEY + JWT_PUBLIC_KEY (PEM). Fall back to JWT_SECRET (HMAC).
func NewSessionSignerFromEnv() (SessionSigner, error) {
	privPEM := os.Getenv("JWT_PRIVATE_KEY")
	pubPEM := os.Getenv("JWT_PUBLIC_KEY")
	if privPEM != "" || pubPEM != "" {
		if privPEM == "" || pubPEM == "" {
			return nil, fmt.Errorf("JWT_PRIVATE_KEY and JWT_PUBLIC_KEY must both be set for RS256")
		}
		return NewRSASessionSigner([]byte(privPEM), []byte(pubPEM))
	}
	secret := []byte(os.Getenv("JWT_SECRET"))
	if len(secret) == 0 {
		return nil, fmt.Errorf("JWT_SECRET or JWT_PRIVATE_KEY+JWT_PUBLIC_KEY is required")
	}
	return hmacSigner{secret: secret}, nil
}

// NewHMACSessionSigner returns an HS256 signer (tests + synthetic mint).
func NewHMACSessionSigner(secret []byte) (SessionSigner, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("jwt secret is empty")
	}
	return hmacSigner{secret: secret}, nil
}

// NewRSASessionSigner parses PEM private + public keys for RS256.
func NewRSASessionSigner(privatePEM, publicPEM []byte) (SessionSigner, error) {
	priv, err := jwt.ParseRSAPrivateKeyFromPEM(privatePEM)
	if err != nil {
		return nil, fmt.Errorf("parse JWT_PRIVATE_KEY: %w", err)
	}
	pub, err := jwt.ParseRSAPublicKeyFromPEM(publicPEM)
	if err != nil {
		return nil, fmt.Errorf("parse JWT_PUBLIC_KEY: %w", err)
	}
	return rsaSigner{private: priv, public: pub}, nil
}
