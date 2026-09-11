package vcenter

// vcsim coverage for the pre-power-on disk probe (ProbeSystemDiskReadable) and
// the one-shot repair (RecreateSystemDisk) that back the ISO-template flow's
// "GET before power on" fix.
//
// vcsim's VirtualDiskManager.QueryVirtualDiskUuid only os.Stat()s the resolved
// backing file — it cannot simulate a genuinely 0-byte/truncated flat that
// opens with a "not a virtual disk" fault. So these tests can only prove the
// happy path here: a freshly created blank VM probes readable, and
// RecreateSystemDisk swaps the disk for a fresh one at the requested capacity
// while leaving the controllers, NIC and CD-ROMs intact. The broken-disk
// decision logic (probe faults -> recreate -> re-probe) is proven at the
// provisioner layer in template_jobs_test.go, where a fake can return the
// disk-not-ready fault the simulator cannot.
//
// The shared harness (withSimulator, firstVM, countVMs), inventory constants
// (simDatastore, simResourcePool, simNetwork, simVMFolder) and baseBlankVMParams
// live in the other *_vcsim_test.go files in this package and are reused here.

import (
	"context"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

func TestProbeSystemDiskReadable_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		moref, err := c.CreateBlankVM(ctx, baseBlankVMParams("probe-ok"))
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}
		// The freshly created VM's flat exists in the simulator's datastore, so
		// the disk open must succeed with no power-on.
		if err := c.ProbeSystemDiskReadable(ctx, moref); err != nil {
			t.Fatalf("ProbeSystemDiskReadable on a healthy disk = %v, want nil", err)
		}
	})
}

func TestProbeSystemDiskReadable_UnknownMoref(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		// A missing VM is not a disk-not-ready condition: the probe must surface
		// the lookup failure rather than report the disk broken (which would send
		// the provisioner into a pointless recreate).
		err := c.ProbeSystemDiskReadable(ctx, "vm-does-not-exist")
		if err == nil {
			t.Fatal("expected an error probing a nonexistent VM")
		}
		if IsDiskNotReadyErr(err) {
			t.Fatalf("a missing VM must not be classified disk-not-ready: %v", err)
		}
	})
}

func TestRecreateSystemDisk_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		p := baseBlankVMParams("recreate-disk")
		p.SeedISOPath = "[" + simDatastore + "] ISOs/seed.iso" // exercise two CD-ROMs
		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}

		before := readVMConfig(t, ctx, vimc, moref)
		beforeDevices := object.VirtualDeviceList(before.Hardware.Device)
		wantSCSI := len(beforeDevices.SelectByType((*types.ParaVirtualSCSIController)(nil)))
		wantNIC := len(beforeDevices.SelectByType((*types.VirtualVmxnet3)(nil)))
		wantCDROM := len(beforeDevices.SelectByType((*types.VirtualCdrom)(nil)))

		vmCountBefore := countVMs(t, ctx, vimc)

		if err := c.RecreateSystemDisk(ctx, moref, p.DiskGB); err != nil {
			t.Fatalf("RecreateSystemDisk: %v", err)
		}

		// The VM itself must survive — this is an in-place disk swap, not a
		// destroy-and-rebuild.
		if after := countVMs(t, ctx, vimc); after != vmCountBefore {
			t.Fatalf("VM count changed from %d to %d; recreate must not add or remove a VM", vmCountBefore, after)
		}

		afterCfg := readVMConfig(t, ctx, vimc, moref)
		afterDevices := object.VirtualDeviceList(afterCfg.Hardware.Device)

		disks := afterDevices.SelectByType((*types.VirtualDisk)(nil))
		if len(disks) != 1 {
			t.Fatalf("disk count after recreate = %d, want exactly 1", len(disks))
		}
		disk := disks[0].(*types.VirtualDisk)
		wantKB := int64(p.DiskGB) * 1024 * 1024
		if disk.CapacityInKB != wantKB {
			t.Errorf("recreated disk CapacityInKB = %d, want %d (%d GB)", disk.CapacityInKB, wantKB, p.DiskGB)
		}
		backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
		if !ok {
			t.Fatalf("recreated disk backing type = %T, want *types.VirtualDiskFlatVer2BackingInfo", disk.Backing)
		}
		if backing.ThinProvisioned == nil || !*backing.ThinProvisioned {
			t.Errorf("recreated disk ThinProvisioned = %v, want true", backing.ThinProvisioned)
		}

		// Everything that is NOT the system disk must be preserved.
		if got := len(afterDevices.SelectByType((*types.ParaVirtualSCSIController)(nil))); got != wantSCSI {
			t.Errorf("SCSI controller count after recreate = %d, want %d (unchanged)", got, wantSCSI)
		}
		if got := len(afterDevices.SelectByType((*types.VirtualVmxnet3)(nil))); got != wantNIC {
			t.Errorf("NIC count after recreate = %d, want %d (unchanged)", got, wantNIC)
		}
		if got := len(afterDevices.SelectByType((*types.VirtualCdrom)(nil))); got != wantCDROM {
			t.Errorf("CD-ROM count after recreate = %d, want %d (unchanged)", got, wantCDROM)
		}

		// And the recreated disk must probe readable.
		if err := c.ProbeSystemDiskReadable(ctx, moref); err != nil {
			t.Fatalf("ProbeSystemDiskReadable after recreate = %v, want nil", err)
		}
	})
}

func TestRecreateSystemDisk_RejectsBadInput(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		if err := c.RecreateSystemDisk(ctx, "", 20); err == nil {
			t.Error("expected an error for an empty moref")
		}
		moref, err := c.CreateBlankVM(ctx, baseBlankVMParams("recreate-bad-gb"))
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}
		if err := c.RecreateSystemDisk(ctx, moref, 0); err == nil {
			t.Error("expected an error for diskGB <= 0")
		}
	})
}
