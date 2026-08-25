package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/opnsense"
)

type fakeContentFilterOPN struct {
	*fakeNetworkOPN
	sourceScopedSupported  bool
	verifyRuntimeCalls     int
	verifyRuntimeErr       error
	dnsPolicies            []opnsense.DNSBLPolicy
	createDNSBLCalls       []opnsense.DNSBLPolicy
	updateDNSBLCalls       []opnsense.DNSBLPolicy
	deleteDNSBLCalls       []string
	activateDNSBLCalls     int
	inspectDNSBLCalls      int
	safeState              opnsense.SourceScopedSafeSearchState
	safeRecoveryPending    bool
	configureSafeCalls     int
	restoreSafeCalls       int
	inspectSafeCalls       int
	ipv6Calls              int
	failOnce               map[string]error
	globalSafeSearch       bool
	globalSafeSearchCalls  int
	globalSafeSearchAtCall int
	transaction            *database.ContentFilterTransaction
	canaryReservations     []string
	armRollbackSabotage    bool
	sabotageRollbackCreate bool
}

func (f *fakeContentFilterOPN) fail(name string) error {
	if f.failOnce == nil {
		return nil
	}
	err := f.failOnce[name]
	delete(f.failOnce, name)
	return err
}

func (f *fakeContentFilterOPN) SupportsSourceScopedSafeSearch(context.Context) (bool, error) {
	if err := f.fail("supports-safe"); err != nil {
		return false, err
	}
	return f.sourceScopedSupported, nil
}

func (f *fakeContentFilterOPN) GetUnboundSafeSearch(context.Context) (bool, error) {
	f.globalSafeSearchCalls++
	if err := f.fail("global-safe"); err != nil {
		return false, err
	}
	if f.globalSafeSearchAtCall > 0 && f.globalSafeSearchCalls == f.globalSafeSearchAtCall {
		return true, nil
	}
	return f.globalSafeSearch, nil
}

func (f *fakeContentFilterOPN) GetContentFilterTransaction(context.Context) (*database.ContentFilterTransaction, error) {
	if err := f.fail("get-journal"); err != nil {
		return nil, err
	}
	return f.transaction, nil
}

func (f *fakeContentFilterOPN) CreateContentFilterTransaction(
	_ context.Context,
	operationID uuid.UUID,
	sourceNetwork string,
	snapshot json.RawMessage,
) error {
	if err := f.fail("create-journal"); err != nil {
		return err
	}
	f.transaction = &database.ContentFilterTransaction{
		OperationID:   operationID,
		SourceNetwork: sourceNetwork,
		Snapshot:      append(json.RawMessage(nil), snapshot...),
	}
	return nil
}

func (f *fakeContentFilterOPN) CompleteContentFilterTransaction(_ context.Context, operationID uuid.UUID) error {
	if err := f.fail("complete-journal"); err != nil {
		return err
	}
	if f.transaction != nil && f.transaction.OperationID != operationID {
		return errors.New("transaction identity changed")
	}
	f.transaction = nil
	return nil
}

func (f *fakeContentFilterOPN) ListContentFilterCanaryReservations(context.Context) ([]string, error) {
	if err := f.fail("list-reservations"); err != nil {
		return nil, err
	}
	return append([]string(nil), f.canaryReservations...), nil
}

func (f *fakeContentFilterOPN) ReplaceContentFilterCanaryReservations(_ context.Context, sources []string) error {
	if err := f.fail("replace-reservations"); err != nil {
		return err
	}
	f.canaryReservations = append([]string(nil), sources...)
	return nil
}

func (f *fakeContentFilterOPN) VerifySourceScopedContentFilter(context.Context, string) error {
	f.verifyRuntimeCalls++
	return f.verifyRuntimeErr
}

func (f *fakeContentFilterOPN) SnapshotSourceScopedSafeSearch(context.Context) (opnsense.SourceScopedSafeSearchState, error) {
	if err := f.fail("snapshot-safe"); err != nil {
		return opnsense.SourceScopedSafeSearchState{}, err
	}
	return cloneSafeSearchState(f.safeState), nil
}

func (f *fakeContentFilterOPN) ReadSourceScopedSafeSearchState(context.Context) (opnsense.SourceScopedSafeSearchReadOnlyState, error) {
	if err := f.fail("read-safe"); err != nil {
		return opnsense.SourceScopedSafeSearchReadOnlyState{}, err
	}
	return opnsense.SourceScopedSafeSearchReadOnlyState{
		State:           cloneSafeSearchState(f.safeState),
		RecoveryPending: f.safeRecoveryPending,
	}, nil
}

func (f *fakeContentFilterOPN) ConfigureSourceScopedSafeSearch(_ context.Context, source string) error {
	f.configureSafeCalls++
	if err := f.fail("configure-safe"); err != nil {
		return err
	}
	f.safeState = opnsense.SourceScopedSafeSearchState{Exists: true, Content: []byte(source)}
	return nil
}

