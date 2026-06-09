package vcenter

// template_ops.go - Template wizard (T4) vCenter operations.
//
// These differ from CloneVM (which is for student pod VMs):
//   - target folder is the Templates folder (configurable), not VMFolder
//   - port group is the staging network, not the pod VLAN
//   - NO guest customization (no cloud-init, no sysprep) — the instructor
//     handles configuration interactively via WebMKS, then we run the
//     generalize script in Phase 2
//   - we keep the resulting VM as a regular VM (Template: false) so it
//     can be powered on for interactive setup
//
// All operations are idempotent where reasonably possible: clone returns
// the existing VM's moref if one with the target name is already in the
// target folder, WaitForTools polls instead of blocking forever, etc.

import (
	"context"
	"fmt"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TemplateCloneParams holds parameters for cloning a source VM into the
// Templates folder for the wizard. The source can be either an existing
// vCenter template (in which case it must be marked as template) or a
// regular VM (which produces a full clone).
type TemplateCloneParams struct {
	// SourceMoref is the moref of the source VM/template. Required.
	SourceMoref string

	// VMName is the desired name of the new VM. Required.
	// Use the convention tpl-{template-name}-{6-char-hex}.
	VMName string

	// FolderPath is the inventory path of the target folder.
	// Example: "JMAL-Datacenter/vm/Templates". If empty, defaults to
	// the same folder as the source VM.
	FolderPath string

	// Network is the port group name for the staging NIC. Required.
	// Example: "LabVMs-VLAN30".
	Network string

	// VCPUs / RAMmb override the source's CPU and memory. If zero, the
	// source's values are preserved.
	VCPUs int32
	RAMmb int64
}

// CloneTemplateSourceVM clones a source VM into the templates folder.
// Unlike CloneVM, this does NOT inject cloud-init data — the resulting
// VM is exactly the source contents with the requested hardware
// adjustments and a NIC on the staging network. Returns the new VM's
// moref.
//
// If a VM with the requested name already exists in the target folder
// (e.g. a previous wizard run was interrupted before recording the
// moref), returns that VM's moref unchanged so callers can resume.
func (c *Client) CloneTemplateSourceVM(ctx context.Context, params TemplateCloneParams) (string, error) {
	if params.SourceMoref == "" {
		return "", fmt.Errorf("source moref required")
	}
	if params.VMName == "" {
		return "", fmt.Errorf("VM name required")
	}
	if params.Network == "" {
		return "", fmt.Errorf("network (port group) required")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	var moref string
	err := c.withRetry(ctx, "clone template source VM", func() error {
		var inner error
		moref, inner = c.cloneTemplateSourceVMInner(ctx, params)
		return inner
	})
	return moref, err
}

func (c *Client) cloneTemplateSourceVMInner(ctx context.Context, params TemplateCloneParams) (string, error) {
	source := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: params.SourceMoref})

	// Pre-fetch source properties so we can default the folder/resource
	// selection if the caller didn't specify, and so we can decide which
	// DiskMoveType to request (depends on whether the source is a VM
	// template marker and whether it has a snapshot chain).
	var sourceProps mo.VirtualMachine
	if err := source.Properties(ctx, source.Reference(),
		[]string{"name", "parent", "resourcePool", "runtime", "snapshot", "config.template"}, &sourceProps); err != nil {
		return "", fmt.Errorf("read source VM properties: %w", err)
	}

	// Resolve target folder. If FolderPath was empty, default to the
	// source VM's current folder so the new template sits alongside it.
	var folder *object.Folder
	if params.FolderPath != "" {
		f, err := c.finder.Folder(ctx, params.FolderPath)
		if err != nil {
			return "", fmt.Errorf("find folder %s: %w", params.FolderPath, err)
		}
		folder = f
	} else if sourceProps.Parent != nil {
		folder = object.NewFolder(c.client.Client, *sourceProps.Parent)
	} else {
		return "", fmt.Errorf("source VM has no parent folder; FolderPath must be provided")
	}

	// Idempotency: if a VM with this name already exists in the target
	// folder, return it. The wizard may be resumed after a worker crash.
	if existing, err := c.findVMInFolder(ctx, folder, params.VMName); err == nil && existing != "" {
		c.logger.Info("template VM already exists, reusing", "name", params.VMName, "moref", existing)
		return existing, nil
	}

	// Pick a resource pool. Reuse selectBestPool with the same CPU/RAM
	// requirements we'll apply post-clone.
	vcpus := params.VCPUs
	if vcpus == 0 {
		// Fall back to "any" by passing 1 (the smallest reasonable value).
		vcpus = 1
	}
	rammb := params.RAMmb
	if rammb == 0 {
		rammb = 1024
	}
	pool, err := c.selectBestPool(ctx, vcpus, rammb)
	if err != nil {
		return "", fmt.Errorf("select resource pool: %w", err)
	}
	poolRef := pool.Reference()

	// Find datastore. Use the configured one for now; future work could
	// let the instructor pick.
	ds, err := c.finder.Datastore(ctx, c.config.Datastore)
	if err != nil {
		return "", fmt.Errorf("find datastore %s: %w", c.config.Datastore, err)
	}
	dsRef := ds.Reference()

	folderRef := folder.Reference()
	hasSnapshot := sourceProps.Snapshot != nil && sourceProps.Snapshot.CurrentSnapshot != nil
	isVCenterTemplate := sourceProps.Config != nil && sourceProps.Config.Template
	diskMoveType := chooseTemplateCloneDiskMoveType(hasSnapshot, isVCenterTemplate)
	c.logger.Info("template source clone disk strategy",
		"source", params.SourceMoref,
		"has_snapshot", hasSnapshot,
		"is_template", isVCenterTemplate,
		"disk_move_type", diskMoveType)
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Datastore:    &dsRef,
			Folder:       &folderRef,
			Pool:         &poolRef,
			DiskMoveType: diskMoveType,
		},
		PowerOn:  false, // we power on after hardware + NIC are configured
		Template: false, // keep as regular VM so it can be edited
	}

	// If the caller wants different hardware, set ConfigSpec. We do this
	// in the clone in a single shot rather than a follow-up Reconfigure
	// to halve the vCenter round-trips.
	if params.VCPUs > 0 || params.RAMmb > 0 {
		cloneSpec.Config = &types.VirtualMachineConfigSpec{}
		if params.VCPUs > 0 {
			cloneSpec.Config.NumCPUs = params.VCPUs
		}
		if params.RAMmb > 0 {
			cloneSpec.Config.MemoryMB = params.RAMmb
		}
	}

	task, err := source.Clone(ctx, folder, params.VMName, cloneSpec)
	if err != nil {
		return "", fmt.Errorf("clone source VM %s → %s: %w", params.SourceMoref, params.VMName, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("wait clone task: %w", err)
	}
	newRef, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return "", fmt.Errorf("clone task returned unexpected result type %T", info.Result)
	}
	c.logger.Info("template source VM cloned", "source", params.SourceMoref, "new", newRef.Value, "name", params.VMName)
	return newRef.Value, nil
}

