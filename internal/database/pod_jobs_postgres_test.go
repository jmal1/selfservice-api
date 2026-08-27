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
		_, _ = pool.Exec(cleanupCtx, `UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.ownerID)
	})
	return fixture
}

func (f *podJobsPostgresFixture) assignAvailableVLAN(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var vlanTag int
	var subnet string
	if err := f.pool.QueryRow(ctx, `
		UPDATE vlan_pool
		SET pod_id = $1,
		    allocated_at = now()
		WHERE id = (
			SELECT id
			FROM vlan_pool
			WHERE pod_id IS NULL
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING vlan_tag, subnet
	`, f.podID).Scan(&vlanTag, &subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE pods SET vlan_id = $2, subnet = $3 WHERE id = $1
	`, f.podID, vlanTag, subnet); err != nil {
		t.Fatal(err)
	}
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
				job, created, err = fixture.queries.CreateEmptyPodDestroyJob(ctx, fixture.podID, payload, nil)
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

func TestPodDestroyPostgresRequeuesFailedPreTransitionJobWithSameID(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO jobs (id, type, payload, status, completed_at, result, retry_count, max_retries)
					VALUES (
						$1,
						'pod_destroy',
						jsonb_build_object('pod_id', $2::text),
						'failed',
						now(),
						'{"error":"pre-transition failure"}',
						3,
						3
					)
				`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "retry"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !queued {
		t.Fatal("terminal pre-transition destroy job was not requeued")
	}
	if job.ID != existingID || job.Status != models.JobStatusPending {
		t.Fatalf("requeued job = %s/%q, want %s/%q", job.ID, job.Status, existingID, models.JobStatusPending)
	}

	var (
		count       int
		status      string
		retryCount  int
		completedAt *time.Time
		result      []byte
	)
	if err := fixture.pool.QueryRow(ctx, `
					SELECT count(*) OVER (), status, retry_count, completed_at, result
					FROM jobs
					WHERE type = 'pod_destroy'
					  AND payload->>'pod_id' = $1
				`, fixture.podID.String()).Scan(&count, &status, &retryCount, &completedAt, &result); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != models.JobStatusPending || retryCount != 0 || completedAt != nil || result != nil {
		t.Fatalf("persisted requeue = count %d status %q retries %d completed_at %v result %s", count, status, retryCount, completedAt, result)
	}
}

func TestPodDestroyPostgresRequeuesCompletedNonterminalInconsistency(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO jobs (id, type, payload, status, completed_at)
					VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'completed', now())
				`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "retry"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !queued || job.ID != existingID || job.Status != models.JobStatusPending {
		t.Fatalf("completed/nonterminal reconciliation = job %v queued %v", job, queued)
	}
}

func TestPodDestroyPostgresDoesNotRequeuePartialCleanup(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusDestroyFailed)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO jobs (id, type, payload, status, completed_at)
					VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
				`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "retry"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if queued || job.ID != existingID || job.Status != models.JobStatusFailed {
		t.Fatalf("partial-cleanup reuse = job %v queued %v", job, queued)
	}

	job, queued, err = fixture.queries.RequeueFailedPodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "destroy_failed_sweep"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !queued || job.ID != existingID || job.Status != models.JobStatusPending {
		t.Fatalf("serialized partial-cleanup retry = job %v queued %v", job, queued)
	}
}

func TestPodDestroyPostgresRequeuesTerminalDestroyingInconsistency(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusDestroying)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, completed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
	`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "terminal_destroying_retry"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !queued || job.ID != existingID || job.Status != models.JobStatusPending {
		t.Fatalf("destroying reconciliation = job %v queued %v", job, queued)
	}
}

