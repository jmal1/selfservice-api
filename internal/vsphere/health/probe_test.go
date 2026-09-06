package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew_validatesRequired(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing url", Config{User: "u", Password: "p"}, "VCenterURL"},
		{"missing auth", Config{VCenterURL: "https://v/sdk"}, "User+Password"},
		{"timeout too small", Config{VCenterURL: "https://v/sdk", User: "u", Password: "p", ProbeTimeout: 500 * time.Millisecond}, "ProbeTimeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg, nil)
			if err == nil {
				t.Fatalf("expected error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNew_defaults(t *testing.T) {
	p, err := New(Config{VCenterURL: "https://v/sdk", User: "u@vsphere.local", Password: "p"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.cfg.ProbeTimeout != 30*time.Second {
		t.Errorf("ProbeTimeout default = %v, want 30s", p.cfg.ProbeTimeout)
	}
	if p.cfg.Job != "crucible_vsphere_health" {
		t.Errorf("Job default = %q, want crucible_vsphere_health", p.cfg.Job)
	}
}

func TestPushURL_isDeterministic(t *testing.T) {
	// Same logical labels in two different insertion orders must produce
	// the same URL — Pushgateway treats different orderings as different
	// groupings, which would leak orphan metrics forever.
	a := pushURL("http://pg:9091", "j", map[string]string{"env": "prod", "layer": "vsphere"})
	b := pushURL("http://pg:9091/", "j", map[string]string{"layer": "vsphere", "env": "prod"})
	if a != b {
		t.Fatalf("non-deterministic URL:\n  a=%q\n  b=%q", a, b)
	}
	want := "http://pg:9091/metrics/job/j/env/prod/layer/vsphere"
	if a != want {
		t.Fatalf("URL = %q, want %q", a, want)
	}
}

func TestSerialize_includesAllFourMetrics(t *testing.T) {
	at := time.Unix(1700000000, 0)
	body := serialize(Result{
		Success: true, Duration: 1234 * time.Millisecond, At: at,
	}, "selfservice-svc@vsphere.local")
	got := string(body)
	wants := []string{
		"vsphere_login_success{user=\"selfservice-svc\"} 1",
		"vsphere_login_duration_seconds{user=\"selfservice-svc\"} 1.234",
		"vsphere_health_check_info{user=\"selfservice-svc\",error=\"\"} 1",
		"vsphere_health_run_timestamp_seconds 1700000000",
		"# TYPE vsphere_login_success gauge",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("body missing %q\nfull body:\n%s", w, got)
		}
	}
}

func TestSerialize_failureUsesClassifiedError(t *testing.T) {
	body := serialize(Result{
		Success: false, Duration: 100 * time.Millisecond, At: time.Unix(0, 0),
		Err: errors.New("ServerFaultCode: Cannot complete login due to an incorrect user name or password."),
	}, "selfservice-svc@vsphere.local")
	got := string(body)
	if !strings.Contains(got, "vsphere_login_success{user=\"selfservice-svc\"} 0") {
		t.Errorf("success metric should be 0 on failure: %s", got)
	}
	if !strings.Contains(got, "error=\"invalid_credentials\"") {
		t.Errorf("info metric should classify as invalid_credentials, got: %s", got)
	}
	// The raw fault message must NOT appear as a metric label.
	if strings.Contains(got, "ServerFaultCode") {
		t.Errorf("raw error text leaked into label body:\n%s", got)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("ServerFaultCode: Cannot complete login due to an incorrect user name or password."), "invalid_credentials"},
		{errors.New("InvalidLogin: foo"), "invalid_credentials"},
		{errors.New("Post \"https://vc/sdk\": context deadline exceeded"), "timeout"},
		{errors.New("x509: certificate signed by unknown authority"), "tls"},
		{errors.New("tls: handshake failure"), "tls"},
		{errors.New("dial tcp: lookup vcenter.lab.jmal.io: no such host"), "network"},
		{errors.New("dial tcp 10.10.10.50:443: connection refused"), "network"},
		{errors.New("parse url: missing scheme"), "config"},
		{errors.New("something unexpected"), "other"},
	}
	for _, tc := range cases {
		got := classifyError(tc.err)
		if got != tc.want {
			t.Errorf("classifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestRun_pushesToPushgateway(t *testing.T) {
	// Build a probe that always fails its login (bad URL forces the
	// govmomi client to error before any network) — we only care that
	// the push body gets to the pushgateway with the expected shape.
	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(r.Context())
		capturedBody, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := New(Config{
		VCenterURL:     "https://127.0.0.1:1/sdk",
		User:           "probe@vsphere.local",
		Password:       "x",
		Insecure:       true,
		ProbeTimeout:   2 * time.Second,
		PushgatewayURL: srv.URL,
		Job:            "test_job",
		GroupingLabels: map[string]string{"layer": "vsphere"},
		HTTP:           srv.Client(),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if res.Success {
		t.Errorf("probe to 127.0.0.1:1 should have failed but reported success")
	}
	if captured == nil {
		t.Fatal("pushgateway never received a request")
	}
	wantPath := "/metrics/job/test_job/layer/vsphere"
	if captured.URL.Path != wantPath {
		t.Errorf("push path = %q, want %q", captured.URL.Path, wantPath)
	}
	if !bytes.Contains(capturedBody, []byte("vsphere_login_success{user=\"probe\"} 0")) {
		t.Errorf("body missing failure gauge:\n%s", string(capturedBody))
	}
}

func TestRun_skipPushWhenURLEmpty(t *testing.T) {
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p, err := New(Config{
		VCenterURL: "https://127.0.0.1:1/sdk", User: "u@v", Password: "p",
		Insecure: true, ProbeTimeout: 2 * time.Second,
		// PushgatewayURL deliberately empty
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called != 0 {
		t.Errorf("pushgateway called %d times despite empty URL", called)
	}
}

func TestRun_pushFailureSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "boom")
	}))
	defer srv.Close()
	p, err := New(Config{
		VCenterURL: "https://127.0.0.1:1/sdk", User: "u@v", Password: "p",
		Insecure: true, ProbeTimeout: 2 * time.Second,
		PushgatewayURL: srv.URL, Job: "j", HTTP: srv.Client(),
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.Run(context.Background())
	if err == nil {
		t.Fatal("expected push error, got nil")
	}
	if !strings.Contains(err.Error(), "pushgateway 500") {
		t.Errorf("expected pushgateway 500 in error, got %v", err)
	}
}