func (f *fakeContentFilterOPN) RestoreSourceScopedSafeSearch(_ context.Context, state opnsense.SourceScopedSafeSearchState) error {
	f.restoreSafeCalls++
	if err := f.fail("restore-safe"); err != nil {
		return err
	}
	f.safeState = cloneSafeSearchState(state)
	return nil
}

func (f *fakeContentFilterOPN) InspectSourceScopedSafeSearch(_ context.Context, source string) error {
	f.inspectSafeCalls++
	if err := f.fail("inspect-safe"); err != nil {
		return err
	}
	if !f.safeState.Exists || string(f.safeState.Content) != source {
		return errors.New("SafeSearch state mismatch")
	}
	return nil
}

func (f *fakeContentFilterOPN) CheckStudentIPv6InternetRoute(context.Context, []string) error {
	f.ipv6Calls++
	return f.fail("ipv6")
}

func (f *fakeContentFilterOPN) GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error) {
	if err := f.fail("get-firewall"); err != nil {
		return nil, err
	}
	return f.fakeNetworkOPN.GetFirewallRules(ctx)
}

func (f *fakeContentFilterOPN) CreateFirewallRule(ctx context.Context, rule opnsense.FirewallRule) (string, error) {
	if err := f.fail("create-firewall"); err != nil {
		return "", err
	}
	ruleUUID, err := f.fakeNetworkOPN.CreateFirewallRule(ctx, rule)
	if err == nil && f.sabotageRollbackCreate {
		duplicate := opnsenseFilterGetReadback(rule, ruleUUID+"-duplicate")
		f.firewallRules = append(f.firewallRules, duplicate)
	}
	return ruleUUID, err
}

func (f *fakeContentFilterOPN) UpdateFirewallRule(ctx context.Context, uuid string, rule opnsense.FirewallRule) error {
	if err := f.fail("update-firewall"); err != nil {
		return err
	}
	return f.fakeNetworkOPN.UpdateFirewallRule(ctx, uuid, rule)
}

func (f *fakeContentFilterOPN) DeleteFirewallRule(ctx context.Context, uuid string) error {
	if err := f.fail("delete-firewall"); err != nil {
		return err
	}
	return f.fakeNetworkOPN.DeleteFirewallRule(ctx, uuid)
}

func (f *fakeContentFilterOPN) ApplyFirewall(ctx context.Context) error {
	if err := f.fail("apply-firewall"); err != nil {
		return err
	}
	return f.fakeNetworkOPN.ApplyFirewall(ctx)
}

func (f *fakeContentFilterOPN) ListDNSBLPolicies(context.Context) ([]opnsense.DNSBLPolicy, error) {
	if err := f.fail("list-dnsbl"); err != nil {
		return nil, err
	}
	out := make([]opnsense.DNSBLPolicy, len(f.dnsPolicies))
	copy(out, f.dnsPolicies)
	return out, nil
}

func (f *fakeContentFilterOPN) GetDNSBLPolicy(_ context.Context, ruleUUID string) (opnsense.DNSBLPolicy, error) {
	if err := f.fail("get-dnsbl"); err != nil {
		return opnsense.DNSBLPolicy{}, err
	}
	for _, policy := range f.dnsPolicies {
		if policy.UUID == ruleUUID {
			return policy, nil
		}
	}
	return opnsense.DNSBLPolicy{}, fmt.Errorf("DNSBL policy %s not found", ruleUUID)
}

func (f *fakeContentFilterOPN) CreateDNSBLPolicy(_ context.Context, policy opnsense.DNSBLPolicy) (string, error) {
	if err := f.fail("create-dnsbl"); err != nil {
		return "", err
	}
	policy.UUID = fmt.Sprintf("dns-%d", len(f.createDNSBLCalls)+1)
	f.createDNSBLCalls = append(f.createDNSBLCalls, policy)
	f.dnsPolicies = append(f.dnsPolicies, policy)
	return policy.UUID, nil
}