func TestFinalizePodDestroyPostgresAtomicallyUpdatesStatusAndReleasesVLAN(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	jobID := uuid.New()
	claimOwner := "worker-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'in_progress', $3, now())
	`, jobID, fixture.podID, claimOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, jobID, claimOwner); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	functionName := "test_fail_vlan_release_" + suffix
	triggerName := "test_fail_vlan_release_" + suffix
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'injected VLAN release failure';
		END;
		$$
	`, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE ON vlan_pool
		FOR EACH ROW
		WHEN (OLD.pod_id = '%s'::uuid AND NEW.pod_id IS NULL)
		EXECUTE FUNCTION %s()
	`, triggerName, fixture.podID, functionName)); err != nil {
		t.Fatal(err)
	}
	dropTrigger := func() {
		_, _ = fixture.pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON vlan_pool", triggerName))
		_, _ = fixture.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	}
	defer dropTrigger()

	if err := fixture.queries.FinalizePodDestroy(ctx, fixture.podID, jobID, claimOwner); err == nil {
		t.Fatal("injected VLAN release failure did not fail finalization")
	}
	var status string
	var ownsVLAN bool
	if err := fixture.pool.QueryRow(ctx, `
		SELECT
			(SELECT status FROM pods WHERE id = $1),
			EXISTS (SELECT 1 FROM vlan_pool WHERE pod_id = $1)
	`, fixture.podID).Scan(&status, &ownsVLAN); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusDestroying || !ownsVLAN {
		t.Fatalf("rolled-back finalization left status/ownership %q/%v, want destroying/true", status, ownsVLAN)
	}

	dropTrigger()
	if err := fixture.queries.FinalizePodDestroy(ctx, fixture.podID, jobID, claimOwner); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT
			(SELECT status FROM pods WHERE id = $1),
			EXISTS (SELECT 1 FROM vlan_pool WHERE pod_id = $1)
	`, fixture.podID).Scan(&status, &ownsVLAN); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusDestroyed || ownsVLAN {
		t.Fatalf("committed finalization left status/ownership %q/%v, want destroyed/false", status, ownsVLAN)
	}
}

func TestPreparePodDestroyPostgresTransitionsAuthoritativeOwnedJob(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	jobID := uuid.New()
	claimOwner := "worker-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'in_progress', $3, now())
	`, jobID, fixture.podID, claimOwner); err != nil {
		t.Fatal(err)
	}

	pod, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, jobID, claimOwner)
	if err != nil {
		t.Fatal(err)
	}
	if pod.ID != fixture.podID || pod.Status != models.PodStatusDestroying {
		t.Fatalf("prepared pod = %v, want %s destroying", pod, fixture.podID)
	}
	var status string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusDestroying {
		t.Fatalf("persisted pod status = %q, want destroying", status)
	}
}

func TestPrepareAndFinalizePodDestroyPostgresRejectStaleClaimGeneration(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	jobID := uuid.New()
	currentClaim := "worker-current-" + uuid.NewString()
	staleClaim := "worker-stale-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'in_progress', $3, now())
	`, jobID, fixture.podID, currentClaim); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, jobID, staleClaim); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("stale prepare error = %v, want %v", err, ErrJobLeaseLost)
	}
	if err := fixture.queries.FinalizePodDestroy(ctx, fixture.podID, jobID, staleClaim); !errors.Is(err, ErrJobLeaseLost) {
		t.Fatalf("stale finalize error = %v, want %v", err, ErrJobLeaseLost)
	}

	var status string
	var ownsVLAN bool
	if err := fixture.pool.QueryRow(ctx, `
		SELECT
			(SELECT status FROM pods WHERE id = $1),
			EXISTS (SELECT 1 FROM vlan_pool WHERE pod_id = $1)
	`, fixture.podID).Scan(&status, &ownsVLAN); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusActive || !ownsVLAN {
		t.Fatalf("stale claim changed status/ownership to %q/%v", status, ownsVLAN)
	}
}

func TestPreparePodDestroyPostgresRejectsLostVLANOwnership(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	claimOwner := "worker-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'in_progress', $3, now())
	`, jobID, fixture.podID, claimOwner); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, jobID, claimOwner); !errors.Is(err, ErrPodDestroyOwnershipLost) {
		t.Fatalf("prepare error = %v, want %v", err, ErrPodDestroyOwnershipLost)
	}
	var status string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusActive {
		t.Fatalf("ownership rejection changed pod status to %q", status)
	}
}

func TestPreparePodDestroyPostgresTreatsDestroyedPodAsNoOpBeforeVLANRead(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	jobID := uuid.New()
	claimOwner := "worker-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'in_progress', $3, now())
	`, jobID, fixture.podID, claimOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `UPDATE pods SET status = 'destroyed' WHERE id = $1`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, jobID, claimOwner); !errors.Is(err, ErrPodAlreadyDestroyed) {
		t.Fatalf("prepare error = %v, want %v", err, ErrPodAlreadyDestroyed)
	}
}

