package worklease_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/worklease"
)

type memStore struct {
	mu         sync.Mutex
	byName     map[string]*row
	failRenew  bool
	claimDelay time.Duration
}

type row struct {
	worker    string
	token     uuid.UUID
	claimedAt time.Time
	gen       int64
}

func newMemStore() *memStore {
	return &memStore{byName: map[string]*row{}}
}

func (m *memStore) TryClaimWorkLease(_ context.Context, name, workerID string, lease time.Duration) (uuid.UUID, int64, bool, error) {
	if m.claimDelay > 0 {
		time.Sleep(m.claimDelay)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byName[name]
	now := time.Now()
	if r != nil && !r.claimedAt.IsZero() && now.Sub(r.claimedAt) < lease {
		return uuid.Nil, 0, false, nil
	}
	tok := uuid.New()
	gen := int64(1)
	if r != nil {
		gen = r.gen + 1
	}
	m.byName[name] = &row{worker: workerID, token: tok, claimedAt: now, gen: gen}
	return tok, gen, true, nil
}

func (m *memStore) RenewWorkLease(_ context.Context, name, workerID string, token uuid.UUID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failRenew {
		return false, errors.New("renew blocked")
	}
	r := m.byName[name]
	if r == nil || r.worker != workerID || r.token != token {
		return false, nil
	}
	r.claimedAt = time.Now()
	return true, nil
}

func (m *memStore) ReleaseWorkLease(_ context.Context, name, workerID string, token uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byName[name]
	if r == nil || r.worker != workerID || r.token != token {
		return nil
	}
	r.worker = ""
	r.token = uuid.Nil
	r.claimedAt = time.Time{}
	return nil
}

func TestAllNamesStable(t *testing.T) {
	names := worklease.AllNames()
	if len(names) < 10 {
		t.Fatalf("expected seeded lease names, got %d", len(names))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Fatalf("duplicate lease name %q", n)
		}
		seen[n] = true
	}
}

func TestRunExclusiveSerializes(t *testing.T) {
	store := newMemStore()
	var ranA, ranB bool
	errA := worklease.RunExclusive(context.Background(), store, worklease.NetworkReconcile, "a", nil,
		func(context.Context, *worklease.Lease) error {
			ranA = true
			return nil
		})
	if errA != nil {
		t.Fatal(errA)
	}
	// Hold an unexpired lease as B tries to claim.
	tok, _, ok, err := store.TryClaimWorkLease(context.Background(), worklease.NetworkReconcile, "holder", worklease.Duration)
	if err != nil || !ok {
		t.Fatalf("setup claim: ok=%v err=%v", ok, err)
	}
	_ = tok
	errB := worklease.RunExclusive(context.Background(), store, worklease.NetworkReconcile, "b", nil,
		func(context.Context, *worklease.Lease) error {
			ranB = true
			return nil
		})
	if !errors.Is(errB, worklease.ErrNotClaimed) {
		t.Fatalf("errB=%v want ErrNotClaimed", errB)
	}
	if !ranA || ranB {
		t.Fatalf("ranA=%v ranB=%v", ranA, ranB)
	}
}

func TestRenewLossCancelsLeaseContext(t *testing.T) {
	restore := worklease.SetHeartbeatIntervalForTest(5 * time.Millisecond)
	defer restore()

	store := newMemStore()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	started := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- worklease.RunExclusive(ctx, store, worklease.IdleEval, "a", nil,
			func(leaseCtx context.Context, lease *worklease.Lease) error {
				close(started)
				store.mu.Lock()
				r := store.byName[worklease.IdleEval]
				r.worker = "b"
				r.token = uuid.New()
				store.mu.Unlock()
				select {
				case <-leaseCtx.Done():
					return context.Cause(leaseCtx)
				case <-time.After(2 * time.Second):
					return errors.New("lease context was not cancelled")
				}
			})
	}()
	<-started
	err := <-errCh
	if !errors.Is(err, worklease.ErrLeaseLost) {
		t.Fatalf("err=%v want ErrLeaseLost", err)
	}
}


func TestTryRunIgnoresNotClaimed(t *testing.T) {
	store := newMemStore()
	_, _, ok, _ := store.TryClaimWorkLease(context.Background(), worklease.ExpireStale, "holder", worklease.Duration)
	if !ok {
		t.Fatal("expected claim")
	}
	called := false
	if err := worklease.TryRun(context.Background(), store, worklease.ExpireStale, "other", nil,
		func(context.Context, *worklease.Lease) error {
			called = true
			return nil
		}); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("TryRun must not invoke fn when lease held")
	}
}
