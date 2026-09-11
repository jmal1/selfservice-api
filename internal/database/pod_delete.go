package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
)

type PodDeletionOutcome string

const (
	PodDeletionOutcomeCancelled        PodDeletionOutcome = "cancelled"
	PodDeletionOutcomeAlreadyCancelled PodDeletionOutcome = "already_cancelled"
	PodDeletionOutcomeNeedsDestroy     PodDeletionOutcome = "needs_destroy"
)

type PodDeletionDecision struct {
	Outcome PodDeletionOutcome
	JobID   uuid.UUID
}

func podCancellationProtectedJobs(ctx context.Context, tx pgx.Tx, podID, excludeJobID uuid.UUID) (bool, error) {
	query := `
		WITH pod_vm_ids AS (
			SELECT id::text AS pod_vm_id
			FROM pod_vms
			WHERE pod_id = $1
		),
		ordered_types AS (
			SELECT job_type, ord
			FROM unnest($3::text[]) WITH ORDINALITY AS job_types(job_type, ord)
		)
		SELECT j.id
		FROM jobs j
		JOIN ordered_types ot ON ot.job_type = j.type
		WHERE j.id <> $2
		  AND (
		    j.payload->>'pod_id' = $1::text
		    OR j.payload->>'pod_vm_id' IN (SELECT pod_vm_id FROM pod_vm_ids)
		  )
		ORDER BY ot.ord, j.created_at ASC, j.id ASC`

	rows, err := tx.Query(ctx, query, podID, excludeJobID, serializedPodJobTypes)
	if err != nil {
		return false, fmt.Errorf("scan protected pod jobs for %s: %w", podID, err)
	}
	defer rows.Close()

	evidence := false
	for rows.Next() {
		var jobID uuid.UUID
		if err := rows.Scan(&jobID); err != nil {
			return false, fmt.Errorf("scan protected pod jobs for %s: %w", podID, err)
		}
		evidence = true
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("scan protected pod jobs for %s: %w", podID, err)
	}
	return evidence, nil
}

