package database

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jmal1/selfservice-api/internal/models"
)

type placementPostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *Queries
	ownerID    uuid.UUID
	templateID uuid.UUID
	podID      uuid.UUID
	jobID      uuid.UUID
	podVMIDs   []uuid.UUID
	workerID   string
}

func newPlacementPostgresFixture(t *testing.T, vmCount int) *placementPostgresFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run placement durability tests")
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

	fixture := &placementPostgresFixture{
		pool:       pool,
		queries:    &Queries{pool: pool},
		ownerID:    uuid.New(),
		templateID: uuid.New(),
		podID:      uuid.New(),
		jobID:      uuid.New(),
		workerID:   "placement-test-" + uuid.NewString(),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, fixture.ownerID, fixture.ownerID.String(), fixture.ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (id, name, vcenter_template, os_type)
		VALUES ($1, $2, $3, 'linux')
	`, fixture.templateID, "placement-"+fixture.templateID.String(), "legacy-source-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
		VALUES ($1, $2, $3, 'provisioning', 3999, '10.253.255.0/24')
	`, fixture.podID, fixture.ownerID, "placement-"+fixture.podID.String()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < vmCount; i++ {
		id := uuid.New()
		fixture.podVMIDs = append(fixture.podVMIDs, id)
		if _, err := pool.Exec(ctx, `
			INSERT INTO pod_vms (
				id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
			)
			VALUES ($1, $2, $3, $4, 1, 1024, 10, 'pending')
		`, id, fixture.podID, fixture.templateID, "vm-"+id.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_create', '{}', 'in_progress', $2, now())
	`, fixture.jobID, fixture.workerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM vm_placements WHERE job_id = $1`, fixture.jobID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE id = $1`, fixture.jobID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.ownerID)
	})
	return fixture
}

func placementCandidate(
	fixture *placementPostgresFixture,
	podVMID uuid.UUID,
	host, source string,
) models.VMPlacement {
	return models.VMPlacement{
		PodVMID:              podVMID,
		JobID:                fixture.jobID,
		TemplateID:           fixture.templateID,
		SourceRef:            source,
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		ResourcePoolMoref:    "resgroup-1",
		HostMoref:            host,
		HostName:             host + ".example.invalid",
		DRSControl:           models.VMPlacementDRSDisabled,
		ObservedFreeMemoryMB: 32768,
		ReservedMemoryMB:     4096,
	}
}

func TestPrepareVMPlacementPlanPostgresIsAtomicAndRetryStable(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 2)
	ctx := context.Background()
	candidates := []models.VMPlacement{
		placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-101"),
		placementCandidate(fixture, fixture.podVMIDs[1], "host-2", "vm-102"),
	}
	first, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		candidates,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 {
		t.Fatalf("persisted placements = %d, want 2", len(first))
	}

	replanned := []models.VMPlacement{
		placementCandidate(fixture, fixture.podVMIDs[0], "host-9", "vm-901"),
		placementCandidate(fixture, fixture.podVMIDs[1], "host-9", "vm-902"),
	}
	retried, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		replanned,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, placement := range retried {
		if placement.HostMoref == "host-9" || strings.HasPrefix(placement.SourceRef, "vm-9") {
			t.Fatalf("retry replaced durable identity: %+v", placement)
		}
	}

	if _, err := fixture.queries.LoadVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podVMIDs[:1],
	); err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial-plan load error = %v, want fail-closed partial diagnosis", err)
	}
}

func TestPrepareVMPlacementPlanPostgresLegacyFallbackStopsAtFirstReplica(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	now := time.Now().UTC()
	replica := &models.TemplateSourceReplica{
		TemplateID:           fixture.templateID,
		SourceVMMoref:        "vm-201",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		ComputeResourcePath:  "/DC0/host/Cluster",
		Status:               models.TemplateSourceReplicaReady,
		LastValidatedAt:      &now,
	}
	if err := fixture.queries.CreateTemplateSourceReplica(context.Background(), replica); err != nil {
		t.Fatal(err)
	}

	legacy := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-legacy")
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		context.Background(),
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{legacy},
	); err == nil || !strings.Contains(err.Error(), "legacy source fallback is prohibited") {
		t.Fatalf("legacy fallback error = %v", err)
	}

	var placementCount int
	if err := fixture.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM vm_placements WHERE job_id = $1`,
		fixture.jobID,
	).Scan(&placementCount); err != nil {
		t.Fatal(err)
	}
	if placementCount != 0 {
		t.Fatalf("legacy rejection left %d partial placements", placementCount)
	}

	withReplica := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", replica.SourceVMMoref)
	withReplica.SourceReplicaID = &replica.ID
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		context.Background(),
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{withReplica},
	); err != nil {
		t.Fatalf("persist validated source replica placement: %v", err)
	}
	if _, err := fixture.pool.Exec(
		context.Background(),
		`UPDATE template_source_replicas SET source_vm_moref = 'vm-999' WHERE id = $1`,
		replica.ID,
	); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("source identity update error = %v, want immutable trigger rejection", err)
	}
}

func TestPrepareVMPlacementPlanPostgresDeletingAllReplicasDoesNotRestoreLegacyFallback(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	now := time.Now().UTC()
	replica := &models.TemplateSourceReplica{
		TemplateID:           fixture.templateID,
		SourceVMMoref:        "vm-201",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		ComputeResourcePath:  "/DC0/host/Cluster",
		Status:               models.TemplateSourceReplicaReady,
		LastValidatedAt:      &now,
	}
	if err := fixture.queries.CreateTemplateSourceReplica(ctx, replica); err != nil {
		t.Fatal(err)
	}
	deleted, err := fixture.queries.DeleteTemplateSourceReplica(ctx, fixture.templateID, replica.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("delete source replica = false, want true")
	}
	replicaMode, err := fixture.queries.TemplateSourceReplicaModeEnabled(ctx, fixture.templateID)
	if err != nil {
		t.Fatal(err)
	}
	if !replicaMode {
		t.Fatal("source-replica mode disabled after deleting last replica")
	}

	legacy := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-legacy")
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{legacy},
	); err == nil || !strings.Contains(err.Error(), "legacy source fallback is prohibited") {
		t.Fatalf("legacy fallback error after replica deletion = %v", err)
	}
}
