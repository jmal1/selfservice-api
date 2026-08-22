package rollback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

type ownershipHandoffPersister interface {
	HandoffRollbackStep(ctx context.Context, jobID uuid.UUID, step Step) error
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

const (
	recordCleanupTimeout = 90 * time.Second
	rollbackFenceTimeout = 10 * time.Second
)

var ErrOwnershipLost = errors.New("rollback persistence ownership lost")

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
	for _, existing := range e.steps {
		if existing.Name == name && bytes.Equal(existing.Data, step.Data) {
			e.logger.Info("identical rollback step already recorded",
				"job_id", e.jobID, "step", name)
			return nil
		}
	}
	e.steps = append(e.steps, step)

	if e.persister != nil {
		if err := e.persister.SaveRollbackSteps(ctx, e.jobID, e.steps); err != nil {
			e.logger.Error("failed to persist rollback steps", "job_id", e.jobID, "error", err)
			handoff, ok := e.persister.(ownershipHandoffPersister)
			if !ok {
				return fmt.Errorf("persist rollback step %s with uncertain ownership: %w", name, err)
			}
			handoffCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordCleanupTimeout)
			defer cancel()
			var handoffErr error
			for attempt := 0; ; attempt++ {
				handoffErr = handoff.HandoffRollbackStep(handoffCtx, e.jobID, step)
				if handoffErr == nil {
					return fmt.Errorf("persist rollback step %s after handing receipt to successor: %w", name, err)
				}
				delay := time.Second << min(attempt, 4)
				timer := time.NewTimer(delay)
				select {
				case <-handoffCtx.Done():
					timer.Stop()
					return fmt.Errorf(
						"persist rollback step %s: %w; successor handoff failed: %v",
						name,
						err,
						handoffErr,
					)
				case <-timer.C:
				}
			}
		}
	}

	e.logger.Info("recorded rollback step", "job_id", e.jobID, "step", name, "total_steps", len(e.steps))
	return nil
}

// Rollback executes undo functions in reverse order.
// Returns a slice of errors (one per failed undo). Empty slice = full success.
func (e *Engine) Rollback(ctx context.Context) []error {
	var errs []error
	remaining := append([]Step(nil), e.steps...)

	for i := len(remaining) - 1; i >= 0; i-- {
		step := remaining[i]
		if e.persister != nil {
			fenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackFenceTimeout)
			err := e.persister.SaveRollbackSteps(fenceCtx, e.jobID, remaining)
			cancel()
			if err != nil {
				e.logger.Error("rollback ownership fence failed",
					"step", step.Name, "job_id", e.jobID, "error", err)
				e.steps = remaining
				return []error{fmt.Errorf("fence rollback %s before undo: %w", step.Name, err)}
			}
		}

		undoFn, ok := e.undoFuncs[step.Name]
		if !ok {
			e.logger.Warn("no undo function registered", "step", step.Name, "job_id", e.jobID)
			errs = append(errs, fmt.Errorf("rollback %s: no undo function registered", step.Name))
			continue
		}

		e.logger.Info("rolling back step", "step", step.Name, "job_id", e.jobID)
		if err := undoFn(ctx, step.Data); err != nil {
			e.logger.Error("rollback step failed", "step", step.Name, "job_id", e.jobID, "error", err)
			errs = append(errs, fmt.Errorf("rollback %s: %w", step.Name, err))
			// Continue rolling back remaining steps even if one fails
		} else {
			e.logger.Info("rollback step succeeded", "step", step.Name, "job_id", e.jobID)
			beforeCheckpoint := append([]Step(nil), remaining...)
			remaining = append(remaining[:i], remaining[i+1:]...)
			if e.persister != nil {
				checkpointCtx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx),
					rollbackFenceTimeout,
				)
				err := e.persister.SaveRollbackSteps(checkpointCtx, e.jobID, remaining)
				cancel()
				if err != nil {
					e.steps = beforeCheckpoint
					errs = append(errs, fmt.Errorf(
						"checkpoint rollback %s after undo: %w",
						step.Name,
						err,
					))
					return errs
				}
			}
		}
	}

	e.steps = remaining
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
