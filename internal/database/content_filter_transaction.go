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

// contentFilterMutationAdvisoryLockKey is shared by every OPNsense firewall
// mutation and VLAN checkout. Changing it would break mixed-version fencing.
const contentFilterMutationAdvisoryLockKey int64 = 0x435243424c4f434b

// contentFilterCanaryReservationAdvisoryLockKey serializes only the short
// reservation-table transaction with VLAN checkout. It must remain distinct
// from the long-running OPNsense mutation lock.
const contentFilterCanaryReservationAdvisoryLockKey int64 = 0x43524343414e4152

// ContentFilterTransaction is the durable recovery record for the one
// deployment-owned, multi-surface OPNsense policy transaction.
type ContentFilterTransaction struct {
	OperationID   uuid.UUID
	SourceNetwork string
	Snapshot      json.RawMessage
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// WithContentFilterMutationLock serializes OPNsense firewall policy mutation,
// content-filter recovery, and VLAN allocation across every worker process.
func (q *Queries) WithContentFilterMutationLock(
	ctx context.Context,
	mutate func(context.Context) error,
) error {
	q.contentFilterMutationLock.Lock()
	defer q.contentFilterMutationLock.Unlock()
	return q.withSessionAdvisoryLock(
		ctx,
		"content-filter and firewall",
		"SELECT pg_advisory_lock($1)",
		"SELECT pg_advisory_unlock($1)",
		[]any{contentFilterMutationAdvisoryLockKey},
		mutate,
	)
}

// GetContentFilterTransaction returns the singleton recovery record, if any.
func (q *Queries) GetContentFilterTransaction(ctx context.Context) (*ContentFilterTransaction, error) {
	var transaction ContentFilterTransaction
	err := q.pool.QueryRow(ctx, `
		SELECT operation_id, source_network, snapshot, created_at, updated_at
		FROM content_filter_transactions
		WHERE singleton = true
	`).Scan(
		&transaction.OperationID,
		&transaction.SourceNetwork,
		&transaction.Snapshot,
		&transaction.CreatedAt,
		&transaction.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get content-filter transaction: %w", err)
	}
	return &transaction, nil
}

// CreateContentFilterTransaction durably records exact rollback state before
// the first OPNsense surface is changed.
func (q *Queries) CreateContentFilterTransaction(
	ctx context.Context,
	operationID uuid.UUID,
	sourceNetwork string,
	snapshot json.RawMessage,
) error {
	if operationID == uuid.Nil {
		return errors.New("content-filter transaction operation id is required")
	}
	if !json.Valid(snapshot) {
		return errors.New("content-filter transaction snapshot is invalid JSON")
	}
	_, err := q.pool.Exec(ctx, `
		INSERT INTO content_filter_transactions
			(singleton, operation_id, source_network, snapshot)
		VALUES (true, $1, $2, $3)
	`, operationID, sourceNetwork, snapshot)
	if err != nil {
		return fmt.Errorf("create content-filter transaction: %w", err)
	}
	return nil
}

// CompleteContentFilterTransaction removes a recovered or committed operation.
// It is idempotent for the same operation so a lost database response is safe.
func (q *Queries) CompleteContentFilterTransaction(ctx context.Context, operationID uuid.UUID) error {
	tag, err := q.pool.Exec(ctx, `
		DELETE FROM content_filter_transactions
		WHERE singleton = true AND operation_id = $1
	`, operationID)
	if err != nil {
		return fmt.Errorf("complete content-filter transaction: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	current, err := q.GetContentFilterTransaction(ctx)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	return fmt.Errorf(
		"content-filter transaction identity changed from %s to %s",
		operationID,
		current.OperationID,
	)
}

// ListContentFilterCanaryReservations returns every /24 kept unavailable to
// vlan_pool while a canary transaction is active or recoverable.
func (q *Queries) ListContentFilterCanaryReservations(ctx context.Context) ([]string, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT source_network::text
		FROM content_filter_canary_reservations
		ORDER BY source_network
	`)
	if err != nil {
		return nil, fmt.Errorf("list content-filter canary reservations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			return nil, fmt.Errorf("scan content-filter canary reservation: %w", err)
		}
		out = append(out, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate content-filter canary reservations: %w", err)
	}
	return out, nil
}

// ReplaceContentFilterCanaryReservations atomically installs the exact
// conservative exclusion set. Callers hold the content-filter advisory lock.
func (q *Queries) ReplaceContentFilterCanaryReservations(ctx context.Context, sources []string) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin content-filter canary reservation replacement: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx,
		"SELECT pg_advisory_xact_lock($1)",
		contentFilterCanaryReservationAdvisoryLockKey,
	); err != nil {
		return fmt.Errorf("acquire content-filter canary reservation lock: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM content_filter_canary_reservations"); err != nil {
		return fmt.Errorf("clear content-filter canary reservations: %w", err)
	}
	for _, source := range sources {
		var allocated bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM vlan_pool
				WHERE pod_id IS NOT NULL
				  AND subnet::cidr = $1::cidr
			)
		`, source).Scan(&allocated); err != nil {
			return fmt.Errorf("validate content-filter canary %q is inactive: %w", source, err)
		}
		if allocated {
			return fmt.Errorf("content-filter canary %q is already allocated", source)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO content_filter_canary_reservations (source_network)
			VALUES ($1::cidr)
		`, source); err != nil {
			return fmt.Errorf("reserve content-filter canary %q: %w", source, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit content-filter canary reservation replacement: %w", err)
	}
	return nil
}
