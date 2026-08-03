package provisioner

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// ---------- fakes ----------

// fakeIdleVC is a test double for idleEvalVCenter.
type fakeIdleVC struct {
	// suspendedMorefs records every SuspendVM call (moref → count).
	suspendedMorefs map[string]int
	// toolsRunning is returned by QueryVMToolsStatusBulk (moref → running).
	toolsRunning map[string]bool
	// perfSamples is returned by SamplePodVMPerf (moref → sample).
	perfSamples map[string]vcenter.VMPerfSample
	// suspendErr, if set, SuspendVM returns this.
	suspendErr error
}

func newFakeVC() *fakeIdleVC {
	return &fakeIdleVC{
		suspendedMorefs: map[string]int{},
		toolsRunning:    map[string]bool{},
		perfSamples:     map[string]vcenter.VMPerfSample{},
	}
}

func (f *fakeIdleVC) SuspendVM(_ context.Context, moref string) error {
	if f.suspendErr != nil {
		return f.suspendErr
	}
	f.suspendedMorefs[moref]++
	return nil
}

func (f *fakeIdleVC) SamplePodVMPerf(_ context.Context, _ []string) (map[string]vcenter.VMPerfSample, error) {
	return f.perfSamples, nil
}

func (f *fakeIdleVC) QueryVMToolsStatusBulk(_ context.Context, _ []string) (map[string]bool, error) {
	return f.toolsRunning, nil
}

// fakeIdleDB is a test double for idleEvalDB.
type fakeIdleDB struct {
	// candidates is returned by ListRunningPodVMsForIdleEval.
	candidates []database.IdleSuspendCandidate
	// suspended records SetVMSuspended calls (vmID → reason).
	suspended map[uuid.UUID]string
	// activityTouched records TouchVMActivityAt calls (vmID → time).
	activityTouched map[uuid.UUID]time.Time
	// activeJob controls HasActiveJobForVM (vmID → result).
	activeJob map[uuid.UUID]bool
	// inFlightRun controls HasInFlightRunForVM (vmID → result).
	inFlightRun map[uuid.UUID]bool
	// idleTimeoutSeconds is returned by GetIdleTimeoutSeconds.
	idleTimeoutSeconds int
	// podIdleOverride overrides per pod: podID → timeoutSeconds.
	podIdleOverride map[uuid.UUID]int
}

func newFakeDB() *fakeIdleDB {
	return &fakeIdleDB{
		suspended:          map[uuid.UUID]string{},
		activityTouched:    map[uuid.UUID]time.Time{},
		activeJob:          map[uuid.UUID]bool{},
		inFlightRun:        map[uuid.UUID]bool{},
		idleTimeoutSeconds: 21600, // 6h default
		podIdleOverride:    map[uuid.UUID]int{},
	}
}

func (f *fakeIdleDB) ListRunningPodVMsForIdleEval(_ context.Context) ([]database.IdleSuspendCandidate, error) {
	return f.candidates, nil
}

func (f *fakeIdleDB) TouchVMActivityAt(_ context.Context, id uuid.UUID, t time.Time) error {
	f.activityTouched[id] = t
	return nil
}

func (f *fakeIdleDB) SetVMSuspended(_ context.Context, id uuid.UUID, _ time.Time, reason string) error {
	f.suspended[id] = reason
	return nil
}

func (f *fakeIdleDB) GetIdleTimeoutSeconds(_ context.Context, podID uuid.UUID) (int, error) {
	if v, ok := f.podIdleOverride[podID]; ok {
		return v, nil
	}
	return f.idleTimeoutSeconds, nil
}

func (f *fakeIdleDB) HasActiveJobForVM(_ context.Context, podVMID uuid.UUID) (bool, error) {
	return f.activeJob[podVMID], nil
}

func (f *fakeIdleDB) HasInFlightRunForVM(_ context.Context, podVMID uuid.UUID) (bool, error) {
	return f.inFlightRun[podVMID], nil
}

// ---------- helpers ----------

// pastTime returns a time that is 'ago' before now.
func pastTime(ago time.Duration) *time.Time {
	t := time.Now().Add(-ago)
	return &t
}

