package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

type podJobsPostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *Queries
	ownerID    uuid.UUID
	templateID uuid.UUID
	podID      uuid.UUID
	podVMID    uuid.UUID
}

func newPodJobsPostgresFixture(t *testing.T, status string) *podJobsPostgresFixture {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run pod job serialization tests")
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

	fixture := &podJobsPostgresFixture{
		pool:       pool,
		queries:    NewQueries(pool),
		ownerID:    uuid.New(),
		templateID: uuid.New(),
		podID:      uuid.New(),
		podVMID:    uuid.New(),
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
	`, fixture.templateID, "pod-jobs-"+fixture.templateID.String(), "source-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
		VALUES ($1, $2, $3, $4, 3998, '10.253.254.0/24')
	`, fixture.podID, fixture.ownerID, "pod-jobs-"+fixture.podID.String(), status); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pod_vms (
			id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
		)
		VALUES ($1, $2, $3, 'target', 1, 1024, 10, 'running')
	`, fixture.podVMID, fixture.podID, fixture.templateID); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE payload->>'pod_id' = $1`, fixture.podID.String())
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.ownerID)
	})
	return fixture
}

func podDestroyPayload(t *testing.T, podID uuid.UUID, source string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"pod_id": podID.String(),
		"reason": source,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func vmJobPayload(t *testing.T, podID, podVMID uuid.UUID) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]string{
		"pod_id":    podID.String(),
		"pod_vm_id": podVMID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestPodDestroyProducersPostgresConvergeOnOneAuthoritativeJob(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_vms SET status = 'deleted' WHERE id = $1
	`, fixture.podVMID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pods SET expires_at = now() - interval '1 minute' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	const producerCount = 12
	type result struct {
		jobID   uuid.UUID
		created bool
		err     error
	}
	results := make(chan result, producerCount)
	var ready sync.WaitGroup
	ready.Add(producerCount)
	start := make(chan struct{})
	payloads := map[string][]byte{
		"expiration": podDestroyPayload(t, fixture.podID, "expiration"),
		"vm_destroy": podDestroyPayload(t, fixture.podID, "vm_destroy"),
	}
	for i := 0; i < producerCount; i++ {
		source := "expiration"
		if i%2 == 1 {
			source = "vm_destroy"
		}
		go func(source string, payload []byte) {
			ready.Done()
			<-start
			var job *models.Job
			var created bool
			var err error
			if source == "expiration" {
				job, created, err = fixture.queries.CreateExpiredPodDestroyJob(ctx, fixture.podID, payload)
			} else {
				job, created, err = fixture.queries.CreateEmptyPodDestroyJob(ctx, fixture.podID, payload)
			}
			var jobID uuid.UUID
			if job != nil {
				jobID = job.ID
			}
			results <- result{jobID: jobID, created: created, err: err}
		}(source, payloads[source])
	}
	ready.Wait()
	close(start)

	var authoritative uuid.UUID
	createdCount := 0
	for i := 0; i < producerCount; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent producer failed: %v", got.err)
		}
		if got.created {
			createdCount++
		}
		if authoritative == uuid.Nil {
			authoritative = got.jobID
		} else if got.jobID != authoritative {
			t.Fatalf("producer returned job %s, want authoritative job %s", got.jobID, authoritative)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created %d pod_destroy jobs, want exactly one", createdCount)
	}

	var count int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM jobs
		WHERE type = 'pod_destroy'
		  AND payload->>'pod_id' = $1
	`, fixture.podID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted pod_destroy count = %d, want 1", count)
	}
}

