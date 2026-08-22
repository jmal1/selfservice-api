package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// vCenterPortGroupAdvisoryLockKey is a production protocol constant. Changing
// it would allow old and new workers to mutate standard switches concurrently.
const vCenterPortGroupAdvisoryLockKey int64 = 0x4352554349424c45

const vCenterPortGroupUnlockTimeout = 10 * time.Second

// WithVCenterPortGroupMutationLock serializes standard-vSwitch mutations across
// worker processes. It deliberately uses a session advisory lock without a
// transaction so slow vCenter calls never hold a PostgreSQL transaction open.
func (q *Queries) WithVCenterPortGroupMutationLock(
	ctx context.Context,
	mutate func(context.Context) error,
) (retErr error) {
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for vCenter port group lock: %w", err)
	}
	release := true
	defer func() {
		if release {
			conn.Release()
		}
	}()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", vCenterPortGroupAdvisoryLockKey); err != nil {
		// The server may have acquired the session lock before the client saw
		// cancellation. Never put an ambiguously locked session back in the pool.
		closeHijackedConnection(conn)
		release = false
		return fmt.Errorf("acquire vCenter port group advisory lock: %w", err)
	}

	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vCenterPortGroupUnlockTimeout)
		defer cancel()
		var unlocked bool
		unlockErr := conn.QueryRow(
			unlockCtx,
			"SELECT pg_advisory_unlock($1)",
			vCenterPortGroupAdvisoryLockKey,
		).Scan(&unlocked)
		if unlockErr != nil || !unlocked {
			closeHijackedConnection(conn)
			release = false
			if unlockErr == nil {
				unlockErr = errors.New("PostgreSQL reported that the advisory lock was not held")
			}
			retErr = errors.Join(retErr, fmt.Errorf("release vCenter port group advisory lock: %w", unlockErr))
		}
	}()

	return mutate(ctx)
}

func closeHijackedConnection(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	_ = raw.Close(context.Background())
}
