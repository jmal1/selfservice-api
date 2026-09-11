// template_health_reconcile.go — periodic health checker for student-visible templates.
//
// # Design overview
//
// The checker runs every 12 hours (configurable). Each cycle:
//
//  1. [Structural check] For every student-visible template, verify the vCenter
//     VM/template object still exists. Cheap: one vCenter API call per template.
//
//  2. [Deep check] Clone exactly one template (least-recently-deep-checked),
//     power it on, wait for a guest IP, then destroy. This rotates across all
//     templates over successive cycles.
//
// # Anti-flap guarantees
//
//   - Retries: each failing check is retried 3 times with exponential backoff
//     (1s, 2s, 4s) before recording a failure. A transient vCenter API error
//     does not count as a failure.
//   - Structural confirmation: structural failures require 2 failed cycles.
//   - Deep confirmation: a first failed deep cycle schedules a durable delayed
//     job which creates fresh validation artifacts. Only that separate failure
//     can mark deep health unhealthy.
//   - Recovery: a passing check clears its own check-type failure state.
//   - Checker-level failures: if vCenter is unreachable or the checker itself
//     errors, crucible_template_health_checker_up is set to 0 and per-template
//     states are NOT modified. This means a vCenter outage fires exactly one
//     infrastructure alert, not N per-template alerts.
//
// # Deep-check cleanup
//
//	The deep check names each clone "crucible-healthcheck-<templateID>-<attempt>" and
//	places it in the Templates folder (not the Student-VMs folder, which the
//	orphan reconciler scans). A deferred destroy runs even when the power-on
//	or wait-for-IP step fails. A sweep at worker startup catches any clones
//	left behind by a crash.
package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// TemplateHealthReconcilerConfig controls one health reconciler pass.
type TemplateHealthReconcilerConfig struct {
	// Interval is how often the reconciler runs. Default 12h.
	Interval time.Duration

	// DeepCheckTimeout is how long the deep check may take (clone + power-on
	// + wait for IP). Default 10 minutes.
	DeepCheckTimeout time.Duration

	// TemplateFolder is the vCenter folder where health-check clones are
	// created. Should be the Templates folder, NOT the Student-VMs folder.
	// Defaults to the vCenter client's configured TemplateFolder.
	TemplateFolder string

	// Network is the staging port group used for health-check clones.
	// Empty skips NIC reconfiguration (acceptable for the health check).
	Network string

	// MaxRetries is the number of per-check retry attempts before a failure
	// is recorded. Default 3.
	MaxRetries int

	// RetryBaseDelay is the initial retry delay. Each retry doubles it
	// (exponential backoff). Default 1s.
	RetryBaseDelay time.Duration

	// DeepRetryBaseDelay is the initial retry delay for the deep check.
	// Deliberately much larger than RetryBaseDelay: a structural check
	// retries a read-only property fetch, where a 1s/2s backoff is ample.
	// The deep check retries a full clone against the "virtual disk is
	// either corrupted or not a supported format" fault, which
	// internal/vcenter/template_ops.go documents (Round 11, 2026-08-03) as
	// intermittent and environmental rather than a property of the source
	// VM or the CloneSpec. The recorded evidence is a clone of
	// student-ubuntu-2404 failing and the identical clone succeeding 68s
	// later, so a 1s/2s backoff would retry entirely inside the failure
	// window and report a healthy template as broken.
	//
	// Default 30s, giving attempts at t=0, t+30s, t+90s.
	DeepRetryBaseDelay time.Duration

	// ConfirmationBackoff is the durable delay between a first failed deep
	// cycle and the independent confirmation job. Default 5 minutes.
	ConfirmationBackoff time.Duration

	// Pusher, if set, receives metrics after each cycle.
	Pusher *TemplateHealthPusher
}

func applyTemplateHealthDefaults(cfg *TemplateHealthReconcilerConfig) {
	if cfg.Interval <= 0 {
		cfg.Interval = 12 * time.Hour
	}
	if cfg.DeepCheckTimeout <= 0 {
		cfg.DeepCheckTimeout = 10 * time.Minute
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = time.Second
	}
	if cfg.DeepRetryBaseDelay <= 0 {
		cfg.DeepRetryBaseDelay = 30 * time.Second
	}
	if cfg.ConfirmationBackoff <= 0 {
		cfg.ConfirmationBackoff = 5 * time.Minute
	}
	if cfg.ConfirmationBackoff > 30*time.Minute {
		cfg.ConfirmationBackoff = 30 * time.Minute
	}
}

// TemplateHealthCounts summarises one reconciler pass for logs and tests.
type TemplateHealthCounts struct {
	Templates    int  // total student-visible templates
	Healthy      int  // templates that passed structural check this cycle
	Unhealthy    int  // templates now in unhealthy state
	NewlyFailing int  // templates whose consecutive_failures incremented
	DeepChecked  bool // whether a deep check ran this cycle
	CheckerUp    bool // false when a checker-level infrastructure error occurred
}

