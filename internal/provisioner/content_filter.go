package provisioner

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/jmal1/selfservice-api/internal/opnsense"
)

const (
	contentFilterRulePrefix       = "crucible:content-filter:v1:"
	contentFilterDNSBLDescription = "crucible:content-filter:v1"
)

var contentFilterBypassPorts = []string{
	"500", "1080", "1194", "1701", "1723", "3128", "4500",
	"8080", "8118", "9001", "9030", "9050", "9150", "51820",
}

// ContentFilterConfig is deliberately deployment-owned. There is no student or
// instructor API for modifying policy or its permanent allowlist.
type ContentFilterConfig struct {
	Enabled       bool
	SourceNetwork string
	CategoryFeed  string
	Allowlist     []string
}

type contentFilterClient interface {
	podFirewallRuleClient
	UpdateFirewallRule(ctx context.Context, uuid string, rule opnsense.FirewallRule) error
	SupportsSourceScopedSafeSearch(ctx context.Context) (bool, error)
	VerifySourceScopedContentFilter(ctx context.Context, sourceNetwork string) error
	ListDNSBLPolicies(ctx context.Context) ([]opnsense.DNSBLPolicy, error)
	GetDNSBLPolicy(ctx context.Context, uuid string) (opnsense.DNSBLPolicy, error)
	CreateDNSBLPolicy(ctx context.Context, policy opnsense.DNSBLPolicy) (string, error)
	UpdateDNSBLPolicy(ctx context.Context, uuid string, policy opnsense.DNSBLPolicy) error
	DeleteDNSBLPolicy(ctx context.Context, uuid string) error
	RefreshUnboundDNSBL(ctx context.Context) error
}

type contentFilterReconcileResult struct {
	ExpectedRules   int
	MissingRules    int
	DriftedRules    int
	RemovedRules    int
	FirewallMutated bool
	DNSBLMutated    bool
	Healthy         bool
}

func validateContentFilterConfig(cfg ContentFilterConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if canonicalField(cfg.SourceNetwork) != "10.100.0.0/16" {
		return fmt.Errorf("content filter source network must be 10.100.0.0/16, got %q", cfg.SourceNetwork)
	}
	feed, err := url.ParseRequestURI(strings.TrimSpace(cfg.CategoryFeed))
	if err != nil || feed.Scheme != "https" || feed.Host == "" {
		return fmt.Errorf("content filter category feed must be an internal HTTPS URL")
	}
	for _, domain := range cfg.Allowlist {
		if !validPolicyDomain(domain) {
			return fmt.Errorf("invalid permanent allowlist domain %q", domain)
		}
	}
	return nil
}

func desiredContentFilterRules(cfg ContentFilterConfig) []opnsense.FirewallRule {
	source := canonicalField(cfg.SourceNetwork)
	rules := []opnsense.FirewallRule{
		{
			Enabled: "1", Sequence: "100", Quick: "1", Action: "pass",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "(self)", DestinationPort: "53", Log: "0",
			Description: contentFilterRulePrefix + "dns-to-firewall",
		},
		{
			Enabled: "1", Sequence: "110", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "any", DestinationPort: "53", Log: "1",
			Description: contentFilterRulePrefix + "external-dns",
		},
		{
			Enabled: "1", Sequence: "120", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, Destination: "any", DestinationPort: "853", Log: "1",
			Description: contentFilterRulePrefix + "dot-doq",
		},
		{
			Enabled: "1", Sequence: "130", Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source,
			Destination: strings.Join([]string{
				"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4",
				"9.9.9.9", "149.112.112.112", "208.67.222.222", "208.67.220.220",
			}, ","),
			DestinationPort: "443", Log: "1",
			Description: contentFilterRulePrefix + "common-doh",
		},
	}
	for i, port := range contentFilterBypassPorts {
		rules = append(rules, opnsense.FirewallRule{
			Enabled: "1", Sequence: fmt.Sprintf("%d", 140+i), Quick: "1", Action: "block",
			Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "TCP/UDP",
			Source: source, DestinationInvert: "1", Destination: source,
			DestinationPort: port, Log: "1",
			Description: contentFilterRulePrefix + "bypass-port-" + port,
		})
	}
	rules = append(rules, opnsense.FirewallRule{
		Enabled: "1", Sequence: "160", Quick: "1", Action: "block",
		Interface: "", Direction: "in", IPProtocol: "inet", Protocol: "GRE",
		Source: source, DestinationInvert: "1", Destination: source, Log: "1",
		Description: contentFilterRulePrefix + "vpn-gre",
	})
	return rules
}

