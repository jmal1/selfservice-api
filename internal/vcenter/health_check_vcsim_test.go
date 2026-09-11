package vcenter

import (
	"context"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25"
)

func TestHealthCheckOrphanSweepRetainsActiveAndUnknownAgeClones(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-5 * time.Minute)
	cutoff := now.Add(-time.Hour)

	tests := []struct {
		name string
		vm   FolderVM
		want bool
	}{
		{
			name: "old health clone",
			vm:   FolderVM{Name: HealthCheckClonePrefix + "old", CreatedAt: &old},
			want: true,
		},
		{
			name: "active health clone",
			vm:   FolderVM{Name: HealthCheckClonePrefix + "active", CreatedAt: &recent},
		},
		{
			name: "unknown creation time is retained",
			vm:   FolderVM{Name: HealthCheckClonePrefix + "unknown"},
		},
		{
			name: "unrelated old VM",
			vm:   FolderVM{Name: "student-vm", CreatedAt: &old},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSweepableHealthCheckClone(tt.vm, cutoff); got != tt.want {
				t.Fatalf("isSweepableHealthCheckClone() = %v, want %v", got, tt.want)
			}
		})
	}
}

// VMExists is the cheap half of the template structural health check, and it
// panicked in production the first time it was ever executed:
//
//	panic: reflect.Set: value of type mo.VirtualMachine is not assignable to
//	       type struct { Name string "mo:\"name\"" }
//
// The destination passed to (*object.VirtualMachine).Properties had been an
// ad-hoc struct with `mo:"name"` tags. That looks right, and compiles, but
// govmomi's mo.LoadObjectContent assigns the *whole* managed object into the
// destination by reflection, so the destination type must be mo.VirtualMachine.
//
// It reached production because nothing ever called it: the template health
// reconciler could not fire (its only trigger was a 12h ticker reset by every
// deploy). The moment a catch-up pass made it run, every provision-worker
// replica panicked, and because leadership then failed over to the next
// replica the crash walked through all four in turn -- provisioning was down
// until template health was disabled.
//
// These tests exercise the real govmomi property path against vcsim, which is
// the only thing that would have caught it: the panic is inside govmomi's
// reflection, so it is invisible to compilation, vet, and any fake client.
func TestVMExists_VCsim_ExistingVMByMoref(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		_, moref := firstVM(t, ctx, vimc)

		// A panic here fails the test rather than the process, which is the
		// whole point: this call previously took down the worker.
		exists, err := c.VMExists(ctx, moref)
		if err != nil {
			t.Fatalf("VMExists(%q) returned an error: %v", moref, err)
		}
		if !exists {
			t.Errorf("VMExists(%q) = false, want true for a VM the simulator created", moref)
		}
	})
}

func TestVMExists_VCsim_ExistingVMByName(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		vm, _ := firstVM(t, ctx, vimc)
		name := vm.Name()

		exists, err := c.VMExists(ctx, name)
		if err != nil {
			t.Fatalf("VMExists(%q) returned an error: %v", name, err)
		}
		if !exists {
			t.Errorf("VMExists(%q) = false, want true", name)
		}
	})
}

// A missing VM must be reported as (false, nil), not as an error. The health
// reconciler distinguishes "template is gone" from "vCenter is unreachable",
// and collapsing the two would either alert on a healthy outage or silently
// pass a deleted template.
func TestVMExists_VCsim_MissingVMIsNotAnError(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		for _, ref := range []string{"vm-does-not-exist-9999", "no-such-template-name"} {
			exists, err := c.VMExists(ctx, ref)
			if err != nil {
				t.Errorf("VMExists(%q) = error %v, want (false, nil) for a missing VM", ref, err)
			}
			if exists {
				t.Errorf("VMExists(%q) = true, want false", ref)
			}
		}
	})
}