// templateHealthDB is the narrow DB surface the health reconciler uses.
type templateHealthDB interface {
	ListStudentVisibleTemplates(ctx context.Context) ([]models.Template, error)
	GetTemplateByID(ctx context.Context, templateID uuid.UUID) (*models.Template, error)
	GetLeastRecentlyDeepCheckedTemplate(ctx context.Context) (*models.Template, error)
	GetTemplateHealthState(ctx context.Context, templateID uuid.UUID) (*database.TemplateHealthState, error)
	CompareAndSwapTemplateHealthState(ctx context.Context, state database.TemplateHealthState) (bool, error)
	ListTemplateHealthStates(ctx context.Context) ([]database.TemplateHealthState, error)
	CreateTemplateHealthConfirmationJob(ctx context.Context, templateID uuid.UUID, payload []byte, nextAt time.Time) (bool, error)
	ApplyTemplateHealthDeepConfirmation(ctx context.Context, templateID uuid.UUID, expectedFailureAt time.Time, passed bool, checkedAt time.Time, durationSeconds float64, errMsg *string, faultClass *string) (bool, error)
	ClearTemplateHealthDeepPending(ctx context.Context, templateID uuid.UUID, expectedFailureAt time.Time) (bool, error)
	GetLastTemplateHealthCycleCompletedAt(ctx context.Context) (*time.Time, error)
	MarkTemplateHealthCycleCompleted(ctx context.Context, completedAt time.Time) error
}

// templateHealthVCenter is the narrow vCenter surface the health reconciler
// needs. Production: *vcenter.Client. Tests: fakeHealthVCenter.
type templateHealthVCenter interface {
	VMExists(ctx context.Context, ref string) (bool, error)
	CloneForHealthCheck(ctx context.Context, params vcenter.HealthCheckCloneParams) (*vcenter.HealthCheckCloneResult, error)
	PowerOnVM(ctx context.Context, moref string) error
	WaitForIP(ctx context.Context, moref string, timeout time.Duration) (string, error)
	DestroyVM(ctx context.Context, moref string) error
}

// templateHealthMetrics is the narrow metrics surface the reconciler pushes to.
type templateHealthMetrics interface {
	ReplaceSnapshot(ctx context.Context, snapshot TemplateHealthSnapshot) error
}

// Compile-time satisfaction checks.
var _ templateHealthDB = (*database.Queries)(nil)
var _ templateHealthVCenter = (*vcenter.Client)(nil)
var _ templateHealthMetrics = (*TemplateHealthPusher)(nil)

// NOTE: there is deliberately no RunTemplateHealthReconciler loop here.
//
// One used to exist and was never called — the provision worker drives this
// reconciler from its own select loop so the run can be leader-gated. It was
// removed rather than left in place because it carried the same ticker-only
// defect this change fixes (a 12h time.Ticker with no immediate first run,
// which never fires on a service that restarts more often than the interval).
// Leaving a second, unreferenced copy of that bug in the tree invites it back.

// ReconcileTemplateHealthIfDue runs a health cycle only when one is actually
// due — that is, when no cycle has ever completed, or the most recent one
// finished longer ago than cfg.Interval. It reports whether it ran.
//
// The worker calls this on leader acquisition. Without it the feature is dead
// on arrival: the reconciler's only other trigger is a 12h time.Ticker created
// at process start, and this platform ships several deploys a day, so the
// ticker is reset long before it ever fires.
//
// The due-check is what makes an unconditional catch-up pass safe. Every cycle
// performs one real clone → power-on → destroy against vCenter, so running
// unconditionally on every leader acquisition would turn each deploy — and each
// leader failover — into another clone, against the same NFS 4.1 datastores
// whose clone failures this check exists to detect.
//
// A failure to read the clock is reported, not swallowed, and does NOT run the
// cycle: an unreadable clock is indistinguishable from "just ran", and the
// ticker remains as a backstop.
func (p *Provisioner) ReconcileTemplateHealthIfDue(ctx context.Context, cfg TemplateHealthReconcilerConfig) (TemplateHealthCounts, bool, error) {
	due, err := templateHealthCycleDue(ctx, p.db, cfg.Interval)
	if err != nil {
		return TemplateHealthCounts{}, false, err
	}
	if !due {
		return TemplateHealthCounts{}, false, nil
	}

	counts, err := p.ReconcileTemplateHealth(ctx, cfg)
	return counts, true, err
}

// templateHealthCycleDue reports whether a health cycle should run now: true
// when no cycle has ever completed, or the newest recorded structural check is
// older than interval. Factored out of the Provisioner method so it can be
// tested against the same fake DB the reconciler tests use.
func templateHealthCycleDue(ctx context.Context, db templateHealthDB, interval time.Duration) (bool, error) {
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	completedAt, err := db.GetLastTemplateHealthCycleCompletedAt(ctx)
	if err != nil {
		return false, fmt.Errorf("read last template health check time: %w", err)
	}
	if completedAt == nil {
		return true, nil
	}
	return time.Since(*completedAt) >= interval, nil
}

