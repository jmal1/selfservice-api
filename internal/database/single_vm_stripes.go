package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
)

var (
	ErrSharedNetworksNotReady = errors.New("shared networks are not ready")
	ErrSharedNetworksFull     = errors.New("shared networks are full")
)

// EnsureSharedNetworks inserts the nine stripe rows once. It does not mark
// them active and it does not call OPNsense.
func (q *Queries) EnsureSharedNetworks(ctx context.Context) error {
	for _, stripe := range models.SharedStripes() {
		if _, err := q.pool.Exec(ctx, `
			INSERT INTO shared_networks (
				vlan_tag, cidr, gateway, portgroup_name,
				dhcp_start, dhcp_end, runner_first, runner_last
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (vlan_tag) DO NOTHING
		`, stripe.VLANTag, stripe.CIDR, stripe.Gateway, stripe.PortGroup,
			stripe.DHCPStart, stripe.DHCPEnd, stripe.RunnerFirst, stripe.RunnerLast); err != nil {
			return fmt.Errorf("insert shared network %d: %w", stripe.VLANTag, err)
		}
	}
	return nil
}

// SharedNetworksReady reports whether the address space is acknowledged and
// every stripe is active.
func (q *Queries) SharedNetworksReady(ctx context.Context, tx pgx.Tx) (bool, error) {
	var ready bool
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT policy_ready,
		       (SELECT count(*) FROM shared_networks WHERE status = 'active')
		FROM shared_address_space
		WHERE id = 1
	`).Scan(&ready, &active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return ready && active == models.SharedStripeCount, nil
}

// PickSharedStripe locks stripe assignment and returns the active stripe with
// the fewest non-destroyed pods. The lock is transaction scoped.
func (q *Queries) PickSharedStripe(ctx context.Context, tx pgx.Tx) (uuid.UUID, models.Stripe, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('crucible-shared-stripes'))`); err != nil {
		return uuid.Nil, models.Stripe{}, err
	}
	ready, err := q.SharedNetworksReady(ctx, tx)
	if err != nil {
		return uuid.Nil, models.Stripe{}, err
	}
	if !ready {
		return uuid.Nil, models.Stripe{}, ErrSharedNetworksNotReady
	}
	rows, err := tx.Query(ctx, `
		SELECT sn.id, sn.vlan_tag,
		       (SELECT count(*) FROM pods p
		         WHERE p.shared_network_id = sn.id
		           AND p.status <> 'destroyed')
		FROM shared_networks sn
		WHERE sn.status = 'active'
		ORDER BY 3 ASC, sn.vlan_tag ASC
	`)
	if err != nil {
		return uuid.Nil, models.Stripe{}, err
	}
	defer rows.Close()
	occupancy := map[int]int{}
	ids := map[int]uuid.UUID{}
	active := map[int]bool{}
	for rows.Next() {
		var id uuid.UUID
		var tag, n int
		if err := rows.Scan(&id, &tag, &n); err != nil {
			return uuid.Nil, models.Stripe{}, err
		}
		ids[tag] = id
		occupancy[tag] = n
		active[tag] = true
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, models.Stripe{}, err
	}
	stripe, ok, full := models.PickStripe(occupancy, active)
	if !ok && full {
		return uuid.Nil, models.Stripe{}, ErrSharedNetworksFull
	}
	if !ok {
		return uuid.Nil, models.Stripe{}, ErrSharedNetworksNotReady
	}
	return ids[stripe.VLANTag], stripe, nil
}

// SharedNetwork is one stored stripe.
type SharedNetwork struct {
	ID     uuid.UUID
	Status string
	Stripe models.Stripe
}

// ListSharedNetworks returns the nine stripes in tag order.
func (q *Queries) ListSharedNetworks(ctx context.Context) ([]SharedNetwork, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, vlan_tag, cidr, gateway, portgroup_name, dhcp_start, dhcp_end,
		       runner_first, runner_last, status
		FROM shared_networks
		ORDER BY vlan_tag
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedNetwork
	for rows.Next() {
		var row SharedNetwork
		if err := rows.Scan(
			&row.ID, &row.Stripe.VLANTag, &row.Stripe.CIDR, &row.Stripe.Gateway, &row.Stripe.PortGroup,
			&row.Stripe.DHCPStart, &row.Stripe.DHCPEnd, &row.Stripe.RunnerFirst, &row.Stripe.RunnerLast,
			&row.Status,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SetSharedNetworkStatus records the result of one stripe provision attempt.
func (q *Queries) SetSharedNetworkStatus(ctx context.Context, id uuid.UUID, status, message string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE shared_networks
		SET status = $2, error_message = $3, updated_at = now()
		WHERE id = $1
	`, id, status, message)
	return err
}

// CountSharedPodsTx counts a user's non-destroyed Single VMs inside tx.
func (q *Queries) CountSharedPodsTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pods
		WHERE owner_id = $1 AND network_mode = 'shared' AND status <> 'destroyed'
	`, userID).Scan(&n)
	return n, err
}
