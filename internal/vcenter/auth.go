package vcenter

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/sts"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

// HasClientCertificate reports whether PEM cert+key are both set for STS login.
func (c Config) HasClientCertificate() bool {
	return len(c.ClientCertPEM) > 0 && len(c.ClientKeyPEM) > 0
}

// HasPasswordAuth reports whether classic SSO user/password is configured.
func (c Config) HasPasswordAuth() bool {
	return c.User != "" && c.Password != ""
}

func (c Config) parseClientCertificate() (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(c.ClientCertPEM, c.ClientKeyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse vCenter client certificate: %w", err)
	}
	return cert, nil
}

// Authenticate opens a govmomi client using client-certificate STS login when
// PEMs are set, otherwise classic SSO username/password.
func Authenticate(ctx context.Context, cfg Config) (*govmomi.Client, error) {
	u, err := soap.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse vCenter URL: %w", err)
	}
	if cfg.HasClientCertificate() {
		cert, cerr := cfg.parseClientCertificate()
		if cerr != nil {
			return nil, cerr
		}
		return loginWithCertificate(ctx, u, cert, cfg.Insecure)
	}
	if !cfg.HasPasswordAuth() {
		return nil, fmt.Errorf("vCenter auth requires VCENTER_CLIENT_CERT+VCENTER_CLIENT_KEY or VCENTER_USER+VCENTER_PASSWORD")
	}
	u.User = url.UserPassword(cfg.User, cfg.Password)
	return loginWithPassword(ctx, u, cfg.Insecure)
}

func loginWithCertificate(ctx context.Context, u *url.URL, cert tls.Certificate, insecure bool) (*govmomi.Client, error) {
	sc := soap.NewClient(u, insecure)
	sc.SetCertificate(cert)

	vc, err := vim25.NewClient(ctx, sc)
	if err != nil {
		return nil, fmt.Errorf("vim25 client: %w", err)
	}

	stsClient, err := sts.NewClient(ctx, vc)
	if err != nil {
		return nil, fmt.Errorf("sts client: %w", err)
	}

	signer, err := stsClient.Issue(ctx, sts.TokenRequest{
		Certificate: &cert,
		Lifetime:    10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("sts Issue: %w", err)
	}

	// LoginByToken requires the SOAPAction version from
	// /sdk/vimServiceVersions.xml. govmomi defaults to vim25.Version (9.1.0.0),
	// which VCSA 8 rejects with VersionMismatchFaultCode. Same guard as govc.
	if vc.Version == vim25.Version {
		if err := vc.UseServiceVersion(); err != nil {
			return nil, fmt.Errorf("vim25 UseServiceVersion: %w", err)
		}
	}

	// Mirror govmomi.NewClient: SessionManager must be set on the returned
	// client. LoginByToken alone authenticates the SOAP session but leaves
	// Client.SessionManager nil — Logout/UserSession then panic (API health
	// probe and worker DRS privilege checks).
	sm := session.NewManager(vc)
	header := soap.Header{Security: signer}
	if err := sm.LoginByToken(vc.WithHeader(ctx, header)); err != nil {
		return nil, fmt.Errorf("LoginByToken: %w", err)
	}

	return certGovmomiClient(vc, sm), nil
}

// certGovmomiClient binds an authenticated vim25 client to a SessionManager.
func certGovmomiClient(vc *vim25.Client, sm *session.Manager) *govmomi.Client {
	if sm == nil {
		sm = session.NewManager(vc)
	}
	return &govmomi.Client{Client: vc, SessionManager: sm}
}

func loginWithPassword(ctx context.Context, u *url.URL, insecure bool) (*govmomi.Client, error) {
	client, err := govmomi.NewClient(ctx, u, insecure)
	if err != nil {
		return nil, err
	}
	return client, nil
}
