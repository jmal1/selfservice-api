package provisioner

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// The exact fault vCenter returns when it opens a VM's system disk over a
// 0-byte flat — the signature vcenter.IsDiskNotReadyErr classifies as
// "recreate the disk". Hard-coding the real string keeps this test honest with
// the matcher it depends on.
const probeDiskBrokenErr = "open system disk [NAS-vmstore] tpl/tpl.vmdk on vm-1: ServerFaultCode: The file specified is not a virtual disk"

// TestProvisionFromISO_RepairsBrokenDiskBeforePowerOn is the core guard for the
// fix: when the pre-power-on probe reports the system disk broken, the provision
// path must recreate the disk ONCE, re-probe to confirm it is now readable, and
// only THEN power the VM on — exactly once. A single power-on proves we never
// hand a broken disk to vCenter (which is what produced the
// "Module 'Disk' power on failed" storm), and the recreate/re-probe ordering
// proves the repair is verified before power-on rather than hoped-for.
func TestProvisionFromISO_RepairsBrokenDiskBeforePowerOn(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	// Broken on the first probe, readable on the re-probe after recreate.
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr)}

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload); err != nil {
		t.Fatalf("provision with a repairable disk returned error: %v", err)
	}

	if vc.recreateCalls != 1 {
		t.Fatalf("broken disk must be recreated exactly once, got %d", vc.recreateCalls)
	}
	if vc.recreateGB != payload.DiskGB {
		t.Errorf("RecreateSystemDisk diskGB = %d, want the payload's %d", vc.recreateGB, payload.DiskGB)
	}
	if vc.probeCalls != 2 {
		t.Errorf("expected two probes (broken, then confirm after recreate), got %d", vc.probeCalls)
	}
	if vc.powerOnCalls != 1 {
		t.Errorf("VM must be powered on exactly once (never with a broken disk), got %d", vc.powerOnCalls)
	}
	// Ordering is the whole contract: verify -> repair -> re-verify -> power on.
	wantSeq := []string{"create", "probe", "recreate", "probe", "power_on"}
	if !reflect.DeepEqual(vc.seq, wantSeq) {
		t.Errorf("call sequence = %v, want %v", vc.seq, wantSeq)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionFromISO_HealthyDiskSkipsRecreate proves the common case adds only
// a cheap probe: a disk that opens on the first GET is powered on directly with
// no recreate.
func TestProvisionFromISO_HealthyDiskSkipsRecreate(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload); err != nil {
		t.Fatalf("provision with a healthy disk returned error: %v", err)
	}

	if vc.probeCalls != 1 {
		t.Errorf("healthy disk should be probed exactly once, got %d", vc.probeCalls)
	}
	if vc.recreateCalls != 0 {
		t.Errorf("healthy disk must NOT be recreated, got %d recreate calls", vc.recreateCalls)
	}
	if vc.powerOnCalls != 1 {
		t.Errorf("expected exactly one power-on, got %d", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionFromISO_NonDiskProbeErrorDoesNotRecreate is the guard that keeps
// the repair narrow. A probe failure that is NOT the broken-flat signature
// (here: the VM vanished) must surface as an error WITHOUT a recreate and
// WITHOUT a power-on — recreating a disk on an unrelated fault would hide a real
// problem and waste a datastore round-trip.
func TestProvisionFromISO_NonDiskProbeErrorDoesNotRecreate(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	vc.probeErrs = []error{errors.New("ServerFaultCode: ManagedObjectNotFound: the VM was deleted")}

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("a non-disk probe failure must fail the provision, not be swallowed")
	}
	if vc.recreateCalls != 0 {
		t.Errorf("a non-disk probe error must NOT trigger a recreate, got %d", vc.recreateCalls)
	}
	if vc.powerOnCalls != 0 {
		t.Errorf("must NOT power on after a probe error, got %d power-on calls", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
}

// TestProvisionFromISO_RepairsBrokenDiskOnPowerOnFallback covers the belt-and-
// suspenders path: the pre-power-on probe passed (the open-time query did not
// fault on this particular truncation) but the hypervisor's own disk-open on
// power-on DID fault with the broken-flat signature. The provision path must
// classify that power-on error, recreate the disk once, re-probe, and retry the
// power-on so the template self-heals instead of erroring — this is the
// "keep isDiskNotReadyErr as a fallback classifier" contract.
func TestProvisionFromISO_RepairsBrokenDiskOnPowerOnFallback(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	// Probe is clean; the first power-on faults disk-not-ready, the retry (after
	// recreate) succeeds.
	vc.powerErrs = []error{errors.New(probeDiskBrokenErr)}

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload); err != nil {
		t.Fatalf("provision that self-heals on the power-on fallback returned error: %v", err)
	}

	if vc.recreateCalls != 1 {
		t.Fatalf("power-on disk fault must recreate the disk exactly once, got %d", vc.recreateCalls)
	}
	if vc.powerOnCalls != 2 {
		t.Errorf("expected two power-ons (fault, then success after recreate), got %d", vc.powerOnCalls)
	}
	// probe(clean) -> power_on(fault) -> recreate -> probe(confirm) -> power_on(ok).
	wantSeq := []string{"create", "probe", "power_on", "recreate", "probe", "power_on"}
	if !reflect.DeepEqual(vc.seq, wantSeq) {
		t.Errorf("call sequence = %v, want %v", vc.seq, wantSeq)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionFromISO_RecreateIsOnceAcrossProbeAndPowerOn proves the one-shot
// guard spans BOTH repair triggers: if the probe recreates the disk and the
// power-on STILL faults disk-not-ready afterward, the job fails rather than
// recreating a second time. A broken flat that survives one recreate is a real
// failure, not something to loop on.
func TestProvisionFromISO_RecreateIsOnceAcrossProbeAndPowerOn(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	// Probe: broken, then readable after recreate. Power-on: still faults.
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr)}
	vc.powerErrs = []error{errors.New(probeDiskBrokenErr)}

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("a disk that still faults on power-on after a recreate must fail the provision")
	}
	if vc.recreateCalls != 1 {
		t.Errorf("recreate must happen exactly once across both triggers, got %d", vc.recreateCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
}

// TestProvisionFromISO_NonDiskPowerOnErrorDoesNotRecreate keeps the power-on
// fallback narrow: a power-on failure that is NOT the broken-flat signature must
// surface as an error without a recreate.
func TestProvisionFromISO_NonDiskPowerOnErrorDoesNotRecreate(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	vc.powerErrs = []error{errors.New("ServerFaultCode: InvalidPowerState: another operation is in progress")}

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("a non-disk power-on failure must fail the provision")
	}
	if vc.recreateCalls != 0 {
		t.Errorf("a non-disk power-on error must NOT trigger a recreate, got %d", vc.recreateCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
}

// TestProvisionFromISO_StillBrokenAfterRecreateFails proves the repair is
// one-shot: if the disk is STILL unreadable after a recreate, the job fails
// rather than looping or powering on a broken disk.
func TestProvisionFromISO_StillBrokenAfterRecreateFails(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	// Broken on both the initial probe and the post-recreate re-probe.
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr), errors.New(probeDiskBrokenErr)}

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("a disk still broken after recreate must fail the provision")
	}
	if vc.recreateCalls != 1 {
		t.Errorf("recreate must be attempted exactly once (no retry loop), got %d", vc.recreateCalls)
	}
	if vc.powerOnCalls != 0 {
		t.Errorf("must NOT power on a disk that is still broken, got %d power-on calls", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
}