const templateHealthCASAttempts = 5

func mutateTemplateHealthState(
	ctx context.Context,
	db templateHealthDB,
	templateID uuid.UUID,
	templateName string,
	mutate func(*database.TemplateHealthState),
) (*database.TemplateHealthState, error) {
	for attempt := 0; attempt < templateHealthCASAttempts; attempt++ {
		state, err := db.GetTemplateHealthState(ctx, templateID)
		if err != nil {
			return nil, err
		}
		if state == nil {
			state = &database.TemplateHealthState{
				TemplateID:   templateID,
				TemplateName: templateName,
				HealthStatus: "unknown",
			}
		}
		mutate(state)
		refreshTemplateHealthAggregate(state)
		applied, err := db.CompareAndSwapTemplateHealthState(ctx, *state)
		if err != nil {
			return nil, err
		}
		if applied {
			return state, nil
		}
	}
	return nil, fmt.Errorf("template health state changed during %d compare-and-swap attempts", templateHealthCASAttempts)
}

// ReconcileTemplateHealth is the Provisioner-bound entry point. The provision
// worker calls this from its select loop and on leader acquisition.
//
// A panic anywhere in the cycle is converted into an error rather than allowed
// to escape. This is not defensive decoration: template health is a
// non-essential background health probe, but it runs in a goroutine inside the
// provision worker, so an escaping panic kills the whole process — including
// job claiming and every other reconciler. Worse, it does not stop at one
// replica: the crash releases the leader lock, the next replica acquires it,
// runs the same cycle, and dies too, walking the fault through all four.
//
// That is exactly what happened the first time this code ever executed: a
// govmomi property-destination bug in VMExists panicked, and provisioning was
// down across the cluster until template health was disabled. Degrading to
// "health checks are broken" is always preferable to "provisioning is down".
func (p *Provisioner) ReconcileTemplateHealth(ctx context.Context, cfg TemplateHealthReconcilerConfig) (TemplateHealthCounts, error) {
	// Guard the nil-pointer-in-interface trap: a nil *TemplateHealthPusher
	// would satisfy metrics != nil, then panic on the first method call.
	var m templateHealthMetrics
	if cfg.Pusher != nil {
		m = cfg.Pusher
	}
	return reconcileTemplateHealth(ctx, p.db, p.vc, m, p.logger, cfg)
}

