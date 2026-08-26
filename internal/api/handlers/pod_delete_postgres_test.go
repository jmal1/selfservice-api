package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
)

type podDeletePostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *database.Queries
	ownerID    uuid.UUID
	templateID uuid.UUID
	podID      uuid.UUID
	podVMIDs   []uuid.UUID
	createJob  uuid.UUID
	jobIDs     map[uuid.UUID]struct{}
	vlanTag    int
}

func newPodDeletePostgresFixture(t *testing.T, vmCount int) *podDeletePostgresFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run pod deletion tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	fixture := &podDeletePostgresFixture{
		pool:       pool,
		queries:    database.NewQueries(pool),
		ownerID:    uuid.New(),
		templateID: uuid.New(),
		podID:      uuid.New(),
		createJob:  uuid.New(),
		jobIDs:     map[uuid.UUID]struct{}{},
	}
	fixture.jobIDs[fixture.createJob] = struct{}{}

	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, fixture.ownerID, fixture.ownerID.String(), fixture.ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (id, name, vcenter_template, os_type)
		VALUES ($1, $2, $3, 'linux')
	`, fixture.templateID, "delete-"+fixture.templateID.String(), "legacy-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, salt, vlan_id, subnet, status, allow_vm_additions)
		VALUES ($1, $2, $3, $4, 0, '', 'pending', true)
	`, fixture.podID, fixture.ownerID, "delete-"+fixture.podID.String(), "abc123"); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	vlanTag, subnet, err := fixture.queries.CheckoutVLAN(ctx, tx, fixture.podID, "all")
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	fixture.vlanTag = vlanTag
	if _, err := tx.Exec(ctx, `
		UPDATE pods SET vlan_id = $1, subnet = $2 WHERE id = $3
	`, vlanTag, subnet, fixture.podID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < vmCount; i++ {
		vmID := uuid.New()
		fixture.podVMIDs = append(fixture.podVMIDs, vmID)
		if _, err := pool.Exec(ctx, `
			INSERT INTO pod_vms (
				id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
			)
			VALUES ($1, $2, $3, $4, 1, 1024, 10, 'pending')
		`, vmID, fixture.podID, fixture.templateID, "vm-"+vmID.String()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload)
		VALUES ($1, 'pod_create', jsonb_build_object('pod_id', $2::text, 'user_id', $3::text))
	`, fixture.createJob, fixture.podID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE jobs
		SET created_at = now() - interval '1 day'
		WHERE id = $1
	`, fixture.createJob); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		fixture.cleanup(t)
	})

	return fixture
}

func (f *podDeletePostgresFixture) cleanup(t *testing.T) {
	t.Helper()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cleanupCancel()
	if err := f.cleanupErr(cleanupCtx); err != nil {
		t.Fatalf("fixture cleanup failed: %v", err)
	}
}

func (f *podDeletePostgresFixture) cleanupErr(ctx context.Context) error {
	if _, err := f.pool.Exec(ctx, `DELETE FROM audit_log WHERE resource_id = $1`, f.podID); err != nil {
		return fmt.Errorf("delete audit log: %w", err)
	}
	for jobID := range f.jobIDs {
		if _, err := f.pool.Exec(ctx, `DELETE FROM vm_placements WHERE job_id = $1`, jobID); err != nil {
			return fmt.Errorf("delete VM placements for job %s: %w", jobID, err)
		}
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM pod_portgroup_receipts WHERE pod_id = $1`, f.podID); err != nil {
		return fmt.Errorf("delete pod port-group receipts: %w", err)
	}
	for jobID := range f.jobIDs {
		if _, err := f.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, jobID); err != nil {
			return fmt.Errorf("delete tracked job %s: %w", jobID, err)
		}
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM pod_vms WHERE pod_id = $1`, f.podID); err != nil {
		return fmt.Errorf("delete pod VMs: %w", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1`, f.podID); err != nil {
		return fmt.Errorf("release VLAN allocation: %w", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM pods WHERE id = $1`, f.podID); err != nil {
		return fmt.Errorf("delete pod: %w", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM templates WHERE id = $1`, f.templateID); err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, f.ownerID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}

	var auditCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log WHERE resource_id = $1`, f.podID).Scan(&auditCount); err != nil {
		return fmt.Errorf("verify audit log cleanup: %w", err)
	}
	if auditCount != 0 {
		return fmt.Errorf("audit log cleanup left %d row(s)", auditCount)
	}

	var receiptCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pod_portgroup_receipts WHERE pod_id = $1`, f.podID).Scan(&receiptCount); err != nil {
		return fmt.Errorf("verify pod port-group receipt cleanup: %w", err)
	}
	if receiptCount != 0 {
		return fmt.Errorf("pod port-group receipt cleanup left %d row(s)", receiptCount)
	}

	var vmCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pod_vms WHERE pod_id = $1`, f.podID).Scan(&vmCount); err != nil {
		return fmt.Errorf("verify pod VM cleanup: %w", err)
	}
	if vmCount != 0 {
		return fmt.Errorf("pod VM cleanup left %d row(s)", vmCount)
	}

	var podCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pods WHERE id = $1`, f.podID).Scan(&podCount); err != nil {
		return fmt.Errorf("verify pod cleanup: %w", err)
	}
	if podCount != 0 {
		return fmt.Errorf("pod cleanup left %d row(s)", podCount)
	}

	var templateCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM templates WHERE id = $1`, f.templateID).Scan(&templateCount); err != nil {
		return fmt.Errorf("verify template cleanup: %w", err)
	}
	if templateCount != 0 {
		return fmt.Errorf("template cleanup left %d row(s)", templateCount)
	}

	var userCount int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE id = $1`, f.ownerID).Scan(&userCount); err != nil {
		return fmt.Errorf("verify user cleanup: %w", err)
	}
	if userCount != 0 {
		return fmt.Errorf("user cleanup left %d row(s)", userCount)
	}

	var vlanPodID sql.NullString
	var vlanAllocatedAt sql.NullTime
	if err := f.pool.QueryRow(ctx, `
		SELECT pod_id::text, allocated_at
		FROM vlan_pool
		WHERE vlan_tag = $1
	`, f.vlanTag).Scan(&vlanPodID, &vlanAllocatedAt); err != nil {
		return fmt.Errorf("verify VLAN cleanup: %w", err)
	}
	if vlanPodID.Valid && vlanPodID.String != "" {
		return fmt.Errorf("VLAN allocation still attached to pod %s", vlanPodID.String)
	}
	if vlanAllocatedAt.Valid {
		return fmt.Errorf("VLAN allocation retained timestamp %s", vlanAllocatedAt.Time)
	}

	for jobID := range f.jobIDs {
		var remaining int
		if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE id = $1`, jobID).Scan(&remaining); err != nil {
			return fmt.Errorf("verify tracked job %s cleanup: %w", jobID, err)
		}
		if remaining != 0 {
			return fmt.Errorf("tracked job %s still exists after cleanup", jobID)
		}
	}

	return nil
}

