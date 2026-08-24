package vcenter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const DRSPlacementPrivilege = "Host.Inventory.EditCluster"

type DRSPlacementTarget struct {
	ComputeResourceType  string
	ComputeResourceMoref string
}

// DRSPlacementTargets extracts the unique immutable compute identities selected
// by a durable placement plan.
func DRSPlacementTargets(placements []models.VMPlacement) []DRSPlacementTarget {
	unique := make(map[string]DRSPlacementTarget)
	for _, placement := range placements {
		key := placement.ComputeResourceType + "\x00" + placement.ComputeResourceMoref
		unique[key] = DRSPlacementTarget{
			ComputeResourceType:  placement.ComputeResourceType,
			ComputeResourceMoref: placement.ComputeResourceMoref,
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	targets := make([]DRSPlacementTarget, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, unique[key])
	}
	return targets
}

// ValidateDRSPlacementPrivileges proves the service account can install the
// mandatory per-VM DRS override before any network or clone mutation.
func (c *Client) ValidateDRSPlacementPrivileges(ctx context.Context, targets []DRSPlacementTarget) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	session, err := c.client.SessionManager.UserSession(ctx)
	if err != nil {
		return fmt.Errorf("load vCenter session for DRS privilege validation: %w", err)
	}
	if session == nil || session.Key == "" {
		return errors.New("vCenter session has no identity for DRS privilege validation")
	}
	manager := object.NewAuthorizationManager(c.client.Client)
	for _, target := range targets {
		switch target.ComputeResourceType {
		case "ComputeResource":
			continue
		case "ClusterComputeResource":
		default:
			return fmt.Errorf("unsupported placement compute type %q", target.ComputeResourceType)
		}
		entity := types.ManagedObjectReference{
			Type:  target.ComputeResourceType,
			Value: target.ComputeResourceMoref,
		}
		granted, err := manager.HasPrivilegeOnEntity(
			ctx,
			entity,
			session.Key,
			[]string{DRSPlacementPrivilege},
		)
		if err != nil {
			return fmt.Errorf("validate DRS privilege on %s: %w", entity, err)
		}
		if len(granted) != 1 || !granted[0] {
			return fmt.Errorf(
				"vCenter service account lacks %s on selected cluster %s",
				DRSPlacementPrivilege,
				strings.TrimSpace(target.ComputeResourceMoref),
			)
		}
	}
	return nil
}

var (
	ErrPlacementDrift                 = errors.New("VM placement drift detected")
	ErrPlacementValidationUnavailable = errors.New("VM placement validation unavailable")
	ErrDRSControlDrift                = errors.New("VM DRS control drift detected")
	ErrDRSControlMissing              = errors.New("VM DRS control is missing")
	ErrDRSControlFailure              = errors.New("VM DRS control could not be enforced")
)

const (
	DRSControlDisabled   = "disabled"
	DRSControlStandalone = "standalone"
)

type PlacementDriftKind string

const (
	PlacementDriftHost     PlacementDriftKind = "host"
	PlacementDriftCompute  PlacementDriftKind = "compute"
	PlacementDriftDRS      PlacementDriftKind = "drs"
	PlacementDriftIdentity PlacementDriftKind = "identity"
)

// PlacementDriftError is returned only after vCenter data proves that a VM no
// longer matches its durable host, compute-resource, or DRS-control identity.
// Property reads and connection failures remain ordinary operational errors.
type PlacementDriftError struct {
	Kind   PlacementDriftKind
	Detail string
	Cause  error
}

func (e *PlacementDriftError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s placement drift: %s: %v", e.Kind, e.Detail, e.Cause)
	}
	return fmt.Sprintf("%s placement drift: %s", e.Kind, e.Detail)
}

func (e *PlacementDriftError) Unwrap() error {
	return e.Cause
}

func (e *PlacementDriftError) Is(target error) bool {
	switch target {
	case ErrPlacementDrift:
		return e.Kind == PlacementDriftHost ||
			e.Kind == PlacementDriftCompute ||
			e.Kind == PlacementDriftIdentity
	case ErrDRSControlDrift:
		return e.Kind == PlacementDriftDRS
	default:
		return errors.Is(e.Cause, target)
	}
}

// VMCloneIdentity is the immutable marker set embedded in every durable clone.
// Cleanup compares it with both live VM properties and extraConfig before any
// destructive retry.
type VMCloneIdentity struct {
	LogicalTemplateID    string
	SourceReplicaID      string
	SourceRef            string
	PodVMID              string
	ComputeResourceType  string
	ComputeResourceMoref string
	ResourcePoolMoref    string
	HostMoref            string
}

