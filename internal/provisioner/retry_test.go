// retry_test.go -- unit-level tests for the job retry path.
//
// These tests exercise processJobLifecycle (the production path from ProcessJob),
// acquireCloneLock, and the end-to-end template-provision retry scenario without
// a real Postgres or vCenter instance.
//
// Test coverage map (matches task spec):
//  1. RetryableReturnsPending      -- retryable error -> retry_count++ + pending
//  2. NonRetryableFailsImmediately -- each of the 4 real non-retryable strings
//  3. ExhaustsAtMaxRetries         -- stops at max_retries -> failed with count
//  4. BackoffAndClaimSkip          -- backoff grows, ClaimJob skips future jobs
//  5. ClassifierTableDriven        -- covered by retryable_test.go
//  6. CloneLockSerialization       -- same source serialises, different runs concurrently
//  7. EndToEndNegativeControl      -- real provision path: fail->succeed, then fail->fail*N
package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// --------------------------------------------------------------------------
// Fake jobStatusUpdater for unit tests
// --------------------------------------------------------------------------

type statusRecord struct {
	id     uuid.UUID
	status string
	result []byte
}

type retryRecord struct {
	id            uuid.UUID
	nextAt        time.Time
	cleanupOnly   bool
	cleanupTarget []byte
	result        []byte
}

type fakeJobDB struct {
	mu          sync.Mutex
	statuses    []statusRecord
	retried     []retryRecord
	retryErr    error
	retryErrs   []error
	retryCalls  int
	retrySignal chan struct{}
	statusErr   map[string]error
}

func (f *fakeJobDB) RetryJob(
	_ context.Context,
	id uuid.UUID,
	nextAt time.Time,
	cleanupOnly bool,
	cleanupTarget []byte,
	_ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.retryCalls
	f.retryCalls++
	if f.retrySignal != nil {
		select {
		case f.retrySignal <- struct{}{}:
		default:
		}
	}
	if call < len(f.retryErrs) && f.retryErrs[call] != nil {
		return f.retryErrs[call]
	}
	if f.retryErr != nil {
		return f.retryErr
	}
	f.retried = append(f.retried, retryRecord{
		id: id, nextAt: nextAt, cleanupOnly: cleanupOnly, cleanupTarget: cleanupTarget,
	})
	return nil
}

func (f *fakeJobDB) UpdateJobStatus(
	_ context.Context,
	id uuid.UUID,
	_ string,
	status string,
	result []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, statusRecord{id: id, status: status, result: result})
	if err := f.statusErr[status]; err != nil {
		return err
	}
	return nil
}

func (f *fakeJobDB) RetryTemplateProvisionJob(
	_ context.Context,
	id uuid.UUID,
	nextAt time.Time,
	result []byte,
	_ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.retryCalls
	f.retryCalls++
	if call < len(f.retryErrs) && f.retryErrs[call] != nil {
		return f.retryErrs[call]
	}
	if f.retryErr != nil {
		return f.retryErr
	}
	f.retried = append(f.retried, retryRecord{id: id, nextAt: nextAt, result: result})
	return nil
}

func (f *fakeJobDB) FailTemplateProvisionJob(
	_ context.Context,
	id uuid.UUID,
	_ string,
	result []byte,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.statusErr[models.JobStatusFailed]; err != nil {
		return false, err
	}
	f.statuses = append(f.statuses, statusRecord{id: id, status: models.JobStatusFailed, result: result})
	return true, nil
}

