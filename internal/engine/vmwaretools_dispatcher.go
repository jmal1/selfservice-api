// Package engine.
//
// vmwaretools_dispatcher.go drives Phase 7 (`vmware_tools` execution mode).
// Unlike the kali_runner path — which provisions a K8s pod that POSTs results
// back via the callback HTTP server — the vmware_tools dispatcher runs in-process:
// for each workflow it uploads the script straight to the target guest VM via
// govmomi GuestOperations, polls until exit-or-timeout, and writes the result
// directly to the database using the same Queries the callback server uses.
//
// This keeps the engine the sole source of truth about run state regardless of
// which execution mode a given workflow uses. A run may freely mix kali_runner
// and vmware_tools workflows; engine.executeRun splits by ExecutionMode and
// dispatches each subset down the appropriate path. Final pass/fail accounting
// happens via the shared queries.UpdateRunCounts call.
//
// v1 limitation: each vmware_tools workflow is treated as a single logical
// action. The full multi-action protocol used by the Kali runner (Unix-socket
// run_action events, per-action callbacks) is not exposed inside the guest VM;
// instructors who need fine-grained action breakdowns should keep using the
// kali_runner mode or wait for v2 which will upload actions.sh into the guest.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-crucible-runner/runner"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// GuestExecutor is the narrow interface the dispatcher needs from a vcenter
// client. Keeping it as an interface lets tests substitute a mock without
// pulling in govmomi or vcsim.
type GuestExecutor interface {
	RunScriptInGuest(ctx context.Context, req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error)
}

// DispatcherQueries is the subset of *Queries that the dispatcher writes to.
// Promoted to an interface so unit tests can record calls instead of needing
// a real Postgres pool. Method signatures mirror the real Queries methods
// exactly (including []byte for JSON payloads — json.RawMessage and []byte
// are not interchangeable for Go interface satisfaction).
type DispatcherQueries interface {
	UpdateWorkflowResultBySlug(ctx context.Context, runID uuid.UUID, workflowSlug, status, message string, instructorOutput, actionResults []byte, durationMs *int) error
	UpdateRunCounts(ctx context.Context, runID uuid.UUID) error
}

// EventPublisher abstracts NATS so tests can capture published events.
type EventPublisher interface {
	publishRunEvent(podID, runID, eventType, message string)
}

// VMwareToolsTarget describes everything the dispatcher needs about the
// target VM. The engine resolves this once per run before dispatching.
type VMwareToolsTarget struct {
	VMMoref  string
	OS       string // "linux" or "windows"; selects shell language
	Username string
	Password string
}

// VMwareToolsDispatcher runs vmware_tools-mode workflows directly against
// the guest VM. One instance per engine; safe to call concurrently across
// different runs (each call carries its own ctx/run/target).
type VMwareToolsDispatcher struct {
	executor  GuestExecutor
	queries   DispatcherQueries
	publisher EventPublisher
	logger    *slog.Logger
}

// NewVMwareToolsDispatcher constructs a dispatcher. All deps are required;
// pass nil for `publisher` and event publication is silently skipped (useful
// for tests).
func NewVMwareToolsDispatcher(executor GuestExecutor, queries DispatcherQueries, publisher EventPublisher, logger *slog.Logger) *VMwareToolsDispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &VMwareToolsDispatcher{
		executor:  executor,
		queries:   queries,
		publisher: publisher,
		logger:    logger,
	}
}

// Dispatch runs each provided workflow against the target VM, writing per-
// workflow results as they complete. The error return is non-nil only when
// the dispatcher itself fails to make progress (e.g. queries layer down);
// individual workflow failures are recorded as ResultStatusFail/Error in the
// database and do NOT abort the loop.
//
// Caller responsibility: filter workflows to only those with
// ExecutionMode == ExecModeVMwareTools before invoking.
func (d *VMwareToolsDispatcher) Dispatch(ctx context.Context, run *models.Run, workflows []models.Workflow, target VMwareToolsTarget) error {
	if d == nil {
		return errors.New("dispatcher is nil")
	}
	if d.executor == nil {
		return errors.New("guest executor not configured")
	}
	if d.queries == nil {
		return errors.New("queries not configured")
	}
	if run == nil {
		return errors.New("run is nil")
	}
	if err := validateTarget(target); err != nil {
		// Mark every workflow as error so the run can still complete.
		for i := range workflows {
			d.recordError(ctx, run, workflows[i].Slug, fmt.Sprintf("target invalid: %v", err))
		}
		return fmt.Errorf("validate target: %w", err)
	}

	d.logf(run, "vmware_tools dispatcher starting", "workflow_count", len(workflows), "vm_moref", target.VMMoref)

	for i := range workflows {
		// Honor context cancellation between workflows so a cancelled run
		// doesn't keep firing GuestOps RPCs.
		if err := ctx.Err(); err != nil {
			for j := i; j < len(workflows); j++ {
				d.recordCancelled(ctx, run, workflows[j].Slug)
			}
			return ctx.Err()
		}
		d.runOne(ctx, run, workflows[i], target)
	}

	// Counts update is best-effort: if it fails the next workflow callback
	// or the watchdog will refresh it. Don't fail the run for this.
	if err := d.queries.UpdateRunCounts(ctx, run.ID); err != nil {
		d.logf(run, "update run counts after dispatch failed", "error", err)
	}

	return nil
}

