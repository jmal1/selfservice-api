package database

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jmal1/selfservice-api/internal/models"
)

type replicaBuildPostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *Queries
	templateID uuid.UUID
	anchor     *models.TemplateSourceReplica
}

func newReplicaBuildPostgresFixture(t *testing.T) *replicaBuildPostgresFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run replica build durability tests")
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
	fixture := &replicaBuildPostgresFixture{
		pool:       pool,
		queries:    NewQueries(pool),
		templateID: uuid.New(),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (id, name, vcenter_template, os_type)
		VALUES ($1, $2, $3, 'linux')
	`, fixture.templateID, "replica-build-"+fixture.templateID.String(), "source-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fixture.anchor = &models.TemplateSourceReplica{
		TemplateID:           fixture.templateID,
		SourceVMMoref:        "vm-3401",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c3401",
		ComputeResourcePath:  "/DC/host/Source",
		Status:               models.TemplateSourceReplicaReady,
		LastValidatedAt:      &now,
	}
	if err := fixture.queries.CreateTemplateSourceReplica(ctx, fixture.anchor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replica_builds WHERE template_id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE type = 'template_replica_build' AND payload->>'template_id' = $1`, fixture.templateID.String())
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replicas WHERE template_id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM template_source_replica_policies WHERE template_id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
	})
	return fixture
}

func (f *replicaBuildPostgresFixture) build(key string) *models.TemplateReplicaBuild {
	return &models.TemplateReplicaBuild{
		TemplateID:           f.templateID,
		SourceReplicaID:      f.anchor.ID,
		IdempotencyKey:       key,
		OperationID:          uuid.NewString(),
		CanaryOperationID:    uuid.NewString(),
		SourceVMMoref:        f.anchor.SourceVMMoref,
		SourceSnapshotName:   "base-image",
		DestinationName:      "replica-" + key,
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c3402",
		ComputeResourcePath:  "/DC/host/Target",
		HostMoref:            "host-3402",
		HostName:             "target.example.invalid",
		ResourcePoolMoref:    "resgroup-3402",
		ResourcePoolPath:     "/DC/host/Target/Resources/Students",
		DatastoreMoref:       "datastore-3402",
		DatastoreName:        "replica-ds",
		FolderMoref:          "group-v3402",
		FolderPath:           "/DC/vm/Templates",
		ProvisionDatastore:   "student-ds",
		Status:               models.TemplateReplicaBuildPending,
		Phase:                models.TemplateReplicaBuildPhasePending,
	}
}

func (f *replicaBuildPostgresFixture) claimBuildJob(
	t *testing.T,
	build *models.TemplateReplicaBuild,
	worker string,
) {
	t.Helper()
	if build == nil || build.JobID == nil {
		t.Fatal("build job identity is missing")
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE jobs SET status = 'in_progress', claimed_by = $2, claimed_at = now()
		WHERE id = $1
	`, *build.JobID, worker); err != nil {
		t.Fatal(err)
	}
}

func TestCreateTemplateReplicaBuildPostgresConcurrentIdempotency(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	const callers = 8
	results := make(chan *models.TemplateReplicaBuild, callers)
	created := make(chan bool, callers)
	errs := make(chan error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			build, wasCreated, err := fixture.queries.CreateTemplateReplicaBuild(
				context.Background(),
				fixture.build("concurrent-operation"),
				uuid.New(),
			)
			results <- build
			created <- wasCreated
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(created)
	close(errs)

	var firstID uuid.UUID
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for build := range results {
		if firstID == uuid.Nil {
			firstID = build.ID
		}
		if build.ID != firstID {
			t.Fatalf("idempotent callers returned build %s and %s", firstID, build.ID)
		}
	}
	createdCount := 0
	for wasCreated := range created {
		if wasCreated {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created callers = %d, want 1", createdCount)
	}
	var builds, jobs int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*), count(job_id) FROM template_source_replica_builds WHERE template_id = $1
	`, fixture.templateID).Scan(&builds, &jobs); err != nil {
		t.Fatal(err)
	}
	if builds != 1 || jobs != 1 {
		t.Fatalf("durable builds=%d jobs=%d, want one transactionally attached operation", builds, jobs)
	}
}