func TestPreparePodDestroyPostgresRejectsNonAuthoritativeDuplicate(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	authoritativeID := uuid.New()
	duplicateID := uuid.New()
	claimOwner := "worker-" + uuid.NewString()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at, created_at)
		VALUES
			($1, 'pod_destroy', jsonb_build_object('pod_id', $3::text), 'failed', NULL, NULL, now() - interval '1 minute'),
			($2, 'pod_destroy', jsonb_build_object('pod_id', $3::text), 'in_progress', $4, now(), now())
	`, authoritativeID, duplicateID, fixture.podID, claimOwner); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.queries.PreparePodDestroy(ctx, fixture.podID, duplicateID, claimOwner); !errors.Is(err, ErrPodDestroyJobObsolete) {
		t.Fatalf("prepare error = %v, want %v", err, ErrPodDestroyJobObsolete)
	}
	var status string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.PodStatusActive {
		t.Fatalf("obsolete duplicate changed pod status to %q", status)
	}
}

func TestPodDestroyPostgresDoesNotRequeueAfterVLANReassignment(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	otherPodID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
					SELECT $1, owner_id, $2, 'destroyed', vlan_id, subnet
					FROM pods
					WHERE id = $3
				`, otherPodID, "reassigned-"+otherPodID.String(), fixture.podID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM pods WHERE id = $1`, otherPodID)
	})
	if _, err := fixture.pool.Exec(ctx, `
					UPDATE vlan_pool SET pod_id = $2 WHERE pod_id = $1
				`, fixture.podID, otherPodID); err != nil {
		t.Fatal(err)
	}
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO jobs (id, type, payload, status, completed_at)
					VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
				`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "delayed"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if queued || job.ID != existingID || job.Status != models.JobStatusFailed {
		t.Fatalf("reassigned VLAN reconciliation = job %v queued %v", job, queued)
	}
}

func TestPodDestroyPostgresDoesNotRequeueNonterminalPodWithoutExactVLANOwnership(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1
	`, fixture.podID); err != nil {
		t.Fatal(err)
	}
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, completed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
	`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "delayed"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if queued || job.ID != existingID || job.Status != models.JobStatusFailed {
		t.Fatalf("released VLAN reconciliation = job %v queued %v", job, queued)
	}
}

func TestPodDestroyPostgresConcurrentRetryRequeuesOnce(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	existingID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
					INSERT INTO jobs (id, type, payload, status, completed_at)
					VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
				`, existingID, fixture.podID); err != nil {
		t.Fatal(err)
	}

	const producers = 8
	type result struct {
		job    *models.Job
		queued bool
		err    error
	}
	results := make(chan result, producers)
	start := make(chan struct{})
	for i := 0; i < producers; i++ {
		go func() {
			<-start
			job, queued, err := fixture.queries.CreatePodDestroyJob(
				ctx,
				fixture.podID,
				podDestroyPayload(t, fixture.podID, "concurrent_retry"),
			)
			results <- result{job: job, queued: queued, err: err}
		}()
	}
	close(start)

	queuedCount := 0
	for i := 0; i < producers; i++ {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.job.ID != existingID {
			t.Fatalf("retry returned job %s, want %s", got.job.ID, existingID)
		}
		if got.queued {
			queuedCount++
		}
	}
	if queuedCount != 1 {
		t.Fatalf("requeue publishers = %d, want 1", queuedCount)
	}
}

func installAdvisoryBlockingInsertTrigger(
	t *testing.T,
	fixture *podJobsPostgresFixture,
	table, predicate string,
) (int64, func()) {
	t.Helper()
	ctx := context.Background()
	suffix := strings.ReplaceAll(uuid.New().String(), "-", "")
	functionName := "test_block_insert_" + suffix
	triggerName := "test_block_insert_" + suffix
	key := time.Now().UnixNano() & 0x3fffffff
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
					CREATE FUNCTION %s() RETURNS trigger
					LANGUAGE plpgsql
					AS $$
					BEGIN
						PERFORM pg_advisory_xact_lock(%d);
						RETURN NEW;
					END;
					$$
				`, functionName, key)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, fmt.Sprintf(`
					CREATE TRIGGER %s
					BEFORE INSERT ON %s
					FOR EACH ROW
					WHEN (%s)
					EXECUTE FUNCTION %s()
				`, triggerName, table, predicate, functionName)); err != nil {
		_, _ = fixture.pool.Exec(ctx, "DROP FUNCTION "+functionName+"()")
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = fixture.pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON %s", triggerName, table))
		_, _ = fixture.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	}
	t.Cleanup(cleanup)
	return key, cleanup
}

