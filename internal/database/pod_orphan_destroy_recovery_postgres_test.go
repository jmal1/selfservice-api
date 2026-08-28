package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

type orphanDestroyRecoveryFixture struct {
	pool    *pgxpool.Pool
	queries *Queries
	ownerID uuid.UUID
	podID   uuid.UUID
	vlanTag int
	subnet  string
	jobID   uuid.UUID
}

func newOrphanDestroyRecoveryFixture(t *testing.T) *orphanDestroyRecoveryFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run orphaned destroy recovery tests")
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
	fixture := &orphanDestroyRecoveryFixture{
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
		VALUES ($1, $2, 'student test', 'destroy_failed', $3, $4, $5)
	`, fixture.podID, fixture.ownerID, fixture.vlanTag, fixture.subnet, models.PodErrorManualCleanupRequiredPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = $1, allocated_at = now() WHERE vlan_tag = $2
	`, fixture.podID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, result)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', jsonb_build_object('status', 'manual_cleanup_required'))
	`, fixture.jobID, fixture.podID); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func assertOrphanRecoveryState(t *testing.T, pool *pgxpool.Pool, podID uuid.UUID, wantStatus string, wantVLAN bool, wantAudit int) {
	t.Helper()
	var podStatus string
	var vlanOwned bool
	var auditCount int
	if err := pool.QueryRow(context.Background(), `
		SELECT
			(SELECT status FROM pods WHERE id = $1),
			EXISTS (SELECT 1 FROM vlan_pool WHERE pod_id = $1),
			(SELECT count(*) FROM pod_destroy_recovery_audit WHERE pod_id = $1)
	`, podID).Scan(&podStatus, &vlanOwned, &auditCount); err != nil {
		t.Fatal(err)
	}
	if podStatus != wantStatus || vlanOwned != wantVLAN || auditCount != wantAudit {
		t.Fatalf("state = %q/%v/%d, want %q/%v/%d", podStatus, vlanOwned, auditCount, wantStatus, wantVLAN, wantAudit)
	}
}

func TestFinalizeOrphanedPodDestroyPostgresFinalizesAndAudits(t *testing.T) {
	fixture := newOrphanDestroyRecoveryFixture(t)
	ctx := context.Background()
	snap, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.PodID != fixture.podID || snap.DestroyJobID != fixture.jobID {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	assertOrphanRecoveryState(t, fixture.pool, fixture.podID, models.PodStatusDestroyed, false, 1)
}

func TestFinalizeOrphanedPodDestroyPostgresRejectsBadAttestation(t *testing.T) {
	fixture := newOrphanDestroyRecoveryFixture(t)
	ctx := context.Background()
	if _, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag + 1,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	}); !errors.Is(err, ErrPodDestroyRecoveryPrecondition) {
		t.Fatalf("attestation error = %v, want %v", err, ErrPodDestroyRecoveryPrecondition)
	}
	assertOrphanRecoveryState(t, fixture.pool, fixture.podID, models.PodStatusDestroyFailed, true, 0)
}

func TestFinalizeOrphanedPodDestroyPostgresRejectsWrongStatus(t *testing.T) {
	fixture := newOrphanDestroyRecoveryFixture(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `UPDATE pods SET status = 'destroying' WHERE id = $1`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	}); !errors.Is(err, ErrPodDestroyRecoveryPrecondition) {
		t.Fatalf("status error = %v, want %v", err, ErrPodDestroyRecoveryPrecondition)
	}
}

func TestFinalizeOrphanedPodDestroyPostgresRejectsCompetingWork(t *testing.T) {
	fixture := newOrphanDestroyRecoveryFixture(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (type, payload, status)
		VALUES ('vm_start', jsonb_build_object('pod_id', $1::text), 'claimed')
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	}); !errors.Is(err, ErrPodDestroyRecoveryPrecondition) {
		t.Fatalf("competing job error = %v, want %v", err, ErrPodDestroyRecoveryPrecondition)
	}
}

func TestFinalizeOrphanedPodDestroyPostgresRejectsRepeatedFinalize(t *testing.T) {
	fixture := newOrphanDestroyRecoveryFixture(t)
	ctx := context.Background()
	if _, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.FinalizeOrphanedPodDestroy(ctx, fixture.ownerID, fixture.podID, PodDestroyRecoveryAttestation{
		VLANID:            fixture.vlanTag,
		Subnet:            fixture.subnet,
		ConfirmationToken: "orphan-123",
	}); !errors.Is(err, ErrPodDestroyRecoveryPrecondition) {
		t.Fatalf("repeat finalize error = %v, want %v", err, ErrPodDestroyRecoveryPrecondition)
	}
}
