package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TestGeneralizeScript_LinuxContainsCriticalSteps locks in the contract
// that the Linux generalize script wipes machine-id, SSH host keys, and
// cloud-init state, then shuts down. Any one of these missing means a
// student VM cloned from this template would either fail to boot
// (missing entropy) or, worse, share a machine-id with another student.
func TestGeneralizeScript_LinuxContainsCriticalSteps(t *testing.T) {
	got := generalizeScript("linux")
	required := []string{
		"cloud-init clean",
		"truncate -s 0 /etc/machine-id",
		"/var/lib/dbus/machine-id",
		"/etc/ssh/ssh_host_",
		"shutdown -h now",
	}
	for _, r := range required {
		if !strings.Contains(got, r) {
			t.Errorf("Linux generalize script missing %q\nscript:\n%s", r, got)
		}
	}
}

// TestGeneralizeScript_WindowsRunsSysprep ensures the Windows path
// dispatches sysprep with /generalize /oobe /shutdown. Without all three
// flags, sysprep either won't generalize (leaving SID intact — every
// clone gets the same SID, breaking AD) or won't shut down (worker
// times out waiting for power-off).
func TestGeneralizeScript_WindowsRunsSysprep(t *testing.T) {
	got := generalizeScript("windows")
	for _, r := range []string{"sysprep.exe", "/generalize", "/oobe", "/shutdown", `/unattend:C:\Windows\Panther\unattend.xml`} {
		if !strings.Contains(got, r) {
			t.Errorf("Windows generalize script missing %q\nscript:\n%s", r, got)
		}
	}
	// Reserved-storage must be disabled BEFORE sysprep, or feature-updated
	// Windows 11 sysprep fails with 0x800F0975 and (fire-and-forget) the
	// VM silently never powers off -> 10-min wait_shutdown timeout.
	for _, r := range []string{"ReserveManager", "ActiveScenario", "/Set-ReservedStorageState /State:Disabled"} {
		if !strings.Contains(got, r) {
			t.Errorf("Windows generalize script missing reserved-storage prep %q\nscript:\n%s", r, got)
		}
	}
	if i, j := strings.Index(got, "/Set-ReservedStorageState"), strings.Index(got, "sysprep.exe"); i < 0 || j < 0 || i > j {
		t.Errorf("reserved-storage disable must run before sysprep (idx dism=%d sysprep=%d)\nscript:\n%s", i, j, got)
	}
}

// TestPowerOffTimeout locks in that Windows gets a much longer power-off wait
// than Linux. Sysprep's generalize pass on feature-updated Windows 11 can take
// 12-20 min before the guest powers off; a 10-min wait (the old value) marked
// the template 'error' while sysprep was still finishing, producing a
// successfully-generalized-but-errored template. Linux shutdown is seconds.
func TestPowerOffTimeout(t *testing.T) {
	if got := powerOffTimeout("windows"); got < 20*time.Minute {
		t.Errorf("windows powerOffTimeout = %s, want >= 20m (sysprep on Win11 is slow)", got)
	}
	if got := powerOffTimeout("Windows"); got < 20*time.Minute {
		t.Errorf("powerOffTimeout should be case-insensitive; Windows = %s", got)
	}
	if got := powerOffTimeout("linux"); got != 10*time.Minute {
		t.Errorf("linux powerOffTimeout = %s, want 10m", got)
	}
	if powerOffTimeout("windows") <= powerOffTimeout("linux") {
		t.Errorf("windows wait must exceed linux wait")
	}
}

func TestGeneralizeScript_UnknownOSDefaultsToLinux(t *testing.T) {
	got := generalizeScript("plan9")
	if !strings.Contains(got, "shutdown -h now") {
		t.Errorf("unknown OS should fall back to Linux script, got:\n%s", got)
	}
}

// TestIsExpectedShutdownErr enumerates the "VM powered off underneath
// us" failure modes that the generalize worker should NOT propagate
// as failures. False positives here would log noise; false negatives
// would mark successful generalize runs as failed.
func TestIsExpectedShutdownErr(t *testing.T) {
	expected := []string{
		"connection reset by peer",
		"guest powered off during operation",
		"tools not running",
		"operation was canceled",
		"CONNECTION RESET",
	}
	for _, msg := range expected {
		if !isExpectedShutdownErr(errors.New(msg)) {
			t.Errorf("%q should be flagged as expected shutdown error", msg)
		}
	}
	unexpected := []string{
		"permission denied",
		"insufficient privileges",
		"out of disk space",
		"",
	}
	for _, msg := range unexpected {
		if isExpectedShutdownErr(errors.New(msg)) {
			t.Errorf("%q should NOT be flagged as expected shutdown", msg)
		}
	}
	if isExpectedShutdownErr(nil) {
		t.Error("nil error must not be flagged as expected shutdown")
	}
}