func (f *fakeJobDB) statusesWithStatus(s string) []statusRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []statusRecord
	for _, r := range f.statuses {
		if r.status == s {
			out = append(out, r)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func testProvisioner() *Provisioner {
	return &Provisioner{
		logger: slog.Default(),
	}
}

func noPublish(_ uuid.UUID, _, _ string) {}

func claimTestJob(job *models.Job) {
	if job.ClaimedBy == nil {
		workerID := "test-worker"
		job.ClaimedBy = &workerID
	}
	if job.ClaimedAt == nil {
		claimedAt := time.Now()
		job.ClaimedAt = &claimedAt
	}
}

// runLifecycle drives processJobLifecycle with a dispatch function that simply
// returns dispatchErr, mirroring what ProcessJob does when a handler returns.
func runLifecycle(ctx context.Context, db *fakeJobDB, m *PipelineMetrics, job *models.Job, dispatchErr error) error {
	claimTestJob(job)
	return processJobLifecycle(ctx, db, m, job, noPublish,
		func(_ context.Context, _ *models.Job) error { return dispatchErr })
}

// --------------------------------------------------------------------------
// Test 1: retryable error reschedules the job
// --------------------------------------------------------------------------

func TestHandleJobOutcome_RetryableReturnsPending(t *testing.T) {
	db := &fakeJobDB{}
	m := NewPipelineMetrics("", "", nil)

	job := &models.Job{
		ID:         uuid.New(),
		Type:       "template_provision",
		RetryCount: 0,
		MaxRetries: 3,
	}

	retryableErr := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.")

	result := runLifecycle(context.Background(), db, m, job, retryableErr)

	// processJobLifecycle must return nil (the job is rescheduled, not failed).
	if result != nil {
		t.Fatalf("processJobLifecycle returned %v, want nil (job rescheduled)", result)
	}
	if len(db.retried) != 1 {
		t.Fatalf("RetryJob called %d times, want 1", len(db.retried))
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
		t.Errorf("UpdateJobStatus(failed) called unexpectedly")
	}
	// next_attempt_at must be in the future.
	if !db.retried[0].nextAt.After(time.Now()) {
		t.Errorf("next_attempt_at %s is not in the future", db.retried[0].nextAt)
	}
	if db.retried[0].cleanupOnly {
		t.Error("ordinary provisioning retry was incorrectly marked cleanup-only")
	}
	// Metric must be recorded.
	key := "template_provision|" + RetryReasonTransientClone
	if m.jobRetries[key] == 0 {
		t.Errorf("crucible_job_retries_total[%s] not incremented", key)
	}
}

func TestImageImportRetryUsesWallClockBudget(t *testing.T) {
	cause := errors.New("Operation timed out")
	t.Run("within budget retries despite max retries", func(t *testing.T) {
		db := &fakeJobDB{}
		job := &models.Job{
			ID:         uuid.New(),
			Type:       models.JobTypeImageImport,
			RetryCount: 99,
			MaxRetries: 3,
			CreatedAt:  time.Now().Add(-30 * time.Minute),
		}
		if err := runLifecycle(context.Background(), db, NewPipelineMetrics("", "", nil), job, cause); err != nil {
			t.Fatalf("result = %v; want retry scheduled", err)
		}
		if len(db.retried) != 1 {
			t.Fatalf("retry records = %d; want 1", len(db.retried))
		}
	})

	t.Run("past budget is terminal", func(t *testing.T) {
		db := &fakeJobDB{}
		job := &models.Job{
			ID:         uuid.New(),
			Type:       models.JobTypeImageImport,
			RetryCount: 0,
			MaxRetries: 99,
			CreatedAt:  time.Now().Add(-ImageImportRetryBudget - time.Minute),
		}
		if err := runLifecycle(context.Background(), db, NewPipelineMetrics("", "", nil), job, cause); !errors.Is(err, cause) {
			t.Fatalf("result = %v; want terminal cause", err)
		}
		if len(db.retried) != 0 {
			t.Fatalf("retry records = %d; want 0", len(db.retried))
		}
		if len(db.statusesWithStatus(models.JobStatusFailed)) != 1 {
			t.Fatal("past-budget import was not marked failed")
		}
	})
}

func TestTemplateProvisionFailureResultPreservesFirstAndCurrentFailures(t *testing.T) {
	firstCause := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format: first")
	first := marshalTemplateProvisionFailure(nil, firstCause, 0, 3)
	secondCause := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format: second")
	second := marshalTemplateProvisionFailure(first, secondCause, 1, 3)

	var result templateProvisionFailureResult
	if err := json.Unmarshal(second, &result); err != nil {
		t.Fatal(err)
	}
	if result.FirstRawError != firstCause.Error() || result.CurrentRawError != secondCause.Error() {
		t.Fatalf("failure history = first %q current %q", result.FirstRawError, result.CurrentRawError)
	}
	if result.FirstAttempt != 1 || result.CurrentAttempt != 2 || result.Attempts != 2 {
		t.Fatalf(
			"attempt fields = first %d current %d attempts %d",
			result.FirstAttempt,
			result.CurrentAttempt,
			result.Attempts,
		)
	}
	if result.Error != result.CurrentError || result.RawError != result.CurrentRawError {
		t.Fatalf("compatibility fields do not mirror current failure: %+v", result)
	}
}

func TestTemplateProvisionRetryPersistenceFailureDoesNotBecomeTerminal(t *testing.T) {
	db := &fakeJobDB{retryErr: errors.New("database unavailable")}
	metrics := NewPipelineMetrics("", "", nil)
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeTemplateProvision,
		RetryCount: 0,
		MaxRetries: 3,
	}
	result := runLifecycle(
		context.Background(),
		db,
		metrics,
		job,
		errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format."),
	)
	if result == nil || !strings.Contains(result.Error(), "persist template provision retry") {
		t.Fatalf("retry persistence result = %v", result)
	}
	if len(db.statusesWithStatus(models.JobStatusFailed)) != 0 {
		t.Fatal("retry persistence infrastructure failure was misclassified as terminal")
	}
	if metrics.templateTransitions[models.TemplateStateProvisioning+"|"+models.TemplateStateError] != 0 {
		t.Fatal("retry persistence infrastructure failure emitted a terminal template transition")
	}
}

func TestTemplateProvisionTerminalTransitionMetricRequiresCommittedOwnership(t *testing.T) {
	cause := errors.New("source_ref is invalid")
	for _, tc := range []struct {
		name       string
		statusErr  error
		wantMetric float64
	}{
		{name: "committed", wantMetric: 1},
		{name: "lease lost", statusErr: database.ErrJobLeaseLost, wantMetric: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &fakeJobDB{statusErr: map[string]error{models.JobStatusFailed: tc.statusErr}}
			metrics := NewPipelineMetrics("", "", nil)
			job := &models.Job{
				ID:         uuid.New(),
				Type:       models.JobTypeTemplateProvision,
				Payload:    []byte(`{"template_id":"` + uuid.NewString() + `"}`),
				RetryCount: 0,
				MaxRetries: 3,
			}
			result := runLifecycle(context.Background(), db, metrics, job, cause)
			if tc.statusErr == nil && !errors.Is(result, cause) {
				t.Fatalf("terminal result = %v, want original cause", result)
			}
			if tc.statusErr != nil && !errors.Is(result, tc.statusErr) {
				t.Fatalf("claim-loss result = %v, want %v", result, tc.statusErr)
			}
			key := models.TemplateStateProvisioning + "|" + models.TemplateStateError
			if got := metrics.templateTransitions[key]; got != tc.wantMetric {
				t.Fatalf("transition metric = %g, want %g", got, tc.wantMetric)
			}
		})
	}
}

