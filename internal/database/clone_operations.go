package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmal1/selfservice-api/internal/models"
)

func validateVMCloneOperation(op models.VMCloneOperation) error {
	if _, err := uuid.Parse(op.OperationID); err != nil {
		return fmt.Errorf("invalid clone operation_id: %w", err)
	}
	if _, err := uuid.Parse(op.PodID); err != nil {
		return fmt.Errorf("invalid clone pod_id: %w", err)
	}
	if _, err := uuid.Parse(op.PodVMID); err != nil {
		return fmt.Errorf("invalid clone pod_vm_id: %w", err)
	}
	if op.TargetName == "" || op.SourceRef == "" {
		return errors.New("clone operation target_name and source_ref are required")
	}
	if op.PreparedAt.IsZero() {
		return errors.New("clone operation prepared_at is required")
	}
	switch op.Phase {
	case models.VMCloneOperationPrepared, models.VMCloneOperationSubmitting, models.VMCloneOperationSubmitted:
	default:
		return fmt.Errorf("invalid clone operation phase %q", op.Phase)
	}
	return nil
}

func sameVMCloneOperationScope(a, b models.VMCloneOperation) bool {
	return a.PodID == b.PodID &&
		a.PodVMID == b.PodVMID &&
		a.TargetName == b.TargetName &&
		a.SourceRef == b.SourceRef
}

func decodeJobPayloadFields(payload []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	return fields, nil
}

func encodeJobPayloadFields(fields map[string]json.RawMessage) ([]byte, error) {
	return json.Marshal(fields)
}

func appendRollbackReceipt(rollbackSteps, step []byte) ([]byte, bool, error) {
	var steps []json.RawMessage
	if err := json.Unmarshal(rollbackSteps, &steps); err != nil {
		return nil, false, err
	}
	var candidate any
	if err := json.Unmarshal(step, &candidate); err != nil {
		return nil, false, err
	}
	for _, existing := range steps {
		var decoded any
		if err := json.Unmarshal(existing, &decoded); err != nil {
			return nil, false, err
		}
		if reflect.DeepEqual(decoded, candidate) {
			return rollbackSteps, true, nil
		}
	}
	steps = append(steps, append(json.RawMessage(nil), step...))
	updated, err := json.Marshal(steps)
	return updated, false, err
}

// PrepareVMCloneOperation persists an operation identity before any vCenter
// clone request can be submitted. A recovered claim reuses an existing
// operation for the same immutable source/target scope.
func (q *Queries) PrepareVMCloneOperation(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	candidate models.VMCloneOperation,
) (*models.VMCloneOperation, error) {
	if candidate.Phase == "" {
		candidate.Phase = models.VMCloneOperationPrepared
	}
	if candidate.PreparedAt.IsZero() {
		candidate.PreparedAt = time.Now().UTC()
	}
	if err := validateVMCloneOperation(candidate); err != nil {
		return nil, err
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var payload []byte
	err = tx.QueryRow(ctx, `
		SELECT payload
		FROM jobs
		WHERE id = $1
		  AND claimed_by = $2
		  AND type IN ('pod_create', 'vm_add', 'template_verify', 'template_revalidate')
		  AND status IN ('claimed', 'in_progress')
		FOR UPDATE
	`, jobID, workerID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: job %s cannot prepare clone operation for %s", ErrJobLeaseLost, jobID, workerID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock clone operation job: %w", err)
	}

	fields, err := decodeJobPayloadFields(payload)
	if err != nil {
		return nil, fmt.Errorf("decode clone operation payload: %w", err)
	}
	if raw := fields["clone_operation"]; len(raw) > 0 {
		var existing models.VMCloneOperation
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("decode existing clone operation: %w", err)
		}
		if err := validateVMCloneOperation(existing); err != nil {
			return nil, fmt.Errorf("validate existing clone operation: %w", err)
		}
		if !sameVMCloneOperationScope(existing, candidate) {
			return nil, fmt.Errorf(
				"job %s already owns unresolved clone operation %s for VM %s",
				jobID,
				existing.OperationID,
				existing.PodVMID,
			)
		}
		return &existing, tx.Commit(ctx)
	}

	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	fields["clone_operation"] = raw
	updated, err := encodeJobPayloadFields(fields)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET payload = $2 WHERE id = $1`, jobID, updated); err != nil {
		return nil, fmt.Errorf("persist clone operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit clone operation: %w", err)
	}
	return &candidate, nil
}

// ArmVMCloneOperation marks a prepared operation cleanup-only before the
// external CloneVM_Task submission. This is the durable crash boundary.
func (q *Queries) ArmVMCloneOperation(
	ctx context.Context,
	jobID uuid.UUID,
	workerID, operationID string,
) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		SET payload = jsonb_set(
			jsonb_set(payload - 'cleanup_completed', '{cleanup_only}', 'true'::jsonb, true),
			'{clone_operation,phase}',
			to_jsonb($4::text),
			true
		)
		WHERE id = $1
		  AND claimed_by = $2
		  AND status IN ('claimed', 'in_progress')
		  AND payload->'clone_operation'->>'operation_id' = $3
		  AND payload->'clone_operation'->>'phase' = 'prepared'
	`, jobID, workerID, operationID, models.VMCloneOperationSubmitting)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: job %s cannot arm clone operation %s for %s", ErrJobLeaseLost, jobID, operationID, workerID)
	}
	return nil
}

