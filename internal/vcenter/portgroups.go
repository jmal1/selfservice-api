package vcenter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	ErrInvalidPortGroupReceipt = errors.New("invalid durable port group receipt")
	ErrLegacyPortGroupReceipt  = errors.New("legacy durable port group receipt lacks immutable host ownership")
)

// PortGroupHostReceipt records the exact immutable host touched by a pod's
// standard-portgroup setup and whether the portgroup predated that pod.
type PortGroupHostReceipt struct {
	HostName     string `json:"host_name"`
	HostMoRef    string `json:"host_moref"`
	ComputeMoRef string `json:"compute_moref"`
	Preexisting  bool   `json:"preexisting"`
}

type PortGroupReceipt struct {
	Name   string                 `json:"name"`
	VLANID int                    `json:"vlan_id"`
	Hosts  []PortGroupHostReceipt `json:"hosts"`
}

func (c *Client) PlanPortGroupMutation(
	ctx context.Context,
	pgName string,
	vlanID int,
) (PortGroupReceipt, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return PortGroupReceipt{}, err
	}
	if strings.TrimSpace(pgName) == "" || vlanID < 1 || vlanID > 4094 {
		return PortGroupReceipt{}, fmt.Errorf("invalid port group name %q or VLAN %d", pgName, vlanID)
	}
	allowed, err := c.resolvedHosts()
	if err != nil {
		return PortGroupReceipt{}, err
	}

	receipt := PortGroupReceipt{Name: pgName, VLANID: vlanID}
	for _, identity := range allowed {
		portGroup, found, err := c.findPortGroupOnHost(ctx, identity, pgName)
		if err != nil {
			return PortGroupReceipt{}, err
		}
		if found && portGroup.Spec.VlanId != int32(vlanID) {
			return PortGroupReceipt{}, fmt.Errorf(
				"port group %q on allowlisted host %s (%s) uses VLAN %d, expected %d",
				pgName,
				identity.Name,
				identity.MoRef,
				portGroup.Spec.VlanId,
				vlanID,
			)
		}
		receipt.Hosts = append(receipt.Hosts, PortGroupHostReceipt{
			HostName:     identity.Name,
			HostMoRef:    identity.MoRef,
			ComputeMoRef: identity.ComputeMoRef,
			Preexisting:  found,
		})
	}
	return receipt, nil
}

// ApplyPortGroupMutation applies a receipt that the caller has already made
// durable while holding the cross-process PostgreSQL advisory lock.
func (c *Client) ApplyPortGroupMutation(ctx context.Context, receipt PortGroupReceipt) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	present, err := c.validatePortGroupReceipt(ctx, receipt)
	if err != nil {
		return err
	}
	if err := applyPortGroupReceipt(ctx, receipt, present, c.addPortGroup, c.removePortGroup); err != nil {
		return err
	}
	c.logger.Info("port group created on allowlisted hosts",
		"name", receipt.Name,
		"vlan_id", receipt.VLANID,
		"hosts", len(receipt.Hosts))
	return nil
}

func applyPortGroupReceipt(
	ctx context.Context,
	receipt PortGroupReceipt,
	present map[string]bool,
	add func(context.Context, PortGroupHostReceipt, string, int) error,
	remove func(context.Context, PortGroupHostReceipt, string) error,
) error {
	var created []PortGroupHostReceipt
	for _, host := range receipt.Hosts {
		if host.Preexisting {
			if !present[host.HostMoRef] {
				return fmt.Errorf(
					"preexisting port group %q disappeared from %s (%s)",
					receipt.Name,
					host.HostName,
					host.HostMoRef,
				)
			}
			continue
		}
		if present[host.HostMoRef] {
			// The receipt was persisted before AddPortGroup. Seeing the exact
			// VLAN on retry proves this receipt still owns its deletion.
			created = append(created, host)
			continue
		}
		if err := add(ctx, host, receipt.Name, receipt.VLANID); err != nil {
			compensationErr := removeCreatedPortGroups(ctx, receipt.Name, created, remove)
			return errors.Join(
				fmt.Errorf("create port group %q on %s (%s): %w", receipt.Name, host.HostName, host.HostMoRef, err),
				compensationErr,
			)
		}
		created = append(created, host)
	}
	return nil
}

// DeletePortGroupMutation removes only portgroups that this exact durable
// receipt says were newly created. All identities are validated before the
// first mutation so a historical ESXi2 receipt fails closed.
func (c *Client) DeletePortGroupMutation(ctx context.Context, receipt PortGroupReceipt) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if _, err := c.validatePortGroupReceipt(ctx, receipt); err != nil {
		return err
	}
	return deletePortGroupReceipt(ctx, receipt, c.removePortGroup)
}

