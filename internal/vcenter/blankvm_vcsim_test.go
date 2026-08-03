package vcenter

// vcsim coverage for CreateBlankVM / DetachCDROMs (Epic B1 — the ISO-template
// flow's blank-shell builder). Unlike the clone paths, CreateBlankVM assembles
// a VM from scratch, so these tests read the created VM's config back from the
// simulator and assert on real device properties (controller/NIC/disk types,
// thin capacity, CD-ROM ISO backings, boot order) rather than just err == nil.
//
// vcsim's CreateVMTask applies GuestId, Firmware and BootOptions verbatim
// (see simulator/virtual_machine.go apply/configureDevices), resolves an
// ethernet NIC's DeviceName backing to a real network reference, and preserves
// a disk's CapacityInKB, so every property below is genuinely simulated — no
// assertion is skipped.
//
// The shared harness (withSimulator, firstVM), the inventory constants
// (simDatastore, simResourcePool, simNetwork) and countVMs live in the other
// *_vcsim_test.go files in this package and are reused here.

import (
	"context"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// simVMFolder is the VM folder in vcsim's default VPX inventory (same value the
// CloneVM idempotency test wires onto the Client).
const simVMFolder = "/DC0/vm"

// baseBlankVMParams returns a valid BlankVMParams wired to the vcsim inventory.
// Individual tests mutate one field to exercise a specific behavior.
func baseBlankVMParams(name string) BlankVMParams {
	return BlankVMParams{
		VMName:       name,
		FolderPath:   simVMFolder,
		Datastore:    simDatastore,
		ResourcePool: simResourcePool,
		Network:      simNetwork,
		GuestID:      "ubuntu64Guest",
		VCPUs:        2,
		RAMmb:        2048,
		DiskGB:       20,
		ISOPath:      "[" + simDatastore + "] ISOs/ubuntu-24.04.iso",
	}
}

func readVMConfig(t *testing.T, ctx context.Context, vimc *vim25.Client, moref string) *types.VirtualMachineConfigInfo {
	t.Helper()
	vm := object.NewVirtualMachine(vimc, types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
	var mvm mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config"}, &mvm); err != nil {
		t.Fatalf("read VM config for %s: %v", moref, err)
	}
	if mvm.Config == nil {
		t.Fatalf("VM %s has nil config", moref)
	}
	return mvm.Config
}

// cdromISOPaths returns the ISO backing FileName of every CD-ROM on the VM, in
// device order.
func cdromISOPaths(devices object.VirtualDeviceList) []string {
	var paths []string
	for _, cd := range devices.SelectByType((*types.VirtualCdrom)(nil)) {
		if b, ok := cd.(*types.VirtualCdrom).Backing.(*types.VirtualCdromIsoBackingInfo); ok {
			paths = append(paths, b.FileName)
		}
	}
	return paths
}

// assertCdromBeforeDisk verifies the VM's boot order lists a CD-ROM ahead of
// the system disk so the installer runs on first power-on.
func assertCdromBeforeDisk(t *testing.T, cfg *types.VirtualMachineConfigInfo) {
	t.Helper()
	if cfg.BootOptions == nil {
		t.Fatal("VM has nil BootOptions; expected a boot order")
	}
	order := cfg.BootOptions.BootOrder
	if len(order) == 0 {
		t.Fatal("BootOrder is empty; expected CD-ROM before disk")
	}
	cdromIdx, diskIdx := -1, -1
	for i, bd := range order {
		switch bd.(type) {
		case *types.VirtualMachineBootOptionsBootableCdromDevice:
			if cdromIdx == -1 {
				cdromIdx = i
			}
		case *types.VirtualMachineBootOptionsBootableDiskDevice:
			if diskIdx == -1 {
				diskIdx = i
			}
		}
	}
	if cdromIdx == -1 {
		t.Fatalf("BootOrder %#v has no CD-ROM entry", order)
	}
	if diskIdx == -1 {
		t.Fatalf("BootOrder %#v has no disk entry", order)
	}
	if cdromIdx > diskIdx {
		t.Errorf("BootOrder lists disk (index %d) before CD-ROM (index %d); CD-ROM must boot first", diskIdx, cdromIdx)
	}
}

func TestCreateBlankVM_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		before := countVMs(t, ctx, vimc)

		p := baseBlankVMParams("blank-ubuntu")
		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}
		if !strings.HasPrefix(moref, "vm-") {
			t.Errorf("moref %q does not look like a VM reference (vm-NNN)", moref)
		}
		if after := countVMs(t, ctx, vimc); after != before+1 {
			t.Fatalf("VM count = %d, want %d (create should add exactly one VM)", after, before+1)
		}

		cfg := readVMConfig(t, ctx, vimc, moref)

		if cfg.GuestId != "ubuntu64Guest" {
			t.Errorf("GuestId = %q, want %q", cfg.GuestId, "ubuntu64Guest")
		}
		if cfg.Firmware != string(types.GuestOsDescriptorFirmwareTypeEfi) {
			t.Errorf("Firmware = %q, want %q (EFI is the default)", cfg.Firmware, "efi")
		}

		devices := object.VirtualDeviceList(cfg.Hardware.Device)

		if scsi := devices.SelectByType((*types.ParaVirtualSCSIController)(nil)); len(scsi) != 1 {
			t.Errorf("ParaVirtual SCSI controller count = %d, want 1", len(scsi))
		}
		if nics := devices.SelectByType((*types.VirtualVmxnet3)(nil)); len(nics) != 1 {
			t.Errorf("VMXNET3 NIC count = %d, want 1", len(nics))
		}

		disks := devices.SelectByType((*types.VirtualDisk)(nil))
		if len(disks) != 1 {
			t.Fatalf("disk count = %d, want 1", len(disks))
		}
		disk := disks[0].(*types.VirtualDisk)
		wantKB := int64(p.DiskGB) * 1024 * 1024
		if disk.CapacityInKB != wantKB {
			t.Errorf("disk CapacityInKB = %d, want %d (%d GB)", disk.CapacityInKB, wantKB, p.DiskGB)
		}
		backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			t.Fatalf("disk backing type = %T, want *types.VirtualDiskFlatVer2BackingInfo", disk.Backing)
		}
		if backing.ThinProvisioned == nil || !*backing.ThinProvisioned {
			t.Errorf("disk ThinProvisioned = %v, want true", backing.ThinProvisioned)
		}

		assertCdromBeforeDisk(t, cfg)
	})
}

