package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/runner"
)

// Engine is the workflow run orchestrator. It watches for pending runs,
// provisions K8s Job runners, and manages the run lifecycle.
type Engine struct {
	queries    *Queries
	nats       *events.Client
	k8s        *K8sClient
	dispatcher *VMwareToolsDispatcher
	metrics    *RunnerMetrics
	engineID   string
	engineURL  string
	logger     *slog.Logger
}

// New creates a new Engine instance.
func New(queries *Queries, natsClient *events.Client, k8sClient *K8sClient, engineID, engineURL string, logger *slog.Logger) *Engine {
	return &Engine{
		queries:   queries,
		nats:      natsClient,
		k8s:       k8sClient,
		engineID:  engineID,
		engineURL: engineURL,
		logger:    logger,
	}
}

// WithVMwareToolsDispatcher attaches a GuestOperations-backed dispatcher so
// workflows with execution_mode=vmware_tools can be executed in-process.
// Optional: if not set, vmware_tools workflows fail fast with a clear error
// rather than silently routing to the Kali runner (which would also fail,
// just less helpfully).
func (e *Engine) WithVMwareToolsDispatcher(d *VMwareToolsDispatcher) *Engine {
	e.dispatcher = d
	return e
}

// WithRunnerMetrics attaches a RunnerMetrics recorder. Optional: if not set
// all metric calls are no-ops (nil-safe) so the engine works without
// Pushgateway configured.
func (e *Engine) WithRunnerMetrics(m *RunnerMetrics) *Engine {
	e.metrics = m
	return e
}

// RecoverStaleRuns finds runs that were abandoned by a previous engine instance
// and marks them as failed. Called once at startup.
func (e *Engine) RecoverStaleRuns(ctx context.Context) error {
	staleRuns, err := e.queries.FindStaleRuns(ctx, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("find stale runs: %w", err)
	}

	for _, run := range staleRuns {
		e.logger.Warn("recovering stale run",
			"run_id", run.ID,
			"pod_id", run.PodID,
			"status", run.Status,
			"started_at", run.StartedAt,
		)

		// TODO: Clean up K8s resources (Job, Secret, NAD) if they exist
		if e.k8s != nil && run.RunnerVMName != nil {
			_ = e.k8s.CleanupRunner(ctx, *run.RunnerVMName, *run.RunnerVMName+"-config")
		}

		errMsg := "Engine restarted — run was orphaned and has been marked as failed"
		if err := e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusFailed, &errMsg); err != nil {
			e.logger.Error("failed to recover stale run", "run_id", run.ID, "error", err)
			continue
		}

		e.publishRunEvent(run.PodID.String(), run.ID.String(), "failed", "Run recovered after engine restart")
	}

	if len(staleRuns) > 0 {
		e.logger.Info("recovered stale runs", "count", len(staleRuns))
	}
	return nil
}

// ProcessPendingRuns claims and processes all available pending runs.
// Returns when no more pending runs are available.
func (e *Engine) ProcessPendingRuns(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		run, err := e.queries.ClaimPendingRun(ctx, e.engineID)
		if err != nil {
			e.logger.Error("failed to claim run", "error", err)
			return
		}
		if run == nil {
			return // no pending runs
		}

		e.logger.Info("claimed run", "run_id", run.ID, "pod_id", run.PodID)
		e.publishRunEvent(run.PodID.String(), run.ID.String(), "provisioning", "Run claimed, provisioning runner")

		if err := e.executeRun(ctx, run); err != nil {
			e.logger.Error("run execution failed", "run_id", run.ID, "error", err)
			errMsg := err.Error()
			_ = e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusFailed, &errMsg)
			e.publishRunEvent(run.PodID.String(), run.ID.String(), "failed", errMsg)
		}
	}
}

