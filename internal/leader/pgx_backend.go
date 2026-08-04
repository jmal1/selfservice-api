package leader

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkerLockKey is the session-scoped advisory lock key used by the provision-
// worker leader elector. Chosen as a memorable hex pattern derived from
// "CRUCIBLE" in ASCII to avoid accidental collision with other advisory locks.
const WorkerLockKey int64 = 0x4352554349424C57

// pgxBackend implements LockBackend using a single dedicated pgx/v5 connection.
//
// Session-scoped advisory locks (pg_try_advisory_lock, NOT pg_try_advisory_xact_lock)
// are used intentionally: the lock survives across individual SQL calls and is
// released automatically when the Postgres session closes — either via an
// explicit Close or due to a TCP reset. This is the key safety property: a
// crashed leader pod cannot wedge the fleet.
//
// The connection is kept separate from the application pool. Pool connections
// may be returned to the pool between calls, which would silently release a
// session-scoped lock. A dedicated connection is the only correct approach.
type pgxBackend struct {
	conn    *pgx.Conn
	lockKey int64
}

func newPgxBackend(ctx context.Context, dsn string, lockKey int64) (LockBackend, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("leader backend: open dedicated connection: %w", err)
	}
	return &pgxBackend{conn: conn, lockKey: lockKey}, nil
}

// TryAcquire calls pg_try_advisory_lock. Returns true if this call acquired the
// lock; false if another session already holds it.
func (b *pgxBackend) TryAcquire(ctx context.Context) (bool, error) {
	var acquired bool
	err := b.conn.QueryRow(ctx,
		"SELECT pg_try_advisory_lock($1)", b.lockKey,
	).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("leader backend: pg_try_advisory_lock: %w", err)
	}
	return acquired, nil
}

// Keepalive verifies the connection is still live by executing a trivial query.
func (b *pgxBackend) Keepalive(ctx context.Context) error {
	var dummy int
	if err := b.conn.QueryRow(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return fmt.Errorf("leader backend: keepalive: %w", err)
	}
	return nil
}

// Release calls pg_advisory_unlock. A graceful shutdown should always call this
// before Close so a follower can take over immediately rather than waiting for
// Postgres to detect the session drop.
func (b *pgxBackend) Release(ctx context.Context) error {
	var released bool
	err := b.conn.QueryRow(ctx,
		"SELECT pg_advisory_unlock($1)", b.lockKey,
	).Scan(&released)
	if err != nil {
		return fmt.Errorf("leader backend: pg_advisory_unlock: %w", err)
	}
	return nil
}

// Close closes the underlying Postgres connection.
func (b *pgxBackend) Close(ctx context.Context) error {
	return b.conn.Close(ctx)
}

// NewPostgresElector creates an Elector backed by a Postgres session-scoped
// advisory lock. Each leadership attempt opens a fresh dedicated connection via
// pgx.Connect — the same DSN format returned by config.DatabaseConfig.DSN()
// is accepted.
//
// retryInterval controls how long followers wait before re-attempting lock
// acquisition; 5 seconds is a reasonable default for production.
func NewPostgresElector(dsn string, lockKey int64, retryInterval time.Duration, logger *slog.Logger) *Elector {
	factory := func(ctx context.Context) (LockBackend, error) {
		return newPgxBackend(ctx, dsn, lockKey)
	}
	return New(factory, retryInterval, logger)
}
