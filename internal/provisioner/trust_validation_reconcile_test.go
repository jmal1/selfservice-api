// trust_validation_reconcile_test.go — unit tests for reconcileL1TrustValidation
// and RevalidateL1Template.
//
// Negative controls: two tests are verified against the production code in
// both broken and fixed states to confirm they catch real bugs.
package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

type fakeL1DB struct {
	allL1        []models.Template
	allL1Err     error
	staleL1      []models.Template
	staleL1Err   error
	createdJobs  []models.Job
	createJobErr error
	activeJobs   map[uuid.UUID]string
	deriveStale  bool
	// For round-trip store tests
	validationState map[uuid.UUID]struct {
		result string
		at     time.Time
	}
	setValidationErr error
}

func (f *fakeL1DB) ListAllActiveCredentialRevalidationTemplates(_ context.Context) ([]models.Template, error) {
	return f.allL1, f.allL1Err
}

func (f *fakeL1DB) ListStaleCredentialRevalidationTemplates(_ context.Context, olderThan time.Duration) ([]models.Template, error) {
	if f.deriveStale {
		var stale []models.Template
		for _, tmpl := range f.allL1 {
			if tmpl.LastValidatedAt == nil || time.Since(*tmpl.LastValidatedAt) >= olderThan {
				stale = append(stale, tmpl)
			}
		}
		return stale, f.staleL1Err
	}
	return f.staleL1, f.staleL1Err
}

func (f *fakeL1DB) CreateTemplateRevalidateJobIfAbsent(
	_ context.Context,
	templateID uuid.UUID,
	payload []byte,
) (*models.Job, bool, error) {
	if f.createJobErr != nil {
		return nil, false, f.createJobErr
	}
	if status := f.activeJobs[templateID]; status == models.JobStatusPending ||
		status == models.JobStatusClaimed ||
		status == models.JobStatusInProgress {
		return nil, false, nil
	}
	j := models.Job{ID: uuid.New(), Type: models.JobTypeTemplateRevalidate, Payload: payload}
	f.createdJobs = append(f.createdJobs, j)
	if f.activeJobs == nil {
		f.activeJobs = make(map[uuid.UUID]string)
	}
	f.activeJobs[templateID] = models.JobStatusPending
	return &j, true, nil
}

func (f *fakeL1DB) SetTemplateValidationState(_ context.Context, id uuid.UUID, result string, at time.Time) error {
	if f.setValidationErr != nil {
		return f.setValidationErr
	}
	if f.validationState == nil {
		f.validationState = make(map[uuid.UUID]struct {
			result string
			at     time.Time
		})
	}
	f.validationState[id] = struct {
		result string
		at     time.Time
	}{result, at}
	return nil
}

func (f *fakeL1DB) GetTemplateByID(_ context.Context, id uuid.UUID) (*models.Template, error) {
	for i := range f.allL1 {
		if f.allL1[i].ID == id {
			return &f.allL1[i], nil
		}
	}
	return nil, nil
}

type fakeL1Metrics struct {
	lastValidated map[string]float64
	pushCalls     int
	pushErr       error
}

func (f *fakeL1Metrics) SetTemplateLastValidated(id string, ts float64) {
	if f.lastValidated == nil {
		f.lastValidated = make(map[string]float64)
	}
	f.lastValidated[id] = ts
}

func (f *fakeL1Metrics) Push(_ context.Context) error {
	f.pushCalls++
	return f.pushErr
}

// makeL1Template returns an active credential-revalidated template with the given ID and
// optional last_validated_at.
func makeL1Template(id uuid.UUID, lastValidated *time.Time) models.Template {
	return models.Template{
		ID:              id,
		Name:            "test-" + id.String()[:8],
		TrustTier:       models.TemplateTrustTierL1,
		IsActive:        true,
		VCenterVMID:     "vm-1234",
		TemplateState:   models.TemplateStateActive,
		LastValidatedAt: lastValidated,
	}
}

// --------------------------------------------------------------------------
// Reconciler selection tests
// --------------------------------------------------------------------------

