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
		Enabled:             true,
		SourceNetwork:       "10.100.0.0/16",
		CategoryFeedBaseURL: "https://student-filter-feed.lab.jmal.io",
		Allowlist:           []string{"course.example"},
	}
}

func TestDesiredContentFilterRules_AreGlobalQuickAndPrecedeInterfacePasses(t *testing.T) {
	rules := desiredContentFilterRules(validContentFilterConfig())
	if len(rules) != 7+len(contentFilterBypassPorts)+3 {
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

	expectedAntiBypass := map[string]struct {
		protocol          string
		port              string
		destination       string
		destinationInvert string
	}{
		contentFilterRulePrefix + "doq-784":  {protocol: "UDP", port: "784", destination: "any"},
		contentFilterRulePrefix + "doq-8853": {protocol: "UDP", port: "8853", destination: "any"},
		contentFilterRulePrefix + "quic-443": {protocol: "UDP", port: "443", destination: "any"},
		contentFilterRulePrefix + "vpn-gre":  {protocol: "GRE", destination: "10.100.0.0/16", destinationInvert: "1"},
		contentFilterRulePrefix + "vpn-esp":  {protocol: "ESP", destination: "10.100.0.0/16", destinationInvert: "1"},
		contentFilterRulePrefix + "vpn-ah":   {protocol: "AH", destination: "10.100.0.0/16", destinationInvert: "1"},
	}
	for _, rule := range rules {
		want, ok := expectedAntiBypass[rule.Description]
		if !ok {
			continue
		}
		if rule.Action != "block" || rule.Protocol != want.protocol ||
			rule.DestinationPort != want.port || rule.Destination != want.destination ||
			rule.DestinationInvert != want.destinationInvert || rule.Interface != "" ||
			rule.Source != "10.100.0.0/16" || rule.Quick != "1" || rule.Log != "1" {
			t.Fatalf("anti-bypass rule %q has unsafe shape: %+v", rule.Description, rule)
		}
		delete(expectedAntiBypass, rule.Description)
	}
	if len(expectedAntiBypass) != 0 {
		t.Fatalf("missing anti-bypass rules: %v", expectedAntiBypass)
	}
	for _, rule := range rules {
		if rule.DestinationPort == "443" && rule.Destination == "any" && rule.Protocol != "UDP" {
			t.Fatalf("only UDP 443 may be blocked globally: %+v", rule)
		}
	}
}

func TestDesiredDNSBLPolicy_PinsCapacityTestedRunning26OptionsAndRequiredFeed(t *testing.T) {
	policy := desiredDNSBLPolicy(validContentFilterConfig())
	if policy.Types != "hgz014,hgz021,oisd2" {
		t.Fatalf("Types = %q", policy.Types)
	}
	for _, forbidden := range []string{"hgz019", "hgz020", "hgz022"} {
		if strings.Contains(policy.Types, forbidden) {
			t.Fatalf("Types %q includes forbidden selector %q", policy.Types, forbidden)
		}
	}
	const wantLists = "https://student-filter-feed.lab.jmal.io/lists/drogue.txt," +
		"https://student-filter-feed.lab.jmal.io/lists/agressif.txt," +
		"https://student-filter-feed.lab.jmal.io/lists/dangerous_material.txt," +
		"https://student-filter-feed.lab.jmal.io/lists/audio-video.txt," +
		"https://student-filter-feed.lab.jmal.io/lists/social_networks.txt," +
		"https://student-filter-feed.lab.jmal.io/lists/weapons.txt"
	if policy.Lists != wantLists {
		t.Fatalf("Lists = %q, want %q", policy.Lists, wantLists)
	}
	withSlash := validContentFilterConfig()
	withSlash.CategoryFeedBaseURL += "/"
	if err := validateContentFilterConfig(withSlash); err != nil {
		t.Fatalf("trailing slash base URL rejected: %v", err)
	}
	if got := desiredDNSBLPolicy(withSlash).Lists; got != wantLists {
		t.Fatalf("trailing slash Lists = %q, want %q", got, wantLists)
	}
	if policy.SourceNets != "10.100.0.0/16" || policy.NXDomain != "1" {
		t.Fatalf("incomplete DNSBL policy: %+v", policy)
	}
}

func TestReconcileContentFilter_ReadOnlyInspectionPassesWithoutMutation(t *testing.T) {
	cfg := validContentFilterConfig()
	var inventory []opnsense.FirewallRuleInfo
	for i, rule := range desiredContentFilterRules(cfg) {
		inventory = append(inventory, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	manualFirewall := opnsenseFilterGetReadback(opnsense.FirewallRule{
		Enabled: "1", Quick: "1", Action: "pass", Direction: "in",
		IPProtocol: "inet", Protocol: "TCP", Source: "10.10.10.0/24",
		Destination: "10.100.0.0/16", Description: "Instructor management access",
	}, "manual-firewall")
	inventory = append(inventory, manualFirewall)
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns-owned"
	manualDNS := opnsense.DNSBLPolicy{UUID: "dns-manual", Description: "Instructor DNS policy"}
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: inventory},
		sourceScopedSupported: true,
		dnsPolicies:           []opnsense.DNSBLPolicy{manualDNS, dns},
	}

	result, err := reconcileContentFilter(context.Background(), opn, inventory, cfg)
	if err != nil {
		t.Fatalf("reconcileContentFilter: %v", err)
	}
	if !result.Healthy || result.ExpectedRules != len(desiredContentFilterRules(cfg)) {
		t.Fatalf("unexpected inspection result: %+v", result)
	}
	if opn.verifyRuntimeCalls != 1 {
		t.Fatalf("effective verification calls = %d, want 1", opn.verifyRuntimeCalls)
	}
	assertNoContentFilterMutation(t, opn)
	if !containsString(ruleUUIDs(opn.firewallRules), "manual-firewall") ||
		len(opn.dnsPolicies) != 2 || opn.dnsPolicies[0].UUID != "dns-manual" {
		t.Fatalf("manual policy changed: firewall=%v dns=%+v", ruleUUIDs(opn.firewallRules), opn.dnsPolicies)
	}
}

func TestReconcileContentFilter_ReadOnlyFailuresNeverStagePartialPolicy(t *testing.T) {
	cfg := validContentFilterConfig()
	var healthyInventory []opnsense.FirewallRuleInfo
	for i, rule := range desiredContentFilterRules(cfg) {
		healthyInventory = append(healthyInventory, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	healthyDNS := desiredDNSBLPolicy(cfg)
	healthyDNS.UUID = "dns"
	drifted := append([]opnsense.FirewallRuleInfo(nil), healthyInventory...)
	drifted[1].Quick = "0"
	duplicateDNS := healthyDNS
	duplicateDNS.UUID = "dns-duplicate"

	tests := []struct {
		name      string
		inventory []opnsense.FirewallRuleInfo
		policies  []opnsense.DNSBLPolicy
		verifyErr error
	}{
		{name: "missing firewall", policies: []opnsense.DNSBLPolicy{healthyDNS}},
		{name: "missing DNSBL", inventory: healthyInventory},
		{name: "drifted firewall", inventory: drifted, policies: []opnsense.DNSBLPolicy{healthyDNS}},
		{name: "duplicate DNSBL", inventory: healthyInventory, policies: []opnsense.DNSBLPolicy{healthyDNS, duplicateDNS}},
		{name: "runtime verification", inventory: healthyInventory, policies: []opnsense.DNSBLPolicy{healthyDNS}, verifyErr: errors.New("effective query failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opn := &fakeContentFilterOPN{
				fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: tt.inventory},
				sourceScopedSupported: true,
				dnsPolicies:           tt.policies,
				verifyRuntimeErr:      tt.verifyErr,
			}
			if _, err := reconcileContentFilter(context.Background(), opn, tt.inventory, cfg); err == nil {
				t.Fatal("unsafe or incomplete policy was accepted")
			}
			assertNoContentFilterMutation(t, opn)
		})
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
	for _, value := range []string{
		"",
		"http://student-filter-feed.lab.jmal.io",
		"https://example.com",
		"https://STUDENT-FILTER-FEED.LAB.JMAL.IO",
		"https://student-filter-feed.lab.jmal.io/lists/drogue.txt",
		"https://student-filter-feed.lab.jmal.io?list=drogue",
		"https://student-filter-feed.lab.jmal.io#fragment",
		"https://user@student-filter-feed.lab.jmal.io",
		"https://student-filter-feed.lab.jmal.io:443",
	} {
		t.Run(value, func(t *testing.T) {
			opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
			cfg := validContentFilterConfig()
			cfg.CategoryFeedBaseURL = value
			if _, err := reconcileContentFilter(context.Background(), opn, nil, cfg); err == nil {
				t.Fatalf("invalid feed base URL %q was accepted", value)
			}
			assertNoContentFilterMutation(t, opn)
		})
	}
}

func TestReconcileContentFilter_UnmanagedSourceScopedSafeSearchFailsBeforeMutation(t *testing.T) {
	opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
	if _, err := reconcileContentFilter(context.Background(), opn, nil, validContentFilterConfig()); err == nil ||
		!strings.Contains(err.Error(), "built-in Force SafeSearch is global") ||
		!strings.Contains(err.Error(), "does not transactionally manage") {
		t.Fatalf("source-scoped SafeSearch integration limitation was not surfaced: %v", err)
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.createDNSBLCalls) != 0 || opn.refreshDNSBLCalls != 0 {
		t.Fatalf("unsupported source-scoped policy mutated OPNsense: fw=%v dns=%v refresh=%d",
			opn.createFirewallCalls, opn.createDNSBLCalls, opn.refreshDNSBLCalls)
	}
}

func assertNoContentFilterMutation(t *testing.T, opn *fakeContentFilterOPN) {
	t.Helper()
	if len(opn.createFirewallCalls) != 0 ||
		len(opn.updateFirewallCalls) != 0 ||
		len(opn.deleteFirewallCalls) != 0 ||
		opn.applyFirewallCalls != 0 ||
		len(opn.createDNSBLCalls) != 0 ||
		len(opn.updateDNSBLCalls) != 0 ||
		len(opn.deleteDNSBLCalls) != 0 ||
		opn.refreshDNSBLCalls != 0 {
		t.Fatalf("read-only content-filter path mutated policy: createFW=%v updateFW=%v deleteFW=%v apply=%d createDNS=%v updateDNS=%v deleteDNS=%v refresh=%d",
			opn.createFirewallCalls, opn.updateFirewallCalls, opn.deleteFirewallCalls,
			opn.applyFirewallCalls, opn.createDNSBLCalls, opn.updateDNSBLCalls,
			opn.deleteDNSBLCalls, opn.refreshDNSBLCalls)
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
	current = opnsenseFilterGetReadback(desired, "live-rule")
	current.Quick = "2"
	if equivalentContentFilterRule(current, desired) {
		t.Fatal("malformed quick value matched desired true")
	}
}
