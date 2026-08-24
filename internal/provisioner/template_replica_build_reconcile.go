package provisioner

import (
	"context"
	"fmt"
	"time"
)

type templateReplicaBuildMetricsDB interface {
	CountTemplateReplicaBuildsByPhase(context.Context, time.Duration) (map[string]int, int, error)
}

type templateReplicaBuildMetricsSink interface {
	SetTemplateReplicaBuildPhases(map[string]int)
	SetTemplateReplicaBuildsStuck(int)
	Push(context.Context) error
}

func reconcileTemplateReplicaBuildMetrics(
	ctx context.Context,
	db templateReplicaBuildMetricsDB,
	metrics templateReplicaBuildMetricsSink,
	staleAfter time.Duration,
) error {
	if staleAfter <= 0 {
		staleAfter = 30 * time.Minute
	}
	phases, stuck, err := db.CountTemplateReplicaBuildsByPhase(ctx, staleAfter)
	if err != nil {
		return fmt.Errorf("count template replica builds: %w", err)
	}
	if metrics == nil {
		return nil
	}
	metrics.SetTemplateReplicaBuildPhases(phases)
	metrics.SetTemplateReplicaBuildsStuck(stuck)
	if err := metrics.Push(ctx); err != nil {
		return fmt.Errorf("push template replica build metrics: %w", err)
	}
	return nil
}

func (p *Provisioner) ReconcileTemplateReplicaBuildMetrics(
	ctx context.Context,
	staleAfter time.Duration,
) error {
	var metrics templateReplicaBuildMetricsSink
	if pipeline, ok := p.pipeline.(*PipelineMetrics); ok {
		metrics = pipeline
	}
	return reconcileTemplateReplicaBuildMetrics(ctx, p.db, metrics, staleAfter)
}