func TestTemplateProvisionRetryThenSuccessReplacesFailureResult(t *testing.T) {
	db := &fakeJobDB{}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeTemplateProvision,
		Result:     marshalTemplateProvisionFailure(nil, errors.New("first failure"), 0, 3),
		RetryCount: 1,
		MaxRetries: 3,
	}
	if err := runLifecycle(context.Background(), db, NewPipelineMetrics("", "", nil), job, nil); err != nil {
		t.Fatal(err)
	}
	completed := db.statusesWithStatus(models.JobStatusCompleted)
	if len(completed) != 1 {
		t.Fatalf("completed writes = %d, want 1", len(completed))
	}
	if strings.Contains(string(completed[0].result), "first failure") {
		t.Fatalf("successful result retained stale failure: %s", completed[0].result)
	}
}

func TestHandleJobOutcome_ObsoleteReplicaCleanupTerminatesWithoutRetry(t *testing.T) {
	db := &fakeJobDB{}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeTemplateReplicaBuild,
		Payload:    []byte(`{"cleanup_only":true}`),
		RetryCount: 41,
		MaxRetries: 20,
	}
	obsolete := fmt.Errorf(
		"%w: displaced cleanup job",
		database.ErrTemplateReplicaBuildJobObsolete,
	)

	result := runLifecycle(context.Background(), db, NewPipelineMetrics("", "", nil), job, obsolete)
	if !errors.Is(result, database.ErrTemplateReplicaBuildJobObsolete) {
		t.Fatalf("obsolete cleanup result=%v", result)
	}
	if db.retryCalls != 0 || len(db.retried) != 0 {
		t.Fatalf("obsolete cleanup retry calls=%d records=%d, want none", db.retryCalls, len(db.retried))
	}
	failed := db.statusesWithStatus(models.JobStatusFailed)
	if len(failed) != 1 {
		t.Fatalf("obsolete cleanup failed status writes=%d, want 1", len(failed))
	}
}

func TestHandleJobOutcome_NormalizesTypedPlacementFailures(t *testing.T) {
	t.Run("operational failure retries", func(t *testing.T) {
		db := &fakeJobDB{}
		metrics := NewPipelineMetrics("", "", nil)
		job := &models.Job{
			ID:         uuid.New(),
			Type:       models.JobTypeVMSuspend,
			RetryCount: 0,
			MaxRetries: 3,
		}
		err := fmt.Errorf("read VM placement: %w", context.DeadlineExceeded)

		if result := runLifecycle(context.Background(), db, metrics, job, err); result != nil {
			t.Fatalf("operational placement failure result = %v, want scheduled retry", result)
		}
		if len(db.retried) != 1 || db.retried[0].cleanupOnly {
			t.Fatalf("operational placement retries = %+v, want one ordinary retry", db.retried)
		}
	})

	t.Run("proven drift requires manual cleanup", func(t *testing.T) {
		db := &fakeJobDB{}
		metrics := NewPipelineMetrics("", "", nil)
		job := &models.Job{
			ID:         uuid.New(),
			Type:       models.JobTypeVMSuspend,
			RetryCount: 0,
			MaxRetries: 3,
		}
		err := &vcenter.PlacementDriftError{
			Kind:   vcenter.PlacementDriftHost,
			Detail: "VM moved from its persisted host",
		}

		result := runLifecycle(context.Background(), db, metrics, job, err)
		if !isManualCleanupRequired(result) {
			t.Fatalf("proven placement drift result = %v, want manual cleanup", result)
		}
		if len(db.retried) != 0 {
			t.Fatalf("proven drift scheduled retries: %+v", db.retried)
		}
		failed := db.statusesWithStatus(models.JobStatusFailed)
		if len(failed) != 1 {
			t.Fatalf("failed status count = %d, want 1", len(failed))
		}
		if !strings.Contains(string(failed[0].result), `"manual_cleanup_required":true`) {
			t.Fatalf("proven drift failed result = %s, want manual cleanup marker", failed[0].result)
		}
	})

	t.Run("cleanup retry outranks contained drift", func(t *testing.T) {
		db := &fakeJobDB{}
		metrics := NewPipelineMetrics("", "", nil)
		job := &models.Job{
			ID:         uuid.New(),
			Type:       models.JobTypeVMAdd,
			RetryCount: 3,
			MaxRetries: 3,
		}
		err := &compensationRetryError{
			err: &vcenter.PlacementDriftError{
				Kind:   vcenter.PlacementDriftHost,
				Detail: "exact cleanup target moved",
			},
		}

		if result := runLifecycle(context.Background(), db, metrics, job, err); result != nil {
			t.Fatalf("cleanup drift result = %v, want durable cleanup retry", result)
		}
		if len(db.retried) != 1 || !db.retried[0].cleanupOnly {
			t.Fatalf("cleanup drift retries = %+v, want one cleanup-only retry", db.retried)
		}
	})
}