func TestPodDestroyPostgresReusesAuthoritativeJobAcrossEveryStatus(t *testing.T) {
	for _, jobStatus := range []string{
		models.JobStatusPending,
		models.JobStatusClaimed,
		models.JobStatusInProgress,
		models.JobStatusCompleted,
		models.JobStatusFailed,
		models.JobStatusRollback,
	} {
		t.Run(jobStatus, func(t *testing.T) {
			fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
			ctx := context.Background()
			existingID := uuid.New()
			if _, err := fixture.pool.Exec(ctx, `
				INSERT INTO jobs (id, type, payload, status)
				VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), $3)
			`, existingID, fixture.podID, jobStatus); err != nil {
				t.Fatal(err)
			}

			job, created, err := fixture.queries.CreatePodDestroyJob(
				ctx,
				fixture.podID,
				podDestroyPayload(t, fixture.podID, "delayed_vm_destroy"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if created {
				t.Fatal("created a second pod_destroy job")
			}
			if job.ID != existingID {
				t.Fatalf("reused job %s, want %s", job.ID, existingID)
			}
		})
	}
}

func TestPodDestroyPostgresRefusesTerminalPodWithoutAuthoritativeJob(t *testing.T) {
	for _, podStatus := range []string{
		models.PodStatusDestroying,
		models.PodStatusDestroyFailed,
		models.PodStatusDestroyed,
		"cancelled",
	} {
		t.Run(podStatus, func(t *testing.T) {
			fixture := newPodJobsPostgresFixture(t, podStatus)
			job, created, err := fixture.queries.CreatePodDestroyJob(
				context.Background(),
				fixture.podID,
				podDestroyPayload(t, fixture.podID, "delayed_vm_destroy"),
			)
			if !errors.Is(err, ErrPodJobRejected) {
				t.Fatalf("error = %v, want %v", err, ErrPodJobRejected)
			}
			if job != nil || created {
				t.Fatalf("terminal enqueue returned job=%v created=%v", job, created)
			}

			var count int
			if err := fixture.pool.QueryRow(context.Background(), `
				SELECT count(*) FROM jobs
				WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1
			`, fixture.podID.String()).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("terminal pod gained %d destroy jobs, want 0", count)
			}
		})
	}
}

func TestPodDestroyPostgresStillCleansUpErrorPod(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusError)
	job, created, err := fixture.queries.CreatePodDestroyJob(
		context.Background(),
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "error_cleanup"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || job == nil || job.Type != models.JobTypePodDestroy {
		t.Fatalf("error-pod cleanup returned job=%v created=%v", job, created)
	}
}

func TestVMJobPostgresCannotInsertAfterTerminalCommit(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	terminalTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer terminalTx.Rollback(ctx)
	if _, err := terminalTx.Exec(ctx, `
		UPDATE pods SET status = 'destroyed' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	payload := vmJobPayload(t, fixture.podID, fixture.podVMID)
	result := make(chan error, 1)
	go func() {
		_, err := fixture.queries.CreateVMJob(
			ctx,
			fixture.podID,
			fixture.podVMID,
			models.JobTypeVMStart,
			payload,
		)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("VM enqueue returned before terminal transaction committed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := terminalTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("VM enqueue error = %v, want %v", err, ErrPodJobRejected)
	}

	var count int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type = 'vm_start' AND payload->>'pod_id' = $1
	`, fixture.podID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("terminalized pod gained %d vm_start jobs, want 0", count)
	}
}

func TestVMJobPostgresCannotInsertAfterVMDeletionCommit(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	deleteTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleteTx.Rollback(ctx)
	if _, err := deleteTx.Exec(ctx, `
		UPDATE pod_vms SET status = 'deleted' WHERE id = $1
	`, fixture.podVMID); err != nil {
		t.Fatal(err)
	}

	payload := vmJobPayload(t, fixture.podID, fixture.podVMID)
	result := make(chan error, 1)
	go func() {
		_, err := fixture.queries.CreateVMJob(
			ctx,
			fixture.podID,
			fixture.podVMID,
			models.JobTypeVMStart,
			payload,
		)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("VM enqueue returned before VM deletion committed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := deleteTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("VM enqueue error = %v, want %v", err, ErrPodJobRejected)
	}

	var count int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type = 'vm_start' AND payload->>'pod_vm_id' = $1
	`, fixture.podVMID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted VM gained %d vm_start jobs, want 0", count)
	}
}

func TestPodDestroyPostgresCannotInsertAfterTerminalCommit(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	terminalTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer terminalTx.Rollback(ctx)
	if _, err := terminalTx.Exec(ctx, `
		UPDATE pods SET status = 'destroyed' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	payload := podDestroyPayload(t, fixture.podID, "delayed_vm_destroy")
	result := make(chan error, 1)
	go func() {
		_, _, err := fixture.queries.CreateEmptyPodDestroyJob(
			ctx,
			fixture.podID,
			payload,
		)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("pod_destroy enqueue returned before terminal transaction committed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := terminalTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("pod_destroy enqueue error = %v, want %v", err, ErrPodJobRejected)
	}

	var count int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1
	`, fixture.podID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("terminalized pod gained %d pod_destroy jobs, want 0", count)
	}
}

func TestExpiredPodDestroyPostgresRevalidatesRenewalUnderLock(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pods SET expires_at = now() - interval '1 minute' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	expired, err := fixture.queries.ListExpiredPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, podID := range expired {
		if podID == fixture.podID {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture pod was not returned by the production expiration scan")
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pods SET expires_at = now() + interval '1 hour' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, created, err := fixture.queries.CreateExpiredPodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "expired"),
	)
	if !errors.Is(err, ErrPodDestroyNotNeeded) {
		t.Fatalf("renewed expiration enqueue error = %v, want %v", err, ErrPodDestroyNotNeeded)
	}
	if job != nil || created {
		t.Fatalf("renewed pod returned job=%v created=%v", job, created)
	}
}

func TestEmptyPodDestroyPostgresRevalidatesConcurrentVMAdd(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_vms SET status = 'deleted' WHERE id = $1
	`, fixture.podVMID); err != nil {
		t.Fatal(err)
	}
	remaining, err := fixture.queries.CountActiveVMsInPod(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("pre-enqueue active VM count = %d, want 0", remaining)
	}

	addedVM := &models.PodVM{
		ID:          uuid.New(),
		PodID:       fixture.podID,
		TemplateID:  fixture.templateID,
		DisplayName: "concurrent-add",
		VCPUs:       1,
		RAMMB:       1024,
		DiskGB:      10,
		Status:      models.VMStatusPending,
	}
	if _, err := fixture.queries.CreateVMAddJob(
		ctx,
		addedVM,
		vmJobPayload(t, fixture.podID, addedVM.ID),
	); err != nil {
		t.Fatal(err)
	}

	job, created, err := fixture.queries.CreateEmptyPodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "vm_destroy"),
	)
	if !errors.Is(err, ErrPodDestroyNotNeeded) {
		t.Fatalf("non-empty cleanup enqueue error = %v, want %v", err, ErrPodDestroyNotNeeded)
	}
	if job != nil || created {
		t.Fatalf("non-empty pod returned job=%v created=%v", job, created)
	}
}

