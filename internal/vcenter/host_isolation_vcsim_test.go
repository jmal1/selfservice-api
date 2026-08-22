package vcenter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type hostIsolationSaboteur struct {
	next                 soap.RoundTripper
	allowedHost          string
	allowedNetworkSystem string
	forbiddenVMs         map[string]struct{}

	mu         sync.Mutex
	violations []string
}

func (s *hostIsolationSaboteur) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	switch body := req.(type) {
	case *methods.AddPortGroupBody:
		if body.Req != nil && body.Req.This.Value != s.allowedNetworkSystem {
			return s.reject("AddPortGroup", body.Req.This.Value)
		}
	case *methods.RemovePortGroupBody:
		if body.Req != nil && body.Req.This.Value != s.allowedNetworkSystem {
			return s.reject("RemovePortGroup", body.Req.This.Value)
		}
	case *methods.CloneVM_TaskBody:
		if body.Req == nil || body.Req.Spec.Location.Host == nil ||
			body.Req.Spec.Location.Host.Value != s.allowedHost {
			return s.reject("CloneVM_Task", hostValue(body.Req, func(req *types.CloneVM_Task) *types.ManagedObjectReference {
				return req.Spec.Location.Host
			}))
		}
	case *methods.CreateVM_TaskBody:
		if body.Req == nil || body.Req.Host == nil || body.Req.Host.Value != s.allowedHost {
			return s.reject("CreateVM_Task", hostValue(body.Req, func(req *types.CreateVM_Task) *types.ManagedObjectReference {
				return req.Host
			}))
		}
	case *methods.ImportVAppBody:
		if body.Req == nil || body.Req.Host == nil || body.Req.Host.Value != s.allowedHost {
			return s.reject("ImportVApp", hostValue(body.Req, func(req *types.ImportVApp) *types.ManagedObjectReference {
				return req.Host
			}))
		}
	case *methods.RelocateVM_TaskBody:
		if body.Req == nil || body.Req.Spec.Host == nil || body.Req.Spec.Host.Value != s.allowedHost {
			return s.reject("RelocateVM_Task", hostValue(body.Req, func(req *types.RelocateVM_Task) *types.ManagedObjectReference {
				return req.Spec.Host
			}))
		}
	case *methods.PowerOnVM_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("PowerOnVM_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	case *methods.PowerOffVM_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("PowerOffVM_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	case *methods.ReconfigVM_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("ReconfigVM_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	case *methods.ResetVM_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("ResetVM_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	case *methods.SuspendVM_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("SuspendVM_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	case *methods.Destroy_TaskBody:
		if body.Req != nil {
			if err := s.rejectForbiddenVM("Destroy_Task", body.Req.This.Value); err != nil {
				return err
			}
		}
	}
	return s.next.RoundTrip(ctx, req, res)
}

func hostValue[T any](request *T, get func(*T) *types.ManagedObjectReference) string {
	if request == nil {
		return "<nil request>"
	}
	host := get(request)
	if host == nil {
		return "<unpinned>"
	}
	return host.Value
}

func (s *hostIsolationSaboteur) rejectForbiddenVM(operation, moref string) error {
	if _, forbidden := s.forbiddenVMs[moref]; forbidden {
		return s.reject(operation, moref)
	}
	return nil
}

func (s *hostIsolationSaboteur) reject(operation, target string) error {
	message := fmt.Sprintf("%s attempted against disallowed or unpinned target %s", operation, target)
	s.mu.Lock()
	s.violations = append(s.violations, message)
	s.mu.Unlock()
	return errors.New(message)
}

func (s *hostIsolationSaboteur) assertClean(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.violations) > 0 {
		t.Fatalf("ESXi2 sabotage triggered: %v", s.violations)
	}
}

func TestESXi1OnlyAllowlistPinsEveryCreationPrimitive(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		allowed, forbidden, source, allowedNetworkSystem, forbiddenVM := simulatorHostIsolationFixture(t, ctx, c)

		original := c.client.RoundTripper
		saboteur := &hostIsolationSaboteur{
			next:                 original,
			allowedHost:          allowed.MoRef,
			allowedNetworkSystem: allowedNetworkSystem,
			forbiddenVMs:         map[string]struct{}{forbiddenVM: {}},
		}
		c.client.RoundTripper = saboteur
		defer func() { c.client.RoundTripper = original }()

		if err := c.DestroyVM(ctx, forbiddenVM); !errors.Is(err, ErrHostNotAllowed) {
			t.Fatalf("destroy VM on simulated ESXi2 error = %v, want ErrHostNotAllowed", err)
		}

		const portGroupName = "Pod-VLAN311"
		receipt, err := c.PlanPortGroupMutation(ctx, portGroupName, 311)
		if err != nil {
			t.Fatal(err)
		}
		if len(receipt.Hosts) != 1 || receipt.Hosts[0].HostMoRef != allowed.MoRef {
			t.Fatalf("portgroup receipt hosts = %+v, want ESXi1 only", receipt.Hosts)
		}
		if err := c.ApplyPortGroupMutation(ctx, receipt); err != nil {
			t.Fatal(err)
		}
		if _, found, err := c.findPortGroupOnHost(ctx, forbidden, portGroupName); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatal("port group was created on simulated ESXi2")
		}
		if err := c.DeletePortGroupMutation(ctx, receipt); err != nil {
			t.Fatal(err)
		}

		var created []string
		cleanup := func(moref string) {
			t.Helper()
			created = append(created, moref)
		}
		defer func() {
			for i := len(created) - 1; i >= 0; i-- {
				if err := c.DestroyVM(ctx, created[i]); err != nil {
					t.Errorf("cleanup VM %s: %v", created[i], err)
				}
			}
		}()

		cloneParams, err := c.ResolveClonePlacement(ctx, CloneVMParams{
			TemplateName: source.Reference().Value,
			VMName:       "isolation-pod-clone",
			VCPUs:        1,
			RAMmb:        512,
			Network:      simNetwork,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cloneParams.HostMoRef != allowed.MoRef {
			t.Fatalf("durable clone selected %s, want %s", cloneParams.HostMoRef, allowed.MoRef)
		}
		armed := false
		taskRef, err := c.StartCloneVMOperation(ctx, cloneParams, func(context.Context) error {
			armed = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if !armed {
			t.Fatal("durable clone submitted without arming")
		}
		podClone, err := c.WaitCloneVMTask(ctx, taskRef)
		if err != nil {
			t.Fatal(err)
		}
		assertVMOnHost(t, ctx, c, podClone, allowed.MoRef)
		cleanup(podClone)

		templateClone, err := c.CloneTemplateSourceVM(ctx, TemplateCloneParams{
			SourceMoref: source.Reference().Value,
			VMName:      "isolation-template-stage",
			FolderPath:  simVMFolder,
			Network:     simNetwork,
			VCPUs:       1,
			RAMmb:       512,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertVMOnHost(t, ctx, c, templateClone, allowed.MoRef)
		cleanup(templateClone)

		blank, err := c.CreateBlankVM(ctx, baseBlankVMParams("isolation-blank-iso"))
		if err != nil {
			t.Fatal(err)
		}
		assertVMOnHost(t, ctx, c, blank, allowed.MoRef)
		cleanup(blank)

		ova := buildSyntheticOVA(t)
		imported, err := c.ImportOVA(ctx, OVAImportParams{
			Reader:       bytes.NewReader(ova),
			Size:         int64(len(ova)),
			VMName:       "isolation-ova",
			Datastore:    simDatastore,
			ResourcePool: simResourcePool,
			Network:      simNetwork,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertVMOnHost(t, ctx, c, imported, allowed.MoRef)
		cleanup(imported)

		health, err := c.CloneForHealthCheck(ctx, HealthCheckCloneParams{
			SourceRef:    source.Reference().Value,
			CloneName:    "crucible-healthcheck-isolation",
			FolderPath:   simVMFolder,
			Datastore:    simDatastore,
			ResourcePool: simResourcePool,
			Network:      simNetwork,
			VCPUs:        1,
			RAMmb:        512,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertVMOnHost(t, ctx, c, health.MoRef, allowed.MoRef)
		cleanup(health.MoRef)

		saboteur.assertClean(t)
	})
}

func TestResolvePlacementFailsClosedBeforeCreation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*testing.T, context.Context, *Client, HostIdentity, *object.VirtualMachine) PlacementRequest
		wantErr string
	}{
		{
			name: "missing standard portgroup",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				return placementRequestForSource(t, ctx, source, "missing-portgroup", 1)
			},
			wantErr: "standard port group",
		},
		{
			name: "missing datastore",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				request := placementRequestForSource(t, ctx, source, simNetwork, 1)
				request.DatastoreName = "missing-datastore"
				return request
			},
			wantErr: "find placement datastore",
		},
		{
			name: "source host unavailable",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				return PlacementRequest{
					RequireSourceHost: true,
					DatastoreName:     simDatastore,
					NetworkName:       simNetwork,
					VCPUs:             1,
					RAMMB:             512,
				}
			},
			wantErr: "source VM has no runtime host assignment",
		},
		{
			name: "incompatible source cluster",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				request := placementRequestForSource(t, ctx, source, simNetwork, 1)
				request.ResourcePoolPath = resourcePoolOutsideCompute(t, ctx, c, allowed.ComputeMoRef)
				return request
			},
			wantErr: "not source compute resource",
		},
		{
			name: "host outside pool compute resource",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				outside := hostOutsideCompute(t, ctx, c, allowed.ComputeMoRef)
				c.hostMu.Lock()
				c.allowedHosts = []HostIdentity{outside}
				c.hostMu.Unlock()
				return PlacementRequest{
					DatastoreName: simDatastore,
					NetworkName:   simNetwork,
					VCPUs:         1,
					RAMMB:         512,
				}
			},
			wantErr: "not a member of pool compute resource",
		},
		{
			name: "disconnected host",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
					Type:  "HostSystem",
					Value: allowed.MoRef,
				})
				task, err := host.Disconnect(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := task.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				return placementRequestForSource(t, ctx, source, simNetwork, 1)
			},
			wantErr: "connection state",
		},
		{
			name: "maintenance host",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
					Type:  "HostSystem",
					Value: allowed.MoRef,
				})
				task, err := host.EnterMaintenanceMode(ctx, 0, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := task.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				return placementRequestForSource(t, ctx, source, simNetwork, 1)
			},
			wantErr: "maintenance mode",
		},
		{
			name: "insufficient host capacity",
			setup: func(t *testing.T, ctx context.Context, c *Client, allowed HostIdentity, source *object.VirtualMachine) PlacementRequest {
				return placementRequestForSource(t, ctx, source, simNetwork, 1<<30)
			},
			wantErr: "CPU cores",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
				allowed, _, source, _, _ := simulatorHostIsolationFixture(t, ctx, c)
				request := tc.setup(t, ctx, c, allowed, source)
				_, err := c.ResolvePlacement(ctx, request)
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ResolvePlacement error = %v, want text %q", err, tc.wantErr)
				}
			})
		})
	}
}

