// Package provisioner — stuck image-upload reconciler.
//
// Background: image_uploads rows can sit in "uploading" or "importing"
// status forever if the browser tab dies mid-upload or the import worker
// crashes. Nothing in the normal job path notices because those rows have
// no associated job to transition to a failed state. Each stuck row
// occupies space on a MinIO host that has only ~85 GB free on its root
// filesystem — the same filesystem the apt package cache uses when building
// Linux templates. A silent leak here eventually breaks unrelated builds.
//
// This reconciler runs on a configurable ticker in the provision-worker.
// Each pass queries the count of rows in a non-terminal status older than a
// staleness threshold and publishes that count via the existing
// crucible_image_uploads_stuck gauge in PipelineMetrics. When the count
// drops to zero the gauge is still pushed so a resolved problem clears the
// alert. When the query fails the gauge is left untouched so a transient
// DB blip never looks like "problem resolved".
package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
)

// imageUploadsDB is the narrow database interface the stuck-upload reconciler
// needs. *database.Queries satisfies it structurally via CountStuckImageUploads.
type imageUploadsDB interface {
	CountStuckImageUploads(ctx context.Context, olderThan time.Duration) (int, error)
}

// imageUploadsMetrics is the narrow metric interface used to publish the
// stuck count. *PipelineMetrics satisfies it via SetImageUploadsStuck and Push.
//
// Push is part of this interface deliberately. SetImageUploadsStuck only
// mutates in-process state; nothing leaves the worker until Push serializes
// the registry to the Pushgateway. Setting without pushing produces a gauge
// that never appears in Prometheus at all, which on a dashboard is
// indistinguishable from "there are no stuck uploads" — the exact false
// all-clear this reconciler exists to prevent.
type imageUploadsMetrics interface {
	SetImageUploadsStuck(n int)
	Push(ctx context.Context) error
}

// Compile-time satisfaction checks: ensure the real implementations remain
// compatible with the narrow interfaces without importing them in tests.
var _ imageUploadsDB = (*database.Queries)(nil)
var _ imageUploadsMetrics = (*PipelineMetrics)(nil)

// ReconcileStuckImageUploads runs a single pass: counts image_uploads rows
// stuck in a non-terminal state older than staleThreshold and publishes the
// result via the crucible_image_uploads_stuck gauge. When p.pipeline is nil
// the gauge update is skipped but the count is still returned (safe in
// environments without observability wired up).
func (p *Provisioner) ReconcileStuckImageUploads(ctx context.Context, staleThreshold time.Duration) (int, error) {
	var m imageUploadsMetrics
	if p.pipeline != nil {
		m = p.pipeline
	}
	return reconcileStuckUploads(ctx, p.db, m, staleThreshold, p.logger)
}

// reconcileStuckUploads is the pure implementation, factored out so tests can
// inject fakes for the database and the metrics sink without touching real
// infrastructure.
//
// On query success the count — including zero — is pushed immediately so that
// a resolved problem clears the alert rather than leaving the gauge stale at
// its last non-zero value. On query error the gauge is not touched: a DB blip
// must not look like "all clear".
func reconcileStuckUploads(
	ctx context.Context,
	db imageUploadsDB,
	metrics imageUploadsMetrics,
	staleThreshold time.Duration,
	logger *slog.Logger,
) (int, error) {
	if staleThreshold <= 0 {
		staleThreshold = 30 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "stuck_upload_reconciler")

	n, err := db.CountStuckImageUploads(ctx, staleThreshold)
	if err != nil {
		return 0, fmt.Errorf("count stuck image uploads: %w", err)
	}

	if metrics != nil {
		metrics.SetImageUploadsStuck(n)
		// Matches the push-and-warn idiom used by the orphan, IP and network
		// reconcilers: a Pushgateway outage must not fail the reconcile pass,
		// but it must be visible in the worker log rather than swallowed.
		if err := metrics.Push(ctx); err != nil {
			log.Warn("stuck-upload metric push failed", "error", err, "stuck_count", n)
		}
	}

	log.Info("stuck-upload reconcile complete",
		"stuck_count", n, "stale_threshold", staleThreshold)
	return n, nil
}
