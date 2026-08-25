package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/workflowvalidation"
)

var ErrWorkflowNotApproved = errors.New("workflow is not in approved status")

// ActivateWorkflow validates the persisted script against the current library
// catalog and atomically moves an approved workflow to active.
func (q *Queries) ActivateWorkflow(ctx context.Context, id uuid.UUID) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workflow activation: %w", err)
	}
	defer tx.Rollback(ctx)

	var script, status, executionMode string
	if err := tx.QueryRow(ctx, `
		SELECT script, status, execution_mode
		FROM workflows
		WHERE id = $1
		FOR UPDATE
	`, id).Scan(&script, &status, &executionMode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowNotApproved
		}
		return fmt.Errorf("load workflow for activation: %w", err)
	}
	if status != models.WorkflowStatusApproved {
		return ErrWorkflowNotApproved
	}

	if executionMode == models.ExecModeKaliRunner {
		rows, err := tx.Query(ctx, `
			SELECT slug,
			       btrim(script) <> '' AS has_script,
			       NOT (COALESCE(supported_platforms, '[]'::jsonb) @> '["windows"]'::jsonb) AS bash_compatible
			FROM actions
			WHERE is_library = true AND slug IS NOT NULL
			FOR SHARE
		`)
		if err != nil {
			return fmt.Errorf("load library actions for activation: %w", err)
		}
		var actions []workflowvalidation.LibraryAction
		for rows.Next() {
			var slug string
			var hasScript, bashCompatible bool
			if err := rows.Scan(&slug, &hasScript, &bashCompatible); err != nil {
				rows.Close()
				return fmt.Errorf("scan library action for activation: %w", err)
			}
			reason := ""
			switch {
			case !hasScript:
				reason = "its script body is empty"
			case !bashCompatible:
				reason = "supported_platforms includes windows"
			}
			actions = append(actions, workflowvalidation.LibraryAction{
				Slug:              slug,
				RunnerCallable:    hasScript && bashCompatible,
				UnavailableReason: reason,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read library actions for activation: %w", err)
		}
		rows.Close()

		if err := workflowvalidation.ValidateRunActionCallsWithCatalog(script, actions); err != nil {
			return fmt.Errorf("workflow script is not activatable: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflows
		SET status = $2, updated_at = NOW()
		WHERE id = $1
	`, id, models.WorkflowStatusActive); err != nil {
		return fmt.Errorf("activate workflow: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workflow activation: %w", err)
	}
	return nil
}