func deletePortGroupReceipt(
	ctx context.Context,
	receipt PortGroupReceipt,
	remove func(context.Context, PortGroupHostReceipt, string) error,
) error {
	var errs []error
	for i := len(receipt.Hosts) - 1; i >= 0; i-- {
		host := receipt.Hosts[i]
		if host.Preexisting {
			continue
		}
		if err := remove(ctx, host, receipt.Name); err != nil {
			errs = append(errs, fmt.Errorf(
				"delete port group %q from %s (%s): %w",
				receipt.Name,
				host.HostName,
				host.HostMoRef,
				err,
			))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) validatePortGroupReceipt(
	ctx context.Context,
	receipt PortGroupReceipt,
) (map[string]bool, error) {
	if strings.TrimSpace(receipt.Name) != "" && receipt.VLANID == 0 && len(receipt.Hosts) == 0 {
		return nil, fmt.Errorf("%w for %q", ErrLegacyPortGroupReceipt, receipt.Name)
	}
	if strings.TrimSpace(receipt.Name) == "" || receipt.VLANID < 1 || receipt.VLANID > 4094 {
		return nil, fmt.Errorf("%w: missing a valid name or VLAN", ErrInvalidPortGroupReceipt)
	}
	if len(receipt.Hosts) == 0 {
		return nil, fmt.Errorf("%w: contains no immutable host identities", ErrInvalidPortGroupReceipt)
	}
	allowed, err := c.resolvedHosts()
	if err != nil {
		return nil, err
	}
	byMoRef := make(map[string]HostIdentity, len(allowed))
	for _, identity := range allowed {
		byMoRef[identity.MoRef] = identity
	}
	seen := make(map[string]struct{}, len(receipt.Hosts))
	for _, host := range receipt.Hosts {
		if host.HostMoRef == "" || host.HostName == "" || host.ComputeMoRef == "" {
			return nil, fmt.Errorf("%w: contains an incomplete host identity", ErrInvalidPortGroupReceipt)
		}
		if _, duplicate := seen[host.HostMoRef]; duplicate {
			return nil, fmt.Errorf(
				"%w: contains duplicate host MoRef %s",
				ErrInvalidPortGroupReceipt,
				host.HostMoRef,
			)
		}
		seen[host.HostMoRef] = struct{}{}
		identity, ok := byMoRef[host.HostMoRef]
		if !ok {
			return nil, fmt.Errorf(
				"%w: receipt host %s (%s) is outside current VCENTER_HOSTS; no switch mutation was attempted",
				ErrHostNotAllowed,
				host.HostName,
				host.HostMoRef,
			)
		}
		if identity.Name != host.HostName || identity.ComputeMoRef != host.ComputeMoRef {
			return nil, fmt.Errorf(
				"%w: receipt identity %s/%s/%s no longer matches %s/%s/%s",
				ErrAmbiguousHostIdentity,
				host.HostName,
				host.HostMoRef,
				host.ComputeMoRef,
				identity.Name,
				identity.MoRef,
				identity.ComputeMoRef,
			)
		}
	}
	present := make(map[string]bool, len(receipt.Hosts))
	for _, host := range receipt.Hosts {
		identity := byMoRef[host.HostMoRef]
		portGroup, found, err := c.findPortGroupOnHost(ctx, identity, receipt.Name)
		if err != nil {
			return nil, err
		}
		if found && portGroup.Spec.VlanId != int32(receipt.VLANID) {
			return nil, fmt.Errorf(
				"%w: port group %q on %s (%s) now uses VLAN %d, receipt expects %d",
				ErrInvalidPortGroupReceipt,
				receipt.Name,
				host.HostName,
				host.HostMoRef,
				portGroup.Spec.VlanId,
				receipt.VLANID,
			)
		}
		present[host.HostMoRef] = found
	}
	return present, nil
}

func (c *Client) findPortGroupOnHost(
	ctx context.Context,
	identity HostIdentity,
	pgName string,
) (types.HostPortGroup, bool, error) {
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: identity.MoRef,
	})
	var props mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"name", "parent", "config.network.portgroup"}, &props); err != nil {
		return types.HostPortGroup{}, false, fmt.Errorf(
			"read port groups on %s (%s): %w",
			identity.Name,
			identity.MoRef,
			err,
		)
	}
	if props.Name != identity.Name || props.Parent == nil || props.Parent.Value != identity.ComputeMoRef {
		return types.HostPortGroup{}, false, fmt.Errorf(
			"%w: configured host %s (%s) changed identity",
			ErrAmbiguousHostIdentity,
			identity.Name,
			identity.MoRef,
		)
	}
	if props.Config == nil || props.Config.Network == nil {
		return types.HostPortGroup{}, false, fmt.Errorf("host %s has no readable network configuration", identity.Name)
	}
	for _, portGroup := range props.Config.Network.Portgroup {
		if portGroup.Spec.Name == pgName {
			return portGroup, true, nil
		}
	}
	return types.HostPortGroup{}, false, nil
}

func (c *Client) addPortGroup(
	ctx context.Context,
	hostReceipt PortGroupHostReceipt,
	pgName string,
	vlanID int,
) error {
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: hostReceipt.HostMoRef,
	})
	ns, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return fmt.Errorf("get network system: %w", err)
	}
	return ns.AddPortGroup(ctx, types.HostPortGroupSpec{
		Name:        pgName,
		VlanId:      int32(vlanID),
		VswitchName: "vSwitch0",
		Policy:      types.HostNetworkPolicy{},
	})
}

func (c *Client) removePortGroup(
	ctx context.Context,
	hostReceipt PortGroupHostReceipt,
	pgName string,
) error {
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: hostReceipt.HostMoRef,
	})
	ns, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return fmt.Errorf("get network system: %w", err)
	}
	if err := ns.RemovePortGroup(ctx, pgName); err != nil && !isResourceNotFoundErr(err) {
		return err
	}
	return nil
}

func removeCreatedPortGroups(
	ctx context.Context,
	pgName string,
	created []PortGroupHostReceipt,
	remove func(context.Context, PortGroupHostReceipt, string) error,
) error {
	var errs []error
	for i := len(created) - 1; i >= 0; i-- {
		host := created[i]
		if err := remove(ctx, host, pgName); err != nil {
			errs = append(errs, fmt.Errorf(
				"compensate port group %q on %s (%s): %w",
				pgName,
				host.HostName,
				host.HostMoRef,
				err,
			))
		}
	}
	return errors.Join(errs...)
}
