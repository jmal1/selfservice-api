package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		VALUES (
			$1,
			'pod_create',
			jsonb_build_object('pod_id', $3::text),
			'in_progress',
			$2,
			now()
		)
	`, fixture.jobID, fixture.workerID, fixture.podID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM vm_placements WHERE job_id = $1`, fixture.jobID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pod_portgroup_receipts WHERE pod_id = $1`, fixture.podID)
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
		PodVMID:               podVMID,
		JobID:                 fixture.jobID,
		TemplateID:            fixture.templateID,
		SourceRef:             source,
		ComputeResourceType:   "ClusterComputeResource",
		ComputeResourceMoref:  "domain-c1",
		ResourcePoolMoref:     "resgroup-1",
		HostMoref:             host,
		HostName:              host + ".example.invalid",
		DRSControl:            models.VMPlacementDRSDisabled,
		ObservedFreeMemoryMB:  32768,
		ReservedMemoryMB:      4096,
		CapacityReservationMB: 1024,
		CapacityObservedAt:    time.Now().UTC(),
	}
}

func TestRetryJobPostgresForwardRetryPreservesExactCloneIdentity(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	operationID := uuid.NewString()
	operation := models.VMCloneOperation{
		OperationID:          operationID,
		PodID:                fixture.podID.String(),
		PodVMID:              fixture.podVMIDs[0].String(),
		LogicalTemplateID:    fixture.templateID.String(),
		TargetName:           "student-vm",
		SourceRef:            "vm-source",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		HostMoref:            "host-1",
		HostName:             "esxi1.example.invalid",
		PoolMoref:            "resgroup-1",
		DRSControl:           models.VMPlacementDRSDisabled,
		TaskRef:              "task-4242",
		Phase:                models.VMCloneOperationSubmitted,
		PreparedAt:           time.Now().UTC(),
	}
	payload, err := json.Marshal(map[string]any{
		"cleanup_only":      true,
		"cleanup_completed": true,
		"cleanup_target": map[string]string{
			"pod_vm_id":     fixture.podVMIDs[0].String(),
			"vcenter_vm_id": "vm-4242",
			"host_moref":    "host-1",
			"pool_moref":    "resgroup-1",
		},
		"clone_operation": operation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE jobs
		SET payload = $2
		WHERE id = $1
	`, fixture.jobID, payload); err != nil {
		t.Fatal(err)
	}

	loaded, err := fixture.queries.GetVMCloneOperation(ctx, fixture.jobID, fixture.workerID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil ||
		loaded.OperationID != operationID ||
		loaded.TaskRef != operation.TaskRef ||
		loaded.SourceRef != operation.SourceRef ||
		loaded.ComputeResourceMoref != operation.ComputeResourceMoref ||
		loaded.HostMoref != operation.HostMoref ||
		loaded.PoolMoref != operation.PoolMoref {
		t.Fatalf("loaded clone operation changed exact identity: %+v", loaded)
	}

	if err := fixture.queries.RetryJob(
		ctx,
		fixture.jobID,
		time.Now().Add(time.Minute),
		false,
		nil,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}

	var (
		status     string
		retryCount int
		claimedBy  *string
		stored     []byte
	)
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, retry_count, claimed_by, payload
		FROM jobs
		WHERE id = $1
	`, fixture.jobID).Scan(&status, &retryCount, &claimedBy, &stored); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusPending || retryCount != 1 || claimedBy != nil {
		t.Fatalf(
			"forward retry state = status:%s retry_count:%d claimed_by:%v",
			status,
			retryCount,
			claimedBy,
		)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stored, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["cleanup_only"]; exists {
		t.Fatal("forward retry retained cleanup_only and would dispatch compensation")
	}
	if _, exists := fields["cleanup_completed"]; exists {
		t.Fatal("forward retry retained stale cleanup completion")
	}
	var decodedOperation map[string]string
	if err := json.Unmarshal(fields["clone_operation"], &decodedOperation); err != nil {
		t.Fatal(err)
	}
	if decodedOperation["operation_id"] != operationID ||
		decodedOperation["task_ref"] != "task-4242" ||
		decodedOperation["host_moref"] != "host-1" ||
		decodedOperation["pool_moref"] != "resgroup-1" {
		t.Fatalf("forward retry changed exact clone operation: %+v", decodedOperation)
	}
	var target map[string]string
	if err := json.Unmarshal(fields["cleanup_target"], &target); err != nil {
		t.Fatal(err)
	}
	if target["vcenter_vm_id"] != "vm-4242" ||
		target["host_moref"] != "host-1" ||
		target["pool_moref"] != "resgroup-1" {
		t.Fatalf("forward retry changed exact cleanup target: %+v", target)
	}
}

func TestAdoptPodVMClonePostgresRefusesDifferentExistingReference(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	podVMID := fixture.podVMIDs[0]
	payload, err := json.Marshal(map[string]any{
		"cleanup_target": map[string]string{
			"pod_id":        fixture.podID.String(),
			"pod_vm_id":     podVMID.String(),
			"vcenter_vm_id": "vm-resumed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE jobs SET payload = $2 WHERE id = $1
	`, fixture.jobID, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_vms
		SET status = 'cloning',
		    vcenter_vm_id = 'vm-existing',
		    vcenter_vm_name = 'existing'
		WHERE id = $1
	`, podVMID); err != nil {
		t.Fatal(err)
	}

	applied, err := fixture.queries.AdoptPodVMClone(
		ctx,
		fixture.jobID,
		fixture.workerID,
		podVMID,
		[]string{models.VMStatusCloning},
		"vm-resumed",
		"resumed",
		models.VMStatusConfiguring,
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("clone adoption overwrote a different existing vCenter reference")
	}
	var (
		moref  *string
		status string
	)
	if err := fixture.pool.QueryRow(ctx, `
		SELECT vcenter_vm_id, status
		FROM pod_vms
		WHERE id = $1
	`, podVMID).Scan(&moref, &status); err != nil {
		t.Fatal(err)
	}
	if moref == nil || *moref != "vm-existing" || status != models.VMStatusCloning {
		t.Fatalf("existing VM reference changed to moref=%v status=%s", moref, status)
	}
}

type placementAdmissionJob struct {
	jobID    uuid.UUID
	workerID string
	podVMID  uuid.UUID
}

func addPlacementAdmissionJob(
	t *testing.T,
	fixture *placementPostgresFixture,
	ramMB int64,
) placementAdmissionJob {
	t.Helper()
	job := placementAdmissionJob{
		jobID:    uuid.New(),
		workerID: "placement-admission-" + uuid.NewString(),
		podVMID:  uuid.New(),
	}
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO pod_vms (
			id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
		)
		VALUES ($1, $2, $3, $4, 1, $5, 10, 'pending')
	`, job.podVMID, fixture.podID, fixture.templateID, "vm-"+job.podVMID.String(), ramMB); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_create', '{}', 'in_progress', $2, now())
	`, job.jobID, job.workerID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM vm_placements WHERE job_id = $1`, job.jobID)
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE id = $1`, job.jobID)
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM pod_vms WHERE id = $1`, job.podVMID)
	})
	return job
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