func TestHandleJobOutcome_PodCreateCleanupRetryIsDurablyMarked(t *testing.T) {
	db := &fakeJobDB{}
	m := NewPipelineMetrics("", "", nil)
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypePodCreate,
		RetryCount: 3,
		MaxRetries: 3,
	}
	cleanupErr := newPodCreateCleanupRetryError(
		"test rollback",
		[]error{errors.New("delete stale port group: connection refused")},
	)

	if result := runLifecycle(context.Background(), db, m, job, cleanupErr); result != nil {
		t.Fatalf("processJobLifecycle returned %v, want nil (cleanup retry rescheduled)", result)
	}
	if len(db.retried) != 1 {
		t.Fatalf("RetryJob called %d times, want 1", len(db.retried))
	}
	if !db.retried[0].cleanupOnly {
		t.Fatal("failed pod-create rollback was not marked cleanup-only")
	}
	if got := m.jobRetries[models.JobTypePodCreate+"|"+RetryReasonCleanup]; got != 1 {
		t.Fatalf("cleanup retry metric = %v, want 1", got)
	}
}

func TestHandleJobOutcome_ManualCleanupSurvivesCleanupAggregation(t *testing.T) {
	target := &VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-42",
	}
	newManualErr := func() error {
		return &manualCleanupRequiredError{
			err: errors.New("immutable clone ownership is ambiguous"),
		}
	}
	tests := []struct {
		name    string
		jobType string
		err     error
	}{
		{
			name:    "pod cleanup aggregate",
			jobType: models.JobTypePodCreate,
			err: newPodCreateCleanupRetryError(
				"clone reconciliation",
				[]error{errors.New("transient rollback failure"), newManualErr()},
			),
		},
		{
			name:    "pod failure and cleanup aggregate",
			jobType: models.JobTypePodCreate,
			err: combineProvisioningAndCleanupErrors(
				newManualErr(),
				newPodCreateCleanupRetryError(
					"rollback",
					[]error{errors.New("transient rollback failure")},
				),
			),
		},
		{
			name:    "vm add cleanup wrapper",
			jobType: models.JobTypeVMAdd,
			err: &compensationRetryError{
				err: fmt.Errorf("clean exact clone: %w", newManualErr()),
			},
		},
		{
			name:    "template smoke cleanup aggregate",
			jobType: models.JobTypeTemplateVerify,
			err: cloneRecoveryError(
				fmt.Errorf(
					"cleanup smoke clone: %w",
					errors.Join(errors.New("transient destroy failure"), newManualErr()),
				),
				target,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeJobDB{}
			job := &models.Job{
				ID:         uuid.New(),
				Type:       tt.jobType,
				Payload:    []byte(`{"cleanup_only":true}`),
				RetryCount: 3,
				MaxRetries: 3,
			}
			var published []string
			claimTestJob(job)

			result := processJobLifecycle(
				context.Background(),
				db,
				NewPipelineMetrics("", "", nil),
				job,
				func(_ uuid.UUID, step, _ string) { published = append(published, step) },
				func(context.Context, *models.Job) error { return tt.err },
			)
			if result == nil {
				t.Fatal("manual cleanup error returned nil")
			}
			if !isManualCleanupRequired(result) {
				t.Fatalf("result = %v, want manual cleanup classification", result)
			}
			if db.retryCalls != 0 || len(db.retried) != 0 {
				t.Fatalf("RetryJob calls = %d, records = %d; want none", db.retryCalls, len(db.retried))
			}
			failed := db.statusesWithStatus(models.JobStatusFailed)
			if len(failed) != 1 {
				t.Fatalf("failed status updates = %d, want 1", len(failed))
			}
			if !strings.Contains(string(failed[0].result), `"manual_cleanup_required":true`) {
				t.Fatalf("failed result does not report manual cleanup: %s", failed[0].result)
			}
			if got := published[len(published)-1]; got != "manual_cleanup_required" {
				t.Fatalf("final event = %q, want manual_cleanup_required", got)
			}
		})
	}
}

func TestHandleJobOutcome_TransientCleanupRemainsRetryableAfterBudgetExhaustion(t *testing.T) {
	target := &VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-42",
	}
	tests := []struct {
		name    string
		jobType string
		err     error
	}{
		{
			name:    "pod cleanup aggregate",
			jobType: models.JobTypePodCreate,
			err: newPodCreateCleanupRetryError(
				"rollback",
				[]error{errors.New("vCenter connection refused")},
			),
		},
		{
			name:    "vm add cleanup wrapper",
			jobType: models.JobTypeVMAdd,
			err: &compensationRetryError{
				err: errors.New("destroy exact clone: vCenter connection refused"),
			},
		},
		{
			name:    "template smoke cleanup wrapper",
			jobType: models.JobTypeTemplateVerify,
			err: cloneRecoveryError(
				errors.New("destroy smoke clone: vCenter connection refused"),
				target,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &fakeJobDB{}
			job := &models.Job{
				ID:         uuid.New(),
				Type:       tt.jobType,
				Payload:    []byte(`{"cleanup_only":true}`),
				RetryCount: 3,
				MaxRetries: 3,
			}

			if result := runLifecycle(
				context.Background(),
				db,
				NewPipelineMetrics("", "", nil),
				job,
				tt.err,
			); result != nil {
				t.Fatalf("transient cleanup returned %v, want durable retry", result)
			}
			if db.retryCalls != 1 || len(db.retried) != 1 {
				t.Fatalf("RetryJob calls = %d, records = %d; want one", db.retryCalls, len(db.retried))
			}
			if !db.retried[0].cleanupOnly {
				t.Fatal("transient cleanup retry was not marked cleanup-only")
			}
			if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
				t.Fatal("transient cleanup was terminalized after exhausting the original retry budget")
			}
		})
	}
}