// TestReconcileL1_SelectsStaleAndIgnoresRecent verifies that only templates
// whose last_validated_at is past the threshold are enqueued.
//
// NEGATIVE CONTROL: this test was verified to FAIL when the stale-template
// selector was patched to always return an empty slice (simulating a bug where
// the reconciler selects nothing). The failure message was:
//
//	"enqueued 0 jobs, want 1"
func TestReconcileL1_SelectsStaleAndIgnoresRecent(t *testing.T) {
	staleID := uuid.New()
	recentID := uuid.New()
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	staleTempl := makeL1Template(staleID, &old)
	recentTempl := makeL1Template(recentID, &recent)

	db := &fakeL1DB{
		allL1:   []models.Template{staleTempl, recentTempl},
		staleL1: []models.Template{staleTempl}, // only stale one
	}
	m := &fakeL1Metrics{}

	counts, err := reconcileL1TrustValidation(context.Background(), db, m, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("reconcileL1TrustValidation: %v", err)
	}
	if counts.L1Templates != 2 {
		t.Errorf("L1Templates = %d, want 2", counts.L1Templates)
	}
	if counts.Enqueued != 1 {
		t.Errorf("enqueued %d jobs, want 1", counts.Enqueued)
	}
	if len(db.createdJobs) != 1 {
		t.Fatalf("created %d jobs, want 1", len(db.createdJobs))
	}
	// Verify job type and payload.
	job := db.createdJobs[0]
	if job.Type != models.JobTypeTemplateRevalidate {
		t.Errorf("job type = %q, want %q", job.Type, models.JobTypeTemplateRevalidate)
	}
	var payload TemplateRevalidatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.TemplateID != staleID {
		t.Errorf("payload.TemplateID = %v, want %v", payload.TemplateID, staleID)
	}
	if payload.VMMoref != staleTempl.VCenterVMID {
		t.Errorf("payload.VMMoref = %q, want %q", payload.VMMoref, staleTempl.VCenterVMID)
	}
}

