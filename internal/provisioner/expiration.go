package provisioner

import (
	"context"
	"encoding/json"
	"time"
)

// ExpireStale queues pod_destroy jobs for any pods whose expiry has passed.
// It is the per-tick body of StartExpirationCron, exported so callers that
// manage the tick schedule themselves (e.g. for leader-election gating) can
// invoke a single reconcile pass without spinning up the internal goroutine.
func (p *Provisioner) ExpireStale(ctx context.Context) {
	p.expireStale(ctx)
}

// StartExpirationCron starts a background goroutine that checks for expired pods
// every 5 minutes and queues pod_destroy jobs for them.
func (p *Provisioner) StartExpirationCron(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	p.logger.Info("expiration cron started", "interval", "5m")

	// Run immediately on startup
	p.expireStale(ctx)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("expiration cron stopped")
			return
		case <-ticker.C:
			p.expireStale(ctx)
		}
	}
}

func (p *Provisioner) expireStale(ctx context.Context) {
	ids, err := p.db.ListExpiredPods(ctx)
	if err != nil {
		p.logger.Error("list expired pods failed", "error", err)
		return
	}

	if len(ids) == 0 {
		return
	}

	p.logger.Info("found expired pods", "count", len(ids))

	for _, podID := range ids {
		payload, _ := json.Marshal(map[string]string{
			"pod_id": podID.String(),
			"reason": "expired",
		})

		job, err := p.db.CreateJob(ctx, "pod_destroy", payload)
		if err != nil {
			p.logger.Error("failed to queue expiration destroy", "pod_id", podID, "error", err)
			continue
		}

		if err := p.nats.PublishJobCreated(job.ID, job.Type); err != nil {
			p.logger.Warn("failed to publish expiration job event", "error", err)
		}

		p.logger.Info("queued expiration destroy",
			"pod_id", podID,
			"job_id", job.ID,
		)
	}
}