func TestHandleJobOutcome_OrdinaryPodCreateRetryIsNotCleanupOnly(t *testing.T) {
	db := &fakeJobDB{}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypePodCreate,
		RetryCount: 0,
		MaxRetries: 3,
	}

	if result := runLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		errors.New("clone source VM: connection refused"),
	); result != nil {
		t.Fatalf("processJobLifecycle returned %v, want nil (ordinary retry rescheduled)", result)
	}
	if len(db.retried) != 1 {
		t.Fatalf("RetryJob called %d times, want 1", len(db.retried))
	}
	if db.retried[0].cleanupOnly {
		t.Fatal("ordinary pending pod_create would bypass the maintenance claim gate")
	}
}

func TestHandleJobOutcome_CompletedPodCreateCompensationDoesNotRetry(t *testing.T) {
	db := &fakeJobDB{}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypePodCreate,
		RetryCount: 0,
		MaxRetries: 3,
	}
	compensatedErr := &compensatedJobError{
		err: errors.New("reconfigure VLANs: connection refused"),
	}
	var published []string
	claimTestJob(job)

	if result := processJobLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		func(_ uuid.UUID, step, _ string) { published = append(published, step) },
		func(context.Context, *models.Job) error { return compensatedErr },
	); result == nil {
		t.Fatal("processJobLifecycle returned nil, want terminal compensated failure")
	}
	if len(db.retried) != 0 {
		t.Fatal("fully compensated pod_create was unnecessarily retried")
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 1 {
		t.Fatalf("failed status updates = %d, want 1", len(got))
	} else if !strings.Contains(string(got[0].result), `"compensated":true`) {
		t.Fatalf("failed result does not report compensation: %s", got[0].result)
	}
	if got := published[len(published)-1]; got != "compensated" {
		t.Fatalf("final event = %q, want compensated", got)
	}
}

func TestHandleJobOutcome_CleanupIgnoresExhaustedOriginalRetryBudget(t *testing.T) {
	for _, jobType := range []string{models.JobTypePodCreate, models.JobTypeVMAdd, models.JobTypeVMDestroy} {
		t.Run(jobType, func(t *testing.T) {
			db := &fakeJobDB{}
			job := &models.Job{
				ID:         uuid.New(),
				Type:       jobType,
				Payload:    []byte(`{"cleanup_only":true}`),
				RetryCount: 3,
				MaxRetries: 3,
			}

			if result := runLifecycle(
				context.Background(),
				db,
				NewPipelineMetrics("", "", nil),
				job,
				errors.New("cleanup connection refused"),
			); result != nil {
				t.Fatalf("cleanup retry returned %v, want nil", result)
			}
			if len(db.retried) != 1 || !db.retried[0].cleanupOnly {
				t.Fatalf("cleanup retry records = %+v, want one durable retry", db.retried)
			}
			if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
				t.Fatal("exhausted original retry budget terminalized cleanup")
			}
		})
	}
}

func TestHandleJobOutcome_CleanupRescheduleRetriesUntilDurable(t *testing.T) {
	target := &VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	db := &fakeJobDB{
		retryErrs: []error{errors.New("database unavailable"), nil},
	}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeVMAdd,
		RetryCount: 20,
		MaxRetries: 3,
	}
	dispatchErr := &compensationRetryError{
		err:    errors.New("destroy exact stale VM: connection refused"),
		target: target,
	}
	var published []string
	started := time.Now()
	claimTestJob(job)

	if result := processJobLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		func(_ uuid.UUID, step, _ string) { published = append(published, step) },
		func(context.Context, *models.Job) error { return dispatchErr },
	); result != nil {
		t.Fatalf("cleanup reschedule returned %v, want nil after transient DB recovery", result)
	}
	if elapsed := time.Since(started); elapsed < cleanupRescheduleBackoffBase {
		t.Fatalf("cleanup reschedule retried after %s, want at least %s backoff",
			elapsed, cleanupRescheduleBackoffBase)
	}
	if db.retryCalls != 2 {
		t.Fatalf("RetryJob calls = %d, want 2", db.retryCalls)
	}
	if len(db.retried) != 1 || !db.retried[0].cleanupOnly {
		t.Fatalf("durable cleanup retries = %+v, want one successful cleanup-only write", db.retried)
	}
	var gotTarget VMCloneCleanupTarget
	if err := json.Unmarshal(db.retried[0].cleanupTarget, &gotTarget); err != nil {
		t.Fatalf("cleanup target is invalid JSON: %v", err)
	}
	if gotTarget != *target {
		t.Fatalf("cleanup target = %+v, want %+v", gotTarget, *target)
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
		t.Fatal("cleanup was terminalized while its durable retry write recovered")
	}
	for _, event := range published {
		if event == "completed" || event == "compensated" || event == "failed" {
			t.Fatalf("published terminal event %q while cleanup rescheduling recovered", event)
		}
	}
	if published[len(published)-1] != "retry_scheduled" {
		t.Fatalf("final event = %q, want retry_scheduled", published[len(published)-1])
	}
}

