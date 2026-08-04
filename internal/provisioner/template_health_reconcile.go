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
//   - 2-cycle confirmation: a template is only marked unhealthy after 2
//     consecutive failed cycles. A single bad 12-hour window never alerts.
//   - Immediate recovery: one passing cycle resets consecutive_failures to 0
//     and sets health_status='healthy'.
//   - Checker-level failures: if vCenter is unreachable or the checker itself
//     errors, crucible_template_health_checker_up is set to 0 and per-template
//     states are NOT modified. This means a vCenter outage fires exactly one
//     infrastructure alert, not N per-template alerts.
//
// # Deep-check cleanup
//
//   The deep check names its clone "crucible-healthcheck-<templateID>" and
//   places it in the Templates folder (not the Student-VMs folder, which the
//   orphan reconciler scans). A deferred destroy runs even when the power-on
//   or wait-for-IP step fails. A sweep at worker startup catches any clones
//   left behind by a crash.
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

// TemplateHealthCheckerUp tracks whether the last checker cycle completed
// without a vCenter-infrastructure-level failure.
const (
	checkerUp   = float64(1)
	checkerDown = float64(0)
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

	// Pusher, if set, receives metrics after each cycle.
	Pusher *TemplateHealthPusher
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
	GetLeastRecentlyDeepCheckedTemplate(ctx context.Context) (*models.Template, error)
	GetTemplateHealthState(ctx context.Context, templateID uuid.UUID) (*database.TemplateHealthState, error)
	UpsertTemplateHealthState(ctx context.Context, state database.TemplateHealthState) error
	GetNewestTemplateHealthCheckTime(ctx context.Context) (*time.Time, error)
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
	SetCheckerUp(up float64)
	RecordStructuralResult(templateName string, durationSec float64, healthy bool)
	RecordDeepResult(templateName string, durationSec float64, healthy bool)
	SetHealthStatus(templateName, checkType string, healthy float64)
	SetLastCheckTimestamp(templateName string, unixSec float64)
	Push(ctx context.Context) error
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
	newest, err := db.GetNewestTemplateHealthCheckTime(ctx)
	if err != nil {
		return false, fmt.Errorf("read last template health check time: %w", err)
	}
	if newest == nil {
		return true, nil
	}
	return time.Since(*newest) >= interval, nil
}

