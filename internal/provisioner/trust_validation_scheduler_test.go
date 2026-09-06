package provisioner

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestL1SchedulerStartupAlreadyLeaderDoesNotNeedEvent(t *testing.T) {
	var calls atomic.Int32
	scheduler := NewL1TrustValidationScheduler(
		func() bool { return true },
		func(context.Context) (L1TrustValidationCounts, error) {
			calls.Add(1)
			return L1TrustValidationCounts{Due: 2, Enqueued: 2}, nil
		},
		nil,
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("startup reconciliations = %d, want 1 without any leadership event", got)
	}
}

func TestL1SchedulerRunsImmediatelyOnLeadershipAcquisition(t *testing.T) {
	var leader atomic.Bool
	var calls atomic.Int32
	scheduler := NewL1TrustValidationScheduler(
		leader.Load,
		func(context.Context) (L1TrustValidationCounts, error) {
			calls.Add(1)
			return L1TrustValidationCounts{}, nil
		},
		nil,
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	if got := calls.Load(); got != 0 {
		t.Fatalf("follower startup reconciliations = %d, want 0", got)
	}

	leader.Store(true)
	scheduler.LeadershipChanged(context.Background(), true)
	scheduler.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("acquisition reconciliations = %d, want 1", got)
	}
}

func TestL1SchedulerPeriodicPollRecoversMissedFailoverEvent(t *testing.T) {
	var leader atomic.Bool
	var calls atomic.Int32
	scheduler := NewL1TrustValidationScheduler(
		leader.Load,
		func(context.Context) (L1TrustValidationCounts, error) {
			calls.Add(1)
			return L1TrustValidationCounts{}, nil
		},
		nil,
		discardLogger(),
	)

	scheduler.Start(context.Background())
	leader.Store(true)
	// Deliberately omit LeadershipChanged(true), simulating a dropped event.
	scheduler.Tick(context.Background())
	scheduler.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("periodic recovery reconciliations = %d, want 1", got)
	}
}

func TestL1SchedulerLeaderFailoverCancelsAndReruns(t *testing.T) {
	var leader atomic.Bool
	leader.Store(true)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	var once sync.Once

	scheduler := NewL1TrustValidationScheduler(
		leader.Load,
		func(ctx context.Context) (L1TrustValidationCounts, error) {
			call := calls.Add(1)
			if call == 1 {
				once.Do(func() { close(firstStarted) })
				<-ctx.Done()
				return L1TrustValidationCounts{}, ctx.Err()
			}
			return L1TrustValidationCounts{Due: 1, Enqueued: 1}, nil
		},
		nil,
		discardLogger(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	scheduler.Start(ctx)
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("first leader reconciliation did not start")
	}

	leader.Store(false)
	scheduler.LeadershipChanged(ctx, false)
	leader.Store(true)
	scheduler.LeadershipChanged(ctx, true)
	scheduler.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("reconciliations across failover = %d, want 2", got)
	}
}

type fakeL1SchedulerMetrics struct {
	mu       sync.Mutex
	observed []error
	counts   []L1TrustValidationCounts
	pushes   int
}

func (m *fakeL1SchedulerMetrics) ObserveRun(counts L1TrustValidationCounts, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts = append(m.counts, counts)
	m.observed = append(m.observed, err)
}

func (m *fakeL1SchedulerMetrics) Push(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushes++
	return nil
}

func TestL1SchedulerRecordsEnqueueError(t *testing.T) {
	metrics := &fakeL1SchedulerMetrics{}
	wantErr := errors.New("enqueue failed")
	scheduler := NewL1TrustValidationScheduler(
		func() bool { return true },
		func(context.Context) (L1TrustValidationCounts, error) {
			return L1TrustValidationCounts{Due: 1}, wantErr
		},
		metrics,
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	if len(metrics.observed) != 1 || !errors.Is(metrics.observed[0], wantErr) {
		t.Fatalf("observed errors = %v, want enqueue failure", metrics.observed)
	}
	if metrics.pushes != 1 {
		t.Fatalf("metrics pushes = %d, want 1", metrics.pushes)
	}
}

func TestL1SchedulerSuccessfulCatchUpRecordsCounts(t *testing.T) {
	metrics := &fakeL1SchedulerMetrics{}
	scheduler := NewL1TrustValidationScheduler(
		func() bool { return true },
		func(context.Context) (L1TrustValidationCounts, error) {
			return L1TrustValidationCounts{L1Templates: 2, Due: 2, Enqueued: 2}, nil
		},
		metrics,
		discardLogger(),
	)

	scheduler.Start(context.Background())
	scheduler.Wait()
	if len(metrics.counts) != 1 {
		t.Fatalf("observed runs = %d, want 1", len(metrics.counts))
	}
	if got := metrics.counts[0]; got.Due != 2 || got.Enqueued != 2 {
		t.Fatalf("observed counts = %+v, want due=2 enqueued=2", got)
	}
	if metrics.observed[0] != nil {
		t.Fatalf("successful catch-up error = %v", metrics.observed[0])
	}
}

func TestL1SchedulerSkipsMetricsWhenNotOwned(t *testing.T) {
	metrics := &fakeL1SchedulerMetrics{}
	scheduler := NewL1TrustValidationScheduler(
		func() bool { return true },
		func(context.Context) (L1TrustValidationCounts, error) {
			return L1TrustValidationCounts{Due: 9}, ErrReconcileNotOwned
		},
		metrics,
		discardLogger(),
	)
	scheduler.Start(context.Background())
	scheduler.Wait()
	if metrics.pushes != 0 || len(metrics.observed) != 0 {
		t.Fatalf("not-owned reconcile must not ObserveRun/Push; pushes=%d observed=%d", metrics.pushes, len(metrics.observed))
	}
}

func TestL1SchedulerWaitTimeoutIsBounded(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	scheduler := NewL1TrustValidationScheduler(
		func() bool { return true },
		func(context.Context) (L1TrustValidationCounts, error) {
			close(started)
			<-release
			return L1TrustValidationCounts{}, nil
		},
		nil,
		discardLogger(),
	)
	scheduler.Start(context.Background())
	<-started

	start := time.Now()
	if scheduler.WaitTimeout(20 * time.Millisecond) {
		t.Fatal("scheduler wait reported completion while reconcile remained blocked")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded scheduler wait took %s", elapsed)
	}

	close(release)
	if !scheduler.WaitTimeout(time.Second) {
		t.Fatal("scheduler did not finish after reconcile was released")
	}
}