func (f *fakeContentFilterOPN) UpdateDNSBLPolicy(_ context.Context, ruleUUID string, policy opnsense.DNSBLPolicy) error {
	if err := f.fail("update-dnsbl"); err != nil {
		return err
	}
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
	if err := f.fail("delete-dnsbl"); err != nil {
		return err
	}
	f.deleteDNSBLCalls = append(f.deleteDNSBLCalls, ruleUUID)
	for i := range f.dnsPolicies {
		if f.dnsPolicies[i].UUID == ruleUUID {
			f.dnsPolicies = append(f.dnsPolicies[:i], f.dnsPolicies[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeContentFilterOPN) ActivateUnboundDNSBL(context.Context, []opnsense.DNSBLPolicy) error {
	f.activateDNSBLCalls++
	return f.fail("activate-dnsbl")
}

func (f *fakeContentFilterOPN) InspectUnboundDNSBLRuntime(context.Context, []opnsense.DNSBLPolicy) error {
	f.inspectDNSBLCalls++
	err := f.fail("inspect-dnsbl")
	if err != nil && f.armRollbackSabotage {
		f.sabotageRollbackCreate = true
	}
	return err
}

func validContentFilterConfig() ContentFilterConfig {
	return ContentFilterConfig{
		Enabled:             true,
		SourceNetwork:       contentFilterFinalSource,
		CategoryFeedBaseURL: contentFilterFeedBaseURL,
		Allowlist:           []string{"course.example"},
	}
}

func TestDesiredContentFilterRules_AreGlobalQuickOrderedAndPreserveIntendedTraffic(t *testing.T) {
	rules := desiredContentFilterRules(validContentFilterConfig())
	if len(rules) != 7+len(contentFilterBypassPorts)+3 {
		t.Fatalf("len(rules) = %d", len(rules))
	}
	lastSequence := 0
	for _, rule := range rules {
		if rule.Interface != "" || rule.Quick != "1" || rule.Source != contentFilterFinalSource {
			t.Fatalf("%s is not global, quick, and source scoped: %+v", rule.Description, rule)
		}
		sequence, err := strconvAtoiForTest(rule.Sequence)
		if err != nil || sequence <= lastSequence || sequence >= 1000 {
			t.Fatalf("invalid floating-rule ordering: previous=%d current=%q", lastSequence, rule.Sequence)
		}
		lastSequence = sequence
		if rule.Action == "block" && rule.Log != "1" {
			t.Fatalf("blocked-only telemetry lost on %s", rule.Description)
		}
		if rule.DestinationPort == "443" && rule.Destination == "any" && rule.Protocol != "UDP" {
			t.Fatalf("ordinary TCP 443 was blocked: %+v", rule)
		}
		if strings.HasPrefix(rule.Description, contentFilterRulePrefix+"bypass-port-") ||
			strings.HasPrefix(rule.Description, contentFilterRulePrefix+"vpn-") {
			if rule.DestinationInvert != "1" || rule.Destination != contentFilterFinalSource {
				t.Fatalf("internal lab tunnel traffic was not preserved: %+v", rule)
			}
		}
	}
	first := rules[0]
	if first.Action != "pass" || first.Destination != "(self)" || first.DestinationPort != "53" {
		t.Fatalf("local firewall DNS is not explicitly preserved first: %+v", first)
	}
	if rules[1].Action != "block" || rules[1].DestinationPort != "53" {
		t.Fatalf("external DNS deny does not follow local DNS pass: %+v", rules[1])
	}
}

func TestDesiredDNSBLPolicy_PinsSixFeedsAndMaintainedBypassDestinations(t *testing.T) {
	policy := desiredDNSBLPolicy(validContentFilterConfig())
	if policy.Types != "hgz014,hgz021,oisd2" {
		t.Fatalf("Types = %q", policy.Types)
	}
	lists := strings.Split(policy.Lists, ",")
	if len(lists) != 6 {
		t.Fatalf("custom list count = %d, want 6: %v", len(lists), lists)
	}
	for _, path := range contentFilterFeedPaths {
		if !containsString(lists, contentFilterFeedBaseURL+path) {
			t.Fatalf("missing required internal feed %s", path)
		}
	}
	for _, domain := range []string{"dns.google", "torproject.org", "protonvpn.com", "psiphon.ca"} {
		if !containsString(strings.Split(policy.Wildcards, ","), domain) {
			t.Fatalf("maintained bypass destination %q is absent", domain)
		}
	}
	if policy.SourceNets != contentFilterFinalSource || policy.Description != contentFilterDNSBLDescription {
		t.Fatalf("DNSBL ownership/scope is incomplete: %+v", policy)
	}
}

func TestValidateContentFilterConfig_BoundsFinalAndInactiveCanarySources(t *testing.T) {
	final := validContentFilterConfig()
	if err := validateContentFilterConfig(final); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"10.99.0.0/16", "10.100.1.0/24", "10.100.0.0/17", "10.100.0.1/16"} {
		cfg := final
		cfg.SourceNetwork = source
		if err := validateContentFilterConfig(cfg); err == nil {
			t.Fatalf("accepted arbitrary final source %q", source)
		}
	}
	canary := final
	canary.Canary = true
	canary.SourceNetwork = "10.100.222.0/24"
	if err := validateContentFilterConfig(canary); err != nil {
		t.Fatalf("bounded canary rejected: %v", err)
	}
	if err := validateContentFilterAllocations(canary, []string{"10.100.221.0/24"}); err != nil {
		t.Fatalf("inactive canary rejected: %v", err)
	}
	if err := validateContentFilterAllocations(canary, []string{"10.100.222.0/24"}); err == nil {
		t.Fatal("active canary subnet was accepted")
	}
	for _, source := range []string{"10.99.1.0/24", "10.100.0.0/23", "192.168.1.0/24"} {
		cfg := canary
		cfg.SourceNetwork = source
		if err := validateContentFilterConfig(cfg); err == nil {
			t.Fatalf("accepted unbounded canary %q", source)
		}
	}
	for _, retained := range []string{
		"10.99.1.0/24",
		"10.100.1.1/24",
		"10.100.1.0/25",
		"not-a-cidr",
	} {
		if err := validateContentFilterAllocations(final, []string{retained}); err == nil {
			t.Fatalf("accepted invalid retained final-mode subnet %q", retained)
		}
	}
}

func TestReconcileContentFilter_ConvergesExactOwnedStateAndPreservesManualObjects(t *testing.T) {
	cfg := validContentFilterConfig()
	manualFirewall := opnsenseFilterGetReadback(opnsense.FirewallRule{
		Enabled: "1", Quick: "1", Action: "pass", Interface: "lan", Direction: "in",
		IPProtocol: "inet", Protocol: "TCP", Source: "10.10.10.0/24",
		Destination: contentFilterFinalSource, Description: "Instructor management access",
	}, "manual-firewall")
	manualDNS := opnsense.DNSBLPolicy{UUID: "manual-dns", Description: "Instructor DNS policy"}
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{manualFirewall}},
		sourceScopedSupported: true,
		dnsPolicies:           []opnsense.DNSBLPolicy{manualDNS},
	}

	result, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg)
	if err != nil {
		t.Fatalf("reconcileContentFilter: %v", err)
	}
	if !result.ControllerReady || result.EffectiveReady || !result.FirewallApplied {
		t.Fatalf("controller/effective semantics are wrong: %+v", result)
	}
	if opn.verifyRuntimeCalls != 0 {
		t.Fatalf("controller called unsupported effective verification %d times", opn.verifyRuntimeCalls)
	}
	if err := InspectContentFilterPolicy(opn.firewallRules, ownedDNSPolicies(opn.dnsPolicies), cfg); err != nil {
		t.Fatalf("converged policy inspection: %v", err)
	}
	if !containsString(ruleUUIDs(opn.firewallRules), "manual-firewall") ||
		len(opn.dnsPolicies) != 2 || opn.dnsPolicies[0].UUID != "manual-dns" {
		t.Fatalf("manual objects changed: firewall=%v dns=%+v", ruleUUIDs(opn.firewallRules), opn.dnsPolicies)
	}
	if opn.transaction != nil {
		t.Fatalf("committed transaction journal was retained: %+v", opn.transaction)
	}
}

