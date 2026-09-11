// trust_validation_reconcile.go — template credential revalidation reconciler.
//
// Background: active clone_with_customize templates must be periodically
// smoke-clone verified even after they are published. Without this loop a
// working template at initial publish could silently break (e.g. the golden
// image gets corrupted, a dependency becomes unavailable) and students would
// clone a broken VM without any automated signal.
//
// A short scheduler poll runs this reconciler; persisted last_validated_at state
// and WORKER_L1_VALIDATION_INTERVAL (default weekly) decide what is due. Each
// pass:
//
//  1. Queries ALL active clone_with_customize templates and emits the
//     crucible_template_last_validated_timestamp gauge so the staleness alert
//     has fresh data every pass, not just when a job runs.
//
//  2. Identifies clone_with_customize templates whose last_validated_at is NULL
//     or older than the configured interval and enqueues a template_revalidate
//     job for each.
//
// On validation failure (handled by RevalidateL1Template in template_jobs.go)
// the template STAYS published; only the metric and last_validation_result
// are updated (alert-only policy — see the USER DECISION note in the spec).
package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

// L1TrustValidationReconcilerConfig controls one reconciler pass.
type L1TrustValidationReconcilerConfig struct {
	// Interval is how long a template may go without validation before the
	// reconciler enqueues a new revalidation job. Zero falls back to 168h
	// (one week). Configurable via WORKER_L1_VALIDATION_INTERVAL.
	Interval time.Duration

	// IsLeader is checked before querying and before every enqueue. Nil is
	// accepted for direct calls and tests. The worker supplies its elector so a
	// reconciliation that outlives leadership cannot continue filling the queue.
	IsLeader func() bool
}

// L1TrustValidationCounts summarises one pass for logs and tests.
type L1TrustValidationCounts struct {
	L1Templates int // total active clone_with_customize templates seen
	Due         int // templates whose persisted validation timestamp is overdue
	Enqueued    int // new template_revalidate jobs created this pass
}

// ErrL1ValidationLeadershipLost reports that a pass stopped because this worker
// no longer holds the reconciler advisory lock.
var ErrL1ValidationLeadershipLost = errors.New("l1 validation scheduler lost leadership")

// l1TrustValidationDB is the narrow DB surface the reconciler needs.
type l1TrustValidationDB interface {
	ListAllActiveCredentialRevalidationTemplates(ctx context.Context) ([]models.Template, error)
	ListStaleCredentialRevalidationTemplates(ctx context.Context, olderThan time.Duration) ([]models.Template, error)
	CreateTemplateRevalidateJobIfAbsent(ctx context.Context, templateID uuid.UUID, payload []byte) (*models.Job, bool, error)
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
	checkLeadership := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.IsLeader != nil && !cfg.IsLeader() {
			return ErrL1ValidationLeadershipLost
		}
		return nil
	}
	if err := checkLeadership(); err != nil {
		return L1TrustValidationCounts{}, err
	}

	var reconcileErr error

	// Step 1: query ALL active clone_with_customize templates so we can update
	// the staleness gauge for every one of them, even those recently validated.
	// Without this the gauge would only refresh on stale templates and a
	// recently-validated template would have a stale gauge reading.
	all, err := db.ListAllActiveCredentialRevalidationTemplates(ctx)
	if err != nil {
		return L1TrustValidationCounts{}, fmt.Errorf("list all credential revalidation templates: %w", err)
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
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("push l1 validation staleness metrics: %w", err))
		}
	}

	counts := L1TrustValidationCounts{L1Templates: len(all)}

	// Step 2: find templates that are stale and enqueue revalidation jobs.
	stale, err := db.ListStaleCredentialRevalidationTemplates(ctx, cfg.Interval)
	if err != nil {
		return counts, errors.Join(reconcileErr, fmt.Errorf("list stale credential revalidation templates: %w", err))
	}
	counts.Due = len(stale)

	for _, tmpl := range stale {
		if err := checkLeadership(); err != nil {
			return counts, errors.Join(reconcileErr, err)
		}
		if tmpl.VCenterVMID == "" && tmpl.VCenterTemplate == "" {
			err := fmt.Errorf("template %s (%s) has no vcenter_vm_id or vcenter_template", tmpl.ID, tmpl.Name)
			log.Error("cannot enqueue template revalidation", "error", err)
			reconcileErr = errors.Join(reconcileErr, err)
			continue
		}
		payload, merr := json.Marshal(TemplateRevalidatePayload{
			TemplateID: tmpl.ID,
			VMMoref:    tmpl.VCenterVMID,
		})
		if merr != nil {
			log.Error("failed to marshal revalidate payload",
				"template_id", tmpl.ID, "error", merr)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("marshal revalidate payload for %s: %w", tmpl.ID, merr))
			continue
		}
		_, created, err := db.CreateTemplateRevalidateJobIfAbsent(ctx, tmpl.ID, payload)
		if err != nil {
			log.Error("failed to enqueue revalidate job",
				"template_id", tmpl.ID, "name", tmpl.Name, "error", err)
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("enqueue revalidate job for %s: %w", tmpl.ID, err))
			continue
		}
		if !created {
			log.Info("active template revalidation job already exists",
				"template_id", tmpl.ID, "name", tmpl.Name)
			continue
		}
		counts.Enqueued++
		log.Info("enqueued template revalidation job",
			"template_id", tmpl.ID, "name", tmpl.Name,
			"last_validated_at", tmpl.LastValidatedAt)
	}

	log.Info("l1 trust validation reconcile complete",
		"l1_templates", counts.L1Templates,
		"due", counts.Due,
		"enqueued", counts.Enqueued,
		"interval", cfg.Interval,
		"error", reconcileErr,
	)
	return counts, reconcileErr
}