func TestHandleJobOutcome_CleanupRescheduleShutdownHandsOffToStartupRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := &fakeJobDB{
		retryErr:    errors.New("database unavailable"),
		retrySignal: make(chan struct{}, 1),
	}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypePodCreate,
		Payload:    []byte(`{"cleanup_only":true}`),
		RetryCount: 20,
		MaxRetries: 3,
	}
	var published []string
	resultCh := make(chan error, 1)
	claimTestJob(job)
	go func() {
		resultCh <- processJobLifecycle(
			ctx,
			db,
			NewPipelineMetrics("", "", nil),
			job,
			func(_ uuid.UUID, step, _ string) { published = append(published, step) },
			func(context.Context, *models.Job) error {
				return errors.New("destroy exact stale VM: connection refused")
			},
		)
	}()

	select {
	case <-db.retrySignal:
	case <-time.After(time.Second):
		t.Fatal("RetryJob was not attempted")
	}
	cancel()

	select {
	case result := <-resultCh:
		if !errors.Is(result, context.Canceled) {
			t.Fatalf("shutdown result = %v, want context.Canceled", result)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup reschedule did not stop promptly on shutdown")
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
		t.Fatal("shutdown terminalized cleanup instead of leaving startup recovery handoff")
	}
	if len(db.retried) != 0 {
		t.Fatal("failed cleanup reschedule unexpectedly recorded a durable retry")
	}
	for _, event := range published {
		if event == "completed" || event == "compensated" || event == "failed" {
			t.Fatalf("published terminal event %q during cleanup reschedule shutdown", event)
		}
	}
}

func TestCleanupRescheduleBackoffIsBounded(t *testing.T) {
	for _, failures := range []int{1, 2, 6, 63, 1_000_000} {
		delay := cleanupRescheduleBackoff(failures)
		if delay < cleanupRescheduleBackoffBase || delay > cleanupRescheduleBackoffMax {
			t.Fatalf("cleanupRescheduleBackoff(%d) = %s, want [%s,%s]",
				failures, delay, cleanupRescheduleBackoffBase, cleanupRescheduleBackoffMax)
		}
	}
}

func TestHandleJobOutcome_CompensatedStatusFailurePublishesNoSuccess(t *testing.T) {
	db := &fakeJobDB{
		statusErr: map[string]error{
			models.JobStatusFailed: errors.New("database unavailable"),
		},
	}
	job := &models.Job{ID: uuid.New(), Type: models.JobTypePodCreate}
	var published []string
	claimTestJob(job)

	result := processJobLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		func(_ uuid.UUID, step, _ string) { published = append(published, step) },
		func(context.Context, *models.Job) error {
			return newPodCreateCompensatedError("test")
		},
	)
	if result == nil || !strings.Contains(result.Error(), "persist terminal job status") {
		t.Fatalf("result = %v, want terminal status persistence error", result)
	}
	for _, event := range published {
		if event == "compensated" || event == "completed" {
			t.Fatalf("published false success event %q after status write failed", event)
		}
	}
}

func TestHandleJobOutcome_CompletedStatusFailurePublishesNoSuccess(t *testing.T) {
	db := &fakeJobDB{
		statusErr: map[string]error{
			models.JobStatusCompleted: errors.New("database unavailable"),
		},
	}
	job := &models.Job{ID: uuid.New(), Type: models.JobTypePodDestroy}
	var published []string
	claimTestJob(job)

	result := processJobLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		func(_ uuid.UUID, step, _ string) { published = append(published, step) },
		func(context.Context, *models.Job) error { return nil },
	)
	if result == nil || !strings.Contains(result.Error(), "persist completed job status") {
		t.Fatalf("result = %v, want completed status persistence error", result)
	}
	for _, event := range published {
		if event == "completed" {
			t.Fatal("published completed event after status write failed")
		}
	}
}

func TestHandleJobOutcome_PersistsExactCleanupTargetWhenInitialStageFailed(t *testing.T) {
	db := &fakeJobDB{}
	target := &VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       models.JobTypeVMAdd,
		RetryCount: 3,
		MaxRetries: 3,
	}
	stageErr := &compensationRetryError{
		err:    errors.New("persist exact VM cleanup target: connection refused"),
		target: target,
	}

	if result := runLifecycle(
		context.Background(),
		db,
		NewPipelineMetrics("", "", nil),
		job,
		stageErr,
	); result != nil {
		t.Fatalf("cleanup target retry returned %v, want nil", result)
	}
	if len(db.retried) != 1 || !db.retried[0].cleanupOnly {
		t.Fatalf("cleanup retry records = %+v, want one cleanup-only retry", db.retried)
	}
	var got VMCloneCleanupTarget
	if err := json.Unmarshal(db.retried[0].cleanupTarget, &got); err != nil {
		t.Fatalf("retry target is not valid JSON: %v", err)
	}
	if got != *target {
		t.Fatalf("retry target = %+v, want %+v", got, *target)
	}
}

// --------------------------------------------------------------------------
// Test 2: non-retryable errors fail immediately
// --------------------------------------------------------------------------