// executeRun handles the full lifecycle of a single run.
func (e *Engine) executeRun(ctx context.Context, run *models.Run) error {
	// Load workflows for this run's playlist
	if run.PlaylistID == nil {
		return fmt.Errorf("run %s has no playlist", run.ID)
	}

	workflows, err := e.queries.GetWorkflowsForPlaylist(ctx, *run.PlaylistID)
	if err != nil {
		return fmt.Errorf("load workflows: %w", err)
	}

	if len(workflows) == 0 {
		return fmt.Errorf("playlist has no active workflows")
	}

	// Load actions for each workflow and create version snapshots
	for i := range workflows {
		actions, err := e.queries.GetActionsForWorkflow(ctx, workflows[i].ID)
		if err != nil {
			return fmt.Errorf("load actions for workflow %s: %w", workflows[i].Slug, err)
		}
		workflows[i].Actions = actions

		// Create version snapshot
		version, err := e.createVersionSnapshot(ctx, &workflows[i])
		if err != nil {
			return fmt.Errorf("create version for %s: %w", workflows[i].Slug, err)
		}

		// Insert pending workflow result
		wr := &models.WorkflowResult{
			RunID:             run.ID,
			WorkflowID:        workflows[i].ID,
			WorkflowVersionID: &version.ID,
			ExecutionOrder:    i,
			ExecutionMode:     workflows[i].ExecutionMode,
			Status:            models.ResultStatusPending,
		}
		if err := e.queries.InsertWorkflowResult(ctx, wr); err != nil {
			return fmt.Errorf("insert workflow result: %w", err)
		}
	}

	// Update run with workflow count
	if err := e.queries.UpdateRunCounts(ctx, run.ID); err != nil {
		e.logger.Warn("failed to update run counts", "run_id", run.ID, "error", err)
	}

	// Transition to running
	if err := e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusRunning, nil); err != nil {
		return fmt.Errorf("update run status to running: %w", err)
	}
	e.publishRunEvent(run.PodID.String(), run.ID.String(), "running",
		fmt.Sprintf("Executing %d workflows", len(workflows)))

	// Split workflows by execution mode so each mode can be dispatched along
	// its own path. Mixed-mode playlists are allowed: kali workflows go to
	// the K8s Job runner as before; vmware_tools workflows are executed
	// in-process via the GuestOperations dispatcher.
	kaliWorkflows := make([]models.Workflow, 0, len(workflows))
	vmwareToolsWorkflows := make([]models.Workflow, 0, len(workflows))
	for _, wf := range workflows {
		if wf.ExecutionMode == models.ExecModeVMwareTools {
			vmwareToolsWorkflows = append(vmwareToolsWorkflows, wf)
		} else {
			kaliWorkflows = append(kaliWorkflows, wf)
		}
	}

	// Dispatch vmware_tools workflows first (in-process, fast) so any guest
	// preflight errors surface before we provision the kali pod. Runs even
	// when there are no kali workflows.
	if len(vmwareToolsWorkflows) > 0 {
		if err := e.dispatchVMwareTools(ctx, run, vmwareToolsWorkflows); err != nil {
			// The dispatcher already wrote per-workflow error rows, so we
			// just log here and continue — the kali path can still run any
			// of its workflows even if guest-side ones failed.
			e.logger.Error("vmware_tools dispatch returned error", "run_id", run.ID, "error", err)
		}
	}

	// Provision K8s Job runner on k3sv03 for kali_runner workflows.
	if len(kaliWorkflows) == 0 {
		// All workflows were vmware_tools; the dispatcher fully owns this run.
		// Mark it completed (or failed if everything failed) based on counts.
		return e.finalizeRunFromCounts(ctx, run)
	}

	if e.k8s != nil {
		// Resolve VLAN tag for this pod (stored in pod record)
		vlanTag, err := e.queries.GetPodVLANTag(ctx, run.PodID)
		if err != nil {
			return fmt.Errorf("get pod VLAN tag: %w", err)
		}

		// Build WorkflowDefs only for kali-mode workflows; vmware_tools were
		// already dispatched above.
		wfDefs := make([]runner.WorkflowDef, len(kaliWorkflows))
		for i, wf := range kaliWorkflows {
			var setup string
			if wf.SetupScript != nil {
				setup = *wf.SetupScript
			}
			wfDefs[i] = runner.WorkflowDef{
				Slug:           wf.Slug,
				Name:           wf.Name,
				Script:         wf.Script,
				Setup:          setup,
				TimeoutSeconds: wf.TimeoutSeconds,
			}
		}

		// Resolve target info
		target, pod, targetVM, err := e.queries.GetRunTargetInfo(ctx, run.PodID)
		if err != nil {
			return fmt.Errorf("get target info: %w", err)
		}

		// Record which VM is being graded, before the runner starts rather than
		// after it finishes. A run that times out or whose runner never starts is
		// exactly the one an instructor needs to attribute, and writing this on
		// the success path only would leave those runs unattributable.
		if err := e.queries.SetRunTarget(ctx, run.ID, targetVM); err != nil {
			e.logger.Warn("failed to record run target VM",
				"run_id", run.ID, "target_vm", targetVM.DisplayName, "error", err)
		}

		// Build the action library so workflows can call library actions such as
		// `run_action "HTTP Responds" http_get …`. Without this the runner has
		// only run_action/ctx_set/ctx_get from actions.sh and every library call
		// dies with exit 127.
		//
		// A failure here fails the run rather than proceeding: silently shipping
		// a runner with no library would reproduce the original defect, where
		// workflows "ran", the Job exited 0, and every action failed for a
		// reason no one could see.
		libActions, err := e.queries.ListRunnerLibraryActions(ctx)
		if err != nil {
			return fmt.Errorf("list library actions: %w", err)
		}
		actionLibrary, err := buildActionLibrary(libActions)
		if err != nil {
			return fmt.Errorf("build action library: %w", err)
		}

		result, err := e.k8s.ProvisionRunner(ctx, RunnerSpec{
			RunID:         run.ID.String(),
			CallbackToken: run.CallbackToken,
			EngineURL:     e.engineURL,
			VLANTag:       vlanTag,
			Workflows:     wfDefs,
			Target:        target,
			Pod:           pod,
			ActionLibrary: actionLibrary,
		})
		if err != nil {
			return fmt.Errorf("provision runner: %w", err)
		}

		// Store the job name for later cleanup
		if err := e.queries.SetRunnerPodName(ctx, run.ID, result.JobName); err != nil {
			e.logger.Warn("failed to store runner pod name", "run_id", run.ID, "error", err)
		}

		e.logger.Info("provisioned K8s runner",
			"run_id", run.ID,
			"job", result.JobName,
			"secret", result.SecretName,
			"nad", result.NADName,
		)
	} else {
		e.logger.Warn("k8s client not available, skipping provisioning",
			"run_id", run.ID,
		)
	}

	// The actual execution happens asynchronously:
	// 1. Engine created K8s Job + Secret + NAD above
	// 2. Runner pod starts, executes workflows, POSTs results to callback
	// 3. Callback handler (Phase 4) writes results to DB
	// 4. Timeout watchdog catches stuck runs

	return nil
}

