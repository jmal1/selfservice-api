package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
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

// -- panic containment -----------------------------------------------------

// panicVC panics on the first vCenter call, reproducing the shape of the
// production incident: a govmomi property-destination bug made VMExists panic
// the moment the reconciler first ran for real.
type panicVC struct{ fakeHealthVC }

func (p *panicVC) VMExists(_ context.Context, _ string) (bool, error) {
	panic("simulated govmomi panic inside VMExists")
}

// TestReconcileTemplateHealthContainsPanics is the guard for the incident this
// feature caused the first time it ever executed. Template health runs in a
// goroutine inside the provision worker, so an escaping panic does not merely
// break health checks — it kills the process. And because the crash releases
// the leader lock, the next replica acquires it, runs the same cycle, and dies
// too. That walked the fault through all four workers and took provisioning
// down cluster-wide.
//
// Degrading to "health checks are broken" must always beat "provisioning is
// down".
func TestReconcileTemplateHealthContainsPanics(t *testing.T) {
	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "panic-template",
		VCenterTemplate: "vm-panic",
		IsActive:        true,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The assertion is simply that this call returns. Without the recover the
	// panic unwinds past it and the test binary dies — exactly as the worker
	// process did.
	_, err := reconcileTemplateHealth(
		context.Background(),
		newFakeHealthDB([]models.Template{tmpl}),
		&panicVC{},
		nil,
		logger,
		TemplateHealthReconcilerConfig{Interval: 12 * time.Hour},
	)
	if err == nil {
		t.Fatal("a panic inside the cycle must surface as an error, not be silently swallowed")
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("the error should identify itself as a contained panic so it is not mistaken for a vCenter fault; got: %v", err)
	}
}

// A panic must not be reported as a partially-healthy cycle. The named return
// used to implement the recover will otherwise surface whatever counts the
// aborted cycle had already accumulated -- the first version of this guard
// returned "Templates: 1, CheckerUp: true" for a cycle that died on its first
// vCenter call, and the worker logs those counts on the catch-up path.
func TestContainedPanicIsNotReportedAsSuccess(t *testing.T) {
	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "panic-template",
		VCenterTemplate: "vm-panic",
		IsActive:        true,
	}
	db := newFakeHealthDB([]models.Template{tmpl})

	counts, err := reconcileTemplateHealth(
		context.Background(),
		db,
		&panicVC{},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplateHealthReconcilerConfig{Interval: 12 * time.Hour},
	)
	if err == nil {
		t.Fatal("expected an error")
	}
	if counts.Templates != 0 || counts.Healthy != 0 {
		t.Errorf("a panicking cycle must not report progress; got %+v", counts)
	}
	if counts.CheckerUp {
		t.Error("CheckerUp must be false after a panic: the checker demonstrably did not work")
	}
}
