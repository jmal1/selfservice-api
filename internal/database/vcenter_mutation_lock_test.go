package database

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestVCenterPortGroupAdvisoryLockKeyIsStable(t *testing.T) {
	const productionKey int64 = 0x4352554349424c45
	if vCenterPortGroupAdvisoryLockKey != productionKey {
		t.Fatalf(
			"vCenter portgroup advisory lock key changed from %x to %x; mixed worker versions would mutate concurrently",
			productionKey,
			vCenterPortGroupAdvisoryLockKey,
		)
	}
}

func TestVCenterPortGroupAdvisoryLockSerializesTwoClients(t *testing.T) {
	dsn := requirePostgresDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool1, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool1.Close()
	pool2, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	if err := pool1.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool2.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	clients := []*Queries{{pool: pool1}, {pool: pool2}}
	var active atomic.Int32
	var maximum atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(client *Queries) {
			defer wg.Done()
			errs <- client.WithVCenterPortGroupMutationLock(ctx, func(context.Context) error {
				now := active.Add(1)
				for {
					previous := maximum.Load()
					if now <= previous || maximum.CompareAndSwap(previous, now) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				active.Add(-1)
				return nil
			})
		}(clients[i%len(clients)])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum concurrent vCenter mutation sections = %d, want 1", got)
	}
}

func TestVCenterPortGroupAdvisoryLockCancellationAndErrorRelease(t *testing.T) {
	dsn := requirePostgresDSN(t)

	rootCtx, rootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rootCancel()
	pool1, err := pgxpool.New(rootCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool1.Close()
	pool2, err := pgxpool.New(rootCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool2.Close()
	first := &Queries{pool: pool1}
	second := &Queries{pool: pool2}

	t.Run("callback error", func(t *testing.T) {
		sentinel := errors.New("simulated vCenter mutation failure")
		if err := first.WithVCenterPortGroupMutationLock(rootCtx, func(context.Context) error {
			return sentinel
		}); !errors.Is(err, sentinel) {
			t.Fatalf("mutation error = %v, want sentinel", err)
		}
		if err := second.WithVCenterPortGroupMutationLock(rootCtx, func(context.Context) error {
			return nil
		}); err != nil {
			t.Fatalf("lock remained held after callback error: %v", err)
		}
	})

	t.Run("callback cancellation", func(t *testing.T) {
		mutationCtx, cancelMutation := context.WithCancel(rootCtx)
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- first.WithVCenterPortGroupMutationLock(mutationCtx, func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		select {
		case <-entered:
		case <-rootCtx.Done():
			t.Fatal(rootCtx.Err())
		}
		cancelMutation()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled mutation error = %v, want context.Canceled", err)
		}
		if err := second.WithVCenterPortGroupMutationLock(rootCtx, func(context.Context) error {
			return nil
		}); err != nil {
			t.Fatalf("lock remained held after cancellation: %v", err)
		}
	})
}
