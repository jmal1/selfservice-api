package vcenter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// VMExists resolves a VM reference (either a managed-object reference like
// "vm-8942" or a vCenter inventory name) and reports whether the VM is
// present in the current vCenter inventory.
//
// A missing VM (NotFound / ManagedObjectNotFound) is not an error — the
// function returns (false, nil). All other vCenter errors propagate as-is
// so the caller can distinguish "definitely gone" from "can't check right now".
//
// This is the cheap half of the structural health check.
func (c *Client) VMExists(ctx context.Context, ref string) (bool, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return false, err
	}

	var exists bool
	err := c.withRetry(ctx, "vm exists", func() error {
		var innerErr error
		exists, innerErr = c.vmExistsInner(ctx, ref)
		return innerErr
	})
	return exists, err
}

func (c *Client) vmExistsInner(ctx context.Context, ref string) (bool, error) {
	vm, err := c.resolveSourceVM(ctx, ref)
	if err != nil {
		if isAlreadyDeletedErr(err) || isNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("resolve VM ref %q: %w", ref, err)
	}

	// Fetch a minimal property to confirm the object is accessible, not just
	// that the reference object was constructed. resolveSourceVM with a moref
	// builds the object locally without a round-trip; Properties confirms the
	// VM actually exists in the inventory.
	//
	// The destination MUST be a mo.VirtualMachine. govmomi's
	// mo.LoadObjectContent assigns the whole managed object into the
	// destination by reflection, so an ad-hoc struct -- even one carrying the
	// right `mo:"name"` tags -- panics at runtime with
	// "reflect.Set: value of type mo.VirtualMachine is not assignable".
	// The property list still limits what is fetched over the wire.
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"name"}, &props); err != nil {
		if isAlreadyDeletedErr(err) || isNotFoundErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetch VM properties for %q: %w", ref, err)
	}
	return true, nil
}

// isNotFoundErr reports whether a govmomi error is a "not found" style error
// distinct from the "already deleted" class handled by isAlreadyDeletedErr.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// govmomi wraps both ManagedObjectNotFound and vimFault NotFound under
	// their Fault.FaultCause or Fault.Message text.
	return strings.Contains(msg, "ManagedObjectNotFound") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "Cannot find object with ID")
}

// HealthCheckCloneParams controls the deep health-check clone operation.
type HealthCheckCloneParams struct {
	// SourceRef is the template's vCenter ref (moref or inventory name).
	SourceRef string

	// CloneName is the VM name for the temporary clone, e.g.
	// "crucible-healthcheck-<template-id>". MUST start with
	// "crucible-healthcheck-" so the orphan-sweep can identify it.
	CloneName string

	// FolderPath is the vCenter folder to clone into. Should be the
	// TemplateFolder (not VMFolder) to keep health-check clones out of
	// the student-VM orphan reconciler's scan path.
	FolderPath string

	// ResourcePool is the resource pool to use for the clone.
	ResourcePool string

	// Datastore is the datastore name to use for the clone.
	Datastore string

	// Network is the port group for the clone's NIC.
	Network string

	// VCPUs / RAMmb are the clone's resources. Use the template defaults.
	VCPUs int32
	RAMmb int64
}

// HealthCheckCloneResult holds the outcome of a deep health check clone.
type HealthCheckCloneResult struct {
	// MoRef is the managed-object reference of the cloned VM. Used for cleanup.
	MoRef     string
	HostMoRef string
}

// CloneForHealthCheck creates a disposable clone of a template VM for the
// deep health check. The clone is placed in FolderPath with the name
// CloneName (which MUST start with "crucible-healthcheck-" for the orphan
// sweep to identify it). No customization is applied.
//
// The caller MUST call DestroyVM on the returned MoRef when done, whether
// the subsequent power-on / IP-wait succeeded or not. Use defer.
//
// If a VM with CloneName already exists in the target folder (e.g. a
// previous crashed check left one behind), its MoRef is returned immediately
// so the caller can destroy it during cleanup.
func (c *Client) CloneForHealthCheck(ctx context.Context, params HealthCheckCloneParams) (*HealthCheckCloneResult, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var result *HealthCheckCloneResult
	err := c.withRetry(ctx, "health check clone", func() error {
		var innerErr error
		result, innerErr = c.cloneForHealthCheckInner(ctx, params)
		return innerErr
	})
	return result, err
}

