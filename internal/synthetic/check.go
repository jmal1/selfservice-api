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
//
// Attempts is the total number of attempts the runner made before recording
// this result. 1 means the check succeeded or failed on the first try with no
// retry applied. Values >1 mean the check passed or exhausted retries on a
// later attempt. Checks without a retry policy always have Attempts=1.
//
// VCenterDegraded is set when at least one attempt failed with an error
// pattern that indicates vCenter slowness or unreachability (as opposed to a
// Crucible defect). A check may be green (Success=true) AND have
// VCenterDegraded=true when it passed on retry after an initial vCenter stall.
// This is intentional: the stall is a meaningful signal even when the check
// eventually passes.
type Result struct {
	Name           string
	Title          string
	Description    string
	Success        bool
	Duration       time.Duration
	HTTPStatus     int
	Severity       Severity
	Err            error
	Attempts       int
	VCenterDegraded bool
}

// Check is the contract every synthetic check must satisfy.
//
// Name() is used as a metric label and MUST be a Prometheus-safe identifier
// (lowercase letters, digits, underscores) and stable across releases so that
// alerts referring to a check do not silently break when implementation
// changes. Severity() controls alert routing.
//
// Title() is a short human-readable name shown in dashboards and alert
// summaries (e.g. "API Liveness"). Description() is the long-form one-line
// explanation that appears in dashboard tooltips and the status table.
// Neither field is allowed to contain commas, double-quotes, or newlines —
// they are emitted directly into Prometheus exposition labels.
//
// Run() executes the check against the supplied Client. Implementations MUST
// respect ctx cancellation and MUST NOT panic on transport errors — return a
// non-nil error instead. The runner enforces an outer timeout, so Run() does
// not need to spawn its own goroutines for timeout enforcement.
type Check interface {
	Name() string
	Title() string
	Description() string
	Severity() Severity
	Run(ctx context.Context, client *Client) (httpStatus int, err error)
}

// CheckFunc adapts a function literal to the Check interface for inline
// definitions in checks/registry.go.
type CheckFunc struct {
	NameVal        string
	TitleVal       string
	DescriptionVal string
	SeverityVal    Severity
	RunFn          func(ctx context.Context, client *Client) (int, error)
}

// Name returns the check name.
func (c CheckFunc) Name() string { return c.NameVal }

// Title returns the human-readable title.
func (c CheckFunc) Title() string { return c.TitleVal }

// Description returns the long-form description.
func (c CheckFunc) Description() string { return c.DescriptionVal }

// Severity returns the check severity classification.
func (c CheckFunc) Severity() Severity { return c.SeverityVal }

// Run invokes the bound function.
func (c CheckFunc) Run(ctx context.Context, client *Client) (int, error) {
	return c.RunFn(ctx, client)
}
