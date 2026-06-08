package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// podLifecycleFakeAPI is a stateful httptest server that pretends to be the
// Crucible API for lifecycle-check tests. It implements a tiny state machine:
//
//	pending -> active (after CreateActiveAfter calls to GET /pods/{id})
//	active  -> destroyed (after DestroyAfter calls following DELETE)
//
// This is enough to exercise the Check's polling logic deterministically
// without needing to inject a clock.
type podLifecycleFakeAPI struct {
	mu               sync.Mutex
	templates        []map[string]string
	pods             map[string]*fakePod
	createActiveAfter int
	destroyAfter     int
	createCalls      int
	deleteCalls      int
}

type fakePod struct {
	ID          string
	Name        string
	Status      string
	CreatedAt   time.Time
	getCalls    int
	deleteAt    *time.Time
}

func newFakeLifecycleAPI() *podLifecycleFakeAPI {
	return &podLifecycleFakeAPI{
		pods:              map[string]*fakePod{},
		templates:         []map[string]string{{"id": "tmpl-uuid-123", "name": "synthetic-noop"}},
		createActiveAfter: 1, // Become active on the first GET after create.
		destroyAfter:      1, // Become destroyed on the first GET after delete.
	}
}

func (f *podLifecycleFakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
			_ = json.NewEncoder(w).Encode(f.templates)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
			out := []map[string]any{}
			for _, p := range f.pods {
				if p.Status == "destroyed" {
					continue
				}
				out = append(out, map[string]any{
					"id":         p.ID,
					"name":       p.Name,
					"status":     p.Status,
					"created_at": p.CreatedAt,
				})
			}
			_ = json.NewEncoder(w).Encode(out)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pods":
			f.createCalls++
			var req struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			id := fmt.Sprintf("pod-%d", len(f.pods)+1)
			f.pods[id] = &fakePod{
				ID:        id,
				Name:      req.Name,
				Status:    "pending",
				CreatedAt: time.Now(),
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pod_id": id,
				"job_id": "job-1",
				"status": "pending",
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/pods/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/pods/")
			p, ok := f.pods[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			p.getCalls++
			// State transitions:
			if p.Status == "pending" && p.getCalls > f.createActiveAfter {
				p.Status = "active"
			}
			if p.deleteAt != nil && p.Status != "destroyed" {
				since := time.Since(*p.deleteAt)
				_ = since
				p.Status = "destroyed"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     p.ID,
				"name":   p.Name,
				"status": p.Status,
			})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/pods/"):
			f.deleteCalls++
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/pods/")
			p, ok := f.pods[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			now := time.Now()
			p.deleteAt = &now
			p.Status = "destroying"
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "del-1", "status": "pending"})

		default:
			http.NotFound(w, r)
		}
	})
}

func TestPodLifecycle_HappyPath(t *testing.T) {
	fake := newFakeLifecycleAPI()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   5 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 1 * time.Minute,
	})
	status, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil {
		t.Fatalf("lifecycle should succeed: status=%d err=%v", status, err)
	}
	if status != http.StatusOK {
		t.Errorf("status=%d, want 200", status)
	}
	if fake.createCalls != 1 {
		t.Errorf("createCalls=%d, want 1", fake.createCalls)
	}
	if fake.deleteCalls < 1 {
		t.Errorf("deleteCalls=%d, want >=1 (the lifecycle delete)", fake.deleteCalls)
	}
}

func TestPodLifecycle_PreCleanOrphan(t *testing.T) {
	fake := newFakeLifecycleAPI()
	// Seed an orphan pod older than the pre-clean cutoff.
	fake.pods["orphan-1"] = &fakePod{
		ID:        "orphan-1",
		Name:      SyntheticPodNamePrefix + "20990101t000000",
		Status:    "active",
		CreatedAt: time.Now().Add(-10 * time.Minute),
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   5 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	if _, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("lifecycle should succeed: %v", err)
	}
	if fake.deleteCalls < 2 {
		t.Errorf("expected at least 2 deletes (orphan + lifecycle), got %d", fake.deleteCalls)
	}
}

