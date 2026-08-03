package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/unattend"
	"github.com/jmal1/selfservice-api/internal/vcenter"
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
		DiskGB:         40,
		GuestID:        "ubuntu64Guest",
		UnattendMode:   models.UnattendModeCloudInitCIData,
		UnattendConfig: json.RawMessage(`{"Hostname":"kali-lab"}`),
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
		`"disk_gb":40`,
		`"guest_id":"ubuntu64Guest"`,
		`"unattend_mode":"cloudinit_cidata"`,
		`"unattend_config":{"Hostname":"kali-lab"}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("provision payload JSON missing %s\ngot: %s", want, b)
		}
	}

	var roundTripped TemplateProvisionPayload
	if err := json.Unmarshal(b, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// TemplateProvisionPayload now carries a json.RawMessage (UnattendConfig),
	// so it is no longer comparable with ==; compare structurally.
	if !reflect.DeepEqual(roundTripped, original) {
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

// ── ISO template-provision fakes ───────────────────────────────────────────
//
// These target the pure provisionTemplateFromISO function with in-package
// fakes for vCenter and the database — the same idiom as image_jobs_test.go.
// The real *vcenter.Client / *database.Queries satisfy the narrow interfaces
// (asserted at compile time in template_jobs.go), so exercising the core here
// covers the branching the source_type=iso path adds.

// fakeISOVC records every vCenter call the ISO path makes so a test can assert
// which dependency ran, in what order, and with what arguments.
type fakeISOVC struct {
	createErr error
	createRet string // moref returned by CreateBlankVM
	waitErr   error
	uploadErr error
	detachErr error
	powerErr  error

	uploadCalls  int
	uploadDS     string
	uploadRemote string

	createCalls  int
	createParams vcenter.BlankVMParams

	powerOnCalls int
	powerOnMoref string

	waitCalls   int
	waitMoref   string
	waitTimeout time.Duration

	detachCalls int
	detachMoref string
}

func (f *fakeISOVC) UploadToDatastore(_ context.Context, datastore, remotePath string, r io.Reader, _ int64, _ func(sent int64)) error {
	f.uploadCalls++
	f.uploadDS = datastore
	f.uploadRemote = remotePath
	if f.uploadErr != nil {
		return f.uploadErr
	}
	// Drain the seed bytes with a bounded buffer, mirroring the real upload.
	_, _ = io.CopyBuffer(io.Discard, r, make([]byte, 32*1024))
	return nil
}

func (f *fakeISOVC) CreateBlankVM(_ context.Context, p vcenter.BlankVMParams) (string, error) {
	f.createCalls++
	f.createParams = p
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.createRet == "" {
		return "vm-iso-0001", nil
	}
	return f.createRet, nil
}

func (f *fakeISOVC) PowerOnVM(_ context.Context, moref string) error {
	f.powerOnCalls++
	f.powerOnMoref = moref
	return f.powerErr
}

func (f *fakeISOVC) WaitForTools(_ context.Context, moref string, timeout time.Duration) error {
	f.waitCalls++
	f.waitMoref = moref
	f.waitTimeout = timeout
	return f.waitErr
}

func (f *fakeISOVC) DetachCDROMs(_ context.Context, moref string) error {
	f.detachCalls++
	f.detachMoref = moref
	return f.detachErr
}

// fakeISODB records lifecycle transitions and keeps the template's current
// state in sync so markTemplateErrorViaDB (which re-reads the row) observes the
// same state the code just moved it through.
type fakeISODB struct {
	tmpl   *models.Template
	getErr error

	setVMCalls  int
	setVMMoref  string
	transitions []string // "from->to"
}

func (f *fakeISODB) GetTemplateByID(_ context.Context, _ uuid.UUID) (*models.Template, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.tmpl, nil
}

func (f *fakeISODB) SetTemplateVCenterVM(_ context.Context, _ uuid.UUID, vcenterVMID string) error {
	f.setVMCalls++
	f.setVMMoref = vcenterVMID
	return nil
}

func (f *fakeISODB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, from, to string) error {
	if f.tmpl != nil && f.tmpl.TemplateState != from {
		// Guard against a stale/incorrect transition source, exactly as the
		// real optimistic-locking UPDATE would.
		return errors.New("stale transition: have " + f.tmpl.TemplateState + " want " + from)
	}
	f.transitions = append(f.transitions, from+"->"+to)
	if f.tmpl != nil {
		f.tmpl.TemplateState = to
	}
	return nil
}

func (f *fakeISODB) finalState() string {
	if f.tmpl == nil {
		return ""
	}
	return f.tmpl.TemplateState
}

// baseISOPayload returns a valid source_type=iso payload; individual tests
// tweak UnattendMode / SourceRef to exercise a branch.
func baseISOPayload() TemplateProvisionPayload {
	return TemplateProvisionPayload{
		TemplateID:     uuid.New(),
		SourceType:     models.TemplateSourceISO,
		SourceRef:      "[NAS-BackupsAndISOS] ISOs/ubuntu-24.04-live-server.iso",
		VMName:         "tpl-ubuntu-abc123",
		FolderPath:     "JMAL-Datacenter/vm/Templates",
		StagingNetwork: "LabVMs-VLAN30",
		VCPUs:          2,
		RAMmb:          4096,
		DiskGB:         40,
		GuestID:        "ubuntu64Guest",
	}
}

func newISOFakes(payload TemplateProvisionPayload) (*fakeISOVC, *fakeISODB) {
	return &fakeISOVC{}, &fakeISODB{
		tmpl: &models.Template{
			ID:            payload.TemplateID,
			TemplateState: models.TemplateStateProvisioning,
			SourceType:    models.TemplateSourceISO,
		},
	}
}

// TestProvisionTemplate_ISO_Manual proves the manual install path NEVER waits
// for VMware Tools (the OS is not installed yet, so Tools can never appear) and
// parks the template in 'configuring' for a hands-on console install. A
// regression that re-enabled WaitForTools here would make every manual build
// hang for the full timeout and then falsely error.
func TestProvisionTemplate_ISO_Manual(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeManual
	vc, db := newISOFakes(payload)

	err := provisionTemplateFromISO(context.Background(), vc, db, discardLogger(), nil, payload)
	if err != nil {
		t.Fatalf("manual ISO provision returned error: %v", err)
	}

	if vc.waitCalls != 0 {
		t.Errorf("manual install must NOT call WaitForTools; got %d calls", vc.waitCalls)
	}
	if vc.detachCalls != 0 {
		t.Errorf("manual install must NOT detach CD-ROMs (installer is still needed); got %d calls", vc.detachCalls)
	}
	if vc.uploadCalls != 0 {
		t.Errorf("manual install must NOT build/upload a seed ISO; got %d uploads", vc.uploadCalls)
	}
	if vc.createCalls != 1 {
		t.Errorf("expected exactly one CreateBlankVM call, got %d", vc.createCalls)
	}
	if vc.powerOnCalls != 1 {
		t.Errorf("expected the VM to be powered on once, got %d", vc.powerOnCalls)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
	// The installer must be mounted with no seed CD for a manual install.
	if vc.createParams.ISOPath != payload.SourceRef {
		t.Errorf("CreateBlankVM ISOPath = %q, want %q", vc.createParams.ISOPath, payload.SourceRef)
	}
	if vc.createParams.SeedISOPath != "" {
		t.Errorf("manual install must not attach a seed ISO, got SeedISOPath=%q", vc.createParams.SeedISOPath)
	}
}

// TestProvisionTemplate_ISO_Unattended proves the unattended path builds+uploads
// a seed ISO, waits for Tools on the LONG install-length deadline (not the
// clone-length 5m), and then detaches the CD-ROMs so the finished template
// holds no ISO lock on the datastore. Each of those is a distinct regression
// guard: a short timeout would fail every real install; a skipped DetachCDROMs
// would leave a datastore lock that blocks replacing the ISO later.
func TestProvisionTemplate_ISO_Unattended(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeCloudInitCIData
	// unattend.BuildSeedISO(cloudinit_cidata) needs a password to hash into the
	// autoinstall; supply it (and a hostname) via UnattendConfig.
	payload.UnattendConfig = json.RawMessage(`{"Hostname":"ubuntu-lab","Password":"S3edP@ss-not-a-real-secret"}`) // pragma: allowlist-secret
	vc, db := newISOFakes(payload)
	vc.createRet = "vm-iso-7788"

	err := provisionTemplateFromISO(context.Background(), vc, db, discardLogger(), nil, payload)
	if err != nil {
		t.Fatalf("unattended ISO provision returned error: %v", err)
	}

	if vc.uploadCalls != 1 {
		t.Fatalf("expected the seed ISO to be uploaded exactly once, got %d", vc.uploadCalls)
	}
	if vc.createCalls != 1 {
		t.Fatalf("expected exactly one CreateBlankVM call, got %d", vc.createCalls)
	}
	// The uploaded seed must be attached as CD-ROM 1 on the created VM, on the
	// same datastore as the installer.
	wantSeedPath := vcenter.DatastorePath(vc.uploadDS, vc.uploadRemote)
	if vc.createParams.SeedISOPath != wantSeedPath {
		t.Errorf("CreateBlankVM SeedISOPath = %q, want the uploaded seed %q", vc.createParams.SeedISOPath, wantSeedPath)
	}
	if vc.uploadDS != "NAS-BackupsAndISOS" {
		t.Errorf("seed uploaded to datastore %q, want it beside the installer on %q", vc.uploadDS, "NAS-BackupsAndISOS")
	}
	if vc.waitCalls != 1 {
		t.Fatalf("unattended install must wait for VMware Tools once, got %d calls", vc.waitCalls)
	}
	if vc.waitTimeout != isoInstallToolsTimeout {
		t.Errorf("WaitForTools timeout = %s, want the long install deadline %s (a clone-length wait would fail every real install)", vc.waitTimeout, isoInstallToolsTimeout)
	}
	if vc.waitTimeout <= 5*time.Minute {
		t.Errorf("install-length WaitForTools timeout (%s) must be much longer than the clone path's 5m", vc.waitTimeout)
	}
	if vc.detachCalls != 1 {
		t.Fatalf("unattended install must detach CD-ROMs after Tools appear, got %d calls", vc.detachCalls)
	}
	if vc.detachMoref != "vm-iso-7788" {
		t.Errorf("DetachCDROMs called on %q, want the created VM %q", vc.detachMoref, "vm-iso-7788")
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionTemplate_ISO_BadSourceRef proves a malformed installer path is
// rejected BEFORE any VM is created — the real assertion is that CreateBlankVM
// recorded zero calls, so a typo can never orphan a half-built shell in vCenter.
// The template must also land in 'error' with an actionable message.
func TestProvisionTemplate_ISO_BadSourceRef(t *testing.T) {
	payload := baseISOPayload()
	payload.SourceRef = "NAS-BackupsAndISOS/ISOs/ubuntu.iso" // missing the [datastore] brackets
	vc, db := newISOFakes(payload)

	err := provisionTemplateFromISO(context.Background(), vc, db, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("expected an error for a malformed source_ref, got nil")
	}
	if vc.createCalls != 0 {
		t.Fatalf("malformed source_ref must NOT create a VM; CreateBlankVM was called %d time(s)", vc.createCalls)
	}
	if vc.uploadCalls != 0 || vc.powerOnCalls != 0 || vc.waitCalls != 0 {
		t.Errorf("no vCenter side effects expected on a bad ref (uploads=%d powerOn=%d waits=%d)", vc.uploadCalls, vc.powerOnCalls, vc.waitCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
	// Actionable message: it should name the offending ref and the expected form.
	if !strings.Contains(err.Error(), payload.SourceRef) || !strings.Contains(err.Error(), "[datastore]") {
		t.Errorf("error should quote the bad ref and the expected \"[datastore] path\" form, got: %v", err)
	}
}

// TestProvisionTemplate_ISO_RemasterUnsupportedFailsLoudly proves that when the
// seed builder reports ErrPreseedRequiresRemaster (debian_preseed, which cannot
// be seeded from a second CD in pure Go), the job FAILS instead of silently
// downgrading to a manual install. A silent downgrade would make the operator
// wait the full 60-minute Tools deadline for automation that was never going to
// run. The regression guards are: (1) the returned error wraps the real
// sentinel, (2) the message tells the operator to set unattend_mode=manual, and
// (3) the code did NOT proceed to create the VM or wait for Tools.
func TestProvisionTemplate_ISO_RemasterUnsupportedFailsLoudly(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeDebianPreseed
	vc, db := newISOFakes(payload)

	err := provisionTemplateFromISO(context.Background(), vc, db, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("debian_preseed must fail loudly, got nil (silent downgrade to manual)")
	}
	if !errors.Is(err, unattend.ErrPreseedRequiresRemaster) {
		t.Errorf("error must wrap the real unattend.ErrPreseedRequiresRemaster sentinel, got: %v", err)
	}
	if !strings.Contains(err.Error(), "manual") {
		t.Errorf("error must tell the operator to set unattend_mode=manual, got: %v", err)
	}
	if vc.createCalls != 0 {
		t.Errorf("must NOT create a VM when the seed cannot be built; CreateBlankVM called %d time(s)", vc.createCalls)
	}
	if vc.waitCalls != 0 {
		t.Errorf("must NOT proceed to wait for Tools (the silent-downgrade bug); WaitForTools called %d time(s)", vc.waitCalls)
	}
	if got := db.finalState(); got != models.TemplateStateError {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateError)
	}
}
