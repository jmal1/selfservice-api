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
	Client        *Client
	Pushgateway   *Pushgateway
	CheckTimeout  time.Duration
	Logger        *slog.Logger
	Checks        []Check
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
	}
}

// Register appends a Check to the runner's list. Useful for tests that want
// to add a fake check after construction.
func (r *Runner) Register(c Check) {
	r.Checks = append(r.Checks, c)
}

// RunOnce executes every registered Check once and pushes the result batch.
// Returns the slice of results so callers (and tests) can inspect them
// directly; the push error is logged but not returned, because a Pushgateway
// outage MUST NOT cause the synthetic runner itself to be marked failed in
// k8s — that would mask the actual checks from the on-call view.
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

// runOne executes a single Check with the configured timeout. It catches any
// panic so a buggy check cannot take down the whole run.
func (r *Runner) runOne(ctx context.Context, check Check) (res Result) {
	res = Result{
		Name:     check.Name(),
		Severity: check.Severity(),
	}
	checkCtx, cancel := context.WithTimeout(ctx, r.CheckTimeout)
	defer cancel()

	start := time.Now()
	defer func() {
		res.Duration = time.Since(start)
		// Recover from panics inside Run so a single buggy check cannot
		// crash the runner. We mark the result failed and log the panic.
		if p := recover(); p != nil {
			res.Success = false
			res.Err = panicError{value: p}
			r.Logger.Error("check panicked",
				"check", check.Name(),
				"panic", p,
			)
		}
		r.Logger.Info("check completed",
			"check", res.Name,
			"success", res.Success,
			"http_status", res.HTTPStatus,
			"duration_ms", res.Duration.Milliseconds(),
		)
	}()

	status, err := check.Run(checkCtx, r.Client)
	res.HTTPStatus = status
	res.Err = err
	res.Success = (err == nil)
	return res
}

// panicError adapts a recover() value into the error interface.
type panicError struct{ value any }

func (p panicError) Error() string {
	return "synthetic check panicked"
}