func TestPrepareVMPlacementPlanPostgresSerializesConcurrentHostAdmission(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 0)
	first := addPlacementAdmissionJob(t, fixture, 8192)
	second := addPlacementAdmissionJob(t, fixture, 8192)
	ctx := context.Background()
	staleObservedAt, err := fixture.queries.BeginHostCapacityObservation(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION test_delay_vm_placement_insert()
		RETURNS TRIGGER AS $$
		BEGIN
			PERFORM pg_sleep(0.5);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER test_delay_vm_placement_insert
		BEFORE INSERT ON vm_placements
		FOR EACH ROW EXECUTE FUNCTION test_delay_vm_placement_insert()
	`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = fixture.pool.Exec(cleanupCtx, `
			DROP TRIGGER IF EXISTS test_delay_vm_placement_insert ON vm_placements;
			DROP FUNCTION IF EXISTS test_delay_vm_placement_insert()
		`)
	})

	type result struct {
		job placementAdmissionJob
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, job := range []placementAdmissionJob{first, second} {
		job := job
		go func() {
			<-start
			candidate := placementCandidate(fixture, job.podVMID, "host-1", "vm-101")
			candidate.JobID = job.jobID
			candidate.ObservedFreeMemoryMB = 16384
			candidate.ReservedMemoryMB = 4096
			candidate.CapacityReservationMB = 8192
			candidate.CapacityObservedAt = staleObservedAt
			_, err := fixture.queries.PrepareVMPlacementPlan(
				context.Background(),
				job.jobID,
				job.workerID,
				[]models.VMPlacement{candidate},
			)
			results <- result{job: job, err: err}
		}()
	}
	close(start)

	var admitted, rejected *placementAdmissionJob
	for range 2 {
		got := <-results
		switch {
		case got.err == nil:
			if admitted != nil {
				t.Fatalf("both concurrent jobs were admitted: %s and %s", admitted.jobID, got.job.jobID)
			}
			admitted = &got.job
		case errors.Is(got.err, ErrHostCapacityAdmission):
			if rejected != nil {
				t.Fatalf("both concurrent jobs were rejected: %v", got.err)
			}
			rejected = &got.job
		default:
			t.Fatalf("unexpected concurrent admission error for job %s: %v", got.job.jobID, got.err)
		}
	}
	if admitted == nil || rejected == nil {
		t.Fatalf("admitted=%v rejected=%v, want exactly one of each", admitted, rejected)
	}

	var placementCount int
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT count(*) FROM vm_placements WHERE host_moref = 'host-1' AND job_id IN ($1, $2)`,
		first.jobID,
		second.jobID,
	).Scan(&placementCount); err != nil {
		t.Fatal(err)
	}
	if placementCount != 1 {
		t.Fatalf("durable host reservations = %d, want 1", placementCount)
	}

	if err := fixture.queries.ReleaseVMPlacementCapacity(
		ctx,
		admitted.jobID,
		admitted.workerID,
		[]uuid.UUID{admitted.podVMID},
	); err != nil {
		t.Fatalf("release admitted capacity reservation: %v", err)
	}
	var releasedAt time.Time
	if err := fixture.pool.QueryRow(
		ctx,
		`SELECT capacity_released_at FROM vm_placements WHERE job_id = $1`,
		admitted.jobID,
	).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	retryCandidate := placementCandidate(fixture, rejected.podVMID, "host-1", "vm-101")
	retryCandidate.JobID = rejected.jobID
	retryCandidate.ObservedFreeMemoryMB = 16384
	retryCandidate.ReservedMemoryMB = 4096
	retryCandidate.CapacityReservationMB = 8192
	retryCandidate.CapacityObservedAt = staleObservedAt
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		rejected.jobID,
		rejected.workerID,
		[]models.VMPlacement{retryCandidate},
	); !errors.Is(err, ErrHostCapacityAdmission) {
		t.Fatalf("stale pre-release observation error = %v, want host admission rejection", err)
	}
	retryCandidate.CapacityObservedAt = releasedAt.Add(time.Microsecond)
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		rejected.jobID,
		rejected.workerID,
		[]models.VMPlacement{retryCandidate},
	); err != nil {
		t.Fatalf("explicit release did not free durable capacity reservation: %v", err)
	}
}