func TestCreateTemplateReplicaBuildPostgresRejectsIdempotencyInputDrift(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	first, _, err := fixture.queries.CreateTemplateReplicaBuild(
		context.Background(),
		fixture.build("drift"),
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}
	drifted := fixture.build("drift")
	drifted.DestinationName = "different-retained-vm"
	got, _, err := fixture.queries.CreateTemplateReplicaBuild(context.Background(), drifted, uuid.New())
	if !errors.Is(err, ErrTemplateReplicaBuildConflict) || got != nil {
		t.Fatalf("drifted replay build=%+v error=%v, want conflict", got, err)
	}
	loaded, err := fixture.queries.GetTemplateReplicaBuild(context.Background(), fixture.templateID, first.ID)
	if err != nil || loaded.DestinationName != first.DestinationName {
		t.Fatalf("original operation changed: build=%+v error=%v", loaded, err)
	}
}

func TestFinalizeTemplateReplicaBuildPostgresIsAnchorFirstAndCleanupGated(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	build, _, err := fixture.queries.CreateTemplateReplicaBuild(
		context.Background(),
		fixture.build("finalize"),
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("cleanup proof releases reservation", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		build, _, err := fixture.queries.CreateTemplateReplicaBuild(
			context.Background(),
			fixture.build("cleanup-release"),
			uuid.New(),
		)
		if err != nil {
			t.Fatal(err)
		}
		const worker = "replica-build-cleanup-test"
		fixture.claimBuildJob(t, build, worker)
		build.DestinationVMMoref = "vm-3402"
		build.Status = models.TemplateReplicaBuildCleanupRequired
		build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
		if err := fixture.queries.SaveTemplateReplicaBuildState(
			context.Background(),
			build,
			models.TemplateReplicaBuildPhasePending,
			worker,
		); err != nil {
			t.Fatal(err)
		}
		if err := fixture.queries.EnsurePendingReplicaForBuild(context.Background(), build, worker); err != nil {
			t.Fatal(err)
		}
		resultReplicaID := *build.ResultReplicaID
		now := time.Now().UTC()
		build.ResidueCleanedAt = &now

		sabotaged := *build
		sabotaged.DestinationVMMoref = "vm-9999"
		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			&sabotaged,
			worker,
			"must not persist mismatched cleanup",
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("mismatched cleanup error=%v, want conflict", err)
		}
		var residueProof *time.Time
		var persistedResult *uuid.UUID
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT residue_cleaned_at, result_replica_id
			FROM template_source_replica_builds
			WHERE id = $1
		`, build.ID).Scan(&residueProof, &persistedResult); err != nil {
			t.Fatal(err)
		}
		if residueProof != nil || persistedResult == nil || *persistedResult != resultReplicaID {
			t.Fatalf("sabotaged cleanup partially committed proof=%v result=%v", residueProof, persistedResult)
		}

		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			build,
			worker,
			"exact retained residue removed",
		); err != nil {
			t.Fatal(err)
		}
		loaded, err := fixture.queries.GetTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			build.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.ResidueCleanedAt == nil ||
			loaded.Status != models.TemplateReplicaBuildFailed ||
			loaded.Phase != models.TemplateReplicaBuildPhaseResidueCleaned ||
			loaded.ResultReplicaID != nil {
			t.Fatalf("cleanup terminal state did not survive reload: %+v", loaded)
		}
		unsafe, err := fixture.queries.HasUnsafeTemplateReplicaBuildForDeletion(
			context.Background(),
			fixture.templateID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if unsafe {
			t.Fatal("fully cleaned failed build still blocks template deletion")
		}
		var orphanCount int
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM template_source_replicas WHERE id = $1
		`, resultReplicaID).Scan(&orphanCount); err != nil {
			t.Fatal(err)
		}
		if orphanCount != 0 {
			t.Fatalf("non-ready result reservation still exists after exact cleanup: %d", orphanCount)
		}

		const callers = 6
		results := make(chan *models.TemplateReplicaBuild, callers)
		errs := make(chan error, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				next, _, err := fixture.queries.CreateTemplateReplicaBuild(
					context.Background(),
					fixture.build("cleanup-retry"),
					uuid.New(),
				)
				results <- next
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		var retryID uuid.UUID
		for result := range results {
			if retryID == uuid.Nil {
				retryID = result.ID
			}
			if result.ID != retryID {
				t.Fatalf("concurrent retry produced builds %s and %s", retryID, result.ID)
			}
		}
	})

	t.Run("failed pre-VM build permits template cascade", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		build, _, err := fixture.queries.CreateTemplateReplicaBuild(
			context.Background(),
			fixture.build("pre-vm-failure"),
			uuid.New(),
		)
		if err != nil {
			t.Fatal(err)
		}
		const worker = "replica-build-pre-vm-failure-test"
		fixture.claimBuildJob(t, build, worker)
		if err := fixture.queries.FailTemplateReplicaBuild(
			context.Background(),
			build,
			worker,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseFailed,
			"validation_failed",
			"failed before vCenter submission",
		); err != nil {
			t.Fatal(err)
		}
		if err := fixture.queries.DeleteTemplateWithHistory(context.Background(), fixture.templateID); err != nil {
			t.Fatalf("failed pre-VM build blocked template cascade: %v", err)
		}
		var templates, builds int
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT
				(SELECT count(*) FROM templates WHERE id = $1),
				(SELECT count(*) FROM template_source_replica_builds WHERE id = $2)
		`, fixture.templateID, build.ID).Scan(&templates, &builds); err != nil {
			t.Fatal(err)
		}
		if templates != 0 || builds != 0 {
			t.Fatalf("template/build rows after cascade=%d/%d, want 0/0", templates, builds)
		}
	})
	const worker = "replica-build-finalize-test"
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE jobs SET status = 'in_progress', claimed_by = $2, claimed_at = now()
		WHERE id = $1
	`, *build.JobID, worker); err != nil {
		t.Fatal(err)
	}
	build.DestinationVMMoref = "vm-3402"
	if err := fixture.queries.EnsurePendingReplicaForBuild(context.Background(), build, worker); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.FinalizeTemplateReplicaBuild(context.Background(), build, worker); err == nil {
		t.Fatal("finalization succeeded before linked-clone cleanup and destination snapshot")
	}
	var pendingStatus string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM template_source_replicas WHERE id = $1
	`, *build.ResultReplicaID).Scan(&pendingStatus); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != models.TemplateSourceReplicaPending {
		t.Fatalf("result replica status = %s before cleanup, want pending", pendingStatus)
	}

	now := time.Now().UTC()
	build.CleanupCompletedAt = &now
	build.DestinationSnapshot = "snapshot-3402"
	build.CanaryVMMoref = "vm-3499"
	build.Phase = models.TemplateReplicaBuildPhaseFinalizing
	build.Status = models.TemplateReplicaBuildRunning
	if err := fixture.queries.SaveTemplateReplicaBuildState(
		context.Background(),
		build,
		models.TemplateReplicaBuildPhasePending,
		worker,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE template_source_replicas SET status = 'unhealthy' WHERE id = $1
	`, fixture.anchor.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.FinalizeTemplateReplicaBuild(context.Background(), build, worker); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
		t.Fatalf("finalize with drifted anchor error = %v, want conflict", err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM template_source_replicas WHERE id = $1
	`, *build.ResultReplicaID).Scan(&pendingStatus); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != models.TemplateSourceReplicaPending {
		t.Fatalf("anchor-drift rollback changed result status to %s", pendingStatus)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE template_source_replicas SET status = 'ready' WHERE id = $1
	`, fixture.anchor.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.FinalizeTemplateReplicaBuild(context.Background(), build, worker); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.queries.GetTemplateReplicaBuild(context.Background(), fixture.templateID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != models.TemplateReplicaBuildReady || loaded.Phase != models.TemplateReplicaBuildPhaseReady {
		t.Fatalf("final build = %s/%s, want ready/ready", loaded.Status, loaded.Phase)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM template_source_replicas WHERE id = $1
	`, *build.ResultReplicaID).Scan(&pendingStatus); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != models.TemplateSourceReplicaReady {
		t.Fatalf("final result replica status = %s, want ready", pendingStatus)
	}
}