func reconcileContentFilter(
	ctx context.Context,
	opn contentFilterClient,
	inventory []opnsense.FirewallRuleInfo,
	cfg ContentFilterConfig,
) (contentFilterReconcileResult, error) {
	var result contentFilterReconcileResult
	if err := validateContentFilterConfig(cfg); err != nil {
		return result, err
	}
	if !cfg.Enabled {
		result.Healthy = true
		return result, nil
	}
	supported, err := opn.SupportsSourceScopedSafeSearch(ctx)
	if err != nil {
		return result, fmt.Errorf("check source-scoped SafeSearch support: %w", err)
	}
	if !supported {
		return result, fmt.Errorf("content filter activation blocked: OPNsense 26.1 built-in Force SafeSearch is global; this client does not transactionally manage and verify the proven custom Unbound view required for 10.100.0.0/16")
	}

	desiredRules := desiredContentFilterRules(cfg)
	result.ExpectedRules = len(desiredRules)
	desiredByDescription := make(map[string]opnsense.FirewallRule, len(desiredRules))
	for _, rule := range desiredRules {
		desiredByDescription[rule.Description] = rule
	}
	ownedByDescription := make(map[string][]opnsense.FirewallRuleInfo)
	for _, rule := range inventory {
		if strings.HasPrefix(strings.TrimSpace(rule.Description), contentFilterRulePrefix) {
			ownedByDescription[rule.Description] = append(ownedByDescription[rule.Description], rule)
		}
	}
	for description := range ownedByDescription {
		sort.Slice(ownedByDescription[description], func(i, j int) bool {
			return ownedByDescription[description][i].UUID < ownedByDescription[description][j].UUID
		})
	}

	for description, desired := range desiredByDescription {
		candidates := ownedByDescription[description]
		if len(candidates) == 0 {
			if _, err := opn.CreateFirewallRule(ctx, desired); err != nil {
				return result, fmt.Errorf("create content-filter rule %q: %w", description, err)
			}
			result.MissingRules++
			result.FirewallMutated = true
			continue
		}
		if !equivalentContentFilterRule(candidates[0], desired) {
			if err := opn.UpdateFirewallRule(ctx, candidates[0].UUID, desired); err != nil {
				return result, fmt.Errorf("repair content-filter rule %q: %w", description, err)
			}
			result.DriftedRules++
			result.FirewallMutated = true
		}
		for _, duplicate := range candidates[1:] {
			if err := opn.DeleteFirewallRule(ctx, duplicate.UUID); err != nil {
				return result, fmt.Errorf("delete duplicate content-filter rule %q: %w", duplicate.UUID, err)
			}
			result.RemovedRules++
			result.FirewallMutated = true
		}
	}
	for description, candidates := range ownedByDescription {
		if _, desired := desiredByDescription[description]; desired {
			continue
		}
		for _, stale := range candidates {
			if err := opn.DeleteFirewallRule(ctx, stale.UUID); err != nil {
				return result, fmt.Errorf("delete stale content-filter rule %q: %w", stale.UUID, err)
			}
			result.RemovedRules++
			result.FirewallMutated = true
		}
	}

	dnsResult, err := reconcileContentFilterDNS(ctx, opn, cfg)
	if err != nil {
		return result, err
	}
	result.DNSBLMutated = dnsResult.DNSBLMutated
	result.Healthy = true
	return result, nil
}

