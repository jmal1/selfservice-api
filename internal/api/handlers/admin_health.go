package handlers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/vsphere/health"
)

// HealthDeps is the dependency bag the /admin/health handler probes. Each
// field is optional: nil means the corresponding probe is skipped and
// reported as "not_configured" in the response. The handler never panics
// on missing deps — operators may be running a stripped-down api-gateway
// in dev with only DB + NATS wired, and the dashboard should reflect that
// honestly rather than show fake-green for unconfigured services.
type HealthDeps struct {
	// Pool is the pgx connection pool. Probe = Pool.Ping(ctx). Required
	// for the handler to be useful at all; if nil, the /admin/health
	// endpoint still works but db reports "not_configured".
	Pool *pgxpool.Pool

	// NATS is the broker connection wrapper. Probe is a non-blocking
	// IsConnected check (no round-trip) plus the connected URL for the
	// detail panel.
	NATS natsHealth

	// VSphere is the in-process credential probe. We read the most-recent
	// Result (LastResult()) instead of triggering a fresh login so /admin/health
	// stays fast (< 100ms p99). If the last result is stale by more than
	// vsphereResultStaleAfter, we report "stale" rather than the cached value.
	VSphere vsphereProbe

	// OPNsense is an HTTP probe target. We do a 5s GET to OPNsenseBaseURL
	// (or /api/diagnostics/firmware/status if available) so the probe
	// covers both network reachability and basic API auth.
	OPNsenseBaseURL string
	// OPNsenseHTTP overrides the HTTP client used for OPNsense probes
	// (tests inject httptest.Server.Client()). nil => default 5s-timeout client
	// that skips TLS verification (matches the worker's setting; lab uses
	// self-signed certs).
	OPNsenseHTTP *http.Client

	// EngineHealthURL is the engine's /healthz endpoint, e.g.
	// http://crucible-engine.selfservice.svc.cluster.local:8081/healthz.
	// Empty disables the probe; reported as "not_configured".
	EngineHealthURL string
	// EngineHTTP overrides the HTTP client used for engine probes (tests
	// inject httptest.Server.Client()). nil => default 5s-timeout client.
	EngineHTTP *http.Client
}

// natsHealth is the small surface /admin/health needs from the NATS client.
// Defined as an interface so tests can stub without spinning up a broker.
type natsHealth interface {
	IsConnected() bool
	ConnectedURL() string
}

// vsphereProbe is the small surface /admin/health needs from the in-process
// vSphere health probe. Defined as an interface so tests can stub.
type vsphereProbe interface {
	LastResult() (health.Result, bool)
}

// vsphereResultStaleAfter is how old the last vSphere probe result can be
// before /admin/health flags it stale. Tuned to 3× the production probe
// interval (5m) so a single missed cycle isn't flagged; two missed cycles
// is suspicious and worth surfacing.
const vsphereResultStaleAfter = 16 * time.Minute

// probeTimeout caps each parallel dep probe. Tight enough that the overall
// handler stays sub-second even if all deps hang; loose enough that a slow
// vCenter or OPNsense API doesn't false-fail on a transient blip.
const probeTimeout = 5 * time.Second

// healthStatus is the per-dep status value. Stays a small enum so the UI
// can colour-code without parsing strings.
type healthStatus string

const (
	healthOK             healthStatus = "ok"
	healthDegraded       healthStatus = "degraded"   // up but stale / partial
	healthDown           healthStatus = "down"       // probe failed
	healthNotConfigured  healthStatus = "not_configured"
)

// depResult is one dependency probe outcome. The shape is part of the API
// contract: the UI parses these fields directly. Don't rename without
// updating selfservice-ui src/lib/types/health.ts.
type depResult struct {
	Name      string       `json:"name"`
	Status    healthStatus `json:"status"`
	LatencyMS int64        `json:"latency_ms"`
	LastCheck time.Time    `json:"last_check"`
	Detail    string       `json:"detail,omitempty"`
}