func (f *podDeletePostgresFixture) handler() *Handler {
	return NewHandler(f.queries, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func (f *podDeletePostgresFixture) handlerWithJobStatusPublisher(publisher jobStatusPublisher) *Handler {
	h := f.handler()
	h.jobStatusEvents = publisher
	return h
}

func (f *podDeletePostgresFixture) trackJobID(jobID uuid.UUID) {
	f.jobIDs[jobID] = struct{}{}
}

type recordingJobStatusPublisher struct {
	pool    *pgxpool.Pool
	podID   uuid.UUID
	jobID   uuid.UUID
	vlanTag int

	mu                     sync.Mutex
	calls                  []recordedJobStatusEvent
	observedPodStatus      string
	observedPodError       string
	observedJobStatus      string
	observedVLANAllocation string
	observedErr            error
}

type recordedJobStatusEvent struct {
	subject string
	evt     events.Event
}

type podDeleteRecordingJobCreatedPublisher struct {
	mu    sync.Mutex
	calls []recordedJobCreatedEvent
}

type recordedJobCreatedEvent struct {
	jobID   uuid.UUID
	jobType string
}

func (p *podDeleteRecordingJobCreatedPublisher) PublishJobCreated(jobID uuid.UUID, jobType string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, recordedJobCreatedEvent{jobID: jobID, jobType: jobType})
	return nil
}

func (p *recordingJobStatusPublisher) PublishRaw(subject string, evt events.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		p.setObservedErr(err)
		return nil
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var podStatus, podError, jobStatus string
	var vlanAllocation sql.NullString
	if err := tx.QueryRow(ctx, `
		SELECT status, COALESCE(error_message, '')
		FROM pods
		WHERE id = $1
		FOR UPDATE NOWAIT
	`, p.podID).Scan(&podStatus, &podError); err != nil {
		p.setObservedErr(err)
		return nil
	}
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM jobs
		WHERE id = $1
		FOR UPDATE NOWAIT
	`, p.jobID).Scan(&jobStatus); err != nil {
		p.setObservedErr(err)
		return nil
	}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(pod_id::text, '')
		FROM vlan_pool
		WHERE vlan_tag = $1
		FOR UPDATE NOWAIT
	`, p.vlanTag).Scan(&vlanAllocation); err != nil {
		p.setObservedErr(err)
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, recordedJobStatusEvent{subject: subject, evt: evt})
	p.observedPodStatus = podStatus
	p.observedPodError = podError
	p.observedJobStatus = jobStatus
	p.observedVLANAllocation = vlanAllocation.String
	p.observedErr = nil
	return nil
}

func (p *recordingJobStatusPublisher) setObservedErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observedErr = err
}

func (f *podDeletePostgresFixture) deleteRequest() *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/pods/"+f.podID.String(), nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req = withRoleAndUser(req, models.RoleStudent, f.ownerID)
	req = withRouteParam(req, "podID", f.podID)
	return req
}

func (f *podDeletePostgresFixture) decodeDeleteResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode delete response: %v", err)
	}
	return body
}

