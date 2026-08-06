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

	// Pick a resource pool. For template clones we MUST stay in the same
	// cluster as the source VM — cross-cluster clone-from-snapshot is
	// rejected by vCenter with the misleading "virtual disk is either
	// corrupted or not a supported format" error (most likely CPU-vendor
	// compatibility validation: Intel→AMD, or vice versa, on a guest
	// with cpuid masks). Verified by `govc vm.clone -pool <source-cluster>`
	// succeeding for the same source where `-pool <other-cluster>` fails
	// in 300ms with the same error.
	vcpus := params.VCPUs
	if vcpus == 0 {
		// Fall back to "any" by passing 1 (the smallest reasonable value).
		vcpus = 1
	}
	rammb := params.RAMmb
	if rammb == 0 {
		rammb = 1024
	}
	pool, err := c.selectBestPoolInSourceCluster(ctx, sourceProps.Runtime.Host, vcpus, rammb)
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
	c.logger.Info("template source clone strategy",
		"source", params.SourceMoref,
		"has_snapshot", hasSnapshot,
		"is_template", isVCenterTemplate)
	// CloneSpec intentionally minimal: leave both `Snapshot` and
	// `DiskMoveType` unset. This matches what `govc vm.clone` does by
	// default and what the vCenter UI does when you right-click
	// "Clone to Virtual Machine" — vCenter copies the full disk chain
	// (including any snapshot deltas) into an independent VM at the
	// target datastore, with no source-side consolidation required and
	// no dependency on the source snapshot's child-clone count.
	//
	// History (this code has been wrong three times — keep the comment):
	//   * Round 2 set DiskMoveType=MoveAllDiskBackingsAndConsolidate to
	//     "fix" a misdiagnosed "virtual disk is either corrupted or not
	//     a supported format" error. It actually masked a different
	//     bug and introduced its own failures.
	//   * Round 6 made the DiskMoveType conditional on hasSnapshot, on
	//     the (also wrong) theory that consolidation was required for
	//     snapshot-bearing sources.
	//   * Round 8 dropped DiskMoveType but set Snapshot=CurrentSnapshot,
	//     which silently turns the clone into a linked-clone-style
	//     "moveChildMostDiskBacking" operation that can't produce an
	//     independent full copy and fails in ~300ms.
	//
	// `govc vm.clone` against student-windows-11 (a regular VM with a
	// 3-deep snapshot chain and active linked-clone children) succeeds
	// with neither flag set, so the minimal spec is the right call.
	//
	//   * Round 11 (2026-08-03): the "virtual disk is either corrupted or
	//     not a supported format" fault was proven INTERMITTENT and
	//     ENVIRONMENTAL — not a property of the source VM or the CloneSpec.
	//     Evidence: job d62777e7 cloned student-ubuntu-2404 at 20:36:11
	//     and failed; job a3ba9028 cloned the same source with identical
	//     parameters at 20:37:19 and succeeded (68 seconds later). The
	//     same clone run by hand with `govc` against two different
	//     datastores also succeeded. vCenter's event log showed the failed
	//     task erroring in ~1 s (a fast validation rejection, not an I/O
	//     timeout). The fix is job-level retries with exponential backoff
	//     (internal/provisioner/retryable.go), NOT another CloneSpec change.
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Datastore: &dsRef,
			Folder:    &folderRef,
			Pool:      &poolRef,
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

