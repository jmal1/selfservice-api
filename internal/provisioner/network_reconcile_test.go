package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
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

// opnsenseFilterGetReadback models the canonical FirewallRuleInfo that
// opnsense.GetFirewallRules produces for a rule created via addRule, after
// parsing firewall/filter/get and canonicalizing its option-map/plain fields.
func opnsenseFilterGetReadback(rule opnsense.FirewallRule, uuid string) opnsense.FirewallRuleInfo {
	return opnsense.FirewallRuleInfo{
		UUID:        uuid,
		Interface:   canonicalInterfaceList(rule.Interface),
		Direction:   canonicalField(rule.Direction),
		IPProtocol:  canonicalField(rule.IPProtocol),
		Protocol:    canonicalField(rule.Protocol),
		Source:      canonicalField(rule.Source),
		Destination: canonicalField(rule.Destination),
		Action:      canonicalField(rule.Action),
		// firewall/filter/get drops the description and the reconciler sets no
		// ports; leave SourcePort/DestinationPort empty.
	}
}

func (f *fakeNetworkOPN) ApplyFirewall(_ context.Context) error {
	f.applyFirewallCalls++
	return f.applyFirewallErr
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
	return opnsenseFilterGetReadback(opnsense.FirewallRule{
		Enabled:     "1",
		Action:      "pass",
		Interface:   ifName,
		Direction:   "in",
		IPProtocol:  "inet",
		Protocol:    "any",
		Source:      subnet,
		Destination: "any",
	}, "fw-existing")
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
	if counts.FirewallRulesRepaired != 0 || counts.FirewallApplied != 0 || opn.applyFirewallCalls != 0 {
		t.Fatalf("expected no firewall changes, got counts=%+v applyCalls=%d", counts, opn.applyFirewallCalls)
	}
	if opn.restartDHPCCalls != 0 {
		t.Fatalf("expected no Kea restart call, got %d", opn.restartDHPCCalls)
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

func TestReconcileNetwork_MixedSet(t *testing.T) {
	activeHealthy := allocatedVLANRow(106, uuid.New(), "active")
	activeNeedsBinding := allocatedVLANRow(107, uuid.New(), "configuring")
	terminal := allocatedVLANRow(108, uuid.New(), "destroy_failed")

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

// TestHasEquivalentPassRule_AgainstRealFilterGetParse is an end-to-end guard for
// the reviewer's "false-green" concern: it runs the REAL
// opnsense.GetFirewallRules against a production-shaped firewall/filter/get
// payload (option-maps, multi-select interface, empty description) and asserts
// the reconciler's content-signature dedup DETECTS the already-present pod rule
// — proving the idempotency source is filter/get, not the near-empty searchRule.
func TestHasEquivalentPassRule_AgainstRealFilterGetParse(t *testing.T) {
	const filterGetBody = `{
	  "filter": { "rules": { "rule": {
	    "pod-opt3": {
	      "enabled": "1", "sequence": "1",
	      "interface":  {"opt3": {"value":"OPT3","selected":1}, "opt5": {"value":"OPT5","selected":0}},
	      "direction":  {"in": {"value":"in","selected":1}, "out": {"value":"out","selected":0}},
	      "action":     {"pass": {"value":"Pass","selected":1}, "block": {"value":"Block","selected":0}},
	      "ipprotocol": {"inet": {"value":"IPv4","selected":1}, "inet6": {"value":"IPv6","selected":0}},
	      "protocol":   {"any": {"value":"any","selected":1}, "TCP": {"value":"TCP","selected":0}},
	      "source_net": "10.100.0.0/24", "source_port": "",
	      "destination_net": "any", "destination_port": "", "description": ""
	    },
	    "mgmt": {
	      "enabled": "1", "sequence": "2",
	      "interface":  {"lan": {"value":"LAN","selected":"1"}, "opt1": {"value":"OPT1","selected":"1"}},
	      "direction":  {"any": {"value":"any","selected":"1"}},
	      "action":     {"pass": {"value":"Pass","selected":"1"}},
	      "ipprotocol": {"inet": {"value":"IPv4","selected":"1"}},
	      "protocol":   {"any": {"value":"any","selected":"1"}},
	      "source_net": "10.10.10.0/24", "source_port": "",
	      "destination_net": "10.100.0.0/16", "destination_port": "",
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

	// The exact rule the reconciler would create for VLAN 100 on opt3 must be
	// detected as already-present (so it is NOT re-created).
	existing := opnsense.FirewallRule{
		Enabled: "1", Action: "pass", Interface: "opt3", Direction: "in",
		IPProtocol: "inet", Protocol: "any", Source: "10.100.0.0/24",
		Destination: "any", Description: "Allow Pod VLAN 100 traffic",
	}
	if !hasEquivalentPassRule(rules, existing) {
		t.Fatalf("expected existing pod rule (opt3) to be detected from filter/get; rules=%+v", rules)
	}

	// A pod rule for a DIFFERENT VLAN/interface is not present -> must NOT match,
	// so the reconciler would (correctly) create it.
	missing := existing
	missing.Interface = "opt4"
	missing.Source = "10.100.1.0/24"
	if hasEquivalentPassRule(rules, missing) {
		t.Fatalf("must not match a rule that is absent from filter/get; rules=%+v", rules)
	}

	// The management rule (lan,opt1) must not be mistaken for a pod pass rule.
	mgmtShaped := existing
	mgmtShaped.Interface = "opt3"
	mgmtShaped.Destination = "10.100.0.0/16" // different destination than the pod rule
	if hasEquivalentPassRule(rules, mgmtShaped) {
		t.Fatalf("destination must be part of the signature; unexpected match; rules=%+v", rules)
	}
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
		AllocatedVLANs:      4,
		ActivePodVLANs:      3,
		InterfacesRepaired:  1,
		SubnetsRepaired:     2,
		KeaBindingsRepaired: 3,
		VLANsReleased:       1,
		Errors:              2,
		KeaRestarted:        1,
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