func (f *podDeletePostgresFixture) claimCreateJob(t *testing.T, workerID string) {
	t.Helper()
	res, err := f.pool.Exec(context.Background(), `
		UPDATE jobs
		SET status = 'claimed',
		    claimed_by = $2,
		    claimed_at = now()
		WHERE id = $1
		  AND status = 'pending'
		  AND claimed_by IS NULL
		  AND claimed_at IS NULL
		  AND started_at IS NULL
		  AND completed_at IS NULL
	`, f.createJob, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if rows := res.RowsAffected(); rows != 1 {
		t.Fatalf("claim create job rows = %d, want 1 for %s", rows, f.createJob)
	}
}

func (f *podDeletePostgresFixture) claimJob(t *testing.T, jobID uuid.UUID, workerID string) {
	t.Helper()
	res, err := f.pool.Exec(context.Background(), `
		UPDATE jobs
		SET status = 'claimed',
		    claimed_by = $2,
		    claimed_at = now()
		WHERE id = $1
		  AND status = 'pending'
		  AND claimed_by IS NULL
		  AND claimed_at IS NULL
		  AND started_at IS NULL
		  AND completed_at IS NULL
	`, jobID, workerID)
	if err != nil {
		t.Fatal(err)
	}
	if rows := res.RowsAffected(); rows != 1 {
		t.Fatalf("claim job rows = %d, want 1 for %s", rows, jobID)
	}
}

func (f *podDeletePostgresFixture) insertJob(t *testing.T, jobType string, payload any) uuid.UUID {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO jobs (id, type, payload)
		VALUES ($1, $2, $3)
	`, jobID, jobType, body); err != nil {
		t.Fatal(err)
	}
	f.trackJobID(jobID)
	return jobID
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

func waitForJobLockHeld(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		tx, err := pool.Begin(probeCtx)
		if err != nil {
			cancel()
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			t.Fatal(err)
		}
		var probeID uuid.UUID
		err = tx.QueryRow(probeCtx, `SELECT id FROM jobs WHERE id = $1 FOR UPDATE NOWAIT`, jobID).Scan(&probeID)
		if err == nil {
			_ = tx.Rollback(probeCtx)
			cancel()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = tx.Rollback(probeCtx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if strings.Contains(err.Error(), "SQLSTATE 55P03") || strings.Contains(err.Error(), "could not obtain lock on row") {
			return
		}
		t.Fatalf("probe job lock: %v", err)
	}
	t.Fatal("timed out waiting for cancellation to lock the create job")
}

func waitForPodLockHeld(t *testing.T, pool *pgxpool.Pool, podID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		tx, err := pool.Begin(probeCtx)
		if err != nil {
			cancel()
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			t.Fatal(err)
		}
		var probeID uuid.UUID
		err = tx.QueryRow(probeCtx, `SELECT id FROM pods WHERE id = $1 FOR UPDATE NOWAIT`, podID).Scan(&probeID)
		if err == nil {
			_ = tx.Rollback(probeCtx)
			cancel()
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = tx.Rollback(probeCtx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if strings.Contains(err.Error(), "SQLSTATE 55P03") || strings.Contains(err.Error(), "could not obtain lock on row") {
			return
		}
		t.Fatalf("probe pod lock: %v", err)
	}
	t.Fatal("timed out waiting for cancellation to lock the pod")
}

func probeExactCreateJobUnavailable(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	probeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	tx, err := pool.Begin(probeCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(probeCtx) }()

	var claimedID uuid.UUID
	err = tx.QueryRow(probeCtx, `
		SELECT id
		FROM jobs
		WHERE id = $1
		  AND status = 'pending'
		  AND (
		    $2
		    OR type NOT IN (
		      'pod_create',
		      'vm_add',
		      'template_provision',
		      'template_generalize',
		      'template_verify',
		      'template_revalidate',
		      'template_health_confirm',
		      'template_replica_build',
		      'image_import'
		    )
		    OR payload->>'cleanup_only' = 'true'
		  )
		  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`, jobID, true).Scan(&claimedID)
	if err == nil {
		t.Fatalf("exact create job probe unexpectedly claimed %s", claimedID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exact create job probe failed: %v", err)
	}
}

func TestDeletePodCancelsNeverStartedPodAndIsIdempotent(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	publisher := &recordingJobStatusPublisher{
		pool:    fixture.pool,
		podID:   fixture.podID,
		jobID:   fixture.createJob,
		vlanTag: fixture.vlanTag,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	podLockTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked uuid.UUID
	if err := podLockTx.QueryRow(ctx, `SELECT id FROM pods WHERE id = $1 FOR UPDATE`, fixture.podID).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		fixture.handlerWithJobStatusPublisher(publisher).DeletePod(rec, fixture.deleteRequest())
		deleteDone <- rec
	}()

	waitForJobLockHeld(t, fixture.pool, fixture.createJob)

	probeExactCreateJobUnavailable(t, fixture.pool, fixture.createJob)

	if err := podLockTx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancellation response")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	body := fixture.decodeDeleteResponse(t, rec)
	if body["status"] != "cancelled" || body["pod_status"] != models.PodStatusDestroyed {
		t.Fatalf("cancel response = %+v, want cancelled/destroyed", body)
	}
	if jobID, ok := body["job_id"].(string); !ok || jobID == "" {
		t.Fatalf("cancel response missing job_id: %+v", body)
	}

	var podStatus, podError string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, COALESCE(error_message, '')
		FROM pods WHERE id = $1
	`, fixture.podID).Scan(&podStatus, &podError); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusDestroyed || podError != models.PodErrorCancelledBeforeProvisioning {
		t.Fatalf("pod state = %s/%q, want destroyed/%q", podStatus, podError, models.PodErrorCancelledBeforeProvisioning)
	}

	publisher.mu.Lock()
	if len(publisher.calls) != 1 {
		publisher.mu.Unlock()
		t.Fatalf("job status publish calls = %d, want 1", len(publisher.calls))
	}
	call := publisher.calls[0]
	observedPodStatus := publisher.observedPodStatus
	observedPodError := publisher.observedPodError
	observedJobStatus := publisher.observedJobStatus
	observedVLANAllocation := publisher.observedVLANAllocation
	observedErr := publisher.observedErr
	publisher.mu.Unlock()
	wantSubject := fmt.Sprintf(events.SubjectJobStatus, fixture.createJob)
	if call.subject != wantSubject {
		t.Fatalf("job status subject = %q, want %q", call.subject, wantSubject)
	}
	if call.evt.Type != "job.status" || call.evt.JobID != fixture.createJob.String() || call.evt.Status != models.JobStatusFailed || call.evt.Message != models.PodErrorCancelledBeforeProvisioning {
		t.Fatalf("job status event = %+v, want failed cancellation event", call.evt)
	}
	if observedErr != nil {
		t.Fatalf("job status publish observed error: %v", observedErr)
	}
	if observedPodStatus != models.PodStatusDestroyed || observedPodError != models.PodErrorCancelledBeforeProvisioning {
		t.Fatalf("job status publish observed pod state = %s/%q", observedPodStatus, observedPodError)
	}
	if observedJobStatus != models.JobStatusFailed {
		t.Fatalf("job status publish observed job status = %s, want failed", observedJobStatus)
	}
	if observedVLANAllocation != "" {
		t.Fatalf("job status publish observed vlan allocation = %q, want released", observedVLANAllocation)
	}

	var jobStatus string
	var claimedBy *string
	var claimedAt, startedAt, completedAt *time.Time
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, claimed_by, claimed_at, started_at, completed_at
		FROM jobs WHERE id = $1
	`, fixture.createJob).Scan(&jobStatus, &claimedBy, &claimedAt, &startedAt, &completedAt); err != nil {
		t.Fatal(err)
	}
	if jobStatus != models.JobStatusFailed || claimedBy != nil || claimedAt != nil || startedAt != nil || completedAt == nil {
		t.Fatalf("cancelled job state = %s claimed=%v claimed_at=%v started_at=%v completed_at=%v", jobStatus, claimedBy, claimedAt, startedAt, completedAt)
	}

	rows, err := fixture.pool.Query(ctx, `
		SELECT status, vcenter_vm_id, vcenter_vm_name, ip_address
		FROM pod_vms
		WHERE pod_id = $1
		ORDER BY created_at
	`, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var vcenterVMID, vcenterVMName, ipAddress sql.NullString
		if err := rows.Scan(&status, &vcenterVMID, &vcenterVMName, &ipAddress); err != nil {
			t.Fatal(err)
		}
		if status != models.VMStatusDeleted ||
			(vcenterVMID.Valid && vcenterVMID.String != "") ||
			(vcenterVMName.Valid && vcenterVMName.String != "") ||
			(ipAddress.Valid && ipAddress.String != "") {
			t.Fatalf("pod VM not terminal after cancellation: status=%s vmid=%v vmname=%v ip=%v", status, vcenterVMID, vcenterVMName, ipAddress)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var vlanPodID *uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT pod_id FROM vlan_pool WHERE vlan_tag = $1
	`, fixture.vlanTag).Scan(&vlanPodID); err != nil {
		t.Fatal(err)
	}
	if vlanPodID != nil {
		t.Fatalf("VLAN allocation still attached to pod %s", vlanPodID)
	}

	var auditAction, auditMode string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT action, COALESCE(details->>'mode', '')
		FROM audit_log
		WHERE resource_id = $1
		ORDER BY id DESC
		LIMIT 1
	`, fixture.podID).Scan(&auditAction, &auditMode); err != nil {
		t.Fatal(err)
	}
	if auditAction != "pod.delete" || auditMode != "cancelled" {
		t.Fatalf("audit entry = %s %s", auditAction, auditMode)
	}

	rec2 := httptest.NewRecorder()
	fixture.handler().DeletePod(rec2, fixture.deleteRequest())
	if rec2.Code != http.StatusOK {
		t.Fatalf("idempotent delete status = %d", rec2.Code)
	}
	body2 := fixture.decodeDeleteResponse(t, rec2)
	if body2["status"] != "cancelled" {
		t.Fatalf("idempotent cancellation response = %+v, want cancelled", body2)
	}
	if jobID, ok := body2["job_id"].(string); !ok || jobID == "" {
		t.Fatalf("idempotent cancellation response missing job_id: %+v", body2)
	}
	var destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
	`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
		t.Fatal(err)
	}
	if destroyJobs != 0 {
		t.Fatalf("idempotent cancellation enqueued %d destroy jobs", destroyJobs)
	}
	publisher.mu.Lock()
	if len(publisher.calls) != 1 {
		publisher.mu.Unlock()
		t.Fatalf("idempotent cancellation republished job status %d times, want 1", len(publisher.calls))
	}
	publisher.mu.Unlock()
	fixture.cleanup(t)
}

