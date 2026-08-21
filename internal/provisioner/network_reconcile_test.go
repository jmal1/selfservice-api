package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/opnsense"
)

type fakeNetworkOPN struct {
	vlans                  map[int]*opnsense.VLAN
	getVLANErr             map[int]error
	createVLANErr          map[int]error
	createVLANCalls        []int
	createVLANParents      []string
	reconfigureVLANCalls   int
	reconfigureVLANErr     error
	dhcpSubnets            map[string]*opnsense.DHCPSubnet
	getDHCPSubnetErr       map[string]error
	createDHCPSubnetErr    map[string]error
	createDHCPSubnetCalls  []string
	selectedDHCPInterfaces []string
	getDHCPInterfacesErr   error
	addDHCPInterfaceErr    map[string]error
	addDHCPInterfaceCalls  []string
	restartDHCPErr         error
	restartDHPCCalls       int
	firewallRules          []opnsense.FirewallRuleInfo
	getFirewallErr         error
	createFirewallErr      error
	createFirewallCalls    []opnsense.FirewallRule
	deleteFirewallErr      map[string]error
	deleteFirewallCalls    []string
	updateFirewallCalls    []opnsense.FirewallRule
	applyFirewallErr       error
	applyFirewallCalls     int
}

func (f *fakeNetworkOPN) GetVLANByTag(_ context.Context, tag int) (*opnsense.VLAN, error) {
	if err := f.getVLANErr[tag]; err != nil {
		return nil, err
	}
	return f.vlans[tag], nil
}

func (f *fakeNetworkOPN) CreateVLAN(_ context.Context, parentIf string, tag int, _ string) (string, error) {
	if err := f.createVLANErr[tag]; err != nil {
		return "", err
	}
	f.createVLANCalls = append(f.createVLANCalls, tag)
	f.createVLANParents = append(f.createVLANParents, parentIf)
	if f.vlans == nil {
		f.vlans = map[int]*opnsense.VLAN{}
	}
	f.vlans[tag] = &opnsense.VLAN{Tag: "x"}
	return "vlan-uuid", nil
}

func (f *fakeNetworkOPN) ReconfigureVLANs(_ context.Context) error {
	f.reconfigureVLANCalls++
	return f.reconfigureVLANErr
}

func (f *fakeNetworkOPN) GetDHCPSubnetByNetwork(_ context.Context, subnet string) (*opnsense.DHCPSubnet, error) {
	if err := f.getDHCPSubnetErr[subnet]; err != nil {
		return nil, err
	}
	return f.dhcpSubnets[subnet], nil
}

func (f *fakeNetworkOPN) CreateDHCPSubnet(_ context.Context, subnet, _ string, _ string) (string, error) {
	if err := f.createDHCPSubnetErr[subnet]; err != nil {
		return "", err
	}
	f.createDHCPSubnetCalls = append(f.createDHCPSubnetCalls, subnet)
	if f.dhcpSubnets == nil {
		f.dhcpSubnets = map[string]*opnsense.DHCPSubnet{}
	}
	f.dhcpSubnets[subnet] = &opnsense.DHCPSubnet{Subnet: subnet}
	return "dhcp-uuid", nil
}

func (f *fakeNetworkOPN) GetDHCPInterfaces(_ context.Context) ([]string, error) {
	if f.getDHCPInterfacesErr != nil {
		return nil, f.getDHCPInterfacesErr
	}
	out := make([]string, len(f.selectedDHCPInterfaces))
	copy(out, f.selectedDHCPInterfaces)
	return out, nil
}

func (f *fakeNetworkOPN) AddDHCPInterface(_ context.Context, ifName string) error {
	if err := f.addDHCPInterfaceErr[ifName]; err != nil {
		return err
	}
	f.addDHCPInterfaceCalls = append(f.addDHCPInterfaceCalls, ifName)
	if !containsString(f.selectedDHCPInterfaces, ifName) {
		f.selectedDHCPInterfaces = append(f.selectedDHCPInterfaces, ifName)
	}
	return nil
}

func (f *fakeNetworkOPN) RestartDHCP(_ context.Context) error {
	f.restartDHPCCalls++
	return f.restartDHCPErr
}

func (f *fakeNetworkOPN) GetFirewallRules(_ context.Context) ([]opnsense.FirewallRuleInfo, error) {
	if f.getFirewallErr != nil {
		return nil, f.getFirewallErr
	}
	out := make([]opnsense.FirewallRuleInfo, len(f.firewallRules))
	copy(out, f.firewallRules)
	return out, nil
}

func (f *fakeNetworkOPN) CreateFirewallRule(_ context.Context, rule opnsense.FirewallRule) (string, error) {
	if f.createFirewallErr != nil {
		return "", f.createFirewallErr
	}
	f.createFirewallCalls = append(f.createFirewallCalls, rule)
	// Model the canonical shape opnsense.GetFirewallRules yields from a
	// firewall/filter/get read-back of a rule created via addRule (lower-cased,
	// sorted interface set, empty ports, description dropped). A verbatim
	// round-trip of the create struct would hide the real bug.
	f.firewallRules = append(f.firewallRules, opnsenseFilterGetReadback(rule, "fw-uuid"))
	return "fw-uuid", nil
}