// healthResponse is the /admin/health envelope. Status is the worst-case
// rollup across all deps so the UI can show a single banner without doing
// the rollup itself.
type healthResponse struct {
	Status healthStatus `json:"status"`
	Deps   []depResult  `json:"deps"`
}

// WithHealthDeps wires the dependency bag /admin/health probes. Returns
// the receiver for fluent chaining alongside WithVCenterFolders.
func (h *Handler) WithHealthDeps(deps HealthDeps) *Handler {
	h.healthDeps = deps
	return h
}

// AdminHealth returns the per-dependency health status of the platform.
// Probes db, nats, vcenter, opnsense, and engine in parallel with a 5s
// per-probe timeout. Always returns 200 — the body is the source of truth;
// returning non-200 would defeat the point (a wrapping monitoring system
// would mark the endpoint itself as down rather than reading the body).
//
// Auth: admin-only. The detail strings can leak internal hostnames and
// error messages from the probed services.
func (h *Handler) AdminHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	deps := h.healthDeps

	// Each probe runs in its own goroutine with its own bounded context.
	// We collect into a slice via mutex (smaller surface than channels for
	// a fixed-size fan-out). Per-probe timeouts are enforced inside the
	// probe funcs so a slow dep can't drag the others.
	var (
		mu      sync.Mutex
		results = make([]depResult, 0, 5)
		wg      sync.WaitGroup
	)
	add := func(d depResult) {
		mu.Lock()
		results = append(results, d)
		mu.Unlock()
	}

	wg.Add(5)
	go func() { defer wg.Done(); add(probeDB(ctx, deps.Pool)) }()
	go func() { defer wg.Done(); add(probeNATS(deps.NATS)) }()
	go func() { defer wg.Done(); add(probeVSphere(deps.VSphere)) }()
	go func() { defer wg.Done(); add(probeOPNsense(ctx, deps.OPNsenseBaseURL, deps.OPNsenseHTTP)) }()
	go func() { defer wg.Done(); add(probeEngine(ctx, deps.EngineHealthURL, deps.EngineHTTP)) }()
	wg.Wait()

	// Deterministic order so the UI doesn't reshuffle every poll.
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	resp := healthResponse{
		Status: rollupStatus(results),
		Deps:   results,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// rollupStatus picks the worst-case status across all probed deps. Order:
// down > degraded > ok > not_configured. not_configured is treated as a
// neutral state (we can't claim health for a thing we aren't checking) so
// it doesn't drag the overall status down — but if everything is
// not_configured, that's reported faithfully as not_configured.
func rollupStatus(deps []depResult) healthStatus {
	worst := healthNotConfigured
	rank := func(s healthStatus) int {
		switch s {
		case healthDown:
			return 3
		case healthDegraded:
			return 2
		case healthOK:
			return 1
		default: // not_configured
			return 0
		}
	}
	for _, d := range deps {
		if rank(d.Status) > rank(worst) {
			worst = d.Status
		}
	}
	return worst
}

func probeDB(ctx context.Context, pool *pgxpool.Pool) depResult {
	d := depResult{Name: "db", LastCheck: time.Now().UTC()}
	if pool == nil {
		d.Status = healthNotConfigured
		d.Detail = "database pool not wired"
		return d
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	err := pool.Ping(ctx)
	d.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		d.Status = healthDown
		d.Detail = err.Error()
		return d
	}
	d.Status = healthOK
	d.Detail = fmt.Sprintf("pool stats: %d/%d", pool.Stat().AcquiredConns(), pool.Stat().MaxConns())
	return d
}

func probeNATS(c natsHealth) depResult {
	d := depResult{Name: "nats", LastCheck: time.Now().UTC()}
	if c == nil {
		d.Status = healthNotConfigured
		d.Detail = "NATS client not wired"
		return d
	}
	// IsConnected is a local atomic read; no round-trip needed. Latency is
	// effectively 0 — we record it anyway so the UI doesn't have to
	// special-case the field.
	start := time.Now()
	ok := c.IsConnected()
	d.LatencyMS = time.Since(start).Milliseconds()
	if !ok {
		d.Status = healthDown
		d.Detail = "client reports disconnected"
		return d
	}
	d.Status = healthOK
	if url := c.ConnectedURL(); url != "" {
		d.Detail = "connected to " + url
	}
	return d
}

func probeVSphere(p vsphereProbe) depResult {
	d := depResult{Name: "vcenter", LastCheck: time.Now().UTC()}
	if p == nil {
		d.Status = healthNotConfigured
		d.Detail = "vSphere probe not wired"
		return d
	}
	res, ok := p.LastResult()
	if !ok {
		// Probe wired but hasn't completed a cycle yet (e.g. cold start).
		// Reporting "degraded" instead of "down" — there's no evidence
		// of failure, just no evidence of success.
		d.Status = healthDegraded
		d.Detail = "no probe cycle has completed yet"
		return d
	}
	d.LatencyMS = res.Duration.Milliseconds()
	d.LastCheck = res.At
	age := time.Since(res.At)
	if age > vsphereResultStaleAfter {
		d.Status = healthDegraded
		d.Detail = fmt.Sprintf("last result is %s old (threshold %s)",
			age.Round(time.Second), vsphereResultStaleAfter)
		return d
	}
	if !res.Success {
		d.Status = healthDown
		if res.Err != nil {
			d.Detail = res.Err.Error()
		} else {
			d.Detail = "login failed with no error captured"
		}
		return d
	}
	d.Status = healthOK
	return d
}

func probeOPNsense(ctx context.Context, baseURL string, client *http.Client) depResult {
	d := depResult{Name: "opnsense", LastCheck: time.Now().UTC()}
	if baseURL == "" {
		d.Status = healthNotConfigured
		d.Detail = "OPNsense base URL not configured"
		return d
	}
	if client == nil {
		client = &http.Client{
			Timeout: probeTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	// Probe the firmware status endpoint — it's authenticated-but-cheap
	// and reachable on every OPNsense build we care about. API key+secret
	// aren't required for the probe — the goal is reachability + auth
	// surface. We hit /diagnostics/firmware/status (a small JSON status
	// endpoint). 200 == healthy, 401 == reachable but auth wrong
	// (degraded), connection error == down. Path is relative to BaseURL
	// which already includes /api (see internal/config and
	// internal/opnsense/client.go).
	url := strings.TrimRight(baseURL, "/") + "/diagnostics/firmware/status"
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		d.Status = healthDown
		d.Detail = "build request: " + err.Error()
		return d
	}
	resp, err := client.Do(req)
	d.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		d.Status = healthDown
		d.Detail = err.Error()
		return d
	}
	defer resp.Body.Close()
	// Drain and discard so the conn can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode == http.StatusOK:
		d.Status = healthOK
		d.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized:
		// Reachable, auth not provided / wrong. We didn't send credentials
		// from /admin/health for security reasons, so a 401 is the expected
		// "reachable but unauthenticated" signal and not a real failure.
		d.Status = healthOK
		d.Detail = "reachable (HTTP 401 — auth not sent from health probe)"
	default:
		d.Status = healthDegraded
		d.Detail = fmt.Sprintf("HTTP %d (unexpected)", resp.StatusCode)
	}
	return d
}

func probeEngine(ctx context.Context, healthURL string, client *http.Client) depResult {
	d := depResult{Name: "engine", LastCheck: time.Now().UTC()}
	if healthURL == "" {
		d.Status = healthNotConfigured
		d.Detail = "engine health URL not configured"
		return d
	}
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		d.Status = healthDown
		d.Detail = "build request: " + err.Error()
		return d
	}
	resp, err := client.Do(req)
	d.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		d.Status = healthDown
		d.Detail = err.Error()
		return d
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		d.Status = healthDown
		d.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
		return d
	}
	d.Status = healthOK
	// Body shape is {"status":"ok","engine_id":"..."} — surface the
	// engine_id so operators can see which pod replied (useful for HA).
	var parsed struct {
		EngineID string `json:"engine_id"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.EngineID != "" {
		d.Detail = "engine_id=" + parsed.EngineID
	}
	return d
}