// reconcileTemplateHealth is the pure implementation. Factored out so tests
// can inject fakes for every dependency.
//
// The recover lives here rather than on the Provisioner method for two
// reasons: it is the single choke point both trigger paths (the 12h ticker and
// the leader-acquisition catch-up) pass through, and Provisioner holds
// concrete *database.Queries / *vcenter.Client fields, so a guard at that
// boundary could never be tested with fakes. An untestable safety net is not a
// safety net.
func reconcileTemplateHealth(
	ctx context.Context,
	db templateHealthDB,
	vc templateHealthVCenter,
	metrics templateHealthMetrics,
	logger *slog.Logger,
	cfg TemplateHealthReconcilerConfig,
) (counts TemplateHealthCounts, err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			// Discard whatever partial counts the aborted cycle had
			// accumulated. A named return would otherwise surface
			// "Templates: 8, CheckerUp: true" for a cycle that died,
			// and the caller logs those counts.
			counts = TemplateHealthCounts{CheckerUp: false}
			err = fmt.Errorf("template health reconcile panicked (contained; provisioning unaffected): %v", r)
			if logger != nil {
				logger.Error("template health reconcile panicked",
					"component", "template_health_reconciler",
					"panic", fmt.Sprint(r),
					"stack", string(stack))
			}
		}
	}()

	applyTemplateHealthDefaults(&cfg)
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "template_health_reconciler")

	// --- Step 1: list student-visible templates --------------------------------
	templates, err := db.ListStudentVisibleTemplates(ctx)
	if err != nil {
		// DB is unavailable → checker-level failure.
		log.Error("list student-visible templates failed", "error", err)
		return TemplateHealthCounts{CheckerUp: false}, fmt.Errorf("list student-visible templates: %w", err)
	}

	counts = TemplateHealthCounts{
		Templates: len(templates),
		CheckerUp: true,
	}

	// --- Step 2: structural check every template --------------------------------
	// Probe vCenter once to detect connectivity before looping, so a vCenter
	// outage doesn't mark every template as failed — it fires a single
	// checker_up=0 alert instead.
	if len(templates) > 0 {
		first := templates[0]
		ref := first.VCenterRef()
		if ref != "" {
			_, probeErr := retryWithBackoff(ctx, cfg.MaxRetries, cfg.RetryBaseDelay, func() error {
				_, err := vc.VMExists(ctx, ref)
				return err
			})
			if probeErr != nil && isCheckerLevelError(probeErr) {
				// vCenter unreachable: mark checker down, leave template states alone.
				log.Error("vCenter probe failed; marking checker_up=0; template states unchanged",
					"error", probeErr)
				_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
				return TemplateHealthCounts{
					Templates: len(templates),
					CheckerUp: false,
				}, nil
			}
		}
	}

	for _, tmpl := range templates {
		ref := tmpl.VCenterRef()
		if ref == "" {
			log.Warn("template has no vCenter ref; skipping structural check",
				"template_id", tmpl.ID, "name", tmpl.Name)
			continue
		}

		start := time.Now()
		_, checkErr := retryWithBackoff(ctx, cfg.MaxRetries, cfg.RetryBaseDelay, func() error {
			ok, err := vc.VMExists(ctx, ref)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("vCenter object %q does not exist", ref)
			}
			return nil
		})
		durSec := time.Since(start).Seconds()
		if checkErr != nil && isCheckerLevelError(checkErr) {
			counts.CheckerUp = false
			log.Error("structural check lost vCenter connectivity; template state unchanged",
				"template", tmpl.Name,
				"fault_class", classifyTemplateHealthFault(checkErr),
				"error", checkErr)
			_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
			return counts, nil
		}

		now := time.Now().UTC().Truncate(time.Microsecond)
		passed := checkErr == nil
		state, stateErr := mutateTemplateHealthState(ctx, db, tmpl.ID, tmpl.Name, func(state *database.TemplateHealthState) {
			state.LastStructuralCheckAt = &now
			state.LastStructuralPassed = boolPtr(passed)
			state.LastStructuralDurationSeconds = float64Ptr(durSec)
			if passed {
				state.StructuralConsecutiveFailures = 0
				state.LastStructuralError = nil
				state.LastStructuralFaultClass = nil
			} else {
				state.StructuralConsecutiveFailures++
				errMsg := "structural check failed"
				if checkErr != nil {
					errMsg = truncateErr(checkErr.Error(), 256)
				}
				faultClass := classifyTemplateHealthFault(checkErr)
				state.LastStructuralError = &errMsg
				state.LastStructuralFaultClass = &faultClass
			}
		})
		if stateErr != nil {
			log.Error("persist structural template health attempt failed",
				"template_id", tmpl.ID, "error", stateErr)
			counts.CheckerUp = false
			_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
			return counts, fmt.Errorf("persist structural template health attempt %s: %w", tmpl.ID, stateErr)
		}
		if passed {
			counts.Healthy++
		} else {
			counts.NewlyFailing++
			if state.StructuralConsecutiveFailures >= 2 {
				counts.Unhealthy++
			}
		}

		log.Info("structural check complete",
			"template", tmpl.Name,
			"ref", ref,
			"passed", passed,
			"consecutive_failures", state.StructuralConsecutiveFailures,
			"health_status", state.HealthStatus,
			"duration_ms", int(durSec*1000),
		)
	}

	// --- Step 3: deep check — one template per cycle, least-recently-checked ---
	deepTarget, err := db.GetLeastRecentlyDeepCheckedTemplate(ctx)
	if err != nil {
		log.Warn("get least-recently-deep-checked template failed; skipping deep check", "error", err)
	}

	if deepTarget != nil {
		ref := deepTarget.VCenterRef()
		if ref == "" {
			log.Warn("deep-check target has no vCenter ref; skipping",
				"template", deepTarget.Name)
		} else {
			start := time.Now()
			// The deep check is a full clone → power-on → wait-for-IP. It is
			// the single most failure-prone operation in this codebase, and
			// internal/vcenter/template_ops.go documents its dominant fault
			// ("virtual disk is either corrupted or not a supported format")
			// as intermittent and environmental, with job-level retry as the
			// prescribed remedy. Without this retry a healthy template is
			// reported broken on the first transient blip — the exact
			// erroneous alert this feature exists to avoid. Each attempt
			// runs under its own DeepCheckTimeout and its own deferred
			// destroy, so a failed attempt cannot leak a clone into the next.
			_, deepErr := retryWithBackoff(ctx, cfg.MaxRetries, cfg.DeepRetryBaseDelay, func() error {
				return runDeepCheck(ctx, vc, deepTarget, cfg, log)
			})
			durSec := time.Since(start).Seconds()
			counts.DeepChecked = true
			if deepErr != nil && isDeepCheckerLevelError(deepErr) {
				counts.CheckerUp = false
				log.Error("deep check could not reach vCenter; raw and confirmed template state unchanged",
					"template", deepTarget.Name,
					"fault_class", classifyTemplateHealthFault(deepErr),
					"error", deepErr)
				_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
				return counts, nil
			}

			// PostgreSQL timestamptz has microsecond precision. The pending
			// timestamp is also the confirmation job's compare-and-swap token,
			// so truncate before both persistence and payload generation.
			now := time.Now().UTC().Truncate(time.Microsecond)
			passed := deepErr == nil
			state, stateErr := mutateTemplateHealthState(ctx, db, deepTarget.ID, deepTarget.Name, func(state *database.TemplateHealthState) {
				state.LastDeepCheckAt = &now
				state.LastDeepPassed = boolPtr(passed)
				state.LastDeepDurationSeconds = float64Ptr(durSec)
				if passed {
					state.DeepConsecutiveFailures = 0
					state.LastDeepError = nil
					state.LastDeepFaultClass = nil
					state.PendingDeepFailureAt = nil
					state.DeepConfirmationDueAt = nil
				} else {
					errMsg := truncateErr(deepErr.Error(), 256)
					faultClass := classifyTemplateHealthFault(deepErr)
					state.LastDeepError = &errMsg
					state.LastDeepFaultClass = &faultClass
					if state.DeepConsecutiveFailures < 2 {
						state.DeepConsecutiveFailures = 1
						if state.PendingDeepFailureAt == nil {
							pendingAt := now
							dueAt := now.Add(cfg.ConfirmationBackoff)
							state.PendingDeepFailureAt = &pendingAt
							state.DeepConfirmationDueAt = &dueAt
						}
					}
				}
			})
			if stateErr != nil {
				log.Error("persist deep template health attempt failed",
					"template_id", deepTarget.ID, "error", stateErr)
				counts.CheckerUp = false
				_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
				return counts, fmt.Errorf("persist deep template health attempt %s: %w", deepTarget.ID, stateErr)
			}

			if state.PendingDeepFailureAt != nil {
				if _, err := ensureTemplateHealthConfirmation(ctx, db, *state); err != nil {
					log.Error("schedule deep-check confirmation failed",
						"template_id", deepTarget.ID,
						"pending_failure_at", state.PendingDeepFailureAt,
						"error", err)
					counts.CheckerUp = false
					_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
					return counts, fmt.Errorf("schedule deep-check confirmation %s: %w", deepTarget.ID, err)
				}
			}

			// Log the error, not just the boolean. Without this the operator
			// sees only `passed:false` and has to go read last_error out of
			// Postgres to find out why — which is exactly what happened on
			// the first production cycle of this feature.
			if deepErr != nil {
				log.Error("deep check failed after retries",
					"template", deepTarget.Name,
					"attempts", cfg.MaxRetries,
					"duration_ms", int(durSec*1000),
					"fault_class", classifyTemplateHealthFault(deepErr),
					"error", deepErr,
				)
			}

			log.Info("deep check complete",
				"template", deepTarget.Name,
				"passed", passed,
				"duration_ms", int(durSec*1000),
			)
		}
	}

	// --- Step 4: push metrics -------------------------------------------------
	if err := pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(true), log); err != nil {
		return counts, err
	}
	if err := db.MarkTemplateHealthCycleCompleted(ctx, time.Now().UTC()); err != nil {
		counts.CheckerUp = false
		_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), log)
		return counts, fmt.Errorf("mark template health cycle completed: %w", err)
	}

	log.Info("template health reconcile complete",
		"templates", counts.Templates,
		"healthy", counts.Healthy,
		"unhealthy", counts.Unhealthy,
		"newly_failing", counts.NewlyFailing,
		"deep_checked", counts.DeepChecked,
	)
	return counts, nil
}

