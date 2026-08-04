package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
)

// TemplateReconcilerConfig controls one template metrics reconciliation pass.
type TemplateReconcilerConfig struct {
	Interval       time.Duration
	StaleThreshold time.Duration
}

// TemplateReconcileCounts summarizes one pass for logs and tests.
type TemplateReconcileCounts struct {
	Templates int
	Stuck     int
}

type templateReconcileDB interface {
	CountTemplateStates(ctx context.Context) (map[string]int, error)
	CountStuckTemplates(ctx context.Context, olderThan time.Duration) (int, error)
}

type templateReconcileMetrics interface {
	SetTemplateStates(counts map[string]int)
	SetTemplatesStuck(n int)
	Push(ctx context.Context) error
}

var _ templateReconcileDB = (*database.Queries)(nil)
var _ templateReconcileMetrics = (*PipelineMetrics)(nil)

// ReconcileTemplateMetrics performs one count + publish pass.
func (p *Provisioner) ReconcileTemplateMetrics(ctx context.Context, cfg TemplateReconcilerConfig) (TemplateReconcileCounts, error) {
	var m templateReconcileMetrics
	if p.pipeline != nil {
		m = p.pipeline
	}
	return reconcileTemplateMetrics(ctx, p.db, m, p.logger, cfg)
}

func reconcileTemplateMetrics(
	ctx context.Context,
	db templateReconcileDB,
	metrics templateReconcileMetrics,
	logger *slog.Logger,
	cfg TemplateReconcilerConfig,
) (TemplateReconcileCounts, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.StaleThreshold <= 0 {
		cfg.StaleThreshold = 30 * time.Minute
	}
	log := logger.With("component", "template_reconciler")

	countsByState, err := db.CountTemplateStates(ctx)
	if err != nil {
		return TemplateReconcileCounts{}, fmt.Errorf("count template states: %w", err)
	}
	stuck, err := db.CountStuckTemplates(ctx, cfg.StaleThreshold)
	if err != nil {
		return TemplateReconcileCounts{}, fmt.Errorf("count stuck templates: %w", err)
	}

	counts := TemplateReconcileCounts{Stuck: stuck}
	for _, n := range countsByState {
		counts.Templates += n
	}

	if metrics != nil {
		metrics.SetTemplateStates(countsByState)
		metrics.SetTemplatesStuck(stuck)
		if err := metrics.Push(ctx); err != nil {
			log.Warn("template metrics push failed", "error", err, "template_count", counts.Templates, "stuck_count", stuck)
		}
	}

	log.Info("template reconcile complete",
		"template_count", counts.Templates,
		"stuck_count", stuck,
		"stale_threshold", cfg.StaleThreshold,
	)
	return counts, nil
}