// CancelPendingPodIfNeverStarted cancels a pending pod only when the create job
// is still pristine and no job, VM, placement, or receipt evidence shows that
// provisioning ever started. It locks the authoritative create job first, then
// the pod, and only reads protected jobs when checking for evidence.
func (q *Queries) CancelPendingPodIfNeverStarted(ctx context.Context, podID uuid.UUID) (*PodDeletionDecision, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pod cancellation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createJobID uuid.UUID
	var createJobStatus string
	var retryCount int
	var cleanupOnly bool
	var rollbackCount int
	var claimedBy sql.NullString
	var claimedAt, startedAt, completedAt, nextAttemptAt sql.NullTime
	jobErr := tx.QueryRow(ctx, `
		SELECT id, status,
		       COALESCE(retry_count, 0),
		       COALESCE(payload->>'cleanup_only', 'false') = 'true',
		       claimed_by,
		       claimed_at,
		       started_at,
		       completed_at,
		       next_attempt_at,
		       CASE
		         WHEN jsonb_typeof(rollback_steps) = 'array' THEN
		           CASE
		             WHEN jsonb_array_length(rollback_steps) = 0 THEN 0
		             ELSE 1
		           END
		         ELSE 1
		       END
		FROM jobs
		WHERE type = $1
		  AND payload->>'pod_id' = $2::text
		ORDER BY created_at ASC, id ASC
		FOR UPDATE
		LIMIT 1
	`, models.JobTypePodCreate, podID.String()).Scan(
		&createJobID,
		&createJobStatus,
		&retryCount,
		&cleanupOnly,
		&claimedBy,
		&claimedAt,
		&startedAt,
		&completedAt,
		&nextAttemptAt,
		&rollbackCount,
	)
	if jobErr != nil && !errors.Is(jobErr, pgx.ErrNoRows) {
		return nil, fmt.Errorf("lock pod create job for cancellation: %w", jobErr)
	}

	pristineCreateJob := jobErr == nil &&
		createJobStatus == models.JobStatusPending &&
		retryCount == 0 &&
		!cleanupOnly &&
		!claimedBy.Valid &&
		!claimedAt.Valid &&
		!startedAt.Valid &&
		!completedAt.Valid &&
		!nextAttemptAt.Valid &&
		rollbackCount == 0

	var competingEvidence bool
	competingEvidence, err = podCancellationProtectedJobs(ctx, tx, podID, createJobID)
	if err != nil {
		return nil, err
	}
	if competingEvidence {
		pristineCreateJob = false
	}

	var podStatus, podError string
	if err := tx.QueryRow(ctx, `
		SELECT status, COALESCE(error_message, '')
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&podStatus, &podError); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod for cancellation: %w", err)
		}
		return nil, fmt.Errorf("lock pod for cancellation: %w", err)
	}

	if podStatus == models.PodStatusDestroyed && podError == models.PodErrorCancelledBeforeProvisioning {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit idempotent pod cancellation: %w", err)
		}
		return &PodDeletionDecision{Outcome: PodDeletionOutcomeAlreadyCancelled, JobID: createJobID}, nil
	}

	postEvidence, err := podCancellationProtectedJobs(ctx, tx, podID, createJobID)
	if err != nil {
		return nil, err
	}

	needsDestroy := !pristineCreateJob || postEvidence || podStatus != models.PodStatusPending || errors.Is(jobErr, pgx.ErrNoRows)
	if needsDestroy {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit pod cancellation preflight: %w", err)
		}
		return &PodDeletionDecision{Outcome: PodDeletionOutcomeNeedsDestroy, JobID: createJobID}, nil
	}

	rows, err := tx.Query(ctx, `
		SELECT id, status, vcenter_vm_id, vcenter_vm_name, ip_address
		FROM pod_vms
		WHERE pod_id = $1
		ORDER BY created_at ASC, id ASC
		FOR UPDATE
	`, podID)
	if err != nil {
		return nil, fmt.Errorf("lock pod VMs for cancellation: %w", err)
	}
	evidence := false
	for rows.Next() {
		var (
			vmID                                  uuid.UUID
			vmStatus                              string
			vcenterVMID, vcenterVMName, ipAddress sql.NullString
		)
		if err := rows.Scan(&vmID, &vmStatus, &vcenterVMID, &vcenterVMName, &ipAddress); err != nil {
			return nil, fmt.Errorf("scan pod VM cancellation evidence: %w", err)
		}
		if vmStatus != models.VMStatusPending ||
			(vcenterVMID.Valid && vcenterVMID.String != "") ||
			(vcenterVMName.Valid && vcenterVMName.String != "") ||
			(ipAddress.Valid && ipAddress.String != "") {
			evidence = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect pod VM cancellation evidence: %w", err)
	}
	rows.Close()
	if evidence {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit pod cancellation preflight: %w", err)
		}
		return &PodDeletionDecision{Outcome: PodDeletionOutcomeNeedsDestroy, JobID: createJobID}, nil
	}

	var hasReceipt bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pod_portgroup_receipts
			WHERE pod_id = $1
		)
	`, podID).Scan(&hasReceipt); err != nil {
		return nil, fmt.Errorf("check pod port group receipt evidence: %w", err)
	}
	if hasReceipt {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit pod cancellation preflight: %w", err)
		}
		return &PodDeletionDecision{Outcome: PodDeletionOutcomeNeedsDestroy, JobID: createJobID}, nil
	}

	var hasPlacement bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM vm_placements vp
			JOIN pod_vms pv ON pv.id = vp.pod_vm_id
			WHERE pv.pod_id = $1
		)
	`, podID).Scan(&hasPlacement); err != nil {
		return nil, fmt.Errorf("check pod placement evidence: %w", err)
	}
	if hasPlacement {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit pod cancellation preflight: %w", err)
		}
		return &PodDeletionDecision{Outcome: PodDeletionOutcomeNeedsDestroy, JobID: createJobID}, nil
	}

	cancellationResult, err := json.Marshal(map[string]any{
		"cancelled":  true,
		"pod_id":     podID.String(),
		"pod_status": models.PodStatusDestroyed,
		"job_id":     createJobID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal pod cancellation result: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE pod_vms
		SET status = 'deleted',
		    vcenter_vm_id = NULL,
		    vcenter_vm_name = NULL,
		    ip_address = NULL,
		    guest_credentials_verified_at = NULL,
		    guest_credentials_verified_vm_id = NULL
		WHERE pod_id = $1
	`, podID); err != nil {
		return nil, fmt.Errorf("mark pod VMs deleted: %w", err)
	}

	if tag, err := tx.Exec(ctx, `
		UPDATE pods
		SET status = $2,
		    error_message = $3,
		    updated_at = now()
		WHERE id = $1
		  AND status = 'pending'
	`, podID, models.PodStatusDestroyed, models.PodErrorCancelledBeforeProvisioning); err != nil {
		return nil, fmt.Errorf("mark pod destroyed during cancellation: %w", err)
	} else if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("mark pod destroyed during cancellation: pod %s changed state", podID)
	}

	if jobErr == nil {
		if tag, err := tx.Exec(ctx, `
			UPDATE jobs
			SET status = 'failed',
			    result = $2::jsonb,
			    claimed_by = NULL,
			    claimed_at = NULL,
			    started_at = NULL,
			    completed_at = now()
			WHERE id = $1
			  AND status = 'pending'
			  AND claimed_by IS NULL
			  AND claimed_at IS NULL
			  AND started_at IS NULL
			  AND completed_at IS NULL
		`, createJobID, cancellationResult); err != nil {
			return nil, fmt.Errorf("mark pod create job cancelled: %w", err)
		} else if tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("mark pod create job cancelled: job %s changed state", createJobID)
		}
	}

	if tag, err := tx.Exec(ctx, `
		UPDATE vlan_pool
		SET pod_id = NULL,
		    allocated_at = NULL
		WHERE pod_id = $1
	`, podID); err != nil {
		return nil, fmt.Errorf("release pod VLAN during cancellation: %w", err)
	} else if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("release pod VLAN during cancellation: allocation for pod %s affected %d rows", podID, tag.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pod cancellation: %w", err)
	}
	return &PodDeletionDecision{Outcome: PodDeletionOutcomeCancelled, JobID: createJobID}, nil
}
