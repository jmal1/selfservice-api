package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

type templateProvisionPostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *Queries
	templateID uuid.UUID
	jobID      uuid.UUID
}

func newTemplateProvisionPostgresFixture(t *testing.T, maxRetries int) *templateProvisionPostgresFixture {
	t.Helper()
	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("set TEST_DATABASE_URL to run template provision retry transaction tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(basePool.Close)

	schema := "template_provision_retry_" + uuid.NewString()[24:]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := basePool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = basePool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE")
	})

	scopedDSN := postgresDSNWithSearchPath(t, baseDSN, schema)
	if err := RunMigrations(scopedDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	fixture := &templateProvisionPostgresFixture{
		pool:       pool,
		queries:    NewQueries(pool),
		templateID: uuid.New(),
		jobID:      uuid.New(),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (
			id, name, vcenter_template, os_type, template_state,
			source_type, source_ref, staging_network
		)
		VALUES ($1, $2, $3, 'linux', 'provisioning', 'clone_vcenter', 'vm-source', 'staging')
	`, fixture.templateID, "retry-"+fixture.templateID.String(), "source-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload, status, retry_count, max_retries)
		VALUES (
			$1,
			'template_provision',
			jsonb_build_object('template_id', $2::text),
			'pending',
			0,
			$3
		)
	`, fixture.jobID, fixture.templateID, maxRetries); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *templateProvisionPostgresFixture) claimAndStart(t *testing.T, workerID string) *models.Job {
	t.Helper()
	job, err := f.queries.ClaimJob(context.Background(), workerID, true)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != f.jobID {
		t.Fatalf("claimed job = %#v, want %s", job, f.jobID)
	}
	if err := f.queries.UpdateJobStatus(context.Background(), job.ID, workerID, models.JobStatusInProgress, nil); err != nil {
		t.Fatal(err)
	}
	return job
}

func templateProvisionAttemptResult(first, current string, attempts int) []byte {
	result, err := json.Marshal(map[string]any{
		"error":             current,
		"raw_error":         current,
		"attempts":          attempts,
		"first_error":       first,
		"first_raw_error":   first,
		"first_attempt":     1,
		"current_error":     current,
		"current_raw_error": current,
		"current_attempt":   attempts,
	})
	if err != nil {
		panic(err)
	}
	return result
}

