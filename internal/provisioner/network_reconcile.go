package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/opnsense"
)

const defaultNetworkReconcilerVLANParent = "vmx1"

// NetworkReconcilerConfig controls one network reconciliation pass.
type NetworkReconcilerConfig struct {
	VLANParent string
	Pusher     *NetworkReconcilePusher
}

// NetworkReconcileCounts summarizes one network reconciliation pass.
type NetworkReconcileCounts struct {
	AllocatedVLANs        int
	ActivePodVLANs        int
	InterfacesRepaired    int
	SubnetsRepaired       int
	KeaBindingsRepaired   int
	FirewallRulesRepaired int
	VLANsReleased         int
	Errors                int
	KeaRestarted          int
	FirewallApplied       int
}

type networkReconcileOPN interface {
	GetVLANByTag(ctx context.Context, tag int) (*opnsense.VLAN, error)
	CreateVLAN(ctx context.Context, parentIf string, tag int, descr string) (string, error)
	ReconfigureVLANs(ctx context.Context) error
	GetDHCPSubnetByNetwork(ctx context.Context, subnet string) (*opnsense.DHCPSubnet, error)
	CreateDHCPSubnet(ctx context.Context, subnet, poolRange, gateway string) (string, error)
	GetDHCPInterfaces(ctx context.Context) ([]string, error)
	AddDHCPInterface(ctx context.Context, ifName string) error
	RestartDHCP(ctx context.Context) error
	GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error)
	CreateFirewallRule(ctx context.Context, rule opnsense.FirewallRule) (string, error)
	ApplyFirewall(ctx context.Context) error
}

type networkReconcileSSH interface {
	FindInterfaceByVLAN(ctx context.Context, vlanTag int) (string, error)
	AssignInterface(ctx context.Context, vlanTag int, ipAddr string) (string, error)
}

type networkReconcileDB interface {
	ListAllocatedVLANs(ctx context.Context) ([]database.AllocatedVLAN, error)
	ReleaseVLAN(ctx context.Context, podID uuid.UUID) error
}

// ReconcileNetwork is the Provisioner-bound entry point.
func (p *Provisioner) ReconcileNetwork(ctx context.Context, cfg NetworkReconcilerConfig) (NetworkReconcileCounts, error) {
	return reconcileNetwork(ctx, p.opn, p.opnSSH, p.db, p.logger, cfg, time.Now)
}

