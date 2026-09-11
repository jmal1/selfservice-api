package vcenter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
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
	HostName     string                         `json:"host_name"`
	HostMoRef    string                         `json:"host_moref"`
	ComputeMoRef string                         `json:"compute_moref"`
	VSwitchName  string                         `json:"vswitch_name"`
	Security     PortGroupSecurityPolicyReceipt `json:"security"`
	PortGroupKey string                         `json:"portgroup_key,omitempty"`
	Preexisting  bool                           `json:"preexisting"`
}

type PortGroupSecurityPolicyReceipt struct {
	AllowPromiscuous *bool `json:"allow_promiscuous"`
	MacChanges       *bool `json:"mac_changes"`
	ForgedTransmits  *bool `json:"forged_transmits"`
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
	return c.PlanPortGroupMutationForHosts(ctx, pgName, vlanID, nil)
}

// PlanPortGroupMutationForHosts records switch state only for hosts selected by
// the durable VM placement plan. A nil target list preserves the legacy
// all-allowlisted-host behavior for existing callers.
func (c *Client) PlanPortGroupMutationForHosts(
	ctx context.Context,
	pgName string,
	vlanID int,
	targetHostMoRefs []string,
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

	targets := make(map[string]struct{}, len(targetHostMoRefs))
	for _, moref := range targetHostMoRefs {
		if strings.TrimSpace(moref) == "" {
			return PortGroupReceipt{}, fmt.Errorf("%w: target host MoRef is empty", ErrInvalidPortGroupReceipt)
		}
		targets[moref] = struct{}{}
	}
	receipt := PortGroupReceipt{Name: pgName, VLANID: vlanID}
	for _, identity := range allowed {
		if len(targets) > 0 {
			if _, selected := targets[identity.MoRef]; !selected {
				continue
			}
			delete(targets, identity.MoRef)
		}
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
		expectedSecurity := PortGroupSecurityPolicyReceipt{}
		portGroupKey := ""
		if found {
			if err := validatePortGroupConfig(portGroup, pgName, vlanID, "vSwitch0", expectedSecurity); err != nil {
				return PortGroupReceipt{}, err
			}
			portGroupKey = portGroup.Key
		}
		receipt.Hosts = append(receipt.Hosts, PortGroupHostReceipt{
			HostName:     identity.Name,
			HostMoRef:    identity.MoRef,
			ComputeMoRef: identity.ComputeMoRef,
			VSwitchName:  "vSwitch0",
			Security:     expectedSecurity,
			PortGroupKey: portGroupKey,
			Preexisting:  found,
		})
	}

	if len(targets) > 0 {
		missing := make([]string, 0, len(targets))
		for moref := range targets {
			missing = append(missing, moref)
		}
		sort.Strings(missing)
		return PortGroupReceipt{}, fmt.Errorf(
			"%w: selected hosts are outside VCENTER_HOSTS: %s",
			ErrHostNotAllowed,
			strings.Join(missing, ", "),
		)
	}
	if len(receipt.Hosts) == 0 {
		return PortGroupReceipt{}, fmt.Errorf("%w: no target hosts selected", ErrInvalidPortGroupReceipt)
	}
	return receipt, nil
}

