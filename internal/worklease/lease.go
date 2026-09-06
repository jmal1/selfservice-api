package worklease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotClaimed means another replica holds a fresh lease.
	ErrNotClaimed = errors.New("work lease not claimed")
	// ErrLeaseLost means renew failed; the caller must stop before the next
	// irreversible external mutation and must not write terminal state.
	ErrLeaseLost = errors.New("work lease ownership lost")
)

const (
	// Duration is the liveness window while heartbeats succeed.
	Duration = 90 * time.Second
	// HeartbeatInterval is independent of blocking external calls.
	HeartbeatInterval = 15 * time.Second
	renewWriteTimeout = 5 * time.Second
)

// Store is the Postgres-backed lease API.
type Store interface {
	TryClaimWorkLease(ctx context.Context, name, workerID string, leaseDuration time.Duration) (claimToken uuid.UUID, generation int64, ok bool, err error)
	RenewWorkLease(ctx context.Context, name, workerID string, claimToken uuid.UUID) (bool, error)
	ReleaseWorkLease(ctx context.Context, name, workerID string, claimToken uuid.UUID) error
}

// Lease is a held claim for one mutating run.
type Lease struct {
	Name       string
	WorkerID   string
	ClaimToken uuid.UUID
	Generation int64
	Context    context.Context
	cancel     context.CancelCauseFunc
}

// Cancel stops the lease context (e.g. on renew loss).
func (l *Lease) Cancel(cause error) {
	if l != nil && l.cancel != nil {
		l.cancel(cause)
	}
}

// RunExclusive claims name for the whole mutating run, renews on an independent
// goroutine, and releases on exit. If the lease cannot be claimed, fn is not
// called and ErrNotClaimed is returned. On renew loss, lease.Context is
// cancelled with ErrLeaseLost; fn must stop before the next irreversible step.
func RunExclusive(
	ctx context.Context,
	store Store,
	name, workerID string,
	logger *slog.Logger,
	fn func(leaseCtx context.Context, lease *Lease) error,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	token, generation, ok, err := store.TryClaimWorkLease(ctx, name, workerID, Duration)
	if err != nil {
		return fmt.Errorf("claim %s: %w", name, err)
	}
	if !ok {
		return ErrNotClaimed
	}

	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &Lease{
		Name:       name,
		WorkerID:   workerID,
		ClaimToken: token,
		Generation: generation,
		Context:    leaseCtx,
		cancel:     cancel,
	}
	defer func() {
		cancel(context.Canceled)
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), renewWriteTimeout)
		defer releaseCancel()
		if err := store.ReleaseWorkLease(releaseCtx, name, workerID, token); err != nil && logger != nil {
			logger.Warn("work lease release failed", "name", name, "error", err)
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		maintain(leaseCtx, store, lease, cancel, logger)
	}()

	runErr := fn(leaseCtx, lease)
	cancel(errLeaseFinished)
	<-done
	if runErr != nil {
		return runErr
	}
	if cause := context.Cause(leaseCtx); cause != nil && !errors.Is(cause, errLeaseFinished) && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

var errLeaseFinished = errors.New("work lease run finished")

// heartbeatInterval is overridable in tests.
var heartbeatInterval = HeartbeatInterval

// SetHeartbeatIntervalForTest overrides renew cadence; restore after the test.
func SetHeartbeatIntervalForTest(d time.Duration) (restore func()) {
	prev := heartbeatInterval
	heartbeatInterval = d
	return func() { heartbeatInterval = prev }
}

func maintain(
	ctx context.Context,
	store Store,
	lease *Lease,
	cancel context.CancelCauseFunc,
	logger *slog.Logger,
) {
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			writeCtx, writeCancel := context.WithTimeout(ctx, renewWriteTimeout)
			owned, err := store.RenewWorkLease(writeCtx, lease.Name, lease.WorkerID, lease.ClaimToken)
			writeCancel()
			switch {
			case err == nil && owned:
				lastOK = time.Now()
			case err == nil:
				logger.Warn("work lease lost", "name", lease.Name, "token", lease.ClaimToken)
				cancel(fmt.Errorf("%w: %s", ErrLeaseLost, lease.Name))
				return
			case time.Since(lastOK) >= Duration:
				logger.Warn("work lease renew expired", "name", lease.Name, "error", err)
				cancel(fmt.Errorf("%w: %s renew expired: %v", ErrLeaseLost, lease.Name, err))
				return
			default:
				logger.Warn("work lease renew failed", "name", lease.Name, "error", err)
			}
		}
	}
}

// TryRun is RunExclusive that treats ErrNotClaimed as a no-op success (another
// replica holds the lease). Useful for periodic tickers.
func TryRun(
	ctx context.Context,
	store Store,
	name, workerID string,
	logger *slog.Logger,
	fn func(leaseCtx context.Context, lease *Lease) error,
) error {
	err := RunExclusive(ctx, store, name, workerID, logger, fn)
	if errors.Is(err, ErrNotClaimed) {
		return nil
	}
	return err
}
