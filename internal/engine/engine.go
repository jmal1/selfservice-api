package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
)

// Engine is the workflow run orchestrator. It watches for pending runs,
// provisions K8s Job runners, and manages the run lifecycle.
type Engine struct {
	queries  *Queries
	nats     *events.Client
	engineID string
	logger   *slog.Logger
}

// New creates a new Engine instance.
func New(queries *Queries, natsClient *events.Client, engineID string, logger *slog.Logger) *Engine {
	return &Engine{
		queries:  queries,
		nats:     natsClient,
		engineID: engineID,
		logger:   logger,
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

	// TODO: Provision K8s Job runner on k3sv03
	// For now, log what would happen
	e.logger.Info("would provision K8s Job runner",
		"run_id", run.ID,
		"pod_id", run.PodID,
		"workflow_count", len(workflows),
		"callback_token", run.CallbackToken[:8]+"...",
	)

	// The actual execution happens asynchronously:
	// 1. Engine creates K8s Job + Secret + NAD
	// 2. Runner pod starts, executes workflows, POSTs results to callback
	// 3. Callback handler (in Phase 4) writes results to DB
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

		// TODO: Clean up K8s resources (delete Job, Secret)

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
