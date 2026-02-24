package rollback

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/google/uuid"
)

// UndoFunc is called during rollback with the step's stored data.
type UndoFunc func(ctx context.Context, data json.RawMessage) error

// Step represents one completed provisioning action and how to undo it.
type Step struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

// Persister saves rollback steps to durable storage (e.g., jobs.rollback_steps).
type Persister interface {
	SaveRollbackSteps(ctx context.Context, jobID uuid.UUID, steps []Step) error
}

// Engine tracks completed provisioning steps and can reverse them on failure.
// Steps are persisted after each Record() so rollback survives worker crashes.
type Engine struct {
	jobID     uuid.UUID
	steps     []Step
	undoFuncs map[string]UndoFunc
	persister Persister
	logger    *slog.Logger
}

// New creates a rollback engine for a specific job.
func New(jobID uuid.UUID, persister Persister, logger *slog.Logger) *Engine {
	return &Engine{
		jobID:     jobID,
		steps:     make([]Step, 0),
		undoFuncs: make(map[string]UndoFunc),
		persister: persister,
		logger:    logger,
	}
}

// RegisterUndo registers an undo function for a step name.
// Must be called before Record() for that step name.
func (e *Engine) RegisterUndo(stepName string, fn UndoFunc) {
	e.undoFuncs[stepName] = fn
}

// Record appends a completed step and persists to the database.
func (e *Engine) Record(ctx context.Context, name string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal step data: %w", err)
	}

	step := Step{Name: name, Data: raw}
	e.steps = append(e.steps, step)

	if e.persister != nil {
		if err := e.persister.SaveRollbackSteps(ctx, e.jobID, e.steps); err != nil {
			e.logger.Error("failed to persist rollback steps", "job_id", e.jobID, "error", err)
			// Don't fail the provisioning step just because persistence failed
		}
	}

	e.logger.Info("recorded rollback step", "job_id", e.jobID, "step", name, "total_steps", len(e.steps))
	return nil
}

// Rollback executes undo functions in reverse order.
// Returns a slice of errors (one per failed undo). Empty slice = full success.
func (e *Engine) Rollback(ctx context.Context) []error {
	var errs []error

	// Process steps in reverse order
	reversed := make([]Step, len(e.steps))
	copy(reversed, e.steps)
	slices.Reverse(reversed)

	for _, step := range reversed {
		undoFn, ok := e.undoFuncs[step.Name]
		if !ok {
			e.logger.Warn("no undo function registered", "step", step.Name, "job_id", e.jobID)
			continue
		}

		e.logger.Info("rolling back step", "step", step.Name, "job_id", e.jobID)
		if err := undoFn(ctx, step.Data); err != nil {
			e.logger.Error("rollback step failed", "step", step.Name, "job_id", e.jobID, "error", err)
			errs = append(errs, fmt.Errorf("rollback %s: %w", step.Name, err))
			// Continue rolling back remaining steps even if one fails
		} else {
			e.logger.Info("rollback step succeeded", "step", step.Name, "job_id", e.jobID)
		}
	}

	return errs
}

// Steps returns the currently recorded steps (for inspection/logging).
func (e *Engine) Steps() []Step {
	return e.steps
}

// LoadSteps restores steps from a previous run (e.g., after worker crash).
func (e *Engine) LoadSteps(steps []Step) {
	e.steps = steps
}