// runOne executes a single workflow and persists its result. All failures
// are converted to a recorded WorkflowRunResult — runOne never returns an
// error so the outer Dispatch loop is guaranteed to make progress.
func (d *VMwareToolsDispatcher) runOne(ctx context.Context, run *models.Run, wf models.Workflow, target VMwareToolsTarget) {
	start := time.Now()
	timeout := workflowTimeout(wf.TimeoutSeconds)
	lang := languageForOS(target.OS, wf.GuestInterpreter)

	req := vcenter.GuestExecRequest{
		VMMoref:        target.VMMoref,
		GuestUser:      target.Username,
		GuestPassword:  target.Password,
		Language:       lang,
		Script:         wf.Script,
		RunID:          run.ID.String(),
		ActionSlug:     wf.Slug,
		Timeout:        timeout,
	}

	d.publish(run, "workflow_start", fmt.Sprintf("%s: vmware_tools dispatch", wf.Slug))

	result, err := d.executor.RunScriptInGuest(ctx, req)
	duration := time.Since(start)

	if err != nil {
		d.recordError(ctx, run, wf.Slug, fmt.Sprintf("guest exec failed: %v", err))
		d.publish(run, "workflow_complete", fmt.Sprintf("%s: error", wf.Slug))
		return
	}

	status, message := classifyGuestResult(result)
	d.recordWorkflowResult(ctx, run, wf, status, message, result, duration)
	d.publish(run, "workflow_complete", fmt.Sprintf("%s: %s", wf.Slug, status))
}

// recordWorkflowResult writes the final WorkflowRunResult to the DB using
// the same JSON shape the callback HTTP path uses, so the admin UI doesn't
// need to know which path produced a given row.
func (d *VMwareToolsDispatcher) recordWorkflowResult(ctx context.Context, run *models.Run, wf models.Workflow, status, message string, guest *vcenter.GuestExecResult, duration time.Duration) {
	exitCode := 0
	stdout := ""
	stderr := ""
	timedOut := false
	truncated := false
	if guest != nil {
		exitCode = guest.ExitCode
		stdout = guest.Stdout
		stderr = guest.Stderr
		timedOut = guest.TimedOut
		truncated = guest.TruncatedOut
	}

	action := runner.ActionOutput{
		Action:   wf.Slug,
		Status:   status,
		Message:  message,
		ExitCode: exitCode,
		Duration: runner.FromDuration(duration),
	}

	if ctx, ok := buildActionContext(stdout, stderr, timedOut, truncated); ok {
		action.Context = ctx
	}

	actionResults, _ := json.Marshal([]runner.ActionOutput{action})
	instructorOutput, _ := json.Marshal(map[string]any{
		"execution_mode": models.ExecModeVMwareTools,
		"vm_moref":       d.morefForLogging(run, wf),
		"exit_code":      exitCode,
		"timed_out":      timedOut,
		"truncated":      truncated,
		"stdout_bytes":   len(stdout),
		"stderr_bytes":   len(stderr),
	})

	durationMs := int(duration.Milliseconds())
	if err := d.queries.UpdateWorkflowResultBySlug(ctx, run.ID, wf.Slug, status, message, instructorOutput, actionResults, &durationMs); err != nil {
		d.logf(run, "failed to persist vmware_tools workflow result", "workflow", wf.Slug, "error", err)
	}
}

// morefForLogging is a hook so tests can substitute a deterministic value;
// in prod it just returns the workflow's TargetVM field if set, otherwise "".
func (d *VMwareToolsDispatcher) morefForLogging(_ *models.Run, wf models.Workflow) string {
	if wf.TargetVM != nil {
		return *wf.TargetVM
	}
	return ""
}

// recordError persists a workflow as ResultStatusError with the given message
// and zero stdout/stderr. Used both for guest-exec failures and for cases
// where we never even got to invoke the guest (bad target, etc).
func (d *VMwareToolsDispatcher) recordError(ctx context.Context, run *models.Run, slug, message string) {
	action := runner.ActionOutput{
		Action:  slug,
		Status:  models.ResultStatusError,
		Message: message,
	}
	actionResults, _ := json.Marshal([]runner.ActionOutput{action})
	instructorOutput, _ := json.Marshal(map[string]any{
		"execution_mode": models.ExecModeVMwareTools,
		"error":          message,
	})
	durationMs := 0
	if err := d.queries.UpdateWorkflowResultBySlug(ctx, run.ID, slug, models.ResultStatusError, message, instructorOutput, actionResults, &durationMs); err != nil {
		d.logf(run, "failed to persist vmware_tools error result", "workflow", slug, "error", err)
	}
}

