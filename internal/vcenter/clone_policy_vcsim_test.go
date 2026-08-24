package vcenter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

type clonePolicySaboteur struct {
	next       soap.RoundTripper
	injectVTPM bool
	wantPolicy string

	mu              sync.Mutex
	hardwareSources map[string]struct{}
	policies        map[string]string
}

func (s *clonePolicySaboteur) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.RetrievePropertiesExBody); ok && s.injectVTPM {
		if sourceMoref := s.requestedHardwareSource(body.Req); sourceMoref != "" {
			if err := s.next.RoundTrip(ctx, req, res); err != nil {
				return err
			}
			return injectVirtualTPM(res, sourceMoref)
		}
	}

	if body, ok := req.(*methods.CloneVM_TaskBody); ok && body.Req != nil {
		got := body.Req.Spec.TpmProvisionPolicy
		if got != s.wantPolicy {
			return fmt.Errorf(
				"clone %q used TPM provisioning policy %q, want %q",
				body.Req.Name,
				got,
				s.wantPolicy,
			)
		}
		if body.Req.Spec.Location.CryptoSpec != nil {
			return fmt.Errorf("clone %q unexpectedly changed VM encryption", body.Req.Name)
		}
		if len(body.Req.Spec.Location.Profile) != 0 {
			return fmt.Errorf("clone %q unexpectedly changed the VM home storage profile", body.Req.Name)
		}
		if body.Req.Spec.Config != nil {
			if body.Req.Spec.Config.Crypto != nil {
				return fmt.Errorf("clone %q unexpectedly changed VM configuration encryption", body.Req.Name)
			}
			if len(body.Req.Spec.Config.VmProfile) != 0 {
				return fmt.Errorf("clone %q unexpectedly changed the VM storage profile", body.Req.Name)
			}
		}
		if strings.Contains(body.Req.Name, "-retained-replica") {
			if err := validateSubmittedReplicaBuildSpec(body.Req); err != nil {
				return err
			}
		}
		s.mu.Lock()
		s.policies[body.Req.Name] = got
		s.mu.Unlock()

		// vcsim predates the vSphere 8 TPM clone policy behavior. Strip the
		// already-verified field before forwarding so it can materialize the VM.
		body.Req.Spec.TpmProvisionPolicy = ""
	}
	return s.next.RoundTrip(ctx, req, res)
}

func (s *clonePolicySaboteur) requestedHardwareSource(req *types.RetrievePropertiesEx) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sourceMoref := range s.hardwareSources {
		if requestsVMHardware(req, sourceMoref) {
			return sourceMoref
		}
	}
	return ""
}

func (s *clonePolicySaboteur) addHardwareSource(moref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hardwareSources[moref] = struct{}{}
}

func validateSubmittedReplicaBuildSpec(req *types.CloneVM_Task) error {
	if req.Spec.PowerOn || req.Spec.Template {
		return fmt.Errorf("replica clone %q was not submitted as a powered-off VM", req.Name)
	}
	if req.Spec.Location.Host == nil || req.Spec.Location.Pool == nil ||
		req.Spec.Location.Datastore == nil || req.Spec.Snapshot == nil {
		return fmt.Errorf("replica clone %q omitted exact host, pool, datastore, or snapshot", req.Name)
	}
	if req.Spec.Config == nil {
		return fmt.Errorf("replica clone %q omitted durable operation markers", req.Name)
	}
	markers := make(map[string]string)
	for _, option := range req.Spec.Config.ExtraConfig {
		value, ok := option.GetOptionValue().Value.(string)
		if ok {
			markers[option.GetOptionValue().Key] = value
		}
	}
	if markers[ReplicaBuildMarkerKey] == "" || markers[CloneOperationIDKey] == "" {
		return fmt.Errorf("replica clone %q omitted ownership markers: %v", req.Name, markers)
	}
	wantMove := string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndDisallowSharing)
	wantKind := ReplicaBuildDestinationKind
	if strings.HasSuffix(req.Name, "-canary") {
		wantMove = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
		wantKind = ReplicaBuildCanaryKind
	}
	if req.Spec.Location.DiskMoveType != wantMove {
		return fmt.Errorf("replica clone %q disk move=%q, want %q", req.Name, req.Spec.Location.DiskMoveType, wantMove)
	}
	if markers[ReplicaBuildKindKey] != wantKind {
		return fmt.Errorf("replica clone %q kind marker=%q, want %q", req.Name, markers[ReplicaBuildKindKey], wantKind)
	}
	return nil
}