// CapturePortGroupKeys binds the immutable exact host/config plan to vSphere's
// stable per-host portgroup key. Missing owned groups require no key; present
// groups must match every planned property before their key is returned.
func (c *Client) CapturePortGroupKeys(
	ctx context.Context,
	receipt PortGroupReceipt,
) (map[string]string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	observed, err := c.validatePortGroupReceipt(ctx, receipt)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]string)
	for _, host := range receipt.Hosts {
		portGroup := observed[host.HostMoRef]
		if portGroup.Key == "" {
			if host.Preexisting {
				return nil, fmt.Errorf(
					"preexisting port group %q disappeared from %s (%s)",
					receipt.Name,
					host.HostName,
					host.HostMoRef,
				)
			}
			if host.PortGroupKey != "" {
				continue
			}
			return nil, fmt.Errorf(
				"%w: port group %q on %s (%s) has no stable vSphere key",
				ErrInvalidPortGroupReceipt,
				receipt.Name,
				host.HostName,
				host.HostMoRef,
			)
		}
		if host.PortGroupKey != "" && host.PortGroupKey != portGroup.Key {
			return nil, fmt.Errorf(
				"%w: port group %q on %s (%s) key changed from %s to %s",
				ErrInvalidPortGroupReceipt,
				receipt.Name,
				host.HostName,
				host.HostMoRef,
				host.PortGroupKey,
				portGroup.Key,
			)
		}
		keys[host.HostMoRef] = portGroup.Key
	}
	return keys, nil
}

func PortGroupReceiptWithKeys(
	receipt PortGroupReceipt,
	keys map[string]string,
) (PortGroupReceipt, error) {
	for i := range receipt.Hosts {
		host := &receipt.Hosts[i]
		key := keys[host.HostMoRef]
		if host.PortGroupKey != "" && key != "" && host.PortGroupKey != key {
			return PortGroupReceipt{}, fmt.Errorf(
				"%w: receipt key conflict for host %s",
				ErrInvalidPortGroupReceipt,
				host.HostMoRef,
			)
		}
		if key != "" {
			host.PortGroupKey = key
		}
	}
	return receipt, nil
}