func TestCreateBlankVM_TwoCDROMs(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		p := baseBlankVMParams("blank-two-cdroms")
		p.ISOPath = "[" + simDatastore + "] ISOs/installer.iso"
		p.SeedISOPath = "[" + simDatastore + "] ISOs/seed.iso"

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}

		cfg := readVMConfig(t, ctx, vimc, moref)
		devices := object.VirtualDeviceList(cfg.Hardware.Device)

		paths := cdromISOPaths(devices)
		if len(paths) != 2 {
			t.Fatalf("CD-ROM count = %d, want exactly 2 (backings=%v)", len(paths), paths)
		}
		got := map[string]bool{}
		for _, pth := range paths {
			got[pth] = true
		}
		if !got[p.ISOPath] {
			t.Errorf("CD-ROM backings %v missing installer ISO %q", paths, p.ISOPath)
		}
		if !got[p.SeedISOPath] {
			t.Errorf("CD-ROM backings %v missing seed ISO %q", paths, p.SeedISOPath)
		}
	})
}

func TestCreateBlankVM_OneCDROMWhenNoSeed(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		p := baseBlankVMParams("blank-one-cdrom")
		p.SeedISOPath = "" // explicit: no seed ISO -> exactly one CD-ROM

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}

		cfg := readVMConfig(t, ctx, vimc, moref)
		devices := object.VirtualDeviceList(cfg.Hardware.Device)

		paths := cdromISOPaths(devices)
		if len(paths) != 1 {
			t.Fatalf("CD-ROM count = %d, want exactly 1 (backings=%v)", len(paths), paths)
		}
		if paths[0] != p.ISOPath {
			t.Errorf("CD-ROM backing = %q, want %q", paths[0], p.ISOPath)
		}
	})
}