// chooseTemplateCloneDiskMoveType picks the vCenter DiskMoveType for the
// template-wizard clone call based on the source VM's shape.
//
// vCenter is picky about how disks are moved during a clone, and it
// reports the same misleading error for two opposite mistakes:
// "The virtual disk is either corrupted or not a supported format."
//
//   - Source is a vCenter-marked Template (Config.Template == true), or
//     a regular VM with NO snapshot chain → leave DiskMoveType empty.
//     vCenter defaults to a plain full copy of the base disk. Setting
//     MoveAllDiskBackingsAndConsolidate against either of these returns
//     the "corrupted" error.
//   - Source is a regular VM WITH snapshots (Crucible templates auto-get
//     a `linked-clone-base` snapshot the first time they're used to
//     spawn a student pod) → request
//     MoveAllDiskBackingsAndConsolidate. Without it, vCenter rejects the
//     clone with the same "corrupted" error because the snapshot chain
//     can't be copied as-is to an independent destination.
//
// Pulled out as a small pure helper so we can unit-test the matrix
// without booting a vcsim VPX.
func chooseTemplateCloneDiskMoveType(hasSnapshot, isVCenterTemplate bool) string {
	if hasSnapshot && !isVCenterTemplate {
		return string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndConsolidate)
	}
	return ""
}


