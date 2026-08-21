package provisioner

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/jmal1/selfservice-api/internal/opnsense"
)

const (
	podPassRuleDescriptionPrefix = "crucible:pod-pass:v1:"
	defaultFirewallCleanupLimit  = 100
	defaultMaxFirewallRules      = 4096
	defaultMaxGeneratedPodRules  = 256
)

type podFirewallRuleClient interface {
	GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error)
	CreateFirewallRule(ctx context.Context, rule opnsense.FirewallRule) (string, error)
	DeleteFirewallRule(ctx context.Context, uuid string) error
	ApplyFirewall(ctx context.Context) error
}

type desiredPodFirewallRule struct {
	VLANTag   int
	Rule      opnsense.FirewallRule
	Signature string
}

type podFirewallReconcileResult struct {
	TotalRules     int
	GeneratedRules int
	DuplicateRules int
	StaleRules     int
	CreatedRules   int
	RemovedRules   int
	CleanupLimited bool
	Mutated        bool
}

func podPassFirewallRule(vlanTag int, ifName, subnet string) opnsense.FirewallRule {
	return opnsense.FirewallRule{
		Enabled:     "1",
		Quick:       "1",
		Action:      "pass",
		Interface:   ifName,
		Direction:   "in",
		IPProtocol:  "inet",
		Protocol:    "any",
		Source:      subnet,
		Destination: "any",
		Description: fmt.Sprintf("%svlan=%d", podPassRuleDescriptionPrefix, vlanTag),
	}
}

func newDesiredPodFirewallRule(vlanTag int, ifName, subnet string) desiredPodFirewallRule {
	rule := podPassFirewallRule(vlanTag, ifName, subnet)
	return desiredPodFirewallRule{
		VLANTag:   vlanTag,
		Rule:      rule,
		Signature: podPassSignature(firewallRuleSignature(rule)),
	}
}

func ensurePodFirewallRule(
	ctx context.Context,
	opn podFirewallRuleClient,
	vlanTag int,
	ifName, subnet string,
	maxRules, cleanupLimit int,
) (string, bool, error) {
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list firewall rules before ensure: %w", err)
	}
	desired := newDesiredPodFirewallRule(vlanTag, ifName, subnet)
	_, createdUUIDs, err := reconcilePodFirewallRules(
		ctx,
		opn,
		rules,
		[]desiredPodFirewallRule{desired},
		false,
		maxRules,
		cleanupLimit,
	)
	var createdUUID string
	if len(createdUUIDs) > 0 {
		createdUUID = createdUUIDs[0]
	}
	// Apply even when the model already matches. A prior add may have reached
	// config.xml while its apply call failed, and model readback cannot
	// distinguish that pending runtime state.
	if applyErr := opn.ApplyFirewall(ctx); applyErr != nil {
		return createdUUID, createdUUID != "", fmt.Errorf("apply ensured pod firewall rule: %w", applyErr)
	}
	if err != nil {
		return createdUUID, createdUUID != "", err
	}
	if createdUUID == "" {
		return "", false, nil
	}
	return createdUUID, true, nil
}

func deletePodFirewallRules(
	ctx context.Context,
	opn podFirewallRuleClient,
	ifName, subnet string,
	maxRules, cleanupLimit int,
) (int, error) {
	rules, err := opn.GetFirewallRules(ctx)
	if err != nil {
		return 0, fmt.Errorf("list firewall rules before delete: %w", err)
	}
	maxRules, cleanupLimit = normalizeFirewallBounds(maxRules, cleanupLimit)
	if len(rules) > maxRules {
		return 0, fmt.Errorf("firewall inventory has %d rules, exceeds safe inspection limit %d", len(rules), maxRules)
	}
	want := podPassSignature(opnsense.FirewallRuleInfo{
		Interface:       canonicalInterfaceList(ifName),
		Action:          "pass",
		Direction:       "in",
		IPProtocol:      "inet",
		Protocol:        "any",
		Source:          canonicalField(subnet),
		Destination:     "any",
		SourcePort:      "",
		DestinationPort: "",
	})
	var candidates []opnsense.FirewallRuleInfo
	for _, rule := range rules {
		managed, signature, ambiguous := classifyManagedPodPassRule(rule)
		if ambiguous {
			return 0, fmt.Errorf("ambiguous generated firewall rule %q; refusing destroy cleanup", rule.UUID)
		}
		if managed && signature == want {
			candidates = append(candidates, rule)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UUID < candidates[j].UUID })
	limited := len(candidates) > cleanupLimit
	if len(candidates) > cleanupLimit {
		candidates = candidates[:cleanupLimit]
	}
	for _, rule := range candidates {
		if err := opn.DeleteFirewallRule(ctx, rule.UUID); err != nil {
			return 0, err
		}
	}
	// Apply even when no candidate remains so a retry completes a deletion
	// whose earlier apply failed after the model mutation succeeded.
	if err := opn.ApplyFirewall(ctx); err != nil {
		return 0, fmt.Errorf("apply pod firewall rule deletion: %w", err)
	}
	if limited {
		return len(candidates), fmt.Errorf("pod firewall cleanup limited to %d rules; retry required", cleanupLimit)
	}
	return len(candidates), nil
}

