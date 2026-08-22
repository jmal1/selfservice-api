package vcenter

import (
	"context"
	"errors"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	ErrPlacementDrift    = errors.New("VM placement drift detected")
	ErrDRSControlDrift   = errors.New("VM DRS control drift detected")
	ErrDRSControlFailure = errors.New("VM DRS control could not be enforced")
)

const (
	DRSControlDisabled   = "disabled"
	DRSControlStandalone = "standalone"
)

// EnsureVMPlacementControl excludes a VM from automatic DRS movement. A
// standalone compute resource has no DRS surface and is intrinsically pinned.
func (c *Client) EnsureVMPlacementControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	if err := c.ValidateVMPlacement(ctx, vmMoref, expectedHostMoref); err != nil {
		return fmt.Errorf("%w: %v", ErrPlacementDrift, err)
	}
	switch control {
	case DRSControlStandalone:
		if computeType != "ComputeResource" {
			return fmt.Errorf(
				"%w: standalone control recorded for compute type %s",
				ErrDRSControlFailure,
				computeType,
			)
		}
		return c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref)
	case DRSControlDisabled:
		if computeType != "ClusterComputeResource" {
			return fmt.Errorf(
				"%w: disabled DRS control recorded for compute type %s",
				ErrDRSControlFailure,
				computeType,
			)
		}
	default:
		return fmt.Errorf("%w: unsupported control %q", ErrDRSControlFailure, control)
	}
	if err := c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref); err != nil {
		return fmt.Errorf("%w: %v", ErrDRSControlFailure, err)
	}

	cluster := object.NewClusterComputeResource(c.client.Client, types.ManagedObjectReference{
		Type:  computeType,
		Value: computeMoref,
	})
	config, err := cluster.Configuration(ctx)
	if err != nil {
		return fmt.Errorf("%w: read cluster configuration: %v", ErrDRSControlFailure, err)
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
		return fmt.Errorf("%w: disable DRS for VM %s: %v", ErrDRSControlFailure, vmMoref, err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("%w: wait for DRS override on VM %s: %v", ErrDRSControlFailure, vmMoref, err)
	}
	if err := c.ValidateVMPlacementControl(
		ctx,
		vmMoref,
		expectedHostMoref,
		computeType,
		computeMoref,
		control,
	); err != nil {
		return fmt.Errorf("%w: %v", ErrDRSControlFailure, err)
	}
	return nil
}

// ValidateVMPlacementControl verifies both exact host placement and the
// persisted mechanism that prevents automatic movement.
func (c *Client) ValidateVMPlacementControl(
	ctx context.Context,
	vmMoref, expectedHostMoref, computeType, computeMoref, control string,
) error {
	if err := c.ValidateVMPlacement(ctx, vmMoref, expectedHostMoref); err != nil {
		return fmt.Errorf("%w: %v", ErrPlacementDrift, err)
	}
	if err := c.validateVMComputeResource(ctx, vmMoref, computeType, computeMoref); err != nil {
		return err
	}
	switch control {
	case DRSControlStandalone:
		if computeType != "ComputeResource" {
			return fmt.Errorf("%w: expected standalone compute resource, got %s", ErrDRSControlDrift, computeType)
		}
		return nil
	case DRSControlDisabled:
		if computeType != "ClusterComputeResource" {
			return fmt.Errorf("%w: expected cluster compute resource, got %s", ErrDRSControlDrift, computeType)
		}
	default:
		return fmt.Errorf("%w: unsupported persisted control %q", ErrDRSControlDrift, control)
	}

	cluster := object.NewClusterComputeResource(c.client.Client, types.ManagedObjectReference{
		Type:  computeType,
		Value: computeMoref,
	})
	config, err := cluster.Configuration(ctx)
	if err != nil {
		return fmt.Errorf("read DRS control for VM %s: %w", vmMoref, err)
	}
	vmRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoref}
	for _, existing := range config.DrsVmConfig {
		if existing.Key != vmRef {
			continue
		}
		if existing.Enabled != nil && !*existing.Enabled {
			return nil
		}
		return fmt.Errorf("%w: VM %s DRS override is enabled", ErrDRSControlDrift, vmMoref)
	}
	return fmt.Errorf("%w: VM %s has no DRS override", ErrDRSControlDrift, vmMoref)
}

func (c *Client) validateVMComputeResource(
	ctx context.Context,
	vmMoref, expectedType, expectedMoref string,
) error {
	if expectedType == "" || expectedMoref == "" {
		return fmt.Errorf("%w: persisted compute-resource identity is incomplete", ErrPlacementDrift)
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var vmProps mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &vmProps); err != nil {
		return fmt.Errorf("read VM %s host: %w", vmMoref, err)
	}
	if vmProps.Runtime.Host == nil {
		return fmt.Errorf("%w: VM %s has no runtime host", ErrPlacementDrift, vmMoref)
	}
	host := object.NewHostSystem(c.client.Client, *vmProps.Runtime.Host)
	var hostProps mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"parent"}, &hostProps); err != nil {
		return fmt.Errorf("read VM %s compute resource: %w", vmMoref, err)
	}
	if hostProps.Parent == nil ||
		hostProps.Parent.Type != expectedType ||
		hostProps.Parent.Value != expectedMoref {
		actualType, actualMoref := "", ""
		if hostProps.Parent != nil {
			actualType = hostProps.Parent.Type
			actualMoref = hostProps.Parent.Value
		}
		return fmt.Errorf(
			"%w: VM %s compute resource is %s/%s, expected %s/%s",
			ErrPlacementDrift,
			vmMoref,
			actualType,
			actualMoref,
			expectedType,
			expectedMoref,
		)
	}
	return nil
}