func TestReconcileContentFilter_ExactNoOpDoesNotReactivateFirewallOrDNSBL(t *testing.T) {
	cfg := validContentFilterConfig()
	var rules []opnsense.FirewallRuleInfo
	for i, rule := range desiredContentFilterRules(cfg) {
		rules = append(rules, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns-owned"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: rules},
		sourceScopedSupported: true,
		dnsPolicies:           []opnsense.DNSBLPolicy{dns},
		safeState:             opnsense.SourceScopedSafeSearchState{Exists: true, Content: []byte(cfg.SourceNetwork)},
	}

	result, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ControllerReady || result.FirewallApplied {
		t.Fatalf("no-op controller result = %+v", result)
	}
	if opn.applyFirewallCalls != 0 || opn.activateDNSBLCalls != 0 {
		t.Fatalf("exact no-op reactivated policy: firewall=%d dnsbl=%d",
			opn.applyFirewallCalls, opn.activateDNSBLCalls)
	}
	if opn.inspectDNSBLCalls != 2 {
		t.Fatalf("exact no-op did not prove DNSBL runtime before and after commit: %d", opn.inspectDNSBLCalls)
	}
}

func TestReconcileContentFilter_GlobalSafeSearchBlocksBeforeMutation(t *testing.T) {
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
		globalSafeSearch:      true,
	}

	if _, err := reconcileContentFilter(
		context.Background(),
		opn,
		opn,
		nil,
		[]string{"opt7"},
		validContentFilterConfig(),
	); err == nil || !strings.Contains(err.Error(), "global Unbound SafeSearch") {
		t.Fatalf("global SafeSearch was accepted: %v", err)
	}
	assertNoContentFilterMutation(t, opn)
	if opn.transaction != nil {
		t.Fatal("global SafeSearch guard persisted a transaction journal")
	}
}