// TemplateHealthConfirmationPayload ties a durable confirmation job to the
// exact first failed deep attempt that created it.
type TemplateHealthConfirmationPayload struct {
	TemplateID       uuid.UUID `json:"template_id"`
	PendingFailureAt time.Time `json:"pending_failure_at"`
}

// ReconcileTemplateHealthConfirmations repairs the durable schedule from
// persisted pending state. It is safe to call concurrently from startup,
// ticker, and failover paths because enqueue uses a deterministic job ID.
func (p *Provisioner) ReconcileTemplateHealthConfirmations(ctx context.Context, cfg TemplateHealthReconcilerConfig) (int, error) {
	return reconcileTemplateHealthConfirmations(ctx, p.db, cfg, p.logger)
}

// ReplaceTemplateHealthSnapshot clears Pushgateway series left by a previous
// worker process before a due-check decides whether a full vCenter cycle is
// needed. A lightweight vCenter probe always supplies checker_up, so a normal
// deploy cannot leave the alert gate absent until the next 12-hour cycle.
func (p *Provisioner) ReplaceTemplateHealthSnapshot(ctx context.Context, cfg TemplateHealthReconcilerConfig) error {
	var metrics templateHealthMetrics
	if cfg.Pusher != nil {
		metrics = cfg.Pusher
	}
	return replaceTemplateHealthSnapshot(ctx, p.db, p.vc, metrics, p.logger, cfg)
}

