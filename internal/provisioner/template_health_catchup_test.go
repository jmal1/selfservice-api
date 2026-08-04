package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
)

// The template health reconciler's only in-process trigger is a 12h
// time.Ticker created at worker start. This platform deploys several times a
// day, so that ticker is reset long before it ever fires and the feature never
// runs. The worker therefore also runs a catch-up pass on leader acquisition,
// gated by templateHealthCycleDue so that frequent deploys do not each trigger
// a real vCenter clone.
//
// These tests pin that gate's three behaviours: bootstrap, suppress, catch up.

func healthStateCheckedAt(id uuid.UUID, at time.Time) *database.TemplateHealthState {
	return &database.TemplateHealthState{
		TemplateID:            id,
		HealthStatus:          "healthy",
		LastStructuralCheckAt: &at,
		UpdatedAt:             at,
	}
}

// A cluster that has never run a cycle must run one immediately, otherwise a
// freshly deployed platform has no template health data for 12 hours.
func TestCycleDueWhenNoCycleHasEverRun(t *testing.T) {
	db := newFakeHealthDB(nil)

	due, err := templateHealthCycleDue(context.Background(), db, 12*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !due {
		t.Fatal("expected first-ever cycle to be due; with no persisted clock the feature would never bootstrap")
	}
}

// The regression guard for clone storms: a deploy (or leader failover) minutes
// after a completed cycle must NOT run another. Each cycle performs a real
// clone → power-on → destroy, so an unguarded catch-up would turn every deploy
// into vCenter load against the same NFS datastores this check monitors.
func TestCycleNotDueWithinInterval(t *testing.T) {
	id := uuid.New()
	db := newFakeHealthDB(nil)
	db.states[id] = healthStateCheckedAt(id, time.Now().Add(-30*time.Minute))

	due, err := templateHealthCycleDue(context.Background(), db, 12*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if due {
		t.Fatal("cycle reported due 30m after a completed cycle; every deploy would trigger a vCenter clone")
	}
}

// A worker that has been down (or redeployed) across the interval boundary
// must catch up rather than wait a further full interval.
func TestCycleDueAfterIntervalElapsed(t *testing.T) {
	id := uuid.New()
	db := newFakeHealthDB(nil)
	db.states[id] = healthStateCheckedAt(id, time.Now().Add(-13*time.Hour))

	due, err := templateHealthCycleDue(context.Background(), db, 12*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !due {
		t.Fatal("cycle not due 13h after the last one; health data would go stale indefinitely")
	}
}

// Freshness must be judged by the newest check across all templates, not an
// arbitrary row. Using the oldest would re-run a cycle on every restart.
func TestCycleDueUsesNewestCheckAcrossTemplates(t *testing.T) {
	oldID, newID := uuid.New(), uuid.New()
	db := newFakeHealthDB(nil)
	db.states[oldID] = healthStateCheckedAt(oldID, time.Now().Add(-40*time.Hour))
	db.states[newID] = healthStateCheckedAt(newID, time.Now().Add(-10*time.Minute))

	due, err := templateHealthCycleDue(context.Background(), db, 12*time.Hour)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if due {
		t.Fatal("due-check keyed off the oldest row; a single stale template would force a clone on every restart")
	}
}

// An unreadable clock must surface as an error and must NOT be treated as
// "due". Failing open would clone on every restart during a DB incident —
// precisely when vCenter and the datastores are least able to absorb it. The
// 12h ticker remains as the backstop.
func TestCycleDueFailsClosedOnDBError(t *testing.T) {
	db := newFakeHealthDB(nil)
	db.newestErr = errors.New("connection refused")

	due, err := templateHealthCycleDue(context.Background(), db, 12*time.Hour)
	if err == nil {
		t.Fatal("expected the DB error to be reported, not swallowed")
	}
	if due {
		t.Fatal("due-check failed open on a DB error; a database incident would trigger a clone on every restart")
	}
}

// A zero/absent interval must fall back to 12h rather than 0, which would make
// every call due and reintroduce the clone storm.
func TestCycleDueZeroIntervalFallsBackTo12h(t *testing.T) {
	id := uuid.New()
	db := newFakeHealthDB(nil)
	db.states[id] = healthStateCheckedAt(id, time.Now().Add(-1*time.Hour))

	due, err := templateHealthCycleDue(context.Background(), db, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if due {
		t.Fatal("zero interval treated as 0 rather than the 12h default; every call would be due")
	}
}