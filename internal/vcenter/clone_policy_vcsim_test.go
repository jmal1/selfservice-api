package vcenter

import (
	"context"
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
	next        soap.RoundTripper
	sourceMoref string
	injectVTPM  bool
	wantPolicy  string

	mu       sync.Mutex
	policies map[string]string
}

func (s *clonePolicySaboteur) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.RetrievePropertiesExBody); ok &&
		s.injectVTPM &&
		requestsVMHardware(body.Req, s.sourceMoref) {
		if err := s.next.RoundTrip(ctx, req, res); err != nil {
			return err
		}
		return injectVirtualTPM(res, s.sourceMoref)
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
		s.mu.Lock()
		s.policies[body.Req.Name] = got
		s.mu.Unlock()

		// vcsim predates the vSphere 8 TPM clone policy behavior. Strip the
		// already-verified field before forwarding so it can materialize the VM.
		body.Req.Spec.TpmProvisionPolicy = ""
	}
	return s.next.RoundTrip(ctx, req, res)
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

				original := c.client.RoundTripper
				saboteur := &clonePolicySaboteur{
					next:        original,
					sourceMoref: source.Reference().Value,
					injectVTPM:  tt.injectVTPM,
					wantPolicy:  tt.wantPolicy,
					policies:    make(map[string]string),
				}
				c.client.RoundTripper = saboteur
				defer func() { c.client.RoundTripper = original }()

				var created []string
				defer func() {
					for _, moref := range created {
						destroySimulatorVM(t, ctx, c, moref)
					}
				}()

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

				saboteur.assertCloneNames(t, durableName, templateName, healthName)
				assertVMOnHost(t, ctx, c, durableMoref, allowed.MoRef)
				assertVMOnHost(t, ctx, c, templateMoref, allowed.MoRef)
				assertVMOnHost(t, ctx, c, health.MoRef, allowed.MoRef)
			})
		})
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