func replaceTemplateHealthSnapshot(
	ctx context.Context,
	db templateHealthDB,
	vc templateHealthVCenter,
	metrics templateHealthMetrics,
	logger *slog.Logger,
	cfg TemplateHealthReconcilerConfig,
) error {
	if metrics == nil {
		return nil
	}
	applyTemplateHealthDefaults(&cfg)
	templates, err := db.ListStudentVisibleTemplates(ctx)
	if err != nil {
		return fmt.Errorf("list templates for replacement snapshot: %w", err)
	}
	checkerUp := true
	for _, tmpl := range templates {
		ref := tmpl.VCenterRef()
		if ref == "" {
			continue
		}
		_, probeErr := retryWithBackoff(ctx, cfg.MaxRetries, cfg.RetryBaseDelay, func() error {
			_, err := vc.VMExists(ctx, ref)
			return err
		})
		if probeErr != nil && isCheckerLevelError(probeErr) {
			checkerUp = false
		}
		break
	}
	return pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(checkerUp), logger)
}

func reconcileTemplateHealthConfirmations(
	ctx context.Context,
	db templateHealthDB,
	_ TemplateHealthReconcilerConfig,
	_ *slog.Logger,
) (int, error) {
	states, err := db.ListTemplateHealthStates(ctx)
	if err != nil {
		return 0, fmt.Errorf("list template health confirmation state: %w", err)
	}
	enqueued := 0
	for _, state := range states {
		if state.PendingDeepFailureAt == nil {
			continue
		}
		created, err := ensureTemplateHealthConfirmation(ctx, db, state)
		if err != nil {
			return enqueued, err
		}
		if created {
			enqueued++
		}
	}

	return enqueued, nil
}

func ensureTemplateHealthConfirmation(
	ctx context.Context,
	db templateHealthDB,
	state database.TemplateHealthState,
) (bool, error) {
	if state.PendingDeepFailureAt == nil {
		return false, nil
	}
	nextAt := *state.PendingDeepFailureAt
	if state.DeepConfirmationDueAt != nil {
		nextAt = *state.DeepConfirmationDueAt
	}
	payload, err := json.Marshal(TemplateHealthConfirmationPayload{
		TemplateID:       state.TemplateID,
		PendingFailureAt: *state.PendingDeepFailureAt,
	})
	if err != nil {
		return false, fmt.Errorf("marshal template health confirmation: %w", err)
	}
	created, err := db.CreateTemplateHealthConfirmationJob(ctx, state.TemplateID, payload, nextAt)
	if err != nil {
		return false, fmt.Errorf("create template health confirmation job: %w", err)
	}
	return created, nil
}

// ConfirmTemplateHealth runs a fresh clone artifact for a separately scheduled
// deep confirmation. A template-level failure is a successful job outcome that
// atomically confirms persisted health; checker-level infrastructure failures
// return an error so the normal durable job retry policy applies.
func (p *Provisioner) ConfirmTemplateHealth(ctx context.Context, job *models.Job) error {
	var metrics templateHealthMetrics
	if p.healthCfg.Pusher != nil {
		metrics = p.healthCfg.Pusher
	}
	return confirmTemplateHealth(ctx, p.db, p.vc, metrics, p.logger, p.healthCfg, job)
}