// ValidateVMCloneIdentity proves that an exact VM MoRef is still the clone
// created from the persisted source, pool, host, and compute decision.
func (c *Client) ValidateVMCloneIdentity(
	ctx context.Context,
	vmMoref string,
	expected VMCloneIdentity,
) error {
	if err := c.ensureConnected(ctx); err != nil {
		return fmt.Errorf("%w: connect to vCenter: %w", ErrPlacementValidationUnavailable, err)
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(
		ctx,
		vm.Reference(),
		[]string{"resourcePool", "config.extraConfig"},
		&props,
	); err != nil {
		return fmt.Errorf(
			"%w: read VM %s clone identity: %w",
			ErrPlacementValidationUnavailable,
			vmMoref,
			err,
		)
	}
	if props.ResourcePool == nil || props.ResourcePool.Value != expected.ResourcePoolMoref {
		actual := ""
		if props.ResourcePool != nil {
			actual = props.ResourcePool.Value
		}
		return newPlacementDrift(
			PlacementDriftIdentity,
			nil,
			"VM %s resource pool is %s, expected %s",
			vmMoref,
			actual,
			expected.ResourcePoolMoref,
		)
	}
	if props.Config == nil {
		return newPlacementDrift(
			PlacementDriftIdentity,
			nil,
			"VM %s has no readable durable clone markers",
			vmMoref,
		)
	}
	if optionValueString(props.Config.ExtraConfig, CloneOperationIDKey) == "" {
		return newPlacementDrift(
			PlacementDriftIdentity,
			nil,
			"VM %s has no durable clone operation marker",
			vmMoref,
		)
	}
	for _, marker := range []struct {
		key      string
		expected string
	}{
		{CloneOperationTemplateKey, expected.LogicalTemplateID},
		{CloneOperationReplicaKey, expected.SourceReplicaID},
		{CloneOperationSourceKey, expected.SourceRef},
		{CloneOperationPodVMKey, expected.PodVMID},
		{CloneOperationComputeTypeKey, expected.ComputeResourceType},
		{CloneOperationComputeKey, expected.ComputeResourceMoref},
		{CloneOperationPoolKey, expected.ResourcePoolMoref},
		{CloneOperationHostKey, expected.HostMoref},
	} {
		actual := optionValueString(props.Config.ExtraConfig, marker.key)
		if actual != marker.expected {
			return newPlacementDrift(
				PlacementDriftIdentity,
				nil,
				"VM %s marker %s is %q, expected %q",
				vmMoref,
				marker.key,
				actual,
				marker.expected,
			)
		}
	}
	return nil
}

func newPlacementDrift(
	kind PlacementDriftKind,
	cause error,
	format string,
	args ...any,
) error {
	return &PlacementDriftError{
		Kind:   kind,
		Detail: fmt.Sprintf(format, args...),
		Cause:  cause,
	}
}

// EnsureVMPlacementControl excludes a VM from automatic DRS movement. A
// standalone compute resource has no DRS surface and is intrinsically pinned.
func (c *Client) EnsureVMPlacementControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	if err := c.ValidateVMPlacement(ctx, vmMoref, expectedHostMoref); err != nil {
		return err
	}
	switch control {
	case DRSControlStandalone:
		if computeType != "ComputeResource" {
			return newPlacementDrift(
				PlacementDriftDRS,
				nil,
				"standalone control recorded for compute type %s",
				computeType,
			)
		}
		return c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref)
	case DRSControlDisabled:
		if computeType != "ClusterComputeResource" {
			return newPlacementDrift(
				PlacementDriftDRS,
				nil,
				"disabled DRS control recorded for compute type %s",
				computeType,
			)
		}
	default:
		return newPlacementDrift(
			PlacementDriftDRS,
			nil,
			"unsupported control %q",
			control,
		)
	}
	if err := c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref); err != nil {
		return err
	}

	cluster := object.NewClusterComputeResource(c.client.Client, types.ManagedObjectReference{
		Type:  computeType,
		Value: computeMoref,
	})
	config, err := cluster.Configuration(ctx)
	if err != nil {
		return fmt.Errorf(
			"%w: %w: read cluster configuration: %w",
			ErrDRSControlFailure,
			ErrPlacementValidationUnavailable,
			err,
		)
	}
	vmRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoref}
	operation := types.ArrayUpdateOperationAdd
	for _, existing := range config.DrsVmConfig {
		if existing.Key == vmRef {
			if existing.Enabled != nil && !*existing.Enabled {
				return nil
			}
			operation = types.ArrayUpdateOperationEdit
			break
		}
	}

	disabled := false
	task, err := cluster.Reconfigure(ctx, &types.ClusterConfigSpecEx{
		DrsVmConfigSpec: []types.ClusterDrsVmConfigSpec{{
			ArrayUpdateSpec: types.ArrayUpdateSpec{Operation: operation},
			Info: &types.ClusterDrsVmConfigInfo{
				Key:      vmRef,
				Enabled:  &disabled,
				Behavior: types.DrsBehaviorManual,
			},
		}},
	}, true)
	if err != nil {
		return fmt.Errorf(
			"%w: %w: disable DRS for VM %s: %w",
			ErrDRSControlFailure,
			ErrPlacementValidationUnavailable,
			vmMoref,
			err,
		)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf(
			"%w: %w: wait for DRS override on VM %s: %w",
			ErrDRSControlFailure,
			ErrPlacementValidationUnavailable,
			vmMoref,
			err,
		)
	}
	if err := c.ValidateVMPlacementControl(
		ctx,
		vmMoref,
		expectedHostMoref,
		computeType,
		computeMoref,
		control,
	); err != nil {
		return err
	}
	return nil
}