func TestResolvePlacementRejectsSourceOnDisallowedHost(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		_, forbidden, _, _, _ := simulatorHostIsolationFixture(t, ctx, c)
		_, err := c.ResolvePlacement(ctx, PlacementRequest{
			SourceHost: &types.ManagedObjectReference{
				Type:  "HostSystem",
				Value: forbidden.MoRef,
			},
			DatastoreName: simDatastore,
			NetworkName:   simNetwork,
			VCPUs:         1,
			RAMMB:         512,
		})
		if !errors.Is(err, ErrPlacementUnavailable) || !strings.Contains(err.Error(), "source host") {
			t.Fatalf("disallowed source error = %v, want fail-closed source-host diagnostic", err)
		}
	})
}

func placementRequestForSource(
	t *testing.T,
	ctx context.Context,
	source *object.VirtualMachine,
	network string,
	vcpus int32,
) PlacementRequest {
	t.Helper()
	var props mo.VirtualMachine
	if err := source.Properties(ctx, source.Reference(), []string{"runtime.host"}, &props); err != nil {
		t.Fatal(err)
	}
	return PlacementRequest{
		SourceHost:        props.Runtime.Host,
		RequireSourceHost: true,
		DatastoreName:     simDatastore,
		NetworkName:       network,
		VCPUs:             vcpus,
		RAMMB:             512,
	}
}