// ResolveVMByName returns the moref ("vm-NNN") of a VM looked up by its
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

	for {
		// Re-derive the VM handle from the current client on every poll and
		// wrap the read in withRetry: the session token can expire mid-loop
		// (or a concurrent op can reconnect and swap out c.client), which
		// otherwise surfaces a raw NotAuthenticated fault that gets
		// misreported to the instructor as "VMware Tools not running".
		var props mo.VirtualMachine
		if err := c.withRetry(ctx, "read guest props", func() error {
			vm := object.NewVirtualMachine(c.client.Client,
				types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
			return vm.Properties(ctx, vm.Reference(), []string{"guest"}, &props)
		}); err != nil {
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

// WaitForPowerOff polls a VM until it reports poweredOff, or the timeout
// elapses.
//
// This is the completion signal for an ISO install, and it exists because the
// obvious signal is wrong. The Ubuntu live-server installer ISO runs
// open-vm-tools in the *ephemeral installer* environment, so WaitForTools
// returns roughly 40 seconds after power-on — before the installer has written
// a single byte to the disk. Using Tools to mean "the install finished"
// produced a template whose disk was empty while every state transition
// reported success, which is a far worse failure than a timeout.
//
// The generated autoinstall sets "shutdown: poweroff" precisely so that this
// transition happens once, at a point that can only be reached after curtin has
// finished writing the target system.
func (c *Client) WaitForPowerOff(ctx context.Context, moref string, timeout time.Duration) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)

	for {
		// Same reconnect-safety as WaitForTools: an install can run for the
		// better part of an hour, which comfortably outlives a session token.
		var props mo.VirtualMachine
		if err := c.withRetry(ctx, "read runtime props", func() error {
			vm := object.NewVirtualMachine(c.client.Client,
				types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
			return vm.Properties(ctx, vm.Reference(), []string{"runtime"}, &props)
		}); err != nil {
			return fmt.Errorf("read runtime props on %s: %w", moref, err)
		}
		if props.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOff {
			c.logger.Info("VM powered off", "moref", moref)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s to power off (current state: %q)",
				timeout, moref, props.Runtime.PowerState)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
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
	// Construct the NIC backing directly by name rather than going through
	// c.finder.Network(). The finder only sees vCenter-level networks
	// (Distributed Virtual Port Groups, opaque networks), not per-host
	// standard vSwitch port groups like "PG-VM-Lab" — which is what
	// the rest of the Crucible plumbing uses. The student-pod clone path
	// (client.go cloneAndCustomize) does the same thing for the same
	// reason. vCenter resolves the DeviceName on the target host when the
	// Reconfigure task lands, so as long as the port group exists on the
	// host where vm/{moref} lives, this works.
	backing := &types.VirtualEthernetCardNetworkBackingInfo{
		VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
			DeviceName: network,
		},
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

// BlankVMParams holds parameters for CreateBlankVM. Unlike the clone paths
// above, this builds an empty VM from scratch — there is no source template
// to clone from. That is the whole point of the ISO-template flow: the
// instructor boots an OS installer ISO into a blank shell, installs
// interactively via WebMKS, then we generalize the result into a template.
type BlankVMParams struct {
	// VMName is the desired inventory name of the new VM. Required.
	VMName string
	// FolderPath is the inventory path of the target folder (e.g.
	// "JMAL-Datacenter/vm/Templates").
	FolderPath string
	// Datastore holds the VM's files and its system disk (e.g. "NAS-vmstore").
	Datastore string
	// ResourcePool is the inventory path of the target pool. If empty, the
	// datacenter's default resource pool is used.
	ResourcePool string
	// Network is the port group name for the VMXNET3 NIC (e.g. "LabVMs-VLAN30").
	Network string

	// GuestID is the vSphere guest OS identifier, e.g. "ubuntu64Guest",
	// "debian12_64Guest", "windows11_64Guest". It selects sensible device
	// defaults and installer behavior.
	GuestID string

	// VCPUs / RAMmb / DiskGB size the shell. All must be > 0.
	VCPUs  int32
	RAMmb  int64
	DiskGB int

	// Firmware is "efi" (default when empty) or "bios".
	Firmware string

	// ISOPath is the installer ISO in "[datastore] path/file.iso" form. It
	// becomes CD-ROM 0 and the first boot device. Required.
	ISOPath string
	// SeedISOPath is an optional second ISO (e.g. a cloud-init / autounattend
	// seed) in "[datastore] path/file.iso" form. When non-empty it becomes
	// CD-ROM 1.
	SeedISOPath string
}

// CreateBlankVM creates an empty VM configured to boot an OS installer ISO and
// returns the new VM's moref. The shell has a SCSI controller with a single
// thin system disk, a VMXNET3 NIC on the requested port group, and one or two
// IDE CD-ROMs (ISOPath, and SeedISOPath when provided) with the CD-ROM ahead of
// the disk in the boot order so the installer runs on first power-on.
//
// The SCSI controller type depends on the guest: Windows Server guests get an
// LSI Logic SAS controller (the Windows in-box installer has no pvscsi driver,
// so a pvscsi disk is invisible to Setup — see isWindowsServerGuestID and
// docs/architecture/iso-build-hardware-gaps.md Gap B); everything else keeps the
// higher-performance pvscsi controller.
//
// All validation happens before any vCenter round-trip, so a rejected request
// never leaves an orphaned VM behind.
func (c *Client) CreateBlankVM(ctx context.Context, p BlankVMParams) (string, error) {
	if p.VMName == "" {
		return "", fmt.Errorf("VM name required")
	}
	if p.ISOPath == "" {
		return "", fmt.Errorf("ISO path required")
	}
	if p.DiskGB <= 0 {
		return "", fmt.Errorf("disk size must be greater than 0 GB (got %d)", p.DiskGB)
	}
	if p.VCPUs <= 0 {
		return "", fmt.Errorf("vCPU count must be greater than 0 (got %d)", p.VCPUs)
	}
	if p.RAMmb <= 0 {
		return "", fmt.Errorf("RAM must be greater than 0 MB (got %d)", p.RAMmb)
	}
	// vSphere requires a guest OS identifier on create. Catching it here keeps
	// the promise made above: an empty GuestID would otherwise surface as an
	// opaque InvalidArgument fault from vCenter well after the caller has
	// committed to the request.
	if p.GuestID == "" {
		return "", fmt.Errorf("guest ID required (e.g. \"ubuntu64Guest\", \"debian12_64Guest\", \"windows11_64Guest\")")
	}
	// The ISO paths come from operator input through the upload wizard, so
	// validate their "[datastore] path" form here — before we create the VM —
	// rather than letting a malformed backing surface as an opaque vCenter
	// fault after the shell already exists.
	if _, _, err := ParseDatastorePath(p.ISOPath); err != nil {
		return "", fmt.Errorf("ISO path: %w", err)
	}
	if p.SeedISOPath != "" {
		if _, _, err := ParseDatastorePath(p.SeedISOPath); err != nil {
			return "", fmt.Errorf("seed ISO path: %w", err)
		}
	}

	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	var moref string
	err := c.withRetry(ctx, "create blank VM", func() error {
		var inner error
		moref, inner = c.createBlankVMInner(ctx, p)
		return inner
	})
	return moref, err
}

func (c *Client) createBlankVMInner(ctx context.Context, p BlankVMParams) (string, error) {
	// Placement defaults. ResourcePool (below) and Firmware have always
	// defaulted when unset; FolderPath and Datastore did not, and that
	// asymmetry was a live bug: every production caller leaves FolderPath
	// empty (nothing builds a TemplateProvisionPayload with folder_path), so
	// the very first real ISO template build failed with
	// `find folder "": folder '' not found` — an opaque message that names
	// no config key and points at no fix. The blank-VM path cannot borrow the
	// clone path's trick of inheriting the source VM's parent folder, because
	// it has no source VM.
	folderPath := p.FolderPath
	if folderPath == "" {
		folderPath = c.config.TemplateFolder
	}
	if folderPath == "" {
		// Last resort so a half-configured deployment still lands the VM
		// somewhere real instead of erroring out on an empty path.
		folderPath = c.config.VMFolder
	}
	folder, err := c.finder.Folder(ctx, folderPath)
	if err != nil {
		return "", fmt.Errorf("find folder %q (set BlankVMParams.FolderPath or vcenter TemplateFolder/VMFolder config): %w", folderPath, err)
	}

	datastore := p.Datastore
	if datastore == "" {
		datastore = c.config.Datastore
	}
	ds, err := c.finder.Datastore(ctx, datastore)
	if err != nil {
		return "", fmt.Errorf("find datastore %q (set BlankVMParams.Datastore or vcenter Datastore config): %w", datastore, err)
	}
	dsRef := ds.Reference()

	pool, err := c.resolvePlacementPool(ctx, p.ResourcePool, p.VCPUs, p.RAMmb)
	if err != nil {
		return "", err
	}

	firmware := p.Firmware
	if firmware == "" {
		firmware = string(types.GuestOsDescriptorFirmwareTypeEfi)
	}

	devices, err := blankVMDevices(dsRef, p)
	if err != nil {
		return "", err
	}
	deviceChange, err := devices.ConfigSpec(types.VirtualDeviceConfigSpecOperationAdd)
	if err != nil {
		return "", fmt.Errorf("build device change spec: %w", err)
	}

	spec := types.VirtualMachineConfigSpec{
		Name:         p.VMName,
		GuestId:      p.GuestID,
		NumCPUs:      p.VCPUs,
		MemoryMB:     p.RAMmb,
		Firmware:     firmware,
		DeviceChange: deviceChange,
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s]", ds.Name()),
		},
		// Boot the installer media first, falling back to the (empty) system
		// disk once the OS is installed and the ISO is detached.
		BootOptions: &types.VirtualMachineBootOptions{
			BootOrder: devices.BootOrder([]string{
				object.DeviceTypeCdrom,
				object.DeviceTypeDisk,
			}),
		},
	}

	task, err := folder.CreateVM(ctx, spec, pool, nil)
	if err != nil {
		return "", fmt.Errorf("create VM %q: %w", p.VMName, err)
	}
	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("wait create VM task: %w", err)
	}
	newRef, ok := info.Result.(types.ManagedObjectReference)
	if !ok {
		return "", fmt.Errorf("create VM task returned unexpected result type %T", info.Result)
	}
	c.logger.Info("blank VM created", "name", p.VMName, "moref", newRef.Value, "guest_id", p.GuestID)
	return newRef.Value, nil
}

// blankVMDevices assembles the device list for a blank installer VM: a SCSI
// controller with a thin system disk, an IDE controller carrying the CD-ROM(s),
// and a VMXNET3 NIC. It uses govmomi's VirtualDeviceList builders (the same
// idiom as `govc vm.create`) so controller keys and unit numbers are assigned
// consistently.
//
// The SCSI controller kind is chosen from the guest OS: Windows Server guests
// get "lsilogic-sas" because the Windows in-box installer ships no pvscsi driver
// (VMware KB 1010398), so a pvscsi system disk is invisible to Setup ("We
// couldn't find any drives."). The LSI SAS driver is in-box on every supported
// Windows Server release, so Setup sees the disk with no manual Load-driver
// step. Linux and Windows client guests keep the faster "pvscsi" controller.
func blankVMDevices(dsRef types.ManagedObjectReference, p BlankVMParams) (object.VirtualDeviceList, error) {
	var devices object.VirtualDeviceList

	scsiType := "pvscsi"
	if isWindowsServerGuestID(p.GuestID) {
		scsiType = "lsilogic-sas"
	}
	scsi, err := devices.CreateSCSIController(scsiType)
	if err != nil {
		return nil, fmt.Errorf("create %s controller: %w", scsiType, err)
	}
	devices = append(devices, scsi)

	// IDE controller to host the CD-ROM(s). One controller has two slots,
	// which is exactly enough for the installer ISO and the optional seed ISO.
	ide, err := devices.CreateIDEController()
	if err != nil {
		return nil, fmt.Errorf("create IDE controller: %w", err)
	}
	devices = append(devices, ide)

	// Thin system disk on the SCSI controller. CreateDisk already produces a
	// VirtualDiskFlatVer2BackingInfo with ThinProvisioned=true and a persistent
	// disk mode; we set the capacity and re-assert thin explicitly so the
	// contract is obvious at the call site.
	disk := devices.CreateDisk(scsi.(types.BaseVirtualController), dsRef, "")
	disk.CapacityInKB = int64(p.DiskGB) * 1024 * 1024
	if backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
		backing.ThinProvisioned = types.NewBool(true)
	}
	devices = append(devices, disk)

	// VMXNET3 NIC. Build the backing directly by port group name (like
	// AttachNetworkAdapter above and the student-pod clone path) because
	// finder.Network cannot see per-host standard vSwitch port groups; vCenter
	// resolves the DeviceName on the target host when the create task lands.
	nicBacking := &types.VirtualEthernetCardNetworkBackingInfo{
		VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
			DeviceName: p.Network,
		},
	}
	nic, err := devices.CreateEthernetCard("vmxnet3", nicBacking)
	if err != nil {
		return nil, fmt.Errorf("create vmxnet3 NIC: %w", err)
	}
	devices = append(devices, nic)

	// CD-ROM 0: the installer ISO and first boot device.
	cdrom0, err := devices.CreateCdrom(ide.(types.BaseVirtualController))
	if err != nil {
		return nil, fmt.Errorf("create CD-ROM: %w", err)
	}
	devices.InsertIso(cdrom0, p.ISOPath)
	devices = append(devices, cdrom0)

	// CD-ROM 1: optional seed ISO (cloud-init / autounattend media).
	if p.SeedISOPath != "" {
		cdrom1, err := devices.CreateCdrom(ide.(types.BaseVirtualController))
		if err != nil {
			return nil, fmt.Errorf("create seed CD-ROM: %w", err)
		}
		devices.InsertIso(cdrom1, p.SeedISOPath)
		devices = append(devices, cdrom1)
	}

	return devices, nil
}

