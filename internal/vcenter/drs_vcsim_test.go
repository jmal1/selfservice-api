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

func markFixtureCloneIdentity(
	t *testing.T,
	ctx context.Context,
	c *Client,
	fixture simulatorComputeFixture,
) VMCloneIdentity {
	t.Helper()
	pool, err := c.finder.ResourcePool(ctx, fixture.poolPath)
	if err != nil {
		t.Fatal(err)
	}
	markerParams := CloneVMParams{
		OperationID:          "operation-exact-cleanup",
		LogicalTemplateID:    "template-exact-cleanup",
		TemplateName:         fixture.sourceMoref,
		SourceReplicaID:      "replica-exact-cleanup",
		PodVMID:              "pod-vm-exact-cleanup",
		ComputeResourceType:  fixture.computeType,
		ComputeResourceMoRef: fixture.computeMoref,
		ResourcePoolMoRef:    pool.Reference().Value,
		HostMoRef:            fixture.sourceHost.MoRef,
	}
	extraConfig, err := cloneOperationExtraConfig(markerParams)
	if err != nil {
		t.Fatal(err)
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: fixture.sourceMoref,
	})
	task, err := vm.Reconfigure(ctx, types.VirtualMachineConfigSpec{ExtraConfig: extraConfig})
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return VMCloneIdentity{
		LogicalTemplateID:    markerParams.LogicalTemplateID,
		SourceReplicaID:      markerParams.SourceReplicaID,
		SourceRef:            markerParams.TemplateName,
		PodVMID:              markerParams.PodVMID,
		ComputeResourceType:  markerParams.ComputeResourceType,
		ComputeResourceMoref: markerParams.ComputeResourceMoRef,
		ResourcePoolMoref:    markerParams.ResourcePoolMoRef,
		HostMoref:            markerParams.HostMoRef,
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
		} else {
			var drift *PlacementDriftError
			if !errors.As(err, &drift) || drift.Kind != PlacementDriftDRS {
				t.Fatalf("enabled override error = %#v, want typed DRS drift", err)
			}
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

func TestValidateVMPlacementControlSeparatesDriftFromOperationalFailure(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)

		hostErr := c.ValidateVMPlacement(
			ctx,
			fixture.sourceMoref,
			"host-deliberately-wrong",
		)
		var hostDrift *PlacementDriftError
		if !errors.As(hostErr, &hostDrift) || hostDrift.Kind != PlacementDriftHost {
			t.Fatalf("host mismatch error = %#v, want typed host drift", hostErr)
		}
		if !errors.Is(hostErr, ErrPlacementDrift) || !errors.Is(hostErr, ErrHostNotAllowed) {
			t.Fatalf("host mismatch classification = %v, want placement drift and host-not-allowed", hostErr)
		}

		computeErr := c.ValidateVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			"domain-deliberately-wrong",
			DRSControlDisabled,
		)
		var computeDrift *PlacementDriftError
		if !errors.As(computeErr, &computeDrift) || computeDrift.Kind != PlacementDriftCompute {
			t.Fatalf("compute mismatch error = %#v, want typed compute drift", computeErr)
		}
		if !errors.Is(computeErr, ErrPlacementDrift) {
			t.Fatalf("compute mismatch error = %v, want ErrPlacementDrift", computeErr)
		}

		cancelledCtx, cancel := context.WithCancel(ctx)
		cancel()
		operationalErr := c.ValidateVMPlacementControl(
			cancelledCtx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		)
		if !errors.Is(operationalErr, context.Canceled) {
			t.Fatalf("property-read error = %v, want context cancellation", operationalErr)
		}
		if !errors.Is(operationalErr, ErrPlacementValidationUnavailable) {
			t.Fatalf("property-read error = %v, want placement-validation unavailable", operationalErr)
		}
		if errors.Is(operationalErr, ErrPlacementDrift) ||
			errors.Is(operationalErr, ErrDRSControlDrift) {
			t.Fatalf("property-read error was mislabeled as proven drift: %v", operationalErr)
		}
		var operationalDrift *PlacementDriftError
		if errors.As(operationalErr, &operationalDrift) {
			t.Fatalf("property-read error has typed drift classification: %#v", operationalDrift)
		}
	})
}