func resourcePoolOutsideCompute(t *testing.T, ctx context.Context, c *Client, computeMoref string) string {
	t.Helper()
	pools, err := c.finder.ResourcePoolList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools {
		var props mo.ResourcePool
		if err := pool.Properties(ctx, pool.Reference(), []string{"owner"}, &props); err != nil {
			t.Fatal(err)
		}
		if props.Owner.Value != computeMoref {
			return pool.InventoryPath
		}
	}
	t.Fatal("vcsim fixture has no resource pool outside the source compute resource")
	return ""
}

func hostOutsideCompute(t *testing.T, ctx context.Context, c *Client, computeMoref string) HostIdentity {
	t.Helper()
	hosts, err := c.finder.HostSystemList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range hosts {
		var props mo.HostSystem
		if err := host.Properties(ctx, host.Reference(), []string{"name", "parent"}, &props); err != nil {
			t.Fatal(err)
		}
		if props.Parent != nil && props.Parent.Value != computeMoref {
			return HostIdentity{
				Name:          props.Name,
				InventoryPath: host.InventoryPath,
				MoRef:         host.Reference().Value,
				ComputeMoRef:  props.Parent.Value,
			}
		}
	}
	t.Fatal("vcsim fixture has no host outside the source compute resource")
	return HostIdentity{}
}

