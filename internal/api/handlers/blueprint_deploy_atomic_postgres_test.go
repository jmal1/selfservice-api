package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

type blueprintDeployPostgresFixture struct {
	*podCreatePostgresFixture
	blueprintID uuid.UUID
}

func newBlueprintDeployPostgresFixture(t *testing.T) *blueprintDeployPostgresFixture {
	t.Helper()

	base := newPodCreatePostgresFixture(t)
	fixture := &blueprintDeployPostgresFixture{
		podCreatePostgresFixture: base,
		blueprintID:              uuid.New(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO blueprints (
			id, name, description, created_by, allow_vm_additions, is_active
		) VALUES ($1, $2, 'atomic deployment fixture', $3, false, true)
	`, fixture.blueprintID, "atomic-blueprint-"+fixture.blueprintID.String(), fixture.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		INSERT INTO blueprint_vms (
			blueprint_id, template_id, display_name, boot_order, quantity
		) VALUES ($1, $2, 'target', 7, 1)
	`, fixture.blueprintID, fixture.templateID); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM audit_log WHERE user_id = $1`, fixture.userID)
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE payload->>'user_id' = $1`, fixture.userID.String())
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM pods WHERE owner_id = $1`, fixture.userID)
		_, _ = fixture.pool.Exec(cleanupCtx, `DELETE FROM blueprints WHERE id = $1`, fixture.blueprintID)
	})

	return fixture
}

func (f *blueprintDeployPostgresFixture) request(
	t *testing.T,
	publisher *recordingJobCreatedPublisher,
) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(map[string]string{"name": "deployed-blueprint"})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(
		f.queries,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	h.jobEvents = publisher

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/blueprints/"+f.blueprintID.String()+"/deploy",
		bytes.NewReader(body),
	)
	req.RemoteAddr = "192.0.2.1"
	ctx := middleware.WithUserID(req.Context(), f.userID)
	ctx = middleware.WithRole(ctx, models.RoleStudent)
	ctx = audit.WithUserID(ctx, f.userID)
	req = req.WithContext(ctx)
	req = withRouteParam(req, "blueprintID", f.blueprintID)
	rec := httptest.NewRecorder()
	h.DeployBlueprint(rec, req)
	return rec
}

func TestDeployBlueprintRollsBackResourcesWhenInitialJobInsertFails(t *testing.T) {
	fixture := newBlueprintDeployPostgresFixture(t)
	forcePodCreateJobInsertFailure(t, fixture.pool, fixture.userID)

	publisher := &recordingJobCreatedPublisher{}
	rec := fixture.request(t, publisher)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if publisher.calls != 0 {
		t.Fatalf("published %d job-created events after rolled-back insert, want 0", publisher.calls)
	}

	ctx := context.Background()
	var pods, vms, jobs, allocatedVLANs, audits int
	if err := fixture.pool.QueryRow(ctx, `SELECT COUNT(*) FROM pods WHERE owner_id = $1`, fixture.userID).Scan(&pods); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM pod_vms pv
		JOIN pods p ON p.id = pv.pod_id
		WHERE p.owner_id = $1
	`, fixture.userID).Scan(&vms); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs WHERE type = 'pod_create' AND payload->>'user_id' = $1
	`, fixture.userID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM vlan_pool vp
		JOIN pods p ON p.id = vp.pod_id
		WHERE p.owner_id = $1
	`, fixture.userID).Scan(&allocatedVLANs); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log WHERE user_id = $1 AND action = 'blueprint.deploy'
	`, fixture.userID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if pods != 0 || vms != 0 || jobs != 0 || allocatedVLANs != 0 || audits != 0 {
		t.Fatalf(
			"failed blueprint enqueue left durable state: pods=%d vms=%d jobs=%d allocated_vlans=%d audits=%d",
			pods,
			vms,
			jobs,
			allocatedVLANs,
			audits,
		)
	}
}

func TestDeployBlueprintCommitsResourcesAndInitialJobTogether(t *testing.T) {
	fixture := newBlueprintDeployPostgresFixture(t)
	publisher := &recordingJobCreatedPublisher{}
	rec := fixture.request(t, publisher)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	var response struct {
		JobID  uuid.UUID `json:"job_id"`
		PodID  uuid.UUID `json:"pod_id"`
		Status string    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.JobID == uuid.Nil || response.PodID == uuid.Nil || response.Status != models.PodStatusPending {
		t.Fatalf("unexpected response: %+v", response)
	}
	if publisher.calls != 1 || publisher.jobID != response.JobID || publisher.jobType != models.JobTypePodCreate {
		t.Fatalf("unexpected job-created publication: %+v", publisher)
	}

	ctx := context.Background()
	var podStatus string
	var blueprintID uuid.UUID
	var allowVMAdditions bool
	var vlanTag int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, blueprint_id, allow_vm_additions, vlan_id
		FROM pods
		WHERE id = $1 AND owner_id = $2
	`, response.PodID, fixture.userID).Scan(&podStatus, &blueprintID, &allowVMAdditions, &vlanTag); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusPending ||
		blueprintID != fixture.blueprintID ||
		allowVMAdditions ||
		vlanTag == 0 {
		t.Fatalf(
			"unexpected pod state: status=%q blueprint=%s allow_vm_additions=%t vlan=%d",
			podStatus,
			blueprintID,
			allowVMAdditions,
			vlanTag,
		)
	}

	var podVMID uuid.UUID
	var displayName, vmStatus string
	var bootOrder int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT id, display_name, status, boot_order
		FROM pod_vms
		WHERE pod_id = $1 AND template_id = $2
	`, response.PodID, fixture.templateID).Scan(&podVMID, &displayName, &vmStatus, &bootOrder); err != nil {
		t.Fatal(err)
	}
	if displayName != "target" || vmStatus != models.VMStatusPending || bootOrder != 7 {
		t.Fatalf("unexpected pod VM state: display=%q status=%q boot_order=%d", displayName, vmStatus, bootOrder)
	}

	var allocatedPodID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `
		SELECT pod_id FROM vlan_pool WHERE vlan_tag = $1
	`, vlanTag).Scan(&allocatedPodID); err != nil {
		t.Fatal(err)
	}
	if allocatedPodID != response.PodID {
		t.Fatalf("VLAN allocated to pod %s, want %s", allocatedPodID, response.PodID)
	}

	var jobCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM jobs
		WHERE type = 'pod_create' AND payload->>'user_id' = $1
	`, fixture.userID.String()).Scan(&jobCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("pod_create jobs = %d, want exactly 1", jobCount)
	}

	var jobType, jobStatus string
	var payload []byte
	if err := fixture.pool.QueryRow(ctx, `
		SELECT type, status, payload FROM jobs WHERE id = $1
	`, response.JobID).Scan(&jobType, &jobStatus, &payload); err != nil {
		t.Fatal(err)
	}
	if jobType != models.JobTypePodCreate || jobStatus != models.JobStatusPending {
		t.Fatalf("unexpected job state: type=%q status=%q", jobType, jobStatus)
	}
	var jobPayload struct {
		PodID   uuid.UUID `json:"pod_id"`
		PodName string    `json:"pod_name"`
		UserID  string    `json:"user_id"`
		VMs     []struct {
			PodVMID      uuid.UUID `json:"pod_vm_id"`
			TemplateName string    `json:"template_name"`
			DisplayName  string    `json:"display_name"`
			BootOrder    int       `json:"boot_order"`
		} `json:"vms"`
	}
	if err := json.Unmarshal(payload, &jobPayload); err != nil {
		t.Fatal(err)
	}
	if jobPayload.PodID != response.PodID ||
		jobPayload.PodName != "deployed-blueprint" ||
		jobPayload.UserID != fixture.userID.String() ||
		len(jobPayload.VMs) != 1 ||
		jobPayload.VMs[0].PodVMID != podVMID ||
		jobPayload.VMs[0].TemplateName != "vm-"+fixture.templateID.String() ||
		jobPayload.VMs[0].DisplayName != "target" ||
		jobPayload.VMs[0].BootOrder != 7 {
		t.Fatalf("job payload does not link committed blueprint resources: %+v", jobPayload)
	}

	var auditCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM audit_log
		WHERE user_id = $1
		  AND action = 'blueprint.deploy'
		  AND resource_type = 'pod'
		  AND resource_id = $2
		  AND details->>'blueprint_id' = $3
		  AND details->>'job_id' = $4
	`, fixture.userID, response.PodID, fixture.blueprintID.String(), response.JobID.String()).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("blueprint.deploy audit rows = %d, want 1", auditCount)
	}
}