func confirmTemplateHealth(
	ctx context.Context,
	db templateHealthDB,
	vc templateHealthVCenter,
	metrics templateHealthMetrics,
	logger *slog.Logger,
	cfg TemplateHealthReconcilerConfig,
	job *models.Job,
) error {
	var payload TemplateHealthConfirmationPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template_health_confirm payload: %w", err)
	}
	if payload.TemplateID == uuid.Nil || payload.PendingFailureAt.IsZero() {
		return fmt.Errorf("template_id and pending_failure_at are required")
	}

	state, err := db.GetTemplateHealthState(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template health state: %w", err)
	}
	if state == nil || state.PendingDeepFailureAt == nil ||
		!state.PendingDeepFailureAt.Equal(payload.PendingFailureAt) {
		logger.Info("template health confirmation is stale; no-op",
			"template_id", payload.TemplateID,
			"pending_failure_at", payload.PendingFailureAt)
		if err := replaceTemplateHealthSnapshot(ctx, db, vc, metrics, logger, cfg); err != nil {
			return fmt.Errorf("republish template health snapshot for stale confirmation: %w", err)
		}
		return nil
	}

	tmpl, err := db.GetTemplateByID(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template for health confirmation: %w", err)
	}
	if tmpl == nil {
		return fmt.Errorf("template_id not found in database: %s", payload.TemplateID)
	}
	if !tmpl.IsActive || tmpl.IsInternal ||
		tmpl.TemplateState != models.TemplateStateActive || tmpl.Visibility != "public" {
		cleared, err := db.ClearTemplateHealthDeepPending(ctx, payload.TemplateID, payload.PendingFailureAt)
		if err != nil {
			return fmt.Errorf("clear hidden template health confirmation: %w", err)
		}
		logger.Info("template health confirmation target is no longer student-visible; pending schedule cleared",
			"template_id", payload.TemplateID)
		if cleared {
			if err := replaceTemplateHealthSnapshot(ctx, db, vc, metrics, logger, cfg); err != nil {
				return fmt.Errorf("replace template health snapshot after visibility change: %w", err)
			}
		}
		return nil
	}

	applyTemplateHealthDefaults(&cfg)

	started := time.Now()
	_, checkErr := retryWithBackoff(ctx, cfg.MaxRetries, cfg.DeepRetryBaseDelay, func() error {
		return runDeepCheck(ctx, vc, tmpl, cfg, logger)
	})
	durationSeconds := time.Since(started).Seconds()
	checkedAt := time.Now()
	if checkErr != nil && isDeepCheckerLevelError(checkErr) {
		_ = pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(false), logger)
		return fmt.Errorf("template health confirmation checker failure: %w", checkErr)
	}

	var errMsg, faultClass *string
	if checkErr != nil {
		msg := truncateErr(checkErr.Error(), 256)
		class := classifyTemplateHealthFault(checkErr)
		errMsg, faultClass = &msg, &class
	}
	applied, err := db.ApplyTemplateHealthDeepConfirmation(
		ctx,
		payload.TemplateID,
		payload.PendingFailureAt,
		checkErr == nil,
		checkedAt,
		durationSeconds,
		errMsg,
		faultClass,
	)
	if err != nil {
		return fmt.Errorf("apply template health confirmation: %w", err)
	}
	if !applied {
		logger.Info("template health confirmation superseded before commit",
			"template_id", payload.TemplateID,
			"pending_failure_at", payload.PendingFailureAt)
		return nil
	}

	if err := pushTemplateHealthSnapshot(ctx, db, metrics, boolPtr(true), logger); err != nil {
		return err
	}
	if checkErr != nil {
		logger.Error("deep check independently confirmed template unhealthy",
			"template", tmpl.Name,
			"fault_class", *faultClass,
			"error", checkErr,
			"duration_ms", int(durationSeconds*1000))
	} else {
		logger.Info("deep check confirmation passed; pending failure cleared",
			"template", tmpl.Name,
			"duration_ms", int(durationSeconds*1000))
	}
	return nil
}

func pushTemplateHealthSnapshot(
	ctx context.Context,
	db templateHealthDB,
	metrics templateHealthMetrics,
	checkerUp *bool,
	logger *slog.Logger,
) error {
	if metrics == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	states, err := db.ListTemplateHealthStates(ctx)
	if err != nil {
		logger.Warn("list persisted template health snapshot failed", "error", err)
		return fmt.Errorf("list persisted template health snapshot: %w", err)
	}
	if err := metrics.ReplaceSnapshot(ctx, TemplateHealthSnapshot{
		CheckerUp: checkerUp,
		States:    states,
		CreatedAt: time.Now(),
	}); err != nil {
		logger.Warn("template health metrics snapshot replacement failed", "error", err)
		return fmt.Errorf("replace template health metrics snapshot: %w", err)
	}
	return nil
}

func refreshTemplateHealthAggregate(state *database.TemplateHealthState) {
	state.ConsecutiveFailures = maxInt(
		state.StructuralConsecutiveFailures,
		state.DeepConsecutiveFailures,
	)
	switch {
	case state.StructuralConsecutiveFailures >= 2:
		state.HealthStatus = "unhealthy"
		state.LastError = state.LastStructuralError
	case state.DeepConsecutiveFailures >= 2:
		state.HealthStatus = "unhealthy"
		state.LastError = state.LastDeepError
	case state.LastStructuralPassed != nil && *state.LastStructuralPassed:
		state.HealthStatus = "healthy"
		state.LastError = firstNonNil(state.LastDeepError, state.LastStructuralError)
	case state.LastDeepPassed != nil && *state.LastDeepPassed:
		state.HealthStatus = "healthy"
		state.LastError = state.LastStructuralError
	default:
		if state.HealthStatus == "" {
			state.HealthStatus = "unknown"
		}
		state.LastError = firstNonNil(state.LastDeepError, state.LastStructuralError)
	}
}

func classifyTemplateHealthFault(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "virtual disk is either corrupted or not a supported format"):
		return "vsphere_virtual_disk_corrupt_or_unsupported"
	case strings.Contains(msg, "wait-for-ip"):
		return "guest_ip_timeout"
	case isCheckerLevelError(err):
		return "vsphere_connectivity"
	case strings.Contains(msg, "does not exist"), strings.Contains(msg, "not found"):
		return "vsphere_object_not_found"
	case strings.Contains(msg, "power-on"):
		return "vsphere_power_on"
	case strings.Contains(msg, "clone"):
		return "vsphere_clone"
	default:
		return "unknown"
	}
}

