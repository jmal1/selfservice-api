package handlers

import (
	"bytes"
	"context"
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

type recordingJobCreatedPublisher struct {
	jobID   uuid.UUID
	jobType string
	calls   int
}

func (p *recordingJobCreatedPublisher) PublishJobCreated(jobID uuid.UUID, jobType string) error {
	p.jobID = jobID
	p.jobType = jobType
	p.calls++
	return nil
}

type podCreatePostgresFixture struct {
	pool       *pgxpool.Pool
	queries    *database.Queries
	userID     uuid.UUID
	templateID uuid.UUID
	vlanTag    int
}

func newPodCreatePostgresFixture(t *testing.T) *podCreatePostgresFixture {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run pod creation persistence tests")
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

	fixture := &podCreatePostgresFixture{
		pool:       pool,
		queries:    database.NewQueries(pool),
		userID:     uuid.New(),
		templateID: uuid.New(),
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (
			id, oidc_sub, username, email, role, max_vcpus, max_ram_mb, max_pods
		) VALUES ($1, $2, $2, $3, 'student', 16, 65536, 10)
	`, fixture.userID, fixture.userID.String(), fixture.userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO templates (
			id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
			default_disk_gb, min_vcpus, min_ram_mb, is_active, visibility
		) VALUES ($1, $2, $3, 'linux', 2, 2048, 20, 1, 1024, true, 'public')
	`, fixture.templateID, "atomic-pod-"+fixture.templateID.String(), "vm-"+fixture.templateID.String()); err != nil {
		t.Fatal(err)
	}
	for vlanTag := 3000; vlanTag <= 4094; vlanTag++ {
		err := pool.QueryRow(ctx, `
			INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
			VALUES ($1, $2, 'all')
			ON CONFLICT DO NOTHING
			RETURNING vlan_tag
		`, vlanTag, fmt.Sprintf("10.%d.%d.0/24", vlanTag/256, vlanTag%256)).Scan(&fixture.vlanTag)
		if err == nil {
			break
		}
		if err != pgx.ErrNoRows {
			t.Fatal(err)
		}
	}
	if fixture.vlanTag == 0 {
		t.Fatal("no test VLAN tag available")
	}

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_log WHERE user_id = $1`, fixture.userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM jobs WHERE payload->>'user_id' = $1`, fixture.userID.String())
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM pods WHERE owner_id = $1`, fixture.userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM vlan_pool WHERE vlan_tag = $1`, fixture.vlanTag)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, fixture.templateID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, fixture.userID)
	})

	return fixture
}

func (f *podCreatePostgresFixture) request(t *testing.T, publisher *recordingJobCreatedPublisher) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(models.CreatePodRequest{
		Name: "atomic-pod",
		VMs: []models.VMRequest{{
			TemplateID:  f.templateID,
			DisplayName: "target",
		}},
	})
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1"
	ctx := middleware.WithUserID(req.Context(), f.userID)
	ctx = middleware.WithRole(ctx, models.RoleStudent)
	ctx = audit.WithUserID(ctx, f.userID)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.CreatePod(rec, req)
	return rec
}

func forcePodCreateJobInsertFailure(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()

	triggerSuffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	triggerName := "fail_pod_create_job_" + triggerSuffix
	functionName := triggerName + "_fn"

	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.type = 'pod_create' AND NEW.payload->>'user_id' = '%s' THEN
				RAISE EXCEPTION 'forced pod_create job insertion failure';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`, functionName, userID.String())); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE INSERT ON jobs
		FOR EACH ROW EXECUTE FUNCTION %s()
	`, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON jobs", triggerName))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
	})
}

func delayPodCreateJobInsert(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	delayJobInsert(t, pool, userID, models.JobTypePodCreate)
}

func delayVMAddJobInsert(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) {
	t.Helper()
	delayJobInsert(t, pool, userID, models.JobTypeVMAdd)
}

