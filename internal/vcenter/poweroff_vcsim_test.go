package vcenter

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

// TestWaitForPowerOff_vcsim proves the ISO path's install-complete signal works
// against a real (simulated) vCenter in both directions: it returns promptly
// once the VM is off, and it does NOT return early while the VM is still
// running the installer.
//
// This exists because the signal it replaces was wrong in the silent direction.
// The Ubuntu live-server ISO runs open-vm-tools in the ephemeral installer, so
// WaitForTools returned ~40s after power-on with an empty disk; a completion
// check that fires too early produces a template that looks provisioned and has
// no OS.
func TestWaitForPowerOff_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		moref, err := c.CreateBlankVM(ctx, baseBlankVMParams("poweroff-probe"))
		if err != nil {
			t.Fatalf("CreateBlankVM: %v", err)
		}
		vm := object.NewVirtualMachine(vimc, types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		t.Run("returns once the VM is powered off", func(t *testing.T) {
			// A freshly created blank VM is already off, which is the state the
			// installer reaches via "shutdown: poweroff".
			if err := c.WaitForPowerOff(ctx, moref, 30*time.Second); err != nil {
				t.Fatalf("WaitForPowerOff on an already-off VM: %v", err)
			}
		})

		t.Run("does not return while the VM is still running", func(t *testing.T) {
			task, err := vm.PowerOn(ctx)
			if err != nil {
				t.Fatalf("PowerOn: %v", err)
			}
			if err := task.Wait(ctx); err != nil {
				t.Fatalf("PowerOn wait: %v", err)
			}
			t.Cleanup(func() {
				if off, err := vm.PowerOff(ctx); err == nil {
					_ = off.Wait(ctx)
				}
			})

			// The whole point of this call is that it keeps waiting while an
			// install is in progress. Give it a deadline short enough to keep
			// the test fast and assert it timed out rather than reporting the
			// install finished.
			err = c.WaitForPowerOff(ctx, moref, 1*time.Second)
			if err == nil {
				t.Fatal("WaitForPowerOff returned nil for a powered-ON VM: an install still in " +
					"progress would be treated as finished, and the installer media would be " +
					"detached out from under it, producing an empty-disk template")
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Errorf("error = %v, want a timeout error naming the current power state", err)
			}
			if !strings.Contains(err.Error(), string(types.VirtualMachinePowerStatePoweredOn)) {
				t.Errorf("timeout error %q should report the observed power state so an operator "+
					"can tell a stalled install from a missing VM", err)
			}
		})
	})
}
