package provisioner

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// stripeAPI is the OPNsense surface a stripe provision uses. Student pod
// create does not call it.
type stripeAPI interface {
	GetVLANByTag(ctx context.Context, tag int) (*opnsense.VLAN, error)
	CreateVLAN(ctx context.Context, parentIf string, tag int, descr string) (string, error)
	ReconfigureVLANs(ctx context.Context) error
	GetDHCPSubnetByNetwork(ctx context.Context, network string) (*opnsense.DHCPSubnet, error)
	CreateDHCPSubnet(ctx context.Context, subnet, poolRange, gateway string) (string, error)
	AddDHCPInterface(ctx context.Context, ifName string) error
	ReconfigureDHCP(ctx context.Context) error
	CreateFirewallRule(ctx context.Context, rule opnsense.FirewallRule) (string, error)
	GetFirewallRules(ctx context.Context) ([]opnsense.FirewallRuleInfo, error)
}

type stripeSSH interface {
	AssignInterface(ctx context.Context, vlanTag int, ipAddr string) (string, error)
}

type stripeSwitch interface {
	PlanPortGroupMutation(ctx context.Context, pgName string, vlanID int) (vcenter.PortGroupReceipt, error)
	ApplyPortGroupMutation(ctx context.Context, receipt vcenter.PortGroupReceipt) error
}

// applySharedStripe creates one stripe's VLAN, interface, DHCP pool, pass
// rule, and port group. A second call reuses objects that already exist.
func applySharedStripe(ctx context.Context, api stripeAPI, ssh stripeSSH, sw stripeSwitch, id uuid.UUID, stripe models.Stripe) error {
	existing, err := api.GetVLANByTag(ctx, stripe.VLANTag)
	if err != nil {
		return fmt.Errorf("read VLAN %d: %w", stripe.VLANTag, err)
	}
	if existing == nil {
		if _, err := api.CreateVLAN(ctx, "vmx1", stripe.VLANTag, stripe.PortGroup); err != nil {
			return fmt.Errorf("create VLAN %d: %w", stripe.VLANTag, err)
		}
		if err := api.ReconfigureVLANs(ctx); err != nil {
			return fmt.Errorf("reconfigure VLANs: %w", err)
		}
	}
	ifName, err := ssh.AssignInterface(ctx, stripe.VLANTag, stripe.Gateway)
	if err != nil {
		return fmt.Errorf("assign interface for VLAN %d: %w", stripe.VLANTag, err)
	}
	dhcp, err := api.GetDHCPSubnetByNetwork(ctx, stripe.CIDR)
	if err != nil {
		return fmt.Errorf("read DHCP %s: %w", stripe.CIDR, err)
	}
	if dhcp == nil {
		pool := stripe.DHCPStart + "-" + stripe.DHCPEnd
		if _, err := api.CreateDHCPSubnet(ctx, stripe.CIDR, pool, stripe.Gateway); err != nil {
			return fmt.Errorf("create DHCP %s: %w", stripe.CIDR, err)
		}
	}
	if err := api.AddDHCPInterface(ctx, ifName); err != nil {
		return fmt.Errorf("add DHCP interface %s: %w", ifName, err)
	}
	if err := api.ReconfigureDHCP(ctx); err != nil {
		return fmt.Errorf("reconfigure DHCP: %w", err)
	}
	description := "crucible:shared-net:v1:" + id.String()
	rules, err := api.GetFirewallRules(ctx)
	if err != nil {
		return fmt.Errorf("read firewall rules: %w", err)
	}
	var haveRule bool
	for _, rule := range rules {
		if rule.Description == description {
			haveRule = true
			break
		}
	}
	if !haveRule {
		if _, err := api.CreateFirewallRule(ctx, opnsense.FirewallRule{
			Enabled:     "1",
			Quick:       "0",
			Action:      "pass",
			Interface:   ifName,
			Direction:   "in",
			IPProtocol:  "inet",
			Protocol:    "any",
			Source:      stripe.CIDR,
			Destination: "any",
			Description: description,
		}); err != nil {
			return fmt.Errorf("create shared pass rule: %w", err)
		}
	}
	receipt, err := sw.PlanPortGroupMutation(ctx, stripe.PortGroup, stripe.VLANTag)
	if err != nil {
		return fmt.Errorf("plan port group %s: %w", stripe.PortGroup, err)
	}
	if err := sw.ApplyPortGroupMutation(ctx, receipt); err != nil {
		return fmt.Errorf("apply port group %s: %w", stripe.PortGroup, err)
	}
	return nil
}

// ProvisionSharedNetworks builds any stripe that is not already active.
// A student request never calls this.
func (p *Provisioner) ProvisionSharedNetworks(ctx context.Context) error {
	if p == nil || p.db == nil || p.opn == nil || p.opnSSH == nil || p.vc == nil {
		return fmt.Errorf("shared network provision is not configured")
	}
	if err := p.db.EnsureSharedNetworks(ctx); err != nil {
		return err
	}
	rows, err := p.db.ListSharedNetworks(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Status == "active" {
			continue
		}
		if err := applySharedStripe(ctx, p.opn, p.opnSSH, p.vc, row.ID, row.Stripe); err != nil {
			_ = p.db.SetSharedNetworkStatus(ctx, row.ID, "error", err.Error())
			return err
		}
		if err := p.db.SetSharedNetworkStatus(ctx, row.ID, "active", ""); err != nil {
			return err
		}
	}
	return nil
}
