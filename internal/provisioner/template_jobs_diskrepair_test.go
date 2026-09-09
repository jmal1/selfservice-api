package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// The exact fault vCenter returns when it opens a VM's system disk over a
// 0-byte flat — the signature vcenter.IsDiskNotReadyErr classifies as
// "recreate the disk". Hard-coding the real string keeps this test honest with
// the matcher it depends on.
const probeDiskBrokenErr = "open system disk [NAS-vmstore] tpl/tpl.vmdk on vm-1: ServerFaultCode: The file specified is not a virtual disk"

// unattendedDiskRepairPayload returns a payload on the UNATTENDED branch, which
// is the only branch that still recreates the system disk after a failed
// power-on. The manual branch deliberately does not (see the defer tests
// below), so the recreate-and-retry contract has to be exercised here.
func unattendedDiskRepairPayload() TemplateProvisionPayload {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeCloudInitCIData
	payload.UnattendConfig = json.RawMessage(`{"Hostname":"ubuntu-lab","Password":"S3edP@ss-not-a-real-secret"}`) // pragma: allowlist-secret
	return payload
}

// recordProgress returns a progress sink plus a pointer to the recorded steps,
// so a test can assert on the operator-visible phase names and wording.
func recordProgress() (func(step, message string), *[]string, *map[string]string) {
	steps := []string{}
	messages := map[string]string{}
	return func(step, message string) {
		steps = append(steps, step)
		messages[step] = message
	}, &steps, &messages
}

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
	// Ordering is the whole contract: verify -> prove the VM is off -> repair ->
	// re-verify -> power on. The guest_info read must come BEFORE the recreate,
	// because the recreate destroys the disk's backing files.
	wantSeq := []string{"create", "probe", "guest_info", "recreate", "probe", "power_on"}
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
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}

// TestProvisionFromISO_RepairsBrokenDiskOnPowerOnFallback covers the belt-and-
// suspenders path on the UNATTENDED branch: the pre-power-on probe passed (the
// open-time query did not fault on this particular truncation) but the
// hypervisor's own disk-open on power-on DID fault with the broken-flat
// signature. An unattended install cannot start without a power-on, so here the
// provision path must classify that error, recreate the disk once, re-probe, and
// retry the power-on rather than erroring.
//
// This is deliberately NOT the manual branch: manual mode defers instead of
// recreating, because it hands the VM to an operator who powers it on anyway.
// See TestProvisionFromISO_ManualDefersPowerOnWhenDiskStillAllocating.
func TestProvisionFromISO_RepairsBrokenDiskOnPowerOnFallback(t *testing.T) {
	payload := unattendedDiskRepairPayload()
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
	if vc.powerOnCalls != 3 {
		t.Errorf("expected three power-ons (fault, success after recreate, then the installed-system boot), got %d", vc.powerOnCalls)
	}
	// upload(seed) -> create -> probe(clean) -> power_on(fault) ->
	// guest_info(off) -> recreate -> probe(confirm) -> power_on(ok) -> install.
	wantSeq := []string{
		"upload", "create", "probe", "power_on", "guest_info", "recreate", "probe", "power_on",
		"wait_power_off", "detach", "power_on", "wait_tools",
	}
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
//
// Unattended branch, for the same reason as the test above.
func TestProvisionFromISO_RecreateIsOnceAcrossProbeAndPowerOn(t *testing.T) {
	payload := unattendedDiskRepairPayload()
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
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
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
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}

// TestProvisionFromISO_ManualDefersPowerOnWhenDiskStillAllocating is the core
// guard for the Linux Mint incident.
//
// A manual ISO build errored with "power on after disk recreate: disk still not
// readable after 20 attempts over ~57s" while the staging VM was in fact fine:
// the operator powered it on by hand minutes later and completed the install in
// the vCenter console. Because `error` has only one outbound edge (error→draft)
// and cancel refused the error state, that finished guest was stranded on a
// template with no way to hand it back — and the orphan reconciler destroys
// error-state rows that still hold a moref.
//
// Manual mode's contract is to hand over a VM with the installer mounted; the
// operator supplies the power-on. So a datastore that has not finished
// allocating the flat must NOT fail the job and must NOT recreate the disk
// (which would destroy the flat being allocated and restart the wait).
func TestProvisionFromISO_ManualDefersPowerOnWhenDiskStillAllocating(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	// Probe clean, power-on faults disk-not-ready and keeps faulting.
	vc.powerErr = errors.New(probeDiskBrokenErr)
	prog, steps, messages := recordProgress()

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), prog, payload); err != nil {
		t.Fatalf("a manual build whose disk was still allocating must still reach configuring, got error: %v", err)
	}

	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Fatalf("final template state = %q, want %q — the operator must be able to drive the console install", got, models.TemplateStateConfiguring)
	}
	if vc.recreateCalls != 0 {
		t.Errorf("manual mode must NOT recreate the disk on a power-on fault (that destroys the flat being allocated), got %d recreates", vc.recreateCalls)
	}
	if vc.powerOnCalls != 1 {
		t.Errorf("expected a single power-on attempt with no retry-after-recreate, got %d", vc.powerOnCalls)
	}
	wantSeq := []string{"create", "probe", "power_on"}
	if !reflect.DeepEqual(vc.seq, wantSeq) {
		t.Errorf("call sequence = %v, want %v", vc.seq, wantSeq)
	}
	// The operator has to learn that the first action is theirs.
	if !containsStep(*steps, "power_on_deferred") {
		t.Errorf("expected a power_on_deferred progress step so the wizard explains the VM is off; got %v", *steps)
	}
	handover := (*messages)["await_manual_install"]
	if handover == "" {
		t.Fatal("expected the await_manual_install handover message to still be published")
	}
	if !strings.Contains(strings.ToUpper(handover), "POWERED OFF") {
		t.Errorf("handover message must tell the operator the VM is powered off, got %q", handover)
	}
}