func TestDeletePodFallsBackForCompetingVMMutators(t *testing.T) {
	tests := []struct {
		name    string
		jobType string
		claim   bool
	}{
		{name: "queued vm_destroy", jobType: models.JobTypeVMDestroy},
		{name: "claimed vm_destroy", jobType: models.JobTypeVMDestroy, claim: true},
		{name: "queued vm_add", jobType: models.JobTypeVMAdd},
		{name: "queued vm_snapshot", jobType: models.JobTypeVMSnapshot},
		{name: "queued vm_snapshot_delete", jobType: models.JobTypeVMSnapshotDelete},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodDeletePostgresFixture(t, 1)
			ctx := context.Background()
			payload := map[string]string{
				"pod_id":    fixture.podID.String(),
				"pod_vm_id": fixture.podVMIDs[0].String(),
			}
			jobID := fixture.insertJob(t, tc.jobType, payload)
			if tc.claim {
				fixture.claimJob(t, jobID, "claimed-mutator-worker")
			}

			rec := httptest.NewRecorder()
			fixture.handler().DeletePod(rec, fixture.deleteRequest())
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete status = %d body=%s, want 409 conflict", rec.Code, rec.Body.String())
			}

			var podStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
				t.Fatal(err)
			}
			if podStatus != models.PodStatusPending {
				t.Fatalf("pod status = %s, want pending while cancellation is rejected", podStatus)
			}

			var createJobStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
				t.Fatal(err)
			}
			if createJobStatus != models.JobStatusPending && createJobStatus != models.JobStatusClaimed {
				t.Fatalf("create job status = %s, want pending or claimed after conflict", createJobStatus)
			}

			var mutatorStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&mutatorStatus); err != nil {
				t.Fatal(err)
			}
			wantMutatorStatus := models.JobStatusPending
			if tc.claim {
				wantMutatorStatus = models.JobStatusClaimed
			}
			if mutatorStatus != wantMutatorStatus {
				t.Fatalf("mutator job status = %s, want %s", mutatorStatus, wantMutatorStatus)
			}

			var destroyJobs int
			if err := fixture.pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM jobs
				WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
			`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
				t.Fatal(err)
			}
			if destroyJobs != 0 {
				t.Fatalf("destroy jobs = %d, want 0 on conflict", destroyJobs)
			}
		})
	}
}

func TestDeletePodQueuesDestroyForTerminalProtectedJobs(t *testing.T) {
	tests := []struct {
		name           string
		terminalStatus string
	}{
		{name: "completed vm_snapshot_delete", terminalStatus: models.JobStatusCompleted},
		{name: "failed vm_snapshot_delete", terminalStatus: models.JobStatusFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodDeletePostgresFixture(t, 1)
			ctx := context.Background()
			jobID := fixture.insertJob(t, models.JobTypeVMSnapshotDelete, map[string]string{
				"pod_id":    fixture.podID.String(),
				"pod_vm_id": fixture.podVMIDs[0].String(),
			})
			if _, err := fixture.pool.Exec(ctx, `
				UPDATE jobs
				SET status = $2,
				    completed_at = now(),
				    result = '{}'::jsonb
				WHERE id = $1
			`, jobID, tc.terminalStatus); err != nil {
				t.Fatal(err)
			}

			rec := httptest.NewRecorder()
			fixture.handler().DeletePod(rec, fixture.deleteRequest())
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete status = %d body=%s, want 409 conservative fallback", rec.Code, rec.Body.String())
			}

			var podStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
				t.Fatal(err)
			}
			if podStatus != models.PodStatusPending {
				t.Fatalf("pod status = %s, want pending after destroy fallback", podStatus)
			}

			var createJobStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
				t.Fatal(err)
			}
			if createJobStatus != models.JobStatusPending {
				t.Fatalf("create job status = %s, want pending after destroy fallback", createJobStatus)
			}

			var terminalStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, jobID).Scan(&terminalStatus); err != nil {
				t.Fatal(err)
			}
			if terminalStatus != tc.terminalStatus {
				t.Fatalf("protected job status = %s, want %s", terminalStatus, tc.terminalStatus)
			}
		})
	}
}

func TestCancelPendingPodIfNeverStartedIgnoresLockedCompetingTerminalJob(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	ctx := context.Background()
	competingJobID := fixture.insertJob(t, models.JobTypePodDestroy, map[string]string{
		"pod_id":  fixture.podID.String(),
		"user_id": fixture.ownerID.String(),
	})
	if _, err := fixture.pool.Exec(ctx, `UPDATE jobs SET status = 'failed', completed_at = now() WHERE id = $1`, competingJobID); err != nil {
		t.Fatal(err)
	}

	lockCtx, lockCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer lockCancel()
	tx, err := fixture.pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	var locked uuid.UUID
	if err := tx.QueryRow(lockCtx, `SELECT id FROM jobs WHERE id = $1 FOR UPDATE`, competingJobID).Scan(&locked); err != nil {
		_ = tx.Rollback(lockCtx)
		t.Fatal(err)
	}

	done := make(chan struct {
		decision *database.PodDeletionDecision
		err      error
	}, 1)
	go func() {
		decision, err := fixture.queries.CancelPendingPodIfNeverStarted(lockCtx, fixture.podID)
		done <- struct {
			decision *database.PodDeletionDecision
			err      error
		}{decision: decision, err: err}
	}()

	var result struct {
		decision *database.PodDeletionDecision
		err      error
	}
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		_ = tx.Rollback(lockCtx)
		t.Fatal("timed out waiting for cancellation decision with locked competing job")
	}
	if result.err != nil {
		_ = tx.Rollback(lockCtx)
		t.Fatal(result.err)
	}
	if result.decision == nil || result.decision.Outcome != database.PodDeletionOutcomeNeedsDestroy {
		_ = tx.Rollback(lockCtx)
		t.Fatalf("decision = %#v, want needs_destroy", result.decision)
	}

	if err := tx.Rollback(lockCtx); err != nil {
		t.Fatal(err)
	}

	var podStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusPending {
		t.Fatalf("pod status = %s, want pending after conservative fallback", podStatus)
	}

	var createJobStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
		t.Fatal(err)
	}
	if createJobStatus != models.JobStatusPending {
		t.Fatalf("create job status = %s, want pending after conservative fallback", createJobStatus)
	}

	var destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
	`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
		t.Fatal(err)
	}
	if destroyJobs != 1 {
		t.Fatalf("destroy jobs = %d, want 1 locked terminal evidence row only", destroyJobs)
	}
}