func TestReconcileContentFilter_GlobalSafeSearchFlipRollsBackBeforeEveryActivationBoundary(t *testing.T) {
	for _, call := range []int{3, 4, 5} {
		t.Run(fmt.Sprintf("call-%d", call), func(t *testing.T) {
			opn := &fakeContentFilterOPN{
				fakeNetworkOPN:         &fakeNetworkOPN{},
				sourceScopedSupported:  true,
				globalSafeSearchAtCall: call,
			}
			_, err := reconcileContentFilter(
				context.Background(),
				opn,
				opn,
				nil,
				[]string{"opt7"},
				validContentFilterConfig(),
			)
			if err == nil || !strings.Contains(err.Error(), "global Unbound SafeSearch") {
				t.Fatalf("global SafeSearch flip on call %d was accepted: %v", call, err)
			}
			if opn.transaction != nil {
				t.Fatalf("global SafeSearch flip on call %d retained a verified rollback journal", call)
			}
			if len(opn.firewallRules) != 0 || len(ownedDNSPolicies(opn.dnsPolicies)) != 0 || opn.safeState.Exists {
				t.Fatalf("global SafeSearch flip on call %d left owned state: firewall=%v dnsbl=%v safe=%+v",
					call, ruleUUIDs(opn.firewallRules), opn.dnsPolicies, opn.safeState)
			}
		})
	}
}

func TestReconcileContentFilter_CanaryReservationLifecycleIsDurable(t *testing.T) {
	canary := validContentFilterConfig()
	canary.Canary = true
	canary.SourceNetwork = "10.100.222.0/24"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
		canaryReservations:    []string{"10.100.221.0/24"},
	}
	if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, canary); err != nil {
		t.Fatalf("canary reconcile: %v", err)
	}
	if !reflect.DeepEqual(opn.canaryReservations, []string{canary.SourceNetwork}) {
		t.Fatalf("canary reservation = %v", opn.canaryReservations)
	}

	final := validContentFilterConfig()
	if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, final); err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if len(opn.canaryReservations) != 0 {
		t.Fatalf("final commit retained canary exclusions: %v", opn.canaryReservations)
	}
}

func TestReconcileContentFilter_DisabledIsReadOnlyAndReportsResidualState(t *testing.T) {
	cfg := ContentFilterConfig{}
	clean := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
	result, err := reconcileContentFilter(context.Background(), clean, clean, nil, nil, cfg)
	if err != nil || !result.ControllerReady || result.EffectiveReady {
		t.Fatalf("clean disabled state mismatch: result=%+v err=%v", result, err)
	}
	assertNoContentFilterMutation(t, clean)

	residualRule := opnsenseFilterGetReadback(desiredContentFilterRules(validContentFilterConfig())[0], "residual")
	dirty := &fakeContentFilterOPN{
		fakeNetworkOPN: &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{residualRule}},
		safeState:      opnsense.SourceScopedSafeSearchState{Exists: true, Content: []byte("residual")},
		transaction: &database.ContentFilterTransaction{
			OperationID: uuid.New(),
			Snapshot:    json.RawMessage(`{"version":1}`),
		},
		canaryReservations: []string{"10.100.222.0/24"},
	}
	result, err = reconcileContentFilter(context.Background(), dirty, dirty, nil, nil, cfg)
	if err == nil || result.ControllerReady || !strings.Contains(err.Error(), "state remains") {
		t.Fatalf("disabled residual state was reported ready: result=%+v err=%v", result, err)
	}
	assertNoContentFilterMutation(t, dirty)
	if dirty.transaction == nil {
		t.Fatal("disabled inspection mutated the durable transaction journal")
	}
	if !reflect.DeepEqual(dirty.canaryReservations, []string{"10.100.222.0/24"}) {
		t.Fatalf("disabled inspection changed canary reservations: %v", dirty.canaryReservations)
	}
}

func TestRecoverContentFilterTransaction_RestoresExactSnapshotAndPreservesManualState(t *testing.T) {
	cfg := validContentFilterConfig()
	owned := opnsenseFilterGetReadback(desiredContentFilterRules(cfg)[0], "snapshot-owned")
	manual := opnsenseFilterGetReadback(opnsense.FirewallRule{
		Enabled: "1", Quick: "1", Action: "pass", Interface: "lan", Direction: "in",
		IPProtocol: "inet", Protocol: "any", Source: "10.10.10.0/24",
		Destination: "any", Description: "manual",
	}, "manual")
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "snapshot-dns"
	snapshot := contentFilterSnapshot{
		Version:            contentFilterSnapshotVersion,
		Firewall:           []opnsense.FirewallRuleInfo{owned},
		DNSBL:              []opnsense.DNSBLPolicy{dns},
		SafeSearch:         opnsense.SourceScopedSafeSearchState{Exists: true, Content: []byte("snapshot-safe")},
		CanaryReservations: []string{"10.100.221.0/24"},
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN: &fakeNetworkOPN{
			firewallRules: []opnsense.FirewallRuleInfo{manual, opnsenseFilterGetReadback(desiredContentFilterRules(cfg)[1], "partial")},
		},
		dnsPolicies:        []opnsense.DNSBLPolicy{},
		safeState:          opnsense.SourceScopedSafeSearchState{Exists: true, Content: []byte("partial-safe")},
		canaryReservations: []string{"10.100.222.0/24"},
		transaction: &database.ContentFilterTransaction{
			OperationID:   uuid.New(),
			SourceNetwork: cfg.SourceNetwork,
			Snapshot:      snapshotJSON,
		},
	}
	if err := recoverContentFilterTransaction(context.Background(), opn, opn); err != nil {
		t.Fatalf("recoverContentFilterTransaction: %v", err)
	}
	if err := compareContentFilterFirewallSnapshot(opn.firewallRules, snapshot.Firewall); err != nil {
		t.Fatal(err)
	}
	if err := compareContentFilterDNSBLSnapshot(ownedDNSPolicies(opn.dnsPolicies), snapshot.DNSBL); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opn.safeState, snapshot.SafeSearch) {
		t.Fatalf("SafeSearch recovery mismatch: got=%+v want=%+v", opn.safeState, snapshot.SafeSearch)
	}
	if !containsString(ruleUUIDs(opn.firewallRules), "manual") {
		t.Fatalf("manual firewall object was removed: %v", ruleUUIDs(opn.firewallRules))
	}
	if opn.transaction != nil {
		t.Fatalf("recovered journal remains: %+v", opn.transaction)
	}
	if !reflect.DeepEqual(opn.canaryReservations, snapshot.CanaryReservations) {
		t.Fatalf("canary reservation recovery mismatch: got=%v want=%v", opn.canaryReservations, snapshot.CanaryReservations)
	}
}

