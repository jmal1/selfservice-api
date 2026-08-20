package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

type confirmationCASRaceDB struct {
	*fakeHealthDB
	once sync.Once
}

func (db *confirmationCASRaceDB) CompareAndSwapTemplateHealthState(
	ctx context.Context,
	state database.TemplateHealthState,
) (bool, error) {
	injected := false
	db.once.Do(func() {
		db.mu.Lock()
		defer db.mu.Unlock()
		current := db.states[state.TemplateID]
		current.DeepConsecutiveFailures = 2
		current.PendingDeepFailureAt = nil
		current.DeepConfirmationDueAt = nil
		current.HealthStatus = "unhealthy"
		current.UpdatedAt = current.UpdatedAt.Add(time.Microsecond)
		injected = true
	})
	if injected {
		return false, nil
	}
	return db.fakeHealthDB.CompareAndSwapTemplateHealthState(ctx, state)
}

func confirmationJob(t *testing.T, payload TemplateHealthConfirmationPayload) *models.Job {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &models.Job{
		Type:       models.JobTypeTemplateHealthConfirm,
		Payload:    raw,
		MaxRetries: 3,
	}
}

// TestDeepFailureRequiresIndependentConfirmation is the primary production
// incident guard. Sabotage: changing the first-failure branch to set
// health_status=unhealthy makes this fail immediately.
func TestDeepFailureRequiresIndependentConfirmation(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	metrics := newFakeHealthMetrics()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), defaultCfg()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	state := db.states[tmpl.ID]
	if state.HealthStatus == "unhealthy" {
		t.Fatal("one failed deep cycle paged confirmed unhealthy state")
	}
	if state.DeepConsecutiveFailures != 1 || state.PendingDeepFailureAt == nil {
		t.Fatalf("first deep failure state = %+v, want one pending failure", state)
	}
	if state.PendingDeepFailureAt.Nanosecond()%1_000 != 0 {
		t.Fatalf("pending failure token %s exceeds PostgreSQL microsecond precision", state.PendingDeepFailureAt)
	}
	if db.confirmationEnqueues != 1 {
		t.Fatalf("confirmation enqueues = %d, want 1", db.confirmationEnqueues)
	}
	if got := metrics.snapshots[len(metrics.snapshots)-1].States[0].HealthStatus; got == "unhealthy" {
		t.Fatal("persisted snapshot exposed raw deep failure as confirmed unhealthy")
	}
}

func TestDeepCheckerOutageDoesNotMutateDeepState(t *testing.T) {
	tmpl := makeTemplate("66666666-6666-6666-6666-666666666666", "vm-606")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New("clone failed: connection refused: dial tcp vcenter:443")
	metrics := newFakeHealthMetrics()

	counts, err := reconcileTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), defaultCfg())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if counts.CheckerUp {
		t.Fatal("mid-cycle vCenter outage reported checker_up=true")
	}
	state := db.states[tmpl.ID]
	if state.LastDeepCheckAt != nil || state.DeepConsecutiveFailures != 0 ||
		state.PendingDeepFailureAt != nil {
		t.Fatalf("checker-level deep outage mutated template deep state: %+v", state)
	}
	if got := classifyTemplateHealthFault(vc.cloneErr); got != "vsphere_connectivity" {
		t.Fatalf("connectivity fault class = %q, want vsphere_connectivity", got)
	}
}

// Sabotage: replacing the compare-and-swap write with the former unconditional
// upsert restores the stale pending token and erases the confirmed unhealthy
// state injected between read and write.
func TestStructuralWriteCannotUndoConcurrentDeepConfirmation(t *testing.T) {
	tmpl := makeTemplate("88888888-8888-8888-8888-888888888888", "vm-808")
	base := newFakeHealthDB([]models.Template{tmpl})
	pendingAt := time.Now().UTC().Truncate(time.Microsecond)
	base.states[tmpl.ID] = &database.TemplateHealthState{
		TemplateID:                    tmpl.ID,
		TemplateName:                  tmpl.Name,
		HealthStatus:                  "healthy",
		StructuralConsecutiveFailures: 0,
		DeepConsecutiveFailures:       1,
		PendingDeepFailureAt:          &pendingAt,
		DeepConfirmationDueAt:         &pendingAt,
		UpdatedAt:                     pendingAt,
	}
	db := &confirmationCASRaceDB{fakeHealthDB: base}
	checkedAt := pendingAt.Add(time.Minute)

	_, err := mutateTemplateHealthState(context.Background(), db, tmpl.ID, tmpl.Name,
		func(state *database.TemplateHealthState) {
			state.LastStructuralCheckAt = &checkedAt
			state.LastStructuralPassed = boolPtr(true)
			state.StructuralConsecutiveFailures = 0
		})
	if err != nil {
		t.Fatalf("mutate structural state: %v", err)
	}

	state := base.states[tmpl.ID]
	if state.HealthStatus != "unhealthy" || state.DeepConsecutiveFailures != 2 {
		t.Fatalf("structural write erased concurrent confirmation: %+v", state)
	}
	if state.PendingDeepFailureAt != nil || state.DeepConfirmationDueAt != nil {
		t.Fatal("structural write restored stale pending confirmation state")
	}
}

