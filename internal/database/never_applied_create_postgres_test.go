package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

type neverAppliedCreateFixture struct {
	pool    *pgxpool.Pool
	queries *Queries
	ownerID uuid.UUID
	podID   uuid.UUID
	jobID   uuid.UUID
	vlanTag int
	subnet  string
}

func newNeverAppliedCreateFixture(t *testing.T, status string) *neverAppliedCreateFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run never-applied create finalization tests")
	}
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	fixture := &neverAppliedCreateFixture{
		pool:    pool,
		queries: NewQueries(pool),
		ownerID: uuid.New(),
		podID:   uuid.New(),
		jobID:   uuid.New(),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email, role)
		VALUES ($1, $2, $2, $3, 'admin')
	`, fixture.ownerID, fixture.ownerID.String(), fixture.ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT vlan_tag, subnet
		FROM vlan_pool
		WHERE pod_id IS NULL
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&fixture.vlanTag, &fixture.subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet, error_message)
		VALUES ($1, $2, 'never-applied create', $3, $4, $5, 'placement failed')
	`, fixture.podID, fixture.ownerID, status, fixture.vlanTag, fixture.subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = $1, allocated_at = now() WHERE vlan_tag = $2
	`, fixture.podID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by)
		VALUES ($1, 'pod_create', jsonb_build_object('pod_id', $2::text), 'in_progress', 'test-worker')
	`, fixture.jobID, fixture.podID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pod_portgroup_receipts WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE id = $1`, fixture.jobID)
		_, _ = pool.Exec(cleanupCtx, `UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.ownerID)
	})
	return fixture
}

func (f *neverAppliedCreateFixture) insertLiveReceipt(t *testing.T) {
	t.Helper()
	receipt, err := json.Marshal(map[string]any{
		"name":    "Pod-VLAN" + f.subnet,
		"vlan_id": f.vlanTag,
		"hosts": []map[string]any{{
			"host_name":     "esxi1.lab.jmal.io",
			"host_moref":    "host-1002",
			"compute_moref": "domain-c9",
			"vswitch_name":  "vSwitch0",
			"security": map[string]any{
				"allow_promiscuous": nil,
				"mac_changes":       nil,
				"forged_transmits":  nil,
			},
			"preexisting": false,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO pod_portgroup_receipts (pod_id, create_job_id, receipt, state)
		VALUES ($1, $2, $3::jsonb, 'planned')
	`, f.podID, f.jobID, string(receipt)); err != nil {
		t.Fatal(err)
	}
}

func (f *neverAppliedCreateFixture) podAndVLAN(t *testing.T) (status string, vlanHeld bool) {
	t.Helper()
	var held *uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `
		SELECT p.status, v.pod_id
		FROM pods p
		JOIN vlan_pool v ON v.vlan_tag = p.vlan_id
		WHERE p.id = $1
	`, f.podID).Scan(&status, &held); err != nil {
		t.Fatal(err)
	}
	return status, held != nil && *held == f.podID
}

func TestFinalizeNeverAppliedPodCreate_NoReceiptReleasesVLAN(t *testing.T) {
	fixture := newNeverAppliedCreateFixture(t, models.PodStatusError)
	ctx := context.Background()

	finalized, err := fixture.queries.FinalizeNeverAppliedPodCreate(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Fatal("expected never-applied create to finalize when no receipt exists")
	}
	status, vlanHeld := fixture.podAndVLAN(t)
	if status != models.PodStatusDestroyed {
		t.Fatalf("pod status = %q, want destroyed", status)
	}
	if vlanHeld {
		t.Fatal("VLAN remained held after never-applied finalize")
	}
	var errMsg string
	if err := fixture.pool.QueryRow(ctx, `SELECT COALESCE(error_message, '') FROM pods WHERE id = $1`, fixture.podID).Scan(&errMsg); err != nil {
		t.Fatal(err)
	}
	if errMsg != "" {
		t.Fatalf("error_message = %q, want empty", errMsg)
	}
}

func TestFinalizeNeverAppliedPodCreate_LiveReceiptDoesNotAutoDestroy(t *testing.T) {
	fixture := newNeverAppliedCreateFixture(t, models.PodStatusError)
	fixture.insertLiveReceipt(t)

	finalized, err := fixture.queries.FinalizeNeverAppliedPodCreate(context.Background(), fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Fatal("live planned receipt must leave lifecycle to destroy/receipt path")
	}
	status, vlanHeld := fixture.podAndVLAN(t)
	if status != models.PodStatusError {
		t.Fatalf("pod status = %q, want error", status)
	}
	if !vlanHeld {
		t.Fatal("VLAN must stay held while a live receipt exists")
	}
}

func TestFinalizeNeverAppliedPodCreate_SabotageOmittingFinalizeLeavesVLANHeld(t *testing.T) {
	fixture := newNeverAppliedCreateFixture(t, models.PodStatusError)
	status, vlanHeld := fixture.podAndVLAN(t)
	if status != models.PodStatusError || !vlanHeld {
		t.Fatalf("precondition failed: status=%q vlanHeld=%v", status, vlanHeld)
	}
	// Sabotage: compensation stopped after empty rollback without calling FinalizeNeverAppliedPodCreate.
	if status == models.PodStatusDestroyed || !vlanHeld {
		t.Fatal("sabotage expected error + held VLAN without finalize")
	}
	finalized, err := fixture.queries.FinalizeNeverAppliedPodCreate(context.Background(), fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Fatal("restore finalize step must succeed")
	}
	status, vlanHeld = fixture.podAndVLAN(t)
	if status != models.PodStatusDestroyed || vlanHeld {
		t.Fatalf("after finalize: status=%q vlanHeld=%v", status, vlanHeld)
	}
}

func TestFinalizeNeverAppliedPodCreate_RejectsUnsafeStatus(t *testing.T) {
	fixture := newNeverAppliedCreateFixture(t, models.PodStatusActive)
	_, err := fixture.queries.FinalizeNeverAppliedPodCreate(context.Background(), fixture.podID)
	if !errors.Is(err, ErrUnsafePodCreateCleanupState) {
		t.Fatalf("err = %v, want ErrUnsafePodCreateCleanupState", err)
	}
	status, vlanHeld := fixture.podAndVLAN(t)
	if status != models.PodStatusActive || !vlanHeld {
		t.Fatalf("unsafe finalize mutated state: status=%q vlanHeld=%v", status, vlanHeld)
	}
}
