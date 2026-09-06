package provisioner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

const (
	// JobLeaseDuration is the liveness window while heartbeats succeed — not a
	// maximum job runtime. Clone/API work may run far longer if renewals keep
	// claimed_at fresh. Sized at ~6 missed 15s intervals for DNS/PG blips.
	JobLeaseDuration          = 90 * time.Second
	JobLeaseHeartbeatInterval = 15 * time.Second
	JobLeaseRecoveryInterval  = 15 * time.Second
	jobLeaseWriteTimeout      = 5 * time.Second
)

var errJobLeaseFinished = errors.New("job execution finished")

type jobLeaseRenewer interface {
	RenewJobLease(ctx context.Context, id uuid.UUID, workerID string) (bool, error)
}

func claimedJobLease(job *models.Job) (string, time.Time, error) {
	if job == nil || job.ClaimedBy == nil || *job.ClaimedBy == "" {
		return "", time.Time{}, fmt.Errorf("%w: job has no claim owner", database.ErrJobLeaseLost)
	}
	if job.ClaimedAt == nil {
		return "", time.Time{}, fmt.Errorf("%w: job has no claim timestamp", database.ErrJobLeaseLost)
	}
	return *job.ClaimedBy, *job.ClaimedAt, nil
}

func maintainJobLease(
	ctx context.Context,
	renewer jobLeaseRenewer,
	jobID uuid.UUID,
	workerID string,
	_ time.Time,
	heartbeatInterval time.Duration,
	leaseDuration time.Duration,
	writeTimeout time.Duration,
	cancel context.CancelCauseFunc,
) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	lastRenewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeCtx, writeCancel := context.WithTimeout(ctx, writeTimeout)
			owned, err := renewer.RenewJobLease(writeCtx, jobID, workerID)
			writeCancel()
			switch {
			case err == nil && owned:
				lastRenewed = time.Now()
			case err == nil:
				cancel(fmt.Errorf("%w: job %s is no longer owned by %s",
					database.ErrJobLeaseLost, jobID, workerID))
				return
			case time.Since(lastRenewed) >= leaseDuration:
				cancel(fmt.Errorf("%w: job %s heartbeat expired after %s: %v",
					database.ErrJobLeaseLost, jobID, leaseDuration, err))
				return
			}
		}
	}
}
