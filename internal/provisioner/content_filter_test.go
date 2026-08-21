package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/opnsense"
)

type fakeContentFilterOPN struct {
	*fakeNetworkOPN
	sourceScopedSupported bool
	verifyRuntimeCalls    int
	verifyRuntimeErr      error
	dnsPolicies           []opnsense.DNSBLPolicy
	createDNSBLCalls      []opnsense.DNSBLPolicy
	updateDNSBLCalls      []opnsense.DNSBLPolicy
	deleteDNSBLCalls      []string
	refreshDNSBLCalls     int
	refreshDNSBLErr       error
}

func (f *fakeContentFilterOPN) SupportsSourceScopedSafeSearch(context.Context) (bool, error) {
	return f.sourceScopedSupported, nil
}

func (f *fakeContentFilterOPN) VerifySourceScopedContentFilter(context.Context, string) error {
	f.verifyRuntimeCalls++
	return f.verifyRuntimeErr
}

func (f *fakeContentFilterOPN) ListDNSBLPolicies(context.Context) ([]opnsense.DNSBLPolicy, error) {
	out := make([]opnsense.DNSBLPolicy, len(f.dnsPolicies))
	copy(out, f.dnsPolicies)
	return out, nil
}

func (f *fakeContentFilterOPN) GetDNSBLPolicy(_ context.Context, ruleUUID string) (opnsense.DNSBLPolicy, error) {
	for _, policy := range f.dnsPolicies {
		if policy.UUID == ruleUUID {
			return policy, nil
		}
	}
	return opnsense.DNSBLPolicy{}, fmt.Errorf("DNSBL policy %s not found", ruleUUID)
}

func (f *fakeContentFilterOPN) CreateDNSBLPolicy(_ context.Context, policy opnsense.DNSBLPolicy) (string, error) {
	policy.UUID = "dns-created"
	f.createDNSBLCalls = append(f.createDNSBLCalls, policy)
	f.dnsPolicies = append(f.dnsPolicies, policy)
	return policy.UUID, nil
}

func (f *fakeContentFilterOPN) UpdateDNSBLPolicy(_ context.Context, ruleUUID string, policy opnsense.DNSBLPolicy) error {
	policy.UUID = ruleUUID
	f.updateDNSBLCalls = append(f.updateDNSBLCalls, policy)
	for i := range f.dnsPolicies {
		if f.dnsPolicies[i].UUID == ruleUUID {
			f.dnsPolicies[i] = policy
			return nil
		}
	}
	return fmt.Errorf("DNSBL policy %s not found", ruleUUID)
}

