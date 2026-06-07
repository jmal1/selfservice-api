package vcenter

// Package-internal integration tests that spin up a vCenter simulator
// (govmomi/simulator a.k.a. vcsim) so we can exercise the parts of
// RunScriptInGuest that actually talk to vSphere — without needing a real
// vCenter. The simulator's guest-operations surface is partial (file
// transfer URLs aren't wired through), so these tests focus on the
// preflight + dispatch error paths where we historically have the most
// regressions:
//
//   - VM not found by moref → meaningful error
//   - VM powered off → refuses to start
//   - VMware Tools not running → refuses to start
//   - Powered-on VM with tools → guest ops manager is reachable, and
//     RunScriptInGuest returns a guest-side error rather than a connection
//     or panic (the vcsim simulator does not implement the real file-
//     transfer URL pipeline, so the upload step itself fails — we treat
//     that as a fixture limitation and assert on the error string only).
//
// Marked t.Parallel() so they run alongside other vcenter tests without
// serializing the whole package. Each test gets its own simulator instance.

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// withSimulator boots a fresh vcsim VPX model and hands the test a *Client
// already connected to it, plus the underlying vim25 client so the test can
// reach in and tweak VM state (power, tools running, etc.). The Datacenter
// name is the simulator default "DC0" and is set on the finder so all our
// helpers work without modification.
func withSimulator(t *testing.T, fn func(ctx context.Context, c *Client, vimc *vim25.Client)) {
	t.Helper()

	model := simulator.VPX()
	// One DC + a couple of standalone hosts + a VM is plenty for these tests
	// and keeps boot time well under 500ms.
	model.Datacenter = 1
	model.Host = 1
	model.Machine = 2

	if err := model.Create(); err != nil {
		t.Fatalf("simulator.Create: %v", err)
	}
	t.Cleanup(model.Remove)

	srv := model.Service.NewServer()
	t.Cleanup(srv.Close)

	// srv.URL already ends in /sdk; using ParseURL+UserPassword is enough.
	u, err := url.Parse(srv.URL.String())
	if err != nil {
		t.Fatalf("parse simulator URL: %v", err)
	}
	u.User = url.UserPassword("user", "pass")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	gc, err := govmomi.NewClient(ctx, u, true)
	if err != nil {
		t.Fatalf("govmomi.NewClient against vcsim: %v", err)
	}

	finder := find.NewFinder(gc.Client, true)
	dc, err := finder.Datacenter(ctx, "DC0")
	if err != nil {
		t.Fatalf("find DC0: %v", err)
	}
	finder.SetDatacenter(dc)

	c := &Client{
		config: Config{
			URL:        u.String(),
			User:       "user",
			Password:   "pass",
			Datacenter: "DC0",
			Insecure:   true,
		},
		client:     gc,
		finder:     finder,
		datacenter: dc,
		logger:     slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	t.Cleanup(func() { c.Disconnect(context.Background()) })

	fn(ctx, c, gc.Client)
}

// firstVM returns the moref string of the first VM in the simulator. The
// canonical name is "DC0_H0_VM0" but we resolve it through the finder so
// the test still passes if simulator naming ever changes upstream.
func firstVM(t *testing.T, ctx context.Context, vimc *vim25.Client) (*object.VirtualMachine, string) {
	t.Helper()
	finder := find.NewFinder(vimc, true)
	dc, err := finder.Datacenter(ctx, "DC0")
	if err != nil {
		t.Fatalf("find DC0 (firstVM): %v", err)
	}
	finder.SetDatacenter(dc)
	vms, err := finder.VirtualMachineList(ctx, "*")
	if err != nil {
		t.Fatalf("VirtualMachineList: %v", err)
	}
	if len(vms) == 0 {
		t.Fatal("simulator created no VMs; bump model.Machine")
	}
	return vms[0], vms[0].Reference().Value
}

func TestRunScriptInGuest_VCsim_UnknownMoref(t *testing.T) {
	t.Parallel()
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		_, err := c.RunScriptInGuest(ctx, GuestExecRequest{
			VMMoref:       "vm-does-not-exist-9999",
			GuestUser:     "student",
			GuestPassword: "hunter2",
			Language:      "bash",
			Script:        "echo hi",
			RunID:         "test-run",
			ActionSlug:    "noop",
			Timeout:       3 * time.Second,
		})
		if err == nil {
			t.Fatal("expected error for unknown moref, got nil")
		}
		// vmFromMoref wraps the moref; the simulator returns ManagedObjectNotFound
		// for a bogus VirtualMachine. Either substring is acceptable; we just
		// need a useful, not-panic error.
		msg := err.Error()
		if !strings.Contains(msg, "vm-does-not-exist-9999") && !strings.Contains(msg, "ManagedObjectNotFound") && !strings.Contains(msg, "not found") {
			t.Errorf("error %q should mention the unknown moref or 'not found'", msg)
		}
	})
}

