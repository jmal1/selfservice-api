package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// --------------------------------------------------------------------------
// Shared fake backend
// --------------------------------------------------------------------------

// fakeMutex simulates the "exactly one holder" invariant of a Postgres advisory
// lock. Multiple fakeLockBackend instances that share the same *fakeMutex
// contend for the same logical lock.
type fakeMutex struct {
	mu   sync.Mutex
	held bool
}

type fakeLockBackend struct {
	mu       *fakeMutex
	acquired bool // whether THIS instance currently holds the lock
	closed   bool

	// keepaliveFn is called on Keepalive; nil means always OK.
	keepaliveFn func() error
}

func (f *fakeLockBackend) TryAcquire(_ context.Context) (bool, error) {
	f.mu.mu.Lock()
	defer f.mu.mu.Unlock()
	if f.mu.held {
		return false, nil
	}
	f.mu.held = true
	f.acquired = true
	return true, nil
}

func (f *fakeLockBackend) Keepalive(_ context.Context) error {
	if f.keepaliveFn != nil {
		return f.keepaliveFn()
	}
	return nil
}

// Release only unlocks if this backend instance actually acquired the lock.
// pg_advisory_unlock in real Postgres is a no-op (returns false + WARNING) when
// called without a prior pg_try_advisory_lock — the fake mirrors that.
func (f *fakeLockBackend) Release(_ context.Context) error {
	f.mu.mu.Lock()
	defer f.mu.mu.Unlock()
	if f.acquired {
		f.mu.held = false
		f.acquired = false
	}
	return nil
}

func (f *fakeLockBackend) Close(_ context.Context) error {
	f.closed = true
	return nil
}

// makeElector returns an Elector whose backend uses shared as the contended lock.
// retryInterval is short so tests complete quickly.
func makeElector(shared *fakeMutex, retryInterval time.Duration) *Elector {
	factory := func(_ context.Context) (LockBackend, error) {
		return &fakeLockBackend{mu: shared}, nil
	}
	return New(factory, retryInterval, slog.Default())
}

// --------------------------------------------------------------------------
// Test: exactly one leader among two concurrent electors
// --------------------------------------------------------------------------

func TestTwoConcurrentElectors_ExactlyOneLeader(t *testing.T) {
	t.Parallel()
	shared := &fakeMutex{}
	const retry = 5 * time.Millisecond

	e1 := makeElector(shared, retry)
	e2 := makeElector(shared, retry)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go e1.Run(ctx)
	go e2.Run(ctx)

	// Give both electors several retry cycles to settle.
	time.Sleep(100 * time.Millisecond)

	leaders := 0
	if e1.IsLeader() {
		leaders++
	}
	if e2.IsLeader() {
		leaders++
	}

	if leaders != 1 {
		t.Errorf("expected exactly 1 leader, got %d (e1=%v e2=%v)",
			leaders, e1.IsLeader(), e2.IsLeader())
	}
}

// --------------------------------------------------------------------------
// Test: follower acquires leadership after the leader's context is cancelled
// --------------------------------------------------------------------------

func TestFollowerAcquiresAfterLeaderReleases(t *testing.T) {
	t.Parallel()
	shared := &fakeMutex{}
	const retry = 10 * time.Millisecond

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	followerCtx, followerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer followerCancel()

	leader := makeElector(shared, retry)
	follower := makeElector(shared, retry)

	go leader.Run(leaderCtx)

	// Wait for leader to acquire.
	waitFor(t, 500*time.Millisecond, func() bool { return leader.IsLeader() },
		"leader did not acquire the lock within timeout")

	// Start follower while leader still holds.
	go follower.Run(followerCtx)

	// Give follower time to attempt (and fail) acquisition.
	time.Sleep(50 * time.Millisecond)
	if follower.IsLeader() {
		t.Fatal("follower should not be leader while leader still holds the lock")
	}

	// Cancel the leader → triggers Release then Close.
	leaderCancel()

	// Wait for follower to take over.
	waitFor(t, 500*time.Millisecond, func() bool { return follower.IsLeader() },
		"follower did not acquire leadership after leader released")
}

// --------------------------------------------------------------------------
// Test: gated loop does not execute its body when the elector is not leader
// --------------------------------------------------------------------------