func reconcilePodFirewallRules(
	ctx context.Context,
	opn podFirewallRuleClient,
	rules []opnsense.FirewallRuleInfo,
	desired []desiredPodFirewallRule,
	allowStaleCleanup bool,
	maxRules, cleanupLimit int,
) (podFirewallReconcileResult, []string, error) {
	maxRules, cleanupLimit = normalizeFirewallBounds(maxRules, cleanupLimit)
	result := podFirewallReconcileResult{TotalRules: len(rules)}
	if len(rules) > maxRules {
		return result, nil, fmt.Errorf("firewall inventory has %d rules, exceeds safe inspection limit %d", len(rules), maxRules)
	}

	desiredBySignature := make(map[string]desiredPodFirewallRule, len(desired))
	for _, want := range desired {
		if want.Signature == "" {
			return result, nil, fmt.Errorf("desired pod firewall rule has an empty signature")
		}
		if _, exists := desiredBySignature[want.Signature]; exists {
			return result, nil, fmt.Errorf("duplicate desired pod firewall signature %q", want.Signature)
		}
		desiredBySignature[want.Signature] = want
	}

	managedBySignature := make(map[string][]opnsense.FirewallRuleInfo)
	namedEquivalent := make(map[string]bool)
	for _, rule := range rules {
		managed, signature, ambiguous := classifyManagedPodPassRule(rule)
		if ambiguous {
			return result, nil, fmt.Errorf("ambiguous generated firewall rule %q; refusing mutation", rule.UUID)
		}
		if managed {
			result.GeneratedRules++
			managedBySignature[signature] = append(managedBySignature[signature], rule)
			continue
		}
		if isExactPodPassShape(rule) && strings.TrimSpace(rule.Description) != "" {
			namedEquivalent[podPassSignature(rule)] = true
		}
	}
	for signature := range managedBySignature {
		sort.Slice(managedBySignature[signature], func(i, j int) bool {
			return managedBySignature[signature][i].UUID < managedBySignature[signature][j].UUID
		})
	}

	var deleteQueue []opnsense.FirewallRuleInfo
	for signature, candidates := range managedBySignature {
		_, isDesired := desiredBySignature[signature]
		switch {
		case isDesired && namedEquivalent[signature]:
			result.DuplicateRules += len(candidates)
			deleteQueue = append(deleteQueue, candidates...)
		case isDesired && len(candidates) > 1:
			result.DuplicateRules += len(candidates) - 1
			deleteQueue = append(deleteQueue, candidates[1:]...)
		case !isDesired && allowStaleCleanup:
			result.StaleRules += len(candidates)
			deleteQueue = append(deleteQueue, candidates...)
		}
	}
	sort.Slice(deleteQueue, func(i, j int) bool { return deleteQueue[i].UUID < deleteQueue[j].UUID })
	if len(deleteQueue) > cleanupLimit {
		deleteQueue = deleteQueue[:cleanupLimit]
		result.CleanupLimited = true
	}

	for _, rule := range deleteQueue {
		if err := opn.DeleteFirewallRule(ctx, rule.UUID); err != nil {
			return result, nil, fmt.Errorf("delete generated firewall rule %s: %w", rule.UUID, err)
		}
		result.RemovedRules++
		result.Mutated = true
	}

	var createdUUIDs []string
	for signature, want := range desiredBySignature {
		if namedEquivalent[signature] || len(managedBySignature[signature]) > 0 {
			continue
		}
		uuid, err := opn.CreateFirewallRule(ctx, want.Rule)
		if err != nil {
			return result, createdUUIDs, fmt.Errorf("create generated firewall rule: %w", err)
		}
		createdUUIDs = append(createdUUIDs, uuid)
		result.CreatedRules++
		result.Mutated = true
	}
	return result, createdUUIDs, nil
}