// makeCandidate builds an IdleSuspendCandidate with reasonable defaults.
func makeCandidate(moref string, lastActivity *time.Time) database.IdleSuspendCandidate {
	vmID := uuid.New()
	podID := uuid.New()
	return database.IdleSuspendCandidate{
		PodVMID:        vmID,
		PodID:          podID,
		PodStatus:      models.PodStatusActive,
		VCenterVMID:    moref,
		DisplayName:    "test-vm",
		LastActivityAt: lastActivity,
	}
}

// idleSample returns a VMPerfSample below the idle thresholds.
func idleSample() vcenter.VMPerfSample {
	return vcenter.VMPerfSample{CPUUsage: 50, NetUsage: 2, Valid: true} // 0.5 % CPU, 2 KBps
}

// activeSample returns a VMPerfSample above the idle CPU threshold.
func activeSample() vcenter.VMPerfSample {
	return vcenter.VMPerfSample{CPUUsage: 2000, NetUsage: 500, Valid: true} // 20 % CPU, 500 KBps
}

// ---------- tests ----------

// TestRefusalGuard1_ActiveJob verifies that a VM with a job currently in
// flight is never suspended, even when both idle signals are satisfied.
func TestRefusalGuard1_ActiveJob(t *testing.T) {
	const moref = "vm-guard1"
	vmID := uuid.New()
	podID := uuid.New()

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{{
		PodVMID:        vmID,
		PodID:          podID,
		PodStatus:      models.PodStatusActive,
		VCenterVMID:    moref,
		DisplayName:    "guard1-vm",
		LastActivityAt: pastTime(8 * time.Hour), // well past 6h threshold
	}}
	db.activeJob[vmID] = true // job in flight

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	_, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM should NOT be suspended when a job is in flight; got suspensions: %v", vc.suspendedMorefs)
	}
	if _, wasSuspended := db.suspended[vmID]; wasSuspended {
		t.Error("DB SetVMSuspended called despite active job guard")
	}
}

// TestRefusalGuard2_PodNotActive verifies that a VM in a pod that is
// provisioning or destroying is never suspended.
func TestRefusalGuard2_PodNotActive(t *testing.T) {
	for _, podStatus := range []string{models.PodStatusProvisioning, models.PodStatusDestroying} {
		t.Run(podStatus, func(t *testing.T) {
			const moref = "vm-guard2"
			vmID := uuid.New()
			podID := uuid.New()

			db := newFakeDB()
			db.candidates = []database.IdleSuspendCandidate{{
				PodVMID:        vmID,
				PodID:          podID,
				PodStatus:      podStatus,
				VCenterVMID:    moref,
				DisplayName:    "guard2-vm",
				LastActivityAt: pastTime(8 * time.Hour),
			}}

			vc := newFakeVC()
			vc.toolsRunning[moref] = true
			vc.perfSamples[moref] = idleSample()

			_, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(vc.suspendedMorefs) != 0 {
				t.Errorf("pod status %q: VM should NOT be suspended; got suspensions: %v", podStatus, vc.suspendedMorefs)
			}
		})
	}
}

// TestRefusalGuard3_AssessmentRunInFlight verifies that a VM with an active
// assessment run is never suspended.
func TestRefusalGuard3_AssessmentRunInFlight(t *testing.T) {
	const moref = "vm-guard3"
	vmID := uuid.New()
	podID := uuid.New()

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{{
		PodVMID:        vmID,
		PodID:          podID,
		PodStatus:      models.PodStatusActive,
		VCenterVMID:    moref,
		DisplayName:    "guard3-vm",
		LastActivityAt: pastTime(8 * time.Hour),
	}}
	db.inFlightRun[vmID] = true // assessment run in flight

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	_, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM should NOT be suspended with an in-flight assessment run; got: %v", vc.suspendedMorefs)
	}
}

// TestRefusalGuard4_ToolsNotRunning verifies that a VM with VMware Tools
// not running is never suspended. Absent signal must not be read as idle.
func TestRefusalGuard4_ToolsNotRunning(t *testing.T) {
	const moref = "vm-guard4"
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = false // Tools not running
	vc.perfSamples[moref] = idleSample()

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM should NOT be suspended when Tools is not running; got: %v", vc.suspendedMorefs)
	}
	if counts.ToolsMissing != 1 {
		t.Errorf("expected ToolsMissing=1, got %d", counts.ToolsMissing)
	}
}