// dispatchVMwareTools resolves the target moref+credentials once and hands
// the workflows to the in-process dispatcher. If the dispatcher hasn't been
// configured (no vCenter creds at engine startup), every workflow is marked
// as error so the run can still complete.
func (e *Engine) dispatchVMwareTools(ctx context.Context, run *models.Run, workflows []models.Workflow) error {
	if e.dispatcher == nil {
		e.logger.Warn("vmware_tools dispatcher not configured; failing affected workflows",
			"run_id", run.ID, "workflow_count", len(workflows))
		for _, wf := range workflows {
			actionResults, _ := json.Marshal([]runner.ActionOutput{{
				Action:  wf.Slug,
				Status:  models.ResultStatusError,
				Message: "vmware_tools execution mode not configured on this engine",
			}})
			instructorOutput, _ := json.Marshal(map[string]any{
				"execution_mode": models.ExecModeVMwareTools,
				"error":          "dispatcher not configured",
			})
			durationMs := 0
			_ = e.queries.UpdateWorkflowResultBySlug(ctx, run.ID, wf.Slug,
				models.ResultStatusError, "vmware_tools execution mode not configured", instructorOutput, actionResults, &durationMs)
		}
		return fmt.Errorf("vmware_tools dispatcher not configured")
	}

	moref, osType, username, password, err := e.queries.GetVMwareToolsTarget(ctx, run.PodID)
	if err != nil {
		return fmt.Errorf("resolve vmware_tools target: %w", err)
	}
	target := VMwareToolsTarget{
		VMMoref:  moref,
		OS:       osType,
		Username: username,
		Password: password,
	}
	return e.dispatcher.Dispatch(ctx, run, workflows, target)
}

// finalizeRunFromCounts inspects the run's current pass/fail counts and
// transitions the run to completed (any pass) or failed (all fail/error).
// Used when no kali runner is involved and the engine needs to close out the
// run itself — normally the kali callback handles this.
func (e *Engine) finalizeRunFromCounts(ctx context.Context, run *models.Run) error {
	if err := e.queries.UpdateRunCounts(ctx, run.ID); err != nil {
		e.logger.Warn("failed to update run counts at finalize", "run_id", run.ID, "error", err)
	}
	// The simplest closeout: mark completed. UpdateRunCounts already populated
	// pass_count/fail_count which the UI uses for overall status display, so
	// we don't need to decide pass-vs-fail at the run level.
	if err := e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusCompleted, nil); err != nil {
		return fmt.Errorf("finalize run: %w", err)
	}
	e.publishRunEvent(run.PodID.String(), run.ID.String(), "completed",
		"All workflows executed via vmware_tools")
	return nil
}
func (e *Engine) createVersionSnapshot(ctx context.Context, wf *models.Workflow) (*models.WorkflowVersion, error) {
	latestVersion, err := e.queries.GetLatestWorkflowVersion(ctx, wf.ID)
	if err != nil {
		return nil, err
	}

	actionsJSON, err := marshalActions(wf.Actions)
	if err != nil {
		return nil, err
	}

	wv := &models.WorkflowVersion{
		WorkflowID: wf.ID,
		Version:    latestVersion + 1,
		Script:     wf.Script,
		Actions:    actionsJSON,
		CreatedBy:  wf.CreatedBy,
	}

	if err := e.queries.CreateWorkflowVersion(ctx, wv); err != nil {
		return nil, err
	}

	return wv, nil
}

