package vcenter

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
)

type simulatorComputeFixture struct {
	computeType  string
	computeMoref string
	poolPath     string
	sourceMoref  string
	sourceHost   HostIdentity
}

func simulatorComputeFixtures(t *testing.T, ctx context.Context, c *Client) []simulatorComputeFixture {
	t.Helper()
	pools, err := c.finder.ResourcePoolList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	poolByCompute := make(map[string]string)
	for _, pool := range pools {
		var props mo.ResourcePool
		if err := pool.Properties(ctx, pool.Reference(), []string{"owner"}, &props); err != nil {
			t.Fatal(err)
		}
		if props.Owner.Value != "" && strings.HasSuffix(pool.InventoryPath, "/Resources") {
			poolByCompute[props.Owner.Value] = pool.InventoryPath
		}
	}

	hosts, err := c.resolvedHosts()
	if err != nil {
		t.Fatal(err)
	}
	hostByMoref := make(map[string]HostIdentity, len(hosts))
	for _, host := range hosts {
		hostByMoref[host.MoRef] = host
	}

	vms, err := c.finder.VirtualMachineList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	fixtureByCompute := make(map[string]simulatorComputeFixture)
	for _, vm := range vms {
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &props); err != nil {
			t.Fatal(err)
		}
		if props.Runtime.Host == nil {
			continue
		}
		host, ok := hostByMoref[props.Runtime.Host.Value]
		if !ok || poolByCompute[host.ComputeMoRef] == "" {
			continue
		}
		if _, exists := fixtureByCompute[host.ComputeMoRef]; exists {
			continue
		}
		fixtureByCompute[host.ComputeMoRef] = simulatorComputeFixture{
			computeType:  host.ComputeType,
			computeMoref: host.ComputeMoRef,
			poolPath:     poolByCompute[host.ComputeMoRef],
			sourceMoref:  vm.Reference().Value,
			sourceHost:   host,
		}
	}
	fixtures := make([]simulatorComputeFixture, 0, len(fixtureByCompute))
	for _, fixture := range fixtureByCompute {
		fixtures = append(fixtures, fixture)
	}
	sort.Slice(fixtures, func(i, j int) bool {
		return fixtures[i].computeMoref < fixtures[j].computeMoref
	})
	if len(fixtures) < 2 {
		t.Fatalf("vcsim supplied %d source-bearing compute resources, want at least two", len(fixtures))
	}
	return fixtures
}

func TestResolveClonePlacementSelectsSourceInTargetCompute(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixtures := simulatorComputeFixtures(t, ctx, c)
		c.config.Datastore = simDatastore
		c.config.ResourcePools = []string{fixtures[0].poolPath, fixtures[1].poolPath}

		got, err := c.ResolveClonePlacement(ctx, CloneVMParams{
			TemplateName:        fixtures[0].sourceMoref,
			VCPUs:               1,
			RAMmb:               128,
			AllowMissingNetwork: true,
			TargetHostMoRefs:    []string{fixtures[1].sourceHost.MoRef},
			SourceCandidates: []CloneSource{
				{
					ReplicaID:            "replica-0",
					Ref:                  fixtures[0].sourceMoref,
					ComputeResourceType:  fixtures[0].computeType,
					ComputeResourceMoRef: fixtures[0].computeMoref,
				},
				{
					ReplicaID:            "replica-1",
					Ref:                  fixtures[1].sourceMoref,
					ComputeResourceType:  fixtures[1].computeType,
					ComputeResourceMoRef: fixtures[1].computeMoref,
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.SourceReplicaID != "replica-1" ||
			got.ComputeResourceMoRef != fixtures[1].computeMoref ||
			got.HostMoRef != fixtures[1].sourceHost.MoRef {
			t.Fatalf("placement = %+v, want the target compute's source and host", got)
		}
	})
}

func TestResolveClonePlacementRejectsSourcePoolMismatch(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixtures := simulatorComputeFixtures(t, ctx, c)
		c.config.Datastore = simDatastore
		c.config.ResourcePools = []string{fixtures[0].poolPath}

		_, err := c.ResolveClonePlacement(ctx, CloneVMParams{
			TemplateName:        fixtures[1].sourceMoref,
			VCPUs:               1,
			RAMmb:               128,
			AllowMissingNetwork: true,
			SourceCandidates: []CloneSource{{
				ReplicaID:            "replica-mismatch",
				Ref:                  fixtures[1].sourceMoref,
				ComputeResourceType:  fixtures[1].computeType,
				ComputeResourceMoRef: fixtures[1].computeMoref,
			}},
		})
		if !errors.Is(err, ErrPlacementUnavailable) {
			t.Fatalf("source/pool mismatch error = %v, want ErrPlacementUnavailable", err)
		}
	})
}

func TestResolveClonePlacementRejectsReservedHeadroom(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := simulatorComputeFixtures(t, ctx, c)[0]
		fixture.sourceHost.ReservedMemoryMB = math.MaxInt64 / 2
		setAllowedHosts(c, fixture.sourceHost)
		c.config.Datastore = simDatastore
		c.config.ResourcePools = []string{fixture.poolPath}

		_, err := c.ResolveClonePlacement(ctx, CloneVMParams{
			TemplateName:        fixture.sourceMoref,
			VCPUs:               1,
			RAMmb:               128,
			AllowMissingNetwork: true,
			SourceCandidates: []CloneSource{{
				ReplicaID:            "replica-reserve",
				Ref:                  fixture.sourceMoref,
				ComputeResourceType:  fixture.computeType,
				ComputeResourceMoRef: fixture.computeMoref,
			}},
		})
		if !errors.Is(err, ErrReservedHeadroom) {
			t.Fatalf("reserve rejection error = %v, want ErrReservedHeadroom", err)
		}
	})
}

func TestResolveExistingClonePlacementUsesLiveIdentityWithoutChargingCapacityAgain(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixture := simulatorComputeFixtures(t, ctx, c)[0]
		fixture.sourceHost.ReservedMemoryMB = math.MaxInt64 / 2
		setAllowedHosts(c, fixture.sourceHost)
		c.config.Datastore = simDatastore
		c.config.ResourcePools = []string{fixture.poolPath}

		got, err := c.ResolveExistingClonePlacement(ctx, fixture.sourceMoref, CloneVMParams{
			LogicalTemplateID:   "legacy-template",
			TemplateName:        fixture.sourceMoref,
			VCPUs:               math.MaxInt32,
			RAMmb:               math.MaxInt64 / 2,
			AllowMissingNetwork: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.HostMoRef != fixture.sourceHost.MoRef ||
			got.ComputeResourceMoRef != fixture.computeMoref ||
			got.ResourcePoolMoRef == "" {
			t.Fatalf("reconstructed placement = %+v, want live host/compute/pool identity", got)
		}
	})
}