func waitForAdvisoryWaiter(t *testing.T, pool *pgxpool.Pool, key int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(), `
						SELECT count(*)
						FROM pg_locks
						WHERE locktype = 'advisory'
						  AND NOT granted
						  AND objid::bigint = $1
					`, key).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("transaction did not wait on advisory lock %d", key)
}

func TestPodExpirationAndExtensionPostgresExtensionWins(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
					UPDATE pods SET expires_at = now() - interval '1 minute' WHERE id = $1
				`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	key, _ := installAdvisoryBlockingInsertTrigger(
		t,
		fixture,
		"pod_attestations",
		fmt.Sprintf("NEW.pod_id = '%s'::uuid", fixture.podID),
	)
	lockConn, err := fixture.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		}
	}()

	newExpiry := time.Now().Add(7 * 24 * time.Hour)
	extensionResult := make(chan error, 1)
	go func() {
		_, err := fixture.queries.ExtendPod(ctx, fixture.podID, fixture.ownerID, newExpiry)
		extensionResult <- err
	}()
	waitForAdvisoryWaiter(t, fixture.pool, key)

	expirationResult := make(chan error, 1)
	go func() {
		_, _, err := fixture.queries.CreateExpiredPodDestroyJob(
			ctx,
			fixture.podID,
			podDestroyPayload(t, fixture.podID, "expiration"),
		)
		expirationResult <- err
	}()
	select {
	case err := <-expirationResult:
		t.Fatalf("expiration returned before extension released the pod lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked = false

	if err := <-extensionResult; err != nil {
		t.Fatalf("extension failed: %v", err)
	}
	if err := <-expirationResult; !errors.Is(err, ErrPodDestroyNotNeeded) {
		t.Fatalf("expiration error = %v, want %v", err, ErrPodDestroyNotNeeded)
	}

	var attestations, destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
					SELECT
						(SELECT count(*) FROM pod_attestations WHERE pod_id = $1),
						(SELECT count(*) FROM jobs WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text)
				`, fixture.podID).Scan(&attestations, &destroyJobs); err != nil {
		t.Fatal(err)
	}
	if attestations != 1 || destroyJobs != 0 {
		t.Fatalf("extension-first result = %d attestations, %d destroy jobs", attestations, destroyJobs)
	}
}

