package vcenter

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25"
)

func TestCertGovmomiClient_WiresSessionManager(t *testing.T) {
	// Regression for 82239b1 CrashLoop: &govmomi.Client{Client: vc} left
	// SessionManager nil so Logout/UserSession panicked under cert auth.
	vc := new(vim25.Client)
	c := certGovmomiClient(vc, nil)
	if c.SessionManager == nil {
		t.Fatal("SessionManager must be non-nil")
	}
	if c.Client != vc {
		t.Fatal("vim25 client must be preserved")
	}
}

func TestConfig_HasClientCertificate(t *testing.T) {
	certPEM, keyPEM := mustTestCertPEM(t)
	cfg := Config{ClientCertPEM: certPEM, ClientKeyPEM: keyPEM}
	if !cfg.HasClientCertificate() {
		t.Fatal("expected HasClientCertificate true")
	}
	if _, err := cfg.parseClientCertificate(); err != nil {
		t.Fatalf("parse: %v", err)
	}
	bad := Config{ClientCertPEM: certPEM}
	if bad.HasClientCertificate() {
		t.Fatal("cert without key must be false")
	}
}

func mustTestCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "crucible-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
