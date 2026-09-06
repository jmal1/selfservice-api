// Package health implements an in-process vCenter credentials health probe
// that pushes login-success metrics to Prometheus Pushgateway on a schedule.
//
// Motivation (OP-1): On 2026-06-07 we discovered that the production
// selfservice-api had a long-running vCenter session token that masked an
// expired/rotated SSO password — every real API call kept working off the
// cached session, but any fresh login (worker restart, new pod, T1 folder
// enum) failed with "incorrect user name or password". A periodic *fresh*
// login probe surfaces this within minutes instead of months.
//
// Design:
//   - Each cycle creates a brand-new govmomi client (not the long-lived one
//     the rest of the API gateway uses) so the probe always exercises real
//     authentication, not a cached session.
//   - Result is pushed as Prometheus text format to Pushgateway. Metric
//     names are deliberately `vsphere_*` so they live in their own
//     namespace from the customer-API synthetic checks.
//   - Probe failures DO NOT crash or restart anything — the metric IS the
//     signal. Grafana fires the alert.
package health

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// Config is the runtime configuration for a Probe. All fields except
// PushgatewayURL are required to make a meaningful probe; if PushgatewayURL
// is empty the probe runs (and logs) but does not push.
type Config struct {
	// VCenterURL is the SDK endpoint, e.g. https://vcenter.lab.jmal.io/sdk
	VCenterURL string
	// User in vsphere.local format, e.g. selfservice-svc@vsphere.local.
	// Optional when ClientCertPEM+ClientKeyPEM are set (solution-user STS).
	User string
	// Password is the cleartext SSO password the probe attempts a fresh
	// login with each cycle. Optional when client certificate PEMs are set.
	Password string
	// ClientCertPEM / ClientKeyPEM enable STS Holder-of-Key login (preferred).
	ClientCertPEM []byte
	ClientKeyPEM  []byte
	// Insecure skips TLS verification (matches the production vCenter
	// client's setting; the homelab uses a self-signed CA).
	Insecure bool

	// PushgatewayURL is the Pushgateway root, e.g.
	// http://pushgateway.observability.svc.cluster.local:9091. Empty
	// disables pushing (probe still runs for logs/tests).
	PushgatewayURL string
	// Job is the Pushgateway {job} label, e.g. crucible_vsphere_health.
	Job string
	// GroupingLabels are appended to the push URL as /{key}/{value} pairs.
	GroupingLabels map[string]string

	// ProbeTimeout caps each individual login attempt. 30s is a sane
	// default — vCenter login is normally < 2s; anything past 30s means
	// either network or SSO degradation.
	ProbeTimeout time.Duration

	// HTTP overrides the Pushgateway HTTP client (tests inject the
	// httptest server's client). nil => 10s-timeout default.
	HTTP *http.Client
}

// Result is the outcome of a single probe cycle. It is intentionally cheap
// to serialize — the Pushgateway body builder takes a Result directly so
// tests can assert the exact wire format without an HTTP round-trip.
type Result struct {
	// Success is 1 if login succeeded, 0 otherwise. Stored as bool here
	// for readability; serialized to gauge 0/1 at push time.
	Success bool
	// Duration is wall-clock time from connect-start to login-complete (or
	// to the error, whichever came first).
	Duration time.Duration
	// Err is the failure reason. Persisted only as a log line + the
	// label-stripped reason on `vsphere_health_check_info`. Never pushed
	// raw to Pushgateway (it can contain server hostnames).
	Err error
	// At is the timestamp at the end of the probe cycle.
	At time.Time
}

// Probe is one configured health prober. Construct via New, then call
// Run to execute one cycle, or RunPeriodic to start a background loop.
type Probe struct {
	cfg    Config
	logger *slog.Logger
	// last holds the most recent Result so callers (e.g. /admin/health)
	// can read it without doing a fresh login. Pointer-typed so we can
	// atomically swap a freshly populated Result in place; readers see a
	// consistent snapshot. nil until the first Run completes.
	last atomic.Pointer[Result]
}

