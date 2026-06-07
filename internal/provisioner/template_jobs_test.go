package provisioner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	for _, r := range []string{"sysprep.exe", "/generalize", "/oobe", "/shutdown"} {
		if !strings.Contains(got, r) {
			t.Errorf("Windows generalize script missing %q\nscript:\n%s", r, got)
		}
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
