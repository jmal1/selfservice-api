// Package provisioner — idle VM suspend evaluator.
//
// The evaluator runs on a ticker in the provision-worker. On each tick it
// inspects every running pod VM and suspends those that have been idle for
// longer than the configured threshold (default 2 hours).
//
// "Idle" means last_activity_at is older than the timeout. Console heartbeats
// and above-threshold CPU or network each refresh that clock. When VMware
// Tools is running, the latest sample must also be under the CPU and network
// thresholds: a compiling VM (high CPU, no console) must not be suspended, and
// a student reading docs with the console open (low CPU, fresh heartbeat)
// must not be suspended either.
//
// Tools not running is not a permanent veto. CPU and network cannot be read,
// so those signals are skipped, and the VM still suspends once the activity
// clock is older than the timeout. A moref missing from the tools result is a
// deleted VM and is never suspended.
//
// The sustained-idle requirement is last_activity_at itself: the evaluator
// updates it whenever it observes above-threshold utilisation, so a VM is
// suspended only after that timestamp has been untouched for the full timeout.
//
// Safety defaults:
//   - Dry-run mode is ON by default (WORKER_IDLE_EVALUATOR_DRY_RUN=true).
//     The evaluator logs decisions but makes zero vCenter mutations until an
//     operator explicitly sets WORKER_IDLE_EVALUATOR_DRY_RUN=false after
//     validating the decisions on live data.
//   - Refusal guards block suspension even when the clock says idle:
//     1. VM has an active job in flight.
//     2. Pod is provisioning or destroying.
//     3. An assessment run is in flight targeting this VM.
//     4. Tools status is absent from the vCenter result (deleted moref).
//     5. No activity timestamp has been recorded.
package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// DryRunParseOutcome classifies which of the three startup-log branches applies.
type DryRunParseOutcome int

const (
	// DryRunOnDefault: env var is unset or empty — dry-run ON by default.
	DryRunOnDefault DryRunParseOutcome = iota
	// DryRunOffExplicit: env var is "false" (case-insensitive) — dry-run OFF, suspension live.
	DryRunOffExplicit
	// DryRunOnUnrecognised: env var is set but not a recognised value — dry-run ON,
	// emit a WARN so the operator knows their value had no effect.
	DryRunOnUnrecognised
)

// DryRunEnvResult is the structured result of ParseDryRunEnv. Callers log
// based on Outcome; the bool DryRun is what actually gates suspension.
type DryRunEnvResult struct {
	DryRun  bool               // whether dry-run is active (true = safe, no mutations)
	Outcome DryRunParseOutcome // which branch was taken (drives startup log level)
	Raw     string             // the raw env value, reproduced verbatim in warnings
}

// ParseDryRunEnv parses WORKER_IDLE_EVALUATOR_DRY_RUN into a DryRunEnvResult.
//
// Fail-closed: dry-run is ON for every value that is not an explicit,
// unambiguous "false". This means "0", "yes", "1", "no", "off", "" — anything
// that is not case-insensitively equal to "false" — leaves dry-run ON.
//
// The three outcomes let callers emit the right startup log:
//   - DryRunOnDefault   → INFO  (unset or empty, safe default)
//   - DryRunOffExplicit → INFO  ("false" — operator has deliberately armed suspension)
//   - DryRunOnUnrecognised → WARN (set to something non-false; the operator likely
//     intended to disable dry-run but the value was not accepted)
func ParseDryRunEnv(v string) DryRunEnvResult {
	if v == "" {
		return DryRunEnvResult{DryRun: true, Outcome: DryRunOnDefault, Raw: v}
	}
	if strings.EqualFold(v, "false") {
		return DryRunEnvResult{DryRun: false, Outcome: DryRunOffExplicit, Raw: v}
	}
	return DryRunEnvResult{DryRun: true, Outcome: DryRunOnUnrecognised, Raw: v}
}

// Default idle-evaluation thresholds.
const (
	// cpuIdleThreshold is cpu.usage.average in hundredths of a percent.
	// 500 = 5.00 %; a VM doing real work almost always exceeds this.
	cpuIdleThreshold int64 = 500

	// netIdleThreshold is net.usage.average in KBps. 10 KBps is well above
	// background noise (ARP, keepalives) but well below a real data transfer.
	netIdleThreshold int64 = 10
)