func isDeepCheckerLevelError(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(strings.ToLower(err.Error()), "wait-for-ip") {
		return false
	}
	return isCheckerLevelError(err)
}

func firstNonNil(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func boolPtr(v bool) *bool          { return &v }
func float64Ptr(v float64) *float64 { return &v }
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// runDeepCheck performs the full clone → power-on → wait-for-IP → destroy
// cycle for a single template. The destroy is deferred and runs even when
// earlier steps fail or the context is cancelled with a background timeout.
func runDeepCheck(
	ctx context.Context,
	vc templateHealthVCenter,
	tmpl *models.Template,
	cfg TemplateHealthReconcilerConfig,
	log *slog.Logger,
) (retErr error) {
	cloneName := vcenter.HealthCheckClonePrefix + tmpl.ID.String() + "-" + uuid.NewString()[:8]
	ref := tmpl.VCenterRef()

	deepCtx, cancel := context.WithTimeout(ctx, cfg.DeepCheckTimeout)
	defer cancel()

	// The clone must land on the template's own staging port group. A clone
	// inherits the template's NIC backing, and template VMs sit on a network
	// with no DHCP -- so leaving this unset means WaitForIP can never succeed
	// and the deep check fails by timeout on a perfectly healthy template.
	// This is the same network the template-verify path uses (template_jobs.go),
	// which is why verify has always worked and the deep check never has.
	network := tmpl.StagingNetwork
	if network == "" {
		network = cfg.Network
	}

	params := vcenter.HealthCheckCloneParams{
		SourceRef:  ref,
		CloneName:  cloneName,
		FolderPath: cfg.TemplateFolder,
		Network:    network,
		VCPUs:      int32(tmpl.DefaultVCPUs),
		RAMmb:      int64(tmpl.DefaultRAMMB),
	}
	if params.VCPUs == 0 {
		params.VCPUs = 1
	}
	if params.RAMmb == 0 {
		params.RAMmb = 512
	}

	log.Info("starting deep check", "template", tmpl.Name, "clone_name", cloneName)

	result, err := vc.CloneForHealthCheck(deepCtx, params)
	if err != nil {
		return fmt.Errorf("deep check: clone failed: %w", err)
	}

	moref := result.MoRef

	// Guarantee the clone is destroyed even if later steps fail or the
	// deadline is exceeded. Use a background context so the destroy still
	// runs when deepCtx is cancelled.
	defer func() {
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer destroyCancel()
		if err := vc.DestroyVM(destroyCtx, moref); err != nil {
			log.Error("deep check: deferred destroy failed (clone may be orphaned)",
				"template", tmpl.Name, "moref", moref, "error", err)
			// Do not override retErr — the check result already reflects the
			// actual health; the destroy failure is a separate operational issue.
		} else {
			log.Info("deep check: clone destroyed", "template", tmpl.Name, "moref", moref)
		}
	}()

	if err := vc.PowerOnVM(deepCtx, moref); err != nil {
		return fmt.Errorf("deep check: power-on failed: %w", err)
	}

	ip, err := vc.WaitForIP(deepCtx, moref, cfg.DeepCheckTimeout)
	if err != nil {
		return fmt.Errorf("deep check: wait-for-IP failed (no IP within %s): %w",
			cfg.DeepCheckTimeout, err)
	}

	log.Info("deep check passed", "template", tmpl.Name, "ip", ip)
	return nil
}

// retryWithBackoff calls fn up to maxRetries times. On each failure it waits
// baseDelay * 2^(attempt-1) before retrying. Returns the last error if all
// attempts fail, or nil on first success.
//
// The second return value (bool) indicates whether the first attempt succeeded
// without any retry — unused here but kept for symmetry with the VMExists signature.
func retryWithBackoff(ctx context.Context, maxRetries int, baseDelay time.Duration, fn func() error) (bool, error) {
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		lastErr = fn()
		if lastErr == nil {
			return attempt == 1, nil
		}
		if attempt == maxRetries {
			break
		}
		delay := baseDelay * time.Duration(1<<uint(attempt-1))
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}
	}
	return false, lastErr
}

// isCheckerLevelError reports whether an error from a vCenter call is a
// connectivity/auth problem (as opposed to a per-template "object not found"
// problem). When this returns true the reconciler should set checker_up=0
// instead of marking individual templates as failing.
func isCheckerLevelError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// A guest that never acquires an IP is a valid deep-check observation,
	// even when the underlying wait ends with context deadline exceeded.
	if strings.Contains(msg, "wait-for-ip failed") {
		return false
	}
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "tls handshake") ||
		strings.Contains(msg, "session is not authenticated") ||
		strings.Contains(msg, "invalid login") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "context canceled")
}

// truncateErr clips an error string to maxLen to avoid unbounded last_error values.
func truncateErr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