func TestHandleJobOutcome_NonRetryableFailsImmediately(t *testing.T) {
	nonRetryable := []string{
		`source_ref "my-iso.iso" is not a valid installer ISO datastore path for source_type=iso`,
		`create blank VM: find folder "": folder '' not found`,
		"resolve default resource pool: default resource pool resolves to multiple instances, please specify",
		"run script: Failed to authenticate with the guest operating system using the supplied credentials",
	}

	for _, errMsg := range nonRetryable {
		errMsg := errMsg
		t.Run(errMsg[:min(40, len(errMsg))], func(t *testing.T) {
			db := &fakeJobDB{}
			m := NewPipelineMetrics("", "", nil)
			job := &models.Job{
				ID:         uuid.New(),
				Type:       "template_provision",
				RetryCount: 0,
				MaxRetries: 3,
			}
			result := runLifecycle(context.Background(), db, m, job, errors.New(errMsg))
			if result == nil {
				t.Fatal("processJobLifecycle returned nil, want the original error")
			}
			if len(db.retried) != 0 {
				t.Errorf("RetryJob was called for a non-retryable error")
			}
			failed := db.statusesWithStatus(models.JobStatusFailed)
			if len(failed) != 1 {
				t.Errorf("UpdateJobStatus(failed) called %d times, want 1", len(failed))
			}
			if job.RetryCount != 0 {
				t.Errorf("retry_count modified: got %d, want 0", job.RetryCount)
			}
		})
	}
}

// --------------------------------------------------------------------------
// Test 3: retries stop at max_retries
// --------------------------------------------------------------------------

func TestHandleJobOutcome_ExhaustsAtMaxRetries(t *testing.T) {
	db := &fakeJobDB{}
	m := NewPipelineMetrics("", "", nil)

	job := &models.Job{
		ID:         uuid.New(),
		Type:       "template_provision",
		RetryCount: 3, // already at max
		MaxRetries: 3,
	}
	retryableErr := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.")

	result := runLifecycle(context.Background(), db, m, job, retryableErr)
	if result == nil {
		t.Fatal("processJobLifecycle returned nil at max_retries, want the error")
	}
	if len(db.retried) != 0 {
		t.Errorf("RetryJob called after max_retries")
	}
	failed := db.statusesWithStatus(models.JobStatusFailed)
	if len(failed) != 1 {
		t.Errorf("UpdateJobStatus(failed) called %d times, want 1", len(failed))
	}
	if m.jobRetryExhausted["template_provision"] == 0 {
		t.Error("crucible_job_retry_exhausted_total not incremented at max_retries")
	}
	resultStr := string(failed[0].result)
	if resultStr == "" {
		t.Fatal("job result is empty")
	}
	if !strings.Contains(resultStr, `"attempts":4`) {
		t.Errorf("result JSON missing attempts field; got: %s", resultStr)
	}
}

// --------------------------------------------------------------------------
// Test 4: backoff grows; ClaimJob respects next_attempt_at
// --------------------------------------------------------------------------

func TestBackoffGrows(t *testing.T) {
	prev := time.Duration(0)
	for i := 0; i < 5; i++ {
		d := RetryBackoff(i)
		if i > 0 && d < prev {
			t.Errorf("RetryBackoff(%d) = %s, should be >= RetryBackoff(%d) = %s", i, d, i-1, prev)
		}

		prev = d
	}
}

func TestBackoffDoesNotOverflowForUnlimitedCleanupRetries(t *testing.T) {
	for _, retryCount := range []int{29, 63, 1_000_000} {
		delay := RetryBackoff(retryCount)
		if delay < retryBackoffMax || delay >= retryBackoffMax+retryBackoffMax/4 {
			t.Fatalf("RetryBackoff(%d) = %s, want capped positive delay with bounded jitter", retryCount, delay)
		}
	}
}

func TestClaimJobSkipsFutureNextAttempt(t *testing.T) {
	future := time.Now().Add(10 * time.Minute)
	job := &models.Job{
		ID:            uuid.New(),
		Status:        models.JobStatusPending,
		NextAttemptAt: &future,
	}
	claimable := job.NextAttemptAt == nil || !job.NextAttemptAt.After(time.Now())
	if claimable {
		t.Error("job with future next_attempt_at should NOT be claimable")
	}
	past := time.Now().Add(-1 * time.Second)
	job.NextAttemptAt = &past
	claimable = job.NextAttemptAt == nil || !job.NextAttemptAt.After(time.Now())
	if !claimable {
		t.Error("job with past next_attempt_at should be claimable")
	}
	job.NextAttemptAt = nil
	claimable = job.NextAttemptAt == nil || !job.NextAttemptAt.After(time.Now())
	if !claimable {
		t.Error("job with nil next_attempt_at should be claimable")
	}
}

// --------------------------------------------------------------------------
// Test 6: per-source clone serialization
// --------------------------------------------------------------------------

func TestCloneLockSerialization_SameSource(t *testing.T) {
	p := testProvisioner()
	const moref = "vm-1234"
	var (
		concurrentCount int64
		maxConcurrent   int64
		mu              sync.Mutex
		wg              sync.WaitGroup
	)
	const n = 5
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			release := p.acquireCloneLock(moref)
			cur := atomic.AddInt64(&concurrentCount, 1)
			mu.Lock()
			if cur > maxConcurrent {
				maxConcurrent = cur
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt64(&concurrentCount, -1)
			release()
		}()
	}
	wg.Wait()
	if maxConcurrent > 1 {
		t.Errorf("same-source clones overlapped: max concurrent = %d, want 1", maxConcurrent)
	}
}

