// Metrics for the provision worker. Pushes a low-cardinality gauge to
// Prometheus Pushgateway each time the destroy-failed retry sweep runs.
// Lets the prometheus alert CruciblePodsStuckInDestroyFailed page within
// 15m of a single pod getting stuck — before the lifecycle synthetic
// would have a chance to flap (and long before max_pods quota is hit).
//
// Design mirrors internal/vsphere/health: a tiny pusher that lives next
// to its caller, separate metric namespace (`crucible_pods_*`), graceful
// degradation when PushgatewayURL is empty (worker still does its job).
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DestroyFailedPusher pushes the count of pods in destroy_failed state to
// Pushgateway. Safe for concurrent use by a single worker process.
type DestroyFailedPusher struct {
	// BaseURL is the Pushgateway root, e.g.
	// http://pushgateway.observability.svc.cluster.local:9091.
	// Empty disables pushing; Push() is a no-op.
	BaseURL string

	// Job is the Pushgateway {job} label, default crucible_provision_worker.
	Job string

	// GroupingLabels are appended to the push URL as /{key}/{value} pairs,
	// sorted for determinism. Typical: {"layer": "api"}.
	GroupingLabels map[string]string

	// HTTP overrides the underlying client; nil falls back to a 10s-timeout
	// http.Client. Tests inject httptest.NewServer().Client().
	HTTP *http.Client
}

// Push uploads a single gauge sample plus a run-timestamp gauge. Returns
// nil immediately (no error, no work) when BaseURL is empty.
//
// Two metrics are emitted:
//   - crucible_pods_destroy_failed_count: the gauge the alert watches.
//   - crucible_pods_destroy_failed_run_timestamp_seconds: lets a staleness
//     alert detect the case where the worker stopped pushing entirely.
func (p *DestroyFailedPusher) Push(ctx context.Context, count int) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeDestroyFailed(count)
	target := destroyFailedPushURL(p.BaseURL, p.Job, p.GroupingLabels)
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

func serializeDestroyFailed(count int) []byte {
	var b bytes.Buffer
	b.WriteString("# HELP crucible_pods_destroy_failed_count Number of pods in destroy_failed status at last worker retry sweep.\n")
	b.WriteString("# TYPE crucible_pods_destroy_failed_count gauge\n")
	fmt.Fprintf(&b, "crucible_pods_destroy_failed_count %d\n", count)

	b.WriteString("# HELP crucible_pods_destroy_failed_run_timestamp_seconds Unix time of the latest worker destroy-failed sweep.\n")
	b.WriteString("# TYPE crucible_pods_destroy_failed_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_pods_destroy_failed_run_timestamp_seconds %d\n", time.Now().Unix())
	return b.Bytes()
}

func destroyFailedPushURL(base, job string, grouping map[string]string) string {
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
