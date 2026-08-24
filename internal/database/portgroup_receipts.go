package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrPortGroupReceiptNotFound = errors.New("durable port group receipt not found")
var ErrPortGroupReceiptConflict = errors.New("durable port group receipt conflicts with existing ownership")

const (
	PodPortGroupReceiptPlanned  = "planned"
	PodPortGroupReceiptApplying = "applying"
	PodPortGroupReceiptActive   = "active"
	PodPortGroupReceiptRemoved  = "removed"
)

type PodPortGroupReceipt struct {
	Receipt   json.RawMessage
	Keys      map[string]string
	State     string
	RemovedAt *time.Time
}

// GetPodPortGroupReceipt returns the immutable per-pod receipt and its removal
// tombstone. Current inventory and a portgroup name are never ownership proof.
func (q *Queries) GetPodPortGroupReceipt(ctx context.Context, podID uuid.UUID) (*PodPortGroupReceipt, error) {
	var record PodPortGroupReceipt
	err := q.pool.QueryRow(ctx, `
		SELECT receipt, portgroup_keys, state, removed_at
		FROM pod_portgroup_receipts
		WHERE pod_id = $1
	`, podID).Scan(&record.Receipt, &record.Keys, &record.State, &record.RemovedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w for pod %s", ErrPortGroupReceiptNotFound, podID)
	}
	if err != nil {
		return nil, fmt.Errorf("query pod port group receipt: %w", err)
	}
	return &record, nil
}

// BeginPodPortGroupMutation durably records that AddPortGroup may run. The
// rollback step is persisted before this transition, so a planned receipt proves
// that no standard-switch mutation was authorized.
func (q *Queries) BeginPodPortGroupMutation(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	podID uuid.UUID,
	receipt json.RawMessage,
) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin port group mutation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		owned          bool
		receiptMatches bool
		state          string
	)
	if err := tx.QueryRow(ctx, `
		SELECT j.claimed_by = $2
		       AND j.status IN ('claimed', 'in_progress')
		       AND j.type = 'pod_create'
		       AND j.payload->>'pod_id' = $3,
		       r.receipt = $4::jsonb,
		       r.state
		FROM jobs j
		JOIN pod_portgroup_receipts r
		  ON r.create_job_id = j.id
		 AND r.pod_id = $3::uuid
		WHERE j.id = $1
		FOR UPDATE OF j, r
	`, jobID, workerID, podID.String(), receipt).Scan(&owned, &receiptMatches, &state); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w for pod %s", ErrPortGroupReceiptNotFound, podID)
	} else if err != nil {
		return fmt.Errorf("lock port group mutation intent: %w", err)
	}
	if !owned {
		return fmt.Errorf("%w: job %s cannot begin port group mutation for pod %s", ErrJobLeaseLost, jobID, podID)
	}
	if !receiptMatches || (state != PodPortGroupReceiptPlanned &&
		state != PodPortGroupReceiptApplying &&
		state != PodPortGroupReceiptActive) {
		return fmt.Errorf("%w for pod %s while beginning mutation from state %s", ErrPortGroupReceiptConflict, podID, state)
	}
	if state == PodPortGroupReceiptPlanned {
		if _, err := tx.Exec(ctx, `
			UPDATE pod_portgroup_receipts
			SET state = 'applying'
			WHERE pod_id = $1
		`, podID); err != nil {
			return fmt.Errorf("persist port group mutation intent: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit port group mutation intent: %w", err)
	}
	return nil
}