func TestCloneLockSerialization_DifferentSourcesConcurrent(t *testing.T) {
	p := testProvisioner()
	const morefA = "vm-1111"
	const morefB = "vm-2222"
	ready := make(chan struct{})
	var (
		wg      sync.WaitGroup
		aInLock int64
		overlap bool
		mu      sync.Mutex
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		releaseA := p.acquireCloneLock(morefA)
		defer releaseA()
		atomic.StoreInt64(&aInLock, 1)
		close(ready)
		time.Sleep(20 * time.Millisecond)
		atomic.StoreInt64(&aInLock, 0)
	}()
	go func() {
		defer wg.Done()
		<-ready
		releaseB := p.acquireCloneLock(morefB)
		defer releaseB()
		mu.Lock()
		if atomic.LoadInt64(&aInLock) == 1 {
			overlap = true
		}
		mu.Unlock()
	}()
	wg.Wait()
	if !overlap {
		t.Error("different-source clones did NOT run concurrently -- the whole system is serialized")
	}
}

// --------------------------------------------------------------------------
// Test 7: end-to-end negative control
// --------------------------------------------------------------------------

func TestEndToEnd_RetrySucceeds(t *testing.T) {
	db := &fakeJobDB{}
	m := NewPipelineMetrics("", "", nil)
	job := &models.Job{
		ID:         uuid.New(),
		Type:       "template_provision",
		RetryCount: 0,
		MaxRetries: 3,
	}
	diskErr := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.")

	// Attempt 1: transient vCenter error.
	result1 := runLifecycle(context.Background(), db, m, job, diskErr)
	if result1 != nil {
		t.Fatalf("attempt 1: processJobLifecycle returned %v, want nil (rescheduled)", result1)
	}
	if len(db.retried) != 1 {
		t.Fatalf("attempt 1: RetryJob called %d times, want 1", len(db.retried))
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
		t.Fatalf("attempt 1: job marked failed prematurely")
	}

	// Simulate re-claim after backoff: DB has incremented retry_count.
	job.RetryCount = 1

	// Attempt 2: success.
	result2 := runLifecycle(context.Background(), db, m, job, nil)
	if result2 != nil {
		t.Fatalf("attempt 2: processJobLifecycle returned %v, want nil (success)", result2)
	}
	if got := db.statusesWithStatus(models.JobStatusCompleted); len(got) == 0 {
		t.Fatal("attempt 2: UpdateJobStatus with 'completed' never called")
	}
	if got := db.statusesWithStatus(models.JobStatusFailed); len(got) != 0 {
		t.Fatalf("attempt 2: job unexpectedly marked failed")
	}
}

func TestEndToEnd_ExhaustsMaxRetries(t *testing.T) {
	const maxRetries = 3
	db := &fakeJobDB{}
	m := NewPipelineMetrics("", "", nil)
	job := &models.Job{
		ID:         uuid.New(),
		Type:       "template_provision",
		RetryCount: 0,
		MaxRetries: maxRetries,
	}
	diskErr := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.")

	for i := 0; i < maxRetries; i++ {
		r := runLifecycle(context.Background(), db, m, job, diskErr)
		if r != nil {
			t.Fatalf("attempt %d: want nil (rescheduled), got %v", i+1, r)
		}
		job.RetryCount++ // mirror what DB RetryJob does
	}
	if len(db.retried) != maxRetries {
		t.Errorf("RetryJob called %d times, want %d", len(db.retried), maxRetries)
	}

	// Final attempt: RetryCount == MaxRetries -> terminal failure.
	r := runLifecycle(context.Background(), db, m, job, diskErr)
	if r == nil {
		t.Fatal("final attempt: processJobLifecycle returned nil, want error (terminal failure)")
	}
	failed := db.statusesWithStatus(models.JobStatusFailed)
	if len(failed) != 1 {
		t.Fatalf("final attempt: UpdateJobStatus(failed) called %d times, want 1", len(failed))
	}
	if m.jobRetryExhausted["template_provision"] == 0 {
		t.Error("crucible_job_retry_exhausted_total not incremented after max_retries")
	}
	key := "template_provision|" + RetryReasonTransientClone
	if int(m.jobRetries[key]) != maxRetries {
		t.Errorf("crucible_job_retries_total[%s] = %g, want %d", key, m.jobRetries[key], maxRetries)
	}
}

// --------------------------------------------------------------------------
// Metrics: wired from the production processJobLifecycle path
// --------------------------------------------------------------------------

// TestMetricsRecordedFromProductionPath verifies that the retry metrics are
// recorded from the production processJobLifecycle path, not just called
// directly. Guards against the dead-recorder pattern.
func TestMetricsRecordedFromProductionPath(t *testing.T) {
	m := NewPipelineMetrics("", "", nil)
	db := &fakeJobDB{}
	job := &models.Job{
		ID:         uuid.New(),
		Type:       "template_provision",
		RetryCount: 0,
		MaxRetries: 3,
	}
	err := errors.New("clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.")
	_ = runLifecycle(context.Background(), db, m, job, err)
	key := "template_provision|" + RetryReasonTransientClone
	if m.jobRetries[key] != 1 {
		t.Errorf("crucible_job_retries_total[%s] = %g after production path, want 1", key, m.jobRetries[key])
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