func (f *fakeNetworkOPN) DeleteFirewallRule(_ context.Context, ruleUUID string) error {
	if err := f.deleteFirewallErr[ruleUUID]; err != nil {
		return err
	}

	f.deleteFirewallCalls = append(f.deleteFirewallCalls, ruleUUID)
	for i, rule := range f.firewallRules {
		if rule.UUID == ruleUUID {
			f.firewallRules = append(f.firewallRules[:i], f.firewallRules[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeNetworkOPN) UpdateFirewallRule(_ context.Context, ruleUUID string, replacement opnsense.FirewallRule) error {
	f.updateFirewallCalls = append(f.updateFirewallCalls, replacement)
	for i, rule := range f.firewallRules {
		if rule.UUID == ruleUUID {
			f.firewallRules[i] = opnsenseFilterGetReadback(replacement, ruleUUID)
			return nil
		}
	}
	return errors.New("rule not found")
}

func (f *fakeNetworkOPN) GetUnboundSafeSearch(context.Context) (bool, error) {
	return true, nil
}

func (f *fakeNetworkOPN) SetUnboundSafeSearch(context.Context, bool) error { return nil }

func (f *fakeNetworkOPN) ListDNSBLPolicies(context.Context) ([]opnsense.DNSBLPolicy, error) {
	return nil, nil
}

func (f *fakeNetworkOPN) GetDNSBLPolicy(context.Context, string) (opnsense.DNSBLPolicy, error) {
	return opnsense.DNSBLPolicy{}, nil
}

func (f *fakeNetworkOPN) CreateDNSBLPolicy(context.Context, opnsense.DNSBLPolicy) (string, error) {
	return "dnsbl", nil
}

func (f *fakeNetworkOPN) UpdateDNSBLPolicy(context.Context, string, opnsense.DNSBLPolicy) error {
	return nil
}

func (f *fakeNetworkOPN) DeleteDNSBLPolicy(context.Context, string) error { return nil }

func (f *fakeNetworkOPN) ReconfigureUnbound(context.Context) error { return nil }

func (f *fakeNetworkOPN) RefreshUnboundDNSBL(context.Context) error { return nil }

// opnsenseFilterGetReadback models the canonical FirewallRuleInfo that
// opnsense.GetFirewallRules produces for a rule created via addRule, after
// parsing firewall/filter/get and canonicalizing its option-map/plain fields.
func opnsenseFilterGetReadback(rule opnsense.FirewallRule, uuid string) opnsense.FirewallRuleInfo {
	return opnsense.FirewallRuleInfo{
		UUID:              uuid,
		Enabled:           canonicalField(rule.Enabled),
		Sequence:          canonicalField(rule.Sequence),
		Quick:             canonicalField(rule.Quick),
		Interface:         canonicalInterfaceList(rule.Interface),
		InterfaceInvert:   canonicalFirewallBoolean(rule.InterfaceInvert),
		Direction:         canonicalField(rule.Direction),
		IPProtocol:        canonicalField(rule.IPProtocol),
		Protocol:          canonicalField(rule.Protocol),
		SourceInvert:      canonicalFirewallBoolean(rule.SourceInvert),
		Source:            canonicalField(rule.Source),
		SourcePort:        canonicalField(rule.SourcePort),
		DestinationInvert: canonicalFirewallBoolean(rule.DestinationInvert),
		Destination:       canonicalField(rule.Destination),
		DestinationPort:   canonicalField(rule.DestinationPort),
		Action:            canonicalField(rule.Action),
		Log:               canonicalField(rule.Log),
		Description:       rule.Description,
	}
}

func (f *fakeNetworkOPN) ApplyFirewall(_ context.Context) error {
	f.applyFirewallCalls++
	return f.applyFirewallErr
}

func (*fakeNetworkOPN) SupportsSourceScopedSafeSearch(context.Context) (bool, error) {
	return false, nil
}

func (*fakeNetworkOPN) VerifySourceScopedContentFilter(context.Context, string) error {
	return errors.New("effective student-source verification unavailable")
}

type fakeNetworkSSH struct {
	findByVLAN   map[int]string
	findErr      map[int]error
	assignByVLAN map[int]string
	assignErr    map[int]error
	assignCalls  []int
}

func (f *fakeNetworkSSH) FindInterfaceByVLAN(_ context.Context, vlanTag int) (string, error) {
	if err := f.findErr[vlanTag]; err != nil {
		return "", err
	}
	return f.findByVLAN[vlanTag], nil
}

func (f *fakeNetworkSSH) AssignInterface(_ context.Context, vlanTag int, _ string) (string, error) {
	if err := f.assignErr[vlanTag]; err != nil {
		return "", err
	}
	f.assignCalls = append(f.assignCalls, vlanTag)
	ifName := f.assignByVLAN[vlanTag]
	f.findByVLAN[vlanTag] = ifName
	return ifName, nil
}

type fakeNetworkDB struct {
	rows       []database.AllocatedVLAN
	listErr    error
	releaseErr map[uuid.UUID]error
	released   []uuid.UUID
}

func (f *fakeNetworkDB) ListAllocatedVLANs(_ context.Context) ([]database.AllocatedVLAN, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeNetworkDB) ReleaseVLAN(_ context.Context, podID uuid.UUID) error {
	if err := f.releaseErr[podID]; err != nil {
		return err
	}
	f.released = append(f.released, podID)
	return nil
}

// podPassRuleReadback builds the OPNsense-search-formatted representation of the
// per-VLAN pass rule the reconciler creates for a given interface + subnet.
// Used to pre-seed "healthy" fixtures the way the live firewall would report
// them (re-cased fields, no description).
func podPassRuleReadback(ifName, subnet string) opnsense.FirewallRuleInfo {
	rule := opnsenseFilterGetReadback(opnsense.FirewallRule{
		Enabled:     "1",
		Quick:       "1",
		Action:      "pass",
		Interface:   ifName,
		Direction:   "in",
		IPProtocol:  "inet",
		Protocol:    "any",
		Source:      subnet,
		Destination: "any",
	}, "fw-existing")
	rule.Description = ""
	return rule
}

func TestReconcileNetwork_RepairsMissingInterfaceSubnetAndBinding(t *testing.T) {
	podID := uuid.New()
	row := allocatedVLANRow(103, podID, "active")

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{103: {Tag: "103"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{},
		selectedDHCPInterfaces: nil,
	}
	ssh := &fakeNetworkSSH{
		findByVLAN:   map[int]string{103: ""},
		assignByVLAN: map[int]string{103: "opt4"},
	}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if counts.InterfacesRepaired != 1 || counts.SubnetsRepaired != 1 || counts.KeaBindingsRepaired != 1 {
		t.Fatalf("expected all repair counters to increment once, got %+v", counts)
	}
	if counts.KeaRestarted != 1 || opn.restartDHPCCalls != 1 {
		t.Fatalf("expected one Kea restart, got counts=%+v calls=%d", counts, opn.restartDHPCCalls)
	}
	if len(opn.addDHCPInterfaceCalls) != 1 || opn.addDHCPInterfaceCalls[0] != "opt4" {
		t.Fatalf("expected AddDHCPInterface(opt4), got %+v", opn.addDHCPInterfaceCalls)
	}
	if counts.FirewallRulesRepaired != 1 || counts.FirewallApplied != 1 || opn.applyFirewallCalls != 1 {
		t.Fatalf("expected one firewall rule created + one apply, got counts=%+v applyCalls=%d", counts, opn.applyFirewallCalls)
	}
	if len(opn.createFirewallCalls) != 1 || opn.createFirewallCalls[0].Interface != "opt4" || opn.createFirewallCalls[0].Source != row.Subnet {
		t.Fatalf("expected pass rule on opt4 for %s, got %+v", row.Subnet, opn.createFirewallCalls)
	}
}

func TestReconcileNetwork_HealthyActivePodNoChanges(t *testing.T) {
	podID := uuid.New()
	row := allocatedVLANRow(104, podID, "active")

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{104: {Tag: "104"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
		selectedDHCPInterfaces: []string{"opt7"},
		firewallRules:          []opnsense.FirewallRuleInfo{podPassRuleReadback("opt7", row.Subnet)},
	}
	ssh := &fakeNetworkSSH{
		findByVLAN: map[int]string{104: "opt7"},
	}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if counts.InterfacesRepaired != 0 || counts.SubnetsRepaired != 0 || counts.KeaBindingsRepaired != 0 || counts.KeaRestarted != 0 {
		t.Fatalf("expected no repairs/restart, got %+v", counts)
	}
	if counts.FirewallRulesRepaired != 0 || counts.FirewallApplied != 1 || opn.applyFirewallCalls != 1 {
		t.Fatalf("expected no model changes and one convergence apply, got counts=%+v applyCalls=%d", counts, opn.applyFirewallCalls)
	}
	if opn.restartDHPCCalls != 0 {
		t.Fatalf("expected no Kea restart call, got %d", opn.restartDHPCCalls)
	}
}

func TestReconcileNetwork_ContentFilterWaitsForGeneratedCleanup(t *testing.T) {
	row := allocatedVLANRow(104, uuid.New(), "active")
	first := podPassRuleReadback("opt7", row.Subnet)
	first.UUID = "legacy-1"
	second := first
	second.UUID = "legacy-2"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN: &fakeNetworkOPN{
			vlans:                  map[int]*opnsense.VLAN{104: {Tag: "104"}},
			dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
			selectedDHCPInterfaces: []string{"opt7"},
			firewallRules:          []opnsense.FirewallRuleInfo{first, second},
		},
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{104: "opt7"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{
		ContentFilter: validContentFilterConfig(),
	}, func() time.Time { return time.Unix(123, 0) })
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.FirewallRulesDuplicate != 1 || counts.FirewallRulesRemoved != 1 ||
		counts.ContentFilterExpected != 1 || counts.ContentFilterHealthy != 0 {
		t.Fatalf("content filter did not wait for cleanup convergence: %+v", counts)
	}
	if len(opn.createDNSBLCalls) != 0 || opn.verifyRuntimeCalls != 0 {
		t.Fatalf("partial content policy mutated before generated cleanup converged: dns=%v verify=%d",
			opn.createDNSBLCalls, opn.verifyRuntimeCalls)
	}
	if opn.applyFirewallCalls != 0 {
		t.Fatalf("partial content policy was activated by an unrelated firewall apply: %d", opn.applyFirewallCalls)
	}
}

func TestReconcileNetwork_ContentFilterInspectionFailureBlocksUnrelatedApply(t *testing.T) {
	row := allocatedVLANRow(104, uuid.New(), "active")
	pass := podPassRuleReadback("opt7", row.Subnet)
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN: &fakeNetworkOPN{
			vlans:                  map[int]*opnsense.VLAN{104: {Tag: "104"}},
			dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
			selectedDHCPInterfaces: []string{"opt7"},
			firewallRules:          []opnsense.FirewallRuleInfo{pass},
		},
		sourceScopedSupported: true,
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{104: "opt7"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{
		ContentFilter: validContentFilterConfig(),
	}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.ContentFilterHealthy != 0 || counts.Errors != 1 {
		t.Fatalf("failed inspection reported a healthy policy: %+v", counts)
	}
	if opn.applyFirewallCalls != 0 {
		t.Fatalf("failed inspection activated staged firewall model: applies=%d", opn.applyFirewallCalls)
	}
	assertNoContentFilterMutation(t, opn)
}

func TestReconcileNetwork_FirewallApplyFailureKeepsContentFilterUnhealthy(t *testing.T) {
	row := allocatedVLANRow(104, uuid.New(), "active")
	cfg := validContentFilterConfig()
	pass := podPassRuleReadback("opt7", row.Subnet)
	pass.UUID = "legacy"
	firewallRules := []opnsense.FirewallRuleInfo{pass}
	for i, rule := range desiredContentFilterRules(cfg) {
		firewallRules = append(firewallRules, opnsenseFilterGetReadback(rule, fmt.Sprintf("policy-%d", i)))
	}
	dns := desiredDNSBLPolicy(cfg)
	dns.UUID = "dns"
	opn := &fakeContentFilterOPN{
		fakeNetworkOPN: &fakeNetworkOPN{
			vlans:                  map[int]*opnsense.VLAN{104: {Tag: "104"}},
			dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
			selectedDHCPInterfaces: []string{"opt7"},
			firewallRules:          firewallRules,
			applyFirewallErr:       errors.New("transient apply failure"),
		},
		sourceScopedSupported: true,
		dnsPolicies:           []opnsense.DNSBLPolicy{dns},
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{104: "opt7"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{
		ContentFilter: cfg,
	}, func() time.Time { return time.Unix(123, 0) })
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.ContentFilterExpected != 1 || counts.ContentFilterHealthy != 0 ||
		counts.ContentFilterSuccessAt != 0 || counts.Errors != 1 {
		t.Fatalf("failed apply reported a false-green policy: %+v", counts)
	}
}

func TestReconcileNetwork_ReleasesTerminalPodAllocations(t *testing.T) {
	podID := uuid.New()
	row := allocatedVLANRow(105, podID, "destroyed")

	opn := &fakeNetworkOPN{}
	ssh := &fakeNetworkSSH{}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if counts.VLANsReleased != 1 {
		t.Fatalf("expected VLAN release count 1, got %+v", counts)
	}
	if len(db.released) != 1 || db.released[0] != podID {
		t.Fatalf("expected ReleaseVLAN called with podID %s, got %+v", podID, db.released)
	}
}

func TestReconcileNetwork_RetainsDestroyFailedAllocationForRetry(t *testing.T) {
	for _, status := range []string{models.PodStatusDestroying, models.PodStatusDestroyFailed} {
		t.Run(status, func(t *testing.T) {
			podID := uuid.New()
			row := allocatedVLANRow(105, podID, status)
			opn := &fakeNetworkOPN{}
			db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

			counts, err := reconcileNetwork(context.Background(), opn, &fakeNetworkSSH{}, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
			if err != nil {
				t.Fatalf("reconcileNetwork: %v", err)
			}
			if counts.VLANsReleased != 0 || counts.ActivePodVLANs != 0 || len(db.released) != 0 {
				t.Fatalf("%s allocation must remain reserved for destroy: counts=%+v released=%v", status, counts, db.released)
			}
			if len(opn.createVLANCalls) != 0 || len(opn.createFirewallCalls) != 0 {
				t.Fatalf("%s allocation was incorrectly reconciled as active: vlan=%v firewall=%v",
					status, opn.createVLANCalls, opn.createFirewallCalls)
			}
		})
	}
}

func TestReconcileNetwork_MixedSet(t *testing.T) {
	activeHealthy := allocatedVLANRow(106, uuid.New(), "active")
	activeNeedsBinding := allocatedVLANRow(107, uuid.New(), "configuring")
	terminal := allocatedVLANRow(108, uuid.New(), "destroyed")

	opn := &fakeNetworkOPN{
		vlans: map[int]*opnsense.VLAN{
			106: {Tag: "106"},
			107: {Tag: "107"},
		},
		dhcpSubnets: map[string]*opnsense.DHCPSubnet{
			activeHealthy.Subnet:      {Subnet: activeHealthy.Subnet},
			activeNeedsBinding.Subnet: {Subnet: activeNeedsBinding.Subnet},
		},
		selectedDHCPInterfaces: []string{"opt9"},
		firewallRules:          []opnsense.FirewallRuleInfo{podPassRuleReadback("opt9", activeHealthy.Subnet)},
	}
	ssh := &fakeNetworkSSH{
		findByVLAN: map[int]string{
			106: "opt9",
			107: "opt10",
		},
	}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{activeHealthy, activeNeedsBinding, terminal}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if counts.AllocatedVLANs != 3 || counts.ActivePodVLANs != 2 || counts.VLANsReleased != 1 {
		t.Fatalf("unexpected allocation counts: %+v", counts)
	}
	if counts.KeaBindingsRepaired != 1 || counts.KeaRestarted != 1 {
		t.Fatalf("expected one binding repair + one restart, got %+v", counts)
	}
	if counts.FirewallRulesRepaired != 1 || counts.FirewallApplied != 1 {
		t.Fatalf("expected one firewall rule repair (for opt10) + apply, got %+v", counts)
	}
}

func TestReconcileNetwork_IsIdempotent_NoDuplicatePassRules(t *testing.T) {
	// Regression test for the 2026-08-02 OPNsense config bloat incident: running
	// the reconciler repeatedly must NOT re-create a content-equivalent per-VLAN
	// pass rule. The fake models OPNsense's lossy/formatted search read-back
	// (re-cased fields, dropped description), which is what defeated the old
	// exact-string dedup and caused unbounded duplicates.
	podID := uuid.New()
	row := allocatedVLANRow(120, podID, "active")

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{120: {Tag: "120"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
		selectedDHCPInterfaces: []string{"opt15"},
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{120: "opt15"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	const reconcilePasses = 3
	for i := 0; i < reconcilePasses; i++ {
		if _, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now); err != nil {
			t.Fatalf("reconcileNetwork pass %d: %v", i, err)
		}
	}

	if len(opn.createFirewallCalls) != 1 {
		t.Fatalf("expected exactly ONE firewall rule created across %d reconcile passes, got %d: %+v",
			reconcilePasses, len(opn.createFirewallCalls), opn.createFirewallCalls)
	}
	if len(opn.firewallRules) != 1 {
		t.Fatalf("expected exactly one stored firewall rule (no duplicates), got %d: %+v",
			len(opn.firewallRules), opn.firewallRules)
	}
}

func TestReconcileNetwork_CompactsLegacyDuplicatesAndStaleRulesWithoutTouchingManualRules(t *testing.T) {
	active := allocatedVLANRow(115, uuid.New(), "active")
	desired := podPassRuleReadback("opt6", active.Subnet)
	desired.UUID = "legacy-keep"
	duplicate1 := desired
	duplicate1.UUID = "legacy-z"
	duplicate2 := desired
	duplicate2.UUID = "legacy-y"
	manual := desired
	manual.UUID = "manual"
	manual.Description = "Instructor emergency pass"
	manual.Interface = "lan,opt1"
	manual.Source = "10.10.10.0/24"
	manual.Destination = "10.100.0.0/16"
	stale := podPassRuleReadback("opt9", "10.100.99.0/24")
	stale.UUID = "stale"

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{115: {Tag: "115"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{active.Subnet: {Subnet: active.Subnet}},
		selectedDHCPInterfaces: []string{"opt6"},
		firewallRules:          []opnsense.FirewallRuleInfo{manual, duplicate1, stale, duplicate2, desired},
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{115: "opt6"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{active}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.FirewallRulesDuplicate != 2 || counts.FirewallRulesStale != 1 || counts.FirewallRulesRemoved != 3 {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
	if len(opn.createFirewallCalls) != 0 {
		t.Fatalf("existing content-equivalent rules must prevent creation: %+v", opn.createFirewallCalls)
	}
	if !containsString(ruleUUIDs(opn.firewallRules), "manual") {
		t.Fatalf("named/manual rule was removed: %+v", opn.firewallRules)
	}
	if got := countRulesWithSignature(opn.firewallRules, desired); got != 1 {
		t.Fatalf("want one generated desired rule, got %d: %+v", got, opn.firewallRules)
	}
	if containsString(ruleUUIDs(opn.firewallRules), "stale") {
		t.Fatalf("stale generated rule was retained: %+v", opn.firewallRules)
	}
	if opn.applyFirewallCalls != 1 {
		t.Fatalf("cleanup must apply exactly once, got %d", opn.applyFirewallCalls)
	}
}

func TestReconcileNetwork_ManualEquivalentReplacesAllGeneratedDuplicates(t *testing.T) {
	active := allocatedVLANRow(115, uuid.New(), "active")
	manual := podPassRuleReadback("opt6", active.Subnet)
	manual.UUID = "manual"
	manual.Description = "Instructor-owned pass"
	legacy1 := podPassRuleReadback("opt6", active.Subnet)
	legacy1.UUID = "legacy-1"
	legacy2 := legacy1
	legacy2.UUID = "legacy-2"

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{115: {Tag: "115"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{active.Subnet: {Subnet: active.Subnet}},
		selectedDHCPInterfaces: []string{"opt6"},
		firewallRules:          []opnsense.FirewallRuleInfo{legacy1, manual, legacy2},
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{115: "opt6"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{active}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.FirewallRulesDuplicate != 2 || counts.FirewallRulesRemoved != 2 {
		t.Fatalf("unexpected cleanup counts: %+v", counts)
	}
	if got := ruleUUIDs(opn.firewallRules); len(got) != 1 || got[0] != "manual" {
		t.Fatalf("manual equivalent must be the only survivor, got %v", got)
	}
}

func TestReconcileNetwork_BoundsCleanupAndReportsRemainingGrowth(t *testing.T) {
	active := allocatedVLANRow(115, uuid.New(), "active")
	var rules []opnsense.FirewallRuleInfo
	for i := 0; i < 6; i++ {
		rule := podPassRuleReadback("opt6", active.Subnet)
		rule.UUID = fmt.Sprintf("legacy-%d", i)
		rules = append(rules, rule)
	}
	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{115: {Tag: "115"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{active.Subnet: {Subnet: active.Subnet}},
		selectedDHCPInterfaces: []string{"opt6"},
		firewallRules:          rules,
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{115: "opt6"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{active}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{
		FirewallCleanupLimit: 2,
	}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.FirewallRulesDuplicate != 5 || counts.FirewallRulesRemoved != 2 || counts.FirewallCleanupLimited != 1 {
		t.Fatalf("bounded cleanup was not reported: %+v", counts)
	}
	if len(opn.firewallRules) != 4 {
		t.Fatalf("want four rules after deleting two, got %d", len(opn.firewallRules))
	}
}

func TestReconcileNetwork_AmbiguousLegacyRuleFailsClosed(t *testing.T) {
	active := allocatedVLANRow(115, uuid.New(), "active")
	ambiguous := podPassRuleReadback("opt6", active.Subnet)
	ambiguous.UUID = "ambiguous"
	ambiguous.DestinationPort = "443"

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{115: {Tag: "115"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{active.Subnet: {Subnet: active.Subnet}},
		selectedDHCPInterfaces: []string{"opt6"},
		firewallRules:          []opnsense.FirewallRuleInfo{ambiguous},
	}

	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{115: "opt6"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{active}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.Errors != 1 {
		t.Fatalf("ambiguous inventory must be reported as an error: %+v", counts)
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.deleteFirewallCalls) != 0 || opn.applyFirewallCalls != 0 {
		t.Fatalf("ambiguous inventory must cause no firewall mutation: create=%v delete=%v apply=%d",
			opn.createFirewallCalls, opn.deleteFirewallCalls, opn.applyFirewallCalls)
	}
}

func TestReconcileNetwork_OverLimitInventoryFailsClosed(t *testing.T) {
	active := allocatedVLANRow(115, uuid.New(), "active")
	var rules []opnsense.FirewallRuleInfo
	for i := 0; i < 4; i++ {
		rule := podPassRuleReadback("opt6", active.Subnet)
		rule.UUID = fmt.Sprintf("legacy-%d", i)
		rules = append(rules, rule)
	}
	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{115: {Tag: "115"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{active.Subnet: {Subnet: active.Subnet}},
		selectedDHCPInterfaces: []string{"opt6"},
		firewallRules:          rules,
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{115: "opt6"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{active}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{
		MaxFirewallRules: 3,
	}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if counts.Errors != 1 {
		t.Fatalf("over-limit inventory must report error: %+v", counts)
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.deleteFirewallCalls) != 0 || opn.applyFirewallCalls != 0 {
		t.Fatalf("over-limit inventory mutated firewall: create=%v delete=%v apply=%d",
			opn.createFirewallCalls, opn.deleteFirewallCalls, opn.applyFirewallCalls)
	}
}

func TestEnsurePodFirewallRule_IsRetryIdempotent(t *testing.T) {
	opn := &fakeNetworkOPN{}
	for i := 0; i < 3; i++ {
		_, _, err := ensurePodFirewallRule(context.Background(), opn, 115, "opt6", "10.100.15.0/24", 100, 10)
		if err != nil {
			t.Fatalf("ensure pass %d: %v", i, err)
		}
	}
	if len(opn.createFirewallCalls) != 1 || opn.applyFirewallCalls != 3 {
		t.Fatalf("retry created more than once or skipped convergence apply: creates=%d applies=%d",
			len(opn.createFirewallCalls), opn.applyFirewallCalls)
	}
	if got := opn.firewallRules[0].Description; got != "crucible:pod-pass:v1:vlan=115" {
		t.Fatalf("ownership description = %q", got)
	}
}

func TestEnsurePodFirewallRule_RetriesApplyAfterModelMutationSucceeded(t *testing.T) {
	opn := &fakeNetworkOPN{applyFirewallErr: errors.New("transient apply failure")}
	createdUUID, created, err := ensurePodFirewallRule(
		context.Background(), opn, 115, "opt6", "10.100.15.0/24", 100, 10,
	)
	if err == nil || !created || createdUUID == "" {
		t.Fatalf("first ensure must expose created rule and apply error: uuid=%q created=%t err=%v", createdUUID, created, err)
	}

	opn.applyFirewallErr = nil
	createdUUID, created, err = ensurePodFirewallRule(
		context.Background(), opn, 115, "opt6", "10.100.15.0/24", 100, 10,
	)
	if err != nil || created || createdUUID != "" {
		t.Fatalf("retry must apply existing model without another create: uuid=%q created=%t err=%v", createdUUID, created, err)
	}
	if len(opn.createFirewallCalls) != 1 || opn.applyFirewallCalls != 2 {
		t.Fatalf("unexpected retry calls: creates=%d applies=%d", len(opn.createFirewallCalls), opn.applyFirewallCalls)
	}
}

func TestDeletePodFirewallRules_PreservesNamedManualRules(t *testing.T) {
	legacy := podPassRuleReadback("opt6", "10.100.15.0/24")
	legacy.UUID = "legacy"
	owned := legacy
	owned.UUID = "owned"
	owned.Description = "crucible:pod-pass:v1:vlan=115"
	manual := legacy
	manual.UUID = "manual"
	manual.Description = "Keep this emergency pass"
	opn := &fakeNetworkOPN{firewallRules: []opnsense.FirewallRuleInfo{legacy, owned, manual}}

	removed, err := deletePodFirewallRules(context.Background(), opn, "opt6", "10.100.15.0/24", 100, 10)
	if err != nil {
		t.Fatalf("deletePodFirewallRules: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if got := ruleUUIDs(opn.firewallRules); len(got) != 1 || got[0] != "manual" {
		t.Fatalf("manual rule was not preserved: %v", got)
	}
}

func TestDeletePodFirewallRules_RetryAppliesAlreadyDeletedModel(t *testing.T) {
	owned := podPassRuleReadback("opt6", "10.100.15.0/24")
	owned.UUID = "owned"
	owned.Description = "crucible:pod-pass:v1:vlan=115"
	opn := &fakeNetworkOPN{
		firewallRules:    []opnsense.FirewallRuleInfo{owned},
		applyFirewallErr: errors.New("transient apply failure"),
	}

	if removed, err := deletePodFirewallRules(context.Background(), opn, "opt6", "10.100.15.0/24", 100, 10); err == nil || removed != 0 {
		t.Fatalf("first delete must report apply failure: removed=%d err=%v", removed, err)
	}
	opn.applyFirewallErr = nil
	if removed, err := deletePodFirewallRules(context.Background(), opn, "opt6", "10.100.15.0/24", 100, 10); err != nil || removed != 0 {
		t.Fatalf("retry must apply already-deleted model: removed=%d err=%v", removed, err)
	}
	if len(opn.deleteFirewallCalls) != 1 || opn.applyFirewallCalls != 2 {
		t.Fatalf("unexpected retry calls: deletes=%v applies=%d", opn.deleteFirewallCalls, opn.applyFirewallCalls)
	}
}

func TestDeletePodFirewallRules_BoundedRetryConverges(t *testing.T) {
	var rules []opnsense.FirewallRuleInfo
	for i := 0; i < 3; i++ {
		owned := podPassRuleReadback("opt6", "10.100.15.0/24")
		owned.UUID = fmt.Sprintf("owned-%d", i)
		owned.Description = "crucible:pod-pass:v1:vlan=115"
		rules = append(rules, owned)
	}
	opn := &fakeNetworkOPN{firewallRules: rules}

	if removed, err := deletePodFirewallRules(context.Background(), opn, "opt6", "10.100.15.0/24", 100, 2); err == nil || removed != 2 {
		t.Fatalf("first bounded pass: removed=%d err=%v", removed, err)
	}
	if removed, err := deletePodFirewallRules(context.Background(), opn, "opt6", "10.100.15.0/24", 100, 2); err != nil || removed != 1 {
		t.Fatalf("retry pass: removed=%d err=%v", removed, err)
	}
	if len(opn.firewallRules) != 0 || opn.applyFirewallCalls != 2 {
		t.Fatalf("bounded retry did not converge: rules=%v applies=%d", ruleUUIDs(opn.firewallRules), opn.applyFirewallCalls)
	}
}

func TestPodFirewallLifecycle_AgainstLiveFilterGetFalseInversionsQuickAndLog(t *testing.T) {
	const filterGetBody = `{
	  "filter": { "rules": { "rule": {
	    "pod-opt3": {
	      "enabled": "1", "sequence": "1", "quick": "1", "log": "0",
	      "interface":  {"opt3": {"value":"OPT3","selected":1}, "opt5": {"value":"OPT5","selected":0}},
	      "interfacenot": "0",
	      "direction":  {"in": {"value":"in","selected":1}, "out": {"value":"out","selected":0}},
	      "action":     {"pass": {"value":"Pass","selected":1}, "block": {"value":"Block","selected":0}},
	      "ipprotocol": {"inet": {"value":"IPv4","selected":1}, "inet6": {"value":"IPv6","selected":0}},
	      "protocol":   {"any": {"value":"any","selected":1}, "TCP": {"value":"TCP","selected":0}},
	      "source_not": "0", "source_net": "10.100.0.0/24", "source_port": "",
	      "destination_not": "0", "destination_net": "any", "destination_port": "", "description": ""
	    },
	    "mgmt": {
	      "enabled": "1", "sequence": "2",
	      "interface":  {"lan": {"value":"LAN","selected":"1"}, "opt1": {"value":"OPT1","selected":"1"}},
	      "interfacenot": "0",
	      "direction":  {"any": {"value":"any","selected":"1"}},
	      "action":     {"pass": {"value":"Pass","selected":"1"}},
	      "ipprotocol": {"inet": {"value":"IPv4","selected":"1"}},
	      "protocol":   {"any": {"value":"any","selected":"1"}},
	      "source_not": "0", "source_net": "10.10.10.0/24", "source_port": "",
	      "destination_not": "0", "destination_net": "10.100.0.0/16", "destination_port": "",
	      "description": "Allow management VLAN to pod subnets"
	    }
	  }}}
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/firewall/filter/get" {
			t.Errorf("idempotency list must use filter/get, got %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, filterGetBody)
	}))
	defer srv.Close()

	c := opnsense.New(opnsense.Config{BaseURL: srv.URL}, discardLogger())
	rules, err := c.GetFirewallRules(context.Background())
	if err != nil {
		t.Fatalf("GetFirewallRules: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules parsed from filter/get, got %d: %+v", len(rules), rules)
	}

	row := allocatedVLANRow(100, uuid.New(), "active")
	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{100: {Tag: "100"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
		selectedDHCPInterfaces: []string{"opt3"},
		firewallRules:          rules,
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{100: "opt3"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	for i := 0; i < 2; i++ {
		if _, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now); err != nil {
			t.Fatalf("reconcile pass %d: %v", i, err)
		}
		if _, created, err := ensurePodFirewallRule(context.Background(), opn, 100, "opt3", row.Subnet, 100, 10); err != nil || created {
			t.Fatalf("ensure pass %d: created=%t err=%v", i, created, err)
		}
	}
	if len(opn.createFirewallCalls) != 0 || len(opn.deleteFirewallCalls) != 0 {
		t.Fatalf("false-like inversion readback churned healthy rule: creates=%v deletes=%v",
			opn.createFirewallCalls, opn.deleteFirewallCalls)
	}

	removed, err := deletePodFirewallRules(context.Background(), opn, "opt3", row.Subnet, 100, 10)
	if err != nil {
		t.Fatalf("destroy cleanup: %v", err)
	}
	if removed != 1 {
		t.Fatalf("destroy cleanup removed %d rules, want 1", removed)
	}
	if got := ruleUUIDs(opn.firewallRules); len(got) != 1 || got[0] != "mgmt" {
		t.Fatalf("destroy cleanup did not preserve named manual rule: %v", got)
	}
}

func TestPodFirewallQuickAndLogReadbackControlOwnership(t *testing.T) {
	liveRuleBody := func(quick, log, description string) string {
		return fmt.Sprintf(`{
		  "filter": { "rules": { "rule": {
		    "pod-opt3": {
		      "enabled": "1", "sequence": "1", "quick": %q, "log": %q,
		      "interface": {"opt3": {"value":"OPT3","selected":1}},
		      "interfacenot": "0",
		      "direction": {"in": {"value":"in","selected":1}},
		      "action": {"pass": {"value":"Pass","selected":1}},
		      "ipprotocol": {"inet": {"value":"IPv4","selected":1}},
		      "protocol": {"any": {"value":"any","selected":1}},
		      "source_not": "0", "source_net": "10.100.0.0/24", "source_port": "",
		      "destination_not": "0", "destination_net": "any", "destination_port": "",
		      "description": %q
		    }
		  }}}
		}`, quick, log, description)
	}
	parseRules := func(t *testing.T, body string) []opnsense.FirewallRuleInfo {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/firewall/filter/get" {
				http.Error(w, "unexpected path", http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		defer srv.Close()

		c := opnsense.New(opnsense.Config{BaseURL: srv.URL}, discardLogger())
		rules, err := c.GetFirewallRules(context.Background())
		if err != nil {
			t.Fatalf("GetFirewallRules: %v", err)
		}
		if len(rules) != 1 {
			t.Fatalf("parsed rules = %d, want 1: %+v", len(rules), rules)
		}
		return rules
	}

	for _, tt := range []struct {
		name        string
		quick       string
		log         string
		description string
	}{
		{name: "owned quick zero", quick: "0", log: "0", description: "crucible:pod-pass:v1:vlan=100"},
		{name: "owned malformed quick", quick: "2", log: "0", description: "crucible:pod-pass:v1:vlan=100"},
		{name: "blank logged pass", quick: "1", log: "1", description: ""},
	} {
		t.Run(tt.name+" fails closed", func(t *testing.T) {
			rules := parseRules(t, liveRuleBody(tt.quick, tt.log, tt.description))
			opn := &fakeNetworkOPN{firewallRules: rules}
			desired := newDesiredPodFirewallRule(100, "opt3", "10.100.0.0/24")

			if _, _, err := reconcilePodFirewallRules(context.Background(), opn, rules, []desiredPodFirewallRule{desired}, true, 100, 10); err == nil {
				t.Fatal("ambiguous quick/log drift was accepted as a healthy pod pass")
			}
			if len(opn.createFirewallCalls) != 0 || len(opn.deleteFirewallCalls) != 0 {
				t.Fatalf("ambiguous drift must fail closed: creates=%v deletes=%v",
					opn.createFirewallCalls, opn.deleteFirewallCalls)
			}
		})
	}

	t.Run("named manual quick zero logged rule is preserved", func(t *testing.T) {
		rules := parseRules(t, liveRuleBody("0", "1", "Instructor non-quick logged pass"))
		opn := &fakeNetworkOPN{firewallRules: rules}
		desired := newDesiredPodFirewallRule(100, "opt3", "10.100.0.0/24")

		if _, _, err := reconcilePodFirewallRules(context.Background(), opn, rules, []desiredPodFirewallRule{desired}, true, 100, 10); err != nil {
			t.Fatalf("reconcilePodFirewallRules: %v", err)
		}
		if len(opn.deleteFirewallCalls) != 0 {
			t.Fatalf("named manual drifted rule was deleted: %v", opn.deleteFirewallCalls)
		}
		if len(opn.createFirewallCalls) != 1 ||
			opn.createFirewallCalls[0].Quick != "1" ||
			isTruthyFirewallField(opn.createFirewallCalls[0].Log) {
			t.Fatalf("desired quick rule was not created: %+v", opn.createFirewallCalls)
		}
		if !containsString(ruleUUIDs(opn.firewallRules), "pod-opt3") {
			t.Fatalf("named manual rule was not preserved: %+v", opn.firewallRules)
		}
	})
}

func TestReconcileNetwork_ListErrorSkipsFirewallRepair(t *testing.T) {
	// Defensive: if listing rules fails (e.g. filter/get 500 during OOM), the
	// reconciler must skip firewall repair rather than blindly re-create rules.
	podID := uuid.New()
	row := allocatedVLANRow(121, podID, "active")

	opn := &fakeNetworkOPN{
		vlans:                  map[int]*opnsense.VLAN{121: {Tag: "121"}},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
		selectedDHCPInterfaces: []string{"opt16"},
		getFirewallErr:         errors.New("get firewall rules: API error 500: Internal Server Error"),
	}
	ssh := &fakeNetworkSSH{findByVLAN: map[int]string{121: "opt16"}}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if len(opn.createFirewallCalls) != 0 {
		t.Fatalf("expected NO firewall rule creation when the rule listing fails, got %+v", opn.createFirewallCalls)
	}
	if counts.FirewallRulesRepaired != 0 || counts.FirewallApplied != 0 || opn.applyFirewallCalls != 0 {
		t.Fatalf("expected no firewall changes/apply on list failure, got counts=%+v applyCalls=%d", counts, opn.applyFirewallCalls)
	}
	if counts.Errors != 1 {
		t.Fatalf("expected one error recorded for the failed rule listing, got %+v", counts)
	}
}

func TestReconcileNetwork_OneVLANErrorDoesNotAbortOthers(t *testing.T) {
	bad := allocatedVLANRow(109, uuid.New(), "active")
	good := allocatedVLANRow(110, uuid.New(), "active")

	opn := &fakeNetworkOPN{
		getVLANErr: map[int]error{
			109: errors.New("opnsense unavailable"),
		},
		vlans: map[int]*opnsense.VLAN{
			110: {Tag: "110"},
		},
		dhcpSubnets:            map[string]*opnsense.DHCPSubnet{},
		selectedDHCPInterfaces: nil,
	}
	ssh := &fakeNetworkSSH{
		findByVLAN:   map[int]string{110: ""},
		assignByVLAN: map[int]string{110: "opt12"},
	}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{bad, good}}

	counts, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}

	if counts.Errors != 1 {
		t.Fatalf("expected exactly one per-VLAN error, got %+v", counts)
	}
	if counts.InterfacesRepaired != 1 || counts.SubnetsRepaired != 1 || counts.KeaBindingsRepaired != 1 {
		t.Fatalf("expected second VLAN to reconcile successfully, got %+v", counts)
	}
}

func TestReconcileNetwork_UsesDefaultVLANParent(t *testing.T) {
	row := allocatedVLANRow(111, uuid.New(), "active")

	opn := &fakeNetworkOPN{
		vlans:       map[int]*opnsense.VLAN{},
		dhcpSubnets: map[string]*opnsense.DHCPSubnet{row.Subnet: {Subnet: row.Subnet}},
	}
	ssh := &fakeNetworkSSH{
		findByVLAN: map[int]string{111: "opt14"},
	}
	db := &fakeNetworkDB{rows: []database.AllocatedVLAN{row}}

	_, err := reconcileNetwork(context.Background(), opn, ssh, db, discardLogger(), NetworkReconcilerConfig{}, time.Now)
	if err != nil {
		t.Fatalf("reconcileNetwork: %v", err)
	}
	if len(opn.createVLANParents) != 1 || opn.createVLANParents[0] != "vmx1" {
		t.Fatalf("expected default VLAN parent vmx1, got %+v", opn.createVLANParents)
	}
}

func TestNetworkReconcilePusher_Push_SerializesExpectedMetrics(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &NetworkReconcilePusher{
		BaseURL:        srv.URL,
		Job:            "crucible_provision_worker",
		GroupingLabels: map[string]string{"layer": "api"},
		HTTP:           srv.Client(),
	}
	err := p.Push(context.Background(), NetworkReconcileCounts{
		AllocatedVLANs:         4,
		ActivePodVLANs:         3,
		InterfacesRepaired:     1,
		SubnetsRepaired:        2,
		KeaBindingsRepaired:    3,
		FirewallRulesTotal:     16,
		FirewallRulesGenerated: 6,
		FirewallRulesDuplicate: 1,
		FirewallRulesStale:     2,
		FirewallRulesRemoved:   3,
		FirewallCleanupLimited: 1,
		ContentFilterExpected:  1,
		ContentFilterHealthy:   0,
		ContentFilterMissing:   2,
		ContentFilterDrifted:   1,
		ContentFilterRemoved:   3,
		ContentFilterSuccessAt: 1234567890,
		VLANsReleased:          1,
		Errors:                 2,
		KeaRestarted:           1,
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	if want := "/metrics/job/crucible_provision_worker/component/network_reconcile/layer/api"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	for _, want := range []string{
		`crucible_network_reconcile_total{category="allocated_vlans"} 4`,
		`crucible_network_reconcile_total{category="active_pod_vlans"} 3`,
		`crucible_network_reconcile_repaired_total{kind="interface"} 1`,
		`crucible_network_reconcile_repaired_total{kind="subnet"} 2`,
		`crucible_network_reconcile_repaired_total{kind="kea_binding"} 3`,
		`crucible_network_reconcile_repaired_total{kind="firewall_rule"} 0`,
		`crucible_network_reconcile_errors_total 2`,
		`crucible_network_reconcile_kea_restarted 1`,
		`crucible_network_reconcile_firewall_applied 0`,
		`crucible_opnsense_firewall_rules{kind="total"} 16`,
		`crucible_opnsense_firewall_rules{kind="generated"} 6`,
		`crucible_opnsense_firewall_rules{kind="duplicate"} 1`,
		`crucible_opnsense_firewall_rules{kind="stale"} 2`,
		`crucible_opnsense_firewall_rules{kind="removed"} 3`,
		`crucible_opnsense_firewall_cleanup_limited 1`,
		`crucible_content_filter_policy{kind="expected"} 1`,
		`crucible_content_filter_policy{kind="healthy"} 0`,
		`crucible_content_filter_policy{kind="missing"} 2`,
		`crucible_content_filter_policy{kind="drifted"} 1`,
		`crucible_content_filter_policy{kind="removed"} 3`,
		`crucible_content_filter_last_success_timestamp_seconds 1234567890`,
		`crucible_network_reconcile_run_timestamp_seconds `,
	} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("body missing %q\n--- body ---\n%s", want, gotBody)
		}
	}
}

func TestNetworkReconcilePusher_NoOpWhenBaseURLEmpty(t *testing.T) {
	p := &NetworkReconcilePusher{}
	if err := p.Push(context.Background(), NetworkReconcileCounts{}); err != nil {
		t.Fatalf("expected nil err when BaseURL empty, got %v", err)
	}
}

func TestNetworkReconcilePusher_PropagatesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	p := &NetworkReconcilePusher{BaseURL: srv.URL, Job: "x", HTTP: srv.Client()}
	err := p.Push(context.Background(), NetworkReconcileCounts{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected 502 error, got %v", err)
	}
}

func allocatedVLANRow(vlanTag int, podID uuid.UUID, status string) database.AllocatedVLAN {
	octet := vlanTag - 100
	return database.AllocatedVLAN{
		VLANTag:   vlanTag,
		Subnet:    "10.100." + strconv.Itoa(octet) + ".0/24",
		PodID:     podID,
		PodName:   "pod-" + strconv.Itoa(vlanTag),
		PodStatus: status,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ruleUUIDs(rules []opnsense.FirewallRuleInfo) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.UUID)
	}
	sort.Strings(out)
	return out
}

func countRulesWithSignature(rules []opnsense.FirewallRuleInfo, desired opnsense.FirewallRuleInfo) int {
	want := podPassSignature(desired)
	count := 0
	for _, rule := range rules {
		if isExactPodPassShape(rule) && podPassSignature(rule) == want {
			count++
		}
	}
	return count
}