func (s *clonePolicySaboteur) assertCloneNames(t *testing.T, names ...string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.policies) != len(names) {
		t.Fatalf("recorded clone policies = %v, want clone names %v", s.policies, names)
	}
	for _, name := range names {
		if _, ok := s.policies[name]; !ok {
			t.Errorf("clone policy for %q was not recorded", name)
		}
	}
}

func requestsVMHardware(req *types.RetrievePropertiesEx, sourceMoref string) bool {
	if req == nil {
		return false
	}
	for _, spec := range req.SpecSet {
		requestsHardware := false
		for _, prop := range spec.PropSet {
			if prop.Type != "VirtualMachine" {
				continue
			}
			for _, path := range prop.PathSet {
				if path == "config.hardware.device" {
					requestsHardware = true
					break
				}
			}
		}
		if !requestsHardware {
			continue
		}
		for _, objectSpec := range spec.ObjectSet {
			if objectSpec.Obj.Type == "VirtualMachine" && objectSpec.Obj.Value == sourceMoref {
				return true
			}
		}
	}
	return false
}

func injectVirtualTPM(res soap.HasFault, sourceMoref string) error {
	body, ok := res.(*methods.RetrievePropertiesExBody)
	if !ok || body.Res == nil {
		return fmt.Errorf("unexpected property response %T", res)
	}
	for objectIndex := range body.Res.Returnval.Objects {
		object := &body.Res.Returnval.Objects[objectIndex]
		if object.Obj.Type != "VirtualMachine" || object.Obj.Value != sourceMoref {
			continue
		}
		for propertyIndex := range object.PropSet {
			property := &object.PropSet[propertyIndex]
			if property.Name != "config.hardware.device" {
				continue
			}
			switch devices := property.Val.(type) {
			case types.ArrayOfVirtualDevice:
				devices.VirtualDevice = append(devices.VirtualDevice, &types.VirtualTPM{})
				property.Val = devices
				return nil
			case *types.ArrayOfVirtualDevice:
				devices.VirtualDevice = append(devices.VirtualDevice, &types.VirtualTPM{})
				return nil
			default:
				return fmt.Errorf(
					"unexpected config.hardware.device property type %T",
					property.Val,
				)
			}
		}
	}
	return fmt.Errorf("source VM %s hardware property was not returned", sourceMoref)
}