// isWindowsServerGuestID reports whether guestID is a Windows Server vSphere
// guest identifier. Windows Server ISO builds need an LSI Logic SAS controller
// rather than pvscsi so the in-box installer can see the system disk (see
// blankVMDevices and docs/architecture/iso-build-hardware-gaps.md Gap B).
//
// The identifiers below are the 64-bit Windows Server values from govmomi's
// vim25/types enum (VirtualMachineGuestOsIdentifier):
//
//	windows9Server64Guest      Windows Server 2016
//	windows2019srv_64Guest     Windows Server 2019
//	windows2019srvNext_64Guest Windows Server 2022
//	windows2022srvNext_64Guest Windows Server 2025
//
// Windows client guests (windows9_64Guest = Win10, windows11_64Guest = Win11)
// and every non-Windows guest are deliberately excluded: they stay on pvscsi and
// rely on the manual Load-driver fallback until Option 1 (driver staging) lands.
func isWindowsServerGuestID(guestID string) bool {
	switch guestID {
	case "windows9Server64Guest", // Windows Server 2016
		"windows2019srv_64Guest",     // Windows Server 2019
		"windows2019srvNext_64Guest", // Windows Server 2022
		"windows2022srvNext_64Guest": // Windows Server 2025
		return true
	default:
		return false
	}
}