func TestPodLifecycle_PreCleanSparesRecentPods(t *testing.T) {
	fake := newFakeLifecycleAPI()
	// Seed a synthetic-named pod that is recent — should be spared by pre-clean.
	fake.pods["recent-1"] = &fakePod{
		ID:        "recent-1",
		Name:      SyntheticPodNamePrefix + "20990101t000000",
		Status:    "active",
		CreatedAt: time.Now(), // very recent
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   5 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	if _, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("lifecycle should succeed: %v", err)
	}
	// The lifecycle primary delete + the deferred safety delete both fire
	// (the defer always runs; destroyPod is idempotent on 404). So we
	// expect 2 deletes on the lifecycle pod and 0 on the recent orphan.
	if fake.deleteCalls != 2 {
		t.Errorf("recent orphan should be spared; deleteCalls=%d, want 2 (lifecycle + defer, no orphan)", fake.deleteCalls)
	}
}

func TestPodLifecycle_TemplateNotFound(t *testing.T) {
	fake := newFakeLifecycleAPI()
	fake.templates = []map[string]string{{"id": "other-id", "name": "other-template"}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   5 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	_, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "synthetic-noop") {
		t.Fatalf("expected template-not-found error mentioning synthetic-noop, got %v", err)
	}
}

func TestPodLifecycle_ReadyTimeout(t *testing.T) {
	fake := newFakeLifecycleAPI()
	// Never become active.
	fake.createActiveAfter = 99999
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   500 * time.Millisecond,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	_, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected ready-timeout error, got %v", err)
	}
}

func TestPodLifecycle_PodEntersErrorState(t *testing.T) {
	fake := newFakeLifecycleAPI()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	// Pre-create an "error" pod so the lifecycle's wait sees terminal failure.
	// Since the check creates its own pod, we monkey the create handler by
	// pre-populating a pod state and intercepting via a wrapped handler.
	// Simpler: rely on createSyntheticPod hitting the server, and let the
	// test inject error AFTER seeing a get call. To keep this test focused
	// and deterministic, we set the seeded fake pod's state to "error" by
	// adjusting the post-create transition: set createActiveAfter very high
	// then mutate after one GET.
	//
	// Implementation: rewrite the handler in-place to flip the status to
	// "error" after the second GET.
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		for _, p := range fake.pods {
			if p.Status == "pending" && p.getCalls >= 1 {
				p.Status = "error"
			}
		}
		fake.mu.Unlock()
		fake.handler().ServeHTTP(w, r)
	})
	srv.Config.Handler = wrapped

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   5 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	_, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil || !strings.Contains(err.Error(), "error") {
		t.Fatalf("expected terminal-error failure, got %v", err)
	}
}

func TestPodLifecycle_AlwaysDestroysOnFailure(t *testing.T) {
	fake := newFakeLifecycleAPI()
	fake.createActiveAfter = 99999
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := PodLifecycle(PodLifecycleConfig{
		TemplateName:   "synthetic-noop",
		ReadyTimeout:   200 * time.Millisecond,
		DestroyTimeout: 1 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	})
	_, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if fake.deleteCalls < 1 {
		t.Errorf("pod must be destroyed even on failure; deleteCalls=%d", fake.deleteCalls)
	}
}

// TestPodLifecycle_NameMetadata locks in the human-readable metadata required
// by the dashboard and alert payloads.
func TestPodLifecycle_NameMetadata(t *testing.T) {
	chk := PodLifecycle(DefaultPodLifecycleConfig("synthetic-noop"))
	if chk.Name() != "pod_lifecycle" {
		t.Errorf("Name() = %q, want pod_lifecycle (alert rules depend on this)", chk.Name())
	}
	if chk.Severity() != synthetic.SeverityCritical {
		t.Errorf("Severity() = %q, want critical", chk.Severity())
	}
	if strings.TrimSpace(chk.Title()) == "" {
		t.Error("Title() empty")
	}
	if len(chk.Title()) > 60 {
		t.Errorf("Title() too long: %q", chk.Title())
	}
	if strings.TrimSpace(chk.Description()) == "" {
		t.Error("Description() empty")
	}
}
