package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/runner"
)

// CallbackServer handles HTTP callbacks from runner pods.
// It validates callback tokens, stores results, and publishes NATS events.
type CallbackServer struct {
	queries *Queries
	engine  *Engine
	logger  *slog.Logger
}

// NewCallbackServer creates a callback HTTP handler.
func NewCallbackServer(engine *Engine, logger *slog.Logger) *CallbackServer {
	return &CallbackServer{
		queries: engine.queries,
		engine:  engine,
		logger:  logger,
	}
}

// Router returns the chi router for callback endpoints.
func (s *CallbackServer) Router() http.Handler {
	r := chi.NewRouter()
	// Liveness probe used by the api-gateway /admin/health endpoint and by
	// k8s readiness checks. Unauthenticated by design — it never reveals
	// state, just returns 200 if the callback server's HTTP listener is
	// up. If the engine is wedged enough to crash this listener, this
	// will fail-closed and the dashboard will report the engine as down.
	r.Get("/healthz", s.handleHealthz)
	r.Route("/internal/callback/{token}", func(r chi.Router) {
		r.Use(s.validateToken)
		r.Post("/action", s.handleAction)
		r.Post("/workflow", s.handleWorkflow)
		r.Post("/complete", s.handleComplete)
		r.Post("/heartbeat", s.handleHeartbeat)
	})
	return r
}

