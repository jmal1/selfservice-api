package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

type concurrentJobPublisher struct {
	mu      sync.Mutex
	jobIDs  []uuid.UUID
	jobType []string
}

func (p *concurrentJobPublisher) PublishJobCreated(jobID uuid.UUID, jobType string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobIDs = append(p.jobIDs, jobID)
	p.jobType = append(p.jobType, jobType)
	return nil
}

func deletePodRequest(podID, userID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/pods/"+podID.String(), nil)
	req.RemoteAddr = "192.0.2.1"
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("podID", podID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
	ctx = middleware.WithUserID(ctx, userID)
	ctx = middleware.WithRole(ctx, models.RoleStudent)
	ctx = audit.WithUserID(ctx, userID)
	return req.WithContext(ctx)
}

func TestDeletePodPostgresConcurrentRequestsReuseAuthoritativeDestroyJob(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	podID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
		VALUES ($1, $2, 'concurrent-delete', 'active', $3, '10.253.253.0/24')
	`, podID, fixture.userID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}

	publisher := &concurrentJobPublisher{}
	h := NewHandler(
		fixture.queries,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	h.jobEvents = publisher

	start := make(chan struct{})
	recorders := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			rec := httptest.NewRecorder()
			h.DeletePod(rec, deletePodRequest(podID, fixture.userID))
			recorders <- rec
		}()
	}
	close(start)

	var authoritative uuid.UUID
	for i := 0; i < 2; i++ {
		rec := <-recorders
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			JobID uuid.UUID `json:"job_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if authoritative == uuid.Nil {
			authoritative = body.JobID
		} else if body.JobID != authoritative {
			t.Fatalf("concurrent delete returned job %s, want %s", body.JobID, authoritative)
		}
	}

	var count int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM jobs
		WHERE type = 'pod_destroy'
		  AND payload->>'pod_id' = $1
	`, podID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pod_destroy count = %d, want 1", count)
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.jobIDs) != 1 {
		t.Fatalf("job-created publishes = %d, want 1", len(publisher.jobIDs))
	}
	if publisher.jobIDs[0] != authoritative || publisher.jobType[0] != models.JobTypePodDestroy {
		t.Fatalf("published (%s, %s), want (%s, %s)", publisher.jobIDs[0], publisher.jobType[0], authoritative, models.JobTypePodDestroy)
	}
}

func TestDeletePodPostgresConcurrentRequestsRequeueFailedPreTransitionJobOnce(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	podID := uuid.New()
	var subnet string
	if err := fixture.pool.QueryRow(context.Background(), `
			SELECT subnet FROM vlan_pool WHERE vlan_tag = $1
		`, fixture.vlanTag).Scan(&subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
			INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
			VALUES ($1, $2, 'retry-delete', 'active', $3, $4)
		`, podID, fixture.userID, fixture.vlanTag, subnet); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
			UPDATE vlan_pool SET pod_id = $1, allocated_at = now() WHERE vlan_tag = $2
		`, podID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}
	jobID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
			INSERT INTO jobs (id, type, payload, status, completed_at)
			VALUES (
				$1,
				'pod_destroy',
				jsonb_build_object('pod_id', $2::text, 'user_id', $3::text),
				'failed',
				now()
			)
		`, jobID, podID, fixture.userID); err != nil {
		t.Fatal(err)
	}

	publisher := &concurrentJobPublisher{}
	h := NewHandler(
		fixture.queries,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	h.jobEvents = publisher

	start := make(chan struct{})
	recorders := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			rec := httptest.NewRecorder()
			h.DeletePod(rec, deletePodRequest(podID, fixture.userID))
			recorders <- rec
		}()
	}
	close(start)

	for i := 0; i < 2; i++ {
		rec := <-recorders
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delete status = %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			JobID  uuid.UUID `json:"job_id"`
			Status string    `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.JobID != jobID || body.Status != models.JobStatusPending {
			t.Fatalf("delete response = %s/%q, want %s/%q", body.JobID, body.Status, jobID, models.JobStatusPending)
		}
	}

	var count int
	if err := fixture.pool.QueryRow(context.Background(), `
			SELECT count(*)
			FROM jobs
			WHERE type = 'pod_destroy'
			  AND payload->>'pod_id' = $1
			  AND status = 'pending'
		`, podID.String()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pending authoritative jobs = %d, want 1", count)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.jobIDs) != 1 || publisher.jobIDs[0] != jobID {
		t.Fatalf("job-created publishes = %v, want [%s]", publisher.jobIDs, jobID)
	}
}

func TestDeletePodPostgresReportsReusedTerminalDestroyStatus(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	podID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
		VALUES ($1, $2, 'failed-delete', 'destroy_failed', $3, '10.253.252.0/24')
	`, podID, fixture.userID, fixture.vlanTag); err != nil {
		t.Fatal(err)
	}

	jobID := uuid.New()
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO jobs (id, type, payload, status)
		VALUES (
			$1,
			'pod_destroy',
			jsonb_build_object('pod_id', $2::text, 'user_id', $3::text),
			'failed'
		)
	`, jobID, podID, fixture.userID); err != nil {
		t.Fatal(err)
	}

	publisher := &concurrentJobPublisher{}
	h := NewHandler(
		fixture.queries,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	h.jobEvents = publisher
	rec := httptest.NewRecorder()
	h.DeletePod(rec, deletePodRequest(podID, fixture.userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		JobID  uuid.UUID `json:"job_id"`
		Status string    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.JobID != jobID || body.Status != models.JobStatusFailed {
		t.Fatalf("delete response = job %s status %q, want %s/%q", body.JobID, body.Status, jobID, models.JobStatusFailed)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if len(publisher.jobIDs) != 0 {
		t.Fatalf("reused terminal destroy published %d job-created events, want 0", len(publisher.jobIDs))
	}
}

func extendPodRequest(podID, userID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods/"+podID.String()+"/extend", nil)
	req.RemoteAddr = "192.0.2.1"
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("podID", podID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
	ctx = middleware.WithUserID(ctx, userID)
	ctx = middleware.WithRole(ctx, models.RoleStudent)
	ctx = audit.WithUserID(ctx, userID)
	return req.WithContext(ctx)
}

func TestExtendPodPostgresRejectsAuthoritativeDestroyWithoutAudit(t *testing.T) {
	fixture := newPodCreatePostgresFixture(t)
	podID := uuid.New()
	previousExpiry := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	if _, err := fixture.pool.Exec(context.Background(), `
			INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet, expires_at)
			VALUES ($1, $2, 'extension-race', 'active', $3, '10.253.251.0/24', $4)
		`, podID, fixture.userID, fixture.vlanTag, previousExpiry); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
			INSERT INTO jobs (type, payload, status)
			VALUES (
				'pod_destroy',
				jsonb_build_object('pod_id', $1::text, 'user_id', $2::text),
				'pending'
			)
		`, podID, fixture.userID); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(
		fixture.queries,
		nil,
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	rec := httptest.NewRecorder()
	h.ExtendPod(rec, extendPodRequest(podID, fixture.userID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("extend status = %d body=%s, want 409", rec.Code, rec.Body.String())
	}

	var (
		expiresAt    time.Time
		attestations int
		audits       int
	)
	if err := fixture.pool.QueryRow(context.Background(), `
			SELECT
				p.expires_at,
				(SELECT count(*) FROM pod_attestations WHERE pod_id = p.id),
				(SELECT count(*) FROM audit_log WHERE resource_id = p.id AND action = 'pod.extend')
			FROM pods p
			WHERE p.id = $1
		`, podID).Scan(&expiresAt, &attestations, &audits); err != nil {
		t.Fatal(err)
	}
	if !expiresAt.Equal(previousExpiry) {
		t.Fatalf("expiry changed to %s, want %s", expiresAt, previousExpiry)
	}
	if attestations != 0 || audits != 0 {
		t.Fatalf("rejected extension wrote %d attestations and %d success audits", attestations, audits)
	}
}
