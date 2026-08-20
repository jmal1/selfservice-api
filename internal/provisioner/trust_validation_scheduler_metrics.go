package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// L1ValidationSchedulerMetrics publishes scheduler health under its own
// Pushgateway grouping key. Non-leader pipeline pushers therefore cannot erase
// these leader-owned series.
type L1ValidationSchedulerMetrics struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client

	mu                   sync.Mutex
	collected            bool
	lastRunTimestamp     float64
	lastSuccessTimestamp float64
	dueTemplates         float64
	enqueuedJobs         float64
	errorsTotal          float64
}

func NewL1ValidationSchedulerMetrics(baseURL, job string, grouping map[string]string) *L1ValidationSchedulerMetrics {
	if job == "" {
		job = "crucible_provision_worker"
	}
	return &L1ValidationSchedulerMetrics{
		BaseURL:        baseURL,
		Job:            job,
		GroupingLabels: grouping,
	}
}

func (m *L1ValidationSchedulerMetrics) ObserveRun(counts L1TrustValidationCounts, runErr error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := float64(time.Now().Unix())
	m.collected = true
	m.lastRunTimestamp = now
	m.dueTemplates = float64(counts.Due)
	m.enqueuedJobs = float64(counts.Enqueued)
	if runErr != nil {
		m.errorsTotal++
		return
	}
	m.lastSuccessTimestamp = now
}

func (m *L1ValidationSchedulerMetrics) Push(ctx context.Context) error {
	if m == nil || m.BaseURL == "" {
		return nil
	}
	target := destroyFailedPushURL(m.BaseURL, m.Job,
		mergeGrouping(m.GroupingLabels, "component", "l1_validation_scheduler"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(m.serialize()))
	if err != nil {
		return fmt.Errorf("build l1 validation scheduler metrics request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push l1 validation scheduler metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (m *L1ValidationSchedulerMetrics) serialize() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b bytes.Buffer
	if !m.collected {
		return b.Bytes()
	}
	writeSchedulerGauge := func(name, help string, value float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&b, "%s %g\n", name, value)
	}
	writeSchedulerGauge(
		"crucible_l1_validation_scheduler_last_run_timestamp_seconds",
		"Unix timestamp when the latest L1 validation scheduler reconciliation finished.",
		m.lastRunTimestamp,
	)
	writeSchedulerGauge(
		"crucible_l1_validation_scheduler_last_success_timestamp_seconds",
		"Unix timestamp when the latest error-free L1 validation scheduler reconciliation finished; 0 until first success.",
		m.lastSuccessTimestamp,
	)
	writeSchedulerGauge(
		"crucible_l1_validation_scheduler_due_templates",
		"Active L1 templates due for validation during the latest scheduler reconciliation.",
		m.dueTemplates,
	)
	writeSchedulerGauge(
		"crucible_l1_validation_scheduler_enqueued_jobs",
		"New template_revalidate jobs inserted during the latest scheduler reconciliation.",
		m.enqueuedJobs,
	)
	b.WriteString("# HELP crucible_l1_validation_scheduler_errors_total L1 validation scheduler reconciliations that completed with one or more errors.\n")
	b.WriteString("# TYPE crucible_l1_validation_scheduler_errors_total counter\n")
	fmt.Fprintf(&b, "crucible_l1_validation_scheduler_errors_total %g\n", m.errorsTotal)
	return b.Bytes()
}
