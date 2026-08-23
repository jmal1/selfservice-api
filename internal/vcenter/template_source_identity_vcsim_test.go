package vcenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func TestResolveTemplateSourceIdentityAllowsHostOutsideProvisioningAllowlist(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		fixtures := simulatorComputeFixtures(t, ctx, c)
		allowed := fixtures[0]
		source := fixtures[1]
		setAllowedHosts(c, allowed.sourceHost)

		sourceVM := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
			Type:  "VirtualMachine",
			Value: source.sourceMoref,
		})
		var sourceProps mo.VirtualMachine
		if err := sourceVM.Properties(ctx, sourceVM.Reference(), []string{"runtime.powerState"}, &sourceProps); err != nil {
			t.Fatal(err)
		}
		if sourceProps.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
			task, err := sourceVM.PowerOff(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := task.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := sourceVM.Properties(ctx, sourceVM.Reference(), []string{"runtime.powerState"}, &sourceProps); err != nil {
				t.Fatal(err)
			}
		}
		if sourceProps.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
			t.Fatalf("source power state = %s, want poweredOff fixture", sourceProps.Runtime.PowerState)
		}

		got, err := c.ResolveTemplateSourceIdentity(ctx, source.sourceMoref)
		if err != nil {
			t.Fatalf("resolve source on non-allowlisted host: %v", err)
		}
		wantPath := strings.TrimSuffix(source.poolPath, "/Resources")
		if got.SourceVMMoref != source.sourceMoref ||
			got.HostMoref != source.sourceHost.MoRef ||
			got.HostName != source.sourceHost.Name ||
			got.ComputeResourceType != source.computeType ||
			got.ComputeResourceMoref != source.computeMoref ||
			got.ComputeResourcePath != wantPath {
			t.Fatalf("source identity = %+v, want VM %s, host %+v, compute %s/%s at %s",
				got,
				source.sourceMoref,
				source.sourceHost,
				source.computeType,
				source.computeMoref,
				wantPath,
			)
		}
		assertTemplateSourceHostInventory(t, ctx, c, got)

		if _, err := c.allowedHostByMoRef(source.sourceHost.MoRef); !errors.Is(err, ErrHostNotAllowed) {
			t.Fatalf("non-allowlisted source host placement lookup error = %v, want ErrHostNotAllowed", err)
		}
		if _, err := c.ResolvePlacement(ctx, PlacementRequest{
			TargetHostMoRefs: []string{source.sourceHost.MoRef},
		}); !errors.Is(err, ErrPlacementUnavailable) {
			t.Fatalf("non-allowlisted source host target placement error = %v, want ErrPlacementUnavailable", err)
		}
	})
}

func TestResolveTemplateSourceIdentityPreservesAllowlistedSourceBehavior(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		source := simulatorComputeFixtures(t, ctx, c)[0]
		setAllowedHosts(c, source.sourceHost)

		got, err := c.ResolveTemplateSourceIdentity(ctx, source.sourceMoref)
		if err != nil {
			t.Fatalf("resolve source on allowlisted host: %v", err)
		}
		if got.SourceVMMoref != source.sourceMoref ||
			got.HostMoref != source.sourceHost.MoRef ||
			got.ComputeResourceType != source.computeType ||
			got.ComputeResourceMoref != source.computeMoref {
			t.Fatalf("source identity = %+v, want source fixture %+v", got, source)
		}
	})
}

