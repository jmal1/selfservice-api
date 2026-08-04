// retry_reconcile.go -- keeps crucible_job_retry_pending accurate.
//
// The gauge counts jobs currently sleeping between retry attempts
// (status='pending', next_attempt_at > now()).  Without this reconciler the
// gauge is never set and serialize() omits it entirely (see jobRetryPendingCollected
// in pipeline_metrics.go), which is honest but means the metric never fires
// an alert.  With it, Prometheus sees a real value every 30 s by default.
//
// Design mirrors image_reconcile.go exactly: narrow interfaces, a pure
// reconcileRetryPending function that tests can call without a real DB or
// Pushgateway, and a RunRetryPendingReconciler loop that the worker starts.
package provisioner

import (
	"context"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
)

// retryPendingDB is the narrow DB surface the reconciler needs.
type retryPendingDB interface {
	CountRetryPendingJobs(ctx context.Context) (int, error)
}

// retryPendingMetrics is the narrow metrics surface the reconciler needs.
type retryPendingMetrics interface {
	SetJobRetryPending(n int)
	Push(ctx context.Context) error
}

// Compile-time satisfaction checks: ensure the real implementations remain
// compatible with the narrow interfaces without importing them in tests.
var _ retryPendingDB = (*database.Queries)(nil)
var _ retryPendingMetrics = (*PipelineMetrics)(nil)

// reconcileRetryPending is the pure reconcile function: counts pending-retry
// jobs and pushes the gauge.  Returns an error only if the DB query fails;
// push errors are logged but not returned so the caller keeps running.
func reconcileRetryPending(ctx context.Context, db retryPendingDB, m retryPendingMetrics, logger *slog.Logger) error {
	n, err := db.CountRetryPendingJobs(ctx)
	if err != nil {
		return err
	}
	m.SetJobRetryPending(n)
	if pushErr := m.Push(ctx); pushErr != nil {
		if logger != nil {
			logger.Warn("retry-pending metrics push failed", "error", pushErr)
		}
	}
	return nil
}

// ReconcileRetryPending is the method the worker calls from its select loop.
// It uses p.db and p.pipeline; it is a no-op when either is nil.
func (p *Provisioner) ReconcileRetryPending(ctx context.Context) error {
	if p.db == nil || p.pipeline == nil {
		return nil
	}
	return reconcileRetryPending(ctx, p.db, p.pipeline, p.logger)
}

// RunRetryPendingReconciler runs ReconcileRetryPending on a ticker until ctx
// is cancelled.  It is started as a goroutine by the worker when a
// Pushgateway URL is configured.
func (p *Provisioner) RunRetryPendingReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.ReconcileRetryPending(ctx); err != nil {
				if p.logger != nil {
					p.logger.Warn("retry-pending reconcile failed", "error", err)
				}
			}
		}
	}
}