// New validates the config and returns a Probe. ProbeTimeout < 1s is
// rejected because vCenter login + TLS handshake reliably needs > 500ms.
func New(cfg Config, logger *slog.Logger) (*Probe, error) {
	if cfg.VCenterURL == "" {
		return nil, fmt.Errorf("VCenterURL is required")
	}
	vcCfg := vcenter.Config{
		URL:           cfg.VCenterURL,
		User:          cfg.User,
		Password:      cfg.Password,
		ClientCertPEM: cfg.ClientCertPEM,
		ClientKeyPEM:  cfg.ClientKeyPEM,
		Insecure:      cfg.Insecure,
	}
	if !vcCfg.HasClientCertificate() && !vcCfg.HasPasswordAuth() {
		return nil, fmt.Errorf("vCenter probe requires client certificate PEMs or User+Password")
	}
	if !vcCfg.HasClientCertificate() && cfg.User == "" {
		return nil, fmt.Errorf("User is required")
	}
	if !vcCfg.HasClientCertificate() && cfg.Password == "" {
		return nil, fmt.Errorf("Password is required")
	}
	if cfg.ProbeTimeout == 0 {
		cfg.ProbeTimeout = 30 * time.Second
	}
	if cfg.ProbeTimeout < time.Second {
		return nil, fmt.Errorf("ProbeTimeout=%s too aggressive; minimum 1s", cfg.ProbeTimeout)
	}
	if cfg.Job == "" {
		cfg.Job = "crucible_vsphere_health"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Probe{cfg: cfg, logger: logger}, nil
}

// Run executes one probe cycle: fresh login attempt, optional Pushgateway
// push. The returned Result is always usable even on push failure (push
// errors are logged and returned separately so the caller can monitor them
// out of band without conflating "probe failed" with "push failed").
func (p *Probe) Run(ctx context.Context) (Result, error) {
	res := p.probe(ctx)
	// Stash the result so consumers like /admin/health can read it without
	// triggering another login round-trip. Done unconditionally — a failed
	// probe is also useful diagnostic state.
	stash := res
	p.last.Store(&stash)
	if p.cfg.PushgatewayURL == "" {
		return res, nil
	}
	if err := p.push(ctx, res); err != nil {
		p.logger.Warn("vsphere health push failed", "error", err)
		return res, err
	}
	return res, nil
}

// LastResult returns the most recent probe Result and true. If no probe
// cycle has completed yet, returns the zero Result and false. Reader-safe:
// uses an atomic load on the internal pointer.
func (p *Probe) LastResult() (Result, bool) {
	r := p.last.Load()
	if r == nil {
		return Result{}, false
	}
	return *r, true
}

// RunPeriodic runs Probe.Run on a fixed interval until ctx is cancelled.
// It fires one probe immediately so the dashboard updates within seconds
// of pod startup rather than waiting for the first tick.
func (p *Probe) RunPeriodic(ctx context.Context, interval time.Duration) {
	if interval < time.Second {
		p.logger.Warn("vsphere health interval too aggressive; clamping to 1m", "given", interval)
		interval = time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		res, _ := p.Run(ctx)
		p.logger.Info("vsphere health probe",
			"success", res.Success,
			"duration_ms", res.Duration.Milliseconds(),
			"error", errString(res.Err),
		)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// probe is the pure login-attempt logic. Extracted so unit tests can
// exercise it without entering the Pushgateway path. The govmomi client
// is closed (Logout) immediately on success so the probe leaves no
// long-lived sessions behind.
func (p *Probe) probe(ctx context.Context) Result {
	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer cancel()

	client, err := vcenter.Authenticate(probeCtx, vcenter.Config{
		URL:           p.cfg.VCenterURL,
		User:          p.cfg.User,
		Password:      p.cfg.Password,
		ClientCertPEM: p.cfg.ClientCertPEM,
		ClientKeyPEM:  p.cfg.ClientKeyPEM,
		Insecure:      p.cfg.Insecure,
	})
	dur := time.Since(start)
	if err != nil {
		return Result{Success: false, Duration: dur, Err: err, At: time.Now()}
	}
	// Best-effort logout — failure here doesn't change the probe result;
	// the server cleans up idle sessions on its own.
	if client != nil {
		_ = client.Logout(probeCtx)
	}
	return Result{Success: true, Duration: dur, Err: nil, At: time.Now()}
}

// push POSTs the result as Prometheus text format to Pushgateway. The
// metric names follow the vsphere_* namespace so they don't clash with
// the customer-API synthetic monitor's crucible_synthetic_* family.
func (p *Probe) push(ctx context.Context, res Result) error {
	body := serialize(res, p.cfg.User)
	target := pushURL(p.cfg.PushgatewayURL, p.cfg.Job, p.cfg.GroupingLabels)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := p.cfg.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

// pushURL builds the Pushgateway path. Grouping labels are sorted so the
// URL is deterministic — Pushgateway treats different key orderings as
// distinct groupings, which would leak orphan metrics forever.
func pushURL(base, job string, grouping map[string]string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	b.WriteString("/metrics/job/")
	b.WriteString(url.PathEscape(job))
	keys := sortedKeys(grouping)
	for _, k := range keys {
		b.WriteString("/")
		b.WriteString(url.PathEscape(k))
		b.WriteString("/")
		b.WriteString(url.PathEscape(grouping[k]))
	}
	return b.String()
}

// serialize emits the four-metric exposition for one probe result. The
// `user` label is intentionally truncated at @ so the SSO domain doesn't
// pollute the metric cardinality, while still letting alerts distinguish
// different service accounts if we ever add more probes.
func serialize(res Result, user string) []byte {
	userLabel := user
	if at := strings.Index(userLabel, "@"); at >= 0 {
		userLabel = userLabel[:at]
	}
	var b bytes.Buffer
	b.WriteString("# HELP vsphere_login_success 1 if a fresh vCenter login succeeded, 0 otherwise.\n")
	b.WriteString("# TYPE vsphere_login_success gauge\n")
	fmt.Fprintf(&b, "vsphere_login_success{user=%q} %d\n", userLabel, boolToInt(res.Success))

	b.WriteString("# HELP vsphere_login_duration_seconds Wall-clock duration of the last login attempt.\n")
	b.WriteString("# TYPE vsphere_login_duration_seconds gauge\n")
	fmt.Fprintf(&b, "vsphere_login_duration_seconds{user=%q} %f\n", userLabel, res.Duration.Seconds())

	b.WriteString("# HELP vsphere_health_check_info Static metadata about the last vCenter health probe.\n")
	b.WriteString("# TYPE vsphere_health_check_info gauge\n")
	fmt.Fprintf(&b, "vsphere_health_check_info{user=%q,error=%q} 1\n",
		userLabel, classifyError(res.Err))

	b.WriteString("# HELP vsphere_health_run_timestamp_seconds Unix time of the latest vCenter probe.\n")
	b.WriteString("# TYPE vsphere_health_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "vsphere_health_run_timestamp_seconds %d\n", res.At.Unix())
	return b.Bytes()
}

// classifyError reduces a probe error to a low-cardinality reason label
// suitable for Prometheus. We do NOT want full error strings as labels —
// they may contain hostnames or session IDs. The buckets here are derived
// from observed vCenter SOAP fault messages.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "incorrect user name") ||
		strings.Contains(msg, "incorrect password") ||
		strings.Contains(msg, "invalidlogin"):
		return "invalid_credentials"
	case strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "tls") || strings.Contains(msg, "certificate"):
		return "tls"
	case strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "dial"):
		return "network"
	case strings.Contains(msg, "parse url"):
		return "config"
	default:
		return "other"
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// inline insertion sort — never more than a handful of grouping labels.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
