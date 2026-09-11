package handlers

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

// ProvisioningAdmissionMetrics publishes the API maintenance state and
// low-cardinality rejection counters to Pushgateway.
type ProvisioningAdmissionMetrics struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client

	mu       sync.Mutex
	enabled  bool
	rejected map[string]uint64
}

// NewProvisioningAdmissionMetrics returns an initialized collector. An empty
// baseURL keeps recording available while making Push and RunPusher no-ops.
func NewProvisioningAdmissionMetrics(baseURL, job string, grouping map[string]string, enabled bool) *ProvisioningAdmissionMetrics {
	if job == "" {
		job = "crucible_provisioning_admission"
	}
	rejected := make(map[string]uint64, len(provisioningRoutes))
	for _, route := range provisioningRoutes {
		rejected[route] = 0
	}
	return &ProvisioningAdmissionMetrics{
		BaseURL:        baseURL,
		Job:            job,
		GroupingLabels: grouping,
		enabled:        enabled,
		rejected:       rejected,
	}
}

// RecordRejected increments one of the fixed route counters. Unknown labels
// are ignored so callers cannot create unbounded metric cardinality.
func (m *ProvisioningAdmissionMetrics) RecordRejected(route string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rejected[route]; ok {
		m.rejected[route]++
	}
}

// Push publishes the current admission snapshot. Empty BaseURL disables it.
func (m *ProvisioningAdmissionMetrics) Push(ctx context.Context) error {
	if m == nil || m.BaseURL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.pushURL(), bytes.NewReader(m.serialize()))
	if err != nil {
		return fmt.Errorf("build provisioning admission metrics request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")

	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push provisioning admission metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, body)
	}
	return nil
}

// RunPusher flushes admission metrics until shutdown. Push failures are
// observable in logs but never take down the API.
func (m *ProvisioningAdmissionMetrics) RunPusher(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if m == nil || m.BaseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := m.Push(flushCtx); err != nil {
				logger.Warn("final provisioning admission metrics push failed", "error", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := m.Push(ctx); err != nil {
				logger.Warn("provisioning admission metrics push failed", "error", err)
			}
		}
	}
}

func (m *ProvisioningAdmissionMetrics) serialize() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b bytes.Buffer
	b.WriteString("# HELP crucible_provisioning_admission_enabled Whether the API accepts new pod and VM provisioning requests (1 enabled, 0 maintenance).\n")
	b.WriteString("# TYPE crucible_provisioning_admission_enabled gauge\n")
	if m.enabled {
		b.WriteString("crucible_provisioning_admission_enabled 1\n")
	} else {
		b.WriteString("crucible_provisioning_admission_enabled 0\n")
	}
	b.WriteString("# HELP crucible_provisioning_admission_rejected_total Number of provisioning requests rejected during maintenance, by bounded API route.\n")
	b.WriteString("# TYPE crucible_provisioning_admission_rejected_total counter\n")
	for _, route := range provisioningRoutes {
		fmt.Fprintf(&b, "crucible_provisioning_admission_rejected_total{route=%q} %d\n", route, m.rejected[route])
	}
	b.WriteString("# HELP crucible_provisioning_admission_metrics_timestamp_seconds Unix time of the latest provisioning admission metrics snapshot.\n")
	b.WriteString("# TYPE crucible_provisioning_admission_metrics_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_provisioning_admission_metrics_timestamp_seconds %d\n", time.Now().Unix())
	return b.Bytes()
}

func (m *ProvisioningAdmissionMetrics) pushURL() string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(m.BaseURL, "/"))
	b.WriteString("/metrics/job/")
	b.WriteString(url.PathEscape(m.Job))
	keys := make([]string, 0, len(m.GroupingLabels))
	for key := range m.GroupingLabels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteString("/")
		b.WriteString(url.PathEscape(key))
		b.WriteString("/")
		b.WriteString(url.PathEscape(m.GroupingLabels[key]))
	}
	return b.String()
}