// DetachCDROMs removes every CD-ROM device from the VM identified by moref.
// A template that still has an ISO attached holds a lock on that datastore
// file, which blocks deleting or replacing the ISO later — so the finalize
// step detaches all CD-ROMs before the template is sealed. Idempotent: a VM
// that already has no CD-ROMs is left unchanged.
func (c *Client) DetachCDROMs(ctx context.Context, moref string) error {
	if moref == "" {
		return fmt.Errorf("moref required")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	return c.withRetry(ctx, "detach CD-ROMs", func() error {
		return c.detachCDROMsInner(ctx, moref)
	})
}

func (c *Client) detachCDROMsInner(ctx context.Context, moref string) error {
	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
	devices, err := vm.Device(ctx)
	if err != nil {
		return fmt.Errorf("list devices on %s: %w", moref, err)
	}
	cdroms := devices.SelectByType((*types.VirtualCdrom)(nil))
	if len(cdroms) == 0 {
		return nil
	}
	changes := make([]types.BaseVirtualDeviceConfigSpec, 0, len(cdroms))
	for _, cd := range cdroms {
		changes = append(changes, &types.VirtualDeviceConfigSpec{
			Operation: types.VirtualDeviceConfigSpecOperationRemove,
			Device:    cd,
		})
	}
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{DeviceChange: changes})
	if err != nil {
		return fmt.Errorf("reconfigure to remove CD-ROMs on %s: %w", moref, err)
	}
	if err := task.Wait(ctx); err != nil {
		return fmt.Errorf("wait remove CD-ROMs on %s: %w", moref, err)
	}
	c.logger.Info("detached CD-ROMs", "moref", moref, "count", len(cdroms))
	return nil
}
