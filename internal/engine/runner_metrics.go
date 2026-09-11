// Metrics for the runner/engine Job lifecycle. Mirrors the design of
// PipelineMetrics and DestroyFailedPusher: no prometheus/client_golang
// dependency, hand-serialized Prometheus text exposition format, pushed to
// Pushgateway, and a no-op when BaseURL is empty so the engine still works in
// environments without observability.
//
// Nil-safe: every exported method checks for a nil receiver so callers can hold
// a *RunnerMetrics that was never initialized (e.g., tests that don't wire
// Pushgateway) without panicking.
//
// Duration counters:
//
//	Histograms are not possible without the client library. Durations are
//	emitted as _sum/_count counter pairs (average = rate(sum)/rate(count))
//	plus a _max gauge for worst-case tracking. No _bucket or le= labels exist.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunnerMetrics accumulates runner Job lifecycle counters/gauges and pushes
// the whole family to Prometheus Pushgateway on demand. Safe for concurrent use.
//
// Zero value is usable but inert (BaseURL empty => Push is a no-op).
// Construct via NewRunnerMetrics so internal maps are non-nil.
type RunnerMetrics struct {
	// BaseURL is the Pushgateway root. Empty disables pushing.
	BaseURL string

	// Job is the Pushgateway {job} label, default crucible_engine.
	Job string

	// GroupingLabels are appended as /{key}/{value} pairs, sorted.
	GroupingLabels map[string]string

	// HTTP overrides the underlying client; nil falls back to a 10s-timeout client.
	HTTP *http.Client

	mu sync.Mutex

	jobTotal          map[string]float64 // keyed by result
	provisionDurSum   float64
	provisionDurCount float64
	provisionDurMax   float64
	execDurSum        map[string]float64 // keyed by execution_mode
	execDurCount      map[string]float64 // keyed by execution_mode
	activeRunners     float64
	cleanupFailed     map[string]float64 // keyed by resource
	orphansCleaned    float64
	callbackTotal     map[string]float64 // keyed by endpoint|result
}

// NewRunnerMetrics returns an initialized recorder. baseURL may be empty,
// in which case Push() is a no-op but all Record* calls remain safe.
func NewRunnerMetrics(baseURL, job string, grouping map[string]string) *RunnerMetrics {
	if job == "" {
		job = "crucible_engine"
	}
	return &RunnerMetrics{
		BaseURL:        baseURL,
		Job:            job,
		GroupingLabels: grouping,
		jobTotal:       map[string]float64{},
		execDurSum:     map[string]float64{},
		execDurCount:   map[string]float64{},
		cleanupFailed:  map[string]float64{},
		callbackTotal:  map[string]float64{},
	}
}

// RecordRunnerJob counts a completed runner Job by outcome.
// result is one of "success", "failed", "timeout", "cancelled".
func (m *RunnerMetrics) RecordRunnerJob(result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobTotal[result]++
}

// ObserveRunnerProvision records a provision latency sample (start of
// ProvisionRunner call to K8s API response). Updates _sum, _count, and _max.
func (m *RunnerMetrics) ObserveRunnerProvision(d time.Duration) {
	if m == nil {
		return
	}
	s := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.provisionDurSum += s
	m.provisionDurCount++
	if s > m.provisionDurMax {
		m.provisionDurMax = s
	}
}

// ObserveRunnerExecution records a runner execution duration keyed by
// execution_mode. Updates _sum and _count for that mode.
func (m *RunnerMetrics) ObserveRunnerExecution(mode string, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execDurSum[mode] += d.Seconds()
	m.execDurCount[mode]++
}

// SetRunnerActive sets the gauge of currently-running runner Jobs to n.
func (m *RunnerMetrics) SetRunnerActive(n int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRunners = float64(n)
}

// incRunnerActive adjusts the active runner gauge by delta. Negative
// values are clamped to zero. Package-internal: wiring in k8s.go and
// engine.go calls this so external callers don't need to track the count.
func (m *RunnerMetrics) incRunnerActive(delta int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeRunners += float64(delta)
	if m.activeRunners < 0 {
		m.activeRunners = 0
	}
}

// RecordCleanupFailure counts a failed K8s resource deletion inside
// CleanupRunner. resource is one of "job", "secret", "nad".
// This is a security signal: a leaked Secret holds a live callback token.
func (m *RunnerMetrics) RecordCleanupFailure(resource string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupFailed[resource]++
}

// RecordOrphansCleaned adds n to the cumulative orphan cleanup counter.
func (m *RunnerMetrics) RecordOrphansCleaned(n int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orphansCleaned += float64(n)
}

// RecordCallback counts a runner-to-engine callback by endpoint and result.
// endpoint is one of "action", "workflow", "complete", "heartbeat".
// result is "success" or "error".
func (m *RunnerMetrics) RecordCallback(endpoint, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbackTotal[endpoint+"|"+result]++
}