func TestPodExpirationAndExtensionPostgresExpirationWins(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
					UPDATE pods SET expires_at = now() - interval '1 minute' WHERE id = $1
				`, fixture.podID); err != nil {
		t.Fatal(err)
	}

	key, _ := installAdvisoryBlockingInsertTrigger(
		t,
		fixture,
		"jobs",
		fmt.Sprintf("NEW.type = 'pod_destroy' AND NEW.payload->>'pod_id' = '%s'", fixture.podID),
	)
	lockConn, err := fixture.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		}
	}()

	expirationResult := make(chan error, 1)
	go func() {
		_, _, err := fixture.queries.CreateExpiredPodDestroyJob(
			ctx,
			fixture.podID,
			podDestroyPayload(t, fixture.podID, "expiration"),
		)
		expirationResult <- err
	}()
	waitForAdvisoryWaiter(t, fixture.pool, key)

	extensionResult := make(chan error, 1)
	go func() {
		_, err := fixture.queries.ExtendPod(
			ctx,
			fixture.podID,
			fixture.ownerID,
			time.Now().Add(7*24*time.Hour),
		)
		extensionResult <- err
	}()
	select {
	case err := <-extensionResult:
		t.Fatalf("extension returned before expiration released the pod lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked = false

	if err := <-expirationResult; err != nil {
		t.Fatalf("expiration failed: %v", err)
	}
	if err := <-extensionResult; !errors.Is(err, ErrPodExtensionRejected) {
		t.Fatalf("extension error = %v, want %v", err, ErrPodExtensionRejected)
	}

	var attestations, destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
					SELECT
						(SELECT count(*) FROM pod_attestations WHERE pod_id = $1),
						(SELECT count(*) FROM jobs WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text)
				`, fixture.podID).Scan(&attestations, &destroyJobs); err != nil {
		t.Fatal(err)
	}
	if attestations != 0 || destroyJobs != 1 {
		t.Fatalf("expiration-first result = %d attestations, %d destroy jobs", attestations, destroyJobs)
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
			nil,
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
		nil,
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

func insertProtectedVMJob(
	t *testing.T,
	fixture *podJobsPostgresFixture,
	jobType, status, claimOwner string,
) uuid.UUID {
	t.Helper()
	jobID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO jobs (id, type, payload, status, claimed_by, claimed_at)
		VALUES (
			$1,
			$2,
			jsonb_build_object('pod_id', $3::text, 'pod_vm_id', $4::text),
			$5,
			NULLIF($6, ''),
			CASE WHEN $6 = '' THEN NULL ELSE now() END
		)
	`, jobID, jobType, fixture.podID, fixture.podVMID, status, claimOwner); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func TestPodDestroyPostgresPendingMutatorBlocksThenCompletedRetrySucceeds(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	actionID := insertProtectedVMJob(t, fixture, models.JobTypeVMStart, models.JobStatusPending, "")

	job, created, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "explicit"),
	)
	var blocked *PodDestroyBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("destroy error = %v, want PodDestroyBlockedError", err)
	}
	if blocked.JobID != actionID || blocked.JobType != models.JobTypeVMStart || blocked.JobStatus != models.JobStatusPending {
		t.Fatalf("blocker = %+v, want pending vm_start %s", blocked, actionID)
	}
	if job != nil || created {
		t.Fatalf("blocked destroy returned job=%v created=%v", job, created)
	}

	if _, err := fixture.pool.Exec(ctx, `
		UPDATE jobs SET status = 'completed', completed_at = now() WHERE id = $1
	`, actionID); err != nil {
		t.Fatal(err)
	}
	job, created, err = fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "explicit_retry"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || job == nil || job.Type != models.JobTypePodDestroy {
		t.Fatalf("retry after action completion returned job=%v created=%v", job, created)
	}
}

func TestPodDestroyPostgresClaimedAndInProgressMutatorsBlockExpiration(t *testing.T) {
	for _, status := range []string{models.JobStatusClaimed, models.JobStatusInProgress} {
		t.Run(status, func(t *testing.T) {
			fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
			ctx := context.Background()
			if _, err := fixture.pool.Exec(ctx, `
				UPDATE pods SET expires_at = now() - interval '1 minute' WHERE id = $1
			`, fixture.podID); err != nil {
				t.Fatal(err)
			}
			insertProtectedVMJob(t, fixture, models.JobTypeVMReset, status, "worker-"+uuid.NewString())

			job, created, err := fixture.queries.CreateExpiredPodDestroyJob(
				ctx,
				fixture.podID,
				podDestroyPayload(t, fixture.podID, "expiration"),
			)
			if !errors.Is(err, ErrPodDestroyBlockedByMutator) {
				t.Fatalf("expiration error = %v, want %v", err, ErrPodDestroyBlockedByMutator)
			}
			if job != nil || created {
				t.Fatalf("blocked expiration returned job=%v created=%v", job, created)
			}
		})
	}
}

func TestPodDestroyPostgresTerminalRequeueWaitsForMutator(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	fixture.assignAvailableVLAN(t)
	ctx := context.Background()
	destroyID := uuid.New()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, completed_at)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'failed', now())
	`, destroyID, fixture.podID); err != nil {
		t.Fatal(err)
	}
	insertProtectedVMJob(t, fixture, models.JobTypeVMStop, models.JobStatusPending, "")

	job, queued, err := fixture.queries.CreatePodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "retry"),
	)
	if !errors.Is(err, ErrPodDestroyBlockedByMutator) {
		t.Fatalf("terminal requeue error = %v, want %v", err, ErrPodDestroyBlockedByMutator)
	}
	if job != nil || queued {
		t.Fatalf("blocked terminal requeue returned job=%v queued=%v", job, queued)
	}
	var status string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, destroyID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusFailed {
		t.Fatalf("blocked authoritative job status = %q, want failed", status)
	}
}

