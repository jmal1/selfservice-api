package provisioner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOrphanSchedulerStartupAlreadyLeaderRunsImmediatelyOnce(t *testing.T) {
	leadership := newTestOrphanLeadership()
	leadership.acquire()
	var calls atomic.Int32
	scheduler := NewOrphanReconcilerScheduler(
		leadership.current,
		func(context.Context) (ReconcileCounts, error) {
			calls.Add(1)
			return ReconcileCounts{}, nil
		},
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.LeadershipChanged(context.Background())
	scheduler.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("startup reconciliations = %d, want 1 without waiting for a leadership event", got)
	}
}

func TestOrphanSchedulerLeadershipAcquisitionRunsImmediately(t *testing.T) {
	leadership := newTestOrphanLeadership()
	var calls atomic.Int32
	scheduler := NewOrphanReconcilerScheduler(
		leadership.current,
		func(context.Context) (ReconcileCounts, error) {
			calls.Add(1)
			return ReconcileCounts{}, nil
		},
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("follower startup reconciliations = %d, want 0", got)
	}

	leadership.acquire()
	scheduler.LeadershipChanged(context.Background())
	scheduler.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("acquisition reconciliations = %d, want 1", got)
	}
}

func TestOrphanSchedulerFollowerDoesNotRun(t *testing.T) {
	leadership := newTestOrphanLeadership()
	var calls atomic.Int32
	scheduler := NewOrphanReconcilerScheduler(
		leadership.current,
		func(context.Context) (ReconcileCounts, error) {
			calls.Add(1)
			return ReconcileCounts{}, nil
		},
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Tick(context.Background())
	scheduler.LeadershipChanged(context.Background())
	scheduler.Wait()

	if got := calls.Load(); got != 0 {
		t.Fatalf("follower reconciliations = %d, want 0", got)
	}
}

func TestOrphanSchedulerPeriodicTickStillRuns(t *testing.T) {
	leadership := newTestOrphanLeadership()
	leadership.acquire()
	var calls atomic.Int32
	scheduler := NewOrphanReconcilerScheduler(
		leadership.current,
		func(context.Context) (ReconcileCounts, error) {
			calls.Add(1)
			return ReconcileCounts{}, nil
		},
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	scheduler.Tick(context.Background())
	scheduler.Wait()

	if got := calls.Load(); got != 2 {
		t.Fatalf("reconciliations after startup and periodic tick = %d, want 2", got)
	}
}

func TestOrphanSchedulerLeadershipLossCancelsWithoutOverlap(t *testing.T) {
	leadership := newTestOrphanLeadership()
	leadership.acquire()
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var closeFirst sync.Once
	var closeSecond sync.Once

	scheduler := NewOrphanReconcilerScheduler(
		leadership.current,
		func(ctx context.Context) (ReconcileCounts, error) {
			call := calls.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for {
				max := maxActive.Load()
				if current <= max || maxActive.CompareAndSwap(max, current) {
					break
				}
			}
			if call == 1 {
				closeFirst.Do(func() { close(firstStarted) })
				<-ctx.Done()
				close(firstCanceled)
				<-releaseFirst
				return ReconcileCounts{}, ctx.Err()
			}
			closeSecond.Do(func() { close(secondStarted) })
			return ReconcileCounts{}, nil
		},
		discardLogger(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	scheduler.Start(ctx)
	awaitSignal(t, ctx, firstStarted, "startup reconciliation")

	// Simulate Changes coalescing a loss and reacquisition into one wake-up.
	// The old lease must still cancel directly, and the new generation must
	// still queue exactly one replacement pass.
	leadership.lose()
	awaitSignal(t, ctx, firstCanceled, "leadership-loss cancellation")

	leadership.acquire()
	scheduler.LeadershipChanged(ctx)
	select {
	case <-secondStarted:
		t.Fatal("replacement reconciliation overlapped the canceled pass")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseFirst)
	awaitSignal(t, ctx, secondStarted, "replacement reconciliation")
	scheduler.Wait()

	if got := calls.Load(); got != 2 {
		t.Fatalf("reconciliations across leadership loss = %d, want 2", got)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum concurrent reconciliations = %d, want 1", got)
	}
}

type testOrphanLeadership struct {
	mu          sync.Mutex
	state       OrphanLeadershipState
	cancelLease context.CancelFunc
}

func newTestOrphanLeadership() *testOrphanLeadership {
	return &testOrphanLeadership{}
}

func (l *testOrphanLeadership) acquire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	leaseCtx, cancel := context.WithCancel(context.Background())
	l.state.Generation++
	l.state.IsLeader = true
	l.state.Context = leaseCtx
	l.cancelLease = cancel
}

func (l *testOrphanLeadership) lose() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state.IsLeader = false
	l.state.Context = nil
	if l.cancelLease != nil {
		l.cancelLease()
		l.cancelLease = nil
	}
}

func (l *testOrphanLeadership) current() OrphanLeadershipState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func awaitSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("%s did not occur: %v", name, ctx.Err())
	}
}