// Push serializes current values and POSTs them to the Pushgateway.
// Returns nil immediately when m is nil or BaseURL is empty.
func (m *RunnerMetrics) Push(ctx context.Context) error {
	if m == nil || m.BaseURL == "" {
		return nil
	}
	body := m.serialize()
	target := runnerMetricsPushURL(m.BaseURL, m.Job, m.GroupingLabels)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := m.HTTP
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

func (m *RunnerMetrics) serialize() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b bytes.Buffer

	writeRM1(&b, "crucible_runner_job_total", "counter",
		"Runner Job outcomes by result (success/failed/timeout/cancelled).",
		"result", m.jobTotal)

	b.WriteString("# HELP crucible_runner_provision_duration_seconds_sum Cumulative seconds spent in ProvisionRunner calls since process start.\n")
	b.WriteString("# TYPE crucible_runner_provision_duration_seconds_sum counter\n")
	fmt.Fprintf(&b, "crucible_runner_provision_duration_seconds_sum %g\n", m.provisionDurSum)

	b.WriteString("# HELP crucible_runner_provision_duration_seconds_count Number of completed ProvisionRunner calls. Average = rate(sum)/rate(count).\n")
	b.WriteString("# TYPE crucible_runner_provision_duration_seconds_count counter\n")
	fmt.Fprintf(&b, "crucible_runner_provision_duration_seconds_count %g\n", m.provisionDurCount)

	b.WriteString("# HELP crucible_runner_provision_duration_seconds_max Largest observed ProvisionRunner duration in seconds since process start.\n")
	b.WriteString("# TYPE crucible_runner_provision_duration_seconds_max gauge\n")
	fmt.Fprintf(&b, "crucible_runner_provision_duration_seconds_max %g\n", m.provisionDurMax)

	writeRM1(&b, "crucible_runner_execution_duration_seconds_sum", "counter",
		"Cumulative execution seconds by execution_mode. Average = rate(sum)/rate(count).",
		"execution_mode", m.execDurSum)
	writeRM1(&b, "crucible_runner_execution_duration_seconds_count", "counter",
		"Completed runner executions by execution_mode.",
		"execution_mode", m.execDurCount)

	b.WriteString("# HELP crucible_runner_active Currently-running runner Jobs.\n")
	b.WriteString("# TYPE crucible_runner_active gauge\n")
	fmt.Fprintf(&b, "crucible_runner_active %g\n", m.activeRunners)

	writeRM1(&b, "crucible_runner_cleanup_failed_total", "counter",
		"CleanupRunner failures by resource (job/secret/nad). A leaked secret holds a live callback token.",
		"resource", m.cleanupFailed)

	b.WriteString("# HELP crucible_runner_orphans_cleaned_total Runner Jobs removed by the orphan sweep since process start.\n")
	b.WriteString("# TYPE crucible_runner_orphans_cleaned_total counter\n")
	fmt.Fprintf(&b, "crucible_runner_orphans_cleaned_total %g\n", m.orphansCleaned)

	writeRM2(&b, "crucible_engine_callback_total", "counter",
		"Runner-to-engine callback hits by endpoint and result.",
		"endpoint", "result", m.callbackTotal)

	return b.Bytes()
}

// writeRM1 emits a metric family keyed by a single label. Keys sorted for
// byte-stable output (deterministic tests).
func writeRM1(b *bytes.Buffer, name, typ, help, label string, vals map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	if len(vals) == 0 {
		return
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %g\n", name, label, k, vals[k])
	}
}

// writeRM2 emits a metric family whose map keys are "a|b".
func writeRM2(b *bytes.Buffer, name, typ, help, l1, l2 string, vals map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	if len(vals) == 0 {
		return
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		v1, v2 := "", ""
		if len(parts) > 0 {
			v1 = parts[0]
		}
		if len(parts) > 1 {
			v2 = parts[1]
		}
		fmt.Fprintf(b, "%s{%s=%q,%s=%q} %g\n", name, l1, v1, l2, v2, vals[k])
	}
}

func runnerMetricsPushURL(base, job string, grouping map[string]string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	b.WriteString("/metrics/job/")
	b.WriteString(url.PathEscape(job))
	keys := make([]string, 0, len(grouping))
	for k := range grouping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("/")
		b.WriteString(url.PathEscape(k))
		b.WriteString("/")
		b.WriteString(url.PathEscape(grouping[k]))
	}
	return b.String()
}

// WithRunnerMetrics attaches a recorder to the K8s client. Optional: when nil
// every metric call is a no-op, so the engine runs fine without Pushgateway.
func (k *K8sClient) WithRunnerMetrics(m *RunnerMetrics) *K8sClient {
	k.metrics = m
	return k
}

// RunPusher flushes the accumulated metric families to Pushgateway every
// interval until ctx is cancelled.
//
// This loop is what makes every Record*/Observe*/Set* call above observable.
// Those methods only mutate in-process counters; nothing reaches Prometheus
// until Push serializes and POSTs them. Without this loop crucible_runner_*
// is silently absent, and an absent series renders on a dashboard exactly
// like a zero one -- "no runner cleanup failures" and "the engine has never
// cleaned up anything" look identical. That matters here because
// CrucibleRunnerCleanupFailing is a critical alert: a leaked Secret holds a
// live callback token.
//
// A push failure is logged and retried on the next tick rather than being
// fatal: Pushgateway being down must not take the engine down with it.
func (m *RunnerMetrics) RunPusher(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if m == nil || m.BaseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "runner_metrics_pusher")
	log.Info("runner metrics pusher started", "interval", interval, "job", m.Job)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush so counters accumulated since the last tick survive
			// a normal shutdown or rolling restart.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := m.Push(flushCtx); err != nil {
				log.Warn("final runner metrics push failed", "error", err)
			}
			cancel()
			log.Info("runner metrics pusher stopped")
			return
		case <-ticker.C:
			if err := m.Push(ctx); err != nil {
				log.Warn("runner metrics push failed", "error", err)
			}
		}
	}
}