func TestReconcileContentFilter_RecoversJournalBeforeNewAllocationValidation(t *testing.T) {
	cfg := validContentFilterConfig()
	cfg.Canary = true
	cfg.SourceNetwork = "10.100.222.0/24"
	snapshot := contentFilterSnapshot{
		Version: contentFilterSnapshotVersion,
	}
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
		canaryReservations:    []string{cfg.SourceNetwork},
		transaction: &database.ContentFilterTransaction{
			OperationID:   uuid.New(),
			SourceNetwork: cfg.SourceNetwork,
			Snapshot:      snapshotJSON,
		},
	}

	_, err = reconcileContentFilter(
		context.Background(),
		opn,
		opn,
		[]string{cfg.SourceNetwork},
		[]string{"opt7"},
		cfg,
	)
	if err == nil || !strings.Contains(err.Error(), "retained as pod subnet") {
		t.Fatalf("new invalid allocation was accepted after recovery: %v", err)
	}
	if opn.transaction != nil {
		t.Fatal("allocation validation ran before durable recovery")
	}
	if len(opn.canaryReservations) != 0 {
		t.Fatalf("recovery did not restore prior reservations: %v", opn.canaryReservations)
	}
}

func TestReconcileContentFilter_RollsBackEveryPostSnapshotBoundary(t *testing.T) {
	cfg := validContentFilterConfig()
	cfg.Canary = true
	cfg.SourceNetwork = "10.100.222.0/24"
	boundaries := []string{
		"replace-reservations",
		"create-firewall",
		"create-dnsbl",
		"configure-safe",
		"apply-firewall",
		"activate-dnsbl",
		"inspect-safe",
		"inspect-dnsbl",
		"complete-journal",
	}
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			manual := opnsenseFilterGetReadback(opnsense.FirewallRule{
				Enabled: "1", Quick: "1", Action: "pass", Interface: "lan", Direction: "in",
				IPProtocol: "inet", Protocol: "any", Source: "10.10.10.0/24",
				Destination: "any", Description: "manual",
			}, "manual")
			opn := &fakeContentFilterOPN{
				fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{manual}},
				sourceScopedSupported: true,
				dnsPolicies:           []opnsense.DNSBLPolicy{{UUID: "manual-dns", Description: "manual"}},
				safeState:             opnsense.SourceScopedSafeSearchState{},
				failOnce:              map[string]error{boundary: errors.New("injected " + boundary)},
			}
			beforeFirewall := canonicalFirewallState(opn.firewallRules)
			beforeDNS := canonicalDNSState(opn.dnsPolicies)
			beforeSafe := cloneSafeSearchState(opn.safeState)
			beforeReservations := append([]string(nil), opn.canaryReservations...)

			if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg); err == nil {
				t.Fatal("injected boundary failure succeeded")
			}
			if got := canonicalFirewallState(opn.firewallRules); !reflect.DeepEqual(got, beforeFirewall) {
				t.Fatalf("firewall rollback mismatch:\n got %v\nwant %v", got, beforeFirewall)
			}
			if got := canonicalDNSState(opn.dnsPolicies); !reflect.DeepEqual(got, beforeDNS) {
				t.Fatalf("DNSBL rollback mismatch:\n got %v\nwant %v", got, beforeDNS)
			}
			if !reflect.DeepEqual(opn.safeState, beforeSafe) {
				t.Fatalf("SafeSearch rollback mismatch: got=%+v want=%+v", opn.safeState, beforeSafe)
			}
			if !reflect.DeepEqual(opn.canaryReservations, beforeReservations) {
				t.Fatalf("canary reservation rollback mismatch: got=%v want=%v", opn.canaryReservations, beforeReservations)
			}
			if opn.transaction != nil {
				t.Fatalf("verified rollback retained journal: %+v", opn.transaction)
			}
		})
	}
}

