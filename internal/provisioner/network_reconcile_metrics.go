package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NetworkReconcilePusher publishes network reconciler gauges to Pushgateway.
type NetworkReconcilePusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client
}

// Push emits one network reconcile sample set to Pushgateway.
func (p *NetworkReconcilePusher) Push(ctx context.Context, counts NetworkReconcileCounts) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeNetworkReconcileCounts(counts)
	target := orphansPushURL(p.BaseURL, p.Job, mergeGrouping(p.GroupingLabels, "component", "network_reconcile"))
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

func serializeNetworkReconcileCounts(c NetworkReconcileCounts) []byte {
	var b bytes.Buffer

	b.WriteString("# HELP crucible_network_reconcile_total VLAN allocation state at the latest network reconciler run.\n")
	b.WriteString("# TYPE crucible_network_reconcile_total gauge\n")
	for _, row := range []struct {
		category string
		value    int
	}{
		{"allocated_vlans", c.AllocatedVLANs},
		{"active_pod_vlans", c.ActivePodVLANs},
		{"vlans_released", c.VLANsReleased},
	} {
		fmt.Fprintf(&b, "crucible_network_reconcile_total{category=%q} %d\n", row.category, row.value)
	}

	b.WriteString("# HELP crucible_network_reconcile_repaired_total Drift healed by the network reconciler in the latest run.\n")
	b.WriteString("# TYPE crucible_network_reconcile_repaired_total gauge\n")
	for _, row := range []struct {
		kind  string
		value int
	}{
		{"interface", c.InterfacesRepaired},
		{"subnet", c.SubnetsRepaired},
		{"kea_binding", c.KeaBindingsRepaired},
		{"firewall_rule", c.FirewallRulesRepaired},
	} {
		fmt.Fprintf(&b, "crucible_network_reconcile_repaired_total{kind=%q} %d\n", row.kind, row.value)
	}

	b.WriteString("# HELP crucible_network_reconcile_errors_total Errors encountered while reconciling network state in the latest run.\n")
	b.WriteString("# TYPE crucible_network_reconcile_errors_total gauge\n")
	fmt.Fprintf(&b, "crucible_network_reconcile_errors_total %d\n", c.Errors)

	b.WriteString("# HELP crucible_network_reconcile_kea_restarted Whether Kea was restarted during this run (0 or 1).\n")
	b.WriteString("# TYPE crucible_network_reconcile_kea_restarted gauge\n")
	fmt.Fprintf(&b, "crucible_network_reconcile_kea_restarted %d\n", c.KeaRestarted)

	b.WriteString("# HELP crucible_network_reconcile_firewall_applied Whether the firewall/NAT ruleset was reloaded during this run (0 or 1).\n")
	b.WriteString("# TYPE crucible_network_reconcile_firewall_applied gauge\n")
	fmt.Fprintf(&b, "crucible_network_reconcile_firewall_applied %d\n", c.FirewallApplied)

	b.WriteString("# HELP crucible_opnsense_firewall_rules OPNsense firewall inventory and generated pod-rule health at the latest reconcile.\n")
	b.WriteString("# TYPE crucible_opnsense_firewall_rules gauge\n")
	for _, row := range []struct {
		kind  string
		value int
	}{
		{"total", c.FirewallRulesTotal},
		{"generated", c.FirewallRulesGenerated},
		{"duplicate", c.FirewallRulesDuplicate},
		{"stale", c.FirewallRulesStale},
		{"removed", c.FirewallRulesRemoved},
	} {
		fmt.Fprintf(&b, "crucible_opnsense_firewall_rules{kind=%q} %d\n", row.kind, row.value)
	}

	b.WriteString("# HELP crucible_opnsense_firewall_cleanup_limited Whether generated-rule cleanup hit its per-run deletion bound (0 or 1).\n")
	b.WriteString("# TYPE crucible_opnsense_firewall_cleanup_limited gauge\n")
	fmt.Fprintf(&b, "crucible_opnsense_firewall_cleanup_limited %d\n", c.FirewallCleanupLimited)

	b.WriteString("# HELP crucible_content_filter_policy Content-filter intent, controller convergence, effective readiness, and repaired drift at the latest reconcile.\n")
	b.WriteString("# TYPE crucible_content_filter_policy gauge\n")
	for _, row := range []struct {
		kind  string
		value int
	}{
		{"expected", c.ContentFilterExpected},
		{"controller_ready", c.ContentFilterControllerReady},
		{"effective_ready", c.ContentFilterEffectiveReady},
		{"missing", c.ContentFilterMissing},
		{"drifted", c.ContentFilterDrifted},
		{"removed", c.ContentFilterRemoved},
	} {
		fmt.Fprintf(&b, "crucible_content_filter_policy{kind=%q} %d\n", row.kind, row.value)
	}
	if c.ContentFilterSuccessAt > 0 {
		b.WriteString("# HELP crucible_content_filter_last_success_timestamp_seconds Unix time of the latest successful firewall and Unbound policy convergence.\n")
		b.WriteString("# TYPE crucible_content_filter_last_success_timestamp_seconds gauge\n")
		fmt.Fprintf(&b, "crucible_content_filter_last_success_timestamp_seconds %d\n", c.ContentFilterSuccessAt)
	}

	b.WriteString("# HELP crucible_network_reconcile_run_timestamp_seconds Unix time of the latest network reconciler run.\n")
	b.WriteString("# TYPE crucible_network_reconcile_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_network_reconcile_run_timestamp_seconds %d\n", time.Now().Unix())

	return b.Bytes()
}
