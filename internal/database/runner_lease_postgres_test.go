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

func TestLeaseRunnerIPStopsAtFourteen(t *testing.T) {
	dsn := requirePostgresDSN(t)
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := &Queries{pool: pool}
	if err := q.EnsureSharedNetworks(ctx); err != nil {
		t.Fatal(err)
	}
	var networkID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM shared_networks WHERE vlan_tag = 347`).Scan(&networkID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM runner_ip_leases WHERE shared_network_id = $1`, networkID)
	})

	var firstRun uuid.UUID
	seen := map[string]bool{}
	for i := 0; i < models.SharedRunnerSlots; i++ {
		runID := uuid.New()
		ip, err := q.LeaseRunnerIP(ctx, networkID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if seen[ip] {
			t.Fatalf("address %s leased twice", ip)
		}
		seen[ip] = true
		if i == 0 {
			firstRun = runID
			if ip != "10.110.0.2" {
				t.Fatalf("first address = %s", ip)
			}
		}
	}
	if _, err := q.LeaseRunnerIP(ctx, networkID, uuid.New()); !errors.Is(err, ErrRunnerPoolExhausted) {
		t.Fatalf("15th lease: %v", err)
	}
	if err := q.ReleaseRunnerLease(ctx, firstRun); err != nil {
		t.Fatal(err)
	}
	ip, err := q.LeaseRunnerIP(ctx, networkID, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.110.0.2" {
		t.Fatalf("reused address = %s", ip)
	}
}