func TestPrepareVMPlacementPlanPostgresRetainsUnresolvedTerminalReservation(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 0)
	first := addPlacementAdmissionJob(t, fixture, 8192)
	second := addPlacementAdmissionJob(t, fixture, 8192)
	ctx := context.Background()

	observedAt, err := fixture.queries.BeginHostCapacityObservation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstCandidate := placementCandidate(fixture, first.podVMID, "host-1", "vm-101")
	firstCandidate.JobID = first.jobID
	firstCandidate.ObservedFreeMemoryMB = 16384
	firstCandidate.ReservedMemoryMB = 4096
	firstCandidate.CapacityReservationMB = 8192
	firstCandidate.CapacityObservedAt = observedAt
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		first.jobID,
		first.workerID,
		[]models.VMPlacement{firstCandidate},
	); err != nil {
		t.Fatalf("admit first placement: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'failed', completed_at = clock_timestamp()
		WHERE id = $1
	`, first.jobID); err != nil {
		t.Fatal(err)
	}

	freshObservedAt, err := fixture.queries.BeginHostCapacityObservation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondCandidate := placementCandidate(fixture, second.podVMID, "host-1", "vm-101")
	secondCandidate.JobID = second.jobID
	secondCandidate.ObservedFreeMemoryMB = 16384
	secondCandidate.ReservedMemoryMB = 4096
	secondCandidate.CapacityReservationMB = 8192
	secondCandidate.CapacityObservedAt = freshObservedAt
	if _, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		second.jobID,
		second.workerID,
		[]models.VMPlacement{secondCandidate},
	); !errors.Is(err, ErrHostCapacityAdmission) {
		t.Fatalf("unresolved terminal placement error = %v, want host admission rejection", err)
	}
}

func TestStageVMCloneCleanupPostgresRecognizesExpandedDestroyedTarget(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	moref := "vm-4242"
	target, err := json.Marshal(map[string]string{
		"pod_id":                 fixture.podID.String(),
		"pod_vm_id":              fixture.podVMIDs[0].String(),
		"vcenter_vm_id":          moref,
		"logical_template_id":    fixture.templateID.String(),
		"source_replica_id":      uuid.NewString(),
		"source_ref":             "vm-101",
		"compute_resource_type":  "ClusterComputeResource",
		"compute_resource_moref": "domain-c1",
		"resource_pool_moref":    "resgroup-1",
		"host_moref":             "host-1",
		"drs_control":            "disabled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.StageVMCloneCleanup(
		ctx,
		fixture.jobID,
		fixture.workerID,
		target,
	); err != nil {
		t.Fatalf("stage expanded cleanup target: %v", err)
	}
	if err := fixture.queries.CompleteVMCloneCleanup(
		ctx,
		fixture.jobID,
		fixture.podVMIDs[0],
		moref,
	); err != nil {
		t.Fatalf("complete expanded cleanup target: %v", err)
	}
	if err := fixture.queries.StageVMCloneCleanup(
		ctx,
		fixture.jobID,
		fixture.workerID,
		target,
	); !errors.Is(err, ErrVMCloneAlreadyDestroyed) {
		t.Fatalf("restage destroyed expanded target error = %v, want ErrVMCloneAlreadyDestroyed", err)
	}
}

func TestPrepareVMPlacementPlanPostgresRejectsUnderstatedMemory(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	candidate := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-101")
	candidate.CapacityReservationMB = 1

	_, err := fixture.queries.PrepareVMPlacementPlan(
		context.Background(),
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{candidate},
	)
	if err == nil || !strings.Contains(err.Error(), "does not match required reservation") {
		t.Fatalf("understated memory error = %v, want required-reservation rejection", err)
	}
}

func TestPrepareVMPlacementPlanPostgresRejectsNonpositiveConfiguredMemory(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(
		ctx,
		`UPDATE pod_vms SET ram_mb = 0 WHERE id = $1`,
		fixture.podVMIDs[0],
	); err != nil {
		t.Fatal(err)
	}
	candidate := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-101")
	candidate.CapacityReservationMB = 0

	_, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{candidate},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid configured RAM") {
		t.Fatalf("nonpositive configured memory error = %v, want rejection", err)
	}
}

func TestPrepareVMPlacementPlanPostgresDoesNotRechargeResidentVM(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(
		ctx,
		`UPDATE pod_vms SET vcenter_vm_id = 'vm-777' WHERE id = $1`,
		fixture.podVMIDs[0],
	); err != nil {
		t.Fatal(err)
	}
	candidate := placementCandidate(fixture, fixture.podVMIDs[0], "host-1", "vm-101")
	candidate.ObservedFreeMemoryMB = 0
	candidate.ReservedMemoryMB = 4096
	candidate.CapacityReservationMB = 0
	candidate.LegacyAdoptionPending = true

	placements, err := fixture.queries.PrepareVMPlacementPlan(
		ctx,
		fixture.jobID,
		fixture.workerID,
		[]models.VMPlacement{candidate},
	)
	if err != nil {
		t.Fatalf("persist resident VM placement without recharging capacity: %v", err)
	}
	if len(placements) != 1 || placements[0].CapacityReservationMB != 0 {
		t.Fatalf("resident VM placement = %+v, want zero capacity reservation", placements)
	}
	if !placements[0].LegacyAdoptionPending {
		t.Fatalf("resident VM placement = %+v, want pending legacy adoption", placements[0])
	}
	if err := fixture.queries.CompleteLegacyVMPlacementAdoption(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podVMIDs[0],
	); err != nil {
		t.Fatalf("complete legacy placement adoption: %v", err)
	}
	placement, err := fixture.queries.GetVMPlacement(ctx, fixture.podVMIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if placement == nil || placement.LegacyAdoptionPending {
		t.Fatalf("completed legacy placement = %+v, want adoption marker cleared", placement)
	}
}

func validateHostCapacityAdmissionWiring(sources map[string]string) error {
	checks := map[string][]string{
		"placements.go": {
			"pg_advisory_xact_lock",
			"SUM(vp.capacity_reservation_mb)",
			"vp.capacity_released_at IS NULL OR vp.capacity_released_at >= $3",
			"ReleaseVMPlacementCapacity",
			"ErrHostCapacityAdmission",
		},
		"migrations/000033_multi_cluster_placement.up.sql": {
			"capacity_reservation_mb BIGINT NOT NULL CHECK (capacity_reservation_mb >= 0)",
			"capacity_observed_at TIMESTAMPTZ NOT NULL",
			"capacity_released_at TIMESTAMPTZ",
			"admitted_headroom_mb BIGINT NOT NULL CHECK (admitted_headroom_mb >= 0)",
			"legacy_adoption_pending BOOLEAN NOT NULL DEFAULT false",
		},
		"../provisioner/placement_plan.go": {
			"CapacityReservationMB: capacityReservationMB",
			"BeginHostCapacityObservation",
			"database.ErrHostCapacityAdmission",
		},
		"../provisioner/create.go": {
			"releaseVMPlacementCapacity",
			"release already-active pod VM capacity reservations",
			"release running pod VM capacity reservations",
			"release compensated VM capacity",
		},
		"../provisioner/vm_ops.go": {
			"releaseVMPlacementCapacity",
			"release running added VM capacity reservation",
			"release compensated vm_add capacity",
		},
	}
	for path, required := range checks {
		body, ok := sources[path]
		if !ok {
			return fmt.Errorf("host-admission source %s is missing", path)
		}
		for _, needle := range required {
			if !strings.Contains(body, needle) {
				return fmt.Errorf("%s is missing host-admission guard %q", path, needle)
			}
		}
	}
	return nil
}

func TestHostCapacityAdmissionGuardIsProductionCoupled(t *testing.T) {
	t.Parallel()
	paths := []string{
		"placements.go",
		"migrations/000033_multi_cluster_placement.up.sql",
		"../provisioner/placement_plan.go",
		"../provisioner/create.go",
		"../provisioner/vm_ops.go",
	}
	sources := make(map[string]string, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sources[path] = string(body)
	}
	if err := validateHostCapacityAdmissionWiring(sources); err != nil {
		t.Fatal(err)
	}

	sabotages := map[string]map[string]string{
		"remove-host-lock": {
			"placements.go": strings.ReplaceAll(
				sources["placements.go"],
				"pg_advisory_xact_lock",
				"removed_advisory_lock",
			),
		},
		"remove-durable-sum": {
			"placements.go": strings.ReplaceAll(
				sources["placements.go"],
				"SUM(vp.capacity_reservation_mb)",
				"0",
			),
		},
		"remove-explicit-release-state": {
			"placements.go": strings.ReplaceAll(
				sources["placements.go"],
				"vp.capacity_released_at IS NULL OR vp.capacity_released_at >= $3",
				"true",
			),
		},
		"remove-production-reservation": {
			"../provisioner/placement_plan.go": strings.ReplaceAll(
				sources["../provisioner/placement_plan.go"],
				"CapacityReservationMB: capacityReservationMB",
				"CapacityReservationMB: 0",
			),
		},
		"remove-pod-release": {
			"../provisioner/create.go": strings.ReplaceAll(
				sources["../provisioner/create.go"],
				"release running pod VM capacity reservations",
				"removed pod capacity release",
			),
		},
		"remove-active-pod-retry-release": {
			"../provisioner/create.go": strings.ReplaceAll(
				sources["../provisioner/create.go"],
				"release already-active pod VM capacity reservations",
				"removed active-pod capacity release",
			),
		},
		"remove-vm-add-release": {
			"../provisioner/vm_ops.go": strings.ReplaceAll(
				sources["../provisioner/vm_ops.go"],
				"release running added VM capacity reservation",
				"removed VM capacity release",
			),
		},
	}
	for name, overrides := range sabotages {
		t.Run(name, func(t *testing.T) {
			sabotaged := make(map[string]string, len(sources))
			for path, body := range sources {
				sabotaged[path] = body
			}
			for path, body := range overrides {
				sabotaged[path] = body
			}
			if err := validateHostCapacityAdmissionWiring(sabotaged); err == nil {
				t.Fatal("sabotaged host-capacity admission wiring unexpectedly passed")
			}
		})
	}
}
