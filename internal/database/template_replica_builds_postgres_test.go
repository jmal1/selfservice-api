package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	nextVM     int
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
		nextVM:     3500,
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
		SourceReplicaID:      &f.anchor.ID,
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

func (f *replicaBuildPostgresFixture) finishBuildJob(t *testing.T, build *models.TemplateReplicaBuild, status string) {
	t.Helper()
	if build == nil || build.JobID == nil {
		t.Fatal("build job identity is missing")
	}
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE jobs
		SET status = $2, claimed_by = NULL, claimed_at = NULL, completed_at = now()
		WHERE id = $1
	`, *build.JobID, status); err != nil {
		t.Fatal(err)
	}
}

func (f *replicaBuildPostgresFixture) readyBuild(t *testing.T, key string) *models.TemplateReplicaBuild {
	t.Helper()
	return f.finalizeBuild(t, f.build(key))
}

func (f *replicaBuildPostgresFixture) finalizeBuild(
	t *testing.T,
	candidate *models.TemplateReplicaBuild,
) *models.TemplateReplicaBuild {
	t.Helper()
	build, _, err := f.queries.CreateTemplateReplicaBuild(
		context.Background(),
		candidate,
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}
	worker := "replica-build-ready-" + uuid.NewString()
	f.claimBuildJob(t, build, worker)
	build.DestinationVMMoref = "vm-" + fmt.Sprint(f.nextVM)
	f.nextVM++
	if err := f.queries.EnsurePendingReplicaForBuild(context.Background(), build, worker); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	build.CleanupCompletedAt = &now
	build.DestinationSnapshot = "snapshot-" + uuid.NewString()
	build.CanaryVMMoref = "vm-3999"
	build.Phase = models.TemplateReplicaBuildPhaseFinalizing
	build.Status = models.TemplateReplicaBuildRunning
	if err := f.queries.SaveTemplateReplicaBuildState(
		context.Background(),
		build,
		models.TemplateReplicaBuildPhasePending,
		worker,
	); err != nil {
		t.Fatal(err)
	}
	if err := f.queries.FinalizeTemplateReplicaBuild(context.Background(), build, worker); err != nil {
		t.Fatal(err)
	}
	f.finishBuildJob(t, build, models.JobStatusCompleted)
	loaded, err := f.queries.GetTemplateReplicaBuild(context.Background(), f.templateID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
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

func TestTemplateDeletionSerializesReplicaBuildAdmissionPostgres(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	lockEntered := make(chan struct{})
	releaseDelete := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- fixture.queries.WithTemplateReplicaBuildLifecycleLock(
			context.Background(),
			fixture.templateID,
			func(ctx context.Context) error {
				close(lockEntered)
				<-releaseDelete
				return fixture.queries.DeleteTemplateWithHistory(ctx, fixture.templateID)
			},
		)
	}()
	<-lockEntered

	createDone := make(chan error, 1)
	go func() {
		_, _, err := fixture.queries.CreateTemplateReplicaBuild(
			context.Background(),
			fixture.build("delete-admission-race"),
			uuid.New(),
		)
		createDone <- err
	}()
	select {
	case err := <-createDone:
		t.Fatalf("replica build admission bypassed template deletion lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(releaseDelete)
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete template under lifecycle lock: %v", err)
	}
	if err := <-createDone; err == nil {
		t.Fatal("replica build was admitted after its template was deleted")
	}
	var builds, jobs int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM template_source_replica_builds
		WHERE template_id = $1
	`, fixture.templateID).Scan(&builds); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM jobs
		WHERE type = 'template_replica_build'
		  AND payload->>'template_id' = $1
	`, fixture.templateID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if builds != 0 || jobs != 0 {
		t.Fatalf("delete/admission race left builds=%d jobs=%d", builds, jobs)
	}

	fixture = newReplicaBuildPostgresFixture(t)
	build, _, err := fixture.queries.CreateTemplateReplicaBuild(
		context.Background(),
		fixture.build("delete-restart-race"),
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}
	const worker = "delete-restart-setup-worker"
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
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE jobs SET status = 'failed', claimed_by = NULL
		WHERE id = $1
	`, *build.JobID); err != nil {
		t.Fatal(err)
	}
	lockEntered = make(chan struct{})
	releaseDelete = make(chan struct{})
	deleteDone = make(chan error, 1)
	go func() {
		deleteDone <- fixture.queries.WithTemplateReplicaBuildLifecycleLock(
			context.Background(),
			fixture.templateID,
			func(ctx context.Context) error {
				close(lockEntered)
				<-releaseDelete
				return fixture.queries.DeleteTemplateWithHistory(ctx, fixture.templateID)
			},
		)
	}()
	<-lockEntered
	restartDone := make(chan error, 1)
	go func() {
		_, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			build.ID,
			false,
		)
		restartDone <- err
	}()
	select {
	case err := <-restartDone:
		t.Fatalf("replica build restart bypassed template deletion lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(releaseDelete)
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete template while restart waited: %v", err)
	}
	if err := <-restartDone; err == nil {
		t.Fatal("replica build restarted after its template was deleted")
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM jobs
		WHERE type = 'template_replica_build'
		  AND payload->>'template_id' = $1
	`, fixture.templateID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("delete/restart race created jobs=%d, want only original terminal job", jobs)
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
	t.Run("delete transaction rechecks unsafe build", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		if _, _, err := fixture.queries.CreateTemplateReplicaBuild(
			context.Background(),
			fixture.build("delete-recheck"),
			uuid.New(),
		); err != nil {
			t.Fatal(err)
		}
		if err := fixture.queries.DeleteTemplateWithHistory(
			context.Background(),
			fixture.templateID,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("delete unsafe template error=%v, want conflict", err)
		}
		var templates int
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM templates WHERE id = $1
		`, fixture.templateID).Scan(&templates); err != nil {
			t.Fatal(err)
		}
		if templates != 1 {
			t.Fatalf("unsafe delete removed template rows=%d", templates)
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

func TestFailTemplateReplicaBuildPreservesForwardResumePhase(t *testing.T) {
	for _, cleanupPhase := range []string{
		models.TemplateReplicaBuildPhaseResiduePrepared,
		models.TemplateReplicaBuildPhaseResidueSubmitting,
	} {
		t.Run(cleanupPhase, func(t *testing.T) {
			fixture := newReplicaBuildPostgresFixture(t)
			build, _, err := fixture.queries.CreateTemplateReplicaBuild(
				context.Background(),
				fixture.build("resume-"+cleanupPhase),
				uuid.New(),
			)
			if err != nil {
				t.Fatal(err)
			}
			const forwardWorker = "resume-forward-worker"
			fixture.claimBuildJob(t, build, forwardWorker)
			build.Status = models.TemplateReplicaBuildRunning
			build.Phase = models.TemplateReplicaBuildPhaseValidating
			if err := fixture.queries.SaveTemplateReplicaBuildState(
				context.Background(),
				build,
				models.TemplateReplicaBuildPhasePending,
				forwardWorker,
			); err != nil {
				t.Fatal(err)
			}
			if err := fixture.queries.FailTemplateReplicaBuild(
				context.Background(),
				build,
				forwardWorker,
				models.TemplateReplicaBuildCleanupRequired,
				models.TemplateReplicaBuildPhaseCleanupRequired,
				"manual_cleanup_required",
				"forward validation failed",
			); err != nil {
				t.Fatal(err)
			}
			fixture.finishBuildJob(t, build, models.JobStatusFailed)

			cleanup, err := fixture.queries.RestartTemplateReplicaBuild(
				context.Background(),
				fixture.templateID,
				build.ID,
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if cleanup.ResumePhase != models.TemplateReplicaBuildPhaseValidating ||
				cleanup.Phase != models.TemplateReplicaBuildPhaseValidating {
				t.Fatalf("cleanup resume=%s phase=%s, want validating", cleanup.ResumePhase, cleanup.Phase)
			}
			cleanupWorker := "resume-cleanup-worker-" + cleanupPhase
			fixture.claimBuildJob(t, cleanup, cleanupWorker)
			cleanup.Phase = cleanupPhase
			if err := fixture.queries.SaveTemplateReplicaBuildState(
				context.Background(),
				cleanup,
				models.TemplateReplicaBuildPhaseValidating,
				cleanupWorker,
			); err != nil {
				t.Fatal(err)
			}
			if err := fixture.queries.FailTemplateReplicaBuild(
				context.Background(),
				cleanup,
				cleanupWorker,
				models.TemplateReplicaBuildCleanupRequired,
				models.TemplateReplicaBuildPhaseCleanupRequired,
				"manual_cleanup_required",
				"cleanup transport failed",
			); err != nil {
				t.Fatal(err)
			}
			reloaded, err := fixture.queries.GetTemplateReplicaBuild(
				context.Background(),
				fixture.templateID,
				build.ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.ResumePhase != models.TemplateReplicaBuildPhaseValidating {
				t.Fatalf("cleanup failure poisoned resume phase=%s", reloaded.ResumePhase)
			}
			fixture.finishBuildJob(t, cleanup, models.JobStatusFailed)
			restarted, err := fixture.queries.RestartTemplateReplicaBuild(
				context.Background(),
				fixture.templateID,
				build.ID,
				true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if restarted.Phase != models.TemplateReplicaBuildPhaseValidating {
				t.Fatalf("subsequent cleanup resumed at %s, want validating", restarted.Phase)
			}
			if _, err := fixture.queries.RestartTemplateReplicaBuild(
				context.Background(),
				fixture.templateID,
				build.ID,
				false,
			); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
				t.Fatalf("forward retry while cleanup active error=%v, want conflict", err)
			}
		})
	}
}

func TestTemplateReplicaBuildResumePhaseConstraintRejectsCleanupPoison(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	build, _, err := fixture.queries.CreateTemplateReplicaBuild(
		context.Background(),
		fixture.build("resume-constraint"),
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE template_source_replica_builds
		SET resume_phase = 'residue_prepared'
		WHERE id = $1
	`, build.ID); err == nil {
		t.Fatal("schema accepted a cleanup-only resume checkpoint")
	}
	loaded, err := fixture.queries.GetTemplateReplicaBuild(context.Background(), fixture.templateID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ResumePhase != models.TemplateReplicaBuildPhasePending {
		t.Fatalf("failed invalid update poisoned resume phase=%s", loaded.ResumePhase)
	}
}

func TestRestartTemplateReplicaBuildPostgresDeduplicatesActiveCleanup(t *testing.T) {
	fixture := newReplicaBuildPostgresFixture(t)
	build, _, err := fixture.queries.CreateTemplateReplicaBuild(
		context.Background(),
		fixture.build("duplicate-cleanup"),
		uuid.New(),
	)
	if err != nil {
		t.Fatal(err)
	}
	const worker = "duplicate-cleanup-forward"
	fixture.claimBuildJob(t, build, worker)
	build.Status = models.TemplateReplicaBuildRunning
	build.Phase = models.TemplateReplicaBuildPhaseValidating
	if err := fixture.queries.SaveTemplateReplicaBuildState(
		context.Background(),
		build,
		models.TemplateReplicaBuildPhasePending,
		worker,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.FailTemplateReplicaBuild(
		context.Background(),
		build,
		worker,
		models.TemplateReplicaBuildCleanupRequired,
		models.TemplateReplicaBuildPhaseCleanupRequired,
		"manual_cleanup_required",
		"cleanup required",
	); err != nil {
		t.Fatal(err)
	}
	fixture.finishBuildJob(t, build, models.JobStatusFailed)

	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	builds := make(chan *models.TemplateReplicaBuild, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			restarted, err := fixture.queries.RestartTemplateReplicaBuild(
				context.Background(),
				fixture.templateID,
				build.ID,
				true,
			)
			builds <- restarted
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(builds)

	successes := 0
	conflicts := 0
	var activeJobID uuid.UUID
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrTemplateReplicaBuildConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	for restarted := range builds {
		if restarted != nil {
			activeJobID = *restarted.JobID
		}
	}
	if successes != 1 || conflicts != callers-1 {
		t.Fatalf("cleanup restarts success/conflict=%d/%d, want 1/%d", successes, conflicts, callers-1)
	}
	var activeJobs int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM jobs
		WHERE type = 'template_replica_build'
		  AND payload->>'build_id' = $1
		  AND status IN ('pending', 'claimed', 'in_progress')
	`, build.ID.String()).Scan(&activeJobs); err != nil {
		t.Fatal(err)
	}
	if activeJobs != 1 {
		t.Fatalf("active cleanup jobs=%d, want exactly 1", activeJobs)
	}
	loaded, err := fixture.queries.GetTemplateReplicaBuild(context.Background(), fixture.templateID, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.JobID == nil || *loaded.JobID != activeJobID {
		t.Fatalf("build linked job=%v, want %s", loaded.JobID, activeJobID)
	}
	const displacedWorker = "displaced-cleanup-worker"
	fixture.claimBuildJob(t, loaded, displacedWorker)
	replacementJobID := uuid.New()
	payload, err := json.Marshal(map[string]any{
		"build_id":     build.ID,
		"template_id":  fixture.templateID,
		"cleanup_only": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO jobs (id, type, payload, status, max_retries)
		VALUES ($1, 'template_replica_build', $2, 'pending', 20)
	`, replacementJobID, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE template_source_replica_builds SET job_id = $2 WHERE id = $1
	`, build.ID, replacementJobID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.GetTemplateReplicaBuildForJob(
		context.Background(),
		activeJobID,
		displacedWorker,
	); !errors.Is(err, ErrTemplateReplicaBuildJobObsolete) {
		t.Fatalf("displaced cleanup load error=%v, want obsolete classification", err)
	}
}

func TestReadyTemplateReplicaBuildRetirementPostgres(t *testing.T) {
	t.Run("success permits direct and template deletion", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		build := fixture.readyBuild(t, "retire-success")
		resultID := *build.ResultReplicaID
		retirement, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			build.ID,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if retirement.Status != models.TemplateReplicaBuildRetiring ||
			retirement.Phase != models.TemplateReplicaBuildPhaseResiduePrepared {
			t.Fatalf("retirement start=%s/%s", retirement.Status, retirement.Phase)
		}
		var resultStatus string
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT status FROM template_source_replicas WHERE id = $1
		`, resultID).Scan(&resultStatus); err != nil {
			t.Fatal(err)
		}
		if resultStatus != models.TemplateSourceReplicaDisabled {
			t.Fatalf("result status while retiring=%s, want disabled", resultStatus)
		}
		const failedWorker = "ready-retirement-failed-worker"
		fixture.claimBuildJob(t, retirement, failedWorker)
		retirement.Phase = models.TemplateReplicaBuildPhaseResidueSubmitting
		if err := fixture.queries.SaveTemplateReplicaBuildState(
			context.Background(),
			retirement,
			models.TemplateReplicaBuildPhaseResiduePrepared,
			failedWorker,
		); err != nil {
			t.Fatal(err)
		}
		if err := fixture.queries.FailTemplateReplicaBuild(
			context.Background(),
			retirement,
			failedWorker,
			models.TemplateReplicaBuildRetiring,
			models.TemplateReplicaBuildPhaseCleanupRequired,
			"retirement_cleanup_required",
			"simulated destroy transport failure",
		); err != nil {
			t.Fatal(err)
		}
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT status FROM template_source_replicas WHERE id = $1
		`, resultID).Scan(&resultStatus); err != nil {
			t.Fatal(err)
		}
		if resultStatus != models.TemplateSourceReplicaDisabled {
			t.Fatalf("retirement failure changed disabled result to %s", resultStatus)
		}
		fixture.finishBuildJob(t, retirement, models.JobStatusFailed)
		retirement, err = fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			build.ID,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		const worker = "ready-retirement-worker"
		fixture.claimBuildJob(t, retirement, worker)
		now := time.Now().UTC()
		retirement.ResidueCleanedAt = &now
		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			retirement,
			worker,
			"exact ready result replica destroyed",
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
		if loaded.Status != models.TemplateReplicaBuildRetired ||
			loaded.Phase != models.TemplateReplicaBuildPhaseRetired ||
			loaded.ResultReplicaID != nil || loaded.ResidueCleanedAt == nil {
			t.Fatalf("retired build did not survive reload: %+v", loaded)
		}
		var resultRows int
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM template_source_replicas WHERE id = $1
		`, resultID).Scan(&resultRows); err != nil {
			t.Fatal(err)
		}
		if resultRows != 0 {
			t.Fatalf("retired result replica rows=%d, want 0", resultRows)
		}
		unsafe, err := fixture.queries.HasUnsafeTemplateReplicaBuildForDeletion(
			context.Background(),
			fixture.templateID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if unsafe {
			t.Fatal("retired build still blocks template deletion")
		}
		deleted, err := fixture.queries.DeleteTemplateSourceReplica(
			context.Background(),
			fixture.templateID,
			fixture.anchor.ID,
		)
		if err != nil || !deleted {
			t.Fatalf("delete source anchor after retirement=%t error=%v", deleted, err)
		}
		if err := fixture.queries.DeleteTemplateWithHistory(context.Background(), fixture.templateID); err != nil {
			t.Fatalf("template deletion after retirement: %v", err)
		}
	})

	t.Run("requires a ready source anchor", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		build := fixture.readyBuild(t, "retire-last-anchor")
		if _, err := fixture.pool.Exec(context.Background(), `
			UPDATE template_source_replicas SET status = 'unhealthy' WHERE id = $1
		`, fixture.anchor.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			build.ID,
			true,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("last-anchor retirement error=%v, want conflict", err)
		}
		var resultStatus string
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT status FROM template_source_replicas WHERE id = $1
		`, *build.ResultReplicaID).Scan(&resultStatus); err != nil {
			t.Fatal(err)
		}
		if resultStatus != models.TemplateSourceReplicaReady {
			t.Fatalf("rejected retirement changed result status to %s", resultStatus)
		}
	})

	t.Run("rejects placement references", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		build := fixture.readyBuild(t, "retire-referenced")
		ownerID := uuid.New()
		podID := uuid.New()
		podVMID := uuid.New()
		jobID := uuid.New()
		ctx := context.Background()
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO users (id, oidc_sub, username, email)
			VALUES ($1, $2, $2, $3)
		`, ownerID, ownerID.String(), ownerID.String()+"@example.invalid"); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
			VALUES ($1, $2, $3, 'active', 3988, '10.253.244.0/24')
		`, podID, ownerID, "retirement-placement-"+podID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO pod_vms (
				id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
			)
			VALUES ($1, $2, $3, $4, 1, 1024, 10, 'running')
		`, podVMID, podID, fixture.templateID, "retirement-vm"); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO jobs (id, type, payload, status)
			VALUES ($1, 'pod_create', '{}', 'completed')
		`, jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(ctx, `
			INSERT INTO vm_placements (
				pod_vm_id, job_id, template_id, source_replica_id, source_ref,
				compute_resource_type, compute_resource_moref, resource_pool_moref,
				host_moref, host_name, drs_control, observed_free_memory_mb,
				reserved_memory_mb, capacity_reservation_mb, capacity_observed_at,
				admitted_headroom_mb
			)
			VALUES (
				$1, $2, $3, $4, $5, $6, $7, 'resgroup-placement',
				'host-placement', 'host.example.invalid', 'disabled', 4096,
				1024, 0, now(), 3072
			)
		`, podVMID, jobID, fixture.templateID, *build.ResultReplicaID,
			build.DestinationVMMoref, build.ComputeResourceType,
			build.ComputeResourceMoref); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM vm_placements WHERE pod_vm_id = $1`, podVMID)
			_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM jobs WHERE id = $1`, jobID)
			_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM pods WHERE id = $1`, podID)
			_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID)
		})
		if _, err := fixture.queries.RestartTemplateReplicaBuild(
			ctx,
			fixture.templateID,
			build.ID,
			true,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("referenced retirement error=%v, want conflict", err)
		}
		var resultStatus string
		if err := fixture.pool.QueryRow(ctx, `
			SELECT status FROM template_source_replicas WHERE id = $1
		`, *build.ResultReplicaID).Scan(&resultStatus); err != nil {
			t.Fatal(err)
		}
		if resultStatus != models.TemplateSourceReplicaReady {
			t.Fatalf("rejected referenced retirement changed result status to %s", resultStatus)
		}
	})

	t.Run("rejects live downstream build anchors and releases retired history", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		upstream := fixture.readyBuild(t, "retire-chain-upstream")
		upstreamReplica, err := fixture.queries.GetTemplateSourceReplica(
			context.Background(),
			fixture.templateID,
			*upstream.ResultReplicaID,
		)
		if err != nil || upstreamReplica == nil {
			t.Fatalf("load upstream result replica=%+v error=%v", upstreamReplica, err)
		}
		downstreamCandidate := fixture.build("retire-chain-downstream")
		downstreamCandidate.SourceReplicaID = &upstreamReplica.ID
		downstreamCandidate.SourceVMMoref = upstreamReplica.SourceVMMoref
		downstreamCandidate.ComputeResourceMoref = "domain-c3403"
		downstreamCandidate.ComputeResourcePath = "/DC/host/Downstream"
		downstreamCandidate.HostMoref = "host-3403"
		downstreamCandidate.HostName = "downstream.example.invalid"
		downstreamCandidate.ResourcePoolMoref = "resgroup-3403"
		downstreamCandidate.ResourcePoolPath = "/DC/host/Downstream/Resources/Students"
		downstreamCandidate.DatastoreMoref = "datastore-3403"
		downstreamCandidate.DatastoreName = "downstream-ds"
		downstreamCandidate.FolderMoref = "group-v3403"
		downstreamCandidate.FolderPath = "/DC/vm/Downstream"
		downstream := fixture.finalizeBuild(t, downstreamCandidate)

		if _, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			upstream.ID,
			true,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("upstream retirement with live downstream error=%v, want conflict", err)
		}
		var upstreamStatus string
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT status FROM template_source_replicas WHERE id = $1
		`, upstreamReplica.ID).Scan(&upstreamStatus); err != nil {
			t.Fatal(err)
		}
		if upstreamStatus != models.TemplateSourceReplicaReady {
			t.Fatalf("rejected upstream retirement changed source status to %s", upstreamStatus)
		}

		downstreamRetirement, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			downstream.ID,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		const downstreamWorker = "downstream-retirement-worker"
		fixture.claimBuildJob(t, downstreamRetirement, downstreamWorker)
		now := time.Now().UTC()
		downstreamRetirement.ResidueCleanedAt = &now
		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			downstreamRetirement,
			downstreamWorker,
			"downstream result destroyed",
		); err != nil {
			t.Fatal(err)
		}
		upstreamRetirement, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			upstream.ID,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		const upstreamWorker = "upstream-retirement-worker"
		fixture.claimBuildJob(t, upstreamRetirement, upstreamWorker)
		now = time.Now().UTC()
		upstreamRetirement.ResidueCleanedAt = &now
		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			upstreamRetirement,
			upstreamWorker,
			"upstream result destroyed after downstream retirement",
		); err != nil {
			t.Fatal(err)
		}
		reloadedDownstream, err := fixture.queries.GetTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			downstream.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if reloadedDownstream.Status != models.TemplateReplicaBuildRetired ||
			reloadedDownstream.SourceReplicaID != nil {
			t.Fatalf("retired downstream retained restrictive source reference: %+v", reloadedDownstream)
		}
	})

	t.Run("releases fully cleaned failed downstream history", func(t *testing.T) {
		fixture := newReplicaBuildPostgresFixture(t)
		upstream := fixture.readyBuild(t, "retire-failed-chain-upstream")
		upstreamReplica, err := fixture.queries.GetTemplateSourceReplica(
			context.Background(),
			fixture.templateID,
			*upstream.ResultReplicaID,
		)
		if err != nil || upstreamReplica == nil {
			t.Fatalf("load upstream result replica=%+v error=%v", upstreamReplica, err)
		}
		downstreamCandidate := fixture.build("retire-failed-chain-downstream")
		downstreamCandidate.SourceReplicaID = &upstreamReplica.ID
		downstreamCandidate.SourceVMMoref = upstreamReplica.SourceVMMoref
		downstreamCandidate.ComputeResourceMoref = "domain-c3404"
		downstreamCandidate.ComputeResourcePath = "/DC/host/Failed-Downstream"
		downstreamCandidate.HostMoref = "host-3404"
		downstreamCandidate.HostName = "failed-downstream.example.invalid"
		downstreamCandidate.ResourcePoolMoref = "resgroup-3404"
		downstreamCandidate.ResourcePoolPath = "/DC/host/Failed-Downstream/Resources/Students"
		downstreamCandidate.DatastoreMoref = "datastore-3404"
		downstreamCandidate.DatastoreName = "failed-downstream-ds"
		downstreamCandidate.FolderMoref = "group-v3404"
		downstreamCandidate.FolderPath = "/DC/vm/Failed-Downstream"
		downstream, _, err := fixture.queries.CreateTemplateReplicaBuild(
			context.Background(),
			downstreamCandidate,
			uuid.New(),
		)
		if err != nil {
			t.Fatal(err)
		}
		const failedWorker = "failed-downstream-worker"
		fixture.claimBuildJob(t, downstream, failedWorker)
		if err := fixture.queries.FailTemplateReplicaBuild(
			context.Background(),
			downstream,
			failedWorker,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseFailed,
			"validation_failed",
			"failed before vCenter submission",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.pool.Exec(context.Background(), `
			UPDATE jobs SET status = 'failed', claimed_by = NULL
			WHERE id = $1
		`, *downstream.JobID); err != nil {
			t.Fatal(err)
		}

		upstreamRetirement, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			upstream.ID,
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			downstream.ID,
			false,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("downstream restarted from disabled retiring source error=%v, want conflict", err)
		}
		const upstreamWorker = "failed-chain-upstream-retirement-worker"
		fixture.claimBuildJob(t, upstreamRetirement, upstreamWorker)
		now := time.Now().UTC()
		upstreamRetirement.ResidueCleanedAt = &now
		if err := fixture.queries.CompleteTemplateReplicaBuildCleanup(
			context.Background(),
			upstreamRetirement,
			upstreamWorker,
			"upstream result destroyed after failed downstream cleanup",
		); err != nil {
			t.Fatal(err)
		}
		reloadedDownstream, err := fixture.queries.GetTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			downstream.ID,
		)
		if err != nil {
			t.Fatal(err)
		}
		if reloadedDownstream.Status != models.TemplateReplicaBuildFailed ||
			reloadedDownstream.SourceReplicaID != nil {
			t.Fatalf("clean failed downstream retained restrictive source reference: %+v", reloadedDownstream)
		}
		if _, err := fixture.queries.RestartTemplateReplicaBuild(
			context.Background(),
			fixture.templateID,
			downstream.ID,
			false,
		); !errors.Is(err, ErrTemplateReplicaBuildConflict) {
			t.Fatalf("released terminal build restart error=%v, want conflict", err)
		}
	})
}