func TestCreateBlankVM_BiosFirmware(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		p := baseBlankVMParams("blank-bios")
		p.Firmware = "bios"

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}

		cfg := readVMConfig(t, ctx, vimc, moref)
		if cfg.Firmware != string(types.GuestOsDescriptorFirmwareTypeBios) {
			t.Errorf("Firmware = %q, want %q", cfg.Firmware, "bios")
		}
	})
}

func TestCreateBlankVM_RejectsBadParams(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *BlankVMParams)
		errSub string
	}{
		{"empty name", func(p *BlankVMParams) { p.VMName = "" }, "VM name required"},
		{"empty ISO", func(p *BlankVMParams) { p.ISOPath = "" }, "ISO path required"},
		{"zero disk", func(p *BlankVMParams) { p.DiskGB = 0 }, "disk size"},
		{"negative disk", func(p *BlankVMParams) { p.DiskGB = -5 }, "disk size"},
		{"zero vcpus", func(p *BlankVMParams) { p.VCPUs = 0 }, "vCPU"},
		{"negative vcpus", func(p *BlankVMParams) { p.VCPUs = -1 }, "vCPU"},
		{"zero ram", func(p *BlankVMParams) { p.RAMmb = 0 }, "RAM"},
		{"negative ram", func(p *BlankVMParams) { p.RAMmb = -512 }, "RAM"},
		{"empty guest id", func(p *BlankVMParams) { p.GuestID = "" }, "guest ID required"},
		{"malformed ISO path", func(p *BlankVMParams) { p.ISOPath = "LocalDS_0 ISOs/x.iso" }, "invalid datastore path"},
		{"malformed seed ISO path", func(p *BlankVMParams) { p.SeedISOPath = "no-brackets.iso" }, "invalid datastore path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
				before := countVMs(t, ctx, vimc)

				p := baseBlankVMParams("blank-bad-" + strings.ReplaceAll(tc.name, " ", "-"))
				tc.mutate(&p)

				moref, err := c.CreateBlankVM(ctx, p)
				if err == nil {
					t.Fatalf("CreateBlankVM(%s) = %q, want error", tc.name, moref)
				}
				if moref != "" {
					t.Errorf("moref = %q, want empty on rejection", moref)
				}
				if !strings.Contains(err.Error(), tc.errSub) {
					t.Errorf("error %q should mention %q", err.Error(), tc.errSub)
				}
				if after := countVMs(t, ctx, vimc); after != before {
					t.Errorf("VM count changed from %d to %d; a rejected request must not create a VM", before, after)
				}
			})
		})
	}
}

func TestDetachCDROMs_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		p := baseBlankVMParams("blank-detach")
		p.SeedISOPath = "[" + simDatastore + "] ISOs/seed.iso"

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}

		// Precondition: the shell was created with two CD-ROMs, a disk and a NIC.
		cfg := readVMConfig(t, ctx, vimc, moref)
		devices := object.VirtualDeviceList(cfg.Hardware.Device)
		if n := len(devices.SelectByType((*types.VirtualCdrom)(nil))); n != 2 {
			t.Fatalf("precondition: CD-ROM count = %d, want 2", n)
		}

		if err := c.DetachCDROMs(ctx, moref); err != nil {
			t.Fatalf("DetachCDROMs: %v", err)
		}

		cfg = readVMConfig(t, ctx, vimc, moref)
		devices = object.VirtualDeviceList(cfg.Hardware.Device)

		if n := len(devices.SelectByType((*types.VirtualCdrom)(nil))); n != 0 {
			t.Errorf("CD-ROM count after detach = %d, want 0", n)
		}
		// Detaching CD-ROMs must not strip the rest of the hardware.
		if n := len(devices.SelectByType((*types.VirtualDisk)(nil))); n != 1 {
			t.Errorf("disk count after detach = %d, want 1 (detach must not touch the disk)", n)
		}
		if n := len(devices.SelectByType((*types.VirtualVmxnet3)(nil))); n != 1 {
			t.Errorf("NIC count after detach = %d, want 1 (detach must not touch the NIC)", n)
		}

		// Idempotent: a second call on a VM with no CD-ROMs is a no-op.
		if err := c.DetachCDROMs(ctx, moref); err != nil {
			t.Fatalf("DetachCDROMs (second call) should be a no-op, got: %v", err)
		}
	})
}