// idleEvalVCenter is the vCenter interface the idle evaluator needs. The real
// *vcenter.Client satisfies it automatically (asserted below). Tests inject a
// fake without a govmomi simulator.
type idleEvalVCenter interface {
	SuspendVM(ctx context.Context, moref string) error
	SamplePodVMPerf(ctx context.Context, morefs []string) (map[string]vcenter.VMPerfSample, error)
	QueryVMToolsStatusBulk(ctx context.Context, morefs []string) (map[string]bool, error)
}

// idleEvalDB is the database interface the idle evaluator needs.
type idleEvalDB interface {
	ListRunningPodVMsForIdleEval(ctx context.Context) ([]database.IdleSuspendCandidate, error)
	IdleSuspendCandidateStillEligible(ctx context.Context, podVMID uuid.UUID) (bool, error)
	TouchVMActivityAt(ctx context.Context, id uuid.UUID, t time.Time) error
	SetVMSuspended(ctx context.Context, id uuid.UUID, t time.Time, reason string) error
	GetIdleTimeoutSeconds(ctx context.Context, podID uuid.UUID) (int, error)
	HasActiveJobForVM(ctx context.Context, podVMID uuid.UUID) (bool, error)
	HasInFlightRunForVM(ctx context.Context, podVMID uuid.UUID) (bool, error)
}

// Compile-time proof the real clients satisfy the narrow seams.
var (
	_ idleEvalVCenter = (*vcenter.Client)(nil)
	_ idleEvalDB      = (*database.Queries)(nil)
)

// IdleEvaluatorConfig configures one EvaluateIdleVMs run.
type IdleEvaluatorConfig struct {
	// DryRun, when true, causes the evaluator to log what it would suspend but
	// make no vCenter mutations. This is the production default: operators must
	// explicitly disable it after validating decisions on live data.
	DryRun bool

	// Pusher, if set, receives metrics after the pass completes. Push failures
	// are logged but never returned.
	Pusher *SuspendMetrics
}

// IdleEvalCounts summarises one evaluator pass for logs and metrics.
type IdleEvalCounts struct {
	Candidates    int // running pod VMs examined
	Skipped       int // skipped for missing moref or bulk-query error
	ToolsMissing  int // Tools status absent, or Tools down while the idle clock is still fresh
	PerfMissing   int // no perf data — not idle (guard 4 variant)
	ActivityFresh int // last_activity_at within threshold — not idle
	// ActivityUnknown counts VMs with no activity timestamp at all. These are
	// refused, never suspended. A non-zero value means rows are being created
	// without an idle clock (see migration 000028) — it should be 0 in steady
	// state, so it is worth alerting on rather than hiding.
	ActivityUnknown int
	RefusedJob      int // active job in flight (guard 1)
	RefusedPod      int // pod provisioning/destroying (guard 2)
	RefusedRun      int // assessment run in flight (guard 3)
	Suspended       int // suspended (or would-be suspended in dry-run)
	Errors          int // DB or vCenter error during processing
}

// EvaluateIdleVMs is the Provisioner-bound entry point. The provision-worker
// calls this on a ticker.
func (p *Provisioner) EvaluateIdleVMs(ctx context.Context, cfg IdleEvaluatorConfig) (IdleEvalCounts, error) {
	return evaluateIdleVMs(ctx, p.vc, p.db, p.logger, cfg)
}