func reconcileContentFilterDNS(
	ctx context.Context,
	opn contentFilterClient,
	cfg ContentFilterConfig,
) (contentFilterReconcileResult, error) {
	var result contentFilterReconcileResult
	want := desiredDNSBLPolicy(cfg)
	rows, err := opn.ListDNSBLPolicies(ctx)
	if err != nil {
		return result, err
	}
	var owned []opnsense.DNSBLPolicy
	for _, row := range rows {
		if row.Description == contentFilterDNSBLDescription {
			owned = append(owned, row)
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].UUID < owned[j].UUID })
	if len(owned) == 0 {
		if _, err := opn.CreateDNSBLPolicy(ctx, want); err != nil {
			return result, err
		}
		result.DNSBLMutated = true
	} else {
		current, err := opn.GetDNSBLPolicy(ctx, owned[0].UUID)
		if err != nil {
			return result, err
		}
		if !equivalentDNSBLPolicy(current, want) {
			if err := opn.UpdateDNSBLPolicy(ctx, owned[0].UUID, want); err != nil {
				return result, err
			}
			result.DNSBLMutated = true
		}
		for _, duplicate := range owned[1:] {
			if err := opn.DeleteDNSBLPolicy(ctx, duplicate.UUID); err != nil {
				return result, err
			}
			result.DNSBLMutated = true
		}
	}
	// Model readback does not reveal whether a prior asynchronous activation
	// reached the runtime, so retries refresh and then verify from the approved
	// student-source scope even without model drift.
	if err := opn.RefreshUnboundDNSBL(ctx); err != nil {
		return result, err
	}
	if err := opn.VerifySourceScopedContentFilter(ctx, cfg.SourceNetwork); err != nil {
		return result, fmt.Errorf("verify effective source-scoped content filter: %w", err)
	}
	return result, nil
}

func desiredDNSBLPolicy(cfg ContentFilterConfig) opnsense.DNSBLPolicy {
	allowlist := append([]string(nil), cfg.Allowlist...)
	for i := range allowlist {
		allowlist[i] = canonicalField(allowlist[i])
	}
	sort.Strings(allowlist)
	return opnsense.DNSBLPolicy{
		Enabled:     "1",
		Types:       "hgz014,hgz019,oisd2",
		Lists:       strings.TrimSpace(cfg.CategoryFeed),
		Allowlists:  strings.Join(allowlist, ","),
		SourceNets:  canonicalField(cfg.SourceNetwork),
		NXDomain:    "1",
		CacheTTL:    "3600",
		Description: contentFilterDNSBLDescription,
	}
}

func equivalentContentFilterRule(current opnsense.FirewallRuleInfo, desired opnsense.FirewallRule) bool {
	return canonicalField(current.Enabled) == canonicalField(desired.Enabled) &&
		canonicalField(current.Sequence) == canonicalField(desired.Sequence) &&
		canonicalField(current.Quick) == canonicalField(desired.Quick) &&
		canonicalInterfaceList(current.Interface) == canonicalInterfaceList(desired.Interface) &&
		canonicalFirewallBoolean(current.InterfaceInvert) == canonicalFirewallBoolean(desired.InterfaceInvert) &&
		canonicalField(current.Action) == canonicalField(desired.Action) &&
		canonicalField(current.Direction) == canonicalField(desired.Direction) &&
		canonicalField(current.IPProtocol) == canonicalField(desired.IPProtocol) &&
		canonicalField(current.Protocol) == canonicalField(desired.Protocol) &&
		canonicalCSV(current.Source) == canonicalCSV(desired.Source) &&
		canonicalCSV(current.SourcePort) == canonicalCSV(desired.SourcePort) &&
		canonicalCSV(current.Destination) == canonicalCSV(desired.Destination) &&
		canonicalCSV(current.DestinationPort) == canonicalCSV(desired.DestinationPort) &&
		canonicalFirewallBoolean(current.SourceInvert) == canonicalFirewallBoolean(desired.SourceInvert) &&
		canonicalFirewallBoolean(current.DestinationInvert) == canonicalFirewallBoolean(desired.DestinationInvert) &&
		canonicalField(current.Log) == canonicalField(desired.Log) &&
		strings.TrimSpace(current.Description) == desired.Description
}