func (c *Client) cloneForHealthCheckInner(ctx context.Context, params HealthCheckCloneParams) (*HealthCheckCloneResult, error) {
	source, err := c.resolveSourceVM(ctx, params.SourceRef)
	if err != nil {
		return nil, fmt.Errorf("resolve source VM %q: %w", params.SourceRef, err)
	}

	// Determine target folder.
	folderPath := params.FolderPath
	if folderPath == "" {
		folderPath = c.config.TemplateFolder
	}
	folder, err := c.finder.Folder(ctx, folderPath)
	if err != nil {
		return nil, fmt.Errorf("find folder %q: %w", folderPath, err)
	}

	var sourceProps mo.VirtualMachine
	if err := source.Properties(ctx, source.Reference(), []string{"runtime.host"}, &sourceProps); err != nil {
		return nil, fmt.Errorf("read health-check source host: %w", err)
	}
	vcpus := params.VCPUs
	if vcpus == 0 {
		vcpus = 1
	}
	rammb := params.RAMmb
	if rammb == 0 {
		rammb = 512
	}
	dsName := params.Datastore
	if dsName == "" {
		dsName = c.config.Datastore
	}
	placementRequest := PlacementRequest{
		SourceHost:        sourceProps.Runtime.Host,
		RequireSourceHost: true,
		ResourcePoolPath:  params.ResourcePool,
		DatastoreName:     dsName,
		NetworkName:       params.Network,
		VCPUs:             vcpus,
		RAMMB:             rammb,
	}
	placement, err := c.ResolvePlacement(ctx, placementRequest)
	if err != nil {
		return nil, fmt.Errorf("select health-check placement: %w", err)
	}

	// Idempotency: reuse if a VM with this name already exists.
	if existing, lookupErr := c.findVMInFolderStrict(ctx, folder, params.CloneName); lookupErr != nil {
		return nil, fmt.Errorf("check existing health-check clone %q: %w", params.CloneName, lookupErr)
	} else if existing != "" {
		if err := c.ValidateVMPlacement(ctx, existing, ""); err != nil {
			return nil, fmt.Errorf("refuse to reuse health-check clone %s: %w", existing, err)
		}
		c.logger.Info("health-check clone already exists, reusing for cleanup",
			"name", params.CloneName, "moref", existing)
		return &HealthCheckCloneResult{MoRef: existing}, nil
	}

	pool := placement.Pool
	poolRef := pool.Reference()
	dsRef := placement.Datastore.Reference()
	hostRef := placement.Host.Reference()

	// Build a minimal full clone spec (no linked clone — health checks
	// should exercise the real disk path, and we don't want to create
	// snapshot chains on templates that don't already have them).
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:      &poolRef,
			Datastore: &dsRef,
			Host:      &hostRef,
		},
		Config: &types.VirtualMachineConfigSpec{
			NumCPUs:  vcpus,
			MemoryMB: rammb,
		},
		PowerOn:  false,
		Template: false,
	}
	sourceDevices, err := applyVTPMClonePolicy(ctx, source, &cloneSpec)
	if err != nil {
		return nil, err
	}

	// Attach to staging network if provided.
	if params.Network != "" {
		netBacking := &types.VirtualEthernetCardNetworkBackingInfo{
			VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
				DeviceName: params.Network,
			},
		}
		for _, dev := range sourceDevices {
			if nic, ok := dev.(types.BaseVirtualEthernetCard); ok {
				card := nic.GetVirtualEthernetCard()
				card.Backing = netBacking
				cloneSpec.Config.DeviceChange = append(cloneSpec.Config.DeviceChange,
					&types.VirtualDeviceConfigSpec{
						Operation: types.VirtualDeviceConfigSpecOperationEdit,
						Device:    dev,
					})
				break
			}
		}
	}

	c.logger.Info("starting health-check clone",
		"source", params.SourceRef, "name", params.CloneName,
		"folder", folderPath, "pool", placement.PoolPath,
		"host", placement.Identity.Name, "host_moref", placement.Identity.MoRef)

	placementRequest.PinnedHostMoRef = placement.Identity.MoRef
	placementRequest.PinnedPoolMoRef = placement.PoolMoRef
	if _, err := c.ResolvePlacement(ctx, placementRequest); err != nil {
		return nil, fmt.Errorf("revalidate health-check placement immediately before clone: %w", err)
	}
	task, err := source.Clone(ctx, folder, params.CloneName, cloneSpec)
	if err != nil {
		return nil, fmt.Errorf("start health-check clone: %w", err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("health-check clone task: %w", err)
	}

	vmRef := info.Result.(types.ManagedObjectReference)
	if err := c.ValidateVMPlacement(ctx, vmRef.Value, placement.Identity.MoRef); err != nil {
		return nil, fmt.Errorf("health-check clone completed on invalid host: %w", err)
	}
	c.logger.Info("health-check clone created", "name", params.CloneName, "moref", vmRef.Value)
	return &HealthCheckCloneResult{MoRef: vmRef.Value, HostMoRef: placement.Identity.MoRef}, nil
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// SweepHealthCheckOrphans destroys any VMs in folderPath whose name starts
// with HealthCheckClonePrefix. This is the orphan sweep that runs once at
// worker startup to clean up any clones left behind by a crashed check.
//
// A crashed deep check may leave a running clone consuming datastore space.
// The prefix "crucible-healthcheck-" is distinctive enough that it cannot
// match a student pod name; the sweep is therefore safe to run automatically.
const HealthCheckClonePrefix = "crucible-healthcheck-"

// SweepHealthCheckOrphans lists VMs in folderPath and destroys health-check
// clones created before cutoff. A newly elected leader can overlap a
// confirmation job already claimed by another worker, so deleting every
// prefixed VM would destroy that job's active validation artifact.
func (c *Client) SweepHealthCheckOrphans(ctx context.Context, folderPath string, cutoff time.Time) (int, error) {
	if folderPath == "" {
		folderPath = c.config.TemplateFolder
	}
	if err := c.ensureConnected(ctx); err != nil {
		return 0, err
	}

	vms, err := c.ListVMsInFolder(ctx, folderPath)
	if err != nil {
		return 0, fmt.Errorf("list VMs in folder %q: %w", folderPath, err)
	}

	var destroyed int
	for _, vm := range vms {
		if !isSweepableHealthCheckClone(vm, cutoff) {
			if strings.HasPrefix(vm.Name, HealthCheckClonePrefix) {
				c.logger.Info("retaining recent health-check clone during orphan sweep",
					"name", vm.Name, "moref", vm.MoRef, "created_at", vm.CreatedAt)
			}
			continue
		}
		c.logger.Warn("sweeping orphaned health-check clone",
			"name", vm.Name, "moref", vm.MoRef)
		if err := c.DestroyVM(ctx, vm.MoRef); err != nil {
			c.logger.Error("failed to destroy health-check orphan",
				"name", vm.Name, "moref", vm.MoRef, "error", err)
			continue
		}
		destroyed++
	}

	if destroyed > 0 {
		c.logger.Info("swept health-check orphans", "destroyed", destroyed, "folder", folderPath)
	}
	return destroyed, nil
}

func isSweepableHealthCheckClone(vm FolderVM, cutoff time.Time) bool {
	return strings.HasPrefix(vm.Name, HealthCheckClonePrefix) &&
		vm.CreatedAt != nil &&
		vm.CreatedAt.Before(cutoff)
}