// ApplyPortGroupMutation applies a receipt that the caller has already made
// durable while holding the cross-process PostgreSQL advisory lock.
func (c *Client) ApplyPortGroupMutation(ctx context.Context, receipt PortGroupReceipt) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	observed, err := c.validatePortGroupReceipt(ctx, receipt)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(observed))
	for host, portGroup := range observed {
		present[host] = portGroup.Key != ""
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
	observed, err := c.validatePortGroupReceipt(ctx, receipt)
	if err != nil {
		return err
	}
	for _, host := range receipt.Hosts {
		portGroup := observed[host.HostMoRef]
		if host.Preexisting {
			continue
		}
		if portGroup.Key == "" {
			if host.PortGroupKey == "" {
				return fmt.Errorf(
					"%w: owned port group %q on %s is absent without a stable key",
					ErrInvalidPortGroupReceipt,
					receipt.Name,
					host.HostMoRef,
				)
			}
			continue
		}
		if host.PortGroupKey == "" {
			return fmt.Errorf(
				"%w: owned port group %q on %s has no stable key",
				ErrInvalidPortGroupReceipt,
				receipt.Name,
				host.HostMoRef,
			)
		}
	}
	var errs []error
	for i := len(receipt.Hosts) - 1; i >= 0; i-- {
		host := receipt.Hosts[i]
		if host.Preexisting || observed[host.HostMoRef].Key == "" {
			continue
		}
		if err := c.removePortGroup(ctx, host, receipt.Name); err != nil {
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
) (map[string]types.HostPortGroup, error) {
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
		if host.HostMoRef == "" || host.HostName == "" || host.ComputeMoRef == "" ||
			host.VSwitchName == "" {
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
	observed := make(map[string]types.HostPortGroup, len(receipt.Hosts))
	for _, host := range receipt.Hosts {
		identity := byMoRef[host.HostMoRef]
		portGroup, found, err := c.findPortGroupOnHostByIdentity(
			ctx,
			identity,
			receipt.Name,
			host.PortGroupKey,
		)
		if err != nil {
			return nil, err
		}
		if found {
			if err := validatePortGroupConfig(
				portGroup,
				receipt.Name,
				receipt.VLANID,
				host.VSwitchName,
				host.Security,
			); err != nil {
				return nil, fmt.Errorf("%s (%s): %w", host.HostName, host.HostMoRef, err)
			}
			if host.PortGroupKey != "" && host.PortGroupKey != portGroup.Key {
				return nil, fmt.Errorf(
					"%w: port group %q key changed from %s to %s",
					ErrInvalidPortGroupReceipt,
					receipt.Name,
					host.PortGroupKey,
					portGroup.Key,
				)
			}
			observed[host.HostMoRef] = portGroup
			continue
		}
		observed[host.HostMoRef] = types.HostPortGroup{}
	}
	return observed, nil
}

func validatePortGroupConfig(
	portGroup types.HostPortGroup,
	name string,
	vlanID int,
	vSwitchName string,
	security PortGroupSecurityPolicyReceipt,
) error {
	if portGroup.Spec.Name != name ||
		portGroup.Spec.VlanId != int32(vlanID) ||
		portGroup.Spec.VswitchName != vSwitchName {
		return fmt.Errorf(
			"%w: port group identity/config is name=%q vlan=%d vSwitch=%q, expected %q/%d/%q",
			ErrInvalidPortGroupReceipt,
			portGroup.Spec.Name,
			portGroup.Spec.VlanId,
			portGroup.Spec.VswitchName,
			name,
			vlanID,
			vSwitchName,
		)
	}
	actual := PortGroupSecurityPolicyReceipt{}
	if portGroup.Spec.Policy.Security != nil {
		actual.AllowPromiscuous = portGroup.Spec.Policy.Security.AllowPromiscuous
		actual.MacChanges = portGroup.Spec.Policy.Security.MacChanges
		actual.ForgedTransmits = portGroup.Spec.Policy.Security.ForgedTransmits
	}
	if !reflect.DeepEqual(actual, security) {
		return fmt.Errorf(
			"%w: port group %q security policy changed",
			ErrInvalidPortGroupReceipt,
			name,
		)
	}
	return nil
}

func (c *Client) findPortGroupOnHost(
	ctx context.Context,
	identity HostIdentity,
	pgName string,
) (types.HostPortGroup, bool, error) {
	return c.findPortGroupOnHostByIdentity(ctx, identity, pgName, "")
}

func (c *Client) findPortGroupOnHostByIdentity(
	ctx context.Context,
	identity HostIdentity,
	pgName string,
	portGroupKey string,
) (types.HostPortGroup, bool, error) {
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: identity.MoRef,
	})
	var props mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"name", "parent"}, &props); err != nil {
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
	networkSystem, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return types.HostPortGroup{}, false, fmt.Errorf(
			"resolve network system on %s (%s): %w",
			identity.Name,
			identity.MoRef,
			err,
		)
	}
	var network mo.HostNetworkSystem
	if err := networkSystem.Properties(
		ctx,
		networkSystem.Reference(),
		[]string{"networkInfo.portgroup"},
		&network,
	); err != nil {
		return types.HostPortGroup{}, false, fmt.Errorf(
			"read network system on %s (%s): %w",
			identity.Name,
			identity.MoRef,
			err,
		)
	}
	if network.NetworkInfo == nil {
		return types.HostPortGroup{}, false, fmt.Errorf("host %s has no readable network configuration", identity.Name)
	}
	for _, portGroup := range network.NetworkInfo.Portgroup {
		portGroup.Key = c.stablePortGroupKey(identity, portGroup)
		if portGroupKey != "" && portGroup.Key == portGroupKey {
			return portGroup, true, nil
		}
		if portGroupKey == "" && portGroup.Spec.Name == pgName {
			return portGroup, true, nil
		}
	}
	if portGroupKey != "" {
		for _, portGroup := range network.NetworkInfo.Portgroup {
			portGroup.Key = c.stablePortGroupKey(identity, portGroup)
			if portGroup.Spec.Name == pgName {
				return portGroup, true, nil
			}
		}
	}
	return types.HostPortGroup{}, false, nil
}

func (c *Client) stablePortGroupKey(
	identity HostIdentity,
	portGroup types.HostPortGroup,
) string {
	if c.portGroupKeyOverride != nil {
		return c.portGroupKeyOverride(identity, portGroup)
	}
	return portGroup.Key
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
