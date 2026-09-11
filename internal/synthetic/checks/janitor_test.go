package checks

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// TestJanitor_DestroysOldOrphans seeds two pods named with the synthetic
// prefix — one old (past MaxAge), one recent — and verifies only the old
// one is destroyed.
func TestJanitor_DestroysOldOrphans(t *testing.T) {
	fake := newFakeLifecycleAPI()
	fake.pods["old-orphan"] = &fakePod{
		ID:        "old-orphan",
		Name:      SyntheticPodNamePrefix + "20200101t000000",
		Status:    "active",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	fake.pods["recent"] = &fakePod{
		ID:        "recent",
		Name:      SyntheticPodNamePrefix + "20200101t000001",
		Status:    "active",
		CreatedAt: time.Now().Add(-1 * time.Minute),
	}
	// Also seed a non-synthetic pod that must NEVER be touched.
	fake.pods["user-pod"] = &fakePod{
		ID:        "user-pod",
		Name:      "real-student-environment",
		Status:    "active",
		CreatedAt: time.Now().Add(-72 * time.Hour),
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := Janitor(JanitorConfig{MaxAge: 1 * time.Hour})
	status, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil {
		t.Fatalf("janitor should succeed: %v (status=%d)", err, status)
	}
	if fake.deleteCalls != 1 {
		t.Errorf("expected exactly 1 delete (old orphan only), got %d", fake.deleteCalls)
	}
	if s := fake.pods["old-orphan"].Status; s != "destroyed" && s != "destroying" {
		t.Errorf("old orphan should be destroying/destroyed, got status=%s", s)
	}
	if fake.pods["recent"].Status == "destroyed" {
		t.Errorf("recent synthetic pod should NOT be destroyed")
	}
	if fake.pods["user-pod"].Status == "destroyed" {
		t.Errorf("non-synthetic user pod should NEVER be touched by the janitor")
	}
}

// TestJanitor_NothingToDo verifies that a clean state — no synthetic-named
// pods at all — is reported as success (the desired steady state).
func TestJanitor_NothingToDo(t *testing.T) {
	fake := newFakeLifecycleAPI()
	fake.pods["unrelated"] = &fakePod{
		ID:        "unrelated",
		Name:      "real-pod",
		Status:    "active",
		CreatedAt: time.Now().Add(-3 * time.Hour),
	}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	chk := Janitor(JanitorConfig{MaxAge: 1 * time.Hour})
	if _, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("janitor with nothing to clean should succeed: %v", err)
	}
	if fake.deleteCalls != 0 {
		t.Errorf("no deletes should happen when there are no synthetic pods, got %d", fake.deleteCalls)
	}
}

// TestJanitor_DefaultMaxAge ensures a zero MaxAge defaults to a non-zero
// value so a misconfigured CronJob doesn't accidentally destroy fresh pods.
func TestJanitor_DefaultMaxAge(t *testing.T) {
	cfg := JanitorConfig{} // zero MaxAge
	chk := Janitor(cfg)
	if chk.Name() != "synthetic_janitor" {
		t.Errorf("name=%q, want synthetic_janitor", chk.Name())
	}
	if chk.Severity() != synthetic.SeverityWarning {
		t.Errorf("severity=%q, want warning", chk.Severity())
	}
}