func reconcileNetwork(
	ctx context.Context,
	opn networkReconcileOPN,
	opnSSH networkReconcileSSH,
	db networkReconcileDB,
	logger *slog.Logger,
	cfg NetworkReconcilerConfig,
	now func() time.Time,
) (NetworkReconcileCounts, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.VLANParent == "" {
		cfg.VLANParent = defaultNetworkReconcilerVLANParent
	}
	log := logger.With("component", "network_reconciler")

	allocations, err := db.ListAllocatedVLANs(ctx)
	if err != nil {
		return NetworkReconcileCounts{}, fmt.Errorf("list allocated vlans: %w", err)
	}

	var counts NetworkReconcileCounts
	counts.AllocatedVLANs = len(allocations)
	var needsKeaRestart bool
	var needsFirewallApply bool

	// Fetch existing firewall rules once so we can verify each active pod has a
	// pass rule on its current interface. Without this, a repaired/renumbered
	// interface has no pass rule (default deny) and pods get DHCP but no
	// internet — the exact failure mode from the 2026-07-07 incident. If the
	// listing fails, skip firewall reconciliation this run rather than risk
	// creating duplicate rules.
	fwRules, fwErr := opn.GetFirewallRules(ctx)
	fwRulesAvailable := fwErr == nil
	if fwErr != nil {
		counts.Errors++
		log.Warn("network reconcile: failed to list firewall rules; skipping firewall repair this run", "error", fwErr)
	}

	for _, row := range allocations {
		if isTerminalPodStatus(row.PodStatus) {
			if err := db.ReleaseVLAN(ctx, row.PodID); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to release terminal pod vlan allocation",
					"pod_id", row.PodID, "pod_name", row.PodName, "pod_status", row.PodStatus,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			counts.VLANsReleased++
			continue
		}

		counts.ActivePodVLANs++
		octet := row.VLANTag - 100
		gateway := fmt.Sprintf("10.100.%d.1/24", octet)
		pool := fmt.Sprintf("10.100.%d.10-10.100.%d.250", octet, octet)
		descr := fmt.Sprintf("Pod-VLAN%d", row.VLANTag)

		vlan, err := opn.GetVLANByTag(ctx, row.VLANTag)
		if err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to read vlan",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "error", err)
			continue
		}
		if vlan == nil {
			if _, err := opn.CreateVLAN(ctx, cfg.VLANParent, row.VLANTag, descr); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to create vlan",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			if err := opn.ReconfigureVLANs(ctx); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to reconfigure vlans",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			needsKeaRestart = true
		}

		ifName, err := opnSSH.FindInterfaceByVLAN(ctx, row.VLANTag)
		if err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to find interface by vlan",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "error", err)
			continue
		}
		if ifName == "" {
			ifName, err = opnSSH.AssignInterface(ctx, row.VLANTag, gateway)
			if err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to assign interface",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			counts.InterfacesRepaired++
			needsKeaRestart = true
			needsFirewallApply = true
		}

		subnet, err := opn.GetDHCPSubnetByNetwork(ctx, row.Subnet)
		if err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to read dhcp subnet",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "subnet", row.Subnet, "error", err)
			continue
		}
		if subnet == nil {
			if _, err := opn.CreateDHCPSubnet(ctx, row.Subnet, pool, gateway); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to create dhcp subnet",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "subnet", row.Subnet, "error", err)
				continue
			}
			counts.SubnetsRepaired++
			needsKeaRestart = true
		}

		selectedInterfaces, err := opn.GetDHCPInterfaces(ctx)
		if err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to read dhcp interfaces",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "interface", ifName, "error", err)
			continue
		}
		if !containsString(selectedInterfaces, ifName) {
			if err := opn.AddDHCPInterface(ctx, ifName); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to bind dhcp interface",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "interface", ifName, "error", err)
				continue
			}
			counts.KeaBindingsRepaired++
			needsKeaRestart = true
		}

		// Ensure a firewall pass rule exists on the pod's current interface.
		// A freshly assigned OPT interface defaults to deny; without this the
		// pod has DHCP but no routed/internet connectivity. Automatic outbound
		// NAT is regenerated by ApplyFirewall below.
		fwRule := opnsense.FirewallRule{
			Enabled:     "1",
			Action:      "pass",
			Interface:   ifName,
			Direction:   "in",
			IPProtocol:  "inet",
			Protocol:    "any",
			Source:      row.Subnet,
			Destination: "any",
			Description: fmt.Sprintf("Allow Pod VLAN %d traffic", row.VLANTag),
		}
		if fwRulesAvailable && !hasEquivalentPassRule(fwRules, fwRule) {
			if _, err := opn.CreateFirewallRule(ctx, fwRule); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to create firewall rule",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "interface", ifName, "error", err)
				continue
			}
			// Track locally (in canonical form) so a later pod in the same run
			// doesn't re-create a content-equivalent rule.
			fwRules = append(fwRules, firewallRuleSignature(fwRule))
			counts.FirewallRulesRepaired++
			needsFirewallApply = true
		}
	}

	if needsKeaRestart {
		if err := opn.RestartDHCP(ctx); err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to restart kea dhcp", "error", err)
		} else {
			counts.KeaRestarted = 1
		}
	}

	if needsFirewallApply {
		if err := opn.ApplyFirewall(ctx); err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to apply firewall", "error", err)
		} else {
			counts.FirewallApplied = 1
		}
	}

	log.Info("network reconcile complete",
		"allocated_vlans", counts.AllocatedVLANs,
		"active_pod_vlans", counts.ActivePodVLANs,
		"interfaces_repaired", counts.InterfacesRepaired,
		"subnets_repaired", counts.SubnetsRepaired,
		"kea_bindings_repaired", counts.KeaBindingsRepaired,
		"firewall_rules_repaired", counts.FirewallRulesRepaired,
		"vlans_released", counts.VLANsReleased,
		"errors", counts.Errors,
		"kea_restarted", counts.KeaRestarted,
		"firewall_applied", counts.FirewallApplied,
	)

	if cfg.Pusher != nil {
		if pushErr := cfg.Pusher.Push(ctx, counts); pushErr != nil {
			log.Warn("network reconcile metric push failed", "error", pushErr)
		}
	}

	return counts, nil
}

