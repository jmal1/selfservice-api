package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrPortGroupReceiptNotFound = errors.New("durable port group receipt not found")

// GetPodPortGroupReceipt returns the newest durable portgroup_create receipt
// for a pod. Destroy must use this receipt rather than current configuration or
// VLAN state, neither of which proves which host mutations this pod owns.
func (q *Queries) GetPodPortGroupReceipt(ctx context.Context, podID uuid.UUID) (json.RawMessage, error) {
	var receipt json.RawMessage
	err := q.pool.QueryRow(ctx, `
		SELECT step.value->'data'
		FROM jobs j
		CROSS JOIN LATERAL jsonb_array_elements(
			COALESCE(j.rollback_steps, '[]'::jsonb)
		) WITH ORDINALITY AS step(value, position)
		WHERE j.type = 'pod_create'
		  AND j.payload->>'pod_id' = $1
		  AND step.value->>'name' = 'portgroup_create'
		ORDER BY j.created_at DESC, step.position ASC
		LIMIT 1
	`, podID.String()).Scan(&receipt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w for pod %s", ErrPortGroupReceiptNotFound, podID)
	}
	if err != nil {
		return nil, fmt.Errorf("query pod port group receipt: %w", err)
	}
	return receipt, nil
}
