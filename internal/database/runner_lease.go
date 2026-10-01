package database

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmal1/selfservice-api/internal/models"
)

// ErrRunnerPoolExhausted means all fourteen runner addresses on a stripe are leased.
var ErrRunnerPoolExhausted = errors.New("runner address pool is exhausted")

// LeaseRunnerIP reserves the lowest free runner address on a stripe for one run.
// A unique index on the active address makes two concurrent leases fail closed
// instead of handing out the same IP.
func (q *Queries) LeaseRunnerIP(ctx context.Context, networkID, runID uuid.UUID) (string, error) {
	var tag int
	if err := q.pool.QueryRow(ctx, `SELECT vlan_tag FROM shared_networks WHERE id = $1`, networkID).Scan(&tag); err != nil {
		return "", err
	}
	var stripe models.Stripe
	found := false
	for _, candidate := range models.SharedStripes() {
		if candidate.VLANTag == tag {
			stripe = candidate
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("shared network tag is outside the stripe catalog")
	}
	for _, ip := range stripe.RunnerAddresses() {
		_, err := q.pool.Exec(ctx, `
			INSERT INTO runner_ip_leases (shared_network_id, ip, run_id)
			VALUES ($1, $2, $3)
		`, networkID, ip, runID)
		if err == nil {
			return ip, nil
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		return "", err
	}
	return "", ErrRunnerPoolExhausted
}

// ReleaseRunnerLease frees the address held by a finished run. Isolated runs
// have no row, so this is a no-op for them.
func (q *Queries) ReleaseRunnerLease(ctx context.Context, runID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE runner_ip_leases
		SET released_at = NOW()
		WHERE run_id = $1 AND released_at IS NULL
	`, runID)
	return err
}
