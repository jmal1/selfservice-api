package provisioner

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jmal1/selfservice-api/internal/database"
)

// ExpireStale queues pod_destroy jobs for any pods whose expiry has passed.
// Called on each tick of expirationCronTicker in the provision-worker select
// loop and immediately on leader acquisition via elec.Changes().
func (p *Provisioner) ExpireStale(ctx context.Context) {
	p.expireStale(ctx)
}

func (p *Provisioner) expireStale(ctx context.Context) {
	ids, err := p.db.ListExpiredPods(ctx)
	if err != nil {
		p.logger.Error("list expired pods failed", "error", err)
		return
	}

	if len(ids) > 0 {
		p.logger.Info("found expired pods", "count", len(ids))
	}

	for _, podID := range ids {
		payload, _ := json.Marshal(map[string]string{
			"pod_id": podID.String(),
			"reason": "expired",
		})

		job, created, err := p.db.CreateExpiredPodDestroyJob(ctx, podID, payload)
		if err != nil {
			if errors.Is(err, database.ErrPodJobRejected) ||
				errors.Is(err, database.ErrPodDestroyNotNeeded) ||
				errors.Is(err, database.ErrPodDestroyBlockedByMutator) {
				p.logger.Info("expiration destroy no longer needed", "pod_id", podID, "error", err)
				continue
			}
			p.logger.Error("failed to queue expiration destroy", "pod_id", podID, "error", err)
			continue
		}

		if created && p.nats != nil {
			if err := p.nats.PublishJobCreated(job.ID, job.Type); err != nil {
				p.logger.Warn("failed to publish expiration job event", "error", err)
			}
		}

		p.logger.Info("ensured expiration destroy",
			"pod_id", podID,
			"job_id", job.ID,
			"created", created,
		)
	}

	suspendedIDs, err := p.db.ListSuspendedPodsPastRetention(ctx)
	if err != nil {
		p.logger.Error("list suspended-too-long pods failed", "error", err)
		return
	}
	for _, podID := range suspendedIDs {
		payload, _ := json.Marshal(map[string]string{
			"pod_id": podID.String(),
			"reason": "suspended_too_long",
		})
		job, created, err := p.db.CreateSuspendedPodDestroyJob(ctx, podID, payload)
		if err != nil {
			if errors.Is(err, database.ErrPodJobRejected) ||
				errors.Is(err, database.ErrPodDestroyNotNeeded) ||
				errors.Is(err, database.ErrPodDestroyBlockedByMutator) {
				continue
			}
			p.logger.Error("failed to queue suspended-too-long destroy", "pod_id", podID, "error", err)
			continue
		}
		if created && p.nats != nil {
			if err := p.nats.PublishJobCreated(job.ID, job.Type); err != nil {
				p.logger.Warn("failed to publish suspended-too-long job event", "error", err)
			}
		}
	}
}
