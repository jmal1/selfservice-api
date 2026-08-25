package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/opnsense"
)

const defaultNetworkReconcilerVLANParent = "vmx1"

// NetworkReconcilerConfig controls one network reconciliation pass.
type NetworkReconcilerConfig struct {
	VLANParent           string
	MaxFirewallRules     int
	FirewallCleanupLimit int
	ContentFilter        ContentFilterConfig
	Pusher               *NetworkReconcilePusher
}

// NetworkReconcileCounts summarizes one network reconciliation pass.
type NetworkReconcileCounts struct {
	AllocatedVLANs               int
	ActivePodVLANs               int
	InterfacesRepaired           int
	SubnetsRepaired              int
	KeaBindingsRepaired          int
	FirewallRulesRepaired        int
	FirewallRulesTotal           int
	FirewallRulesGenerated       int
	FirewallRulesDuplicate       int
	FirewallRulesStale           int
	FirewallRulesRemoved         int
	FirewallCleanupLimited       int
	ContentFilterExpected        int
	ContentFilterControllerReady int
	ContentFilterEffectiveReady  int
	ContentFilterMissing         int
	ContentFilterDrifted         int
	ContentFilterRemoved         int
	ContentFilterSuccessAt       int64
	VLANsReleased                int
	Errors                       int
	KeaRestarted                 int
	FirewallApplied              int
}

type networkReconcileOPN interface {
	BeginContentFilterTransaction(ctx context.Context) (context.Context, func(), error)
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
	UpdateFirewallRule(ctx context.Context, uuid string, rule opnsense.FirewallRule) error
	DeleteFirewallRule(ctx context.Context, uuid string) error
	ApplyFirewall(ctx context.Context) error
	GetUnboundSafeSearch(ctx context.Context) (bool, error)
	SupportsSourceScopedSafeSearch(ctx context.Context) (bool, error)
	SnapshotSourceScopedSafeSearch(ctx context.Context) (opnsense.SourceScopedSafeSearchState, error)
	ReadSourceScopedSafeSearchState(ctx context.Context) (opnsense.SourceScopedSafeSearchReadOnlyState, error)
	ConfigureSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error
	RestoreSourceScopedSafeSearch(ctx context.Context, state opnsense.SourceScopedSafeSearchState) error
	InspectSourceScopedSafeSearch(ctx context.Context, sourceNetwork string) error
	CheckStudentIPv6InternetRoute(ctx context.Context, studentInterfaces []string) error
	VerifySourceScopedContentFilter(ctx context.Context, sourceNetwork string) error
	ListDNSBLPolicies(ctx context.Context) ([]opnsense.DNSBLPolicy, error)
	GetDNSBLPolicy(ctx context.Context, uuid string) (opnsense.DNSBLPolicy, error)
	CreateDNSBLPolicy(ctx context.Context, policy opnsense.DNSBLPolicy) (string, error)
	UpdateDNSBLPolicy(ctx context.Context, uuid string, policy opnsense.DNSBLPolicy) error
	DeleteDNSBLPolicy(ctx context.Context, uuid string) error
	ActivateUnboundDNSBL(ctx context.Context, expected []opnsense.DNSBLPolicy) error
	InspectUnboundDNSBLRuntime(ctx context.Context, expected []opnsense.DNSBLPolicy) error
}

type networkReconcileSSH interface {
	FindInterfaceByVLAN(ctx context.Context, vlanTag int) (string, error)
	AssignInterface(ctx context.Context, vlanTag int, ipAddr string) (string, error)
}

type networkReconcileDB interface {
	ListAllocatedVLANs(ctx context.Context) ([]database.AllocatedVLAN, error)
	ReleaseVLAN(ctx context.Context, podID uuid.UUID) error
	WithContentFilterMutationLock(ctx context.Context, mutate func(context.Context) error) error
	GetContentFilterTransaction(ctx context.Context) (*database.ContentFilterTransaction, error)
	CreateContentFilterTransaction(
		ctx context.Context,
		operationID uuid.UUID,
		sourceNetwork string,
		snapshot json.RawMessage,
	) error
	CompleteContentFilterTransaction(ctx context.Context, operationID uuid.UUID) error
	ListContentFilterCanaryReservations(ctx context.Context) ([]string, error)
	ReplaceContentFilterCanaryReservations(ctx context.Context, sources []string) error
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
) (counts NetworkReconcileCounts, retErr error) {
	err := db.WithContentFilterMutationLock(ctx, func(lockCtx context.Context) error {
		var err error
		counts, err = reconcileNetworkLocked(lockCtx, opn, opnSSH, db, logger, cfg, now)
		return err
	})
	if err != nil {
		return counts, err
	}
	return counts, nil
}