func TestCloneBuildersApplyVTPMPolicyToSubmittedSpecs(t *testing.T) {
	tests := []struct {
		name       string
		injectVTPM bool
		wantPolicy string
	}{
		{
			name:       "vTPM source is replaced",
			injectVTPM: true,
			wantPolicy: string(types.VirtualMachineCloneSpecTpmProvisionPolicyReplace),
		},
		{
			name: "source without vTPM preserves default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
				allowed, _, source, _, _ := simulatorHostIsolationFixture(t, ctx, c)
				prefix := "without-vtpm"
				if tt.injectVTPM {
					prefix = "with-vtpm"
				}
				durableName := prefix + "-durable-clone"
				templateName := prefix + "-template-stage"
				healthName := "crucible-healthcheck-" + prefix
				replicaName := prefix + "-retained-replica"
				canaryName := prefix + "-retained-replica-canary"

				powerState, err := source.PowerState(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if powerState != types.VirtualMachinePowerStatePoweredOff {
					powerTask, err := source.PowerOff(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if err := powerTask.Wait(ctx); err != nil {
						t.Fatal(err)
					}
				}
				snapshotTask, err := source.CreateSnapshot(
					ctx,
					"base-image",
					"clone policy test source",
					false,
					false,
				)
				if err != nil {
					t.Fatal(err)
				}
				snapshotResult, err := snapshotTask.WaitForResult(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				sourceSnapshot, ok := snapshotResult.Result.(types.ManagedObjectReference)
				if !ok {
					t.Fatalf("snapshot result=%T, want ManagedObjectReference", snapshotResult.Result)
				}

				original := c.client.RoundTripper
				saboteur := &clonePolicySaboteur{
					next:            original,
					injectVTPM:      tt.injectVTPM,
					wantPolicy:      tt.wantPolicy,
					hardwareSources: map[string]struct{}{source.Reference().Value: {}},
					policies:        make(map[string]string),
				}
				c.client.RoundTripper = saboteur
				defer func() { c.client.RoundTripper = original }()

				var created []string
				defer func() {
					for _, moref := range created {
						destroySimulatorVM(t, ctx, c, moref)
					}
				}()

				replicaTarget := simulatorReplicaBuildTarget(t, ctx, c, allowed)
				replicaTask, replicaSourceSnapshot, err := c.StartReplicaBuildClone(
					ctx,
					ReplicaBuildCloneParams{
						BuildID:             "11111111-1111-1111-1111-111111111111",
						OperationID:         "22222222-2222-2222-2222-222222222222",
						Kind:                ReplicaBuildDestinationKind,
						TemplateID:          "33333333-3333-3333-3333-333333333333",
						SourceReplicaID:     "44444444-4444-4444-4444-444444444444",
						SourceVMMoref:       source.Reference().Value,
						SourceSnapshotName:  "base-image",
						SourceSnapshotMoref: sourceSnapshot.Value,
						DestinationName:     replicaName,
						Target:              replicaTarget,
					},
					func(context.Context) error { return nil },
				)
				if err != nil {
					t.Fatal(err)
				}
				if replicaSourceSnapshot != sourceSnapshot.Value {
					t.Fatalf("replica source snapshot=%s, want %s", replicaSourceSnapshot, sourceSnapshot.Value)
				}
				replicaMoref, err := c.WaitReplicaBuildCloneTask(ctx, replicaTask)
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, replicaMoref)
				saboteur.addHardwareSource(replicaMoref)
				// vcsim verifies but does not persist clone-time extraConfig.
				// Reapply the already-asserted markers so snapshot/canary
				// reconciliation can exercise the production ownership checks.
				replicaVM := object.NewVirtualMachine(
					c.client.Client,
					types.ManagedObjectReference{Type: "VirtualMachine", Value: replicaMoref},
				)
				markerTask, err := replicaVM.Reconfigure(ctx, types.VirtualMachineConfigSpec{
					ExtraConfig: replicaBuildExtraConfig(
						"11111111-1111-1111-1111-111111111111",
						"22222222-2222-2222-2222-222222222222",
						ReplicaBuildDestinationKind,
					),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := markerTask.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				replicaSnapshotTask, err := c.StartReplicaBuildSnapshot(
					ctx,
					replicaMoref,
					"11111111-1111-1111-1111-111111111111",
					"22222222-2222-2222-2222-222222222222",
					"base-image",
					func(context.Context) error { return nil },
				)
				if err != nil {
					t.Fatal(err)
				}
				replicaSnapshot, err := c.WaitReplicaBuildSnapshotTask(ctx, replicaSnapshotTask)
				if err != nil {
					t.Fatal(err)
				}

				canaryParams := ReplicaBuildCloneParams{
					BuildID:               "11111111-1111-1111-1111-111111111111",
					OperationID:           "55555555-5555-5555-5555-555555555555",
					SourceOperationID:     "22222222-2222-2222-2222-222222222222",
					Kind:                  ReplicaBuildCanaryKind,
					TemplateID:            "33333333-3333-3333-3333-333333333333",
					SourceReplicaID:       "44444444-4444-4444-4444-444444444444",
					SourceVMMoref:         replicaMoref,
					DestinationVMMoref:    replicaMoref,
					DestinationSnapshot:   replicaSnapshot,
					DestinationName:       canaryName,
					Target:                replicaTarget,
					UseProvisionDatastore: true,
				}
				canaryTask, err := c.StartReplicaBuildCanary(
					ctx,
					canaryParams,
					func(context.Context) error { return nil },
				)
				if err != nil {
					t.Fatal(err)
				}
				canaryMoref, err := c.WaitReplicaBuildCloneTask(ctx, canaryTask)
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, canaryMoref)
				canaryVM := object.NewVirtualMachine(
					c.client.Client,
					types.ManagedObjectReference{Type: "VirtualMachine", Value: canaryMoref},
				)
				canaryMarkerTask, err := canaryVM.Reconfigure(ctx, types.VirtualMachineConfigSpec{
					ExtraConfig: replicaBuildExtraConfig(
						canaryParams.BuildID,
						canaryParams.OperationID,
						ReplicaBuildCanaryKind,
					),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := canaryMarkerTask.Wait(ctx); err != nil {
					t.Fatal(err)
				}
				canaryParams.DestinationVMMoref = canaryMoref
				assertVMOnHost(t, ctx, c, canaryMoref, allowed.MoRef)
				if _, _, err := c.StartReplicaBuildCanaryCleanup(
					ctx,
					canaryMoref,
					canaryParams.BuildID,
					"wrong-operation-id",
					func(context.Context) error { return nil },
				); err == nil || !errors.Is(err, ErrReplicaBuildAmbiguous) {
					t.Fatalf("marker-mismatched canary cleanup error=%v, want ambiguity", err)
				}
				cleanupTask, alreadyGone, err := c.StartReplicaBuildCanaryCleanup(
					ctx,
					canaryMoref,
					canaryParams.BuildID,
					canaryParams.OperationID,
					func(context.Context) error { return nil },
				)
				if err != nil || alreadyGone {
					t.Fatalf("exact canary cleanup task=%s alreadyGone=%t error=%v", cleanupTask, alreadyGone, err)
				}
				if err := c.WaitReplicaBuildCleanupTask(ctx, cleanupTask); err != nil {
					t.Fatal(err)
				}
				created = created[:len(created)-1]
				if exists, err := c.ReplicaBuildCanaryExists(ctx, canaryParams); err != nil || exists {
					t.Fatalf("canary residue exists=%t error=%v after exact cleanup", exists, err)
				}

				cloneParams, err := c.ResolveClonePlacement(ctx, CloneVMParams{
					TemplateName: source.Reference().Value,
					VMName:       durableName,
					VCPUs:        1,
					RAMmb:        512,
					Network:      simNetwork,
				})
				if err != nil {
					t.Fatal(err)
				}
				taskRef, err := c.StartCloneVMOperation(ctx, cloneParams, func(context.Context) error {
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				durableMoref, err := c.WaitCloneVMTask(ctx, taskRef)
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, durableMoref)

				templateMoref, err := c.CloneTemplateSourceVM(ctx, TemplateCloneParams{
					SourceMoref: source.Reference().Value,
					VMName:      templateName,
					FolderPath:  simVMFolder,
					Network:     simNetwork,
					VCPUs:       1,
					RAMmb:       512,
				})
				if err != nil {
					t.Fatal(err)
				}
				created = append(created, templateMoref)

				health, err := c.CloneForHealthCheck(ctx, HealthCheckCloneParams{
					SourceRef:    source.Reference().Value,
					CloneName:    healthName,
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
				created = append(created, health.MoRef)

				saboteur.assertCloneNames(t, replicaName, canaryName, durableName, templateName, healthName)
				assertVMOnHost(t, ctx, c, replicaMoref, allowed.MoRef)
				assertVMOnHost(t, ctx, c, durableMoref, allowed.MoRef)
				assertVMOnHost(t, ctx, c, templateMoref, allowed.MoRef)
				assertVMOnHost(t, ctx, c, health.MoRef, allowed.MoRef)
			})
		})
	}
}

func simulatorReplicaBuildTarget(
	t *testing.T,
	ctx context.Context,
	c *Client,
	host HostIdentity,
) ReplicaBuildTarget {
	t.Helper()
	compute, err := c.finder.ObjectReference(ctx, types.ManagedObjectReference{
		Type: host.ComputeType, Value: host.ComputeMoRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := c.finder.ResourcePool(ctx, simResourcePool)
	if err != nil {
		t.Fatal(err)
	}
	datastore, err := c.finder.Datastore(ctx, simDatastore)
	if err != nil {
		t.Fatal(err)
	}
	folder, err := c.finder.Folder(ctx, simVMFolder)
	if err != nil {
		t.Fatal(err)
	}
	return ReplicaBuildTarget{
		ComputeResourceType:  host.ComputeType,
		ComputeResourceMoref: host.ComputeMoRef,
		ComputeResourcePath:  inventoryPath(compute),
		HostMoref:            host.MoRef,
		HostName:             host.Name,
		ResourcePoolMoref:    pool.Reference().Value,
		ResourcePoolPath:     pool.InventoryPath,
		DatastoreMoref:       datastore.Reference().Value,
		DatastoreName:        simDatastore,
		FolderMoref:          folder.Reference().Value,
		FolderPath:           folder.InventoryPath,
		ProvisionDatastore:   simDatastore,
	}
}

type cloneHardwareReadFailure struct {
	next        soap.RoundTripper
	sourceMoref string

	mu         sync.Mutex
	cloneCalls int
}

func (f *cloneHardwareReadFailure) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.RetrievePropertiesExBody); ok &&
		requestsVMHardware(body.Req, f.sourceMoref) {
		return fmt.Errorf("injected source hardware read failure")
	}
	if _, ok := req.(*methods.CloneVM_TaskBody); ok {
		f.mu.Lock()
		f.cloneCalls++
		f.mu.Unlock()
	}
	return f.next.RoundTrip(ctx, req, res)
}

func (f *cloneHardwareReadFailure) assertNoClone(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cloneCalls != 0 {
		t.Fatalf("CloneVM_Task calls = %d, want 0 after source hardware read failure", f.cloneCalls)
	}
}

func TestCloneBuildersFailClosedWhenSourceHardwareCannotBeInspected(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(context.Context, *Client, *object.VirtualMachine) error
	}{
		{
			name: "durable clone",
			invoke: func(ctx context.Context, c *Client, source *object.VirtualMachine) error {
				params, err := c.ResolveClonePlacement(ctx, CloneVMParams{
					TemplateName: source.Reference().Value,
					VMName:       "hardware-read-failure-durable",
					VCPUs:        1,
					RAMmb:        512,
					Network:      simNetwork,
				})
				if err != nil {
					return err
				}
				_, err = c.StartCloneVMOperation(ctx, params, func(context.Context) error {
					return fmt.Errorf("arm must not run")
				})
				return err
			},
		},
		{
			name: "template staging clone",
			invoke: func(ctx context.Context, c *Client, source *object.VirtualMachine) error {
				_, err := c.cloneTemplateSourceVMInner(ctx, TemplateCloneParams{
					SourceMoref: source.Reference().Value,
					VMName:      "hardware-read-failure-template",
					FolderPath:  simVMFolder,
					Network:     simNetwork,
				})
				return err
			},
		},
		{
			name: "deep health clone",
			invoke: func(ctx context.Context, c *Client, source *object.VirtualMachine) error {
				_, err := c.cloneForHealthCheckInner(ctx, HealthCheckCloneParams{
					SourceRef:    source.Reference().Value,
					CloneName:    "crucible-healthcheck-hardware-read-failure",
					FolderPath:   simVMFolder,
					Datastore:    simDatastore,
					ResourcePool: simResourcePool,
					Network:      simNetwork,
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
				_, _, source, _, _ := simulatorHostIsolationFixture(t, ctx, c)
				original := c.client.RoundTripper
				failure := &cloneHardwareReadFailure{
					next:        original,
					sourceMoref: source.Reference().Value,
				}
				c.client.RoundTripper = failure
				defer func() { c.client.RoundTripper = original }()

				err := tt.invoke(ctx, c, source)
				if err == nil || !strings.Contains(err.Error(), "inspect source VM hardware for clone policy") {
					t.Fatalf("clone error = %v, want source hardware inspection failure", err)
				}
				failure.assertNoClone(t)
			})
		})
	}
}

func destroySimulatorVM(t *testing.T, ctx context.Context, c *Client, moref string) {
	t.Helper()
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: moref,
	})
	task, err := vm.Destroy(ctx)
	if err != nil {
		t.Errorf("destroy simulator VM %s: %v", moref, err)
		return
	}
	if err := task.Wait(ctx); err != nil {
		t.Errorf("wait to destroy simulator VM %s: %v", moref, err)
	}
}