func TestReconcileContentFilter_RollbackSabotageRetainsRecoveryJournal(t *testing.T) {
	cfg := validContentFilterConfig()
	cfg.Canary = true
	cfg.SourceNetwork = "10.100.222.0/24"
	owned := opnsenseFilterGetReadback(desiredContentFilterRules(cfg)[0], "snapshot-owned")
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{owned}},
		sourceScopedSupported: true,
		failOnce:              map[string]error{"inspect-dnsbl": errors.New("injected runtime failure")},
		armRollbackSabotage:   true,
	}
	_, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg)
	if err == nil || !strings.Contains(err.Error(), "rollback incomplete") ||
		!strings.Contains(err.Error(), "count") {
		t.Fatalf("rollback sabotage was not surfaced exactly: %v", err)
	}
	if opn.transaction == nil {
		t.Fatal("incomplete rollback discarded its durable recovery journal")
	}
	if !reflect.DeepEqual(opn.canaryReservations, []string{cfg.SourceNetwork}) {
		t.Fatalf("incomplete rollback released the active canary reservation: %v", opn.canaryReservations)
	}
}

func TestRequireContentFilterFirewallMutationSafe(t *testing.T) {
	opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
	if err := requireContentFilterFirewallMutationSafe(context.Background(), opn, opn); err != nil {
		t.Fatal(err)
	}
	opn.transaction = &database.ContentFilterTransaction{OperationID: uuid.New()}
	if err := requireContentFilterFirewallMutationSafe(context.Background(), opn, opn); err != nil {
		t.Fatalf("journal with proven-absent owned firewall state blocked unrelated mutation: %v", err)
	}
	owned := opnsenseFilterGetReadback(desiredContentFilterRules(validContentFilterConfig())[0], "owned")
	opn.firewallRules = []opnsense.FirewallRuleInfo{owned}
	if err := requireContentFilterFirewallMutationSafe(context.Background(), opn, opn); err == nil ||
		!strings.Contains(err.Error(), opn.transaction.OperationID.String()) {
		t.Fatalf("pending transaction with owned firewall state did not block mutation: %v", err)
	}
}

func TestReconcileContentFilter_RepairsDriftAndDuplicatesInPlace(t *testing.T) {
	cfg := validContentFilterConfig()
	var rules []opnsense.FirewallRuleInfo
	for i, desired := range desiredContentFilterRules(cfg) {
		current := opnsenseFilterGetReadback(desired, fmt.Sprintf("rule-%d", i))
		if i == 0 {
			current.Quick = "0"
		}
		rules = append(rules, current)
	}
	duplicate := rules[1]
	duplicate.UUID = "duplicate"
	rules = append(rules, duplicate)
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns-owned"
	dns.CacheTTL = "72000"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: rules},
		sourceScopedSupported: true,
		dnsPolicies:           []opnsense.DNSBLPolicy{dns},
	}

	result, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.DriftedRules != 1 || result.RemovedRules != 1 || len(opn.updateFirewallCalls) != 1 {
		t.Fatalf("firewall drift/duplicate repair mismatch: result=%+v updates=%v deletes=%v",
			result, opn.updateFirewallCalls, opn.deleteFirewallCalls)
	}
	if len(opn.updateDNSBLCalls) != 1 || opn.updateDNSBLCalls[0].UUID != "dns-owned" {
		t.Fatalf("DNSBL drift was not updated in place: %+v", opn.updateDNSBLCalls)
	}
}

func TestReconcileContentFilter_AmbiguousOwnedObjectsAndIPv6FailBeforeMutation(t *testing.T) {
	cfg := validContentFilterConfig()
	ambiguous := opnsenseFilterGetReadback(desiredContentFilterRules(cfg)[0], "sabotaged")
	ambiguous.Interface = "opt7"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{ambiguous}},
		sourceScopedSupported: true,
	}
	if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("sabotaged owned object was accepted: %v", err)
	}
	assertNoContentFilterMutation(t, opn)

	for field, value := range map[string]string{
		"gateway":   "WAN_GW",
		"replyto":   "LAN_GW",
		"sched":     "class-hours",
		"statetype": "sloppy",
		"unknown":   "",
	} {
		sabotaged := opnsenseFilterGetReadback(desiredContentFilterRules(cfg)[0], "sabotaged-"+field)
		sabotaged.Advanced = map[string]string{field: value}
		opn = &fakeContentFilterOPN{
			fakeNetworkOPN:        &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{sabotaged}},
			sourceScopedSupported: true,
		}
		if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg); err == nil ||
			!strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("behavior-changing field %s=%q was accepted: %v", field, value, err)
		}
		assertNoContentFilterMutation(t, opn)
	}

	opn = &fakeContentFilterOPN{
		fakeNetworkOPN:        &fakeNetworkOPN{},
		sourceScopedSupported: true,
		failOnce:              map[string]error{"ipv6": errors.New("globally routed IPv6")},
	}
	if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, []string{"opt7"}, cfg); err == nil ||
		!strings.Contains(err.Error(), "IPv6") {
		t.Fatalf("student IPv6 Internet route was accepted: %v", err)
	}
	assertNoContentFilterMutation(t, opn)
}

