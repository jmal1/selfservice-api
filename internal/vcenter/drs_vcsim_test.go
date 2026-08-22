package vcenter

import (
	"context"
	"errors"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type failDRSReconfigure struct {
	next soap.RoundTripper
}

func (f *failDRSReconfigure) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if _, ok := req.(*methods.ReconfigureComputeResource_TaskBody); ok {
		return errors.New("simulated DRS control failure")
	}
	return f.next.RoundTrip(ctx, req, res)
}

func clusterFixture(t *testing.T, ctx context.Context, c *Client) simulatorComputeFixture {
	t.Helper()
	for _, fixture := range simulatorComputeFixtures(t, ctx, c) {
		if fixture.computeType == "ClusterComputeResource" {
			return fixture
		}
	}
	t.Fatal("vcsim did not provide a source VM in a cluster")
	return simulatorComputeFixture{}
}

func setVMDRSEnabled(
	t *testing.T,
	ctx context.Context,
	c *Client,
	fixture simulatorComputeFixture,
) {
	t.Helper()
	cluster := object.NewClusterComputeResource(c.client.Client, types.ManagedObjectReference{
		Type:  fixture.computeType,
		Value: fixture.computeMoref,
	})
	enabled := true
	task, err := cluster.Reconfigure(ctx, &types.ClusterConfigSpecEx{
		DrsVmConfigSpec: []types.ClusterDrsVmConfigSpec{{
			ArrayUpdateSpec: types.ArrayUpdateSpec{Operation: types.ArrayUpdateOperationEdit},
			Info: &types.ClusterDrsVmConfigInfo{
				Key:      types.ManagedObjectReference{Type: "VirtualMachine", Value: fixture.sourceMoref},
				Enabled:  &enabled,
				Behavior: types.DrsBehaviorFullyAutomated,
			},
		}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureVMPlacementControlRepairsDRSDrift(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)
		if err := c.EnsureVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); err != nil {
			t.Fatal(err)
		}
		setVMDRSEnabled(t, ctx, c, fixture)
		if err := c.ValidateVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); !errors.Is(err, ErrDRSControlDrift) {
			t.Fatalf("enabled override error = %v, want ErrDRSControlDrift", err)
		}
		if err := c.EnsureVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); err != nil {
			t.Fatalf("repair enabled DRS override: %v", err)
		}
		if err := c.ValidateVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); err != nil {
			t.Fatalf("validate repaired DRS override: %v", err)
		}
	})
}

func TestEnsureVMPlacementControlSurfacesDRSFailure(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)
		original := c.client.RoundTripper
		c.client.RoundTripper = &failDRSReconfigure{next: original}
		defer func() { c.client.RoundTripper = original }()

		err := c.EnsureVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		)
		if !errors.Is(err, ErrDRSControlFailure) {
			t.Fatalf("DRS reconfigure error = %v, want ErrDRSControlFailure", err)
		}
	})
}
