package provisioner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestPodDestroyWorkerPreTransitionFailureCanRequeueAuthoritativeJob(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run pod destroy worker tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	queries := database.NewQueries(pool)

	ownerID := uuid.New()
	podID := uuid.New()
	jobID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, ownerID, ownerID.String(), ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	setupTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var vlanTag int
	var subnet string
	if err := setupTx.QueryRow(ctx, `
		SELECT vlan_tag, subnet
		FROM vlan_pool
		WHERE pod_id IS NULL
		ORDER BY id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&vlanTag, &subnet); err != nil {
		_ = setupTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := setupTx.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
		VALUES ($1, $2, $3, 'active', $4, $5)
	`, podID, ownerID, "worker-pre-transition-"+podID.String(), vlanTag, subnet); err != nil {
		_ = setupTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := setupTx.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = $1, allocated_at = now() WHERE vlan_tag = $2
	`, podID, vlanTag); err != nil {
		_ = setupTx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := setupTx.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, max_retries)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text), 'pending', 0)
	`, jobID, podID); err != nil {
		_ = setupTx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := setupTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM jobs WHERE id = $1`, jobID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM pods WHERE id = $1`, podID)
		_, _ = pool.Exec(context.Background(), `UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1`, podID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID)
	}()

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	functionName := "test_fail_destroy_transition_" + suffix
	triggerName := "test_fail_destroy_transition_" + suffix
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			RAISE EXCEPTION 'injected pre-transition failure';
		END;
		$$
	`, functionName)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER %s
		BEFORE UPDATE OF status ON pods
		FOR EACH ROW
		WHEN (NEW.id = '%s'::uuid AND NEW.status = 'destroying')
		EXECUTE FUNCTION %s()
	`, triggerName, podID, functionName)); err != nil {
		t.Fatal(err)
	}
	dropFailureTrigger := func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON pods", triggerName))
		_, _ = pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+functionName+"()")
	}
	defer dropFailureTrigger()

	workerID := "worker-" + uuid.NewString()
	job := &models.Job{}
	if err := pool.QueryRow(ctx, `
		UPDATE jobs
		SET status = 'claimed',
		    claimed_by = $2,
		    claimed_at = now()
		WHERE id = $1
		RETURNING id, type, payload, status, claimed_by, claimed_at,
		          retry_count, max_retries, next_attempt_at, rollback_steps, created_at
	`, jobID, workerID).Scan(
		&job.ID,
		&job.Type,
		&job.Payload,
		&job.Status,
		&job.ClaimedBy,
		&job.ClaimedAt,
		&job.RetryCount,
		&job.MaxRetries,
		&job.NextAttemptAt,
		&job.RollbackSteps,
		&job.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	provisioner := New(
		queries,
		nil,
		nil,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := processJobLifecycle(ctx, queries, nil, job, nil, provisioner.DestroyPod); err == nil {
		t.Fatal("worker destroy unexpectedly succeeded")
	}

	var podStatus, jobStatus string
	if err := pool.QueryRow(ctx, `
		SELECT
			(SELECT status FROM pods WHERE id = $1),
			(SELECT status FROM jobs WHERE id = $2)
	`, podID, jobID).Scan(&podStatus, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusActive || jobStatus != models.JobStatusFailed {
		t.Fatalf("pre-transition failure left pod/job at %q/%q, want active/failed", podStatus, jobStatus)
	}

	dropFailureTrigger()
	requeued, queued, err := queries.CreatePodDestroyJob(
		ctx,
		podID,
		[]byte(fmt.Sprintf(`{"pod_id":%q}`, podID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !queued || requeued.ID != jobID || requeued.Status != models.JobStatusPending {
		t.Fatalf("requeue = job %v queued %v, want %s pending", requeued, queued, jobID)
	}
}