// TestReconcileL1_NeverValidatedTemplateIncluded verifies that a template with
// last_validated_at=NULL is always treated as stale.
func TestReconcileL1_NeverValidatedTemplateIncluded(t *testing.T) {
	id := uuid.New()
	tmpl := makeL1Template(id, nil) // nil = never validated

	db := &fakeL1DB{
		allL1:   []models.Template{tmpl},
		staleL1: []models.Template{tmpl},
	}
	m := &fakeL1Metrics{}

	counts, err := reconcileL1TrustValidation(context.Background(), db, m, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.Enqueued != 1 {
		t.Errorf("enqueued %d jobs for never-validated template, want 1", counts.Enqueued)
	}
}

func TestReconcileL1_IncludesDerivedAndUntrustedCustomizedTemplates(t *testing.T) {
	derivedID := uuid.New()
	untrustedID := uuid.New()
	derived := makeL1Template(derivedID, nil)
	derived.TrustTier = models.TemplateTrustTierDerived
	untrusted := makeL1Template(untrustedID, nil)
	untrusted.TrustTier = models.TemplateTrustTierUntrusted

	db := &fakeL1DB{
		allL1:       []models.Template{derived, untrusted},
		deriveStale: true,
	}

	counts, err := reconcileL1TrustValidation(context.Background(), db, nil, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.Enqueued != 2 {
		t.Fatalf("enqueued %d jobs, want 2 for derived/untrusted customized templates", counts.Enqueued)
	}

	got := map[uuid.UUID]bool{}
	for _, job := range db.createdJobs {
		var payload TemplateRevalidatePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		got[payload.TemplateID] = true
	}
	for _, want := range []uuid.UUID{derivedID, untrustedID} {
		if !got[want] {
			t.Fatalf("missing revalidation job for template %s", want)
		}
	}
}

// TestReconcileL1_IdentifierSelection verifies that stale templates enqueue
// when they have either identifier, and are skipped only when both are missing.
func TestReconcileL1_IdentifierSelection(t *testing.T) {
	tests := []struct {
		name            string
		vcenterVMID     string
		vcenterTemplate string
		wantEnqueued    int
		wantVMMoref     string
		wantError       bool
	}{
		{
			name:            "enqueues when only vcenter_template is set",
			vcenterTemplate: "student-ubuntu-2404",
			wantEnqueued:    1,
			wantVMMoref:     "",
		},
		{
			name:         "skips when neither identifier is set",
			wantEnqueued: 0,
			wantError:    true,
		},
		{
			name:         "preserves existing vcenter_vm_id",
			vcenterVMID:  "vm-1234",
			wantEnqueued: 1,
			wantVMMoref:  "vm-1234",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			tmpl := makeL1Template(id, nil)
			tmpl.VCenterVMID = tc.vcenterVMID
			tmpl.VCenterTemplate = tc.vcenterTemplate

			db := &fakeL1DB{
				allL1:   []models.Template{tmpl},
				staleL1: []models.Template{tmpl},
			}
			m := &fakeL1Metrics{}

			counts, err := reconcileL1TrustValidation(context.Background(), db, m, discardLogger(),
				L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
			if err != nil && !tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err == nil && tc.wantError {
				t.Fatal("expected missing template identifier to surface as an error")
			}
			if counts.Enqueued != tc.wantEnqueued {
				t.Fatalf("enqueued %d jobs, want %d", counts.Enqueued, tc.wantEnqueued)
			}
			if tc.wantEnqueued == 0 {
				if len(db.createdJobs) != 0 {
					t.Fatalf("created %d jobs, want 0", len(db.createdJobs))
				}
				return
			}
			if len(db.createdJobs) != 1 {
				t.Fatalf("created %d jobs, want 1", len(db.createdJobs))
			}
			var payload TemplateRevalidatePayload
			if err := json.Unmarshal(db.createdJobs[0].Payload, &payload); err != nil {
				t.Fatalf("unmarshal payload: %v", err)
			}
			if payload.TemplateID != id {
				t.Errorf("payload.TemplateID = %v, want %v", payload.TemplateID, id)
			}
			if payload.VMMoref != tc.wantVMMoref {
				t.Errorf("payload.VMMoref = %q, want %q", payload.VMMoref, tc.wantVMMoref)
			}
		})
	}
}

func TestReconcileL1_RestartAfterValidationIntervalCatchesUp(t *testing.T) {
	id := uuid.New()
	lastValidated := time.Now().Add(-8 * 24 * time.Hour)
	tmpl := makeL1Template(id, &lastValidated)
	db := &fakeL1DB{
		allL1:       []models.Template{tmpl},
		deriveStale: true,
	}

	counts, err := reconcileL1TrustValidation(context.Background(), db, nil, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("catch-up reconcile: %v", err)
	}
	if counts.Due != 1 || counts.Enqueued != 1 {
		t.Fatalf("catch-up counts = %+v, want due=1 enqueued=1", counts)
	}
}

func TestReconcileL1_ExistingActiveJobIsDatabaseIdempotent(t *testing.T) {
	for _, status := range []string{
		models.JobStatusPending,
		models.JobStatusClaimed,
		models.JobStatusInProgress,
	} {
		t.Run(status, func(t *testing.T) {
			id := uuid.New()
			tmpl := makeL1Template(id, nil)
			db := &fakeL1DB{
				allL1:      []models.Template{tmpl},
				staleL1:    []models.Template{tmpl},
				activeJobs: map[uuid.UUID]string{id: status},
			}

			counts, err := reconcileL1TrustValidation(context.Background(), db, nil, discardLogger(),
				L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
			if err != nil {
				t.Fatalf("reconcile with active %s job: %v", status, err)
			}
			if counts.Due != 1 || counts.Enqueued != 0 {
				t.Fatalf("counts = %+v, want due=1 enqueued=0", counts)
			}
			if len(db.createdJobs) != 0 {
				t.Fatalf("created %d duplicate jobs", len(db.createdJobs))
			}
		})
	}
}

func TestReconcileL1_EnqueueFailureIsSurfaced(t *testing.T) {
	id := uuid.New()
	tmpl := makeL1Template(id, nil)
	db := &fakeL1DB{
		allL1:        []models.Template{tmpl},
		staleL1:      []models.Template{tmpl},
		createJobErr: errors.New("insert failed"),
	}

	counts, err := reconcileL1TrustValidation(context.Background(), db, nil, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err == nil || !contains(err.Error(), "insert failed") {
		t.Fatalf("enqueue error = %v, want surfaced insert failure", err)
	}
	if counts.Due != 1 || counts.Enqueued != 0 {
		t.Fatalf("counts = %+v, want due=1 enqueued=0", counts)
	}
}

func TestReconcileL1_StopsEnqueueingAfterLeadershipLoss(t *testing.T) {
	firstID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	secondID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	first := makeL1Template(firstID, nil)
	second := makeL1Template(secondID, nil)
	db := &fakeL1DB{
		allL1:   []models.Template{first, second},
		staleL1: []models.Template{first, second},
	}
	leaderChecks := 0
	isLeader := func() bool {
		leaderChecks++
		return leaderChecks <= 2
	}

	counts, err := reconcileL1TrustValidation(context.Background(), db, nil, discardLogger(),
		L1TrustValidationReconcilerConfig{
			Interval: 7 * 24 * time.Hour,
			IsLeader: isLeader,
		})
	if !errors.Is(err, ErrL1ValidationLeadershipLost) {
		t.Fatalf("reconcile error = %v, want ErrL1ValidationLeadershipLost", err)
	}
	if counts.Enqueued != 1 {
		t.Fatalf("enqueued after leadership loss = %d, want exactly first template", counts.Enqueued)
	}
	if len(db.createdJobs) != 1 {
		t.Fatalf("created jobs after leadership loss = %d, want 1", len(db.createdJobs))
	}
}

// --------------------------------------------------------------------------
// Staleness gauge tests (Collected pattern)
// --------------------------------------------------------------------------

// TestReconcileL1_StalenessGaugeEmittedAfterRun verifies the gauge is
// populated with the correct timestamps after one reconciler pass.
func TestReconcileL1_StalenessGaugeEmittedAfterRun(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	id1 := uuid.New()
	id2 := uuid.New()
	t1 := makeL1Template(id1, &ts)
	t2 := makeL1Template(id2, nil) // never validated → expect 0

	db := &fakeL1DB{
		allL1:   []models.Template{t1, t2},
		staleL1: []models.Template{},
	}
	m := &fakeL1Metrics{}

	if _, err := reconcileL1TrustValidation(context.Background(), db, m, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, ok := m.lastValidated[id1.String()]; !ok || got != float64(ts.Unix()) {
		t.Errorf("lastValidated[%v] = %v, want %v", id1, got, float64(ts.Unix()))
	}
	if got, ok := m.lastValidated[id2.String()]; !ok || got != 0 {
		t.Errorf("lastValidated[%v] = %v, want 0 (never validated)", id2, got)
	}
	if m.pushCalls != 1 {
		t.Errorf("Push called %d times, want 1", m.pushCalls)
	}
}

// TestReconcileL1_ListAllErrorAbortsEarly verifies that a DB error on
// ListAllActiveCredentialRevalidationTemplates aborts the pass before enqueuing
// any jobs.
func TestReconcileL1_ListAllErrorAbortsEarly(t *testing.T) {
	db := &fakeL1DB{
		allL1Err: errors.New("db down"),
		staleL1:  []models.Template{makeL1Template(uuid.New(), nil)},
	}
	m := &fakeL1Metrics{}

	_, err := reconcileL1TrustValidation(context.Background(), db, m, discardLogger(),
		L1TrustValidationReconcilerConfig{Interval: 7 * 24 * time.Hour})
	if err == nil {
		t.Fatal("expected error when ListAllActiveCredentialRevalidationTemplates fails")
	}
	if len(db.createdJobs) != 0 {
		t.Errorf("created %d jobs despite DB error, want 0", len(db.createdJobs))
	}
}

// --------------------------------------------------------------------------
// PipelineMetrics Collected-pattern tests
// --------------------------------------------------------------------------

// TestPipelineMetrics_ValidationGaugeNotEmittedBeforeCollection verifies
// that crucible_template_last_validated_timestamp is absent from the
// serialized output before SetTemplateLastValidated is ever called.
func TestPipelineMetrics_ValidationGaugeNotEmittedBeforeCollection(t *testing.T) {
	m := NewPipelineMetrics("", "test", nil)
	// Do NOT call SetTemplateLastValidated.
	body := string(m.serialize())
	if contains(body, "crucible_template_last_validated_timestamp") {
		t.Errorf("staleness gauge emitted before SetTemplateLastValidated was called:\n%s", body)
	}
}

// TestPipelineMetrics_ValidationGaugeEmittedAfterCollection verifies that
// the gauge appears with the correct value after SetTemplateLastValidated.
func TestPipelineMetrics_ValidationGaugeEmittedAfterCollection(t *testing.T) {
	m := NewPipelineMetrics("", "test", nil)
	m.SetTemplateLastValidated("tmpl-abc", 1700000000)
	body := string(m.serialize())
	if !contains(body, "crucible_template_last_validated_timestamp") {
		t.Errorf("staleness gauge not emitted after SetTemplateLastValidated:\n%s", body)
	}
	if !contains(body, `template_id="tmpl-abc"`) {
		t.Errorf("gauge missing template_id label:\n%s", body)
	}
	if !contains(body, "1.7e+09") && !contains(body, "1700000000") {
		t.Errorf("gauge missing expected timestamp value:\n%s", body)
	}
}

// TestPipelineMetrics_ValidationCounterIncremented verifies RecordTemplateValidation
// increments the counter and the correct label values appear.
func TestPipelineMetrics_ValidationCounterIncremented(t *testing.T) {
	m := NewPipelineMetrics("", "test", nil)
	m.RecordTemplateValidation("tmpl-abc", "pass")
	m.RecordTemplateValidation("tmpl-abc", "fail")
	m.RecordTemplateValidation("tmpl-xyz", "pass")
	body := string(m.serialize())

	if !contains(body, "crucible_template_validation_total") {
		t.Errorf("validation counter not emitted:\n%s", body)
	}
	// Verify pass+fail for tmpl-abc, pass for tmpl-xyz.
	if !contains(body, `template_id="tmpl-abc",result="pass"`) {
		t.Errorf("pass counter for tmpl-abc not in output:\n%s", body)
	}
	if !contains(body, `template_id="tmpl-abc",result="fail"`) {
		t.Errorf("fail counter for tmpl-abc not in output:\n%s", body)
	}
	if !contains(body, `template_id="tmpl-xyz",result="pass"`) {
		t.Errorf("pass counter for tmpl-xyz not in output:\n%s", body)
	}
}

// --------------------------------------------------------------------------
// RevalidateL1Template: failure path must NOT unpublish
// --------------------------------------------------------------------------

// TestRevalidateL1_FailureDoesNotUnpublish verifies that when the smoke check
// fails, the template's is_active and template_state are NEVER modified.
//
// NEGATIVE CONTROL: this test was verified to FAIL when a bug was introduced
// into revalidateL1TemplateCore that called db.SetTemplateActive(ctx, id, false)
// on smoke check failure. The failure message was:
//
//	"FAIL: SetTemplateActive was called 1 time(s), but must NEVER be called on revalidation failure (alert-only policy)"
//
// (verified by temporarily adding db.SetTemplateActive(ctx, tmpl.ID, false)
// inside revalidateL1TemplateCore's checkErr != nil branch, then confirming
// the test caught it via the setActiveCalls counter)
func TestRevalidateL1_FailureDoesNotUnpublish(t *testing.T) {
	tmplID := uuid.New()
	tmpl := makeL1Template(tmplID, nil)
	tmpl.VCenterVMID = "vm-9999"

	db := &fakeRevalidateDB{
		tmpl:     &tmpl,
		isActive: true,
	}
	metrics := &fakeRevalidateMetrics{}

	// Simulate a smoke check failure.
	smokeErr := errors.New("smoke check: CloneVM: dial tcp: connection refused")

	revalidateL1TemplateCore(context.Background(), db, metrics, discardLogger(), &tmpl, smokeErr)

	// KEY ASSERTION: is_active must NOT have been changed.
	if db.setActiveCalls > 0 {
		t.Errorf("FAIL: SetTemplateActive was called %d time(s), but must NEVER be called on revalidation failure (alert-only policy)", db.setActiveCalls)
	}
	if db.setActiveValue != nil && !*db.setActiveValue {
		t.Error("FAIL: template was unpublished (is_active = false), but should remain published")
	}

	// Validation state must have been recorded.
	if db.validationResult == "" {
		t.Error("validation result was not persisted to DB")
	}
	if len(db.validationResult) < 5 || db.validationResult[:5] != "fail:" {
		t.Errorf("validation result = %q, want prefix %q", db.validationResult, "fail:")
	}

	// Metric must have been recorded.
	if metrics.validations["fail"] == 0 {
		t.Errorf("fail metric not recorded: validations = %v", metrics.validations)
	}
}

// TestRevalidateL1_SuccessRecordsPassResult verifies the happy path:
// result is "pass", metric is recorded, and validation state is persisted.
func TestRevalidateL1_SuccessRecordsPassResult(t *testing.T) {
	tmplID := uuid.New()
	tmpl := makeL1Template(tmplID, nil)
	db := &fakeRevalidateDB{isActive: true}
	metrics := &fakeRevalidateMetrics{}

	revalidateL1TemplateCore(context.Background(), db, metrics, discardLogger(), &tmpl, nil)

	if db.validationResult != "pass" {
		t.Errorf("validation result = %q, want %q", db.validationResult, "pass")
	}
	if metrics.validations["pass"] == 0 {
		t.Errorf("pass metric not recorded: validations = %v", metrics.validations)
	}
	if db.setActiveCalls != 0 {
		t.Errorf("SetTemplateActive called %d time(s) on success path, want 0", db.setActiveCalls)
	}
}

// --------------------------------------------------------------------------
// Helpers / fake DB for revalidateL1TemplateCore tests
// --------------------------------------------------------------------------

// fakeRevalidateDB implements revalidateL1CoreDB.
// SetTemplateActive is deliberately included to detect any accidental unpublish
// calls — the test will catch them via setActiveCalls.
type fakeRevalidateDB struct {
	tmpl             *models.Template
	isActive         bool
	getCalls         int
	setActiveCalls   int
	setActiveValue   *bool
	validationResult string
	validationAt     time.Time
}

var _ revalidateL1CoreDB = (*fakeRevalidateDB)(nil)
var _ revalidateL1TemplateDB = (*fakeRevalidateDB)(nil)

func (f *fakeRevalidateDB) SetTemplateActive(_ context.Context, _ uuid.UUID, active bool) error {
	f.setActiveCalls++
	f.setActiveValue = &active
	f.isActive = active
	return nil
}

func (f *fakeRevalidateDB) SetTemplateValidationState(_ context.Context, _ uuid.UUID, result string, at time.Time) error {
	f.validationResult = result
	f.validationAt = at
	return nil
}

func (f *fakeRevalidateDB) MarkTemplateGuestCredentialsVerified(_ context.Context, _ uuid.UUID) error {
	return nil
}

func (f *fakeRevalidateDB) GetTemplateByID(_ context.Context, id uuid.UUID) (*models.Template, error) {
	f.getCalls++
	if f.tmpl == nil || f.tmpl.ID != id {
		return nil, nil
	}
	return f.tmpl, nil
}

type fakeRevalidateMetrics struct {
	validations   map[string]int
	lastValidated map[string]float64
}

var _ revalidateL1CorePipeline = (*fakeRevalidateMetrics)(nil)

func (f *fakeRevalidateMetrics) RecordTemplateValidation(_, result string) {
	if f.validations == nil {
		f.validations = make(map[string]int)
	}
	f.validations[result]++
}

func (f *fakeRevalidateMetrics) SetTemplateLastValidated(templateID string, unixSec float64) {
	if f.lastValidated == nil {
		f.lastValidated = make(map[string]float64)
	}
	f.lastValidated[templateID] = unixSec
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

// TestRevalidateL1_PushesLastValidatedGauge is the regression test for a
// production defect: the smoke check passed and templates.last_validated_at was
// written, but crucible_template_last_validated_timestamp stayed at its
// pre-validation value because ONLY the trust reconciler pushed that gauge --
// and it runs weekly. The alert
// `time() - crucible_template_last_validated_timestamp > 8d` therefore kept
// firing for up to a full reconcile interval AFTER a successful validation.
//
// The gauge must advance on BOTH outcomes, mirroring SetTemplateValidationState
// (which writes last_validated_at for pass and fail alike). Staleness means
// "nobody checked recently"; a failing check is carried by the separate
// RecordTemplateValidation result label, so advancing the gauge on failure
// avoids double-alerting on a single fault.
func TestRevalidateL1_PushesLastValidatedGauge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkErr error
	}{
		{"pass", nil},
		{"fail", errors.New("smoke check: CloneVM: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmplID := uuid.New()
			tmpl := makeL1Template(tmplID, nil)
			db := &fakeRevalidateDB{isActive: true}
			metrics := &fakeRevalidateMetrics{}

			before := time.Now().Add(-time.Second)
			revalidateL1TemplateCore(context.Background(), db, metrics, discardLogger(), &tmpl, tc.checkErr)
			after := time.Now().Add(time.Second)

			got, ok := metrics.lastValidated[tmplID.String()]
			if !ok {
				t.Fatalf("crucible_template_last_validated_timestamp was never pushed for %s; "+
					"the staleness alert will keep firing until the next weekly reconcile "+
					"even though this validation just ran (gauge map = %v)", tmplID, metrics.lastValidated)
			}
			if got < float64(before.Unix()) || got > float64(after.Unix()) {
				t.Errorf("gauge = %v, want a current timestamp in [%d, %d]",
					got, before.Unix(), after.Unix())
			}

			// The gauge must agree with what was written to the DB, otherwise
			// the dashboard and the templates table tell different stories.
			if db.validationAt.IsZero() {
				t.Fatal("validationAt was not persisted; cannot compare gauge to DB")
			}
			if delta := got - float64(db.validationAt.Unix()); delta > 1 || delta < -1 {
				t.Errorf("gauge (%v) and DB last_validated_at (%d) disagree by %vs; they must track together",
					got, db.validationAt.Unix(), delta)
			}
		})
	}
}
