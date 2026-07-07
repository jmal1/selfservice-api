// Metrics for the pod-VM DHCP IP reconciler. Mirrors OrphanCountPusher:
// same Pushgateway, separate metric namespace (`crucible_podvm_ip_reconcile_*`),
// reusing the shared push helpers in reconcile_metrics.go.
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// IPReconcileCountPusher pushes one IPReconcileCounts as a small gauge family
// to Pushgateway. Safe for use by a single worker process. Push is a no-op
// when BaseURL is empty.
type IPReconcileCountPusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client
}

// Push emits the per-category candidate gauge, a run timestamp, and an
// updated counter as gauges. Grouping matches OrphanCountPusher so both
// reconcilers coexist under the same Pushgateway job without clobbering.
func (p *IPReconcileCountPusher) Push(ctx context.Context, counts IPReconcileCounts) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeIPReconcileCounts(counts)
	grouping := mergeGrouping(p.GroupingLabels, "reconciler", "podvm_ip")
	target := orphansPushURL(p.BaseURL, p.Job, grouping)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := p.HTTP
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

func serializeIPReconcileCounts(c IPReconcileCounts) []byte {
	var b bytes.Buffer

	b.WriteString("# HELP crucible_podvm_ip_reconcile_total Running IP-assigned pod VMs classified by outcome at the last IP-reconciler run.\n")
	b.WriteString("# TYPE crucible_podvm_ip_reconcile_total gauge\n")
	for _, row := range []struct {
		category string
		value    int
	}{
		{"candidates", c.Candidates},
		{"updated", c.Updated},
		{"unchanged", c.Unchanged},
		{"no_ip", c.NoIP},
		{"errors", c.Errors},
	} {
		fmt.Fprintf(&b, "crucible_podvm_ip_reconcile_total{category=%q} %d\n", row.category, row.value)
	}

	b.WriteString("# HELP crucible_podvm_ip_reconcile_run_timestamp_seconds Unix time of the latest IP-reconciler run.\n")
	b.WriteString("# TYPE crucible_podvm_ip_reconcile_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_podvm_ip_reconcile_run_timestamp_seconds %d\n", time.Now().Unix())

	return b.Bytes()
}