func TestHealthPersistenceFailureCannotPublishCheckerSuccess(t *testing.T) {
	tmpl := makeTemplate("99999999-9999-9999-9999-999999999999", "vm-909")
	db := newFakeHealthDB([]models.Template{tmpl})
	db.casErr = errors.New("database write unavailable")
	metrics := newFakeHealthMetrics()

	counts, err := reconcileTemplateHealth(context.Background(), db, newFakeHealthVC(), metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), defaultCfg())
	if err == nil {
		t.Fatal("persistence failure was reported as a successful cycle")
	}
	if counts.CheckerUp {
		t.Fatal("persistence failure returned checker_up=true")
	}
	checkerUp := metrics.snapshots[len(metrics.snapshots)-1].CheckerUp
	if checkerUp == nil || *checkerUp {
		t.Fatalf("persistence failure snapshot checker_up = %v, want false", checkerUp)
	}
	if db.cycleCompletedAt != nil {
		t.Fatal("failed cycle advanced the durable completion marker")
	}
}

func TestMidCycleStructuralOutageDoesNotMutateTemplateState(t *testing.T) {
	tmpl := makeTemplate("cccccccc-cccc-cccc-cccc-cccccccccccc", "vm-1212")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	outage := errors.New("connection refused")
	vc.transientErrors[tmpl.VCenterRef()] = []error{nil, outage, outage, outage}
	metrics := newFakeHealthMetrics()

	counts, err := reconcileTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), defaultCfg())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.CheckerUp {
		t.Fatal("mid-cycle structural outage returned checker_up=true")
	}
	if state := db.states[tmpl.ID]; state != nil {
		t.Fatalf("mid-cycle structural outage mutated template state: %+v", state)
	}
}

func TestStartupReplacementSnapshotAlwaysIncludesCheckerUp(t *testing.T) {
	tmpl := makeTemplate("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "vm-1010")
	db := newFakeHealthDB([]models.Template{tmpl})
	metrics := newFakeHealthMetrics()
	vc := newFakeHealthVC()

	if err := replaceTemplateHealthSnapshot(context.Background(), db, vc, metrics, nil, defaultCfg()); err != nil {
		t.Fatalf("replace startup snapshot: %v", err)
	}
	checkerUp := metrics.snapshots[len(metrics.snapshots)-1].CheckerUp
	if checkerUp == nil || !*checkerUp {
		t.Fatalf("reachable startup probe checker_up = %v, want true", checkerUp)
	}

	vc.vmExists[tmpl.VCenterRef()] = errors.New("connection refused")
	if err := replaceTemplateHealthSnapshot(context.Background(), db, vc, metrics, nil, defaultCfg()); err != nil {
		t.Fatalf("replace unavailable startup snapshot: %v", err)
	}
	checkerUp = metrics.snapshots[len(metrics.snapshots)-1].CheckerUp
	if checkerUp == nil || *checkerUp {
		t.Fatalf("unreachable startup probe checker_up = %v, want false", checkerUp)
	}
}

func TestStaleConfirmationRetryRepublishesCommittedState(t *testing.T) {
	tmpl := makeTemplate("dddddddd-dddd-dddd-dddd-dddddddddddd", "vm-1313")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	payload := db.confirmationJobs[tmpl.ID]
	job := confirmationJob(t, payload)
	metrics := newFakeHealthMetrics()
	metrics.err = errors.New("pushgateway unavailable")
	if err := confirmTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, job); err == nil {
		t.Fatal("confirmation publication failure did not request a job retry")
	}
	if db.states[tmpl.ID].HealthStatus != "unhealthy" {
		t.Fatal("precondition: confirmation state was not committed before publication failed")
	}

	metrics.err = nil
	if err := confirmTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, job); err != nil {
		t.Fatalf("stale confirmation retry: %v", err)
	}
	last := metrics.snapshots[len(metrics.snapshots)-1]
	if len(last.States) != 1 || last.States[0].HealthStatus != "unhealthy" {
		t.Fatalf("stale retry did not republish committed state: %+v", last.States)
	}
}