func delayJobInsert(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, jobType string) {
	t.Helper()
	triggerSuffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	triggerName := "delay_provisioning_job_" + triggerSuffix
	functionName := triggerName + "_fn"

	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			IF NEW.type = '%s' AND NEW.payload->>'user_id' = '%s' THEN
				PERFORM pg_sleep(1);
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql
	`, functionName, jobType, userID.String())); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(`
		CREATE TRIGGER %s BEFORE INSERT ON jobs
		FOR EACH ROW EXECUTE FUNCTION %s()
	`, triggerName, functionName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON jobs", triggerName))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", functionName))
	})
}

func (f *podCreatePostgresFixture) addVMRequest(
	t *testing.T,
	podID uuid.UUID,
	publisher *recordingJobCreatedPublisher,
) *httptest.ResponseRecorder {
	t.Helper()

	vcpus := 2
	ramMB := 2048
	body, err := json.Marshal(models.AddVMRequest{
		TemplateID:  f.templateID,
		DisplayName: "added-target",
		VCPUs:       &vcpus,
		RAMMB:       &ramMB,
	})
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

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods/"+podID.String()+"/vms", bytes.NewReader(body))
	req.RemoteAddr = "192.0.2.1"
	ctx := middleware.WithUserID(req.Context(), f.userID)
	ctx = middleware.WithRole(ctx, models.RoleStudent)
	ctx = audit.WithUserID(ctx, f.userID)
	req = withRouteParam(req.WithContext(ctx), "podID", podID)
	rec := httptest.NewRecorder()
	h.AddVM(rec, req)
	return rec
}

func TestCreatePodRollsBackResourcesWhenInitialJobInsertFails(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	ctx := context.Background()
	forcePodCreateJobInsertFailure(t, fixture.pool, fixture.userID)

	publisher := &recordingJobCreatedPublisher{}
	rec := fixture.request(t, publisher)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if publisher.calls != 0 {
		t.Fatalf("published %d job-created events after rolled-back insert, want 0", publisher.calls)
	}

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
		SELECT COUNT(*) FROM audit_log WHERE user_id = $1 AND action = 'pod.create'
	`, fixture.userID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if pods != 0 || vms != 0 || jobs != 0 || allocatedVLANs != 0 || audits != 0 {
		t.Fatalf(
			"failed enqueue left durable state: pods=%d vms=%d jobs=%d allocated_vlans=%d audits=%d",
			pods,
			vms,
			jobs,
			allocatedVLANs,
			audits,
		)
	}
}