// AbandonUnsubmittedVMCloneOperation removes an operation after the vCenter
// client proves CloneVM_Task was never invoked. The submitting phase is allowed
// because an ambiguous database response from ArmVMCloneOperation still causes
// the client to return before remote submission.
func (q *Queries) AbandonUnsubmittedVMCloneOperation(
	ctx context.Context,
	jobID uuid.UUID,
	workerID, operationID string,
) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		SET payload = CASE
			WHEN NOT (payload ? 'cleanup_target')
			  AND jsonb_array_length(COALESCE(payload->'cleanup_handoff_targets', '[]'::jsonb)) = 0
			THEN payload - 'clone_operation' - 'cleanup_only' - 'cleanup_completed'
			ELSE payload - 'clone_operation'
		END
		WHERE id = $1
		  AND claimed_by = $2
		  AND status IN ('claimed', 'in_progress')
		  AND payload->'clone_operation'->>'operation_id' = $3
		  AND payload->'clone_operation'->>'phase' IN ('prepared', 'submitting')
		  AND COALESCE(payload->'clone_operation'->>'task_ref', '') = ''
	`, jobID, workerID, operationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		var alreadyAbsent bool
		err := q.pool.QueryRow(ctx, `
			SELECT NOT (payload ? 'clone_operation')
			FROM jobs
			WHERE id = $1
			  AND claimed_by = $2
			  AND status IN ('claimed', 'in_progress')
		`, jobID, workerID).Scan(&alreadyAbsent)
		if err == nil && alreadyAbsent {
			return nil
		}
		return fmt.Errorf(
			"%w: job %s cannot abandon unsubmitted clone operation %s for %s",
			ErrJobLeaseLost,
			jobID,
			operationID,
			workerID,
		)
	}
	return nil
}

// PersistVMCloneTask records the task reference before the worker waits for it.
// Repeating the same write is safe; replacing a different task is forbidden.
func (q *Queries) PersistVMCloneTask(
	ctx context.Context,
	jobID uuid.UUID,
	workerID, operationID, taskRef string,
) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		SET payload = jsonb_set(
			jsonb_set(payload, '{clone_operation,task_ref}', to_jsonb($4::text), true),
			'{clone_operation,phase}',
			to_jsonb($5::text),
			true
		)
		WHERE id = $1
		  AND claimed_by = $2
		  AND status IN ('claimed', 'in_progress')
		  AND payload->'clone_operation'->>'operation_id' = $3
		  AND COALESCE(payload->'clone_operation'->>'task_ref', '') IN ('', $4)
	`, jobID, workerID, operationID, taskRef, models.VMCloneOperationSubmitted)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: job %s cannot persist task %s for operation %s", ErrJobLeaseLost, jobID, taskRef, operationID)
	}
	return nil
}

