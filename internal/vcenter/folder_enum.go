package vcenter

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// FolderVM is the enriched description of a VM living in a vCenter folder,
// suitable for surfacing to admin UIs that need to register Crucible templates.
type FolderVM struct {
	Name              string `json:"name"`
	MoRef             string `json:"moref"`
	PowerState        string `json:"power_state"`
	OSType            string `json:"os_type"`
	GuestFullName     string `json:"guest_full_name"`
	NumCPU            int32  `json:"num_cpu"`
	MemoryMB          int32  `json:"memory_mb"`
	DiskGB            int64  `json:"disk_gb"`
	SnapshotCount     int    `json:"snapshot_count"`
	HasSnapshot       bool   `json:"has_initial_snapshot"`
	VMwareToolsStatus string `json:"vmware_tools_status"`
}

// ListVMsInFolder enumerates VirtualMachine children of a vCenter folder by path
// (e.g. "/JMAL-Datacenter/vm/Templates") and returns enriched metadata. It uses
// a single PropertyCollector round-trip for all VMs in the folder.
func (c *Client) ListVMsInFolder(ctx context.Context, folderPath string) ([]FolderVM, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, fmt.Errorf("ensure connected: %w", err)
	}

	folder, err := c.finder.Folder(ctx, folderPath)
	if err != nil {
		return nil, fmt.Errorf("folder lookup %q: %w", folderPath, err)
	}

	children, err := folder.Children(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing folder children: %w", err)
	}

	var vmRefs []types.ManagedObjectReference
	for _, child := range children {
		ref := child.Reference()
		if ref.Type == "VirtualMachine" {
			vmRefs = append(vmRefs, ref)
		}
	}
	if len(vmRefs) == 0 {
		return []FolderVM{}, nil
	}

	var raw []mo.VirtualMachine
	pc := property.DefaultCollector(c.client.Client)
	err = pc.Retrieve(ctx, vmRefs, []string{
		"name",
		"config.hardware.numCPU",
		"config.hardware.memoryMB",
		"guest.guestFullName",
		"guest.guestFamily",
		"guest.toolsStatus",
		"runtime.powerState",
		"snapshot",
		"summary.storage.committed",
	}, &raw)
	if err != nil {
		return nil, fmt.Errorf("property collection: %w", err)
	}

	out := make([]FolderVM, 0, len(raw))
	for i := range raw {
		out = append(out, vmToFolderVM(&raw[i]))
	}
	return out, nil
}

// vmToFolderVM transforms a mo.VirtualMachine into the API-facing FolderVM
// shape, applying sensible zero-value fallbacks for fields the property
// collector may have left unset (e.g. when the VM is powered off and tools
// have never run).
func vmToFolderVM(vm *mo.VirtualMachine) FolderVM {
	out := FolderVM{
		Name:              vm.Name,
		MoRef:             vm.Reference().Value,
		PowerState:        string(vm.Runtime.PowerState),
		VMwareToolsStatus: "toolsNotInstalled",
	}

	if vm.Config != nil {
		out.NumCPU = vm.Config.Hardware.NumCPU
		out.MemoryMB = vm.Config.Hardware.MemoryMB
	}

	if vm.Guest != nil {
		out.GuestFullName = vm.Guest.GuestFullName
		out.OSType = mapOSType(vm.Guest.GuestFamily)
		if vm.Guest.ToolsStatus != "" {
			out.VMwareToolsStatus = string(vm.Guest.ToolsStatus)
		}
	}
	if out.OSType == "" {
		out.OSType = "other"
	}

	if vm.Snapshot != nil {
		out.SnapshotCount = countSnapshotTree(vm.Snapshot.RootSnapshotList)
		out.HasSnapshot = out.SnapshotCount > 0
	}

	if vm.Summary.Storage != nil {
		// Round up bytes to GB; integer arithmetic keeps it cheap.
		const gb = int64(1024 * 1024 * 1024)
		out.DiskGB = (vm.Summary.Storage.Committed + gb - 1) / gb
	}

	return out
}

func mapOSType(guestFamily string) string {
	gf := strings.ToLower(guestFamily)
	switch {
	case strings.Contains(gf, "linux"):
		return "linux"
	case strings.Contains(gf, "windows"):
		return "windows"
	case gf == "":
		return ""
	default:
		return "other"
	}
}

func countSnapshotTree(nodes []types.VirtualMachineSnapshotTree) int {
	count := 0
	for _, n := range nodes {
		count += 1 + countSnapshotTree(n.ChildSnapshotList)
	}
	return count
}