func TestAddVMConcurrentRequestsDoNotExceedQuota(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	podID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO pods (
			id, owner_id, name, salt, vlan_id, subnet, status, allow_vm_additions
		) VALUES ($1, $2, 'vm-add-quota', 'vmadd', $3, '10.250.250.0/24', 'active', true)
	`, podID, fixture.userID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE vlan_pool SET pod_id = $1, allocated_at = now() WHERE vlan_tag = $2
	`, podID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE users SET max_vcpus = 2, max_ram_mb = 2048 WHERE id = $1
	`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	delayVMAddJobInsert(t, fixture.pool, fixture.userID)

	type result struct {
		rec       *httptest.ResponseRecorder
		publisher *recordingJobCreatedPublisher
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			publisher := &recordingJobCreatedPublisher{}
			results <- result{
				rec:       fixture.addVMRequest(t, podID, publisher),
				publisher: publisher,
			}
		}()
	}
	close(start)

	statuses := map[int]int{}
	published := 0
	var acceptedVMID, acceptedJobID uuid.UUID
	for i := 0; i < 2; i++ {
		got := <-results
		statuses[got.rec.Code]++
		published += got.publisher.calls
		if got.rec.Code == http.StatusAccepted {
			var response struct {
				JobID uuid.UUID `json:"job_id"`
				VMID  uuid.UUID `json:"vm_id"`
			}
			if err := json.Unmarshal(got.rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			acceptedVMID = response.VMID
			acceptedJobID = response.JobID
		} else if got.rec.Code == http.StatusConflict &&
			!strings.Contains(got.rec.Body.String(), "vcpus quota exceeded") {
			t.Fatalf("rejected add did not report vCPU quota: %s", got.rec.Body.String())
		}
	}
	if statuses[http.StatusAccepted] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent vm_add statuses = %v, want one 202 and one quota 409", statuses)
	}
	if published != 1 {
		t.Fatalf("vm_add job-created publishes = %d, want 1", published)
	}

	var vmCount, jobCount, auditCount int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT
			(SELECT COUNT(*) FROM pod_vms WHERE pod_id = $1),
			(SELECT COUNT(*) FROM jobs WHERE type = 'vm_add' AND payload->>'user_id' = $2),
			(SELECT COUNT(*) FROM audit_log WHERE user_id = $3 AND action = 'vm.add')
	`, podID, fixture.userID.String(), fixture.userID).Scan(&vmCount, &jobCount, &auditCount); err != nil {
		t.Fatal(err)
	}
	if vmCount != 1 || jobCount != 1 || auditCount != 1 {
		t.Fatalf("committed vm_add aggregate: vms=%d jobs=%d audits=%d, want 1 each", vmCount, jobCount, auditCount)
	}

	usage, err := fixture.queries.GetResourceUsage(context.Background(), fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := fixture.queries.GetUserByID(context.Background(), fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.UsedVCPUs != 2 ||
		usage.UsedRAMMB != 2048 ||
		usage.UsedVCPUs > user.MaxVCPUs ||
		usage.UsedRAMMB > user.MaxRAMMB {
		t.Fatalf("committed VM usage exceeds quota: usage=%+v user=%+v", usage, user)
	}

	var ownedPodID uuid.UUID
	var payloadPodID, payloadVMID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT pv.pod_id, j.payload->>'pod_id', j.payload->>'pod_vm_id'
		FROM pod_vms pv
		JOIN jobs j ON j.id = $2
		WHERE pv.id = $1
	`, acceptedVMID, acceptedJobID).Scan(&ownedPodID, &payloadPodID, &payloadVMID); err != nil {
		t.Fatal(err)
	}
	if ownedPodID != podID || payloadPodID != podID.String() || payloadVMID != acceptedVMID.String() {
		t.Fatalf(
			"vm_add ownership mismatch: pod=%s payload_pod=%s vm=%s payload_vm=%s",
			ownedPodID,
			payloadPodID,
			acceptedVMID,
			payloadVMID,
		)
	}
}

func TestCreatePodConcurrentRequestsDoNotExceedQuota(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE users SET max_pods = 1 WHERE id = $1
	`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	delayPodCreateJobInsert(t, fixture.pool, fixture.userID)

	type result struct {
		rec       *httptest.ResponseRecorder
		publisher *recordingJobCreatedPublisher
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			publisher := &recordingJobCreatedPublisher{}
			results <- result{
				rec:       fixture.request(t, publisher),
				publisher: publisher,
			}
		}()
	}
	close(start)

	statuses := map[int]int{}
	published := 0
	var acceptedPodID, acceptedJobID uuid.UUID
	for i := 0; i < 2; i++ {
		got := <-results
		statuses[got.rec.Code]++
		published += got.publisher.calls
		if got.rec.Code == http.StatusAccepted {
			var response struct {
				JobID uuid.UUID `json:"job_id"`
				PodID uuid.UUID `json:"pod_id"`
			}
			if err := json.Unmarshal(got.rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			acceptedPodID = response.PodID
			acceptedJobID = response.JobID
		}
	}
	if statuses[http.StatusAccepted] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent statuses = %v, want one 202 and one quota 409", statuses)
	}
	if published != 1 {
		t.Fatalf("job-created publishes = %d, want 1", published)
	}

	var pods, vms, jobs, allocatedVLANs int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT
			(SELECT COUNT(*) FROM pods WHERE owner_id = $1),
			(SELECT COUNT(*) FROM pod_vms pv JOIN pods p ON p.id = pv.pod_id WHERE p.owner_id = $1),
			(SELECT COUNT(*) FROM jobs WHERE type = 'pod_create' AND payload->>'user_id' = $1::text),
			(SELECT COUNT(*) FROM vlan_pool vp JOIN pods p ON p.id = vp.pod_id WHERE p.owner_id = $1)
	`, fixture.userID).Scan(&pods, &vms, &jobs, &allocatedVLANs); err != nil {
		t.Fatal(err)
	}
	if pods != 1 || vms != 1 || jobs != 1 || allocatedVLANs != 1 {
		t.Fatalf(
			"committed quota aggregate: pods=%d vms=%d jobs=%d allocated_vlans=%d, want 1 each",
			pods,
			vms,
			jobs,
			allocatedVLANs,
		)
	}
	usage, err := fixture.queries.GetResourceUsage(context.Background(), fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	user, err := fixture.queries.GetUserByID(context.Background(), fixture.userID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.ActivePods > user.MaxPods ||
		usage.UsedVCPUs > user.MaxVCPUs ||
		usage.UsedRAMMB > user.MaxRAMMB {
		t.Fatalf("committed usage exceeds quota: usage=%+v user=%+v", usage, user)
	}

	var ownedPodID, ownedVMID, ownedVLANPodID uuid.UUID
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT p.id, pv.id, vp.pod_id
		FROM pods p
		JOIN pod_vms pv ON pv.pod_id = p.id
		JOIN vlan_pool vp ON vp.pod_id = p.id
		WHERE p.owner_id = $1
	`, fixture.userID).Scan(&ownedPodID, &ownedVMID, &ownedVLANPodID); err != nil {
		t.Fatal(err)
	}
	var payloadPodID string
	var payloadVMID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT payload->>'pod_id', payload->'vms'->0->>'pod_vm_id'
		FROM jobs
		WHERE id = $1 AND type = 'pod_create'
	`, acceptedJobID).Scan(&payloadPodID, &payloadVMID); err != nil {
		t.Fatal(err)
	}
	if ownedPodID != acceptedPodID ||
		ownedVLANPodID != acceptedPodID ||
		payloadPodID != acceptedPodID.String() ||
		payloadVMID != ownedVMID.String() {
		t.Fatalf(
			"aggregate ownership mismatch: response_pod=%s pod=%s vlan_pod=%s payload_pod=%s vm=%s payload_vm=%s",
			acceptedPodID,
			ownedPodID,
			ownedVLANPodID,
			payloadPodID,
			ownedVMID,
			payloadVMID,
		)
	}
}

func TestCreatePodCommitsResourcesAndInitialJobTogether(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
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
	var vlanTag int
	var subnet string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT status, vlan_id, subnet FROM pods WHERE id = $1 AND owner_id = $2
	`, response.PodID, fixture.userID).Scan(&podStatus, &vlanTag, &subnet); err != nil {
		t.Fatal(err)
	}
	if podStatus != models.PodStatusPending || vlanTag == 0 || subnet == "" {
		t.Fatalf("unexpected pod state: status=%q vlan=%d subnet=%q", podStatus, vlanTag, subnet)
	}

	var podVMID uuid.UUID
	var vmStatus string
	if err := fixture.pool.QueryRow(ctx, `
		SELECT id, status FROM pod_vms WHERE pod_id = $1 AND template_id = $2
	`, response.PodID, fixture.templateID).Scan(&podVMID, &vmStatus); err != nil {
		t.Fatal(err)
	}
	if vmStatus != models.VMStatusPending {
		t.Fatalf("pod VM status = %q, want pending", vmStatus)
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
			PodVMID uuid.UUID `json:"pod_vm_id"`
		} `json:"vms"`
	}
	if err := json.Unmarshal(payload, &jobPayload); err != nil {
		t.Fatal(err)
	}
	if jobPayload.PodID != response.PodID ||
		jobPayload.PodName != "atomic-pod" ||
		jobPayload.UserID != fixture.userID.String() ||
		len(jobPayload.VMs) != 1 ||
		jobPayload.VMs[0].PodVMID != podVMID {
		t.Fatalf("job payload does not link committed resources: %+v", jobPayload)
	}

	var auditCount int
	if err := fixture.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM audit_log
		WHERE user_id = $1
		  AND action = 'pod.create'
		  AND resource_type = 'job'
		  AND resource_id = $2
		  AND details->>'pod_id' = $3
	`, fixture.userID, response.JobID, response.PodID.String()).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("pod.create audit rows = %d, want 1", auditCount)
	}
}