// inventory name via the finder. Used by the template wizard's
// clone_template branch: the API stores the *Crucible* template UUID in
// templates.source_ref, but vCenter clones need a real MoRef. We resolve
// the source template row's vcenter_template (a name) to a MoRef here.
//
// Returns a descriptive error if the VM is not found or the lookup
// fails. The returned moref is suitable for passing as
// TemplateCloneParams.SourceMoref.
func (c *Client) ResolveVMByName(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("vm name required")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	var moref string
	err := c.withRetry(ctx, "resolve VM by name", func() error {
		vm, inner := c.finder.VirtualMachine(ctx, name)
		if inner != nil {
			return fmt.Errorf("find VM %q: %w", name, inner)
		}
		moref = vm.Reference().Value
		return nil
	})
	return moref, err
}

// findVMInFolder returns the moref of a VM matching `name` within
// `folder`, or "" if none is found. Used for the clone idempotency
// check above. Errors only on unexpected vCenter failures, not on
// "no such VM" (which returns "" nil to signal "go ahead and clone").
func (c *Client) findVMInFolder(ctx context.Context, folder *object.Folder, name string) (string, error) {
	children, err := folder.Children(ctx)
	if err != nil {
		return "", err
	}
	for _, child := range children {
		vm, ok := child.(*object.VirtualMachine)
		if !ok {
			continue
		}
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"name"}, &props); err != nil {
			continue
		}
		if props.Name == name {
			return vm.Reference().Value, nil
		}
	}
	return "", nil
}

// WaitForTools polls a VM until VMware Tools reports running, or the
// timeout elapses. Returns a typed error so the worker can surface a
// helpful message to the instructor (most common cause: VM lacks open-vm-tools
// or vmware-tools is masked/disabled).
//
// Uses GuestState property which goes "notRunning" → "running" once tools
// install + start. Polls every 5s.
func (c *Client) WaitForTools(ctx context.Context, moref string, timeout time.Duration) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	for {
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"guest"}, &props); err != nil {
			return fmt.Errorf("read guest props on %s: %w", moref, err)
		}
		if props.Guest != nil && props.Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsRunning) {
			c.logger.Info("VMware Tools running", "moref", moref)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for VMware Tools on %s (current status: %q)",
				timeout, moref, toolsStatusString(props.Guest))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func toolsStatusString(g *types.GuestInfo) string {
	if g == nil {
		return "unknown"
	}
	return g.ToolsRunningStatus
}

// AttachNetworkAdapter replaces (or adds) the first NIC on a VM with one
// connected to the named port group. Used by template_provision when the
// staging network differs from the source's default NIC. Idempotent: if
// the first NIC is already on the requested port group, returns nil.
//
// Returns an error if there are zero NICs (we don't add one — that's a
// design decision: templates without NICs are unusable as student VMs,
// so the source must have a NIC already).
func (c *Client) AttachNetworkAdapter(ctx context.Context, moref, network string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	return c.withRetry(ctx, "attach NIC", func() error {
		return c.attachNetworkAdapterInner(ctx, moref, network)
	})
}

func (c *Client) attachNetworkAdapterInner(ctx context.Context, moref, network string) error {
	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
	devices, err := vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("list devices on %s: %w", moref, err)
	}
	nics := devices.SelectByType((*types.VirtualEthernetCard)(nil))
	if len(nics) == 0 {
		return fmt.Errorf("VM %s has no NICs; template source must have at least one NIC", moref)
	}
	// Take the first NIC; reconfigure it to the staging network.
	first := nics[0]
	pg, err := c.finder.Network(ctx, network)
	if err != nil {
		return fmt.Errorf("find network %s: %w", network, err)
	}
	backing, err := pg.EthernetCardBackingInfo(ctx)
	if err != nil {
		return fmt.Errorf("get NIC backing for %s: %w", network, err)
	}
	nic, ok := first.(types.BaseVirtualEthernetCard)
	if !ok {
		return fmt.Errorf("first NIC is not a VirtualEthernetCard")
	}
	card := nic.GetVirtualEthernetCard()
	card.Backing = backing

	spec := &types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device:    nic.(types.BaseVirtualDevice),
			},
		},
	}
	task, err := vm.Reconfigure(ctx, *spec)
	if err != nil {
		return fmt.Errorf("reconfigure NIC on %s: %w", moref, err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait NIC reconfigure on %s: %w", moref, err)
	}
	c.logger.Info("attached NIC to staging network", "moref", moref, "network", network)
	return nil
}
