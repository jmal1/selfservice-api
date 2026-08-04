// trust_validation_reconcile.go — L1 trust-tier revalidation reconciler.
//
// Background: templates with trust_tier='l1' must be periodically smoke-clone
// verified even after they are published. Without this loop a working template
// at initial publish could silently break (e.g. the golden image gets
// corrupted, a dependency becomes unavailable) and students would clone a
// broken VM without any automated signal.
//
// The reconciler runs on a configurable ticker (default weekly via
// WORKER_L1_VALIDATION_INTERVAL). Each pass:
//
//  1. Queries ALL active L1 templates and emits the
//     crucible_template_last_validated_timestamp gauge so the staleness alert
//     has fresh data every pass, not just when a job runs.
//
//  2. Identifies L1 templates whose last_validated_at is NULL or older than
//     the configured interval and enqueues a template_revalidate job for each.
//
// On validation failure (handled by RevalidateL1Template in template_jobs.go)
// the template STAYS published; only the metric and last_validation_result
// are updated (alert-only policy — see the USER DECISION note in the spec).
package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

// L1TrustValidationReconcilerConfig controls one reconciler pass.
type L1TrustValidationReconcilerConfig struct {
	// Interval is how long a template may go without validation before the
	// reconciler enqueues a new revalidation job. Zero falls back to 168h
	// (one week). Configurable via WORKER_L1_VALIDATION_INTERVAL.
	Interval time.Duration
}

// L1TrustValidationCounts summarises one pass for logs and tests.
type L1TrustValidationCounts struct {
	L1Templates int // total active L1 templates seen
	Enqueued    int // new template_revalidate jobs created this pass
}

// l1TrustValidationDB is the narrow DB surface the reconciler needs.
type l1TrustValidationDB interface {
	ListAllActiveL1Templates(ctx context.Context) ([]models.Template, error)
	ListStaleL1Templates(ctx context.Context, olderThan time.Duration) ([]models.Template, error)
	CreateJob(ctx context.Context, jobType string, payload []byte) (*models.Job, error)
}

// l1TrustValidationMetrics is the narrow metrics surface the reconciler needs.
type l1TrustValidationMetrics interface {
	SetTemplateLastValidated(templateID string, unixSec float64)
	Push(ctx context.Context) error
}

// Compile-time satisfaction checks.
var _ l1TrustValidationDB = (*database.Queries)(nil)
var _ l1TrustValidationMetrics = (*PipelineMetrics)(nil)

// ReconcileL1TrustValidation is the Provisioner-bound entry point. Production
// callers (cmd/provision-worker) invoke this from their select loop.
func (p *Provisioner) ReconcileL1TrustValidation(ctx context.Context, cfg L1TrustValidationReconcilerConfig) (L1TrustValidationCounts, error) {
	var m l1TrustValidationMetrics
	if p.pipeline != nil {
		m = p.pipeline
	}
	return reconcileL1TrustValidation(ctx, p.db, m, p.logger, cfg)
}

// reconcileL1TrustValidation is the pure implementation, factored out so tests
// can inject fakes for the database and metrics without touching the
// Provisioner struct.
func reconcileL1TrustValidation(
	ctx context.Context,
	db l1TrustValidationDB,
	metrics l1TrustValidationMetrics,
	logger *slog.Logger,
	cfg L1TrustValidationReconcilerConfig,
) (L1TrustValidationCounts, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 168 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "l1_trust_validation_reconciler")

	// Step 1: query ALL active L1 templates so we can update the staleness
	// gauge for every one of them, even those recently validated. Without this
	// the gauge would only refresh on stale templates and a recently-validated
	// template would have a stale gauge reading.
	all, err := db.ListAllActiveL1Templates(ctx)
	if err != nil {
		return L1TrustValidationCounts{}, fmt.Errorf("list all l1 templates: %w", err)
	}

	// Emit the staleness gauge. Never-validated templates get timestamp 0 so
	// the alert rule `time() - crucible_template_last_validated_timestamp > threshold`
	// always fires for them (since time() >> 0 + threshold for any sane threshold).
	if metrics != nil {
		for _, tmpl := range all {
			ts := float64(0) // never validated
			if tmpl.LastValidatedAt != nil {
				ts = float64(tmpl.LastValidatedAt.Unix())
			}
			metrics.SetTemplateLastValidated(tmpl.ID.String(), ts)
		}
		if err := metrics.Push(ctx); err != nil {
			log.Warn("l1 validation staleness gauge push failed", "error", err)
		}
	}

	counts := L1TrustValidationCounts{L1Templates: len(all)}

	// Step 2: find templates that are stale and enqueue revalidation jobs.
	stale, err := db.ListStaleL1Templates(ctx, cfg.Interval)
	if err != nil {
		return counts, fmt.Errorf("list stale l1 templates: %w", err)
	}

	for _, tmpl := range stale {
		if tmpl.VCenterVMID == "" && tmpl.VCenterTemplate == "" {
			log.Warn("l1 template has no vcenter_vm_id and no vcenter_template; skipping revalidation",
				"template_id", tmpl.ID, "name", tmpl.Name)
			continue
		}
		payload, merr := json.Marshal(TemplateRevalidatePayload{
			TemplateID: tmpl.ID,
			VMMoref:    tmpl.VCenterVMID,
		})
		if merr != nil {
			log.Error("failed to marshal revalidate payload",
				"template_id", tmpl.ID, "error", merr)
			continue
		}
		if _, err := db.CreateJob(ctx, models.JobTypeTemplateRevalidate, payload); err != nil {
			log.Error("failed to enqueue revalidate job",
				"template_id", tmpl.ID, "name", tmpl.Name, "error", err)
			continue
		}
		counts.Enqueued++
		log.Info("enqueued l1 revalidation job",
			"template_id", tmpl.ID, "name", tmpl.Name,
			"last_validated_at", tmpl.LastValidatedAt)
	}

	log.Info("l1 trust validation reconcile complete",
		"l1_templates", counts.L1Templates,
		"enqueued", counts.Enqueued,
		"interval", cfg.Interval,
	)
	return counts, nil
}