func TestConfirmationCheckerOutagePublishesCheckerDown(t *testing.T) {
	tmpl := makeTemplate("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "vm-1414")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	payload := db.confirmationJobs[tmpl.ID]
	vc.cloneErr = errors.New("connection refused")
	metrics := newFakeHealthMetrics()
	if err := confirmTemplateHealth(context.Background(), db, vc, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err == nil {
		t.Fatal("confirmation checker outage was not retried")
	}
	checkerUp := metrics.snapshots[len(metrics.snapshots)-1].CheckerUp
	if checkerUp == nil || *checkerUp {
		t.Fatalf("confirmation outage checker_up = %v, want false", checkerUp)
	}
	if db.states[tmpl.ID].PendingDeepFailureAt == nil {
		t.Fatal("checker outage consumed pending confirmation state")
	}
}

func TestDeepCheckCloneNamesAreAttemptUnique(t *testing.T) {
	tmpl := makeTemplate("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "vm-1111")
	vc := newFakeHealthVC()

	for i := 0; i < 2; i++ {
		if err := runDeepCheck(context.Background(), vc, &tmpl, defaultCfg(),
			slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
			t.Fatalf("deep check %d: %v", i, err)
		}
	}
	if len(vc.cloneNames) != 2 || vc.cloneNames[0] == vc.cloneNames[1] {
		t.Fatalf("clone names = %v, want two unique attempt identities", vc.cloneNames)
	}
}

func TestDeepGuestTimeoutRemainsTemplateFailure(t *testing.T) {
	tmpl := makeTemplate("88888888-8888-8888-8888-888888888888", "vm-808")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.waitForIPErr = errors.New("context deadline exceeded")

	counts, err := reconcileTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), defaultCfg())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !counts.CheckerUp {
		t.Fatal("guest wait-for-IP timeout was misclassified as a checker outage")
	}
	state := db.states[tmpl.ID]
	if state.DeepConsecutiveFailures != 1 || state.PendingDeepFailureAt == nil {
		t.Fatalf("guest timeout did not schedule independent confirmation: %+v", state)
	}
	if state.LastDeepFaultClass == nil || *state.LastDeepFaultClass != "guest_ip_timeout" {
		t.Fatalf("guest timeout fault class = %v, want guest_ip_timeout", state.LastDeepFaultClass)
	}
}

// TestIndependentDeepConfirmationFailureMarksUnhealthy proves the scheduled job
// path, not a second call to the 12-hour reconciler, advances the policy.
// Sabotage: making confirmation failure a no-op leaves the status healthy.
func TestIndependentDeepConfirmationFailureMarksUnhealthy(t *testing.T) {
	tmpl := makeTemplate("22222222-2222-2222-2222-222222222222", "vm-202")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	initialCloneCalls := vc.cloneCalls
	payload := db.confirmationJobs[tmpl.ID]
	if err := confirmTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err != nil {
		t.Fatalf("confirmation: %v", err)
	}

	state := db.states[tmpl.ID]
	if state.HealthStatus != "unhealthy" || state.DeepConsecutiveFailures < 2 {
		t.Fatalf("confirmation state = %+v, want confirmed unhealthy", state)
	}
	if state.PendingDeepFailureAt != nil {
		t.Fatal("confirmed failure left pending state behind")
	}
	if state.LastDeepFaultClass == nil || *state.LastDeepFaultClass != "vsphere_virtual_disk_corrupt_or_unsupported" {
		t.Fatalf("fault class = %v, want vsphere_virtual_disk_corrupt_or_unsupported", state.LastDeepFaultClass)
	}
	if vc.cloneCalls-initialCloneCalls != cfg.MaxRetries {
		t.Fatalf("confirmation used %d fresh clone attempts, want %d", vc.cloneCalls-initialCloneCalls, cfg.MaxRetries)
	}
}

// TestPassingDeepCycleClearsConfirmedDeepFailure proves recovery from the
// confirmed state, not only from a pending first failure.
func TestPassingDeepCycleClearsConfirmedDeepFailure(t *testing.T) {
	tmpl := makeTemplate("55555555-5555-5555-5555-555555555555", "vm-505")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	payload := db.confirmationJobs[tmpl.ID]
	if err := confirmTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err != nil {
		t.Fatalf("confirmation: %v", err)
	}
	if db.states[tmpl.ID].HealthStatus != "unhealthy" {
		t.Fatal("precondition: independent failure did not confirm unhealthy")
	}

	vc.cloneErr = nil
	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	state := db.states[tmpl.ID]
	if state.HealthStatus != "healthy" || state.DeepConsecutiveFailures != 0 ||
		state.PendingDeepFailureAt != nil || state.LastDeepError != nil {
		t.Fatalf("passing deep cycle did not clear confirmed failure: %+v", state)
	}
}

// TestIndependentDeepConfirmationSuccessClearsPendingFailure guards recovery.
// Sabotage: retaining the pending token causes both this assertion and the
// idempotent scheduler assertion to fail.
func TestIndependentDeepConfirmationSuccessClearsPendingFailure(t *testing.T) {
	tmpl := makeTemplate("33333333-3333-3333-3333-333333333333", "vm-303")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	payload := db.confirmationJobs[tmpl.ID]
	vc.cloneErr = nil
	if err := confirmTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err != nil {
		t.Fatalf("confirmation: %v", err)
	}

	state := db.states[tmpl.ID]
	if state.HealthStatus != "healthy" || state.DeepConsecutiveFailures != 0 {
		t.Fatalf("recovered state = %+v, want healthy with zero deep failures", state)
	}
	if state.PendingDeepFailureAt != nil || state.DeepConfirmationDueAt != nil {
		t.Fatal("successful confirmation did not clear pending failure schedule")
	}
}

func TestHiddenConfirmationTargetClearsPendingSchedule(t *testing.T) {
	tmpl := makeTemplate("77777777-7777-7777-7777-777777777777", "vm-707")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	payload := db.confirmationJobs[tmpl.ID]
	db.templates[0].Visibility = "instructor_only"
	if err := confirmTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err != nil {
		t.Fatalf("confirmation: %v", err)
	}

	state := db.states[tmpl.ID]
	if state.PendingDeepFailureAt != nil || state.DeepConfirmationDueAt != nil {
		t.Fatal("hidden target left an unconfirmable pending schedule")
	}
	if state.HealthStatus == "unhealthy" {
		t.Fatal("visibility change invented a confirmed failure")
	}
}

func TestLaterDeepSuccessClearsConfirmedFailure(t *testing.T) {
	tmpl := makeTemplate("55555555-5555-5555-5555-555555555555", "vm-505")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)
	cfg := defaultCfg()

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	payload := db.confirmationJobs[tmpl.ID]
	if err := confirmTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg, confirmationJob(t, payload)); err != nil {
		t.Fatalf("confirmation: %v", err)
	}
	if db.states[tmpl.ID].HealthStatus != "unhealthy" {
		t.Fatal("precondition: confirmation did not mark template unhealthy")
	}

	vc.cloneErr = nil
	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, nil, cfg); err != nil {
		t.Fatalf("recovery reconcile: %v", err)
	}
	state := db.states[tmpl.ID]
	if state.HealthStatus != "healthy" || state.DeepConsecutiveFailures != 0 {
		t.Fatalf("later deep success did not clear confirmed state: %+v", state)
	}
}