// TestRefusalGuard4_PerfDataMissing verifies that a VM with no perf data
// returned from vCenter is never suspended.
func TestRefusalGuard4_PerfDataMissing(t *testing.T) {
	const moref = "vm-guard4b"
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	// perfSamples is empty — no data for this VM

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM should NOT be suspended when perf data is missing; got: %v", vc.suspendedMorefs)
	}
	if counts.PerfMissing != 1 {
		t.Errorf("expected PerfMissing=1, got %d", counts.PerfMissing)
	}
}

// TestIdleRequiresBothSignals_RecentConsoleNotIdle verifies that a VM with
// a recent console session (last_activity_at < threshold ago) is NOT idle,
// even when CPU/net is low.
func TestIdleRequiresBothSignals_RecentConsoleNotIdle(t *testing.T) {
	const moref = "vm-both1"
	// last_activity_at only 30 minutes ago — well within 6h threshold.
	c := makeCandidate(moref, pastTime(30*time.Minute))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample() // CPU/net are low

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM with recent console session should NOT be suspended; got: %v", vc.suspendedMorefs)
	}
	if counts.ActivityFresh != 1 {
		t.Errorf("expected ActivityFresh=1, got %d", counts.ActivityFresh)
	}
}

// TestIdleRequiresBothSignals_HighCPUNotIdle verifies that a VM with high
// CPU utilisation is NOT idle, even when there has been no console session.
// The evaluator should update last_activity_at when it sees high utilisation.
func TestIdleRequiresBothSignals_HighCPUNotIdle(t *testing.T) {
	const moref = "vm-both2"
	// last_activity_at 8 hours ago — normally past threshold.
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = activeSample() // HIGH CPU

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("VM with high CPU should NOT be suspended; got: %v", vc.suspendedMorefs)
	}
	// The evaluator must have refreshed last_activity_at.
	if _, touched := db.activityTouched[c.PodVMID]; !touched {
		t.Error("expected TouchVMActivityAt to be called for high-CPU VM")
	}
	if counts.ActivityFresh != 1 {
		t.Errorf("expected ActivityFresh=1, got %d", counts.ActivityFresh)
	}
}

// TestIdleRequiresBothSignals_BothIdleFor6h verifies that a VM qualifies for
// suspension only when BOTH signals indicate idle for >= 6 hours.
func TestIdleRequiresBothSignals_BothIdleFor6h(t *testing.T) {
	const moref = "vm-both3"
	// last_activity_at 7 hours ago — past 6h threshold.
	c := makeCandidate(moref, pastTime(7*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample() // low CPU/net

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.Suspended != 1 {
		t.Errorf("expected 1 VM suspended, got %d", counts.Suspended)
	}
	if _, suspended := db.suspended[c.PodVMID]; !suspended {
		t.Error("expected DB SetVMSuspended to be called")
	}
	if vc.suspendedMorefs[moref] != 1 {
		t.Errorf("expected SuspendVM called once for %q, got %d", moref, vc.suspendedMorefs[moref])
	}
}

// TestThresholdFromSettings_GlobalDefault verifies that the 6-hour idle
// threshold is read from the database settings (not hardcoded) and that a VM
// that has been idle for just under the threshold is NOT suspended.
func TestThresholdFromSettings_GlobalDefault(t *testing.T) {
	const moref = "vm-thresh1"
	// last_activity_at 5h50m ago — just under the 6h default threshold.
	c := makeCandidate(moref, pastTime(5*time.Hour+50*time.Minute))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}
	db.idleTimeoutSeconds = 21600 // 6h

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.Suspended != 0 {
		t.Errorf("VM should NOT be suspended at 5h50m with 6h threshold, got %d suspensions", counts.Suspended)
	}
}

// TestThresholdFromSettings_PerPodOverride verifies that a per-pod timeout
// override wins over the global default.
func TestThresholdFromSettings_PerPodOverride(t *testing.T) {
	const moref = "vm-thresh2"
	// last_activity_at 2.5 hours ago.
	c := makeCandidate(moref, pastTime(2*time.Hour+30*time.Minute))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}
	db.idleTimeoutSeconds = 21600          // global 6h
	db.podIdleOverride[c.PodID] = 3600 * 2 // per-pod override: 2h

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 2h30m > 2h override → should be suspended
	if counts.Suspended != 1 {
		t.Errorf("expected 1 suspension with 2h override at 2h30m idle, got %d", counts.Suspended)
	}
}

