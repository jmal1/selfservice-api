package engine

import (
	"context"
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
	queries   *Queries
	nats      *events.Client
	k8s       *K8sClient
	engineID  string
	engineURL string
	logger    *slog.Logger
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

	// Provision K8s Job runner on k3sv03
	if e.k8s != nil {
		// Resolve VLAN tag for this pod (stored in pod record)
		vlanTag, err := e.queries.GetPodVLANTag(ctx, run.PodID)
		if err != nil {
			return fmt.Errorf("get pod VLAN tag: %w", err)
		}

		// Build WorkflowDefs from loaded workflows
		wfDefs := make([]runner.WorkflowDef, len(workflows))
		for i, wf := range workflows {
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
		target, pod, err := e.queries.GetRunTargetInfo(ctx, run.PodID)
		if err != nil {
			return fmt.Errorf("get target info: %w", err)
		}

		result, err := e.k8s.ProvisionRunner(ctx,
			run.ID.String(),
			run.CallbackToken,
			vlanTag,
			wfDefs,
			target,
			pod,
			e.engineURL,
		)
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

// createVersionSnapshot creates an immutable snapshot of a workflow.
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
		e.logger.Warn("timeout watchdog: marking run as timed out",
			"run_id", run.ID,
			"status", run.Status,
		)

		// Clean up K8s resources (delete Job, Secret)
		if e.k8s != nil && run.RunnerVMName != nil {
			_ = e.k8s.CleanupRunner(ctx, *run.RunnerVMName, *run.RunnerVMName+"-config")
		}

		errMsg := "Run timed out after 10 minutes"
		if err := e.queries.UpdateRunStatus(ctx, run.ID, models.RunStatusTimeout, &errMsg); err != nil {
			e.logger.Error("timeout watchdog: failed to update run", "run_id", run.ID, "error", err)
		}

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