func reconcileNetworkLocked(
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
	contentFilterSafeToApply := false
	var desiredFirewallRules []desiredPodFirewallRule
	var retainedSubnets []string
	firewallMappingComplete := true

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
				retainedSubnets = append(retainedSubnets, row.Subnet)
				counts.Errors++
				log.Warn("network reconcile: failed to release terminal pod vlan allocation",
					"pod_id", row.PodID, "pod_name", row.PodName, "pod_status", row.PodStatus,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			counts.VLANsReleased++
			continue
		}
		retainedSubnets = append(retainedSubnets, row.Subnet)
		if row.PodStatus == models.PodStatusDestroying || row.PodStatus == models.PodStatusDestroyFailed {
			// Destruction retries still need the original VLAN/interface/subnet
			// mapping to finish exact firewall cleanup safely. Do not race an
			// in-progress destroy by recreating state it has already removed.
			continue
		}

		counts.ActivePodVLANs++
		octet := row.VLANTag - 100
		gateway := fmt.Sprintf("10.100.%d.1/24", octet)
		pool := fmt.Sprintf("10.100.%d.10-10.100.%d.250", octet, octet)
		descr := fmt.Sprintf("Pod-VLAN%d", row.VLANTag)

		vlan, err := opn.GetVLANByTag(ctx, row.VLANTag)
		if err != nil {
			firewallMappingComplete = false
			counts.Errors++
			log.Warn("network reconcile: failed to read vlan",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "error", err)
			continue
		}
		if vlan == nil {
			if _, err := opn.CreateVLAN(ctx, cfg.VLANParent, row.VLANTag, descr); err != nil {
				firewallMappingComplete = false
				counts.Errors++
				log.Warn("network reconcile: failed to create vlan",
					"pod_id", row.PodID, "pod_name", row.PodName,
					"vlan_tag", row.VLANTag, "error", err)
				continue
			}
			if err := opn.ReconfigureVLANs(ctx); err != nil {
				firewallMappingComplete = false
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
			firewallMappingComplete = false
			counts.Errors++
			log.Warn("network reconcile: failed to find interface by vlan",
				"pod_id", row.PodID, "pod_name", row.PodName,
				"vlan_tag", row.VLANTag, "error", err)
			continue
		}
		if ifName == "" {
			ifName, err = opnSSH.AssignInterface(ctx, row.VLANTag, gateway)
			if err != nil {
				firewallMappingComplete = false
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
		desiredFirewallRules = append(desiredFirewallRules, newDesiredPodFirewallRule(row.VLANTag, ifName, row.Subnet))

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

	}

	if cfg.ContentFilter.Enabled {
		counts.ContentFilterExpected = 1
	}
	var policyResult contentFilterReconcileResult
	var policyErr error
	policyErrorAlreadyCounted := false
	switch {
	case !fwRulesAvailable:
		policyErr = errors.New("firewall inventory is unavailable")
		policyErrorAlreadyCounted = true
	case cfg.ContentFilter.Enabled && !firewallMappingComplete:
		policyErr = errors.New("active pod interface mapping is incomplete")
	case cfg.ContentFilter.Enabled && InspectGeneratedPodFirewallRules(fwRules, 0) != nil:
		policyErr = errors.New("ambiguous generated-rule inventory")
	default:
		activeInterfaces := make([]string, 0, len(desiredFirewallRules))
		for _, desired := range desiredFirewallRules {
			activeInterfaces = append(activeInterfaces, desired.Rule.Interface)
		}
		policyResult, policyErr = reconcileContentFilter(
			ctx,
			opn,
			db,
			retainedSubnets,
			activeInterfaces,
			cfg.ContentFilter,
		)
	}
	counts.ContentFilterMissing = policyResult.MissingRules
	counts.ContentFilterDrifted = policyResult.DriftedRules
	counts.ContentFilterRemoved = policyResult.RemovedRules
	if policyResult.ControllerReady {
		counts.ContentFilterControllerReady = 1
	}
	contentFilterSafeToApply = policyResult.FirewallSafeToApply
	if policyResult.EffectiveReady {
		counts.ContentFilterEffectiveReady = 1
	}
	if policyResult.FirewallApplied {
		counts.FirewallApplied = 1
	}
	if policyErr != nil && !policyErrorAlreadyCounted {
		counts.Errors++
	}
	if policyErr != nil {
		log.Warn("network reconcile: content-filter controller is not ready", "error", policyErr)
	}

	var fwResult podFirewallReconcileResult
	var fwReconcileErr error
	if fwRulesAvailable && contentFilterSafeToApply {
		firewallInventory := fwRules
		if cfg.ContentFilter.Enabled {
			var refreshErr error
			firewallInventory, refreshErr = opn.GetFirewallRules(ctx)
			if refreshErr != nil {
				counts.Errors++
				fwReconcileErr = fmt.Errorf("refresh firewall inventory after content-filter reconciliation: %w", refreshErr)
				log.Warn("network reconcile: skipping generated-rule reconciliation", "error", fwReconcileErr)
			}
		}
		if fwReconcileErr == nil {
			fwResult, _, fwReconcileErr = reconcilePodFirewallRules(
				ctx,
				opn,
				firewallInventory,
				desiredFirewallRules,
				firewallMappingComplete,
				cfg.MaxFirewallRules,
				cfg.FirewallCleanupLimit,
			)
			counts.FirewallRulesTotal = fwResult.TotalRules
			counts.FirewallRulesGenerated = fwResult.GeneratedRules
			counts.FirewallRulesDuplicate = fwResult.DuplicateRules
			counts.FirewallRulesStale = fwResult.StaleRules
			counts.FirewallRulesRepaired = fwResult.CreatedRules
			counts.FirewallRulesRemoved = fwResult.RemovedRules
			if fwResult.CleanupLimited {
				counts.FirewallCleanupLimited = 1
			}
			if fwResult.Mutated {
				needsFirewallApply = true
			}
			if fwReconcileErr != nil {
				counts.Errors++
				log.Warn("network reconcile: firewall ownership reconciliation failed", "error", fwReconcileErr)
			}
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
		if !contentFilterSafeToApply {
			log.Warn("network reconcile: refusing firewall apply while content-filter inspection is unhealthy")
		} else if cfg.ContentFilter.Enabled {
			if err := requireGlobalSafeSearchDisabled(ctx, opn); err != nil {
				counts.Errors++
				counts.ContentFilterControllerReady = 0
				log.Warn("network reconcile: refusing firewall apply while global SafeSearch is not explicitly disabled", "error", err)
			} else if err := opn.ApplyFirewall(ctx); err != nil {
				counts.Errors++
				counts.ContentFilterControllerReady = 0
				log.Warn("network reconcile: failed to apply firewall", "error", err)
			} else {
				counts.FirewallApplied = 1
			}
		} else if err := opn.ApplyFirewall(ctx); err != nil {
			counts.Errors++
			log.Warn("network reconcile: failed to apply firewall", "error", err)
		} else {
			counts.FirewallApplied = 1
		}
	}
	if counts.ContentFilterExpected == 1 && counts.ContentFilterControllerReady == 1 {
		counts.ContentFilterSuccessAt = now().Unix()
	}

	log.Info("network reconcile complete",
		"allocated_vlans", counts.AllocatedVLANs,
		"active_pod_vlans", counts.ActivePodVLANs,
		"interfaces_repaired", counts.InterfacesRepaired,
		"subnets_repaired", counts.SubnetsRepaired,
		"kea_bindings_repaired", counts.KeaBindingsRepaired,
		"firewall_rules_repaired", counts.FirewallRulesRepaired,
		"firewall_rules_total", counts.FirewallRulesTotal,
		"firewall_rules_generated", counts.FirewallRulesGenerated,
		"firewall_rules_duplicate", counts.FirewallRulesDuplicate,
		"firewall_rules_stale", counts.FirewallRulesStale,
		"firewall_rules_removed", counts.FirewallRulesRemoved,
		"firewall_cleanup_limited", counts.FirewallCleanupLimited,
		"content_filter_expected", counts.ContentFilterExpected,
		"content_filter_controller_ready", counts.ContentFilterControllerReady,
		"content_filter_effective_ready", counts.ContentFilterEffectiveReady,
		"content_filter_missing", counts.ContentFilterMissing,
		"content_filter_drifted", counts.ContentFilterDrifted,
		"content_filter_removed", counts.ContentFilterRemoved,
		"content_filter_success_timestamp", counts.ContentFilterSuccessAt,
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
	return status == "destroyed"
}

// firewallRuleSignature reduces a to-be-created FirewallRule to the canonical
// content signature used for idempotency comparisons. It mirrors the
// normalization opnsense.GetFirewallRules applies to rules read back from
// firewall/filter/get (lower-cased/trimmed enum fields, canonical interface set)
// so a rule we intend to create compares equal to the same rule already present
// on the firewall.
func firewallRuleSignature(rule opnsense.FirewallRule) opnsense.FirewallRuleInfo {
	return opnsense.FirewallRuleInfo{
		Quick:             canonicalFirewallBoolean(rule.Quick),
		Log:               canonicalFirewallBoolean(rule.Log),
		Interface:         canonicalInterfaceList(rule.Interface),
		InterfaceInvert:   canonicalFirewallBoolean(rule.InterfaceInvert),
		Direction:         canonicalField(rule.Direction),
		IPProtocol:        canonicalField(rule.IPProtocol),
		Protocol:          canonicalField(rule.Protocol),
		SourceInvert:      canonicalFirewallBoolean(rule.SourceInvert),
		Source:            canonicalField(rule.Source),
		DestinationInvert: canonicalFirewallBoolean(rule.DestinationInvert),
		Destination:       canonicalField(rule.Destination),
		Action:            canonicalField(rule.Action),
		// The reconciler never sets source/destination ports on the pod pass
		// rule, so they are empty in the signature and must be empty on the
		// existing rule too for a match.
		SourcePort:      "",
		DestinationPort: "",
	}
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

func containsString(values []string, needle string) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}
