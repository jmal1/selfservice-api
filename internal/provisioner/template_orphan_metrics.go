package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TemplateOrphanCountPusher pushes one TemplateOrphanCounts as a gauge family
// to Pushgateway, scoped by Templates folder. Push is a no-op when BaseURL is
// empty.
type TemplateOrphanCountPusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client
}

// Push emits category gauges plus destroyed / failure / run-timestamp series.
func (p *TemplateOrphanCountPusher) Push(ctx context.Context, folder string, counts TemplateOrphanCounts) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeTemplateOrphanCounts(folder, counts)
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

func serializeTemplateOrphanCounts(folder string, c TemplateOrphanCounts) []byte {
	var b bytes.Buffer

	b.WriteString("# HELP crucible_template_orphans_total Templates-folder orphan reconciler observations by category.\n")
	b.WriteString("# TYPE crucible_template_orphans_total gauge\n")
	for _, row := range []struct {
		category string
		value    int
	}{
		{"inventory", c.InventoryVMs},
		{"stale_failed", c.StaleFailed},
		{"stale_draft", c.StaleDraft},
		{"inventory_destroyed", c.InventoryDestroyed},
		{"skipped_owned", c.SkippedOwned},
		{"skipped_recent", c.SkippedRecent},
		// A retained VM needs an operator, so it has to be visible somewhere
		// other than a worker log line.
		{"skipped_powered_on", c.SkippedPoweredOn},
		{"skipped_undetermined", c.SkippedUndetermined},
		{"unknown_dryrun", c.UnknownDryRun},
	} {
		fmt.Fprintf(&b, "crucible_template_orphans_total{folder=%q,category=%q} %d\n", folder, row.category, row.value)
	}

	b.WriteString("# HELP crucible_template_orphans_destroyed_total VMs the template orphan reconciler successfully destroyed during the latest run.\n")
	b.WriteString("# TYPE crucible_template_orphans_destroyed_total gauge\n")
	fmt.Fprintf(&b, "crucible_template_orphans_destroyed_total{folder=%q} %d\n", folder, c.Destroyed)

	b.WriteString("# HELP crucible_template_orphans_destroy_failures_total VMs the template orphan reconciler failed to destroy during the latest run.\n")
	b.WriteString("# TYPE crucible_template_orphans_destroy_failures_total gauge\n")
	fmt.Fprintf(&b, "crucible_template_orphans_destroy_failures_total{folder=%q} %d\n", folder, c.DestroyFailures)

	b.WriteString("# HELP crucible_template_orphans_run_timestamp_seconds Unix time of the latest template orphan reconciler run.\n")
	b.WriteString("# TYPE crucible_template_orphans_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_template_orphans_run_timestamp_seconds{folder=%q} %d\n", folder, time.Now().Unix())

	return b.Bytes()
}