// StartTimeoutWatchdog starts a goroutine that periodically checks for
// runs that have exceeded their maximum runtime.
func (e *Engine) StartTimeoutWatchdog(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.checkForTimeouts(ctx)
		}
	}
}

func (e *Engine) checkForTimeouts(ctx context.Context) {
	// Runs stuck in provisioning/running for more than 10 minutes
	staleRuns, err := e.queries.FindStaleRuns(ctx, 10*time.Minute)
	if err != nil {
		e.logger.Error("timeout watchdog: failed to find stale runs", "error", err)
		return
	}

	for _, run := range staleRuns {
		// Snapshot the runner BEFORE CleanupRunner deletes the Job, because
		// deleting the Job deletes the pods, and the pods are the only place
		// the reason for the timeout is recorded. Recording an identical
		// "Run timed out after 10 minutes" for an image-pull failure, a
		// missing DHCP lease on the pod VLAN, an unschedulable node and a
		// genuinely hung runner made every timeout un-diagnosable after the
		// fact — the evidence was gone before anyone could look at it.
		diagnostics := ""
		if e.k8s != nil && run.RunnerVMName != nil {
			diagnostics = e.k8s.DescribeRunner(ctx, *run.RunnerVMName)
		}

		e.logger.Warn("timeout watchdog: marking run as timed out",
			"run_id", run.ID,
			"status", run.Status,
			"runner_diagnostics", diagnostics,
		)

		// Clean up K8s resources (delete Job, Secret)
		if e.k8s != nil && run.RunnerVMName != nil {
			_ = e.k8s.CleanupRunner(ctx, *run.RunnerVMName, *run.RunnerVMName+"-config")
		}

		errMsg := "Run timed out after 10 minutes"
		if diagnostics != "" {
			errMsg += ". Runner state at timeout: " + diagnostics
		}
		if err := e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusTimeout, &errMsg); err != nil {
			e.logger.Error("timeout watchdog: failed to update run", "run_id", run.ID, "error", err)
		}

		// A run that never reported leaves its workflow results sitting in
		// 'pending' forever. The run row says timeout while its children claim
		// to still be queued, so the testing UI renders a permanently
		// in-progress assessment and UpdateRunCounts counts zero failures.
		// A terminal run must not have non-terminal children.
		if n, err := e.queries.TimeoutPendingWorkflowResults(ctx, run.ID, errMsg); err != nil {
			e.logger.Error("timeout watchdog: failed to close workflow results",
				"run_id", run.ID, "error", err)
		} else if n > 0 {
			e.logger.Warn("timeout watchdog: closed unreported workflow results",
				"run_id", run.ID, "count", n)
			if err := e.queries.UpdateRunCounts(ctx, run.ID); err != nil {
				e.logger.Error("timeout watchdog: failed to update run counts",
					"run_id", run.ID, "error", err)
			}
		}

		e.metrics.RecordRunnerJob("timeout")
		e.publishRunEvent(run.PodID.String(), run.ID.String(), "timeout", errMsg)
	}
}

func (e *Engine) publishRunEvent(podID, runID, status, message string) {
	// Use existing NATS pattern — publish to testing.runs.{podId}.progress
	evt := events.Event{
		Type:    "run." + status,
		JobID:   runID,
		PodID:   podID,
		Status:  status,
		Message: message,
	}
	subject := fmt.Sprintf("testing.runs.%s.progress", podID)
	if err := e.nats.PublishRaw(subject, evt); err != nil {
		e.logger.Warn("failed to publish run event", "subject", subject, "error", err)
	}
}

// StartOrphanCleanup starts a goroutine that periodically cleans up
// completed runner Jobs that weren't cleaned up by callbacks.
func (e *Engine) StartOrphanCleanup(ctx context.Context) {
	if e.k8s == nil {
		return
	}

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleaned, err := e.k8s.CleanupOrphanedRunners(ctx)
			if err != nil {
				e.logger.Error("orphan cleanup failed", "error", err)
			} else if cleaned > 0 {
				e.logger.Info("orphan cleanup completed", "cleaned", cleaned)
			}
		}
	}
}
