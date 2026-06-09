package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/vsphere/health"
)

// stubNATS is a hand-rolled implementation of natsHealth for tests.
// Avoids spinning up an embedded NATS server just to check the probe
// branches.
type stubNATS struct {
	connected bool
	url       string
}

func (s stubNATS) IsConnected() bool    { return s.connected }
func (s stubNATS) ConnectedURL() string { return s.url }

// stubVSphere is a hand-rolled vsphereProbe for tests.
type stubVSphere struct {
	res Result
	ok  bool
}

// Result is just an alias so tests don't need to import the vsphere
// package; we redefine it here for clarity but it must structurally
// match health.Result.
type Result = health.Result

func (s stubVSphere) LastResult() (health.Result, bool) { return s.res, s.ok }

func TestAdminHealth_AllNotConfigured(t *testing.T) {
	// Zero-value deps. Endpoint should return 200, all deps not_configured,
	// rollup not_configured. This is the "dev workstation" case where the
	// api-gateway is running with no backing services wired.
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	rec := httptest.NewRecorder()
	h.AdminHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var resp healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != healthNotConfigured {
		t.Errorf("rollup: want %s, got %s", healthNotConfigured, resp.Status)
	}
	if len(resp.Deps) != 5 {
		t.Errorf("deps: want 5, got %d", len(resp.Deps))
	}
	for _, d := range resp.Deps {
		if d.Status != healthNotConfigured {
			t.Errorf("%s: want %s, got %s (detail=%q)", d.Name, healthNotConfigured, d.Status, d.Detail)
		}
	}
}

func TestAdminHealth_DepsSortedByName(t *testing.T) {
	// Probes run concurrently; the handler must sort by name so the UI
	// doesn't reshuffle on every poll. Regression guard.
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	rec := httptest.NewRecorder()
	h.AdminHealth(rec, req)

	var resp healthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	for i := 1; i < len(resp.Deps); i++ {
		if resp.Deps[i-1].Name > resp.Deps[i].Name {
			t.Errorf("deps out of order: %s before %s", resp.Deps[i-1].Name, resp.Deps[i].Name)
		}
	}
}

func TestProbeNATS(t *testing.T) {
	tests := []struct {
		name      string
		nats      natsHealth
		wantState healthStatus
	}{
		{"nil_client", nil, healthNotConfigured},
		{"connected", stubNATS{connected: true, url: "nats://localhost:4222"}, healthOK},
		{"disconnected", stubNATS{connected: false}, healthDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := probeNATS(tc.nats)
			if d.Status != tc.wantState {
				t.Errorf("status: want %s, got %s", tc.wantState, d.Status)
			}
			if d.Name != "nats" {
				t.Errorf("name: %q", d.Name)
			}
		})
	}
}

func TestProbeVSphere(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		probe     vsphereProbe
		wantState healthStatus
	}{
		{"nil_probe", nil, healthNotConfigured},
		{"no_cycle_yet", stubVSphere{ok: false}, healthDegraded},
		{"success_fresh", stubVSphere{ok: true, res: health.Result{Success: true, At: now, Duration: 100 * time.Millisecond}}, healthOK},
		{"success_stale", stubVSphere{ok: true, res: health.Result{Success: true, At: now.Add(-30 * time.Minute), Duration: 100 * time.Millisecond}}, healthDegraded},
		{"failure_fresh", stubVSphere{ok: true, res: health.Result{Success: false, At: now, Err: errors.New("login failed")}}, healthDown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := probeVSphere(tc.probe)
			if d.Status != tc.wantState {
				t.Errorf("status: want %s, got %s (detail=%q)", tc.wantState, d.Status, d.Detail)
			}
		})
	}
}

func TestProbeOPNsense(t *testing.T) {
	t.Run("not_configured", func(t *testing.T) {
		d := probeOPNsense(context.Background(), "", nil)
		if d.Status != healthNotConfigured {
			t.Errorf("want %s, got %s", healthNotConfigured, d.Status)
		}
	})

	t.Run("ok_200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/api/diagnostics/firmware/status") {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		d := probeOPNsense(context.Background(), srv.URL, srv.Client())
		if d.Status != healthOK {
			t.Errorf("want %s, got %s (detail=%q)", healthOK, d.Status, d.Detail)
		}
	})

	t.Run("ok_401_means_reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		d := probeOPNsense(context.Background(), srv.URL, srv.Client())
		if d.Status != healthOK {
			t.Errorf("401 should be ok (reachable but unauthenticated), got %s", d.Status)
		}
	})

	t.Run("degraded_unexpected_status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		d := probeOPNsense(context.Background(), srv.URL, srv.Client())
		if d.Status != healthDegraded {
			t.Errorf("want %s, got %s", healthDegraded, d.Status)
		}
	})

	t.Run("down_unreachable", func(t *testing.T) {
		// Point at a closed port; should fail fast with connection refused.
		d := probeOPNsense(context.Background(), "http://127.0.0.1:1", &http.Client{Timeout: 2 * time.Second})
		if d.Status != healthDown {
			t.Errorf("want %s, got %s (detail=%q)", healthDown, d.Status, d.Detail)
		}
	})
}

func TestProbeEngine(t *testing.T) {
	t.Run("not_configured", func(t *testing.T) {
		d := probeEngine(context.Background(), "", nil)
		if d.Status != healthNotConfigured {
			t.Errorf("want %s, got %s", healthNotConfigured, d.Status)
		}
	})

	t.Run("ok_with_engine_id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"status":"ok","engine_id":"engine-abc-123"}`)
		}))
		defer srv.Close()
		d := probeEngine(context.Background(), srv.URL+"/healthz", srv.Client())
		if d.Status != healthOK {
			t.Errorf("want %s, got %s", healthOK, d.Status)
		}
		if !strings.Contains(d.Detail, "engine-abc-123") {
			t.Errorf("detail should include engine_id; got %q", d.Detail)
		}
	})

	t.Run("down_500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		d := probeEngine(context.Background(), srv.URL+"/healthz", srv.Client())
		if d.Status != healthDown {
			t.Errorf("want %s, got %s", healthDown, d.Status)
		}
	})
}

func TestRollupStatus(t *testing.T) {
	tests := []struct {
		name string
		in   []depResult
		want healthStatus
	}{
		{"all_ok", []depResult{{Status: healthOK}, {Status: healthOK}}, healthOK},
		{"one_down", []depResult{{Status: healthOK}, {Status: healthDown}}, healthDown},
		{"one_degraded", []depResult{{Status: healthOK}, {Status: healthDegraded}}, healthDegraded},
		{"down_beats_degraded", []depResult{{Status: healthDegraded}, {Status: healthDown}}, healthDown},
		{"all_not_configured", []depResult{{Status: healthNotConfigured}, {Status: healthNotConfigured}}, healthNotConfigured},
		{"ok_plus_not_configured_is_ok", []depResult{{Status: healthOK}, {Status: healthNotConfigured}}, healthOK},
		{"empty", nil, healthNotConfigured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rollupStatus(tc.in)
			if got != tc.want {
				t.Errorf("want %s, got %s", tc.want, got)
			}
		})
	}
}