// TestTemplateProvisionPayload_JSONRoundTrip locks in the wire shape of
// the job payload. Changes here cascade to the API handlers that build
// the payload AND to any in-flight jobs at the time of deploy, so this
// test fires loudly on any unintentional field rename.
func TestTemplateProvisionPayload_JSONRoundTrip(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	original := TemplateProvisionPayload{
		TemplateID:     id,
		SourceType:     models.TemplateSourceCloneTemplate,
		SourceRef:      "vm-1234",
		VMName:         "tpl-myKali-abc123",
		FolderPath:     "JMAL-Datacenter/vm/Templates",
		StagingNetwork: "LabVMs-VLAN30",
		VCPUs:          4,
		RAMmb:          8192,
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check JSON field names match what API handlers send.
	for _, want := range []string{
		`"template_id"`,
		`"source_type":"clone_template"`,
		`"source_ref":"vm-1234"`,
		`"vm_name":"tpl-myKali-abc123"`,
		`"folder_path":"JMAL-Datacenter/vm/Templates"`,
		`"staging_network":"LabVMs-VLAN30"`,
		`"vcpus":4`,
		`"ram_mb":8192`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("provision payload JSON missing %s\ngot: %s", want, b)
		}
	}

	var roundTripped TemplateProvisionPayload
	if err := json.Unmarshal(b, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTripped != original {
		t.Errorf("round-trip mismatch\nwant: %+v\ngot:  %+v", original, roundTripped)
	}
}

// TestTemplateGeneralizePayload_JSONRoundTripIncludesCredentials covers
// the OS/credential shape worker reads. NOTE: credentials live in the
// payload — they get scrubbed in the job RESULT after completion (see
// SECURITY note on the struct), not the payload itself.
func TestTemplateGeneralizePayload_JSONRoundTripIncludesCredentials(t *testing.T) {
	original := TemplateGeneralizePayload{
		TemplateID:    uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
		OSType:        "linux",
		GuestUsername: "admin",
		GuestPassword: "hunter2!", // pragma: allowlist-secret
		VMMoref:       "vm-9999",
		SnapshotName:  "base-image",
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"os_type":"linux"`,
		`"guest_username":"admin"`,
		`"guest_password":"hunter2!"`,
		`"vm_moref":"vm-9999"`,
		`"snapshot_name":"base-image"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("generalize payload JSON missing %s\ngot: %s", want, b)
		}
	}
	var rt TemplateGeneralizePayload
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt != original {
		t.Errorf("round-trip mismatch\nwant: %+v\ngot:  %+v", original, rt)
	}
}

// TestPollGuestCredentials_SucceedsOnceValid proves the smoke-gate poll
// tolerates transient failures (guest not ready / mid-reboot / still on the
// bootstrap password) and returns nil as soon as the guest accepts the
// credentials — the signal that cloudbase-init/cloud-init reset the account.
func TestPollGuestCredentials_SucceedsOnceValid(t *testing.T) {
	calls := 0
	validate := func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("guest not ready yet")
		}
		return nil
	}
	if err := pollGuestCredentials(context.Background(), validate, time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected success once credentials valid, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts before success, got %d", calls)
	}
}

// TestPollGuestCredentials_TimesOutWithLastErr proves that when the guest
// NEVER accepts the credentials (e.g. cloudbase-init disabled in the golden
// image, so the password is never reset), the poll fails with the last
// underlying error rather than hanging or falsely passing. This is the case
// that must fail the publish gate instead of surfacing at L3.
func TestPollGuestCredentials_TimesOutWithLastErr(t *testing.T) {
	sentinel := errors.New("InvalidGuestLogin")
	validate := func(context.Context) error { return sentinel }
	err := pollGuestCredentials(context.Background(), validate, 5*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil (broken image would pass the gate)")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected last underlying error to be surfaced, got %v", err)
	}
}

// TestPollGuestCredentials_HonorsContextCancel ensures a cancelled job
// context aborts the poll promptly instead of blocking for the full timeout.
func TestPollGuestCredentials_HonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	validate := func(context.Context) error { return errors.New("still failing") }
	err := pollGuestCredentials(ctx, validate, time.Hour, 10*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}