func TestDestroyVMWithPlacementFailsClosedOnIdentityDrift(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)
		identity := markFixtureCloneIdentity(t, ctx, c, fixture)
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
		err := c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			identity,
		)
		var drsDrift *PlacementDriftError
		if !errors.As(err, &drsDrift) || drsDrift.Kind != PlacementDriftDRS {
			t.Fatalf("enabled-DRS cleanup error = %#v, want typed DRS drift", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("enabled-DRS cleanup mutated VM: exists=%v err=%v", exists, existsErr)
		}
		if err := c.EnsureVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); err != nil {
			t.Fatalf("repair DRS before identity sabotage checks: %v", err)
		}

		err = c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			"host-deliberately-wrong",
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			identity,
		)
		var hostDrift *PlacementDriftError
		if !errors.As(err, &hostDrift) || hostDrift.Kind != PlacementDriftHost {
			t.Fatalf("wrong-host cleanup error = %#v, want typed host drift", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("wrong-host cleanup mutated VM: exists=%v err=%v", exists, existsErr)
		}

		err = c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			"domain-deliberately-wrong",
			DRSControlDisabled,
			identity,
		)
		var computeDrift *PlacementDriftError
		if !errors.As(err, &computeDrift) || computeDrift.Kind != PlacementDriftCompute {
			t.Fatalf("wrong-compute cleanup error = %#v, want typed compute drift", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("wrong-compute cleanup mutated VM: exists=%v err=%v", exists, existsErr)
		}

		wrongSource := identity
		wrongSource.SourceRef = "vm-source-deliberately-wrong"
		err = c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			wrongSource,
		)
		var identityDrift *PlacementDriftError
		if !errors.As(err, &identityDrift) || identityDrift.Kind != PlacementDriftIdentity {
			t.Fatalf("wrong-source cleanup error = %#v, want typed identity drift", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("wrong-source cleanup mutated VM: exists=%v err=%v", exists, existsErr)
		}

		wrongPool := identity
		wrongPool.ResourcePoolMoref = "resgroup-deliberately-wrong"
		err = c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			wrongPool,
		)
		if !errors.As(err, &identityDrift) || identityDrift.Kind != PlacementDriftIdentity {
			t.Fatalf("wrong-pool cleanup error = %#v, want typed identity drift", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("wrong-pool cleanup mutated VM: exists=%v err=%v", exists, existsErr)
		}

		if err := c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			identity,
		); err != nil {
			t.Fatalf("destroy exact-placement VM: %v", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || exists {
			t.Fatalf("exact-placement cleanup result: exists=%v err=%v", exists, existsErr)
		}
	})
}

func TestDestroyVMWithPlacementAllowsPreconfigurationCloneWithoutDRSOverride(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)
		identity := markFixtureCloneIdentity(t, ctx, c, fixture)
		if err := c.ValidateVMPlacementControl(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
		); !errors.Is(err, ErrDRSControlDrift) {
			t.Fatalf("strict forward validation error = %v, want missing-control drift", err)
		}
		if err := c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			identity,
		); err != nil {
			t.Fatalf("destroy exact preconfiguration clone: %v", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || exists {
			t.Fatalf("preconfiguration cleanup result: exists=%v err=%v", exists, existsErr)
		}
	})
}

func TestDestroyVMWithPlacementRetriesMissingDRSControlFailure(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := clusterFixture(t, ctx, c)
		identity := markFixtureCloneIdentity(t, ctx, c, fixture)
		original := c.client.RoundTripper
		c.client.RoundTripper = &failDRSReconfigure{next: original}
		defer func() { c.client.RoundTripper = original }()

		err := c.DestroyVMWithPlacement(
			ctx,
			fixture.sourceMoref,
			fixture.sourceHost.MoRef,
			fixture.computeType,
			fixture.computeMoref,
			DRSControlDisabled,
			identity,
		)
		if !errors.Is(err, ErrDRSControlFailure) ||
			!errors.Is(err, ErrPlacementValidationUnavailable) {
			t.Fatalf("cleanup DRS enforcement error = %v, want retryable control failure", err)
		}
		if errors.Is(err, ErrDRSControlDrift) || errors.Is(err, ErrPlacementDrift) {
			t.Fatalf("cleanup DRS enforcement error was mislabeled as drift: %v", err)
		}
		if exists, existsErr := c.VMExists(ctx, fixture.sourceMoref); existsErr != nil || !exists {
			t.Fatalf("failed cleanup control enforcement mutated VM: exists=%v err=%v", exists, existsErr)
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
		if !errors.Is(err, ErrPlacementValidationUnavailable) {
			t.Fatalf("DRS reconfigure error = %v, want placement-validation unavailable", err)
		}
	})
}