// handleHealthz is the liveness probe. Returns {"status":"ok"} with the
// engine_id of the responding instance so /admin/health can show which
// pod replied.
func (s *CallbackServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	engineID := ""
	if s.engine != nil {
		engineID = s.engine.engineID
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"ok","engine_id":%q}`, engineID)))
}

// validateToken middleware checks the callback token against the runs table.
func (s *CallbackServer) validateToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		if s.queries == nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}

		run, err := s.queries.GetRunByCallbackToken(r.Context(), token)
		if err != nil || run == nil {
			s.logger.Warn("invalid callback token", "token", token[:min(8, len(token))]+"...")
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		// Store the run in context for handlers
		ctx := context.WithValue(r.Context(), ctxKeyRun, run)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type contextKey string

const ctxKeyRun contextKey = "run"

func getRun(r *http.Request) *models.Run {
	run, _ := r.Context().Value(ctxKeyRun).(*models.Run)
	return run
}

// handleAction receives an individual action result from the runner.
func (s *CallbackServer) handleAction(w http.ResponseWriter, r *http.Request) {
	cbResult := "success"
	defer func() { s.engine.metrics.RecordCallback("action", cbResult) }()

	run := getRun(r)

	var payload runner.CallbackActionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		cbResult = "error"
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	s.logger.Info("action callback",
		"run_id", run.ID,
		"workflow", payload.WorkflowSlug,
		"action", payload.Action.Action,
		"status", payload.Action.Status,
	)

	// Append action result to workflow_results.action_results JSONB
	if err := s.queries.AppendActionResult(r.Context(), run.ID, payload.WorkflowSlug, payload.Action); err != nil {
		s.logger.Error("failed to append action result", "error", err)
		cbResult = "error"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Publish NATS progress event
	s.engine.publishRunEvent(run.PodID.String(), run.ID.String(), "action_complete",
		fmt.Sprintf("%s: %s (%s)", payload.WorkflowSlug, payload.Action.Action, payload.Action.Status))

	w.WriteHeader(http.StatusOK)
}

// handleWorkflow receives a completed workflow result from the runner.
func (s *CallbackServer) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	cbResult := "success"
	defer func() { s.engine.metrics.RecordCallback("workflow", cbResult) }()

	run := getRun(r)

	var payload runner.CallbackWorkflowPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		cbResult = "error"
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	result := payload.Result
	s.logger.Info("workflow callback",
		"run_id", run.ID,
		"workflow", result.WorkflowSlug,
		"status", result.Status,
		"actions", len(result.ActionResults),
		"duration_ms", result.TotalDuration.Duration().Milliseconds(),
	)

	// Build instructor output
	instructorOutput, _ := json.Marshal(map[string]any{
		"action_count": len(result.ActionResults),
		"setup":        result.SetupOutput,
	})

	// Build action results JSON
	actionResults, _ := json.Marshal(result.ActionResults)

	// Update workflow result in DB
	durationMs := int(result.TotalDuration.Duration().Milliseconds())
	if err := s.queries.UpdateWorkflowResultBySlug(r.Context(), run.ID, result.WorkflowSlug,
		result.Status, result.Message, instructorOutput, actionResults, &durationMs); err != nil {
		s.logger.Error("failed to update workflow result", "error", err)
		cbResult = "error"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Update run pass/fail counts
	if err := s.queries.UpdateRunCounts(r.Context(), run.ID); err != nil {
		s.logger.Warn("failed to update run counts", "run_id", run.ID, "error", err)
	}

	// Publish NATS event
	s.engine.publishRunEvent(run.PodID.String(), run.ID.String(), "workflow_complete",
		fmt.Sprintf("%s: %s", result.WorkflowSlug, result.Status))

	w.WriteHeader(http.StatusOK)
}

// handleComplete receives the final run completion signal from the runner.
func (s *CallbackServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	cbResult := "success"
	defer func() { s.engine.metrics.RecordCallback("complete", cbResult) }()

	run := getRun(r)

	var payload runner.CallbackCompletePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		cbResult = "error"
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	s.logger.Info("run complete callback",
		"run_id", run.ID,
		"status", payload.Status,
		"workflows", len(payload.Results),
	)

	// Mark the run as completed
	finalStatus := models.RunStatusCompleted
	jobResult := "success"
	if payload.Status == "failed" {
		finalStatus = models.RunStatusFailed
		jobResult = "failed"
	}
	if err := s.queries.UpdateRunStatus(r.Context(), run.ID, finalStatus, nil); err != nil {
		s.logger.Error("failed to update run status", "error", err)
		cbResult = "error"
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.engine.metrics.RecordRunnerJob(jobResult)

	// Final count update
	if err := s.queries.UpdateRunCounts(r.Context(), run.ID); err != nil {
		s.logger.Warn("failed to update final run counts", "run_id", run.ID, "error", err)
	}

	// Cleanup K8s resources
	if s.engine.k8s != nil && run.RunnerVMName != nil {
		go func() {
			if err := s.engine.k8s.CleanupRunner(context.Background(), *run.RunnerVMName, *run.RunnerVMName+"-config"); err != nil {
				s.logger.Warn("cleanup failed", "run_id", run.ID, "error", err)
			}
		}()
	}

	// Publish NATS completion event
	s.engine.publishRunEvent(run.PodID.String(), run.ID.String(), "completed",
		fmt.Sprintf("Run %s — %d workflows", payload.Status, len(payload.Results)))

	w.WriteHeader(http.StatusOK)
}

// handleHeartbeat receives a liveness heartbeat from the runner.
func (s *CallbackServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	cbResult := "success"
	defer func() { s.engine.metrics.RecordCallback("heartbeat", cbResult) }()

	run := getRun(r)

	var payload runner.CallbackHeartbeatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		cbResult = "error"
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Update last_heartbeat_at in the runs table
	if err := s.queries.UpdateRunHeartbeat(r.Context(), run.ID, payload.Phase, payload.CurrentAction); err != nil {
		s.logger.Warn("failed to update heartbeat", "run_id", run.ID, "error", err)
	}

	w.WriteHeader(http.StatusOK)
}

// StartCallbackServer starts the HTTP server for runner callbacks.
func StartCallbackServer(ctx context.Context, eng *Engine, port int, logger *slog.Logger) *http.Server {
	cb := NewCallbackServer(eng, logger)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: cb.Router(),
	}

	go func() {
		logger.Info("callback server starting", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("callback server error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	return srv
}

// --- Additional DB queries for callbacks ---

// GetRunByCallbackToken finds a run by its callback token.
func (q *Queries) GetRunByCallbackToken(ctx context.Context, token string) (*models.Run, error) {
	var r models.Run
	err := q.pool.QueryRow(ctx, `
		SELECT id, pod_id, playlist_id, triggered_by, runner_vm_id, runner_vm_name,
		       callback_token, status, total_workflows, passed_workflows, failed_workflows,
		       error_message, started_at, completed_at, created_at, updated_at
		FROM runs WHERE callback_token = $1
	`, token).Scan(
		&r.ID, &r.PodID, &r.PlaylistID, &r.TriggeredBy,
		&r.RunnerVMID, &r.RunnerVMName, &r.CallbackToken, &r.Status,
		&r.TotalWorkflows, &r.PassedWorkflows, &r.FailedWorkflows,
		&r.ErrorMessage, &r.StartedAt, &r.CompletedAt,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("get run by token: %w", err)
	}
	return &r, nil
}

// AppendActionResult appends an action result to the workflow_results.action_results JSONB array.
func (q *Queries) AppendActionResult(ctx context.Context, runID uuid.UUID, workflowSlug string, action runner.ActionOutput) error {
	actionJSON, err := json.Marshal(action)
	if err != nil {
		return fmt.Errorf("marshal action: %w", err)
	}

	_, err = q.pool.Exec(ctx, `
		UPDATE workflow_results
		SET action_results = COALESCE(action_results, '[]'::jsonb) || $3::jsonb,
		    status = 'running',
		    started_at = COALESCE(started_at, NOW())
		WHERE run_id = $1
		  AND workflow_id = (SELECT id FROM workflows WHERE slug = $2 LIMIT 1)
	`, runID, workflowSlug, actionJSON)
	return err
}

// UpdateWorkflowResultBySlug updates a workflow result identified by run_id + workflow slug.
func (q *Queries) UpdateWorkflowResultBySlug(ctx context.Context, runID uuid.UUID, workflowSlug, status, message string,
	instructorOutput, actionResults []byte, durationMs *int) error {

	now := time.Now()
	studentMsg := &message
	if message == "" {
		studentMsg = nil
	}

	_, err := q.pool.Exec(ctx, `
		UPDATE workflow_results
		SET status = $3, student_message = $4, instructor_output = $5,
		    action_results = $6, duration_ms = $7, completed_at = $8
		WHERE run_id = $1
		  AND workflow_id = (SELECT id FROM workflows WHERE slug = $2 LIMIT 1)
	`, runID, workflowSlug, status, studentMsg, instructorOutput, actionResults, durationMs, now)
	return err
}

// UpdateRunHeartbeat records a heartbeat from the runner.
func (q *Queries) UpdateRunHeartbeat(ctx context.Context, runID uuid.UUID, phase, currentAction string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE runs SET updated_at = NOW() WHERE id = $1
	`, runID)
	return err
}