func TestVMJobPostgresPreservesActivePodActions(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	payload := vmJobPayload(t, fixture.podID, fixture.podVMID)
	for _, jobType := range []string{
		models.JobTypeVMDestroy,
		models.JobTypeVMStart,
		models.JobTypeVMStop,
		models.JobTypeVMRestart,
		models.JobTypeVMReset,
		models.JobTypeVMSnapshot,
		models.JobTypeVMRevert,
		models.JobTypeVMSnapshotDelete,
		models.JobTypeVMSuspend,
	} {
		job, err := fixture.queries.CreateVMJob(ctx, fixture.podID, fixture.podVMID, jobType, payload)
		if err != nil {
			t.Fatalf("CreateVMJob(%s): %v", jobType, err)
		}
		if job.Type != jobType || job.Status != models.JobStatusPending {
			t.Fatalf("CreateVMJob(%s) = type %q status %q", jobType, job.Type, job.Status)
		}
	}
}

func TestVMAddPostgresCreatesVMAndJobAtomically(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	vm := &models.PodVM{
		ID:          uuid.New(),
		PodID:       fixture.podID,
		TemplateID:  fixture.templateID,
		DisplayName: "added",
		VCPUs:       2,
		RAMMB:       2048,
		DiskGB:      20,
		Status:      models.VMStatusPending,
	}
	job, err := fixture.queries.CreateVMAddJob(
		context.Background(),
		vm,
		vmJobPayload(t, fixture.podID, vm.ID),
	)
	if err != nil {
		t.Fatal(err)
	}
	if job.Type != models.JobTypeVMAdd || job.Status != models.JobStatusPending {
		t.Fatalf("vm_add job = type %q status %q", job.Type, job.Status)
	}

	var vmStatus string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT status FROM pod_vms WHERE id = $1
	`, vm.ID).Scan(&vmStatus); err != nil {
		t.Fatal(err)
	}
	if vmStatus != models.VMStatusPending {
		t.Fatalf("added VM status = %q, want %q", vmStatus, models.VMStatusPending)
	}
}

func TestVMAddPostgresRollsBackVMWhenJobInsertFails(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	vm := &models.PodVM{
		ID:          uuid.New(),
		PodID:       fixture.podID,
		TemplateID:  fixture.templateID,
		DisplayName: "rollback-add",
		VCPUs:       1,
		RAMMB:       1024,
		DiskGB:      10,
		Status:      models.VMStatusPending,
	}

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	functionName := "fail_vm_add_" + suffix
	triggerName := functionName + "_trigger"
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.type = 'vm_add' AND NEW.payload->>'pod_vm_id' = '%s' THEN
				RAISE EXCEPTION 'forced vm_add job insertion failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`, functionName, vm.ID.String())); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE INSERT ON jobs
		FOR EACH ROW EXECUTE FUNCTION %s()
	`, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON jobs", triggerName))
		_, _ = fixture.pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
	})

	if _, err := fixture.queries.CreateVMAddJob(
		ctx,
		vm,
		vmJobPayload(t, fixture.podID, vm.ID),
	); err == nil {
		t.Fatal("vm_add unexpectedly succeeded despite forced job insertion failure")
	}

	var vmCount, jobCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM pod_vms WHERE id = $1`, vm.ID).Scan(&vmCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type = 'vm_add' AND payload->>'pod_vm_id' = $1
	`, vm.ID.String()).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if vmCount != 0 || jobCount != 0 {
		t.Fatalf("failed vm_add left VM rows=%d jobs=%d, want 0/0", vmCount, jobCount)
	}
}

func TestAuthoritativePodDestroyPostgresRejectsLaterVMWork(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, created, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "api"),
	); err != nil || !created {
		t.Fatalf("create authoritative pod_destroy = created %v error %v", created, err)
	}

	if _, err := fixture.queries.CreateVMJob(
		ctx,
		fixture.podID,
		fixture.podVMID,
		models.JobTypeVMStart,
		vmJobPayload(t, fixture.podID, fixture.podVMID),
	); !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("VM action after pod_destroy error = %v, want %v", err, ErrPodJobRejected)
	}

	addedVM := &models.PodVM{
		ID:          uuid.New(),
		PodID:       fixture.podID,
		TemplateID:  fixture.templateID,
		DisplayName: "too-late",
		VCPUs:       1,
		RAMMB:       1024,
		DiskGB:      10,
		Status:      models.VMStatusPending,
	}
	if _, err := fixture.queries.CreateVMAddJob(
		ctx,
		addedVM,
		vmJobPayload(t, fixture.podID, addedVM.ID),
	); !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("vm_add after pod_destroy error = %v, want %v", err, ErrPodJobRejected)
	}

	var vmCount, actionCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM pod_vms WHERE id = $1
	`, addedVM.ID).Scan(&vmCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type IN ('vm_add', 'vm_start')
		  AND payload->>'pod_id' = $1
	`, fixture.podID.String()).Scan(&actionCount); err != nil {
		t.Fatal(err)
	}
	if vmCount != 0 || actionCount != 0 {
		t.Fatalf("destroy-intent race left VM rows=%d action jobs=%d, want 0/0", vmCount, actionCount)
	}
}

func TestVMAddPostgresTerminalRaceLeavesNoOrphanVM(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	terminalTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer terminalTx.Rollback(ctx)
	if _, err := terminalTx.Exec(ctx, `
		UPDATE pods SET status = 'destroyed' WHERE id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	vm := &models.PodVM{
		ID:          uuid.New(),
		PodID:       fixture.podID,
		TemplateID:  fixture.templateID,
		DisplayName: "racing-add",
		VCPUs:       1,
		RAMMB:       1024,
		DiskGB:      10,
		Status:      models.VMStatusPending,
	}
	payload := vmJobPayload(t, fixture.podID, vm.ID)
	result := make(chan error, 1)
	go func() {
		_, err := fixture.queries.CreateVMAddJob(ctx, vm, payload)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("vm_add returned before terminal transaction committed: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := terminalTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("vm_add error = %v, want %v", err, ErrPodJobRejected)
	}

	var vmCount, jobCount int
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*) FROM pod_vms WHERE id = $1`, vm.ID).Scan(&vmCount); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT count(*) FROM jobs
		WHERE type = 'vm_add' AND payload->>'pod_vm_id' = $1
	`, vm.ID.String()).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if vmCount != 0 || jobCount != 0 {
		t.Fatalf("terminal vm_add race left VM rows=%d jobs=%d, want 0/0", vmCount, jobCount)
	}
}

func TestPodDestroyPostgresDoesNotLockExistingJobAfterPod(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text))
	`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	workerTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer workerTx.Rollback(ctx)
	var locked uuid.UUID
	if err := workerTx.QueryRow(ctx, `
		SELECT id FROM jobs WHERE id = $1 FOR UPDATE
	`, existingID).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	enqueueCtx, enqueueCancel := context.WithTimeout(ctx, time.Second)
	defer enqueueCancel()
	job, created, err := fixture.queries.CreatePodDestroyJob(
		enqueueCtx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "concurrent_worker"),
	)
	if err != nil {
		t.Fatalf("producer blocked on worker-held job lock: %v", err)
	}
	if created || job.ID != existingID {
		t.Fatalf("producer returned job=%v created=%v, want existing %s", job.ID, created, existingID)
	}

	var status string
	if err := workerTx.QueryRow(ctx, `
		SELECT status FROM pods WHERE id = $1 FOR UPDATE
	`, fixture.podID).Scan(&status); err != nil {
		t.Fatalf("worker could not continue job -> pod ordering: %v", err)
	}
}