// TestProvisionFromISO_ManualDefersAfterProbeRepair pins down which check is the
// fail-closed gate. The probe is: a disk that cannot be opened even after one
// recreate fails the job before the operator invests a manual install. The
// power-on is not: once the probe is satisfied, a host-side disk-open fault is
// a slow datastore, and manual mode defers rather than erroring — even when a
// recreate already happened for the probe.
func TestProvisionFromISO_ManualDefersAfterProbeRepair(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr)}
	vc.powerErr = errors.New(probeDiskBrokenErr)

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload); err != nil {
		t.Fatalf("manual build must reach configuring once the disk probes clean, got error: %v", err)
	}

	if vc.recreateCalls != 1 {
		t.Errorf("the probe-driven repair should still run exactly once, got %d", vc.recreateCalls)
	}
	if vc.powerOnCalls != 1 {
		t.Errorf("the deferred power-on must not be retried after a recreate, got %d", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionFromISO_RefusesDiskRecreateOnPoweredOnVM sabotage-proves the
// data-loss guard. RecreateSystemDisk destroys the disk's backing files, so it
// may only ever run against a shell that has never booted. If a powered-on VM
// ever reaches the repair helper, the job must fail closed rather than delete
// an OS an operator installed by hand.
func TestProvisionFromISO_RefusesDiskRecreateOnPoweredOnVM(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr)}
	vc.guestPoweredOn = true

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("recreating the system disk of a powered-on VM must fail the provision, not proceed")
	}
	if !strings.Contains(err.Error(), "powered on") {
		t.Errorf("error should explain the VM was powered on, got %q", err)
	}
	if vc.recreateCalls != 0 {
		t.Fatalf("must NOT destroy the disk of a powered-on VM, got %d recreates", vc.recreateCalls)
	}
	if vc.powerOnCalls != 0 {
		t.Errorf("must not power on after refusing the repair, got %d", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}

// TestProvisionFromISO_UnreadablePowerStateBlocksDiskRecreate keeps the guard
// fail-closed: if we cannot establish that the VM is powered off, we do not get
// to assume it is.
func TestProvisionFromISO_UnreadablePowerStateBlocksDiskRecreate(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)
	vc.probeErrs = []error{errors.New(probeDiskBrokenErr)}
	vc.guestInfoErr = errors.New("ServerFaultCode: connection reset by peer")

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("an unreadable power state must block the destroy, not be treated as powered off")
	}
	if vc.recreateCalls != 0 {
		t.Fatalf("must NOT destroy the disk when the power state is unknown, got %d recreates", vc.recreateCalls)
	}
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}

func containsStep(steps []string, want string) bool {
	for _, s := range steps {
		if s == want {
			return true
		}
	}
	return false
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
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}