func TestDeletePodRejectsProducerCommittedBeforePodLock(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	ctx := context.Background()

	producerTxCtx, producerCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer producerCancel()
	producerTx, err := fixture.pool.Begin(producerTxCtx)
	if err != nil {
		t.Fatal(err)
	}
	var heldPod uuid.UUID
	if err := producerTx.QueryRow(producerTxCtx, `SELECT id FROM pods WHERE id = $1 FOR UPDATE`, fixture.podID).Scan(&heldPod); err != nil {
		_ = producerTx.Rollback(producerTxCtx)
		t.Fatal(err)
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		fixture.handler().DeletePod(rec, fixture.deleteRequest())
		deleteDone <- rec
	}()

	waitForJobLockHeld(t, fixture.pool, fixture.createJob)

	producerJobID := uuid.New()
	if _, err := producerTx.Exec(producerTxCtx, `
		INSERT INTO jobs (id, type, payload)
		VALUES ($1, $2, jsonb_build_object('pod_id', $3::text, 'pod_vm_id', $4::text))
	`, producerJobID, models.JobTypeVMSnapshotDelete, fixture.podID, fixture.podVMIDs[0]); err != nil {
		_ = producerTx.Rollback(producerTxCtx)
		t.Fatal(err)
	}
	if err := producerTx.Commit(producerTxCtx); err != nil {
		t.Fatal(err)
	}
	fixture.trackJobID(producerJobID)

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delete response after producer commit")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete status = %d body=%s, want 409 conflict", rec.Code, rec.Body.String())
	}

	var podStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusPending {
		t.Fatalf("pod status = %s, want pending after rejection", podStatus)
	}

	var createJobStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
		t.Fatal(err)
	}
	if createJobStatus != models.JobStatusPending {
		t.Fatalf("create job status = %s, want pending after rejection", createJobStatus)
	}

	var destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
	`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
		t.Fatal(err)
	}
	if destroyJobs != 0 {
		t.Fatalf("destroy jobs = %d, want 0 after rejection", destroyJobs)
	}

	var competingJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE id = $1 AND type = $2 AND payload->>'pod_id' = $3::text
	`, producerJobID, models.JobTypeVMSnapshotDelete, fixture.podID.String()).Scan(&competingJobs); err != nil {
		t.Fatal(err)
	}
	if competingJobs != 1 {
		t.Fatalf("producer job count = %d, want 1 committed job", competingJobs)
	}
}

func TestDeletePodProducerWaitsForDestroyedPodAndReceivesRejection(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	publisher := &recordingJobStatusPublisher{
		pool:    fixture.pool,
		podID:   fixture.podID,
		jobID:   fixture.createJob,
		vlanTag: fixture.vlanTag,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	vlanLockTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var vlanID int64
	if err := vlanLockTx.QueryRow(ctx, `SELECT id FROM vlan_pool WHERE vlan_tag = $1 FOR UPDATE`, fixture.vlanTag).Scan(&vlanID); err != nil {
		_ = vlanLockTx.Rollback(ctx)
		t.Fatal(err)
	}

	deleteDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		fixture.handlerWithJobStatusPublisher(publisher).DeletePod(rec, fixture.deleteRequest())
		deleteDone <- rec
	}()

	waitForJobLockHeld(t, fixture.pool, fixture.createJob)
	waitForPodLockHeld(t, fixture.pool, fixture.podID)

	producerCtx, producerCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer producerCancel()
	producerDone := make(chan error, 1)
	go func() {
		_, err := fixture.queries.CreateVMJob(producerCtx, fixture.podID, fixture.podVMIDs[0], models.JobTypeVMSnapshotDelete, vmJobPayload(t, fixture.podID, fixture.podVMIDs[0]))
		producerDone <- err
	}()

	if err := vlanLockTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-deleteDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cancellation response")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var podStatus, podError string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, COALESCE(error_message, '')
		FROM pods
		WHERE id = $1
	`, fixture.podID).Scan(&podStatus, &podError); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusDestroyed || podError != models.PodErrorCancelledBeforeProvisioning {
		t.Fatalf("pod state = %s/%q, want destroyed/%q", podStatus, podError, models.PodErrorCancelledBeforeProvisioning)
	}

	var createJobStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
		t.Fatal(err)
	}
	if createJobStatus != models.JobStatusFailed {
		t.Fatalf("create job status = %s, want failed after cancellation", createJobStatus)
	}

	var producerErr error
	select {
	case producerErr = <-producerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for vm_snapshot_delete producer")
	}
	if !errors.Is(producerErr, database.ErrPodJobRejected) {
		t.Fatalf("producer error = %v, want %v", producerErr, database.ErrPodJobRejected)
	}

	var producerJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE type = $1 AND payload->>'pod_id' = $2::text
	`, models.JobTypeVMSnapshotDelete, fixture.podID.String()).Scan(&producerJobs); err != nil {
		t.Fatal(err)
	}
	if producerJobs != 0 {
		t.Fatalf("producer jobs = %d, want 0 after rejection", producerJobs)
	}

	publisher.mu.Lock()
	if len(publisher.calls) != 1 {
		publisher.mu.Unlock()
		t.Fatalf("job status publish calls = %d, want 1", len(publisher.calls))
	}
	publisher.mu.Unlock()
}