func equivalentDNSBLPolicy(current, desired opnsense.DNSBLPolicy) bool {
	return canonicalField(current.Enabled) == canonicalField(desired.Enabled) &&
		canonicalCSV(current.Types) == canonicalCSV(desired.Types) &&
		strings.TrimSpace(current.Lists) == strings.TrimSpace(desired.Lists) &&
		canonicalCSV(current.Allowlists) == canonicalCSV(desired.Allowlists) &&
		canonicalCSV(current.Blocklists) == canonicalCSV(desired.Blocklists) &&
		canonicalCSV(current.Wildcards) == canonicalCSV(desired.Wildcards) &&
		canonicalCSV(current.SourceNets) == canonicalCSV(desired.SourceNets) &&
		canonicalField(current.Address) == canonicalField(desired.Address) &&
		canonicalField(current.NXDomain) == canonicalField(desired.NXDomain) &&
		canonicalField(current.CacheTTL) == canonicalField(desired.CacheTTL) &&
		strings.TrimSpace(current.Description) == strings.TrimSpace(desired.Description)
}

func canonicalCSV(value string) string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = canonicalField(part); part != "" {
			out = append(out, part)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func validPolicyDomain(value string) bool {
	value = canonicalField(value)
	if value == "" || strings.ContainsAny(value, "/:@ ") || !strings.Contains(value, ".") {
		return false
	}

	for _, label := range strings.Split(value, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// InspectContentFilterPolicy performs the same canonical comparison as the
// reconciler without mutating OPNsense. It is used by the read-only synthetic.
func InspectContentFilterPolicy(
	inventory []opnsense.FirewallRuleInfo,
	dnsPolicies []opnsense.DNSBLPolicy,
	cfg ContentFilterConfig,
) error {
	if err := validateContentFilterConfig(cfg); err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	desiredRules := desiredContentFilterRules(cfg)
	desiredByDescription := make(map[string]opnsense.FirewallRule, len(desiredRules))
	for _, desired := range desiredRules {
		desiredByDescription[desired.Description] = desired
	}
	ownedCount := 0
	for _, current := range inventory {
		description := strings.TrimSpace(current.Description)
		if !strings.HasPrefix(description, contentFilterRulePrefix) {
			continue
		}
		ownedCount++
		if _, desired := desiredByDescription[description]; !desired {
			return fmt.Errorf("stale owned content-filter rule %q is present", description)
		}
	}
	if ownedCount != len(desiredRules) {
		return fmt.Errorf("content-filter owned rule count is %d, want %d", ownedCount, len(desiredRules))
	}
	for _, desired := range desiredRules {
		var matches []opnsense.FirewallRuleInfo
		for _, current := range inventory {
			if current.Description == desired.Description {
				matches = append(matches, current)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("content-filter rule %q count is %d, want 1", desired.Description, len(matches))
		}
		if !equivalentContentFilterRule(matches[0], desired) {
			return fmt.Errorf("content-filter rule %q is drifted", desired.Description)
		}
	}
	var owned []opnsense.DNSBLPolicy
	for _, policy := range dnsPolicies {
		if policy.Description == contentFilterDNSBLDescription {
			owned = append(owned, policy)
		}
	}
	if len(owned) != 1 {
		return fmt.Errorf("content-filter DNSBL policy count is %d, want 1", len(owned))
	}
	if !equivalentDNSBLPolicy(owned[0], desiredDNSBLPolicy(cfg)) {
		return fmt.Errorf("content-filter DNSBL policy is drifted")
	}
	return nil
}