func normalizeFirewallBounds(maxRules, cleanupLimit int) (int, int) {
	if maxRules <= 0 {
		maxRules = defaultMaxFirewallRules
	}
	if cleanupLimit <= 0 {
		cleanupLimit = defaultFirewallCleanupLimit
	}
	return maxRules, cleanupLimit
}

// InspectGeneratedPodFirewallRules validates the conservative ownership
// boundary and enforces a deployment ceiling without mutating OPNsense.
func InspectGeneratedPodFirewallRules(inventory []opnsense.FirewallRuleInfo, maxGenerated int) error {
	if maxGenerated <= 0 {
		maxGenerated = defaultMaxGeneratedPodRules
	}
	generated := 0
	for _, rule := range inventory {
		managed, _, ambiguous := classifyManagedPodPassRule(rule)
		if ambiguous {
			return fmt.Errorf("ambiguous generated firewall rule %q", rule.UUID)
		}
		if managed {
			generated++
		}
	}
	if generated > maxGenerated {
		return fmt.Errorf("generated pod firewall rule count is %d, exceeds ceiling %d", generated, maxGenerated)
	}
	return nil
}

func classifyManagedPodPassRule(rule opnsense.FirewallRuleInfo) (managed bool, signature string, ambiguous bool) {
	owned := strings.HasPrefix(strings.TrimSpace(rule.Description), podPassRuleDescriptionPrefix)
	legacy := strings.TrimSpace(rule.Description) == ""
	if !owned && !legacy {
		return false, "", false
	}
	if isExactPodPassShape(rule) {
		return true, podPassSignature(rule), false
	}
	if owned || looksLikeGeneratedPodPassRule(rule) {
		return false, "", true
	}
	return false, "", false
}

func isExactPodPassShape(rule opnsense.FirewallRuleInfo) bool {
	if rule.UUID == "" ||
		(rule.Enabled != "" && canonicalField(rule.Enabled) != "1") ||
		isTruthyFirewallField(rule.InterfaceInvert) ||
		canonicalField(rule.Action) != "pass" ||
		canonicalField(rule.Direction) != "in" ||
		canonicalField(rule.IPProtocol) != "inet" ||
		canonicalField(rule.Protocol) != "any" ||
		canonicalField(rule.SourcePort) != "" ||
		canonicalField(rule.Destination) != "any" ||
		canonicalField(rule.DestinationPort) != "" ||
		isTruthyFirewallField(rule.SourceInvert) ||
		isTruthyFirewallField(rule.DestinationInvert) {
		return false
	}
	if !isSingleOPTInterface(rule.Interface) {
		return false
	}
	prefix, err := netip.ParsePrefix(canonicalField(rule.Source))
	if err != nil || prefix.Bits() != 24 {
		return false
	}
	studentPrefix := netip.MustParsePrefix("10.100.0.0/16")
	return studentPrefix.Contains(prefix.Addr()) && studentPrefix.Contains(prefix.Masked().Addr())
}

func looksLikeGeneratedPodPassRule(rule opnsense.FirewallRuleInfo) bool {
	if isSingleOPTInterface(rule.Interface) {
		return true
	}
	prefix, err := netip.ParsePrefix(canonicalField(rule.Source))
	return err == nil && prefix.Bits() == 24 && netip.MustParsePrefix("10.100.0.0/16").Contains(prefix.Addr())
}

func isSingleOPTInterface(value string) bool {
	parts := strings.Split(canonicalInterfaceList(value), ",")
	if len(parts) != 1 || !strings.HasPrefix(parts[0], "opt") {
		return false
	}
	_, err := strconv.Atoi(strings.TrimPrefix(parts[0], "opt"))
	return err == nil
}

func isTruthyFirewallField(value string) bool {
	switch canonicalField(value) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

func canonicalFirewallBoolean(value string) string {
	if isTruthyFirewallField(value) {
		return "1"
	}
	return "0"
}

func podPassSignature(rule opnsense.FirewallRuleInfo) string {
	return strings.Join([]string{
		canonicalInterfaceList(rule.Interface),
		canonicalFirewallBoolean(rule.InterfaceInvert),
		canonicalField(rule.Action),
		canonicalField(rule.Direction),
		canonicalField(rule.IPProtocol),
		canonicalField(rule.Protocol),
		canonicalField(rule.Source),
		canonicalField(rule.SourcePort),
		canonicalField(rule.Destination),
		canonicalField(rule.DestinationPort),
		canonicalFirewallBoolean(rule.SourceInvert),
		canonicalFirewallBoolean(rule.DestinationInvert),
	}, "|")
}