func TestDeletePodDoesNotPublishStatusOnTransactionFailure(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	publisher := &recordingJobStatusPublisher{
		pool:    fixture.pool,
		podID:   fixture.podID,
		jobID:   fixture.createJob,
		vlanTag: fixture.vlanTag,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	jobLockTx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked uuid.UUID
	if err := jobLockTx.QueryRow(ctx, `SELECT id FROM jobs WHERE id = $1 FOR UPDATE`, fixture.createJob).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = jobLockTx.Rollback(context.Background()) }()

	req := fixture.deleteRequest()
	reqCtx, reqCancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer reqCancel()
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	fixture.handlerWithJobStatusPublisher(publisher).DeletePod(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d body=%s, want 500 on transaction failure", rec.Code, rec.Body.String())
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.calls) != 0 {
		t.Fatalf("published %d job status events on transaction failure", len(publisher.calls))
	}
	if publisher.observedErr != nil {
		t.Fatalf("unexpected publish attempt on transaction failure: %v", publisher.observedErr)
	}
}

func TestDeletePodFailsWhenVLANReleaseTouchesMultipleRows(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	publisher := &recordingJobStatusPublisher{
		pool:    fixture.pool,
		podID:   fixture.podID,
		jobID:   fixture.createJob,
		vlanTag: fixture.vlanTag,
	}
	ctx := context.Background()
	var extraVLANTag int
	if err := fixture.pool.QueryRow(ctx, `
		UPDATE vlan_pool
		SET pod_id = $1,
		    allocated_at = now()
		WHERE id = (
			SELECT id
			FROM vlan_pool
			WHERE pod_id IS NULL
			  AND vlan_tag <> $2
			ORDER BY id
			LIMIT 1
		)
		RETURNING vlan_tag
	`, fixture.podID, fixture.vlanTag).Scan(&extraVLANTag); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	fixture.handlerWithJobStatusPublisher(publisher).DeletePod(rec, fixture.deleteRequest())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d body=%s, want 500 on ambiguous VLAN release", rec.Code, rec.Body.String())
	}

	var podStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusPending {
		t.Fatalf("pod status = %s, want pending after VLAN failure", podStatus)
	}

	var createJobStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
		t.Fatal(err)
	}
	if createJobStatus != models.JobStatusPending {
		t.Fatalf("create job status = %s, want pending after VLAN failure", createJobStatus)
	}

	var podVMStatus string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pod_vms WHERE id = $1`, fixture.podVMIDs[0]).Scan(&podVMStatus); err != nil {
		t.Fatal(err)
	}
	if podVMStatus != models.VMStatusPending {
		t.Fatalf("pod VM status = %s, want pending after VLAN failure", podVMStatus)
	}

	var vlanAssignments int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM vlan_pool
		WHERE pod_id = $1
	`, fixture.podID).Scan(&vlanAssignments); err != nil {
		t.Fatal(err)
	}
	if vlanAssignments != 2 {
		t.Fatalf("vlan assignments = %d, want 2", vlanAssignments)
	}

	var destroyJobs int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
	`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
		t.Fatal(err)
	}
	if destroyJobs != 0 {
		t.Fatalf("destroy jobs = %d, want 0 after VLAN failure", destroyJobs)
	}

	publisher.mu.Lock()
	if len(publisher.calls) != 0 {
		publisher.mu.Unlock()
		t.Fatalf("job status publish calls = %d, want 0", len(publisher.calls))
	}
	if publisher.observedErr != nil {
		publisher.mu.Unlock()
		t.Fatalf("unexpected publish attempt on VLAN failure: %v", publisher.observedErr)
	}
	publisher.mu.Unlock()
}

func TestDeletePodRejectsClaimedJobAndExternalEvidence(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *podDeletePostgresFixture)
		want  int
	}{
		{
			name: "claimed create job",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				f.claimCreateJob(t, "claimed-delete-worker")
			},
			want: http.StatusConflict,
		},
		{
			name: "retried create job",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				workerID := "retried-delete-worker"
				f.claimCreateJob(t, workerID)
				if err := f.queries.RetryJob(context.Background(), f.createJob, time.Now().Add(time.Hour), false, nil, workerID); err != nil {
					t.Fatal(err)
				}
				var (
					status     string
					retryCount int
					claimedBy  *string
					claimedAt  *time.Time
					startedAt  *time.Time
				)
				if err := f.pool.QueryRow(context.Background(), `
					SELECT status, retry_count, claimed_by, claimed_at, started_at
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&status, &retryCount, &claimedBy, &claimedAt, &startedAt); err != nil {
					t.Fatal(err)
				}
				if status != models.JobStatusPending || retryCount != 1 || claimedBy != nil || claimedAt != nil || startedAt != nil {
					t.Fatalf("retried job state = status:%s retry_count:%d claimed_by:%v claimed_at:%v started_at:%v", status, retryCount, claimedBy, claimedAt, startedAt)
				}
			},
			want: http.StatusConflict,
		},
		{
			name: "vcenter moref evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE pod_vms SET vcenter_vm_id = 'vm-1234', vcenter_vm_name = 'delete-vm' WHERE id = $1
				`, f.podVMIDs[0]); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusConflict,
		},
		{
			name: "placement evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					INSERT INTO vm_placements (
						pod_vm_id, job_id, template_id, source_ref, compute_resource_type,
						compute_resource_moref, resource_pool_moref, host_moref, host_name,
						drs_control, observed_free_memory_mb, reserved_memory_mb,
						capacity_reservation_mb, capacity_observed_at, admitted_headroom_mb
					)
					VALUES ($1, $2, $3, 'vm-legacy', 'ClusterComputeResource', 'domain-c9',
						'resgroup-9', 'host-9', 'host-9.example.invalid', 'disabled', 1024, 512, 0, now(), 1024)
				`, f.podVMIDs[0], f.createJob, f.templateID); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusConflict,
		},
		{
			name: "port group receipt evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				receipt, err := json.Marshal(map[string]any{
					"name":    fmt.Sprintf("Pod-VLAN%d", f.vlanTag),
					"vlan_id": f.vlanTag,
					"hosts": []map[string]any{{
						"host_name":     "esxi1.lab.jmal.io",
						"host_moref":    "host-1002",
						"compute_moref": "domain-c9",
						"vswitch_name":  "vSwitch0",
						"security": map[string]any{
							"allow_promiscuous": false,
							"mac_changes":       false,
							"forged_transmits":  false,
						},
						"preexisting": false,
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.pool.Exec(context.Background(), `
					INSERT INTO pod_portgroup_receipts (pod_id, create_job_id, receipt)
					VALUES ($1, $2, $3)
				`, f.podID, f.createJob, receipt); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusConflict,
		},
		{
			name: "rollback evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				rollbackSteps, err := json.Marshal([]map[string]any{{
					"name": "portgroup_create",
					"data": map[string]any{"name": fmt.Sprintf("Pod-VLAN%d", f.vlanTag)},
				}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET rollback_steps = $1::jsonb
					WHERE id = $2
				`, rollbackSteps, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			want: http.StatusConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodDeletePostgresFixture(t, 2)
			tc.setup(t, fixture)

			rec := httptest.NewRecorder()
			fixture.handler().DeletePod(rec, fixture.deleteRequest())
			if rec.Code != tc.want {
				t.Fatalf("delete status = %d body=%s, want %d", rec.Code, rec.Body.String(), tc.want)
			}

			if rec.Code == http.StatusConflict {
				var body map[string]any
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode conflict response: %v", err)
				}
				if body["error"] == "" {
					t.Fatalf("conflict response missing error: %+v", body)
				}

				var podStatus string
				if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
					t.Fatal(err)
				}
				if podStatus != models.PodStatusPending {
					t.Fatalf("pod status = %s, want pending while cancellation is rejected", podStatus)
				}

				var createJobStatus string
				if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
					t.Fatal(err)
				}
				if createJobStatus != models.JobStatusPending && createJobStatus != models.JobStatusClaimed {
					t.Fatalf("create job status = %s, want pending or claimed after conflict", createJobStatus)
				}

				var destroyJobs int
				if err := fixture.pool.QueryRow(context.Background(), `
					SELECT COUNT(*) FROM jobs
					WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
				`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
					t.Fatal(err)
				}
				if destroyJobs != 0 {
					t.Fatalf("destroy jobs = %d, want 0 on conflict", destroyJobs)
				}
				return
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode delete response: %v", err)
			}
			if body["status"] != "cancelled" {
				t.Fatalf("delete response = %+v, want cancelled", body)
			}
			if jobID, ok := body["job_id"].(string); !ok || jobID == "" {
				t.Fatalf("cancel response missing job_id: %+v", body)
			}
		})
	}
}

func TestDeletePodRejectsAmbiguousCreateJobLifecycleEvidence(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T, f *podDeletePostgresFixture)
		verify func(t *testing.T, f *podDeletePostgresFixture)
	}{
		{
			name: "cleanup_only payload",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET payload = jsonb_set(payload, '{cleanup_only}', 'true'::jsonb, true)
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var cleanupOnly bool
				if err := f.pool.QueryRow(context.Background(), `
					SELECT COALESCE(payload->>'cleanup_only', 'false')::boolean
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&cleanupOnly); err != nil {
					t.Fatal(err)
				}
				if !cleanupOnly {
					t.Fatal("cleanup_only evidence was cleared")
				}
			},
		},
		{
			name: "claimed_by only",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET claimed_by = 'ambiguous-delete-worker'
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var status string
				var claimedBy sql.NullString
				if err := f.pool.QueryRow(context.Background(), `
					SELECT status, claimed_by
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&status, &claimedBy); err != nil {
					t.Fatal(err)
				}
				if status != models.JobStatusPending || !claimedBy.Valid || claimedBy.String != "ambiguous-delete-worker" {
					t.Fatalf("claimed_by evidence was altered: status=%s claimed_by=%v", status, claimedBy)
				}
			},
		},
		{
			name: "claimed_at only",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET claimed_at = now()
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var status string
				var claimedAt sql.NullTime
				if err := f.pool.QueryRow(context.Background(), `
					SELECT status, claimed_at
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&status, &claimedAt); err != nil {
					t.Fatal(err)
				}
				if status != models.JobStatusPending || !claimedAt.Valid {
					t.Fatalf("claimed_at evidence was altered: status=%s claimed_at=%v", status, claimedAt)
				}
			},
		},
		{
			name: "next_attempt_at scheduled retry",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET retry_count = 1,
					    next_attempt_at = now() + interval '1 hour'
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var nextAttemptAt *time.Time
				if err := f.pool.QueryRow(context.Background(), `
					SELECT next_attempt_at
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&nextAttemptAt); err != nil {
					t.Fatal(err)
				}
				if nextAttemptAt == nil {
					t.Fatal("next_attempt_at evidence was cleared")
				}
			},
		},
		{
			name: "started_at evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET started_at = now()
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var startedAt *time.Time
				if err := f.pool.QueryRow(context.Background(), `
					SELECT started_at
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&startedAt); err != nil {
					t.Fatal(err)
				}
				if startedAt == nil {
					t.Fatal("started_at evidence was cleared")
				}
			},
		},
		{
			name: "completed_at evidence",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET completed_at = now()
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
			},
			verify: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				var completedAt *time.Time
				if err := f.pool.QueryRow(context.Background(), `
					SELECT completed_at
					FROM jobs
					WHERE id = $1
				`, f.createJob).Scan(&completedAt); err != nil {
					t.Fatal(err)
				}
				if completedAt == nil {
					t.Fatal("completed_at evidence was cleared")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodDeletePostgresFixture(t, 1)
			tc.setup(t, fixture)

			rec := httptest.NewRecorder()
			fixture.handler().DeletePod(rec, fixture.deleteRequest())
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete status = %d body=%s, want 409 conflict", rec.Code, rec.Body.String())
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode conflict response: %v", err)
			}
			if body["error"] == "" {
				t.Fatalf("conflict response missing error: %+v", body)
			}

			var podStatus string
			if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
				t.Fatal(err)
			}
			if podStatus != models.PodStatusPending {
				t.Fatalf("pod status = %s, want pending while cancellation is rejected", podStatus)
			}

			var destroyJobs int
			if err := fixture.pool.QueryRow(context.Background(), `
				SELECT COUNT(*) FROM jobs
				WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
			`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
				t.Fatal(err)
			}
			if destroyJobs != 0 {
				t.Fatalf("destroy jobs = %d, want 0 on conflict", destroyJobs)
			}

			tc.verify(t, fixture)
		})
	}
}