func TestTemplateProvisionRetryPostgresPreservesFailuresAcrossClaimsThenSucceeds(t *testing.T) {
	f := newTemplateProvisionPostgresFixture(t, 3)
	ctx := context.Background()

	first := templateProvisionAttemptResult("first clone failure", "first clone failure", 1)
	job := f.claimAndStart(t, "worker-one")
	if len(job.Result) != 0 {
		t.Fatalf("initial claim result = %s, want empty", job.Result)
	}
	nextAt := time.Now().Add(time.Minute)
	if err := f.queries.RetryTemplateProvisionJob(ctx, f.jobID, nextAt, first, "worker-one"); err != nil {
		t.Fatal(err)
	}
	assertTemplateProvisionRetryState(t, f, 1, first)
	f.makeRetryClaimable(t)

	job = f.claimAndStart(t, "worker-two")
	assertTemplateProvisionResultJSON(t, job.Result, first)
	started, err := f.queries.GetJob(ctx, f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	assertTemplateProvisionResultJSON(t, started.Result, first)

	second := templateProvisionAttemptResult("first clone failure", "second distinct clone failure", 2)
	if err := f.queries.RetryTemplateProvisionJob(ctx, f.jobID, time.Now().Add(time.Minute), second, "worker-two"); err != nil {
		t.Fatal(err)
	}
	assertTemplateProvisionRetryState(t, f, 2, second)
	f.makeRetryClaimable(t)

	f.claimAndStart(t, "worker-three")
	if err := f.queries.UpdateTemplateLifecycleState(ctx, f.templateID, models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		t.Fatal(err)
	}

	success := []byte(`{"message":"completed successfully"}`)
	if err := f.queries.UpdateJobStatus(ctx, f.jobID, "worker-three", models.JobStatusCompleted, success); err != nil {
		t.Fatal(err)
	}

	var (
		status        string
		templateState string
		result        []byte
	)
	if err := f.pool.QueryRow(ctx, `
		SELECT j.status, t.template_state, j.result
		FROM jobs j
		JOIN templates t ON t.id = $2
		WHERE j.id = $1
	`, f.jobID, f.templateID).Scan(&status, &templateState, &result); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusCompleted || templateState != models.TemplateStateConfiguring {
		t.Fatalf("success state = job %q template %q", status, templateState)
	}
	assertTemplateProvisionResultJSON(t, result, success)
}

func (f *templateProvisionPostgresFixture) makeRetryClaimable(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE jobs SET next_attempt_at = now() - interval '1 second' WHERE id = $1
	`, f.jobID); err != nil {
		t.Fatal(err)
	}
}

func assertTemplateProvisionRetryState(
	t *testing.T,
	f *templateProvisionPostgresFixture,
	wantRetries int,
	wantResult []byte,
) {
	t.Helper()
	var (
		status        string
		templateState string
		retryCount    int
		claimedBy     *string
		claimedAt     *time.Time
		startedAt     *time.Time
		completedAt   *time.Time
		nextAttemptAt *time.Time
		result        []byte
	)
	if err := f.pool.QueryRow(context.Background(), `
		SELECT j.status, t.template_state, j.retry_count, j.claimed_by,
		       j.claimed_at, j.started_at, j.completed_at, j.next_attempt_at, j.result
		FROM jobs j
		JOIN templates t ON t.id = $2
		WHERE j.id = $1
	`, f.jobID, f.templateID).Scan(
		&status,
		&templateState,
		&retryCount,
		&claimedBy,
		&claimedAt,
		&startedAt,
		&completedAt,
		&nextAttemptAt,
		&result,
	); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusPending || templateState != models.TemplateStateProvisioning || retryCount != wantRetries {
		t.Fatalf("retry state = job %q template %q retries %d", status, templateState, retryCount)
	}
	if claimedBy != nil || claimedAt != nil || startedAt != nil || completedAt != nil || nextAttemptAt == nil {
		t.Fatalf(
			"retry lifecycle fields = claimed_by:%v claimed_at:%v started_at:%v completed_at:%v next_attempt_at:%v",
			claimedBy,
			claimedAt,
			startedAt,
			completedAt,
			nextAttemptAt,
		)
	}
	assertTemplateProvisionResultJSON(t, result, wantResult)
}

func TestTemplateProvisionTerminalFailurePostgresIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryCount int
	}{
		{name: "initial non-retryable failure", retryCount: 0},
		{name: "exhausted retryable failure", retryCount: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateProvisionPostgresFixture(t, 3)
			if tc.retryCount > 0 {
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs SET retry_count = $2 WHERE id = $1
				`, f.jobID, tc.retryCount); err != nil {
					t.Fatal(err)
				}
			}
			f.claimAndStart(t, "terminal-worker")
			result := templateProvisionAttemptResult("originating failure", "terminal failure", tc.retryCount+1)
			transitioned, err := f.queries.FailTemplateProvisionJob(
				context.Background(),
				f.jobID,
				"terminal-worker",
				result,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !transitioned {
				t.Fatal("terminal transaction did not report provisioning-to-error transition")
			}
			assertTemplateProvisionTerminalState(t, f, result)
		})
	}
}

func TestTemplateProvisionClaimLossPostgresCannotRetryOrFailTemplate(t *testing.T) {
	for _, action := range []string{"retry", "terminal"} {
		t.Run(action, func(t *testing.T) {
			f := newTemplateProvisionPostgresFixture(t, 3)
			f.claimAndStart(t, "stale-worker")
			if _, err := f.pool.Exec(context.Background(), `
				UPDATE jobs SET claimed_by = 'new-worker' WHERE id = $1
			`, f.jobID); err != nil {
				t.Fatal(err)
			}
			result := templateProvisionAttemptResult("originating failure", "current failure", 1)
			var err error
			if action == "retry" {
				err = f.queries.RetryTemplateProvisionJob(
					context.Background(),
					f.jobID,
					time.Now().Add(time.Minute),
					result,
					"stale-worker",
				)
			} else {
				_, err = f.queries.FailTemplateProvisionJob(
					context.Background(),
					f.jobID,
					"stale-worker",
					result,
				)
			}
			if !errors.Is(err, ErrJobLeaseLost) {
				t.Fatalf("%s claim-loss error = %v, want ErrJobLeaseLost", action, err)
			}
			assertTemplateProvisionUnchanged(t, f, "new-worker")
		})
	}
}

