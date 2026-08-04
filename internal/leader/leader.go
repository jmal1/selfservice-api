// Package leader implements Postgres advisory-lock-based leader election for
// the provision-worker. It is designed for a fleet of identical replicas where
// periodic reconciler loops (network, orphan, IP, etc.) must run on exactly
// one replica at a time.
//
// # Safety guarantees
//
// The lock is session-scoped (pg_try_advisory_lock, NOT pg_try_advisory_xact_lock).
// This is essential: the lock survives across ticks and releases automatically
// when the connection is closed — so a crashed leader cannot wedge the fleet.
// Followers poll on a configurable retry interval and take over automatically.
//
// # Shutdown
//
// Cancel the context passed to Run to trigger a graceful shutdown. The current
// leader explicitly calls pg_advisory_unlock before closing its connection,
// so a follower can take over immediately rather than waiting for the Postgres
// session-timeout to expire.
package leader

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// LockBackend abstracts the Postgres advisory lock for testability. One backend
// instance corresponds to one connection lifetime. Create a fresh instance via
// BackendFactory for each leadership attempt.
type LockBackend interface {
	// TryAcquire attempts a non-blocking session-scoped advisory lock.
	// Returns true if the lock was acquired by this call.
	TryAcquire(ctx context.Context) (bool, error)

	// Keepalive verifies that the underlying connection is still live.
	// A non-nil error means the connection has been lost and leadership
	// should be relinquished.
	Keepalive(ctx context.Context) error

	// Release explicitly releases the advisory lock. Called on graceful
	// shutdown so a follower can take over immediately.
	Release(ctx context.Context) error

	// Close closes the underlying connection.
	Close(ctx context.Context) error
}

// BackendFactory creates a fresh LockBackend for each leadership attempt.
// Implementations must be safe to call concurrently.
type BackendFactory func(ctx context.Context) (LockBackend, error)

// Elector manages leader election. Create via New, NewPostgresElector, or
// NewAlwaysLeader; then call Run in a goroutine. It is safe to call IsLeader
// and Transitions concurrently.
type Elector struct {
	newBackend    BackendFactory // nil means AlwaysLeader mode
	retryInterval time.Duration
	logger        *slog.Logger

	isLeader    atomic.Bool
	transitions atomic.Int64
	changeCh    chan bool
}

// New creates an Elector. newBackend is called each time a new connection is
// needed — on start-up and after a connection loss. Pass nil for newBackend to
// create an always-leader instance (see NewAlwaysLeader).
func New(newBackend BackendFactory, retryInterval time.Duration, logger *slog.Logger) *Elector {
	if logger == nil {
		logger = slog.Default()
	}
	return &Elector{
		newBackend:    newBackend,
		retryInterval: retryInterval,
		logger:        logger,
		changeCh:      make(chan bool, 1),
	}
}

// NewAlwaysLeader returns an Elector that permanently reports itself as leader.
// Run blocks until ctx is cancelled and never modifies the leader state.
// Use when leader election is disabled (WORKER_LEADER_ELECTION_ENABLED=false).
func NewAlwaysLeader(logger *slog.Logger) *Elector {
	if logger == nil {
		logger = slog.Default()
	}
	e := &Elector{
		newBackend:    nil, // sentinel: AlwaysLeader mode
		retryInterval: time.Hour,
		logger:        logger,
		changeCh:      make(chan bool, 1),
	}
	e.isLeader.Store(true)
	e.transitions.Store(1)
	return e
}

// IsLeader reports whether this instance currently holds the advisory lock.
// Safe to call from any goroutine.
func (e *Elector) IsLeader() bool { return e.isLeader.Load() }

// Transitions returns the total number of leadership state changes (acquire +
// release events) observed by this instance. Used as the metric value for
// crucible_worker_leader_transitions_total.
func (e *Elector) Transitions() int64 { return e.transitions.Load() }

// Changes returns a channel that receives true when leadership is acquired and
// false when it is lost. The channel is buffered; if the receiver is slow a
// notification may be dropped — callers must tolerate gaps and poll IsLeader.
func (e *Elector) Changes() <-chan bool { return e.changeCh }

// Run is the election loop. It blocks until ctx is cancelled.
//
// When created via NewAlwaysLeader, Run simply blocks until ctx is cancelled
// without modifying any state.
//
// Typical usage:
//
//	go elec.Run(ctx)
func (e *Elector) Run(ctx context.Context) {
	// AlwaysLeader mode: no-op until shutdown.
	if e.newBackend == nil {
		<-ctx.Done()
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}
		e.attemptLeadership(ctx)

		// Wait before the next attempt (follower backoff / reconnect delay).
		select {
		case <-ctx.Done():
			return
		case <-time.After(e.retryInterval):
		}
	}
}

// attemptLeadership opens a backend, tries to acquire the advisory lock, and if
// successful holds it by blocking in a keepalive loop until ctx is done or the
// connection drops. Returns when the lock is released or was never acquired.
func (e *Elector) attemptLeadership(ctx context.Context) {
	backend, err := e.newBackend(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.logger.Warn("leader election: failed to open backend", "error", err)
		return
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = backend.Release(releaseCtx)
		_ = backend.Close(releaseCtx)
	}()

	acquired, err := backend.TryAcquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.logger.Warn("leader election: acquire failed", "error", err)
		return
	}
	if !acquired {
		// We are a follower; return immediately so the caller waits retryInterval.
		return
	}

	e.becomeLeader()
	defer e.loseLeadership()

	// Hold the lock until the context is cancelled or keepalive detects a drop.
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := backend.Keepalive(ctx); err != nil {
				e.logger.Warn("leader election: keepalive failed, relinquishing",
					"error", err)
				return
			}
		}
	}
}

func (e *Elector) becomeLeader() {
	if !e.isLeader.Swap(true) {
		e.transitions.Add(1)
		select {
		case e.changeCh <- true:
		default:
		}
		e.logger.Info("leader election: acquired leadership")
	}
}

func (e *Elector) loseLeadership() {
	if e.isLeader.Swap(false) {
		e.transitions.Add(1)
		select {
		case e.changeCh <- false:
		default:
		}
		e.logger.Info("leader election: lost leadership")
	}
}
