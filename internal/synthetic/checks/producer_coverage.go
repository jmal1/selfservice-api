package checks

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// CoverageConfig controls a producer-coverage meta-check.
type CoverageConfig struct {
	Enabled bool
}

func producerCoverageCheck(name, title, description, envName string, cfg CoverageConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:        name,
		TitleVal:       title,
		DescriptionVal: description,
		RunbookVal:     "https://github.com/jmal1/selfservice-api/blob/main/docs/instructor/troubleshooting.md#synthetic-producer-coverage-alerts",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(context.Context, *synthetic.Client) (int, error) {
			if cfg.Enabled {
				return http.StatusOK, nil
			}
			return http.StatusServiceUnavailable, fmt.Errorf("%s=false; %s is gated off and its freshness series will be absent until the producer is re-enabled", envName, name)
		},
	}
}

// PodLifecycleEnabled reports whether the required mutating pod_lifecycle
// producer is actually enabled in the API monitor.
func PodLifecycleEnabled(cfg CoverageConfig) synthetic.Check {
	return producerCoverageCheck(
		"pod_lifecycle_enabled",
		"Pod Lifecycle Coverage Enabled",
		"Reports whether the API monitor is configured to run pod_lifecycle. A false result means the required mutating producer is gated off and its series will be absent.",
		"SYNTHETIC_LIFECYCLE_ENABLED",
		cfg,
	)
}

// RunnerSmokeEnabled reports whether the runner_smoke producer is expected to
// be scheduled by Helm.
func RunnerSmokeEnabled(cfg CoverageConfig) synthetic.Check {
	return producerCoverageCheck(
		"runner_smoke_enabled",
		"Runner Smoke Coverage Enabled",
		"Reports whether Helm expects the dedicated runner_smoke CronJob to be scheduled. A false result means the runner producer is intentionally absent and freshness alerts should fire.",
		"SYNTHETIC_RUNNER_EXPECTED_ENABLED",
		cfg,
	)
}