func TestPodDestroyPostgresVerifiedCurrentVMDestroyExclusion(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_vms SET status = $2 WHERE id = $1
	`, fixture.podVMID, models.VMStatusDeleted); err != nil {
		t.Fatal(err)
	}
	claimOwner := "worker-" + uuid.NewString()
	currentJobID := insertProtectedVMJob(t, fixture, models.JobTypeVMDestroy, models.JobStatusInProgress, claimOwner)

	job, created, err := fixture.queries.CreateEmptyPodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "last_vm"),
		&PodDestroyVMJobExclusion{
			JobID:      currentJobID,
			PodVMID:    fixture.podVMID,
			ClaimOwner: claimOwner,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created || job == nil || job.Type != models.JobTypePodDestroy {
		t.Fatalf("verified exclusion returned job=%v created=%v", job, created)
	}
}

func TestPodDestroyPostgresRejectsForgedWrongAndTerminalExclusions(t *testing.T) {
	tests := []struct {
		name       string
		jobType    string
		jobStatus  string
		claimOwner string
		exclusion  func(jobID, podVMID uuid.UUID, owner string) *PodDestroyVMJobExclusion
	}{
		{
			name:       "forged claim owner",
			jobType:    models.JobTypeVMDestroy,
			jobStatus:  models.JobStatusInProgress,
			claimOwner: "current-owner",
			exclusion: func(jobID, podVMID uuid.UUID, _ string) *PodDestroyVMJobExclusion {
				return &PodDestroyVMJobExclusion{JobID: jobID, PodVMID: podVMID, ClaimOwner: "forged-owner"}
			},
		},
		{
			name:       "wrong job type",
			jobType:    models.JobTypeVMStart,
			jobStatus:  models.JobStatusInProgress,
			claimOwner: "current-owner",
			exclusion: func(jobID, podVMID uuid.UUID, owner string) *PodDestroyVMJobExclusion {
				return &PodDestroyVMJobExclusion{JobID: jobID, PodVMID: podVMID, ClaimOwner: owner}
			},
		},
		{
			name:       "terminal vm destroy",
			jobType:    models.JobTypeVMDestroy,
			jobStatus:  models.JobStatusCompleted,
			claimOwner: "current-owner",
			exclusion: func(jobID, podVMID uuid.UUID, owner string) *PodDestroyVMJobExclusion {
				return &PodDestroyVMJobExclusion{JobID: jobID, PodVMID: podVMID, ClaimOwner: owner}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
			ctx := context.Background()
			if _, err := fixture.pool.Exec(ctx, `
				UPDATE pod_vms SET status = $2 WHERE id = $1
			`, fixture.podVMID, models.VMStatusDeleted); err != nil {
				t.Fatal(err)
			}
			jobID := insertProtectedVMJob(t, fixture, tc.jobType, tc.jobStatus, tc.claimOwner)

			job, created, err := fixture.queries.CreateEmptyPodDestroyJob(
				ctx,
				fixture.podID,
				podDestroyPayload(t, fixture.podID, "forged_exclusion"),
				tc.exclusion(jobID, fixture.podVMID, tc.claimOwner),
			)
			if !errors.Is(err, ErrPodDestroyExclusionInvalid) {
				t.Fatalf("exclusion error = %v, want %v", err, ErrPodDestroyExclusionInvalid)
			}
			if job != nil || created {
				t.Fatalf("invalid exclusion returned job=%v created=%v", job, created)
			}
		})
	}
}

func TestPodDestroyPostgresCurrentVMDestroyExclusionDoesNotHideOtherMutators(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_vms SET status = $2 WHERE id = $1
	`, fixture.podVMID, models.VMStatusDeleted); err != nil {
		t.Fatal(err)
	}
	claimOwner := "worker-" + uuid.NewString()
	currentJobID := insertProtectedVMJob(t, fixture, models.JobTypeVMDestroy, models.JobStatusInProgress, claimOwner)
	firstOther := insertProtectedVMJob(t, fixture, models.JobTypeVMStart, models.JobStatusPending, "")
	secondOther := insertProtectedVMJob(t, fixture, models.JobTypeVMStop, models.JobStatusClaimed, "other-"+uuid.NewString())

	job, created, err := fixture.queries.CreateEmptyPodDestroyJob(
		ctx,
		fixture.podID,
		podDestroyPayload(t, fixture.podID, "last_vm"),
		&PodDestroyVMJobExclusion{
			JobID:      currentJobID,
			PodVMID:    fixture.podVMID,
			ClaimOwner: claimOwner,
		},
	)
	var blocked *PodDestroyBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("destroy error = %v, want PodDestroyBlockedError", err)
	}
	if blocked.JobID != firstOther && blocked.JobID != secondOther {
		t.Fatalf("blocker %s is neither remaining mutator %s nor %s", blocked.JobID, firstOther, secondOther)
	}
	if blocked.JobID == currentJobID {
		t.Fatal("verified current vm_destroy incorrectly blocked itself")
	}
	if job != nil || created {
		t.Fatalf("blocked destroy returned job=%v created=%v", job, created)
	}
}