func TestGatedLoopDoesNotExecuteWhenNotLeader(t *testing.T) {
	t.Parallel()
	// Pre-lock the mutex so no elector can ever acquire.
	shared := &fakeMutex{held: true}
	elec := makeElector(shared, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	go elec.Run(ctx)

	// Let the elector spin for a few cycles without acquiring.
	time.Sleep(60 * time.Millisecond)

	if elec.IsLeader() {
		t.Fatal("expected elector to NOT be leader (lock is pre-held)")
	}

	// Simulate the gated-loop pattern used in main.go.
	executed := false
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(100 * time.Millisecond)
outer:
	for {
		select {
		case <-deadline:
			break outer
		case <-ticker.C:
			if !elec.IsLeader() {
				continue // cheap no-op when not leader
			}
			executed = true
		}
	}

	if executed {
		t.Error("gated loop body executed despite elector not being leader")
	}
}

// --------------------------------------------------------------------------
// Test: transitions counter increments correctly
// --------------------------------------------------------------------------

func TestTransitionsCounter(t *testing.T) {
	t.Parallel()
	shared := &fakeMutex{}
	const retry = 5 * time.Millisecond

	elec := makeElector(shared, retry)

	leaderCtx, leaderCancel := context.WithCancel(context.Background())

	if elec.Transitions() != 0 {
		t.Fatalf("expected 0 transitions before Run, got %d", elec.Transitions())
	}

	go elec.Run(leaderCtx)

	// Acquire.
	waitFor(t, 500*time.Millisecond, func() bool { return elec.IsLeader() },
		"did not acquire leadership")

	if elec.Transitions() != 1 {
		t.Errorf("expected 1 transition after acquire, got %d", elec.Transitions())
	}

	// Release.
	leaderCancel()
	waitFor(t, 500*time.Millisecond, func() bool { return !elec.IsLeader() },
		"did not lose leadership after cancel")

	if elec.Transitions() != 2 {
		t.Errorf("expected 2 transitions after release, got %d", elec.Transitions())
	}
}

// --------------------------------------------------------------------------
// Test: Changes channel receives true on acquire and false on release
// --------------------------------------------------------------------------

func TestChangesChannel(t *testing.T) {
	t.Parallel()
	shared := &fakeMutex{}
	elec := makeElector(shared, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	go elec.Run(ctx)

	select {
	case v := <-elec.Changes():
		if !v {
			t.Error("first change should be true (acquired)")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for leadership acquire event")
	}

	cancel()

	select {
	case v := <-elec.Changes():
		if v {
			t.Error("second change should be false (released)")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for leadership release event")
	}
}

// --------------------------------------------------------------------------
// Test: Pusher serializes metrics correctly
// --------------------------------------------------------------------------

func TestPusherSerializesMetrics(t *testing.T) {
	t.Parallel()
	var received string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pusher := &Pusher{
		BaseURL: srv.URL,
		Job:     "test_job",
		Pod:     "worker-0",
		HTTP:    srv.Client(),
	}

	// Fake an elector that is the leader with 3 transitions.
	elec := &Elector{changeCh: make(chan bool, 1)}
	elec.isLeader.Store(true)
	elec.transitions.Store(3)
	elec.logger = slog.Default()

	if err := pusher.Push(context.Background(), elec); err != nil {
		t.Fatalf("Push returned error: %v", err)
	}

	if !strings.Contains(received, `crucible_worker_is_leader{pod="worker-0"} 1`) {
		t.Errorf("missing is_leader=1 in pushed body:\n%s", received)
	}
	if !strings.Contains(received, `crucible_worker_leader_transitions_total{pod="worker-0"} 3`) {
		t.Errorf("missing transitions=3 in pushed body:\n%s", received)
	}
}

// --------------------------------------------------------------------------
// Test: Pusher is a no-op when BaseURL is empty
// --------------------------------------------------------------------------

func TestPusherNoopWhenNoURL(t *testing.T) {
	t.Parallel()
	pusher := &Pusher{} // BaseURL empty
	elec := &Elector{changeCh: make(chan bool, 1), logger: slog.Default()}
	if err := pusher.Push(context.Background(), elec); err != nil {
		t.Fatalf("expected no error from empty-URL pusher, got: %v", err)
	}
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// waitFor polls cond every 10 ms until it returns true or timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}