// ValidateVMPlacementControl verifies both exact host placement and the
// persisted mechanism that prevents automatic movement.
func (c *Client) ValidateVMPlacementControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	return c.validateVMPlacementControl(
		ctx,
		vmMoref,
		expectedHostMoref,
		computeType,
		computeMoref,
		control,
	)
}

// ValidateVMPlacementCleanupControl installs a missing DRS override before
// destructive cleanup because an exact clone can be staged before its first
// configuration. An enabled override remains proven drift.
func (c *Client) ValidateVMPlacementCleanupControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	err := c.ValidateVMPlacementControl(
		ctx,
		vmMoref,
		expectedHostMoref,
		computeType,
		computeMoref,
		control,
	)
	if !errors.Is(err, ErrDRSControlMissing) {
		return err
	}
	return c.EnsureVMPlacementControl(
		ctx,
		vmMoref,
		expectedHostMoref,
		computeType,
		computeMoref,
		control,
	)
}

func (c *Client) validateVMPlacementControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	if err := c.ValidateVMPlacement(ctx, vmMoref, expectedHostMoref); err != nil {
		return err
	}
	if err := c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref); err != nil {
		return err
	}
	switch control {
	case DRSControlStandalone:
		if computeType != "ComputeResource" {
			return newPlacementDrift(
				PlacementDriftDRS,
				nil,
				"expected standalone compute resource, got %s",
				computeType,
			)
		}
		return nil
	case DRSControlDisabled:
		if computeType != "ClusterComputeResource" {
			return newPlacementDrift(
				PlacementDriftDRS,
				nil,
				"expected cluster compute resource, got %s",
				computeType,
			)
		}
	default:
		return newPlacementDrift(
			PlacementDriftDRS,
			nil,
			"unsupported persisted control %q",
			control,
		)
	}

	cluster := object.NewClusterComputeResource(c.client.Client, types.ManagedObjectReference{
		Type:  computeType,
		Value: computeMoref,
	})
	config, err := cluster.Configuration(ctx)
	if err != nil {
		return fmt.Errorf("%w: read DRS control for VM %s: %w", ErrPlacementValidationUnavailable, vmMoref, err)
	}
	vmRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoref}
	for _, existing := range config.DrsVmConfig {
		if existing.Key != vmRef {
			continue
		}
		if existing.Enabled != nil && !*existing.Enabled {
			return nil
		}
		return newPlacementDrift(
			PlacementDriftDRS,
			nil,
			"VM %s DRS override is enabled",
			vmMoref,
		)
	}
	return newPlacementDrift(
		PlacementDriftDRS,
		ErrDRSControlMissing,
		"VM %s has no DRS override",
		vmMoref,
	)
}

func (c *Client) validateVMComputeResource(
	ctx context.Context,
	vmMoref, expectedType, expectedMoref string,
) error {
	if expectedType == "" || expectedMoref == "" {
		return newPlacementDrift(
			PlacementDriftCompute,
			nil,
			"persisted compute-resource identity is incomplete",
		)
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var vmProps mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &vmProps); err != nil {
		return fmt.Errorf("%w: read VM %s host: %w", ErrPlacementValidationUnavailable, vmMoref, err)
	}
	if vmProps.Runtime.Host == nil {
		return newPlacementDrift(
			PlacementDriftCompute,
			nil,
			"VM %s has no runtime host",
			vmMoref,
		)
	}
	host := object.NewHostSystem(c.client.Client, *vmProps.Runtime.Host)
	var hostProps mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"parent"}, &hostProps); err != nil {
		return fmt.Errorf("%w: read VM %s compute resource: %w", ErrPlacementValidationUnavailable, vmMoref, err)
	}
	if hostProps.Parent == nil ||
		hostProps.Parent.Type != expectedType ||
		hostProps.Parent.Value != expectedMoref {
		actualType, actualMoref := "", ""
		if hostProps.Parent != nil {
			actualType = hostProps.Parent.Type
			actualMoref = hostProps.Parent.Value
		}
		return newPlacementDrift(
			PlacementDriftCompute,
			nil,
			"VM %s compute resource is %s/%s, expected %s/%s",
			vmMoref,
			actualType,
			actualMoref,
			expectedType,
			expectedMoref,
		)
	}
	return nil
}
