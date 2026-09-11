package synthetic

import (
	"context"
	"log/slog"
	"time"
)

// Runner executes a sequence of Checks against the supplied Client, captures
// timings, and pushes the result batch to Pushgateway.
//
// Concurrency: checks run sequentially by default. The synthetic API surface
// is small and we want predictable load patterns; parallelism saves seconds
// at the cost of confusing flame graphs. Add a concurrency option only if
// a future check needs explicit serialization to detect race conditions.
type Runner struct {
	Client       *Client
	Pushgateway  *Pushgateway
	CheckTimeout time.Duration
	Logger       *slog.Logger
	Checks       []Check

	// RetryPolicy maps check name to its retry configuration. Checks not
	// present in this map receive a single-shot (MaxAttempts=1) execution.
	// Only apply retry to expensive, latency-sensitive checks — cheap
	// contract checks (403, 404, healthz) must remain single-shot so that
	// a real regression is not masked by a spurious passing retry.
	RetryPolicy map[string]RetryConfig
}

// NewRunner builds a Runner with sane defaults. Pass an empty checks list and
// append via Register if you want runtime control.
func NewRunner(client *Client, pushgateway *Pushgateway, checks []Check, logger *slog.Logger) *Runner {
	return &Runner{
		Client:       client,
		Pushgateway:  pushgateway,
		CheckTimeout: 30 * time.Second,
		Logger:       logger,
		Checks:       checks,
		RetryPolicy:  make(map[string]RetryConfig),
	}
}

// Register appends a Check to the runner's list. Useful for tests that want
// to add a fake check after construction.
func (r *Runner) Register(c Check) {
	r.Checks = append(r.Checks, c)
}

// SetRetry registers a retry policy for the named check. Calling this with
// MaxAttempts=1 is equivalent to single-shot (the default).
func (r *Runner) SetRetry(checkName string, cfg RetryConfig) {
	if r.RetryPolicy == nil {
		r.RetryPolicy = make(map[string]RetryConfig)
	}
	r.RetryPolicy[checkName] = cfg
}

// RunOnce executes every registered Check once (with retry if configured) and
// pushes the result batch. Returns the slice of results so callers (and tests)
// can inspect them directly; the push error is logged but not returned, because
// a Pushgateway outage MUST NOT cause the synthetic runner itself to be marked
// failed in k8s — that would mask the actual checks from the on-call view.
func (r *Runner) RunOnce(ctx context.Context) []Result {
	results := make([]Result, 0, len(r.Checks))
	for _, check := range r.Checks {
		results = append(results, r.runOne(ctx, check))
	}

	if r.Pushgateway != nil {
		pushCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := r.Pushgateway.PushResults(pushCtx, results); err != nil {
			r.Logger.Error("pushgateway push failed", "error", err)
		}
	}
	return results
}

// runOne executes a single Check, honouring any RetryPolicy entry for its
// name. It catches panics so a buggy check cannot take down the whole run.
func (r *Runner) runOne(ctx context.Context, check Check) (res Result) {
	res = Result{
		Name:        check.Name(),
		Title:       check.Title(),
		Description: check.Description(),
		Runbook:     check.Runbook(),
		Severity:    check.Severity(),
		Attempts:    0,
	}

	policy := r.retryConfigFor(check.Name())
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	backoff := policy.Backoff
	// backoff==0 is valid: it means "retry immediately" (useful in tests and
	// for checks where even a small delay is undesirable). No fallback.

	start := time.Now()
	defer func() {
		res.Duration = time.Since(start)
	}()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		res.Attempts = attempt
		res.Success = false
		res.Err = nil
		res.HTTPStatus = 0

		attemptStart := time.Now()
		status, err, panicked := r.execOnce(ctx, check)
		attemptDuration := time.Since(attemptStart)

		res.HTTPStatus = status
		res.Err = err
		res.Success = (err == nil && !panicked)

		if !res.Success && IsVCenterDegradedError(err) {
			// Classify vCenter degradation on the first failure, even
			// if a later attempt succeeds — a stall is still a signal.
			res.VCenterDegraded = true
		}

		r.logAttempt(check.Name(), attempt, maxAttempts, status, err, panicked, attemptDuration)

		if res.Success {
			return res
		}

		if attempt < maxAttempts {
			r.Logger.Warn("check failed; will retry",
				"check", check.Name(),
				"attempt", attempt,
				"max_attempts", maxAttempts,
				"backoff", backoff,
				"vcenter_degraded", res.VCenterDegraded,
			)
			select {
			case <-ctx.Done():
				res.Err = ctx.Err()
				res.Success = false
				return res
			case <-time.After(backoff):
			}
		}
	}
	return res
}

// execOnce runs a single check attempt with the configured timeout and recovers
// from panics. Returns (httpStatus, err, panicked).
func (r *Runner) execOnce(ctx context.Context, check Check) (status int, err error, panicked bool) {
	checkCtx, cancel := context.WithTimeout(ctx, r.CheckTimeout)
	defer cancel()

	defer func() {
		if p := recover(); p != nil {
			panicked = true
			err = panicError{value: p}
			r.Logger.Error("check panicked",
				"check", check.Name(),
				"panic", p,
			)
		}
	}()

	status, err = check.Run(checkCtx, r.Client)
	return status, err, false
}

// logAttempt emits a structured log line for a single attempt outcome.
// On failure we ERROR-log with the full error message so an operator paging
// on this can see the reason without cross-referencing other services. A
// 30-day log retention means an alert that fires now must be diagnosable
// from logs alone — silent failures are the worst kind.
func (r *Runner) logAttempt(name string, attempt, max, httpStatus int, err error, panicked bool, d time.Duration) {
	success := err == nil && !panicked
	if success {
		r.Logger.Info("check completed",
			"check", name,
			"success", true,
			"attempt", attempt,
			"max_attempts", max,
			"http_status", httpStatus,
			"duration_ms", d.Milliseconds(),
		)
	} else {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		r.Logger.Error("check failed",
			"check", name,
			"success", false,
			"attempt", attempt,
			"max_attempts", max,
			"http_status", httpStatus,
			"duration_ms", d.Milliseconds(),
			"error", errMsg,
		)
	}
}

// retryConfigFor returns the RetryConfig for the named check, or a
// single-shot config if none is registered.
func (r *Runner) retryConfigFor(name string) RetryConfig {
	if r.RetryPolicy != nil {
		if cfg, ok := r.RetryPolicy[name]; ok {
			return cfg
		}
	}
	return RetryConfig{MaxAttempts: 1}
}

// panicError adapts a recover() value into the error interface.
type panicError struct{ value any }

func (p panicError) Error() string {
	return "synthetic check panicked"
}