func simulatorHostIsolationFixture(
	t *testing.T,
	ctx context.Context,
	c *Client,
) (HostIdentity, HostIdentity, *object.VirtualMachine, string, string) {
	t.Helper()
	pool, err := c.finder.ResourcePool(ctx, simResourcePool)
	if err != nil {
		t.Fatal(err)
	}
	var poolProps mo.ResourcePool
	if err := pool.Properties(ctx, pool.Reference(), []string{"owner"}, &poolProps); err != nil {
		t.Fatal(err)
	}
	hosts, err := c.resolvedHosts()
	if err != nil {
		t.Fatal(err)
	}
	byMoref := make(map[string]HostIdentity)
	for _, host := range hosts {
		if host.ComputeMoRef == poolProps.Owner.Value {
			byMoref[host.MoRef] = host
		}
	}
	if len(byMoref) < 2 {
		t.Fatalf("vcsim fixture has %d hosts in %s, want at least two", len(byMoref), simResourcePool)
	}

	vms, err := c.finder.VirtualMachineList(ctx, "*")
	if err != nil {
		t.Fatal(err)
	}
	var source *object.VirtualMachine
	var allowed HostIdentity
	vmByHost := make(map[string]string)
	for _, vm := range vms {
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &props); err != nil {
			t.Fatal(err)
		}
		if props.Runtime.Host == nil {
			continue
		}
		vmByHost[props.Runtime.Host.Value] = vm.Reference().Value
		if source == nil {
			if identity, ok := byMoref[props.Runtime.Host.Value]; ok {
				source = vm
				allowed = identity
			}
		}
	}
	if source == nil {
		t.Fatal("vcsim fixture has no source VM on a clustered host")
	}
	var forbidden HostIdentity
	for moref, identity := range byMoref {
		if moref != allowed.MoRef {
			forbidden = identity
			break
		}
	}
	if forbidden.MoRef == "" {
		t.Fatal("vcsim fixture has no second host to represent ESXi2")
	}

	c.hostMu.Lock()
	c.allowedHosts = []HostIdentity{allowed}
	c.hostMu.Unlock()
	c.config.Hosts = []string{allowed.Name}
	c.config.ResourcePools = []string{simResourcePool}
	c.config.Datastore = simDatastore
	c.config.VMFolder = simVMFolder
	c.config.TemplateFolder = simVMFolder

	allowedNetworkSystem, err := object.NewHostSystem(
		c.client.Client,
		types.ManagedObjectReference{Type: "HostSystem", Value: allowed.MoRef},
	).ConfigManager().NetworkSystem(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenVM := vmByHost[forbidden.MoRef]
	if forbiddenVM == "" {
		folder, err := c.finder.Folder(ctx, simVMFolder)
		if err != nil {
			t.Fatal(err)
		}
		host := object.NewHostSystem(c.client.Client, types.ManagedObjectReference{
			Type:  "HostSystem",
			Value: forbidden.MoRef,
		})
		task, err := folder.CreateVM(ctx, types.VirtualMachineConfigSpec{
			Name:     "simulated-esxi2-owned-vm",
			GuestId:  "otherGuest",
			NumCPUs:  1,
			MemoryMB: 64,
			Files: &types.VirtualMachineFileInfo{
				VmPathName: "[" + simDatastore + "]",
			},
		}, pool, host)
		if err != nil {
			t.Fatal(err)
		}
		info, err := task.WaitForResult(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		ref, ok := info.Result.(types.ManagedObjectReference)
		if !ok {
			t.Fatalf("fixture CreateVM returned %T", info.Result)
		}
		forbiddenVM = ref.Value
	}
	return allowed, forbidden, source, allowedNetworkSystem.Reference().Value, forbiddenVM
}

func assertVMOnHost(t *testing.T, ctx context.Context, c *Client, moref, wantHost string) {
	t.Helper()
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: moref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &props); err != nil {
		t.Fatal(err)
	}
	if props.Runtime.Host == nil || props.Runtime.Host.Value != wantHost {
		t.Fatalf("VM %s host = %v, want %s", moref, props.Runtime.Host, wantHost)
	}
}
