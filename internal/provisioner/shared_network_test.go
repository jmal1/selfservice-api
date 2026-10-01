package provisioner

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type fakeStripeAPI struct {
	vlan           *opnsense.VLAN
	dhcp           *opnsense.DHCPSubnet
	createVLAN     int
	createDHCP     int
	createRule     int
	reconfigure    int
	rules          []opnsense.FirewallRuleInfo
	failCreateVLAN error
}

func (f *fakeStripeAPI) GetVLANByTag(context.Context, int) (*opnsense.VLAN, error) {
	return f.vlan, nil
}
func (f *fakeStripeAPI) CreateVLAN(context.Context, string, int, string) (string, error) {
	f.createVLAN++
	if f.failCreateVLAN != nil {
		return "", f.failCreateVLAN
	}
	return "vlan-uuid", nil
}
func (f *fakeStripeAPI) ReconfigureVLANs(context.Context) error {
	f.reconfigure++
	return nil
}
func (f *fakeStripeAPI) GetDHCPSubnetByNetwork(context.Context, string) (*opnsense.DHCPSubnet, error) {
	return f.dhcp, nil
}
func (f *fakeStripeAPI) CreateDHCPSubnet(context.Context, string, string, string) (string, error) {
	f.createDHCP++
	return "dhcp-uuid", nil
}
func (f *fakeStripeAPI) AddDHCPInterface(context.Context, string) error { return nil }
func (f *fakeStripeAPI) ReconfigureDHCP(context.Context) error          { return nil }
func (f *fakeStripeAPI) CreateFirewallRule(_ context.Context, rule opnsense.FirewallRule) (string, error) {
	f.createRule++
	f.rules = append(f.rules, opnsense.FirewallRuleInfo{Description: rule.Description})
	return "rule-uuid", nil
}
func (f *fakeStripeAPI) GetFirewallRules(context.Context) ([]opnsense.FirewallRuleInfo, error) {
	return f.rules, nil
}

type fakeStripeSSH struct{ calls int }

func (f *fakeStripeSSH) AssignInterface(context.Context, int, string) (string, error) {
	f.calls++
	return "opt9", nil
}

type fakeStripeSwitch struct{ planned, applied int }

func (f *fakeStripeSwitch) PlanPortGroupMutation(context.Context, string, int) (vcenter.PortGroupReceipt, error) {
	f.planned++
	return vcenter.PortGroupReceipt{Name: "Shared-VLAN347", VLANID: 347}, nil
}
func (f *fakeStripeSwitch) ApplyPortGroupMutation(context.Context, vcenter.PortGroupReceipt) error {
	f.applied++
	return nil
}

func TestApplySharedStripeCreatesObjectsOnce(t *testing.T) {
	api := &fakeStripeAPI{}
	ssh := &fakeStripeSSH{}
	sw := &fakeStripeSwitch{}
	stripe := models.SharedStripes()[0]
	id := uuid.New()
	if err := applySharedStripe(context.Background(), api, ssh, sw, id, stripe); err != nil {
		t.Fatal(err)
	}
	if api.createVLAN != 1 || api.createDHCP != 1 || api.createRule != 1 || sw.planned != 1 || sw.applied != 1 {
		t.Fatalf("creates vlan=%d dhcp=%d rule=%d plan=%d apply=%d", api.createVLAN, api.createDHCP, api.createRule, sw.planned, sw.applied)
	}
	api.vlan = &opnsense.VLAN{UUID: "already"}
	api.dhcp = &opnsense.DHCPSubnet{UUID: "already"}
	if err := applySharedStripe(context.Background(), api, ssh, sw, id, stripe); err != nil {
		t.Fatal(err)
	}
	if api.createVLAN != 1 || api.createDHCP != 1 || api.createRule != 1 {
		t.Fatalf("second call created vlan=%d dhcp=%d rule=%d", api.createVLAN, api.createDHCP, api.createRule)
	}
}

func TestProvisionSharedNetworksRequiresClients(t *testing.T) {
	if err := (&Provisioner{}).ProvisionSharedNetworks(context.Background()); err == nil {
		t.Fatal("expected missing-client error")
	}
}

func TestApplySharedStripeStopsWhenVLANCreateFails(t *testing.T) {
	api := &fakeStripeAPI{failCreateVLAN: errors.New("down")}
	err := applySharedStripe(context.Background(), api, &fakeStripeSSH{}, &fakeStripeSwitch{}, uuid.New(), models.SharedStripes()[0])
	if err == nil {
		t.Fatal("expected error")
	}
	if api.createDHCP != 0 {
		t.Fatal("DHCP was created after VLAN failure")
	}
}
