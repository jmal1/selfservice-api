package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/jmal1/selfservice-api/internal/models"
)

// fakeSkipVC records power-off / snapshot and never exposes GuestOps methods.
type fakeSkipVC struct {
	powerState    types.VirtualMachinePowerState
	getErr        error
	powerOffErr   error
	snapshotErr   error
	powerOffCalls int
	snapshotCalls int
	snapshotName  string
}

func (f *fakeSkipVC) GetVM(_ context.Context, _ string) (*mo.VirtualMachine, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &mo.VirtualMachine{Runtime: types.VirtualMachineRuntimeInfo{PowerState: f.powerState}}, nil
}

func (f *fakeSkipVC) PowerOffVM(_ context.Context, _ string) error {
	f.powerOffCalls++
	if f.powerOffErr != nil {
		return f.powerOffErr
	}
	f.powerState = types.VirtualMachinePowerStatePoweredOff
	return nil
}

func (f *fakeSkipVC) CreateVMSnapshot(_ context.Context, _, name, _ string) (string, error) {
	f.snapshotCalls++
	f.snapshotName = name
	return "snapshot-1", f.snapshotErr
}

type fakeSkipDB struct {
	from, to string
	calls    int
	err      error
}

func (f *fakeSkipDB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, from, to string) error {
	f.calls++
	f.from, f.to = from, to
	return f.err
}

func TestSkipGeneralizeAndFinalize_PowerOffSnapshotReady_NoGuestOps(t *testing.T) {
	vc := &fakeSkipVC{powerState: types.VirtualMachinePowerStatePoweredOn}
	db := &fakeSkipDB{}
	payload := &TemplateGeneralizePayload{
		TemplateID: uuid.New(),
		VMMoref:    "vm-ova",
	}

	if err := skipGeneralizeAndFinalize(context.Background(), vc, db, nil, nil, payload, "base-image"); err != nil {
		t.Fatalf("skipGeneralizeAndFinalize: %v", err)
	}
	if vc.powerOffCalls != 1 {
		t.Errorf("PowerOffVM calls = %d; want 1", vc.powerOffCalls)
	}
	if vc.snapshotCalls != 1 {
		t.Errorf("CreateVMSnapshot calls = %d; want 1", vc.snapshotCalls)
	}
	if vc.snapshotName != "base-image" {
		t.Errorf("snapshot name = %q; want base-image", vc.snapshotName)
	}
	if db.calls != 1 || db.from != models.TemplateStateGeneralizing || db.to != models.TemplateStateReady {
		t.Errorf("lifecycle = %s→%s (calls=%d); want generalizing→ready", db.from, db.to, db.calls)
	}
	if payload.GuestUsername != "[redacted]" || payload.GuestPassword != "[redacted]" {
		t.Errorf("payload creds not scrubbed: user=%q", payload.GuestUsername)
	}
}

func TestSkipGeneralizeAndFinalize_AlreadyOffSkipsPowerOff(t *testing.T) {
	vc := &fakeSkipVC{powerState: types.VirtualMachinePowerStatePoweredOff}
	db := &fakeSkipDB{}
	payload := &TemplateGeneralizePayload{TemplateID: uuid.New(), VMMoref: "vm-off"}

	if err := skipGeneralizeAndFinalize(context.Background(), vc, db, nil, nil, payload, "base-image"); err != nil {
		t.Fatalf("skipGeneralizeAndFinalize: %v", err)
	}
	if vc.powerOffCalls != 0 {
		t.Errorf("PowerOffVM calls = %d; want 0 when already powered off", vc.powerOffCalls)
	}
	if vc.snapshotCalls != 1 {
		t.Errorf("CreateVMSnapshot calls = %d; want 1", vc.snapshotCalls)
	}
	if db.to != models.TemplateStateReady {
		t.Errorf("to = %q; want ready", db.to)
	}
}

func TestPlanGeneralize_SkipTrueDoesNotRequireCredentials(t *testing.T) {
	tmpl := &models.Template{SkipGeneralize: true}
	payload := &TemplateGeneralizePayload{} // empty creds
	skip, err := planGeneralize(tmpl, payload)
	if err != nil {
		t.Fatalf("planGeneralize: %v", err)
	}
	if !skip {
		t.Fatal("skip_generalize=true must skip GuestOps")
	}
}

func TestPlanGeneralize_SkipFalseStillRequiresCredentialsAndGuestOps(t *testing.T) {
	tmpl := &models.Template{SkipGeneralize: false}
	_, err := planGeneralize(tmpl, &TemplateGeneralizePayload{})
	if err == nil {
		t.Fatal("skip_generalize=false with empty creds must fail")
	}
	if !strings.Contains(err.Error(), "guest_username") {
		t.Errorf("error = %q; want guest_username required", err)
	}

	skip, err := planGeneralize(tmpl, &TemplateGeneralizePayload{
		GuestUsername: "student",
		GuestPassword: "pw",
		OSType:        "linux",
	})
	if err != nil {
		t.Fatalf("planGeneralize with creds: %v", err)
	}
	if skip {
		t.Fatal("skip_generalize=false must enter the GuestOps generalize path")
	}
}

func TestPlanGeneralize_SkipFalseRejectsUnknownOS(t *testing.T) {
	_, err := planGeneralize(&models.Template{}, &TemplateGeneralizePayload{
		GuestUsername: "u",
		GuestPassword: "p",
		OSType:        "plan9",
	})
	if err == nil {
		t.Fatal("unknown os_type must fail on the GuestOps path")
	}
}

func TestIsAlreadyPoweredOffErr(t *testing.T) {
	if !isAlreadyPoweredOffErr(errors.New("InvalidPowerState")) {
		t.Error("InvalidPowerState should count as already off")
	}
	if !isAlreadyPoweredOffErr(fmt.Errorf("VM is powered off")) {
		t.Error("powered off text should count")
	}
	if isAlreadyPoweredOffErr(errors.New("permission denied")) {
		t.Error("unrelated error must not count as already off")
	}
	if isAlreadyPoweredOffErr(nil) {
		t.Error("nil must not count as already off")
	}
}