// recordCancelled is used when the outer ctx was cancelled before we got to
// a particular workflow; we still want a row so the run aggregation is sane.
func (d *VMwareToolsDispatcher) recordCancelled(ctx context.Context, run *models.Run, slug string) {
	action := runner.ActionOutput{
		Action:  slug,
		Status:  models.ResultStatusCancelled,
		Message: "run cancelled before vmware_tools workflow executed",
	}
	actionResults, _ := json.Marshal([]runner.ActionOutput{action})
	instructorOutput, _ := json.Marshal(map[string]any{
		"execution_mode": models.ExecModeVMwareTools,
		"cancelled":      true,
	})
	durationMs := 0
	// Cancelled ctx can't be used for the DB write; use a short fresh ctx.
	writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.queries.UpdateWorkflowResultBySlug(writeCtx, run.ID, slug, models.ResultStatusCancelled, "run cancelled", instructorOutput, actionResults, &durationMs); err != nil {
		d.logf(run, "failed to persist vmware_tools cancelled result", "workflow", slug, "error", err)
	}
}

func (d *VMwareToolsDispatcher) publish(run *models.Run, eventType, message string) {
	if d.publisher == nil || run == nil {
		return
	}
	d.publisher.publishRunEvent(run.PodID.String(), run.ID.String(), eventType, message)
}

func (d *VMwareToolsDispatcher) logf(run *models.Run, msg string, kv ...any) {
	if d.logger == nil {
		return
	}
	args := []any{"run_id"}
	if run != nil {
		args = append(args, run.ID)
	} else {
		args = append(args, "<nil>")
	}
	args = append(args, kv...)
	d.logger.Info(msg, args...)
}

// classifyGuestResult turns the raw GuestExecResult into a workflow status
// + student-safe message. Exit 0 → pass; non-zero exit → fail; timed-out →
// timeout. Stderr is preferred over stdout for the message because that's
// where bash convention puts error text.
func classifyGuestResult(g *vcenter.GuestExecResult) (string, string) {
	if g == nil {
		return models.ResultStatusError, "no result from guest"
	}
	if g.TimedOut {
		return models.ResultStatusTimeout, fmt.Sprintf("script timed out after %s", g.Duration.Round(time.Second))
	}
	if g.ExitCode == 0 {
		msg := lastNonEmptyLine(g.Stdout)
		if msg == "" {
			msg = "script exited cleanly"
		}
		return models.ResultStatusPass, msg
	}
	msg := lastNonEmptyLine(g.Stderr)
	if msg == "" {
		msg = lastNonEmptyLine(g.Stdout)
	}
	if msg == "" {
		msg = fmt.Sprintf("script exited with code %d", g.ExitCode)
	}
	return models.ResultStatusFail, msg
}

// buildActionContext serializes the guest output buffers into an
// ActionOutput.Context JSON blob, mirroring the kali runner's convention so
// admin UI doesn't need special-casing per mode.
func buildActionContext(stdout, stderr string, timedOut, truncated bool) (json.RawMessage, bool) {
	if stdout == "" && stderr == "" && !timedOut && !truncated {
		return nil, false
	}
	blob, err := json.Marshal(map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"timed_out": timedOut,
		"truncated": truncated,
	})
	if err != nil {
		return nil, false
	}
	return blob, true
}

// lastNonEmptyLine returns the trailing non-blank line from buf. Useful for
// turning a multi-line script output into a one-line student message that
// reads naturally in the UI.
func lastNonEmptyLine(buf string) string {
	if buf == "" {
		return ""
	}
	end := len(buf)
	for end > 0 {
		start := end - 1
		for start > 0 && buf[start-1] != '\n' {
			start--
		}
		line := buf[start:end]
		// trim trailing \r and \n
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		// trim leading whitespace
		i := 0
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		line = line[i:]
		if line != "" {
			return line
		}
		end = start
		if end > 0 && buf[end-1] == '\n' {
			end--
		}
		if end > 0 && buf[end-1] == '\r' {
			end--
		}
	}
	return ""
}

// languageForOS picks the GuestExec language string. Explicit
// GuestInterpreter on the workflow wins; otherwise we map OS to a default.
// Anything unrecognized falls back to bash since most images we register
// are Linux.
func languageForOS(os string, override *string) string {
	if override != nil && *override != "" {
		return *override
	}
	switch normalizeOS(os) {
	case "windows":
		return "powershell"
	default:
		return "bash"
	}
}

func normalizeOS(s string) string {
	switch s {
	case "windows", "Windows", "WINDOWS":
		return "windows"
	default:
		return "linux"
	}
}

// workflowTimeout converts the workflow's timeout_seconds into a Duration,
// applying a sane default when zero. The hard cap is enforced inside
// vcenter.GuestOps (15 min), so we just need to avoid sending 0 which
// GuestOps would also default but it's clearer to do it here.
func workflowTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(seconds) * time.Second
}

func validateTarget(t VMwareToolsTarget) error {
	if t.VMMoref == "" {
		return errors.New("VMMoref is required")
	}
	if t.Username == "" {
		return errors.New("Username is required")
	}
	if t.Password == "" {
		return errors.New("Password is required")
	}
	return nil
}
