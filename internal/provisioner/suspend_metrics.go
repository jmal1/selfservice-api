// Metrics for the VM idle-suspend pipeline. Follows the same design as
// DestroyFailedPusher and PipelineMetrics: no prometheus client dependency,
// hand-serialized text exposition, pushed to Pushgateway, no-op when BaseURL
// is empty.
//
// Metrics exposed:
//   crucible_vm_suspended_total{reason}      — counter: suspensions by reason
//   crucible_vms_suspended                   — gauge: VMs currently suspended
//   crucible_idle_evaluator_last_run_timestamp — gauge: Unix time of last evaluator pass
//
// Each metric has a real production call site in the idle evaluator loop
// (see idle_eval.go evaluateIdleVMs). The pusher loop makes those call sites
// observable in Prometheus; without the push loop an absent series is
// indistinguishable from zero, which would appear healthy when the evaluator
// has never run.
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// SuspendMetrics accumulates idle-suspend counters and pushes them on demand.
// Zero value is usable but inert (BaseURL empty => Push is a no-op);
// always construct via NewSuspendMetrics so the map is non-nil.
type SuspendMetrics struct {
	// BaseURL is the Pushgateway root. Empty disables pushing.
	BaseURL string

	// Job is the Pushgateway {job} label.
	Job string

	// GroupingLabels are appended as /{key}/{value} pairs, sorted.
	GroupingLabels map[string]string

	// HTTP overrides the client; nil falls back to a 10s-timeout client.
	HTTP *http.Client

	mu sync.Mutex

	suspendTotal   map[string]float64 // reason
	suspendedGauge float64
	lastRunUnix    float64
}

// NewSuspendMetrics returns an initialized collector.
func NewSuspendMetrics(baseURL, job string, grouping map[string]string) *SuspendMetrics {
	if job == "" {
		job = "crucible_provision_worker"
	}
	return &SuspendMetrics{
		BaseURL:        baseURL,
		Job:            job,
		GroupingLabels: grouping,
		suspendTotal:   map[string]float64{},
	}
}

// RecordSuspend increments the suspension counter for the given reason.
// reason is one of: "idle" (real suspension), "dry_run", "error".
// Production call sites: evaluateIdleVMs in idle_eval.go (multiple paths).
func (m *SuspendMetrics) RecordSuspend(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspendTotal[reason]++
}

// SetSuspendedGauge replaces the count of VMs currently suspended this pass.
// Production call site: evaluateIdleVMs in idle_eval.go after the main loop.
func (m *SuspendMetrics) SetSuspendedGauge(n int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspendedGauge = float64(n)
}

// SetLastRunTimestamp records the wall-clock time of the latest evaluator pass.
// Production call site: evaluateIdleVMs in idle_eval.go after the main loop.
func (m *SuspendMetrics) SetLastRunTimestamp(t time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastRunUnix = float64(t.Unix())
}

// Push serializes current values and POSTs to Pushgateway. No-op when BaseURL is empty.
func (m *SuspendMetrics) Push(ctx context.Context) error {
	if m == nil || m.BaseURL == "" {
		return nil
	}
	body := m.serialize()
	target := destroyFailedPushURL(m.BaseURL, m.Job, m.GroupingLabels)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build suspend metrics push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("suspend metrics push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("suspend metrics pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

func (m *SuspendMetrics) serialize() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b bytes.Buffer

	writeCounter1(&b,
		"crucible_vm_suspended_total",
		"Total VM idle-suspensions by reason (idle, dry_run, error).",
		"reason", m.suspendTotal)

	b.WriteString("# HELP crucible_vms_suspended VMs suspended in the latest idle-evaluator pass.\n")
	b.WriteString("# TYPE crucible_vms_suspended gauge\n")
	fmt.Fprintf(&b, "crucible_vms_suspended %g\n", m.suspendedGauge)

	b.WriteString("# HELP crucible_idle_evaluator_last_run_timestamp Unix time of the latest idle-evaluator pass.\n")
	b.WriteString("# TYPE crucible_idle_evaluator_last_run_timestamp gauge\n")
	fmt.Fprintf(&b, "crucible_idle_evaluator_last_run_timestamp %g\n", m.lastRunUnix)

	return b.Bytes()
}

// RunSuspendMetricsPusher flushes accumulated metrics to Pushgateway every
// interval until ctx is cancelled. No-op when m is nil or BaseURL is empty.
//
// This loop is what makes every RecordSuspend / SetSuspendedGauge call above
// visible in Prometheus. Without it the series are silently absent, which is
// indistinguishable from zero and makes "the evaluator has never run" look
// identical to "no suspensions have occurred."
func (m *SuspendMetrics) RunSuspendMetricsPusher(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if m == nil || m.BaseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "suspend_metrics_pusher")
	log.Info("suspend metrics pusher started", "interval", interval, "job", m.Job)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := m.Push(flushCtx); err != nil {
				log.Warn("final suspend metrics push failed", "error", err)
			}
			cancel()
			log.Info("suspend metrics pusher stopped")
			return
		case <-ticker.C:
			if err := m.Push(ctx); err != nil {
				log.Warn("suspend metrics push failed", "error", err)
			}
		}
	}
}