func TestResolveTemplateSourceIdentityFailsClosed(t *testing.T) {
	t.Run("missing VM", func(t *testing.T) {
		withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
			_, err := c.ResolveTemplateSourceIdentity(ctx, "vm-999999")
			if err == nil {
				t.Fatal("missing source VM resolved successfully")
			}
		})
	})

	t.Run("missing runtime host", func(t *testing.T) {
		withSimulatorModel(t, func(
			ctx context.Context,
			c *Client,
			_ *vim25.Client,
			model *simulator.Model,
		) {
			vm := simulatorComputeFixtures(t, ctx, c)[0].sourceMoref
			ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: vm}
			simVM, ok := model.Map().Get(ref).(*simulator.VirtualMachine)
			if !ok {
				t.Fatalf("simulator object %s is not a VirtualMachine", vm)
			}
			simVM.Runtime.Host = nil

			_, err := c.ResolveTemplateSourceIdentity(ctx, vm)
			if !errors.Is(err, ErrPlacementUnavailable) ||
				!strings.Contains(err.Error(), "has no runtime host") {
				t.Fatalf("missing runtime host error = %v, want fail-closed placement error", err)
			}
		})
	})

	t.Run("inventory property failure", func(t *testing.T) {
		withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
			source := simulatorComputeFixtures(t, ctx, c)[0]
			original := c.client.RoundTripper
			c.client.RoundTripper = propertyReadFailure{
				next:       original,
				objectType: "VirtualMachine",
			}
			defer func() { c.client.RoundTripper = original }()

			_, err := c.ResolveTemplateSourceIdentity(ctx, source.sourceMoref)
			if err == nil || !strings.Contains(err.Error(), "injected property read failure") {
				t.Fatalf("property failure error = %v, want injected failure", err)
			}
		})
	})

	t.Run("host inventory property failure", func(t *testing.T) {
		withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
			source := simulatorComputeFixtures(t, ctx, c)[0]
			original := c.client.RoundTripper
			c.client.RoundTripper = propertyReadFailure{
				next:       original,
				objectType: "HostSystem",
			}
			defer func() { c.client.RoundTripper = original }()

			_, err := c.ResolveTemplateSourceIdentity(ctx, source.sourceMoref)
			if err == nil || !strings.Contains(err.Error(), "injected property read failure") {
				t.Fatalf("host property failure error = %v, want injected failure", err)
			}
		})
	})

	t.Run("non-compute host parent", func(t *testing.T) {
		withSimulatorModel(t, func(
			ctx context.Context,
			c *Client,
			_ *vim25.Client,
			model *simulator.Model,
		) {
			source := simulatorComputeFixtures(t, ctx, c)[0]
			hostRef := types.ManagedObjectReference{Type: "HostSystem", Value: source.sourceHost.MoRef}
			simHost, ok := model.Map().Get(hostRef).(*simulator.HostSystem)
			if !ok {
				t.Fatalf("simulator object %s is not a HostSystem", source.sourceHost.MoRef)
			}
			folder, ok := model.Map().Any("Folder").(*simulator.Folder)
			if !ok {
				t.Fatal("simulator has no Folder")
			}
			parent := folder.Reference()
			simHost.Parent = &parent

			_, err := c.ResolveTemplateSourceIdentity(ctx, source.sourceMoref)
			if !errors.Is(err, ErrPlacementUnavailable) ||
				!strings.Contains(err.Error(), "is not a compute resource") {
				t.Fatalf("non-compute parent error = %v, want fail-closed placement error", err)
			}
		})
	})
}

type propertyReadFailure struct {
	next       soap.RoundTripper
	objectType string
}

func (f propertyReadFailure) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.RetrievePropertiesExBody); ok && body.Req != nil {
		for _, spec := range body.Req.SpecSet {
			for _, objectSpec := range spec.ObjectSet {
				if objectSpec.Obj.Type == f.objectType {
					return fmt.Errorf("injected property read failure")
				}
			}
		}
	}
	return f.next.RoundTrip(ctx, req, res)
}

func assertTemplateSourceHostInventory(
	t *testing.T,
	ctx context.Context,
	c *Client,
	identity *TemplateSourceIdentity,
) {
	t.Helper()
	host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
		Type:  "HostSystem",
		Value: identity.HostMoref,
	})
	var props mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"name", "parent"}, &props); err != nil {
		t.Fatal(err)
	}
	if props.Name != identity.HostName || props.Parent == nil ||
		props.Parent.Type != identity.ComputeResourceType ||
		props.Parent.Value != identity.ComputeResourceMoref {
		t.Fatalf("identity %+v does not match live host properties %+v", identity, props)
	}
}
