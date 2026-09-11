package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestSessionSigner_HMACRoundTrip(t *testing.T) {
	s, err := NewHMACSessionSigner([]byte("test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u1",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			Issuer:    "test",
		},
		UserID:   "u1",
		Username: "alice",
		Role:     "student",
	}
	tok, err := s.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseWithClaims(tok, &SessionClaims{}, s.Keyfunc)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parsed.Claims.(*SessionClaims)
	if !ok || !parsed.Valid || got.Username != "alice" {
		t.Fatalf("bad claims: %#v valid=%v", got, parsed.Valid)
	}
	if s.Alg() != "HS256" {
		t.Fatalf("alg=%s", s.Alg())
	}
}

func TestSessionSigner_RSARoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	s, err := NewRSASessionSigner(privPEM, pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "u2",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
			Issuer:    "test",
		},
		UserID: "u2", Username: "bob", Role: "admin",
	}
	tok, err := s.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseWithClaims(tok, &SessionClaims{}, s.Keyfunc)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}
	if s.Alg() != "RS256" {
		t.Fatalf("alg=%s", s.Alg())
	}
}