func TestDeletePodFixtureCleanupLeavesSameUserSentinelJob(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	sentinelJobID := uuid.New()
	sentinelPodID := uuid.New()
	ctx := context.Background()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO jobs (id, type, payload)
		VALUES ($1, 'pod_destroy', jsonb_build_object('pod_id', $2::text, 'user_id', $3::text))
	`, sentinelJobID, sentinelPodID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}

	fixture.cleanup(t)

	var exists bool
	if err := fixture.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, sentinelJobID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("same-user sentinel job was deleted by fixture cleanup")
	}
	if res, err := fixture.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, sentinelJobID); err != nil {
		t.Fatal(err)
	} else if rows := res.RowsAffected(); rows != 1 {
		t.Fatalf("sentinel delete rows = %d, want 1", rows)
	}
	if err := fixture.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, sentinelJobID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("same-user sentinel job still exists after explicit cleanup")
	}
}

func TestDeletePodCleanupFailsLoudlyWhenAuditRowLocked(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
	ctx := context.Background()
	audit.Log(ctx, fixture.queries, "pod.delete", audit.Resource("pod", fixture.podID))

	lockCtx, lockCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer lockCancel()
	tx, err := fixture.pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := tx.QueryRow(lockCtx, `
		SELECT id
		FROM audit_log
		WHERE resource_id = $1
		ORDER BY id DESC
		LIMIT 1
		FOR UPDATE
	`, fixture.podID).Scan(&lockedID); err != nil {
		_ = tx.Rollback(lockCtx)
		t.Fatal(err)
	}

	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cleanupCancel()
	if err := fixture.cleanupErr(cleanupCtx); err == nil {
		_ = tx.Rollback(lockCtx)
		t.Fatal("cleanupErr succeeded while audit row was locked")
	}
	if err := tx.Rollback(lockCtx); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePodRoutesRollbackStepsShapeMatrix(t *testing.T) {
	type shapeCase struct {
		name          string
		sqlNull       bool
		rollbackValue string
		wantCancel    bool
		wantDecision  database.PodDeletionOutcome
	}

	cases := []shapeCase{
		{name: "sql_null", sqlNull: true, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "json_null", rollbackValue: "null", wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "object", rollbackValue: `{"name":"portgroup_create"}`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "string", rollbackValue: `"legacy"`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "number", rollbackValue: `123`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "boolean", rollbackValue: `true`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "object_array", rollbackValue: `[{"name":"portgroup_create"}]`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "non_empty_array", rollbackValue: `["legacy"]`, wantDecision: database.PodDeletionOutcomeNeedsDestroy},
		{name: "empty_array", rollbackValue: `[]`, wantCancel: true, wantDecision: database.PodDeletionOutcomeCancelled},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPodDeletePostgresFixture(t, 1)
			ctx := context.Background()
			if tc.sqlNull {
				if _, err := fixture.pool.Exec(ctx, `
					UPDATE jobs
					SET rollback_steps = NULL
					WHERE id = $1
				`, fixture.createJob); err != nil {
					t.Fatal(err)
				}
			} else if _, err := fixture.pool.Exec(ctx, `
				UPDATE jobs
				SET rollback_steps = $1::jsonb
				WHERE id = $2
			`, tc.rollbackValue, fixture.createJob); err != nil {
				t.Fatal(err)
			}

			decision, err := fixture.queries.CancelPendingPodIfNeverStarted(ctx, fixture.podID)
			if err != nil {
				t.Fatalf("cancel pending pod: %v", err)
			}
			if decision == nil || decision.Outcome != tc.wantDecision || decision.JobID != fixture.createJob {
				t.Fatalf("database decision = %+v, want %s with create job", decision, tc.wantDecision)
			}

			if tc.wantCancel {
				rec := httptest.NewRecorder()
				publisher := &recordingJobStatusPublisher{
					pool:    fixture.pool,
					podID:   fixture.podID,
					jobID:   fixture.createJob,
					vlanTag: fixture.vlanTag,
				}
				h := fixture.handlerWithJobStatusPublisher(publisher)
				h.DeletePod(rec, fixture.deleteRequest())
				if rec.Code != http.StatusOK {
					t.Fatalf("delete status = %d body=%s, want 200 cancellation", rec.Code, rec.Body.String())
				}
				body := fixture.decodeDeleteResponse(t, rec)
				if body["status"] != "cancelled" {
					t.Fatalf("delete response = %+v, want cancelled", body)
				}
				if jobID, ok := body["job_id"].(string); !ok || jobID == "" {
					t.Fatalf("cancel response missing job_id: %+v", body)
				}
				var podStatus string
				if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
					t.Fatal(err)
				}
				if podStatus != models.PodStatusDestroyed {
					t.Fatalf("pod status = %s, want destroyed after cancellation", podStatus)
				}
				var createJobStatus string
				if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
					t.Fatal(err)
				}
				if createJobStatus != models.JobStatusFailed {
					t.Fatalf("create job status = %s, want failed cancellation", createJobStatus)
				}
				var destroyJobs int
				if err := fixture.pool.QueryRow(ctx, `
					SELECT COUNT(*) FROM jobs
					WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
				`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
					t.Fatal(err)
				}
				if destroyJobs != 0 {
					t.Fatalf("unexpected destroy jobs = %d", destroyJobs)
				}
				return
			}

			rec := httptest.NewRecorder()
			fixture.handler().DeletePod(rec, fixture.deleteRequest())
			if rec.Code != http.StatusConflict {
				t.Fatalf("delete status = %d body=%s, want 409 conflict", rec.Code, rec.Body.String())
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode conflict response: %v", err)
			}
			if body["error"] == "" {
				t.Fatalf("conflict response missing error: %+v", body)
			}

			var podStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
				t.Fatal(err)
			}
			if podStatus != models.PodStatusPending {
				t.Fatalf("pod status = %s, want pending while cancellation is rejected", podStatus)
			}

			var createJobStatus string
			if err := fixture.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, fixture.createJob).Scan(&createJobStatus); err != nil {
				t.Fatal(err)
			}
			if createJobStatus != models.JobStatusPending && createJobStatus != models.JobStatusClaimed {
				t.Fatalf("create job status = %s, want pending or claimed after conflict", createJobStatus)
			}

			var destroyJobs int
			if err := fixture.pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM jobs
				WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
			`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
				t.Fatal(err)
			}
			if destroyJobs != 0 {
				t.Fatalf("destroy jobs = %d, want 0 on conflict", destroyJobs)
			}
		})
	}
}