// ReconcileTemplateHealth is the Provisioner-bound entry point. The provision
// worker calls this from its select loop.
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
func reconcileTemplateHealth(
	ctx context.Context,
	db templateHealthDB,
	vc templateHealthVCenter,
	metrics templateHealthMetrics,
	logger *slog.Logger,
	cfg TemplateHealthReconcilerConfig,
) (TemplateHealthCounts, error) {
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
		cfg.RetryBaseDelay = 1 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "template_health_reconciler")

	// --- Step 1: list student-visible templates --------------------------------
	templates, err := db.ListStudentVisibleTemplates(ctx)
	if err != nil {
		// DB is unavailable → checker-level failure.
		log.Error("list student-visible templates failed", "error", err)
		if metrics != nil {
			metrics.SetCheckerUp(checkerDown)
			_ = metrics.Push(ctx)
		}
		return TemplateHealthCounts{CheckerUp: false}, fmt.Errorf("list student-visible templates: %w", err)
	}

	counts := TemplateHealthCounts{
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
				if metrics != nil {
					metrics.SetCheckerUp(checkerDown)
					_ = metrics.Push(ctx)
				}
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

		now := time.Now()
		state, stateErr := db.GetTemplateHealthState(ctx, tmpl.ID)
		if stateErr != nil {
			log.Warn("get template health state failed; using zero state",
				"template_id", tmpl.ID, "error", stateErr)
		}
		if state == nil {
			state = &database.TemplateHealthState{
				TemplateID:   tmpl.ID,
				TemplateName: tmpl.Name,
				HealthStatus: "unknown",
			}
		}
		state.LastStructuralCheckAt = &now

		passed := checkErr == nil
		if passed {
			state.ConsecutiveFailures = 0
			state.HealthStatus = "healthy"
			state.LastError = nil
			counts.Healthy++
		} else {
			state.ConsecutiveFailures++
			counts.NewlyFailing++
			errMsg := "structural check failed"
			if checkErr != nil {
				errMsg = truncateErr(checkErr.Error(), 256)
			}
			state.LastError = &errMsg

			// Only mark unhealthy after 2 consecutive failed cycles.
			if state.ConsecutiveFailures >= 2 {
				state.HealthStatus = "unhealthy"
				counts.Unhealthy++
			}
		}

		if err := db.UpsertTemplateHealthState(ctx, *state); err != nil {
			log.Warn("upsert template health state failed",
				"template_id", tmpl.ID, "error", err)
		}

		if metrics != nil {
			healthVal := float64(0)
			if passed {
				healthVal = 1
			}
			metrics.RecordStructuralResult(tmpl.Name, durSec, passed)
			metrics.SetHealthStatus(tmpl.Name, "structural", healthVal)
			metrics.SetLastCheckTimestamp(tmpl.Name, float64(now.Unix()))
		}

		log.Info("structural check complete",
			"template", tmpl.Name,
			"ref", ref,
			"passed", passed,
			"consecutive_failures", state.ConsecutiveFailures,
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
			deepErr := runDeepCheck(ctx, vc, deepTarget, cfg, log)
			durSec := time.Since(start).Seconds()
			counts.DeepChecked = true

			now := time.Now()
			state, stateErr := db.GetTemplateHealthState(ctx, deepTarget.ID)
			if stateErr != nil {
				log.Warn("get deep-check state failed; using zero state",
					"template_id", deepTarget.ID, "error", stateErr)
			}
			if state == nil {
				state = &database.TemplateHealthState{
					TemplateID:   deepTarget.ID,
					TemplateName: deepTarget.Name,
					HealthStatus: "unknown",
				}
			}
			state.LastDeepCheckAt = &now

			if deepErr == nil {
				// Deep check passed — reset failure state immediately.
				state.ConsecutiveFailures = 0
				state.HealthStatus = "healthy"
				state.LastError = nil
			} else {
				state.ConsecutiveFailures++
				errMsg := truncateErr(deepErr.Error(), 256)
				state.LastError = &errMsg
				if state.ConsecutiveFailures >= 2 {
					state.HealthStatus = "unhealthy"
				}
			}

			if err := db.UpsertTemplateHealthState(ctx, *state); err != nil {
				log.Warn("upsert deep-check state failed",
					"template_id", deepTarget.ID, "error", err)
			}

			if metrics != nil {
				healthVal := float64(0)
				if deepErr == nil {
					healthVal = 1
				}
				metrics.RecordDeepResult(deepTarget.Name, durSec, deepErr == nil)
				metrics.SetHealthStatus(deepTarget.Name, "deep", healthVal)
				metrics.SetLastCheckTimestamp(deepTarget.Name, float64(now.Unix()))
			}

			log.Info("deep check complete",
				"template", deepTarget.Name,
				"passed", deepErr == nil,
				"duration_ms", int(durSec*1000),
			)
		}
	}

	// --- Step 4: push metrics -------------------------------------------------
	if metrics != nil {
		metrics.SetCheckerUp(checkerUp)
		if err := metrics.Push(ctx); err != nil {
			log.Warn("template health metrics push failed", "error", err)
		}
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
	cloneName := vcenter.HealthCheckClonePrefix + tmpl.ID.String()
	ref := tmpl.VCenterRef()

	deepCtx, cancel := context.WithTimeout(ctx, cfg.DeepCheckTimeout)
	defer cancel()

	params := vcenter.HealthCheckCloneParams{
		SourceRef:    ref,
		CloneName:    cloneName,
		FolderPath:   cfg.TemplateFolder,
		Network:      cfg.Network,
		VCPUs:        int32(tmpl.DefaultVCPUs),
		RAMmb:        int64(tmpl.DefaultRAMMB),
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
