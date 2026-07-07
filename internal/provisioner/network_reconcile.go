package provisioner

import (
	"context"
	"fmt"
	"log/slog"
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
		if fwRulesAvailable && !hasPodPassRule(fwRules, ifName, row.Subnet) {
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
			if _, err := opn.CreateFirewallRule(ctx, fwRule); err != nil {
				counts.Errors++
				log.Warn("network reconcile: failed to create firewall rule",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "interface", ifName, "error", err)
				continue
			}
			// Track locally so a later pod in the same run doesn't re-create it.
			fwRules = append(fwRules, opnsense.FirewallRuleInfo{
				Interface: ifName, Source: row.Subnet, Action: "pass",
			})
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

// hasPodPassRule reports whether a "pass" rule with the given source subnet is
// already bound to ifName. The interface field may be a comma-joined list of
// logical names (e.g. "lan,opt1"), so each member is checked.
func hasPodPassRule(rules []opnsense.FirewallRuleInfo, ifName, subnet string) bool {
	for _, r := range rules {
		if r.Action != "pass" || r.Source != subnet {
			continue
		}
		for _, part := range strings.Split(r.Interface, ",") {
			if strings.TrimSpace(part) == ifName {
				return true
			}
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