// PersistPodPortGroupKeys binds the immutable plan to stable per-host vSphere
// portgroup keys and marks the mutation active atomically. A host key may be
// learned once and may never change.
func (q *Queries) PersistPodPortGroupKeys(
	ctx context.Context,
	podID uuid.UUID,
	receipt json.RawMessage,
	keys map[string]string,
) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin port group key transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		receiptMatches bool
		existing       map[string]string
		state          string
		removedAt      *time.Time
	)
	if err := tx.QueryRow(ctx, `
			SELECT receipt = $2::jsonb, portgroup_keys, state, removed_at
			FROM pod_portgroup_receipts
			WHERE pod_id = $1
			FOR UPDATE
		`, podID, receipt).Scan(&receiptMatches, &existing, &state, &removedAt); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w for pod %s", ErrPortGroupReceiptNotFound, podID)
	} else if err != nil {
		return fmt.Errorf("lock pod port group keys: %w", err)
	}
	if !receiptMatches || removedAt != nil ||
		(state != PodPortGroupReceiptApplying && state != PodPortGroupReceiptActive) {
		return fmt.Errorf("%w for pod %s while binding stable keys", ErrPortGroupReceiptConflict, podID)
	}
	for host, key := range keys {
		if host == "" || key == "" {
			return fmt.Errorf("%w for pod %s: empty host or portgroup key", ErrPortGroupReceiptConflict, podID)
		}
		if current, ok := existing[host]; ok && current != key {
			return fmt.Errorf(
				"%w for pod %s: host %s key changed from %s to %s",
				ErrPortGroupReceiptConflict,
				podID,
				host,
				current,
				key,
			)
		}
		existing[host] = key
	}
	if _, err := tx.Exec(ctx, `
			UPDATE pod_portgroup_receipts
			SET portgroup_keys = $2,
			    state = 'active'
			WHERE pod_id = $1
		`, podID, existing); err != nil {
		return fmt.Errorf("persist pod port group keys: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pod port group keys: %w", err)
	}
	return nil
}

// PersistPodPortGroupReceipt records the first exact ownership decision before
// AddPortGroup can run. Identical retries are idempotent; any different job or
// receipt for the pod fails closed.
func (q *Queries) PersistPodPortGroupReceipt(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	podID uuid.UUID,
	receipt json.RawMessage,
) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin port group receipt transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owned bool
	if err := tx.QueryRow(ctx, `
		SELECT claimed_by = $2
		       AND status IN ('claimed', 'in_progress')
		       AND type = 'pod_create'
		       AND payload->>'pod_id' = $3
		FROM jobs
		WHERE id = $1
		FOR UPDATE
	`, jobID, workerID, podID.String()).Scan(&owned); err != nil {
		return fmt.Errorf("lock port group receipt job: %w", err)
	}
	if !owned {
		return fmt.Errorf("%w: job %s cannot persist port group ownership for pod %s", ErrJobLeaseLost, jobID, podID)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO pod_portgroup_receipts (pod_id, create_job_id, receipt)
		VALUES ($1, $2, $3)
		ON CONFLICT (pod_id) DO NOTHING
	`, podID, jobID, receipt)
	if err != nil {
		return fmt.Errorf("insert pod port group receipt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existingJob uuid.UUID
		var receiptMatches bool
		var removedAt *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT create_job_id, receipt = $2::jsonb, removed_at
			FROM pod_portgroup_receipts
			WHERE pod_id = $1
			FOR UPDATE
		`, podID, receipt).Scan(&existingJob, &receiptMatches, &removedAt); err != nil {
			return fmt.Errorf("load conflicting pod port group receipt: %w", err)
		}
		if existingJob != jobID || !receiptMatches || removedAt != nil {
			return fmt.Errorf(
				"%w for pod %s: existing job=%s removed=%t",
				ErrPortGroupReceiptConflict,
				podID,
				existingJob,
				removedAt != nil,
			)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pod port group receipt: %w", err)
	}
	return nil
}

// MarkPodPortGroupRemoved persists completion proof for the exact receipt after
// exact deletion, or directly from planned state where durable state proves no
// mutation was authorized. Repeating the same completion is idempotent.
func (q *Queries) MarkPodPortGroupRemoved(
	ctx context.Context,
	podID uuid.UUID,
	receipt json.RawMessage,
) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE pod_portgroup_receipts
		SET state = 'removed',
		    removed_at = COALESCE(removed_at, clock_timestamp())
		WHERE pod_id = $1
		  AND receipt = $2::jsonb
		  AND state IN ('planned', 'active', 'removed')
	`, podID, receipt)
	if err != nil {
		return fmt.Errorf("mark pod port group removed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w for pod %s while recording removal", ErrPortGroupReceiptConflict, podID)
	}
	return nil
}
