package vcenter

import (
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestMapOSType(t *testing.T) {
	cases := map[string]string{
		"linuxGuest":      "linux",
		"otherLinux":      "linux",
		"windowsGuest":    "windows",
		"OtherWindowsX":   "windows",
		"darwinGuest":     "other",
		"netwareGuest":    "other",
		"":                "",
		"  Linux Family ": "linux",
	}
	for in, want := range cases {
		got := mapOSType(in)
		if got != want {
			t.Errorf("mapOSType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCountSnapshotTree(t *testing.T) {
	// Build a snapshot tree:
	// root1
	//   ├─ child1a
	//   └─ child1b
	//        └─ grandchild
	// root2
	tree := []types.VirtualMachineSnapshotTree{
		{
			Name: "root1",
			ChildSnapshotList: []types.VirtualMachineSnapshotTree{
				{Name: "child1a"},
				{
					Name: "child1b",
					ChildSnapshotList: []types.VirtualMachineSnapshotTree{
						{Name: "grandchild"},
					},
				},
			},
		},
		{Name: "root2"},
	}
	if got, want := countSnapshotTree(tree), 5; got != want {
		t.Errorf("countSnapshotTree = %d, want %d", got, want)
	}

	if got := countSnapshotTree(nil); got != 0 {
		t.Errorf("countSnapshotTree(nil) = %d, want 0", got)
	}
}

func TestVMToFolderVM_FullyPopulated(t *testing.T) {
	vm := &mo.VirtualMachine{
		ManagedEntity: mo.ManagedEntity{
			ExtensibleManagedObject: mo.ExtensibleManagedObject{
				Self: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-123"},
			},
			Name: "student-ubuntu-2404",
		},
		Config: &types.VirtualMachineConfigInfo{
			Hardware: types.VirtualHardware{NumCPU: 4, MemoryMB: 8192},
		},
		Guest: &types.GuestInfo{
			GuestFullName: "Ubuntu 24.04 LTS (64-bit)",
			GuestFamily:   "linuxGuest",
			ToolsStatus:   types.VirtualMachineToolsStatusToolsOk,
		},
		Runtime: types.VirtualMachineRuntimeInfo{PowerState: types.VirtualMachinePowerStatePoweredOff},
		Snapshot: &types.VirtualMachineSnapshotInfo{
			RootSnapshotList: []types.VirtualMachineSnapshotTree{
				{Name: "initial"},
			},
		},
		Summary: types.VirtualMachineSummary{
			Storage: &types.VirtualMachineStorageSummary{Committed: int64(40) * 1024 * 1024 * 1024},
		},
	}

	got := vmToFolderVM(vm)
	if got.Name != "student-ubuntu-2404" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.MoRef != "vm-123" {
		t.Errorf("MoRef = %q", got.MoRef)
	}
	if got.PowerState != "poweredOff" {
		t.Errorf("PowerState = %q", got.PowerState)
	}
	if got.OSType != "linux" {
		t.Errorf("OSType = %q", got.OSType)
	}
	if got.NumCPU != 4 || got.MemoryMB != 8192 {
		t.Errorf("Hardware = (%d, %d)", got.NumCPU, got.MemoryMB)
	}
	if got.DiskGB != 40 {
		t.Errorf("DiskGB = %d, want 40", got.DiskGB)
	}
	if !got.HasSnapshot || got.SnapshotCount != 1 {
		t.Errorf("Snapshot = (%v, %d)", got.HasSnapshot, got.SnapshotCount)
	}
	if got.VMwareToolsStatus != "toolsOk" {
		t.Errorf("ToolsStatus = %q", got.VMwareToolsStatus)
	}
}

func TestVMToFolderVM_MissingOptionalFields(t *testing.T) {
	// Powered-off, tools never run, no snapshots — exercises every nil-guard.
	vm := &mo.VirtualMachine{
		ManagedEntity: mo.ManagedEntity{
			ExtensibleManagedObject: mo.ExtensibleManagedObject{
				Self: types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-999"},
			},
			Name: "barebones",
		},
		Runtime: types.VirtualMachineRuntimeInfo{PowerState: types.VirtualMachinePowerStatePoweredOff},
	}
	got := vmToFolderVM(vm)
	if got.OSType != "other" {
		t.Errorf("OSType fallback = %q, want \"other\"", got.OSType)
	}
	if got.HasSnapshot || got.SnapshotCount != 0 {
		t.Errorf("Snapshot zero-value broken")
	}
	if got.DiskGB != 0 {
		t.Errorf("DiskGB = %d, want 0", got.DiskGB)
	}
	if got.VMwareToolsStatus != "toolsNotInstalled" {
		t.Errorf("ToolsStatus fallback = %q", got.VMwareToolsStatus)
	}
	if got.NumCPU != 0 || got.MemoryMB != 0 {
		t.Errorf("Hardware zero-value broken")
	}
}

func TestVMToFolderVM_DiskGBRoundsUp(t *testing.T) {
	vm := &mo.VirtualMachine{
		Summary: types.VirtualMachineSummary{
			Storage: &types.VirtualMachineStorageSummary{Committed: 1024 * 1024 * 1024 * 10 / 3}, // ~3.33 GiB
		},
	}
	got := vmToFolderVM(vm)
	if got.DiskGB != 4 {
		t.Errorf("DiskGB round-up = %d, want 4", got.DiskGB)
	}
}