func TestPodDestroyAndVMActionPostgresActionCommitsFirst(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	key, _ := installAdvisoryBlockingInsertTrigger(
		t,
		fixture,
		"jobs",
		fmt.Sprintf("NEW.type = 'vm_start' AND NEW.payload->>'pod_id' = '%s'", fixture.podID),
	)
	lockConn, err := fixture.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		}
	}()

	actionResult := make(chan error, 1)
	actionPayload := vmJobPayload(t, fixture.podID, fixture.podVMID)
	go func() {
		_, err := fixture.queries.CreateVMJob(
			ctx,
			fixture.podID,
			fixture.podVMID,
			models.JobTypeVMStart,
			actionPayload,
		)
		actionResult <- err
	}()
	waitForAdvisoryWaiter(t, fixture.pool, key)

	destroyResult := make(chan error, 1)
	destroyPayload := podDestroyPayload(t, fixture.podID, "explicit")
	go func() {
		_, _, err := fixture.queries.CreatePodDestroyJob(ctx, fixture.podID, destroyPayload)
		destroyResult <- err
	}()
	select {
	case err := <-destroyResult:
		t.Fatalf("destroy returned before action released the pod lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-actionResult; err != nil {
		t.Fatalf("action enqueue failed: %v", err)
	}
	if err := <-destroyResult; !errors.Is(err, ErrPodDestroyBlockedByMutator) {
		t.Fatalf("destroy error = %v, want %v", err, ErrPodDestroyBlockedByMutator)
	}
}

func TestPodDestroyAndVMActionPostgresDestroyCommitsFirst(t *testing.T) {
	fixture := newPodJobsPostgresFixture(t, models.PodStatusActive)
	ctx := context.Background()
	key, _ := installAdvisoryBlockingInsertTrigger(
		t,
		fixture,
		"jobs",
		fmt.Sprintf("NEW.type = 'pod_destroy' AND NEW.payload->>'pod_id' = '%s'", fixture.podID),
	)
	lockConn, err := fixture.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		}
	}()

	destroyResult := make(chan error, 1)
	destroyPayload := podDestroyPayload(t, fixture.podID, "explicit")
	go func() {
		_, _, err := fixture.queries.CreatePodDestroyJob(ctx, fixture.podID, destroyPayload)
		destroyResult <- err
	}()
	waitForAdvisoryWaiter(t, fixture.pool, key)

	actionResult := make(chan error, 1)
	actionPayload := vmJobPayload(t, fixture.podID, fixture.podVMID)
	go func() {
		_, err := fixture.queries.CreateVMJob(
			ctx,
			fixture.podID,
			fixture.podVMID,
			models.JobTypeVMStart,
			actionPayload,
		)
		actionResult <- err
	}()
	select {
	case err := <-actionResult:
		t.Fatalf("action returned before destroy released the pod lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-destroyResult; err != nil {
		t.Fatalf("destroy enqueue failed: %v", err)
	}
	if err := <-actionResult; !errors.Is(err, ErrPodJobRejected) {
		t.Fatalf("action error = %v, want %v", err, ErrPodJobRejected)
	}
}
