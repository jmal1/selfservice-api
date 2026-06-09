package checks

import (
	"context"
	"log/slog"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// JanitorConfig controls the standalone janitor check. The janitor is
// independent of the pod_lifecycle check's built-in pre-clean step — it runs
// from its own CronJob (daily, typically) so orphan synthetic pods don't
// accumulate even if pod_lifecycle is broken, paused, or stuck destroying.
type JanitorConfig struct {
	// MaxAge is the cutoff for considering a synthetic pod an orphan.
	// 1 hour is conservative: the lifecycle check itself completes in
	// ~3-5 minutes, so anything older than an hour is almost certainly
	// abandoned. Operators can lower this when investigating accumulation.
	MaxAge time.Duration

	// Logger receives the per-pod audit lines emitted by preCleanOrphans.
	Logger *slog.Logger
}

// DefaultJanitorConfig returns production-tuned defaults.
func DefaultJanitorConfig() JanitorConfig {
	return JanitorConfig{
		MaxAge: 1 * time.Hour,
	}
}

// Janitor returns a Check that destroys orphan synthetic pods.
//
// Design note: this is intentionally implemented as a synthetic.Check (not
// a one-off binary) so it shares plumbing with the rest of the monitor —
// session auth, Pushgateway pushes, structured logging, severity routing.
// The success/failure metric for `synthetic_janitor` lets us alert when
// the daily sweep itself starts erroring (e.g. stale token, API down)
// instead of finding out only when the lifecycle check trips because
// orphan accumulation has consumed the per-user pod quota.
//
// The check returns success even when zero pods are cleaned — "nothing to
// do" is the desired steady state. It only reports failure when an actual
// destroy call errors, when listing pods errors, or when the API is
// unreachable.
func Janitor(cfg JanitorConfig) synthetic.Check {
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 1 * time.Hour
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return synthetic.CheckFunc{
		NameVal:        "synthetic_janitor",
		TitleVal:       "Orphan synthetic pod sweep",
		DescriptionVal: "Lists synthetic-noop-* pods owned by the synthetic user and destroys any older than the configured maxAge. Runs from a standalone CronJob (daily) so orphan accumulation is bounded even if pod_lifecycle is broken or paused.",
		SeverityVal:    synthetic.SeverityWarning,
		RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
			return preCleanOrphans(ctx, c, cfg.MaxAge, log.With("component", "synthetic_janitor"))
		},
	}
}
