package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/unattend"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// TestGeneralizeScript_LinuxContainsCriticalSteps locks in the contract
// that the Linux generalize script wipes machine-id, SSH host keys, and
// cloud-init state. Any one of these missing means a student VM cloned
// from this template would either fail to boot (missing entropy) or,
// worse, share a machine-id with another student.
//
// Note it deliberately does NOT require a shutdown line - see
// TestGeneralizeScript_LinuxMustNotPowerItselfOff.
func TestGeneralizeScript_LinuxContainsCriticalSteps(t *testing.T) {
	got := generalizeScript("linux", "job-1")
	required := []string{
		"cloud-init clean",
		"truncate -s 0 /etc/machine-id",
		"/var/lib/dbus/machine-id",
		"/etc/ssh/ssh_host_",
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
	got := generalizeScript("windows", "job-1")
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
	got := generalizeScript("plan9", "job-1")
	if !strings.Contains(got, "truncate -s 0 /etc/machine-id") {
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

// TestTemplateProvisionedBeyond guards the idempotency gate that keeps a
// duplicate/late provision job from (a) failing loudly when the template has
// already been provisioned, or (b) flipping a healthy configuring template to
// error when it loses the provisioning→configuring race. Only the pre-clone
// states and 'error' must be treated as "not yet provisioned".
func TestTemplateProvisionedBeyond(t *testing.T) {
	beyond := []string{
		models.TemplateStateConfiguring,
		models.TemplateStateGeneralizing,
		models.TemplateStateReady,
		models.TemplateStateVerifying,
		models.TemplateStateActive,
	}
	for _, s := range beyond {
		if !templateProvisionedBeyond(s) {
			t.Errorf("state %q should count as provisioned-beyond", s)
		}
	}
	notBeyond := []string{
		models.TemplateStateDraft,
		models.TemplateStateProvisioning,
		models.TemplateStateError,
		"",
		"bogus",
	}
	for _, s := range notBeyond {
		if templateProvisionedBeyond(s) {
			t.Errorf("state %q must NOT count as provisioned-beyond", s)
		}
	}
}

func newTemplateProvisionRetryDB(t *testing.T) (*database.Queries, *pgxpool.Pool, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run template provision retry tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	queries := database.NewQueries(pool)
	id := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (
			id, name, vcenter_template, os_type, template_state,
			source_type, source_ref, staging_network
		)
		VALUES ($1, $2, $3, 'linux', 'provisioning', 'clone_vcenter', 'vm-source', 'staging')
	`, id, "retry-"+id.String(), "source-"+id.String()); err != nil {
		t.Fatal(err)
	}
	return queries, pool, id
}

func TestProvisionTemplateFailTemplateStep_RespectsRetryBudget(t *testing.T) {
	transientErr := errors.New("The virtual disk is either corrupted or not a supported format.")
	queries, _, id := newTemplateProvisionRetryDB(t)
	ctx := context.Background()

	t.Run("retryable-error-with-retries-remaining-keeps-template-in-provisioning", func(t *testing.T) {
		p := &Provisioner{db: queries, logger: discardLogger()}
		job := &models.Job{Type: models.JobTypeTemplateProvision, RetryCount: 0, MaxRetries: 3}

		gotErr := p.failTemplateStep(ctx, job, id, transientErr)
		if gotErr == nil || gotErr.Error() != transientErr.Error() {
			t.Fatalf("failTemplateStep returned %v, want %v", gotErr, transientErr)
		}
		fresh, err := queries.GetTemplateByID(ctx, id)
		if err != nil {
			t.Fatalf("GetTemplateByID: %v", err)
		}
		if fresh == nil || fresh.TemplateState != models.TemplateStateProvisioning {
			t.Fatalf("template state = %#v, want %q after retryable failure with retries remaining", fresh, models.TemplateStateProvisioning)
		}
	})

	t.Run("retryable-error-at-max-retries-transitions-to-error", func(t *testing.T) {
		if err := queries.UpdateTemplateLifecycleState(ctx, id, models.TemplateStateProvisioning, models.TemplateStateProvisioning); err != nil {
			t.Fatalf("reset template state: %v", err)
		}
		p := &Provisioner{db: queries, logger: discardLogger()}
		job := &models.Job{Type: models.JobTypeTemplateProvision, RetryCount: 2, MaxRetries: 2}

		gotErr := p.failTemplateStep(ctx, job, id, transientErr)
		if gotErr == nil || gotErr.Error() != transientErr.Error() {
			t.Fatalf("failTemplateStep returned %v, want %v", gotErr, transientErr)
		}
		fresh, err := queries.GetTemplateByID(ctx, id)
		if err != nil {
			t.Fatalf("GetTemplateByID: %v", err)
		}
		if fresh == nil || fresh.TemplateState != models.TemplateStateError {
			t.Fatalf("template state = %#v, want %q after retryable failure exhausted retries", fresh, models.TemplateStateError)
		}
	})

	t.Run("nonretryable-error-transitions-immediately", func(t *testing.T) {
		if err := queries.UpdateTemplateLifecycleState(ctx, id, models.TemplateStateError, models.TemplateStateProvisioning); err != nil {
			t.Fatalf("reset template state: %v", err)
		}
		p := &Provisioner{db: queries, logger: discardLogger()}
		job := &models.Job{Type: models.JobTypeTemplateProvision, RetryCount: 0, MaxRetries: 3}
		nonRetryable := errors.New("template_id is required")

		gotErr := p.failTemplateStep(ctx, job, id, nonRetryable)
		if gotErr == nil || gotErr.Error() != nonRetryable.Error() {
			t.Fatalf("failTemplateStep returned %v, want %v", gotErr, nonRetryable)
		}
		fresh, err := queries.GetTemplateByID(ctx, id)
		if err != nil {
			t.Fatalf("GetTemplateByID: %v", err)
		}
		if fresh == nil || fresh.TemplateState != models.TemplateStateError {
			t.Fatalf("template state = %#v, want %q after nonretryable failure", fresh, models.TemplateStateError)
		}
	})
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

// --------------------------------------------------------------------------
// RevalidateL1Template: late moref resolution
// --------------------------------------------------------------------------

type fakeResolveVC struct {
	db              *fakeRevalidateDB
	resolveCalls    int
	resolveName     string
	resolveRet      string
	resolveErr      error
	sawTemplateLoad bool
}

var _ revalidateL1TemplateVCenter = (*fakeResolveVC)(nil)

func (f *fakeResolveVC) ResolveVMByName(_ context.Context, name string) (string, error) {
	f.resolveCalls++
	f.resolveName = name
	if f.db != nil {
		f.sawTemplateLoad = f.db.getCalls > 0
	}
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return f.resolveRet, nil
}

func TestRevalidateL1Template_LateResolutionAndFastPath(t *testing.T) {
	tests := []struct {
		name             string
		templateName     string
		sourceName       string
		payloadMoref     string
		resolveRet       string
		resolveErr       error
		wantResolveCalls int
		wantResolveName  string
		wantSmokeCalls   int
		wantSmokeMoref   string
		wantErrContains  []string
	}{
		{
			name:             "resolve by source VM name when payload omits moref",
			templateName:     "Ubuntu 24.04 Server",
			sourceName:       "student-ubuntu-2404",
			resolveRet:       "vm-1111",
			wantResolveCalls: 1,
			wantResolveName:  "student-ubuntu-2404",
			wantSmokeCalls:   1,
			wantSmokeMoref:   "vm-1111",
		},
		{
			name:             "resolve failure mentions template and source VM",
			templateName:     "Windows 11",
			sourceName:       "student-windows-11",
			resolveErr:       errors.New("not found"),
			wantResolveCalls: 1,
			wantResolveName:  "student-windows-11",
			wantSmokeCalls:   0,
			wantErrContains:  []string{"Windows 11", "student-windows-11", "renamed or deleted"},
		},
		{
			name:             "empty resolved moref is rejected",
			templateName:     "Windows Server 2022",
			sourceName:       "student-windows-server-2022",
			resolveRet:       "",
			wantResolveCalls: 1,
			wantResolveName:  "student-windows-server-2022",
			wantSmokeCalls:   0,
			wantErrContains:  []string{"Windows Server 2022", "student-windows-server-2022", "empty moref"},
		},
		{
			name:             "fast path preserves payload moref",
			templateName:     "Windows Server 2025",
			sourceName:       "student-windows-server-2025",
			payloadMoref:     "vm-4242",
			wantResolveCalls: 0,
			wantSmokeCalls:   1,
			wantSmokeMoref:   "vm-4242",
		},
		{
			name:             "missing payload moref and source name errors",
			templateName:     "synthetic-noop",
			wantResolveCalls: 0,
			wantSmokeCalls:   0,
			wantErrContains:  []string{"synthetic-noop", "no vm_moref", "no vcenter_template"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			tmpl := makeL1Template(id, nil)
			tmpl.Name = tc.templateName
			tmpl.VCenterVMID = ""
			tmpl.VCenterTemplate = tc.sourceName

			db := &fakeRevalidateDB{tmpl: &tmpl, isActive: true}
			vc := &fakeResolveVC{db: db, resolveRet: tc.resolveRet, resolveErr: tc.resolveErr}
			payload, err := json.Marshal(TemplateRevalidatePayload{TemplateID: id, VMMoref: tc.payloadMoref})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			job := &models.Job{ID: uuid.New(), Type: models.JobTypeTemplateRevalidate, Payload: payload}

			var smokeCalls int
			var smokeMoref string
			err = revalidateL1TemplateJob(context.Background(), db, vc, nil, discardLogger(), job,
				func(string, string) {},
				func(_ context.Context, tmpl *models.Template, vmMoref string, _ func(string, string)) error {
					smokeCalls++
					if tmpl.ID != id {
						t.Fatalf("smoke check template ID = %v, want %v", tmpl.ID, id)
					}
					smokeMoref = vmMoref
					return nil
				},
			)

			if db.getCalls != 1 {
				t.Fatalf("GetTemplateByID called %d time(s), want 1", db.getCalls)
			}
			if vc.resolveCalls != tc.wantResolveCalls {
				t.Fatalf("ResolveVMByName called %d time(s), want %d", vc.resolveCalls, tc.wantResolveCalls)
			}
			if tc.wantResolveCalls > 0 && !vc.sawTemplateLoad {
				t.Fatal("ResolveVMByName ran before the template was loaded")
			}
			if tc.wantResolveName != "" && vc.resolveName != tc.wantResolveName {
				t.Fatalf("ResolveVMByName name = %q, want %q", vc.resolveName, tc.wantResolveName)
			}
			if smokeCalls != tc.wantSmokeCalls {
				t.Fatalf("smoke check called %d time(s), want %d", smokeCalls, tc.wantSmokeCalls)
			}
			if smokeCalls > 0 && smokeMoref != tc.wantSmokeMoref {
				t.Fatalf("smoke check moref = %q, want %q", smokeMoref, tc.wantSmokeMoref)
			}
			if len(tc.wantErrContains) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			for _, want := range tc.wantErrContains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRevalidateL1Template_ForwardCloneRetryDoesNotRecordValidationOutcome(t *testing.T) {
	id := uuid.New()
	tmpl := makeL1Template(id, nil)
	db := &fakeRevalidateDB{tmpl: &tmpl, isActive: true}
	vc := &fakeResolveVC{resolveErr: errors.New("renamed source no longer resolves")}
	pipeline := NewPipelineMetrics("", "", nil)
	payload, err := json.Marshal(TemplateRevalidatePayload{
		TemplateID: id,
		VMMoref:    "vm-renamed-source",
		CloneOperation: &models.VMCloneOperation{
			OperationID:       uuid.NewString(),
			LogicalTemplateID: id.String(),
			SourceRef:         "vm-exact-persisted-source",
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeTemplateRevalidate,
		Payload:    payload,
		RetryCount: 0,
		MaxRetries: 2,
	}
	forwardErr := newCloneForwardRetryError(context.DeadlineExceeded, nil)
	var smokeMoref string

	err = revalidateL1TemplateJob(
		context.Background(),
		db,
		vc,
		pipeline,
		discardLogger(),
		job,
		func(string, string) {},
		func(_ context.Context, _ *models.Template, vmMoref string, _ func(string, string)) error {
			smokeMoref = vmMoref
			return forwardErr
		},
	)
	if !isCloneForwardRetry(err) {
		t.Fatalf("error = %v, want clone forward retry", err)
	}
	if db.validationResult != "" || !db.validationAt.IsZero() {
		t.Fatalf(
			"forward retry persisted validation result=%q at=%v",
			db.validationResult,
			db.validationAt,
		)
	}
	if vc.resolveCalls != 0 || smokeMoref != "vm-exact-persisted-source" {
		t.Fatalf(
			"resume resolved mutable source: resolve calls=%d smoke source=%q",
			vc.resolveCalls,
			smokeMoref,
		)
	}
	metrics := string(pipeline.serialize())
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "crucible_template_validation_total{") ||
			strings.HasPrefix(line, "crucible_template_last_validated_timestamp_seconds{") {
			t.Fatalf("forward retry emitted validation outcome metric %q", line)
		}
	}
}

func TestRevalidateL1Template_MissingTemplateCompensatesPersistedClone(t *testing.T) {
	id := uuid.New()
	db := &fakeRevalidateDB{}
	payload, err := json.Marshal(TemplateRevalidatePayload{
		TemplateID: id,
		CloneOperation: &models.VMCloneOperation{
			OperationID:       uuid.NewString(),
			LogicalTemplateID: id.String(),
			SourceRef:         "vm-exact-persisted-source",
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeTemplateRevalidate,
		Payload:    payload,
		MaxRetries: 2,
	}

	err = revalidateL1TemplateJob(
		context.Background(),
		db,
		&fakeResolveVC{},
		NewPipelineMetrics("", "", nil),
		discardLogger(),
		job,
		func(string, string) {},
		func(context.Context, *models.Template, string, func(string, string)) error {
			t.Fatal("smoke check ran after template preamble failed")
			return nil
		},
	)
	if !isCompensationRetry(err) {
		t.Fatalf("missing template abandoned persisted clone: %T %v", err, err)
	}
	if isCompensatedJobError(err) {
		t.Fatal("missing template was incorrectly finalized without exact clone cleanup")
	}
	if db.validationResult != "" || !db.validationAt.IsZero() {
		t.Fatalf(
			"preamble compensation persisted validation result=%q at=%v",
			db.validationResult,
			db.validationAt,
		)
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
	createErr   error
	createRet   string // moref returned by CreateBlankVM
	waitErr     error
	powerOffErr error
	uploadErr   error
	detachErr   error
	powerErr    error

	// powerErrs is consumed one-per-call by PowerOnVM so a test can model
	// "power-on faults disk-not-ready on the first try, succeeds after the disk
	// is recreated". A call past the end of the slice falls back to powerErr.
	powerErrs []error

	// probeErrs is consumed one-per-call by ProbeSystemDiskReadable so a test
	// can model "broken on the first probe, readable after the recreate". A
	// call past the end of the slice returns nil (readable).
	probeErrs   []error
	recreateErr error

	// seq records the order of vCenter calls so a test can assert the ISO
	// install sequence, not just that each call happened. Ordering is the
	// whole contract here: detaching the installer media before the install
	// finishes silently produces an empty-disk template.
	seq []string

	uploadCalls  int
	uploadDS     string
	uploadRemote string

	createCalls  int
	createParams vcenter.BlankVMParams

	powerOnCalls int
	powerOnMoref string

	probeCalls    int
	probeMoref    string
	recreateCalls int
	recreateMoref string
	recreateGB    int

	waitCalls   int
	waitMoref   string
	waitTimeout time.Duration

	powerOffCalls   int
	powerOffMoref   string
	powerOffTimeout time.Duration

	detachCalls int
	detachMoref string
}

func (f *fakeISOVC) UploadToDatastore(_ context.Context, datastore, remotePath string, r io.Reader, _ int64, _ func(sent int64)) error {
	f.uploadCalls++
	f.seq = append(f.seq, "upload")
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
	f.seq = append(f.seq, "create")
	f.createParams = p
	if f.createErr != nil {
		return "", f.createErr
	}
	if f.createRet == "" {
		return "vm-iso-0001", nil
	}
	return f.createRet, nil
}

// ProbeSystemDiskReadable models the pre-power-on disk GET. It records the call
// and returns the next queued probe error (nil once the slice is exhausted), so
// a test can drive "broken on first probe, readable after recreate".
func (f *fakeISOVC) ProbeSystemDiskReadable(_ context.Context, moref string) error {
	f.probeCalls++
	f.seq = append(f.seq, "probe")
	f.probeMoref = moref
	if len(f.probeErrs) > 0 {
		err := f.probeErrs[0]
		f.probeErrs = f.probeErrs[1:]
		return err
	}
	return nil
}

func (f *fakeISOVC) RecreateSystemDisk(_ context.Context, moref string, diskGB int) error {
	f.recreateCalls++
	f.seq = append(f.seq, "recreate")
	f.recreateMoref = moref
	f.recreateGB = diskGB
	return f.recreateErr
}

func (f *fakeISOVC) PowerOnVM(_ context.Context, moref string) error {
	f.powerOnCalls++
	f.seq = append(f.seq, "power_on")
	f.powerOnMoref = moref
	if len(f.powerErrs) > 0 {
		err := f.powerErrs[0]
		f.powerErrs = f.powerErrs[1:]
		return err
	}
	return f.powerErr
}

// WaitForPowerOff is the unattended install's completion signal: the generated
// autoinstall sets "shutdown: poweroff", so the VM powering itself off is the
// first moment the target disk is known to be written.
func (f *fakeISOVC) WaitForPowerOff(_ context.Context, moref string, timeout time.Duration) error {
	f.powerOffCalls++
	f.seq = append(f.seq, "wait_power_off")
	f.powerOffMoref = moref
	f.powerOffTimeout = timeout
	return f.powerOffErr
}

func (f *fakeISOVC) WaitForTools(_ context.Context, moref string, timeout time.Duration) error {
	f.waitCalls++
	f.seq = append(f.seq, "wait_tools")
	f.waitMoref = moref
	f.waitTimeout = timeout
	return f.waitErr
}

func (f *fakeISOVC) DetachCDROMs(_ context.Context, moref string) error {
	f.detachCalls++
	f.seq = append(f.seq, "detach")
	f.detachMoref = moref
	return f.detachErr
}

// fakeISODB records lifecycle transitions and keeps the template's current
// state in sync with successful provision transitions.
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

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err != nil {
		t.Fatalf("manual ISO provision returned error: %v", err)
	}

	if vc.waitCalls != 0 {
		t.Errorf("manual install must NOT call WaitForTools; got %d calls", vc.waitCalls)
	}
	if vc.powerOffCalls != 0 {
		t.Errorf("manual install must NOT wait for power-off (nothing will power the VM off); got %d calls", vc.powerOffCalls)
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

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
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
	if vc.powerOffCalls != 1 {
		t.Fatalf("unattended install must wait for the VM to power itself off exactly once, got %d calls", vc.powerOffCalls)
	}
	if vc.powerOffTimeout != isoInstallToolsTimeout {
		t.Errorf("WaitForPowerOff timeout = %s, want the long install deadline %s (a clone-length wait would fail every real install)", vc.powerOffTimeout, isoInstallToolsTimeout)
	}
	if vc.powerOffTimeout <= 5*time.Minute {
		t.Errorf("install-length WaitForPowerOff timeout (%s) must be much longer than the clone path's 5m", vc.powerOffTimeout)
	}
	if vc.detachCalls != 1 {
		t.Fatalf("unattended install must detach CD-ROMs after the install finishes, got %d calls", vc.detachCalls)
	}
	if vc.detachMoref != "vm-iso-7788" {
		t.Errorf("DetachCDROMs called on %q, want the created VM %q", vc.detachMoref, "vm-iso-7788")
	}
	// Tools are still waited for, but only AFTER the installed system is booted
	// off its own disk — see TestProvisionTemplate_ISO_Unattended_WaitsForPowerOffNotTools.
	if vc.waitCalls != 1 {
		t.Fatalf("expected exactly one WaitForTools call (on the installed system), got %d", vc.waitCalls)
	}
	if vc.waitTimeout != isoInstalledBootTimeout {
		t.Errorf("post-install WaitForTools timeout = %s, want the ordinary first-boot deadline %s", vc.waitTimeout, isoInstalledBootTimeout)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

// TestProvisionTemplate_ISO_Unattended_WaitsForPowerOffNotTools is the
// regression guard for a silent, template-destroying bug.
//
// The Ubuntu live-server installer ISO runs open-vm-tools in the *ephemeral
// installer* environment: on a real build, Tools reported RUNNING 39 seconds
// after power-on, with nothing yet written to the disk. The original code used
// WaitForTools as the "install finished" signal, so it would have detached the
// installer media out from under the running installer roughly a minute in and
// advanced the template to 'configuring' with a completely empty disk — a
// template that looks perfectly provisioned and has no OS.
//
// This asserts the ORDER, not just the calls: the install must complete
// (power-off) before the media is detached, and Tools must only be consulted
// after the installed system has been booted off its own disk.
func TestProvisionTemplate_ISO_Unattended_WaitsForPowerOffNotTools(t *testing.T) {
	payload := baseISOPayload()
	payload.UnattendMode = models.UnattendModeCloudInitCIData
	payload.UnattendConfig = json.RawMessage(`{"Hostname":"ubuntu-lab","Password":"S3edP@ss-not-a-real-secret"}`) // pragma: allowlist-secret
	vc, db := newISOFakes(payload)

	if err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload); err != nil {
		t.Fatalf("unattended ISO provision returned error: %v", err)
	}

	want := []string{"upload", "create", "probe", "power_on", "wait_power_off", "detach", "power_on", "wait_tools"}
	if len(vc.seq) != len(want) {
		t.Fatalf("vCenter call sequence = %v, want %v", vc.seq, want)
	}
	for i := range want {
		if vc.seq[i] != want[i] {
			t.Fatalf("vCenter call sequence = %v, want %v (first difference at index %d)", vc.seq, want, i)
		}
	}

	// Spell out the two orderings that carry the whole contract, so a failure
	// names the production consequence rather than just an index.
	offAt, detachAt, toolsAt := indexOf(vc.seq, "wait_power_off"), indexOf(vc.seq, "detach"), indexOf(vc.seq, "wait_tools")
	if !(offAt < detachAt) {
		t.Errorf("CD-ROMs detached at step %d but the install was only confirmed finished at step %d: "+
			"detaching the installer media mid-install produces a template with an empty disk", detachAt, offAt)
	}
	if !(offAt < toolsAt) {
		t.Errorf("WaitForTools ran at step %d, before the install completed at step %d: "+
			"the Ubuntu live installer runs open-vm-tools ~40s after power-on, so Tools appearing "+
			"does NOT mean the OS is installed", toolsAt, offAt)
	}
	if got := db.finalState(); got != models.TemplateStateConfiguring {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateConfiguring)
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

// TestProvisionTemplate_ISO_BadSourceRef proves a malformed installer path is
// rejected BEFORE any VM is created — the real assertion is that CreateBlankVM
// recorded zero calls, so a typo can never orphan a half-built shell in vCenter.
// The template must remain provisioning so the job lifecycle can own retry or
// the atomic terminal transition.
func TestProvisionTemplate_ISO_BadSourceRef(t *testing.T) {
	payload := baseISOPayload()
	payload.SourceRef = "NAS-BackupsAndISOS/ISOs/ubuntu.iso" // missing the [datastore] brackets
	vc, db := newISOFakes(payload)

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
	if err == nil {
		t.Fatal("expected an error for a malformed source_ref, got nil")
	}
	if vc.createCalls != 0 {
		t.Fatalf("malformed source_ref must NOT create a VM; CreateBlankVM was called %d time(s)", vc.createCalls)
	}
	if vc.uploadCalls != 0 || vc.powerOnCalls != 0 || vc.waitCalls != 0 {
		t.Errorf("no vCenter side effects expected on a bad ref (uploads=%d powerOn=%d waits=%d)", vc.uploadCalls, vc.powerOnCalls, vc.waitCalls)
	}
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
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

	err := provisionTemplateFromISO(context.Background(), vc, db, nil, discardLogger(), nil, payload)
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
	if got := db.finalState(); got != models.TemplateStateProvisioning {
		t.Errorf("final template state = %q, want %q", got, models.TemplateStateProvisioning)
	}
}

func TestProvisionTemplateCloneAttemptDoesNotOwnErrorTransition(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "template_jobs.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var provision *ast.FuncDecl
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == "ProvisionTemplate" {
			provision = fn
			break
		}
	}
	if provision == nil {
		t.Fatal("ProvisionTemplate declaration not found")
	}
	ast.Inspect(provision.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "markTemplateError" {
			t.Errorf("ProvisionTemplate directly calls markTemplateError; terminal state must be owned by processJobLifecycle")
		}
		return true
	})
}

// --- generalize completion sentinel -----------------------------------------
//
// Background: generalize's last act is to power the guest off, which kills the
// guest agent mid-call, so RunScriptInGuest returns an error even on total
// success. The original code decided success by pattern-matching that error
// string. That is unsound -- vCenter emits "the guest operations agent could
// not be contacted" BOTH for a guest that shut itself down on purpose and for
// a guest whose VMware Tools never started -- and it cost a full rebuild cycle
// when a correct generalize run was marked 'error'. These tests lock in the
// replacement: a run-scoped guestinfo sentinel, read while the guest is still
// powered on (vCenter clears guest-written guestinfo on power-off).

type fakeSentinelVC struct {
	vals  []string
	errs  []error
	calls int
	key   string
}

func (f *fakeSentinelVC) GetGuestInfoVar(_ context.Context, _, key string) (string, error) {
	f.key = key
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return "", f.errs[i]
	}
	if i < len(f.vals) {
		return f.vals[i], nil
	}
	return "", nil
}

// TestGeneralizeScript_LinuxStampsSentinel is the guard for the whole
// mechanism: if the stamp is missing, or is swallowed by `|| true` (so a
// failed stamp still looks fine), the worker cannot prove the cleanup ran.
func TestGeneralizeScript_LinuxStampsSentinel(t *testing.T) {
	const runID = "9f1c2d3e-4a5b-6c7d-8e9f-0a1b2c3d4e5f"
	got := generalizeScript("linux", runID)

	stamp := strings.Index(got, "vmware-rpctool")
	if stamp < 0 {
		t.Fatalf("linux generalize script never stamps the completion sentinel.\n"+
			"Without it the worker cannot distinguish a guest that finished cleanup\n"+
			"from one that died halfway.\ngot:\n%s", got)
	}
	if !strings.Contains(got, generalizeSentinelKey) {
		t.Errorf("stamp does not reference %s, so the worker will read a key nobody writes", generalizeSentinelKey)
	}
	if !strings.Contains(got, runID) {
		t.Errorf("stamp does not carry the run ID, so a sentinel left by an EARLIER generalize attempt would be accepted as this run's proof")
	}

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "vmware-rpctool") && strings.Contains(line, "|| true") {
			t.Errorf("stamp is swallowed by `|| true`: %q\n"+
				"A stamp that cannot fail is not evidence -- if we cannot record completion we must not claim it.", line)
		}
	}
}

// TestGeneralizeScript_LinuxMustNotPowerItselfOff is the regression guard for
// a defect that made EVERY Linux ISO template generalize fail while the
// cleanup itself was working perfectly.
//
// Guest-written guestinfo lives only in the running VM's config.extraConfig,
// and vCenter CLEARS IT WHEN THE VM POWERS OFF. Verified on real hardware: a
// marker written with vmware-rpctool was present in `govc vm.info -e`
// immediately before a guest-initiated shutdown and absent immediately after,
// with nothing else changed.
//
// So a script that stamps the sentinel and then powers itself off destroys its
// own evidence: generalizeConfirmed can never return true, and the template is
// marked 'error' with "did not run to completion" even though it completed.
// The guest must stay up long enough for the worker to read the stamp; the
// worker then issues the shutdown (GeneralizeTemplate Step 2b).
//
// Windows is exempt because sysprep insists on powering the machine off
// itself, which is exactly why the Windows branch never stamps a sentinel and
// falls back to the salvage path instead.
func TestGeneralizeScript_LinuxMustNotPowerItselfOff(t *testing.T) {
	got := generalizeScript("linux", "job-1")

	// Assert the premise, so this test cannot pass vacuously if the script is
	// ever gutted: it must still be the script that stamps the sentinel.
	if !strings.Contains(got, "vmware-rpctool") {
		t.Fatalf("premise broken: the linux script no longer stamps a sentinel, "+
			"so this guard is testing nothing.\ngot:\n%s", got)
	}

	for _, banned := range []string{"shutdown", "poweroff", "halt", "systemctl poweroff"} {
		if strings.Contains(got, banned) {
			t.Errorf("linux generalize script contains %q.\n"+
				"Powering the guest off from inside the script ERASES the guestinfo sentinel "+
				"it just wrote (vCenter clears guest-written guestinfo on power-off), so "+
				"generalizeConfirmed can never confirm and every successful generalize is "+
				"reported as 'did not run to completion'. Let the worker power the guest off "+
				"after it has read the stamp.\nscript:\n%s", banned, got)
		}
	}
}

// TestGeneralizeScript_SentinelStampRunsAsRoot guards a foot-gun in the fix
// itself. Setting a guestinfo variable goes through the VMware backdoor and
// open-vm-tools restricts that to root; as the unprivileged build user the
// command fails with permission denied. Because the script runs under `set -e`
// and the stamp is deliberately NOT swallowed, an unprivileged stamp would
// abort the script BEFORE `shutdown` -- leaving the VM powered on and turning
// every Linux generalize into a hard failure. Every other privileged line in
// this script already uses sudo.
func TestGeneralizeScript_SentinelStampRunsAsRoot(t *testing.T) {
	got := generalizeScript("linux", "run-1")
	for _, line := range strings.Split(got, "\n") {
		if !strings.Contains(line, "vmware-rpctool") {
			continue
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "sudo ") {
			t.Fatalf("sentinel stamp does not run as root: %q\n"+
				"info-set is root-only, so this aborts under `set -e` before the guest ever shuts down.", line)
		}
		return
	}
	t.Fatal("no vmware-rpctool line found to check")
}

// TestGeneralizeScript_SentinelIsRunScoped guards the specific trap a constant
// sentinel would fall into: retrying generalize on a VM that already carries a
// sentinel from a previous attempt would confirm instantly, even if this run
// died on its first line.
func TestGeneralizeScript_SentinelIsRunScoped(t *testing.T) {
	a := generalizeScript("linux", "run-aaa")
	b := generalizeScript("linux", "run-bbb")
	if a == b {
		t.Fatal("generalize script is identical for two different runs; a stale sentinel from an earlier attempt would be mistaken for this run's completion")
	}
	if strings.Contains(b, "run-aaa") {
		t.Error("script leaks a foreign run ID")
	}
}

func TestGeneralizeConfirmed_MatchingSentinelConfirms(t *testing.T) {
	vc := &fakeSentinelVC{vals: []string{"run-1"}}
	ok, err := generalizeConfirmed(context.Background(), vc, "vm-1", "run-1", 3, time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("matching sentinel must confirm: ok=%v err=%v", ok, err)
	}
	if vc.key != generalizeSentinelKey {
		t.Errorf("read key %q, want %q", vc.key, generalizeSentinelKey)
	}
	if vc.calls != 1 {
		t.Errorf("confirmed on read %d, want to stop at the first match", vc.calls)
	}
}

// TestGeneralizeConfirmed_StaleSentinelIsNotConfirmation is the reason the
// sentinel carries a run ID at all.
func TestGeneralizeConfirmed_StaleSentinelIsNotConfirmation(t *testing.T) {
	vc := &fakeSentinelVC{vals: []string{"an-older-run", "an-older-run", "an-older-run"}}
	ok, err := generalizeConfirmed(context.Background(), vc, "vm-1", "this-run", 3, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("a sentinel from a PREVIOUS generalize attempt was accepted as this run's completion; " +
			"a retry would publish a template whose machine-id and SSH host keys were never cleaned")
	}
}

// TestGeneralizeConfirmed_PollsPastTheShutdownRace: the stamp is written
// immediately before power-off, so the first read can legitimately miss it.
func TestGeneralizeConfirmed_PollsPastTheShutdownRace(t *testing.T) {
	vc := &fakeSentinelVC{vals: []string{"", "", "run-1"}}
	ok, err := generalizeConfirmed(context.Background(), vc, "vm-1", "run-1", 5, time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("must keep polling past an empty first read: ok=%v err=%v calls=%d", ok, err, vc.calls)
	}
}

func TestGeneralizeConfirmed_AbsentSentinelIsNotConfirmation(t *testing.T) {
	vc := &fakeSentinelVC{}
	ok, err := generalizeConfirmed(context.Background(), vc, "vm-1", "run-1", 3, time.Millisecond)
	if err != nil {
		t.Fatalf("absence is a normal answer, not an error: %v", err)
	}
	if ok {
		t.Fatal("no sentinel must not confirm completion")
	}
}

// TestGeneralizeConfirmed_ReadFailureIsReportedNotSwallowed keeps "vCenter is
// unreachable" distinguishable from "the guest did not finish". The call site
// picks a different, deliberately weaker branch for the former.
func TestGeneralizeConfirmed_ReadFailureIsReportedNotSwallowed(t *testing.T) {
	boom := errors.New("vcenter unreachable")
	vc := &fakeSentinelVC{errs: []error{boom, boom}}
	ok, err := generalizeConfirmed(context.Background(), vc, "vm-1", "run-1", 2, time.Millisecond)
	if ok {
		t.Fatal("must not confirm when the sentinel could not be read")
	}
	if err == nil {
		t.Fatal("a read failure must be reported so the caller can tell 'unknown' from 'did not finish'")
	}
}

// TestIsExpectedShutdownErr_CoversTheGuestAgentMessage documents the message
// that started all this. It is in the hint list, but ONLY as a hint -- the
// sentinel is what actually decides, because this exact string is also what a
// guest whose Tools never started produces.
func TestIsExpectedShutdownErr_CoversTheGuestAgentMessage(t *testing.T) {
	err := errors.New("ServerFaultCode: The guest operations agent could not be contacted.")
	if !isExpectedShutdownErr(err) {
		t.Error("the real-world generalize shutdown message must be recognised as a shutdown hint")
	}
}
