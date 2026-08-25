package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

type podDeletePostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *database.Queries
	ownerID    uuid.UUID
	templateID uuid.UUID
	podID      uuid.UUID
	podVMIDs   []uuid.UUID
	createJob  uuid.UUID
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

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_log WHERE resource_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM vm_placements WHERE job_id = $1`, fixture.createJob)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pod_portgroup_receipts WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE id = $1`, fixture.createJob)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pod_vms WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE id = $1`, fixture.podID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.ownerID)
	})

	return fixture
}

func (f *podDeletePostgresFixture) handler() *Handler {
	return NewHandler(f.queries, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func (f *podDeletePostgresFixture) deleteRequest() *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/pods/"+f.podID.String(), nil)
	req.RemoteAddr = "192.0.2.1"
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
	job, err := f.queries.ClaimJob(context.Background(), workerID, true)
	if err != nil {
		t.Fatal(err)
	}
	if job == nil || job.ID != f.createJob || job.Type != models.JobTypePodCreate {
		t.Fatalf("claimed job = %+v, want create job %s", job, f.createJob)
	}
}

func waitForJobLockHeld(t *testing.T, pool *pgxpool.Pool, jobID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		tx, err := pool.Begin(probeCtx)
		if err != nil {
			cancel()
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
		if strings.Contains(err.Error(), "SQLSTATE 55P03") || strings.Contains(err.Error(), "could not obtain lock on row") {
			return
		}
		t.Fatalf("probe job lock: %v", err)
	}
	t.Fatal("timed out waiting for cancellation to lock the create job")
}

func TestDeletePodCancelsNeverStartedPodAndIsIdempotent(t *testing.T) {
	fixture := newPodDeletePostgresFixture(t, 1)
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
		fixture.handler().DeletePod(rec, fixture.deleteRequest())
		deleteDone <- rec
	}()

	waitForJobLockHeld(t, fixture.pool, fixture.createJob)

	claimDone := make(chan struct {
		job *models.Job
		err error
	}, 1)
	go func() {
		job, err := fixture.queries.ClaimJob(ctx, "delete-race-worker", true)
		claimDone <- struct {
			job *models.Job
			err error
		}{job: job, err: err}
	}()

	select {
	case claimResult := <-claimDone:
		if claimResult.err != nil {
			t.Fatalf("claim raced cancellation: %v", claimResult.err)
		}
		if claimResult.job != nil {
			t.Fatalf("claim raced cancellation and claimed job %+v", claimResult.job)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for concurrent claim to lose the race")
	}

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
			want: http.StatusAccepted,
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
			want: http.StatusAccepted,
		},
		{
			name: "recovered stale job",
			setup: func(t *testing.T, f *podDeletePostgresFixture) {
				t.Helper()
				workerID := "recovered-delete-worker"
				f.claimCreateJob(t, workerID)
				if _, err := f.pool.Exec(context.Background(), `
					UPDATE jobs
					SET claimed_at = now() - interval '2 hours'
					WHERE id = $1
				`, f.createJob); err != nil {
					t.Fatal(err)
				}
				recovered, err := f.queries.RecoverStaleJobs(context.Background(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if recovered != 1 {
					t.Fatalf("recovered job count = %d, want 1", recovered)
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
					t.Fatalf("recovered job state = status:%s retry_count:%d claimed_by:%v claimed_at:%v started_at:%v", status, retryCount, claimedBy, claimedAt, startedAt)
				}
			},
			want: http.StatusAccepted,
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
			want: http.StatusAccepted,
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
			want: http.StatusAccepted,
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
			want: http.StatusAccepted,
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
			want: http.StatusAccepted,
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

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode delete response: %v", err)
			}
			if body["status"] != "pending" {
				t.Fatalf("fallback delete response = %+v, want queued destroy", body)
			}

			var podStatus string
			if err := fixture.pool.QueryRow(context.Background(), `SELECT status FROM pods WHERE id = $1`, fixture.podID).Scan(&podStatus); err != nil {
				t.Fatal(err)
			}
			if podStatus != models.PodStatusPending {
				t.Fatalf("pod status = %s, want pending while destroy job is queued", podStatus)
			}

			var destroyJobs int
			if err := fixture.pool.QueryRow(context.Background(), `
				SELECT COUNT(*) FROM jobs
				WHERE type = 'pod_destroy' AND payload->>'pod_id' = $1::text
			`, fixture.podID.String()).Scan(&destroyJobs); err != nil {
				t.Fatal(err)
			}
			if destroyJobs != 1 {
				t.Fatalf("destroy jobs = %d, want 1 queued destroy", destroyJobs)
			}
		})
	}
}