func TestInspectContentFilterPolicy_RejectsMissingSixListOrWrongSource(t *testing.T) {
	cfg := validContentFilterConfig()
	var rules []opnsense.FirewallRuleInfo
	for i, rule := range desiredContentFilterRules(cfg) {
		rules = append(rules, opnsenseFilterGetReadback(rule, fmt.Sprintf("rule-%d", i)))
	}
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns"
	dns.Lists = strings.Join(strings.Split(dns.Lists, ",")[:5], ",")
	if err := InspectContentFilterPolicy(rules, []opnsense.DNSBLPolicy{dns}, cfg); err == nil {
		t.Fatal("five-list DNSBL policy was accepted")
	}
	dns = desiredDNSBLPolicy(cfg)
	dns.UUID = "dns"
	dns.SourceNets = "10.100.1.0/24"
	if err := InspectContentFilterPolicy(rules, []opnsense.DNSBLPolicy{dns}, cfg); err == nil {
		t.Fatal("wrong DNSBL source scope was accepted")
	}
}

func TestReconcileContentFilter_InvalidFeedFailsBeforeAnyMutation(t *testing.T) {
	for _, value := range []string{
		"",
		"http://student-filter-feed.lab.jmal.io",
		"https://example.com",
		"https://student-filter-feed.lab.jmal.io/lists/drogue.txt",
		"https://student-filter-feed.lab.jmal.io?list=drogue",
		"https://user@student-filter-feed.lab.jmal.io",
	} {
		opn := &fakeContentFilterOPN{fakeNetworkOPN: &fakeNetworkOPN{}}
		cfg := validContentFilterConfig()
		cfg.CategoryFeedBaseURL = value
		if _, err := reconcileContentFilter(context.Background(), opn, opn, nil, nil, cfg); err == nil {
			t.Fatalf("invalid feed base URL %q was accepted", value)
		}
		assertNoContentFilterMutation(t, opn)
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
		opn.activateDNSBLCalls != 0 ||
		opn.configureSafeCalls != 0 ||
		opn.restoreSafeCalls != 0 {
		t.Fatalf("content-filter path mutated policy: createFW=%v updateFW=%v deleteFW=%v apply=%d createDNS=%v updateDNS=%v deleteDNS=%v activate=%d safe=%d/%d",
			opn.createFirewallCalls, opn.updateFirewallCalls, opn.deleteFirewallCalls,
			opn.applyFirewallCalls, opn.createDNSBLCalls, opn.updateDNSBLCalls,
			opn.deleteDNSBLCalls, opn.activateDNSBLCalls, opn.configureSafeCalls, opn.restoreSafeCalls)
	}
}

func canonicalFirewallState(rules []opnsense.FirewallRuleInfo) []string {
	var out []string
	for _, rule := range rules {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s",
			rule.Description, rule.Enabled, rule.Sequence, rule.Quick, rule.Action, rule.Interface,
			rule.Direction, rule.IPProtocol, rule.Protocol, rule.Source, rule.SourcePort,
			rule.Destination, rule.DestinationPort, rule.DestinationInvert, rule.Log))
	}
	sort.Strings(out)
	return out
}

func canonicalDNSState(policies []opnsense.DNSBLPolicy) []string {
	var out []string
	for _, policy := range policies {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|%s",
			policy.Description, policy.Enabled, canonicalCSV(policy.Types), canonicalCSV(policy.Lists),
			canonicalCSV(policy.Allowlists), canonicalCSV(policy.Wildcards), canonicalCSV(policy.SourceNets),
			policy.NXDomain, policy.CacheTTL))
	}
	sort.Strings(out)
	return out
}

func cloneSafeSearchState(state opnsense.SourceScopedSafeSearchState) opnsense.SourceScopedSafeSearchState {
	return opnsense.SourceScopedSafeSearchState{
		Content: append([]byte(nil), state.Content...),
		Exists:  state.Exists,
	}
}

func ownedDNSPolicies(policies []opnsense.DNSBLPolicy) []opnsense.DNSBLPolicy {
	var out []opnsense.DNSBLPolicy
	for _, policy := range policies {
		if strings.HasPrefix(policy.Description, contentFilterRulePrefix) {
			out = append(out, policy)
		}
	}
	return out
}

func strconvAtoiForTest(value string) (int, error) {
	var result int
	_, err := fmt.Sscanf(value, "%d", &result)
	return result, err
}
