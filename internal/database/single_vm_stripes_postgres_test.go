package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestSharedStripesAssignWithoutCreatingAVLAN(t *testing.T) {
	dsn := requirePostgresDSN(t)
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := &Queries{pool: pool}

	var pooled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM vlan_pool WHERE vlan_tag BETWEEN 347 AND 355`).Scan(&pooled); err != nil {
		t.Fatal(err)
	}
	if pooled != 0 {
		t.Fatalf("isolated pool still has %d shared tags", pooled)
	}
	if err := q.EnsureSharedNetworks(ctx); err != nil {
		t.Fatal(err)
	}

	owner := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email, role, max_single_vms)
		VALUES ($1, $1, $1, $2, 'student', 3)
	`, owner, owner.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM pods WHERE owner_id = $1`, owner)
		_, _ = pool.Exec(cctx, `DELETE FROM users WHERE id = $1`, owner)
		_, _ = pool.Exec(cctx, `UPDATE shared_networks SET status = 'pending'`)
		_, _ = pool.Exec(cctx, `UPDATE shared_address_space SET policy_ready = false`)
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = q.PickSharedStripe(ctx, tx)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrSharedNetworksNotReady) {
		t.Fatalf("before activation: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE shared_networks SET status = 'active'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE shared_address_space SET policy_ready = true`); err != nil {
		t.Fatal(err)
	}

	for _, stripe := range models.SharedStripes() {
		if stripe.VLANTag == 355 {
			continue
		}
		var id uuid.UUID
		if err := pool.QueryRow(ctx, `SELECT id FROM shared_networks WHERE vlan_tag = $1`, stripe.VLANTag).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < models.SharedStripeSlots; n++ {
			if _, err := pool.Exec(ctx, `
				INSERT INTO pods (id, owner_id, name, vlan_id, subnet, status, network_mode, shared_network_id)
				VALUES ($1, $2, $3, $4, $5, 'active', 'shared', $6)
			`, uuid.New(), owner, "fill", stripe.VLANTag, stripe.CIDR, id); err != nil {
				t.Fatal(err)
			}
		}
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, stripe, err := q.PickSharedStripe(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if stripe.VLANTag != 355 {
		t.Fatalf("129th vm tag = %d", stripe.VLANTag)
	}

	var last uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM shared_networks WHERE vlan_tag = 355`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < models.SharedStripeSlots; n++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO pods (id, owner_id, name, vlan_id, subnet, status, network_mode, shared_network_id)
			VALUES ($1, $2, 'full', 355, '10.110.2.0/26', 'active', 'shared', $3)
		`, uuid.New(), owner, last); err != nil {
			t.Fatal(err)
		}
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = q.PickSharedStripe(ctx, tx)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrSharedNetworksFull) {
		t.Fatalf("145th vm: %v", err)
	}
}