// RunIdleEvaluator is the pure implementation, factored out so tests can
// inject fake vCenter and database clients.
func evaluateIdleVMs(
	ctx context.Context,
	vc idleEvalVCenter,
	db idleEvalDB,
	logger *slog.Logger,
	cfg IdleEvaluatorConfig,
) (IdleEvalCounts, error) {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "idle_evaluator", "dry_run", cfg.DryRun)

	candidates, err := db.ListRunningPodVMsForIdleEval(ctx)
	if err != nil {
		return IdleEvalCounts{}, fmt.Errorf("list idle candidates: %w", err)
	}

	counts := IdleEvalCounts{Candidates: len(candidates)}
	if len(candidates) == 0 {
		return counts, nil
	}

	// Collect morefs for the two batched vCenter calls.
	morefs := make([]string, 0, len(candidates))
	for i := range candidates {
		if candidates[i].VCenterVMID != "" {
			morefs = append(morefs, candidates[i].VCenterVMID)
		} else {
			counts.Skipped++
		}
	}
	if len(morefs) == 0 {
		return counts, nil
	}

	// Batched bulk call 1: VMware Tools status for all morefs (one PropertyCollector request).
	toolsRunning, err := vc.QueryVMToolsStatusBulk(ctx, morefs)
	if err != nil {
		log.Error("bulk tools status query failed; skipping all VMs this tick", "error", err)
		counts.Skipped += len(morefs)
		return counts, err
	}

	// Batched bulk call 2: performance sample for all morefs (one QueryPerf request).
	perfSamples, err := vc.SamplePodVMPerf(ctx, morefs)
	if err != nil {
		log.Error("batched perf sample failed; skipping all VMs this tick", "error", err)
		counts.Skipped += len(morefs)
		return counts, err
	}

	now := time.Now()

	for i := range candidates {
		c := &candidates[i]
		if c.VCenterVMID == "" {
			continue
		}

		vmLog := log.With("vm", c.DisplayName, "vm_id", c.PodVMID, "moref", c.VCenterVMID)

		// A moref missing from the tools result is a deleted VM. Do not suspend
		// an object vCenter did not return.
		toolsUp, toolsKnown := toolsRunning[c.VCenterVMID]
		if !toolsKnown {
			vmLog.Debug("idle-eval: skip — VMware Tools status absent")
			counts.ToolsMissing++
			continue
		}

		var (
			perf     vcenter.VMPerfSample
			havePerf bool
		)
		if toolsUp {
			// Performance data is required only while Tools can report it.
			sample, sampleOK := perfSamples[c.VCenterVMID]
			if !sampleOK || !sample.Valid {
				vmLog.Debug("idle-eval: skip — no perf data available")
				counts.PerfMissing++
				continue
			}
			perf = sample
			havePerf = true

			// Above-threshold utilisation refreshes last_activity_at so the
			// idle clock restarts — this VM is actively working.
			if perf.CPUUsage >= cpuIdleThreshold || perf.NetUsage >= netIdleThreshold {
				if terr := db.TouchVMActivityAt(ctx, c.PodVMID, now); terr != nil {
					vmLog.Warn("idle-eval: failed to update activity timestamp", "error", terr)
					counts.Errors++
				}
				vmLog.Debug("idle-eval: not idle — utilisation above threshold",
					"cpu_usage", perf.CPUUsage, "net_usage", perf.NetUsage)
				counts.ActivityFresh++
				continue
			}
		} else {
			vmLog.Debug("idle-eval: VMware Tools not running; using the activity clock only")
		}

		// Resolve idle timeout for this pod (per-pod override or global default).
		timeoutSecs, terr := db.GetIdleTimeoutSeconds(ctx, c.PodID)
		if terr != nil {
			vmLog.Warn("idle-eval: failed to get idle timeout; using default 2h", "error", terr)
			timeoutSecs = models.DefaultIdleTimeoutSeconds
		}
		threshold := time.Duration(timeoutSecs) * time.Second

		// Guard 5: an unknown activity clock is not evidence of idleness.
		//
		// last_activity_at is NULL for any row created before migration 000028
		// and for any insert path that bypasses the column DEFAULT. Reading NULL
		// as "idle since the beginning of time" is the most dangerous possible
		// interpretation: it makes a freshly-provisioned VM instantly eligible
		// for suspension, before the student has even connected. Unknown must
		// mean "do not act", because the cost of a wrong suspend is destroyed
		// student work while the cost of a missed suspend is some idle RAM.
		if c.LastActivityAt == nil {
			vmLog.Warn("idle-eval: refuse — no activity timestamp recorded; " +
				"treating unknown as active (see migration 000028)")
			counts.ActivityUnknown++
			continue
		}

		// Check last_activity_at freshness. Tools-down VMs stay in ToolsMissing
		// until this clock expires, so a pass summary still shows why they
		// were held.
		if now.Sub(*c.LastActivityAt) < threshold {
			if !toolsUp {
				vmLog.Debug("idle-eval: skip — Tools not running and last_activity_at within threshold",
					"last_activity_at", c.LastActivityAt, "threshold", threshold)
				counts.ToolsMissing++
				continue
			}
			vmLog.Debug("idle-eval: not idle — last_activity_at within threshold",
				"last_activity_at", c.LastActivityAt, "threshold", threshold)
			counts.ActivityFresh++
			continue
		}

		// The activity clock has expired. Apply refusal guards before acting.

		// Guard 2: pod must be active (not provisioning or destroying).
		if c.PodStatus != models.PodStatusActive {
			vmLog.Info("idle-eval: refuse — pod not active", "pod_status", c.PodStatus)
			counts.RefusedPod++
			continue
		}

		// Guard 1: no active job targeting this VM.
		if hasJob, jerr := db.HasActiveJobForVM(ctx, c.PodVMID); jerr != nil {
			vmLog.Warn("idle-eval: active-job check failed; skipping VM", "error", jerr)
			counts.Errors++
			continue
		} else if hasJob {
			vmLog.Info("idle-eval: refuse — active job in flight")
			counts.RefusedJob++
			continue
		}

		// Guard 3: no assessment run in flight for this VM.
		if hasRun, rerr := db.HasInFlightRunForVM(ctx, c.PodVMID); rerr != nil {
			vmLog.Warn("idle-eval: in-flight run check failed; skipping VM", "error", rerr)
			counts.Errors++
			continue
		} else if hasRun {
			vmLog.Info("idle-eval: refuse — assessment run in flight")
			counts.RefusedRun++
			continue
		}

		// All guards passed. Build the suspend reason. LastActivityAt is
		// guaranteed non-nil here: guard 5 refuses any VM without a clock.
		idleFor := now.Sub(*c.LastActivityAt).Round(time.Minute)
		reason := fmt.Sprintf("idle: no console activity and low CPU/net for %s (threshold %s)", idleFor, threshold)
		if !toolsUp {
			reason = fmt.Sprintf("idle: no console activity for %s and VMware Tools is not running (threshold %s)", idleFor, threshold)
		}
		logArgs := []any{
			"reason", reason,
			"tools_running", toolsUp,
			"last_activity_at", c.LastActivityAt,
			"threshold_seconds", timeoutSecs,
		}
		if havePerf {
			logArgs = append(logArgs, "cpu_usage", perf.CPUUsage, "net_usage", perf.NetUsage)
		}

		if cfg.DryRun {
			vmLog.Info("idle-eval: DRY-RUN — would suspend", logArgs...)
			counts.Suspended++
			if cfg.Pusher != nil {
				cfg.Pusher.RecordSuspend("dry_run")
			}
			continue
		}

		vmLog.Info("idle-eval: suspending VM", logArgs...)

		stillEligible, eligibilityErr := db.IdleSuspendCandidateStillEligible(ctx, c.PodVMID)
		if eligibilityErr != nil || !stillEligible {
			vmLog.Warn("idle-eval: final role/status revalidation refused suspension", "error", eligibilityErr)
			counts.Errors++
			continue
		}
		if serr := vc.SuspendVM(ctx, c.VCenterVMID); serr != nil {
			vmLog.Error("idle-eval: SuspendVM failed", "error", serr)
			counts.Errors++
			if cfg.Pusher != nil {
				cfg.Pusher.RecordSuspend("error")
			}
			continue
		}

		if serr := db.SetVMSuspended(ctx, c.PodVMID, now, reason); serr != nil {
			vmLog.Error("idle-eval: SetVMSuspended failed", "error", serr)
			counts.Errors++
		}

		vmLog.Info("idle-eval: VM suspended", "vm", c.DisplayName)
		counts.Suspended++
		if cfg.Pusher != nil {
			cfg.Pusher.RecordSuspend("idle")
		}
	}

	if cfg.Pusher != nil {
		cfg.Pusher.SetSuspendedGauge(counts.Suspended)
		cfg.Pusher.SetLastRunTimestamp(now)
	}

	log.Info("idle-eval: pass complete",
		"candidates", counts.Candidates,
		"suspended", counts.Suspended,
		"refused_job", counts.RefusedJob,
		"refused_pod", counts.RefusedPod,
		"refused_run", counts.RefusedRun,
		"tools_missing", counts.ToolsMissing,
		"perf_missing", counts.PerfMissing,
		"activity_fresh", counts.ActivityFresh,
		"activity_unknown", counts.ActivityUnknown,
		"errors", counts.Errors,
	)

	return counts, nil
}