// --- Placement defaults (the bug that broke the first real ISO template build) ---
//
// Every test above hands CreateBlankVM an explicit FolderPath and Datastore.
// Production does the opposite: nothing builds a TemplateProvisionPayload with
// folder_path or a datastore, so the first live source_type=iso provision died
// with `find folder "": folder '' not found` before creating anything. The
// tests passed the whole time because they supplied the very inputs production
// never supplies.
//
// These tests pin the defaults by leaving the fields EMPTY — the production
// shape — so the gap cannot reopen.

func TestCreateBlankVM_DefaultsFolderToTemplateFolder(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		// TemplateFolder is what the worker sets from VCENTER_TEMPLATES_FOLDER.
		c.config.TemplateFolder = simVMFolder
		c.config.Datastore = simDatastore
		// VMFolder deliberately points somewhere invalid: a template shell must
		// never land in the student-pod folder, which the orphan reconciler
		// scans. If the fallback order regresses, this fails rather than
		// quietly parking templates where they get reported as orphans.
		c.config.VMFolder = "/DC0/vm/does-not-exist"

		p := baseBlankVMParams("blank-default-folder")
		p.FolderPath = ""
		p.Datastore = ""

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM with empty FolderPath/Datastore should fall back to config, got: %v", err)
		}
		if moref == "" {
			t.Fatal("CreateBlankVM returned an empty moref")
		}

		// Prove it actually landed in the template folder rather than merely
		// not erroring.
		vm := object.NewVirtualMachine(vimc, types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		var mvm mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"parent", "name"}, &mvm); err != nil {
			t.Fatalf("read VM parent: %v", err)
		}
		if mvm.Parent == nil {
			t.Fatal("created VM has no parent folder")
		}
		wantFolder, err := c.finder.Folder(ctx, simVMFolder)
		if err != nil {
			t.Fatalf("resolve expected folder: %v", err)
		}
		if mvm.Parent.Value != wantFolder.Reference().Value {
			t.Errorf("VM parent folder = %s, want %s (TemplateFolder)", mvm.Parent.Value, wantFolder.Reference().Value)
		}
	})
}

func TestCreateBlankVM_ExplicitFolderWinsOverConfig(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		// A caller that names a folder must still be honored; the defaults are
		// a fallback, not an override.
		c.config.TemplateFolder = "/DC0/vm/does-not-exist"
		c.config.Datastore = "no-such-datastore"

		p := baseBlankVMParams("blank-explicit-folder")
		if _, err := c.CreateBlankVM(ctx, p); err != nil {
			t.Fatalf("explicit FolderPath/Datastore must take precedence over config, got: %v", err)
		}
	})
}

func TestCreateBlankVM_UnresolvableFolderErrorNamesTheFix(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		// When nothing resolves, the operator needs to know WHICH knob to turn.
		// The original message was `find folder "": folder '' not found`, which
		// named no config key and sent the reader looking for a caller bug.
		c.config.TemplateFolder = ""
		c.config.VMFolder = ""
		c.config.Datastore = simDatastore

		p := baseBlankVMParams("blank-no-folder")
		p.FolderPath = ""
		p.Datastore = ""

		_, err := c.CreateBlankVM(ctx, p)
		if err == nil {
			t.Fatal("CreateBlankVM with no resolvable folder should fail")
		}
		for _, want := range []string{"FolderPath", "TemplateFolder", "VMFolder"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q so the operator knows what to configure", err, want)
			}
		}
	})
}