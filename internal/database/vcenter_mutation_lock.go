package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// vCenterPortGroupAdvisoryLockKey is a production protocol constant. Changing
// it would allow old and new workers to mutate standard switches concurrently.
const vCenterPortGroupAdvisoryLockKey int64 = 0x4352554349424c45

const sessionAdvisoryUnlockTimeout = 10 * time.Second

// WithVCenterPortGroupMutationLock serializes standard-vSwitch mutations across
// worker processes. It deliberately uses a session advisory lock without a
// transaction so slow vCenter calls never hold a PostgreSQL transaction open.
func (q *Queries) WithVCenterPortGroupMutationLock(
	ctx context.Context,
	mutate func(context.Context) error,
) (retErr error) {
	return q.withSessionAdvisoryLock(
		ctx,
		"vCenter port group",
		"SELECT pg_advisory_lock($1)",
		"SELECT pg_advisory_unlock($1)",
		[]any{vCenterPortGroupAdvisoryLockKey},
		mutate,
	)
}

// WithTemplateReplicaBuildLifecycleLock prevents replica-build admission from
// racing the template deletion preflight and irreversible vCenter cleanup.
func (q *Queries) WithTemplateReplicaBuildLifecycleLock(
	ctx context.Context,
	templateID uuid.UUID,
	mutate func(context.Context) error,
) error {
	return q.withSessionAdvisoryLock(
		ctx,
		"template replica build lifecycle",
		"SELECT pg_advisory_lock(hashtextextended($1, 0))",
		"SELECT pg_advisory_unlock(hashtextextended($1, 0))",
		[]any{templateReplicaBuildLifecycleLockKey(templateID)},
		mutate,
	)
}

func templateReplicaBuildLifecycleLockKey(templateID uuid.UUID) string {
	return "crucible:template-replica-lifecycle:" + templateID.String()
}

func (q *Queries) withSessionAdvisoryLock(
	ctx context.Context,
	label, lockSQL, unlockSQL string,
	args []any,
	mutate func(context.Context) error,
) (retErr error) {
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for %s lock: %w", label, err)
	}
	release := true
	defer func() {
		if release {
			conn.Release()
		}
	}()

	if _, err := conn.Exec(ctx, lockSQL, args...); err != nil {
		// The server may have acquired the session lock before the client saw
		// cancellation. Never put an ambiguously locked session back in the pool.
		closeHijackedConnection(conn)
		release = false
		return fmt.Errorf("acquire %s advisory lock: %w", label, err)
	}

	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionAdvisoryUnlockTimeout)
		defer cancel()
		var unlocked bool
		unlockErr := conn.QueryRow(
			unlockCtx,
			unlockSQL,
			args...,
		).Scan(&unlocked)
		if unlockErr != nil || !unlocked {
			closeHijackedConnection(conn)
			release = false
			if unlockErr == nil {
				unlockErr = errors.New("PostgreSQL reported that the advisory lock was not held")
			}
			retErr = errors.Join(retErr, fmt.Errorf("release %s advisory lock: %w", label, unlockErr))
		}
	}()

	return mutate(ctx)
}

func closeHijackedConnection(conn *pgxpool.Conn) {
	raw := conn.Hijack()
	_ = raw.Close(context.Background())
}
