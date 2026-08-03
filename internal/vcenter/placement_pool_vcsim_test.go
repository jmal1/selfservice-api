package vcenter

// vcsim coverage for resolvePlacementPool — the placement fallback used by the
// two paths that have no source VM to inherit from: CreateBlankVM (ISO
// templates) and ImportOVA.
//
// These tests exist because the original fallback went straight to
// finder.DefaultResourcePool, and no production caller sets ResourcePool. On a
// datacenter with more than one compute resource that call cannot choose, and
// the first real ISO template build failed with "default resource pool
// resolves to multiple instances, please specify" — an error naming no config
// key and no caller.
//
// vcsim's VPX model gives us exactly that shape for free: a standalone host
// (DC0_H0/Resources) alongside a cluster (DC0_C0/Resources), so the ambiguity
// is genuinely simulated rather than mocked. TestDefaultResourcePool_IsAmbiguous
// asserts that premise, so if vcsim ever ships a model with a single compute
// resource these guards fail loudly instead of passing vacuously.

import (
	"context"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// vmResourcePool returns the moref of the resource pool a VM was placed in.
func vmResourcePool(t *testing.T, ctx context.Context, vimc *vim25.Client, moref string) types.ManagedObjectReference {
	t.Helper()
	vm := object.NewVirtualMachine(vimc, types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
	var mvm mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"resourcePool"}, &mvm); err != nil {
		t.Fatalf("read resourcePool for %s: %v", moref, err)
	}
	if mvm.ResourcePool == nil {
		t.Fatalf("VM %s has no resourcePool", moref)
	}
	return *mvm.ResourcePool
}

// TestDefaultResourcePool_IsAmbiguous asserts the premise of every other test
// in this file. If this ever passes, the simulator no longer reproduces the
// production condition and the guards below stop proving anything.
func TestDefaultResourcePool_IsAmbiguous(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		_, err := c.finder.DefaultResourcePool(ctx)
		if err == nil {
			t.Fatal("simulator's DefaultResourcePool resolved cleanly; this harness no longer " +
				"reproduces the multi-cluster ambiguity these tests guard against — " +
				"add a second compute resource to the model")
		}
		if !strings.Contains(err.Error(), "multiple") {
			t.Fatalf("expected a multiple-instances error, got %v", err)
		}
	})
}

func TestResolvePlacementPool_FallsBackToConfiguredPools(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		// Exactly the production shape: nothing passes a pool, the deployment
		// configures them.
		c.config.ResourcePools = []string{simResourcePool}

		pool, err := c.resolvePlacementPool(ctx, "", 2, 2048)
		if err != nil {
			t.Fatalf("resolvePlacementPool with configured pools: %v", err)
		}

		want, err := c.finder.ResourcePool(ctx, simResourcePool)
		if err != nil {
			t.Fatalf("find %s: %v", simResourcePool, err)
		}
		if pool.Reference() != want.Reference() {
			t.Fatalf("placed in %v, want the configured pool %v", pool.Reference(), want.Reference())
		}
	})
}

func TestResolvePlacementPool_ExplicitPathWins(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		// A bogus configured pool proves the explicit path is not merely
		// agreeing with the fallback by coincidence.
		c.config.ResourcePools = []string{"/DC0/host/does-not-exist/Resources"}

		pool, err := c.resolvePlacementPool(ctx, simResourcePool, 2, 2048)
		if err != nil {
			t.Fatalf("resolvePlacementPool with explicit path: %v", err)
		}
		want, err := c.finder.ResourcePool(ctx, simResourcePool)
		if err != nil {
			t.Fatalf("find %s: %v", simResourcePool, err)
		}
		if pool.Reference() != want.Reference() {
			t.Fatalf("explicit path ignored: got %v, want %v", pool.Reference(), want.Reference())
		}
	})
}

// TestResolvePlacementPool_ErrorNamesTheConfigKey guards the diagnosability of
// the failure, not just the failure. The message this replaced said only
// "specify ResourcePool", which is not a thing an operator can set anywhere.
func TestResolvePlacementPool_ErrorNamesTheConfigKey(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		c.config.ResourcePools = nil

		_, err := c.resolvePlacementPool(ctx, "", 2, 2048)
		if err == nil {
			t.Fatal("expected an error with no explicit pool and none configured")
		}
		if !strings.Contains(err.Error(), "VCENTER_RESOURCE_POOLS") {
			t.Fatalf("error must name the config key an operator can actually set; got: %v", err)
		}
	})
}

// TestCreateBlankVM_NoResourcePoolUsesConfigured is the end-to-end version:
// the exact call the ISO template flow makes, which failed in production.
func TestCreateBlankVM_NoResourcePoolUsesConfigured(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		c.config.ResourcePools = []string{simResourcePool}

		p := baseBlankVMParams("iso-shell-no-pool")
		p.ResourcePool = "" // no production caller sets this

		moref, err := c.CreateBlankVM(ctx, p)
		if err != nil {
			t.Fatalf("CreateBlankVM without an explicit resource pool: %v", err)
		}

		want, err := c.finder.ResourcePool(ctx, simResourcePool)
		if err != nil {
			t.Fatalf("find %s: %v", simResourcePool, err)
		}
		if got := vmResourcePool(t, ctx, vimc, moref); got != want.Reference() {
			t.Fatalf("VM landed in pool %v, want the configured %v", got, want.Reference())
		}
	})
}