func (f *fakeContentFilterOPN) DeleteDNSBLPolicy(_ context.Context, ruleUUID string) error {
	f.deleteDNSBLCalls = append(f.deleteDNSBLCalls, ruleUUID)
	for i := range f.dnsPolicies {
		if f.dnsPolicies[i].UUID == ruleUUID {
			f.dnsPolicies = append(f.dnsPolicies[:i], f.dnsPolicies[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeContentFilterOPN) RefreshUnboundDNSBL(context.Context) error {
	f.refreshDNSBLCalls++
	return f.refreshDNSBLErr
}

func validContentFilterConfig() ContentFilterConfig {
	return ContentFilterConfig{
		Enabled:       true,
		SourceNetwork: "10.100.0.0/16",
		CategoryFeed:  "https://student-filter-feed.lab.jmal.io",
		Allowlist:     []string{"course.example"},
	}
}

func TestDesiredContentFilterRules_AreGlobalQuickAndPrecedeInterfacePasses(t *testing.T) {
	rules := desiredContentFilterRules(validContentFilterConfig())
	if len(rules) != 4+len(contentFilterBypassPorts)+1 {
		t.Fatalf("len(rules) = %d", len(rules))
	}
	lastSequence := 0
	for _, rule := range rules {
		if rule.Interface != "" {
			t.Fatalf("%s is not global/floating: interface=%q", rule.Description, rule.Interface)
		}
		if rule.Quick != "1" || rule.Source != "10.100.0.0/16" {
			t.Fatalf("%s lacks quick/source scope: %+v", rule.Description, rule)
		}
		var sequence int
		if _, err := fmt.Sscanf(rule.Sequence, "%d", &sequence); err != nil {
			t.Fatalf("parse sequence %q: %v", rule.Sequence, err)
		}
		if sequence <= lastSequence || sequence >= 1000 {
			t.Fatalf("global sequence ordering invalid: previous=%d current=%d", lastSequence, sequence)
		}
		lastSequence = sequence
		if rule.Action == "block" && rule.Log != "1" {
			t.Fatalf("blocked-only telemetry lost on %s", rule.Description)
		}
		if rule.Action == "pass" && rule.Log != "0" {
			t.Fatalf("pass rule %s must not log allowed traffic", rule.Description)
		}
	}
	seenBypassPorts := make(map[string]bool, len(contentFilterBypassPorts))
	for _, rule := range rules {
		if strings.HasPrefix(rule.Description, contentFilterRulePrefix+"bypass-port-") {
			if rule.DestinationInvert != "1" || rule.Destination != "10.100.0.0/16" {
				t.Fatalf("bypass-port rule must preserve intra-pod labs while blocking external destinations: %+v", rule)
			}
			if strings.Contains(rule.DestinationPort, ",") {
				t.Fatalf("OPNsense PortField is single-valued, got %+v", rule)
			}
			seenBypassPorts[rule.DestinationPort] = true
		}
	}
	for _, port := range []string{"1080", "3128", "9001", "9050", "51820"} {
		if !seenBypassPorts[port] {
			t.Fatalf("bypass-port rule is missing %s", port)
		}
	}
}

func TestDesiredDNSBLPolicy_UsesOnlyRunning26OptionsAndRequiredFeed(t *testing.T) {
	policy := desiredDNSBLPolicy(validContentFilterConfig())
	if policy.Types != "hgz014,hgz019,oisd2" {
		t.Fatalf("Types = %q", policy.Types)
	}
	if policy.Lists == "" || policy.SourceNets != "10.100.0.0/16" || policy.NXDomain != "1" {
		t.Fatalf("incomplete DNSBL policy: %+v", policy)
	}
}

func TestReconcileContentFilter_ConvergesAndIsIdempotent(t *testing.T) {
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
	}
	cfg := validContentFilterConfig()

	first, err := reconcileContentFilter(context.Background(), opn, nil, cfg)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if !first.Healthy || first.MissingRules != 19 || !first.FirewallMutated || !first.DNSBLMutated {
		t.Fatalf("unexpected first result: %+v", first)
	}
	if len(opn.createFirewallCalls) != 19 || len(opn.createDNSBLCalls) != 1 ||
		opn.refreshDNSBLCalls != 1 || opn.verifyRuntimeCalls != 1 {
		t.Fatalf("first reconcile did not apply and verify complete policy: fw=%d dns=%d refresh=%d verify=%d",
			len(opn.createFirewallCalls), len(opn.createDNSBLCalls),
			opn.refreshDNSBLCalls, opn.verifyRuntimeCalls)
	}

	second, err := reconcileContentFilter(context.Background(), opn, opn.firewallRules, cfg)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if !second.Healthy || second.FirewallMutated || second.DNSBLMutated {
		t.Fatalf("second reconcile was not idempotent: %+v", second)
	}
	if len(opn.createFirewallCalls) != 19 || len(opn.createDNSBLCalls) != 1 {
		t.Fatalf("idempotent pass created more policy: fw=%d dns=%d",
			len(opn.createFirewallCalls), len(opn.createDNSBLCalls))
	}
	if opn.refreshDNSBLCalls != 2 || opn.verifyRuntimeCalls != 2 {
		t.Fatalf("runtime activation must be retried and verified on an idempotent model: refresh=%d verify=%d",
			opn.refreshDNSBLCalls, opn.verifyRuntimeCalls)
	}
}

func TestReconcileContentFilter_RetriesUnboundActivationAfterModelMutation(t *testing.T) {
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
		verifyRuntimeErr:      errors.New("effective query still misses DNSBL"),
	}
	cfg := validContentFilterConfig()
	if _, err := reconcileContentFilter(context.Background(), opn, nil, cfg); err == nil {
		t.Fatal("first reconcile must report runtime activation failure")
	}
	opn.verifyRuntimeErr = nil
	result, err := reconcileContentFilter(context.Background(), opn, opn.firewallRules, cfg)
	if err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if !result.Healthy || result.FirewallMutated || result.DNSBLMutated {
		t.Fatalf("retry should activate the converged model without rewriting it: %+v", result)
	}
	if opn.refreshDNSBLCalls != 2 || opn.verifyRuntimeCalls != 2 {
		t.Fatalf("activation retry calls: refresh=%d verify=%d", opn.refreshDNSBLCalls, opn.verifyRuntimeCalls)
	}
}

func TestReconcileContentFilter_RepairsSabotagedGlobalRule(t *testing.T) {
	cfg := validContentFilterConfig()
	want := desiredContentFilterRules(cfg)
	var inventory []opnsense.FirewallRuleInfo
	for i, rule := range want {
		inventory = append(inventory, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	inventory[1].Quick = "0"
	inventory[1].Sequence = "999999"
	inventory[1].Source = "any"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: inventory},
		sourceScopedSupported: true,
		dnsPolicies: []opnsense.DNSBLPolicy{
			func() opnsense.DNSBLPolicy {
				p := desiredDNSBLPolicy(cfg)
				p.UUID = "dns"
				return p
			}(),
		},
	}

	result, err := reconcileContentFilter(context.Background(), opn, inventory, cfg)
	if err != nil {
		t.Fatalf("reconcileContentFilter: %v", err)
	}
	if result.DriftedRules != 1 || len(opn.updateFirewallCalls) != 1 {
		t.Fatalf("sabotage was not repaired: result=%+v updates=%v", result, opn.updateFirewallCalls)
	}
	repaired := opn.updateFirewallCalls[0]
	if repaired.Quick != "1" || repaired.Sequence != "110" || repaired.Source != "10.100.0.0/16" {
		t.Fatalf("repair did not restore ordering/scope: %+v", repaired)
	}
}

func TestInspectContentFilterPolicy_RejectsUnknownOwnedRule(t *testing.T) {
	cfg := validContentFilterConfig()
	var inventory []opnsense.FirewallRuleInfo
	for i, rule := range desiredContentFilterRules(cfg) {
		inventory = append(inventory, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	stale := inventory[0]
	stale.UUID = "stale-bypass"
	stale.Description = contentFilterRulePrefix + "stale-allow-all"
	stale.Destination = "any"
	inventory = append(inventory, stale)

	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns"
	err := InspectContentFilterPolicy(inventory, []opnsense.DNSBLPolicy{dns}, cfg)
	if err == nil || !strings.Contains(err.Error(), "stale owned") {
		t.Fatalf("unknown owned bypass must fail inspection, got %v", err)
	}
}

func TestReconcileContentFilter_InvalidFeedFailsBeforeAnyMutation(t *testing.T) {
	opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
	cfg := validContentFilterConfig()
	cfg.CategoryFeed = ""
	if _, err := reconcileContentFilter(context.Background(), opn, nil, cfg); err == nil {
		t.Fatal("missing required internal feed was accepted")
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.createDNSBLCalls) != 0 {
		t.Fatalf("partial policy mutation occurred: fw=%v dns=%v",
			opn.createFirewallCalls, opn.createDNSBLCalls)
	}
}

func TestReconcileContentFilter_GlobalSafeSearchLimitationFailsBeforeMutation(t *testing.T) {
	opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
	if _, err := reconcileContentFilter(context.Background(), opn, nil, validContentFilterConfig()); err == nil ||
		!strings.Contains(err.Error(), "Force SafeSearch is global") {
		t.Fatalf("global SafeSearch limitation was not surfaced: %v", err)
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.createDNSBLCalls) != 0 || opn.refreshDNSBLCalls != 0 {
		t.Fatalf("unsupported source-scoped policy mutated OPNsense: fw=%v dns=%v refresh=%d",
			opn.createFirewallCalls, opn.createDNSBLCalls, opn.refreshDNSBLCalls)
	}
}

func TestContentFilterRulesDoNotDependOnDynamicOPTMapping(t *testing.T) {
	cfg := validContentFilterConfig()
	first := desiredContentFilterRules(cfg)
	second := desiredContentFilterRules(cfg)
	for i := range first {
		if !equivalentContentFilterRule(opnsenseFilterGetReadback(first[i], fmt.Sprintf("a-%d", i)), second[i]) {
			t.Fatalf("rule %d drifted without any policy change", i)
		}
		if first[i].Interface != "" {
			t.Fatalf("rule %d unexpectedly depends on an optN interface", i)
		}
	}
}

func TestEquivalentContentFilterRule_NormalizesLiveFalseInversions(t *testing.T) {
	desired := desiredContentFilterRules(validContentFilterConfig())[0]
	current := opnsenseFilterGetReadback(desired, "live-rule")
	if current.InterfaceInvert != "0" || current.SourceInvert != "0" || current.DestinationInvert != "0" {
		t.Fatalf("test readback is not production-shaped: %+v", current)
	}
	if !equivalentContentFilterRule(current, desired) {
		t.Fatalf("live false-like inversion readback drifted from empty desired fields: current=%+v desired=%+v", current, desired)
	}
	current.SourceInvert = "1"
	if equivalentContentFilterRule(current, desired) {
		t.Fatal("true source inversion was normalized as false")
	}
}
