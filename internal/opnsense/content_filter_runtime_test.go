package opnsense

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeDNSBLRuntimeExpectationsRequiresExactOwnedSingleSource(t *testing.T) {
	valid := DNSBLPolicy{
		UUID:        "uuid",
		Description: "crucible:content-filter:v1:dnsbl",
		SourceNets:  "10.100.0.0/16",
	}
	if got, err := normalizeDNSBLRuntimeExpectations([]DNSBLPolicy{valid}); err != nil ||
		len(got) != 1 || got[0].SourceNets[0] != "10.100.0.0/16" {
		t.Fatalf("valid runtime expectation = %+v, %v", got, err)
	}
	for _, policy := range []DNSBLPolicy{
		{Description: valid.Description, SourceNets: valid.SourceNets},
		{UUID: "uuid", Description: "manual", SourceNets: valid.SourceNets},
		{UUID: "uuid", Description: valid.Description, SourceNets: "10.100.0.0/16,10.101.0.0/16"},
		{UUID: "uuid", Description: valid.Description, SourceNets: "10.100.1.1/24"},
	} {
		if _, err := normalizeDNSBLRuntimeExpectations([]DNSBLPolicy{policy}); err == nil {
			t.Fatalf("accepted unsafe runtime expectation %+v", policy)
		}
	}
	if _, err := normalizeDNSBLRuntimeExpectations([]DNSBLPolicy{valid, valid}); err == nil {
		t.Fatal("accepted duplicate runtime UUID")
	}
}

func TestCheckStudentIPv6InternetRouteScopesToStudentInterfaces(t *testing.T) {
	tests := []struct {
		name       string
		routes     string
		interfaces string
		students   []string
		wantErr    string
	}{
		{
			name:       "no IPv6 default",
			routes:     `[{"destination":"0.0.0.0/0","gateway":"10.10.10.1","netif":"vtnet0"}]`,
			interfaces: `{"opt7":{"ipv6":[{"ipaddr":"fe80::7"}]}}`,
			students:   []string{"opt7"},
		},
		{
			name:       "management IPv6 does not taint student",
			routes:     `[{"destination":"::/0","gateway":"2001:db8::1","netif":"vtnet0"}]`,
			interfaces: `{"opt7":{"ipv6":[{"ipaddr":"fe80::7"}]},"lan":{"ipv6":[{"ipaddr":"2001:4860::10"}]}}`,
			students:   []string{"opt7"},
		},
		{
			name:       "student global IPv6 fails",
			routes:     `[{"destination":"default","gateway":"2001:4860::1","netif":"vtnet0"}]`,
			interfaces: `{"opt7":{"ipv6":[{"ipaddr":"2001:4860:1::7"}]}}`,
			students:   []string{"opt7"},
			wantErr:    "non-link-local IPv6",
		},
		{
			name:       "student ULA with link gateway fails",
			routes:     `[{"destination":"::/0","gateway":"link#12","netif":"opt7"}]`,
			interfaces: `{"opt7":{"ipv6":[{"ipaddr":"fd00:100::7"}]}}`,
			students:   []string{"opt7"},
			wantErr:    "non-link-local IPv6",
		},
		{
			name:       "student ULA without default still fails",
			routes:     `[{"destination":"0.0.0.0/0","gateway":"10.10.10.1","netif":"vtnet0"}]`,
			interfaces: `{"opt7":{"ipv6":[{"ipaddr":"fd00:100::7"}]}}`,
			students:   []string{"opt7"},
			wantErr:    "student IPv6 is unsupported",
		},
		{
			name:       "missing student interface fails closed",
			routes:     `[{"destination":"::/0","gateway":"2001:4860::1","netif":"vtnet0"}]`,
			interfaces: `{"opt8":{"ipv6":[]}}`,
			students:   []string{"opt7"},
			wantErr:    "absent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/diagnostics/interface/getRoutes":
					_, _ = io.WriteString(w, tt.routes)
				case "/diagnostics/interface/getInterfaceConfig":
					_, _ = io.WriteString(w, tt.interfaces)
				default:
					http.Error(w, "unexpected path", http.StatusNotFound)
				}
			}))
			defer srv.Close()
			client := New(Config{BaseURL: srv.URL}, discardLogger())
			err := client.CheckStudentIPv6InternetRoute(context.Background(), tt.students)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestContentFilterTransactionSerializesFirewallApply(t *testing.T) {
	enteredApply := make(chan struct{})
	releaseApply := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/firewall/filter/apply" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		once.Do(func() { close(enteredApply) })
		<-releaseApply
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	client := New(Config{BaseURL: srv.URL}, discardLogger())

	txCtx, release, err := client.BeginContentFilterTransaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	txDone := make(chan error, 1)
	go func() { txDone <- client.ApplyFirewall(txCtx) }()
	<-enteredApply

	outsideDone := make(chan error, 1)
	go func() { outsideDone <- client.ApplyFirewall(context.Background()) }()
	select {
	case err := <-outsideDone:
		t.Fatalf("outside apply bypassed transaction lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseApply)
	if err := <-txDone; err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-outsideDone; err != nil {
		t.Fatal(err)
	}
}

func TestContentFilterTransactionHonorsCanceledWaiter(t *testing.T) {
	client := New(Config{}, discardLogger())
	_, release, err := client.BeginContentFilterTransaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.BeginContentFilterTransaction(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
}

func TestContentFilterTransactionDetachedContextRetainsOwnership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/firewall/filter/apply" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	client := New(Config{BaseURL: srv.URL}, discardLogger())

	txCtx, release, err := client.BeginContentFilterTransaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	detached, cancel := context.WithTimeout(context.WithoutCancel(txCtx), time.Second)
	defer cancel()
	if err := client.ApplyFirewall(detached); err != nil {
		t.Fatalf("detached transaction context lost policy-lock ownership: %v", err)
	}
}