func TestRunScriptInGuest_VCsim_PoweredOff(t *testing.T) {
	t.Parallel()
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		vm, moref := firstVM(t, ctx, vimc)
		// vcsim VMs start powered off; explicitly leave them that way.
		// Just double-check via VM properties to make the test intent obvious.
		var mvm mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"runtime"}, &mvm); err != nil {
			t.Fatalf("read vm runtime: %v", err)
		}
		if mvm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
			task, err := vm.PowerOff(ctx)
			if err != nil {
				t.Fatalf("PowerOff: %v", err)
			}
			if err := task.Wait(ctx); err != nil {
				t.Fatalf("wait PowerOff: %v", err)
			}
		}

		_, err := c.RunScriptInGuest(ctx, GuestExecRequest{
			VMMoref:       moref,
			GuestUser:     "student",
			GuestPassword: "hunter2",
			Language:      "bash",
			Script:        "echo hi",
			RunID:         "test-run",
			ActionSlug:    "noop",
			Timeout:       3 * time.Second,
		})
		if err == nil {
			t.Fatal("expected error for powered-off VM, got nil")
		}
		if !strings.Contains(err.Error(), "not powered on") {
			t.Errorf("error %q should mention 'not powered on'", err.Error())
		}
	})
}

func TestRunScriptInGuest_VCsim_PoweredOnNoTools(t *testing.T) {
	t.Parallel()
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		vm, moref := firstVM(t, ctx, vimc)
		if err := ensurePoweredOn(ctx, vm); err != nil {
			t.Fatalf("ensurePoweredOn: %v", err)
		}

		// vcsim's default Guest.ToolsRunningStatus is empty/unknown — the
		// preflight in runScriptInGuestInner should reject this.
		_, err := c.RunScriptInGuest(ctx, GuestExecRequest{
			VMMoref:       moref,
			GuestUser:     "student",
			GuestPassword: "hunter2",
			Language:      "bash",
			Script:        "echo hi",
			RunID:         "test-run",
			ActionSlug:    "noop",
			Timeout:       3 * time.Second,
		})
		if err == nil {
			t.Fatal("expected error for VM without VMware Tools, got nil")
		}
		if !strings.Contains(err.Error(), "VMware Tools") {
			t.Errorf("error %q should mention 'VMware Tools'", err.Error())
		}
	})
}

// Note: a "PoweredOn + Tools Running" happy-path test was attempted here but
// the vcsim file-transfer URL pipeline isn't fully simulated, and the
// internal simulator.Map() helper requires a private *simulator.Context that
// we can't synthesize from a regular client test. The full StartProgram path
// is already covered by the unit tests in vmwaretools_dispatcher_test.go
// using a mock GuestExecutor, so we stop the vcsim coverage at preflight.

// ensurePoweredOn brings the VM up if it isn't already. vcsim defaults to
// Autostart=true on some model versions and false on others, so we have to
// be tolerant of either initial state.
func ensurePoweredOn(ctx context.Context, vm *object.VirtualMachine) error {
	var mvm mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime"}, &mvm); err != nil {
		return err
	}
	if mvm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn {
		return nil
	}
	task, err := vm.PowerOn(ctx)
	if err != nil {
		return err
	}
	return task.Wait(ctx)
}