func isTerminalPodStatus(status string) bool {
	return status == "destroyed" || status == "destroy_failed"
}

// firewallRuleSignature reduces a to-be-created FirewallRule to the canonical
// content signature used for idempotency comparisons. It mirrors the
// normalization opnsense.GetFirewallRules applies to rules read back from
// firewall/filter/get (lower-cased/trimmed enum fields, canonical interface set)
// so a rule we intend to create compares equal to the same rule already present
// on the firewall.
func firewallRuleSignature(rule opnsense.FirewallRule) opnsense.FirewallRuleInfo {
	return opnsense.FirewallRuleInfo{
		Interface:   canonicalInterfaceList(rule.Interface),
		Direction:   canonicalField(rule.Direction),
		IPProtocol:  canonicalField(rule.IPProtocol),
		Protocol:    canonicalField(rule.Protocol),
		Source:      canonicalField(rule.Source),
		Destination: canonicalField(rule.Destination),
		Action:      canonicalField(rule.Action),
		// The reconciler never sets source/destination ports on the pod pass
		// rule, so they are empty in the signature and must be empty on the
		// existing rule too for a match.
		SourcePort:      "",
		DestinationPort: "",
	}
}

// hasEquivalentPassRule reports whether a content-equivalent rule for the
// desired rule already exists. Matching is by CONTENT signature — interface-set,
// action, direction, ipprotocol, protocol, source(+port) and destination(+port)
// — NOT by description, uuid or sequence, all of which OPNsense
// normalizes/omits. This is the idempotency guard that prevents the reconciler
// from re-adding an identical per-VLAN pass rule every cycle (root cause of the
// 2026-08-02 config.xml bloat / OPNsense OOM incident).
func hasEquivalentPassRule(rules []opnsense.FirewallRuleInfo, desired opnsense.FirewallRule) bool {
	want := firewallRuleSignature(desired)
	for _, r := range rules {
		if canonicalField(r.Action) != want.Action ||
			canonicalField(r.Source) != want.Source ||
			canonicalField(r.SourcePort) != want.SourcePort ||
			canonicalField(r.Destination) != want.Destination ||
			canonicalField(r.DestinationPort) != want.DestinationPort ||
			canonicalField(r.Protocol) != want.Protocol ||
			canonicalField(r.Direction) != want.Direction ||
			canonicalField(r.IPProtocol) != want.IPProtocol {
			continue
		}
		if interfaceListContains(r.Interface, want.Interface) {
			return true
		}
	}
	return false
}

// canonicalField normalizes a firewall enum/string field for comparison.
func canonicalField(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// canonicalInterfaceList normalizes a (possibly comma-joined) interface value
// into a canonical, sorted, comma-joined lower-cased list with each member
// trimmed and empties dropped. Sorting makes the interface set order-insensitive.
func canonicalInterfaceList(v string) string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = canonicalField(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// interfaceListContains reports whether the desired logical interface name
// appears in a candidate rule's (possibly comma-joined) interface field. Both
// sides are compared in canonical form so casing/label differences from the
// OPNsense search API don't defeat the match.
func interfaceListContains(candidate, want string) bool {
	want = canonicalField(want)
	if want == "" {
		return false
	}
	for _, part := range strings.Split(candidate, ",") {
		if canonicalField(part) == want {
			return true
		}
	}
	return false
}

func containsString(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