// TestLongLivedConsoleSession verifies the "don't suspend at hour 6 of an
// 8-hour session" case. A VM whose last_activity_at has been kept fresh by
// console heartbeats must NOT be suspended, even if it has low CPU.
func TestLongLivedConsoleSession(t *testing.T) {
	const moref = "vm-longsession"
	// Simulate a console heartbeat arriving 10 minutes ago (within 6h threshold).
	c := makeCandidate(moref, pastTime(10*time.Minute))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample() // low CPU — student is reading docs

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: false})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.Suspended != 0 {
		t.Errorf("VM with recent console heartbeat should NOT be suspended; got %d suspensions", counts.Suspended)
	}
	if counts.ActivityFresh != 1 {
		t.Errorf("expected ActivityFresh=1, got %d", counts.ActivityFresh)
	}
}

// TestDryRunMode verifies that dry-run mode performs zero vCenter mutations.
// The fake VC records any SuspendVM call; the test asserts none were made even
// when a VM is fully eligible for suspension.
func TestDryRunMode(t *testing.T) {
	const moref = "vm-dryrun"
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	counts, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Dry-run must make no vCenter calls.
	if len(vc.suspendedMorefs) != 0 {
		t.Errorf("dry-run: expected zero SuspendVM calls, got: %v", vc.suspendedMorefs)
	}
	// Dry-run must not write to the DB.
	if len(db.suspended) != 0 {
		t.Errorf("dry-run: expected zero SetVMSuspended calls, got: %v", db.suspended)
	}
	// Dry-run still counts the would-be suspension.
	if counts.Suspended != 1 {
		t.Errorf("dry-run: expected Suspended=1 (logged decision), got %d", counts.Suspended)
	}
}

// TestDryRunMetrics verifies that dry-run suspensions are recorded with the
// "dry_run" reason, not "idle", so metrics dashboards can distinguish them.
func TestDryRunMetrics(t *testing.T) {
	const moref = "vm-dryrun-metrics"
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	metrics := NewSuspendMetrics("", "test", nil) // BaseURL="" means Push is no-op
	_, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{
		DryRun: true,
		Pusher: metrics,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the metric was recorded under "dry_run" reason.
	metrics.mu.Lock()
	dryRunCount := metrics.suspendTotal["dry_run"]
	idleCount := metrics.suspendTotal["idle"]
	metrics.mu.Unlock()

	if dryRunCount != 1 {
		t.Errorf("expected dry_run counter=1, got %g", dryRunCount)
	}
	if idleCount != 0 {
		t.Errorf("expected idle counter=0 in dry-run mode, got %g", idleCount)
	}
}

// TestMetricsProductionCallSites verifies that the production evaluator path
// drives all three metrics — the counter, the gauge, and the timestamp — not
// just the RecordSuspend call site. This proves the metrics have real call
// sites in the production code path rather than being dead recorders.
func TestMetricsProductionCallSites(t *testing.T) {
	const moref = "vm-metrics-callsite"
	c := makeCandidate(moref, pastTime(8*time.Hour))

	db := newFakeDB()
	db.candidates = []database.IdleSuspendCandidate{c}

	vc := newFakeVC()
	vc.toolsRunning[moref] = true
	vc.perfSamples[moref] = idleSample()

	before := time.Now()
	metrics := NewSuspendMetrics("", "test", nil)
	_, err := evaluateIdleVMs(context.Background(), vc, db, nil, IdleEvaluatorConfig{
		DryRun: false,
		Pusher: metrics,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := time.Now()

	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	// RecordSuspend("idle") was called from evaluateIdleVMs.
	if metrics.suspendTotal["idle"] != 1 {
		t.Errorf("expected suspendTotal[idle]=1, got %g", metrics.suspendTotal["idle"])
	}
	// SetSuspendedGauge was called.
	if metrics.suspendedGauge != 1 {
		t.Errorf("expected suspendedGauge=1, got %g", metrics.suspendedGauge)
	}
	// SetLastRunTimestamp was called with a time in [before, after].
	tsUnix := int64(metrics.lastRunUnix)
	if tsUnix < before.Unix() || tsUnix > after.Unix() {
		t.Errorf("lastRunUnix %d not in [%d, %d]", tsUnix, before.Unix(), after.Unix())
	}
}
