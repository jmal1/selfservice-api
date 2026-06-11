// Metrics for the vCenter orphan reconciler. Mirrors the design of
// DestroyFailedPusher: same Pushgateway, separate metric namespace
// (`crucible_vcenter_orphans_*`) and grouped by folder so the alert rules
// can scope per-folder thresholds.
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

// OrphanCountPusher pushes one ReconcileCounts as a small gauge family to
// Pushgateway, scoped by folder. Safe for concurrent use by a single
// worker process. Push is a no-op when BaseURL is empty.
type OrphanCountPusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client
}

// Push emits five gauges (inventory, active, destroyed_synthetic,
// destroyed_other, unknown) plus a run timestamp. The folder label is set
// per series and also used as a grouping label so multiple folders don't
// overwrite each other in Pushgateway.
func (p *OrphanCountPusher) Push(ctx context.Context, folder string, counts ReconcileCounts) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeOrphanCounts(folder, counts)
	grouping := mergeGrouping(p.GroupingLabels, "folder", folder)
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

func serializeOrphanCounts(folder string, c ReconcileCounts) []byte {
	var b bytes.Buffer

	// Single gauge with a "category" label keeps the metric family compact.
	b.WriteString("# HELP crucible_vcenter_orphans_total VMs observed in the vCenter folder, classified by orphan category at the last reconciler run.\n")
	b.WriteString("# TYPE crucible_vcenter_orphans_total gauge\n")
	for _, row := range []struct {
		category string
		value    int
	}{
		{"inventory", c.InventoryVMs},
		{"active", c.Active},
		{"destroyed_synthetic", c.DestroyedSynthetic},
		{"destroyed_other_dryrun", c.DestroyedOther},
		{"unknown_dryrun", c.Unknown},
		{"skipped_recent", c.SkippedRecent},
	} {
		fmt.Fprintf(&b, "crucible_vcenter_orphans_total{folder=%q,category=%q} %d\n", folder, row.category, row.value)
	}

	b.WriteString("# HELP crucible_vcenter_orphans_destroyed_total VMs the reconciler successfully destroyed during the latest run.\n")
	b.WriteString("# TYPE crucible_vcenter_orphans_destroyed_total gauge\n")
	fmt.Fprintf(&b, "crucible_vcenter_orphans_destroyed_total{folder=%q} %d\n", folder, c.Destroyed)

	b.WriteString("# HELP crucible_vcenter_orphans_destroy_failures_total VMs the reconciler attempted to destroy but the operation failed.\n")
	b.WriteString("# TYPE crucible_vcenter_orphans_destroy_failures_total gauge\n")
	fmt.Fprintf(&b, "crucible_vcenter_orphans_destroy_failures_total{folder=%q} %d\n", folder, c.DestroyFailures)

	b.WriteString("# HELP crucible_vcenter_orphans_run_timestamp_seconds Unix time of the latest reconciler run for this folder.\n")
	b.WriteString("# TYPE crucible_vcenter_orphans_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_vcenter_orphans_run_timestamp_seconds{folder=%q} %d\n", folder, time.Now().Unix())

	return b.Bytes()
}

func mergeGrouping(base map[string]string, k, v string) map[string]string {
	out := make(map[string]string, len(base)+1)
	for bk, bv := range base {
		out[bk] = bv
	}
	out[k] = v
	return out
}

func orphansPushURL(base, job string, grouping map[string]string) string {
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