// TestConfirmationScheduleRepairIsIdempotent exercises the production repair
// function under the startup/ticker/failover race shape. The fake's locked
// Create method models the database method's advisory-lock critical section.
func TestConfirmationScheduleRepairIsIdempotent(t *testing.T) {
	tmpl := makeTemplate("44444444-4444-4444-4444-444444444444", "vm-404")
	db := newFakeHealthDB([]models.Template{tmpl})
	pendingAt := time.Now()
	dueAt := pendingAt.Add(5 * time.Minute)
	db.states[tmpl.ID] = &database.TemplateHealthState{
		TemplateID:              tmpl.ID,
		TemplateName:            tmpl.Name,
		HealthStatus:            "healthy",
		DeepConsecutiveFailures: 1,
		PendingDeepFailureAt:    &pendingAt,
		DeepConfirmationDueAt:   &dueAt,
	}

	const racers = 8
	errs := make(chan error, racers)
	for i := 0; i < racers; i++ {
		go func() {
			_, err := reconcileTemplateHealthConfirmations(context.Background(), db, defaultCfg(), nil)
			errs <- err
		}()
	}
	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if db.confirmationEnqueues != 1 {
		t.Fatalf("racing schedule repairs enqueued %d jobs, want exactly 1", db.confirmationEnqueues)
	}
}
