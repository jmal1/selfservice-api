// Package synthetic implements the customer-emulating monitor that exercises
// the live Crucible API every few minutes from a real user's perspective. Each
// Check is a single user-flow assertion (e.g. "list pods returns 200") that
// reports pass/fail, duration, and HTTP status to Prometheus via Pushgateway.
//
// The synthetic monitor authenticates by minting its own session JWT using the
// API's shared HMAC secret, so it does not require Authentik or the OIDC
// authorization-code flow. The OIDC flow itself is exercised by the external
// Playwright suite (see future/Synthetic-Monitoring.md).
package synthetic

import (
	"context"
	"time"
)

// Severity classifies a check failure for alert routing.
type Severity string

const (
	// SeverityCritical indicates a failure that should page on-call.
	SeverityCritical Severity = "critical"
	// SeverityWarning indicates a degradation that should warn but not page.
	SeverityWarning Severity = "warning"
)

// Result describes the outcome of a single Check execution.
//
// Success is the canonical pass/fail indicator. HTTPStatus is best-effort: it
// reports the last HTTP response status observed by the Check (0 if the check
// failed before any HTTP call completed). Err carries the failure reason for
// log diagnostics; it MUST NOT contain credentials.
type Result struct {
	Name       string
	Success    bool
	Duration   time.Duration
	HTTPStatus int
	Severity   Severity
	Err        error
}

// Check is the contract every synthetic check must satisfy.
//
// Name() is used as a metric label and MUST be a Prometheus-safe identifier
// (lowercase letters, digits, underscores) and stable across releases so that
// alerts referring to a check do not silently break when implementation
// changes. Severity() controls alert routing.
//
// Run() executes the check against the supplied Client. Implementations MUST
// respect ctx cancellation and MUST NOT panic on transport errors — return a
// non-nil error instead. The runner enforces an outer timeout, so Run() does
// not need to spawn its own goroutines for timeout enforcement.
type Check interface {
	Name() string
	Severity() Severity
	Run(ctx context.Context, client *Client) (httpStatus int, err error)
}

// CheckFunc adapts a function literal to the Check interface for inline
// definitions in checks/registry.go.
type CheckFunc struct {
	NameVal     string
	SeverityVal Severity
	RunFn       func(ctx context.Context, client *Client) (int, error)
}

// Name returns the check name.
func (c CheckFunc) Name() string { return c.NameVal }

// Severity returns the check severity classification.
func (c CheckFunc) Severity() Severity { return c.SeverityVal }

// Run invokes the bound function.
func (c CheckFunc) Run(ctx context.Context, client *Client) (int, error) {
	return c.RunFn(ctx, client)
}