func TestTemplateProvisionTerminalFailurePostgresRollsBackEitherWriteFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table string
		when  string
	}{
		{name: "template write fails first", table: "templates", when: "NEW.template_state = 'error'"},
		{name: "job write fails second", table: "jobs", when: "NEW.status = 'failed'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTemplateProvisionPostgresFixture(t, 3)
			f.claimAndStart(t, "rollback-worker")
			functionName := "fail_" + tc.table + "_" + uuid.NewString()[24:]
			triggerName := functionName + "_trigger"
			quotedFunction := pgx.Identifier{functionName}.Sanitize()
			quotedTrigger := pgx.Identifier{triggerName}.Sanitize()
			quotedTable := pgx.Identifier{tc.table}.Sanitize()
			if _, err := f.pool.Exec(context.Background(), `
				CREATE FUNCTION `+quotedFunction+`() RETURNS trigger
				LANGUAGE plpgsql AS $$
				BEGIN
					IF `+tc.when+` THEN
						RAISE EXCEPTION 'forced `+tc.table+` write failure';
					END IF;
					RETURN NEW;
				END
				$$
			`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.pool.Exec(context.Background(), `
				CREATE TRIGGER `+quotedTrigger+`
				BEFORE UPDATE ON `+quotedTable+`
				FOR EACH ROW EXECUTE FUNCTION `+quotedFunction+`()
			`); err != nil {
				t.Fatal(err)
			}

			_, err := f.queries.FailTemplateProvisionJob(
				context.Background(),
				f.jobID,
				"rollback-worker",
				templateProvisionAttemptResult("failure", "failure", 1),
			)
			if err == nil {
				t.Fatal("forced transactional write failure returned nil")
			}
			assertTemplateProvisionUnchanged(t, f, "rollback-worker")
		})
	}
}

func assertTemplateProvisionTerminalState(t *testing.T, f *templateProvisionPostgresFixture, wantResult []byte) {
	t.Helper()
	var status, templateState string
	var result []byte
	if err := f.pool.QueryRow(context.Background(), `
		SELECT j.status, t.template_state, j.result
		FROM jobs j
		JOIN templates t ON t.id = $2
		WHERE j.id = $1
	`, f.jobID, f.templateID).Scan(&status, &templateState, &result); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusFailed || templateState != models.TemplateStateError {
		t.Fatalf("terminal state = job %q template %q", status, templateState)
	}
	assertTemplateProvisionResultJSON(t, result, wantResult)
}

func assertTemplateProvisionUnchanged(t *testing.T, f *templateProvisionPostgresFixture, wantOwner string) {
	t.Helper()
	var status, templateState, claimedBy string
	var result []byte
	if err := f.pool.QueryRow(context.Background(), `
		SELECT j.status, t.template_state, j.claimed_by, j.result
		FROM jobs j
		JOIN templates t ON t.id = $2
		WHERE j.id = $1
	`, f.jobID, f.templateID).Scan(&status, &templateState, &claimedBy, &result); err != nil {
		t.Fatal(err)
	}
	if status != models.JobStatusInProgress ||
		templateState != models.TemplateStateProvisioning ||
		claimedBy != wantOwner ||
		result != nil {
		t.Fatalf(
			"state changed after rejected/rolled-back write: job=%q template=%q owner=%q result=%s",
			status,
			templateState,
			claimedBy,
			result,
		)
	}
}

func assertTemplateProvisionResultJSON(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode result %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode expected result %s: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("result:\n got %s\nwant %s", got, want)
	}
}