// CompleteVMCloneOperationWithoutResource closes a terminally failed task only
// after vCenter reconciliation proves that no marked VM exists.
func (q *Queries) CompleteVMCloneOperationWithoutResource(
	ctx context.Context,
	jobID uuid.UUID,
	workerID, operationID string,
) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var payload []byte
	err = tx.QueryRow(ctx, `
		SELECT payload
		FROM jobs
		WHERE id = $1
		  AND claimed_by = $2
		  AND status IN ('claimed', 'in_progress')
		FOR UPDATE
	`, jobID, workerID).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: job %s cannot complete clone operation for %s", ErrJobLeaseLost, jobID, workerID)
	}
	if err != nil {
		return err
	}
	fields, err := decodeJobPayloadFields(payload)
	if err != nil {
		return err
	}
	var existing models.VMCloneOperation
	if err := json.Unmarshal(fields["clone_operation"], &existing); err != nil {
		return fmt.Errorf("decode clone operation completion: %w", err)
	}
	if existing.OperationID != operationID {
		return fmt.Errorf("clone operation changed from %s to %s", operationID, existing.OperationID)
	}
	delete(fields, "clone_operation")
	if len(fields["cleanup_target"]) == 0 && len(fields["cleanup_handoff_targets"]) == 0 {
		fields["cleanup_completed"] = json.RawMessage("true")
	}
	updated, err := encodeJobPayloadFields(fields)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET payload = $2 WHERE id = $1`, jobID, updated); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinishStandaloneVMCloneCleanup records cleanup of a smoke-test clone. Unlike
// pod/vm provisioning cleanup, no pod_vms row exists for this target.
func (q *Queries) FinishStandaloneVMCloneCleanup(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	target []byte,
	compensated bool,
) error {
	decoded, err := decodePersistedVMCloneTarget(target)
	if err != nil {
		return err
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs
		SET payload = CASE
			WHEN $4 THEN jsonb_set(
				payload - 'cleanup_target' - 'clone_operation',
				'{cleanup_completed}',
				'true'::jsonb,
				true
			)
			ELSE payload - 'cleanup_target' - 'clone_operation' - 'cleanup_only' - 'cleanup_completed'
		END
		WHERE id = $1
		  AND claimed_by = $2
		  AND type IN ('template_verify', 'template_revalidate')
		  AND status IN ('claimed', 'in_progress')
		  AND (
		    payload->'cleanup_target' = $3::jsonb
		    OR (
		      payload->'clone_operation'->>'pod_id' = $5
		      AND payload->'clone_operation'->>'pod_vm_id' = $6
		    )
		  )
	`, jobID, workerID, target, compensated, decoded.PodID, decoded.PodVMID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf(
			"%w: job %s cannot finish standalone clone cleanup for %s",
			ErrJobLeaseLost,
			jobID,
			workerID,
		)
	}
	return nil
}

// AdoptJobRollbackStep appends a lost owner's exact rollback receipt and
// atomically fences any successor generation. The next claim is cleanup-only,
// so it can only consume the accumulated rollback receipts.
func (q *Queries) AdoptJobRollbackStep(ctx context.Context, jobID uuid.UUID, step []byte) error {
	if !json.Valid(step) {
		return errors.New("rollback handoff step is invalid JSON")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var payload, rollbackSteps []byte
	err = tx.QueryRow(ctx, `
		SELECT payload, COALESCE(rollback_steps, '[]'::jsonb)
		FROM jobs
		WHERE id = $1
		  AND type = 'pod_create'
		  AND status IN ('pending', 'claimed', 'in_progress', 'rollback', 'failed')
		FOR UPDATE
	`, jobID).Scan(&payload, &rollbackSteps)
	if err != nil {
		return fmt.Errorf("lock rollback receipt handoff: %w", err)
	}

	updatedSteps, _, err := appendRollbackReceipt(rollbackSteps, step)
	if err != nil {
		return fmt.Errorf("append rollback receipt handoff: %w", err)
	}
	fields, err := decodeJobPayloadFields(payload)
	if err != nil {
		return err
	}
	fields["cleanup_only"] = json.RawMessage("true")
	delete(fields, "cleanup_completed")
	updatedPayload, err := encodeJobPayloadFields(fields)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET rollback_steps = $2,
		    payload = $3,
		    status = 'pending',
		    claimed_by = NULL,
		    claimed_at = NULL,
		    started_at = NULL,
		    completed_at = NULL,
		    result = NULL,
		    next_attempt_at = now()
		WHERE id = $1
	`, jobID, updatedSteps, updatedPayload); err != nil {
		return fmt.Errorf("persist rollback receipt handoff: %w", err)
	}
	return tx.Commit(ctx)
}
