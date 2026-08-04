package synthetic

import (
	"context"
	"errors"
	"strings"
	"time"
)

// RetryConfig controls how many times the runner re-attempts a failing check
// before recording it as failed. It is designed for the expensive vCenter-
// dependent checks (pod_lifecycle, runner_smoke) where a single transient
// SDK stall should not produce a critical alert.
//
// The retry loop lives in the Runner (runner.go) rather than inside each
// Check.Run so that:
//
//   - The per-check timeout set by lifecycleSafeTimeout applies to each
//     individual attempt, not to the total retry budget.
//   - Attempt count and vCenter-degradation attribution are surfaced in Result
//     without requiring a non-standard Check interface.
//   - Cheap contract checks (403s, 404s, healthz) retain their single-shot
//     behaviour with zero code changes to those checks.
type RetryConfig struct {
	// MaxAttempts is the total number of times the check is allowed to run,
	// including the first attempt. 1 means no retry; 2 means one retry.
	// Values ≤0 are treated as 1.
	MaxAttempts int

	// Backoff is the pause between attempts. A running context expiring during
	// the backoff terminates the loop early without a further attempt.
	Backoff time.Duration
}

// DefaultRetryConfig returns production-safe retry defaults for the expensive
// vCenter-dependent checks.
//
// MaxAttempts=2 (one retry) combined with the 2-minute ReadyTimeout gives a
// worst-case pod_lifecycle cycle time of:
//
//	2 × (ReadyTimeout + DestroyTimeout) + Backoff + overhead
//	= 2 × (120s + 90s) + 30s + 60s
//	= 510s ≈ 8.5 minutes < 10-minute CronJob schedule
//
// For runner_smoke (30-minute CronJob), the same config with a 10-minute
// RunTimeout gives:
//
//	2 × (120s + 600s + 90s) + 30s + 60s
//	= 1710s ≈ 28.5 minutes < 30-minute CronJob schedule
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 2,
		Backoff:     30 * time.Second,
	}
}

// IsVCenterDegradedError reports whether err is attributable to vCenter
// slowness or unreachability rather than to a Crucible provisioning defect.
//
// Classification rules (any one sufficient):
//
//  1. errors.Is(err, context.DeadlineExceeded) — the transport context
//     expired; most commonly vCenter's SDK endpoint timing out.
//  2. Error message contains "provisioning" AND "timed out" — the pod was
//     stuck in the "provisioning" vCenter-clone step and the ready-poll timed
//     out. This is the exact pattern observed in production.
//  3. Error message contains "context deadline exceeded" — a lower-level
//     string form of rule 1 that may appear after error wrapping loses the
//     typed sentinel.
//
// This function deliberately checks message strings rather than only typed
// sentinels because the error originates from the Crucible API, traverses an
// HTTP transport, and is then wrapped again by runPodLifecycle — by the time
// the runner sees it the original sentinel is often gone.
//
// Important: a false positive here (marking a non-vCenter failure as vCenter
// degradation) produces a misleading metric but does NOT suppress the failure
// — Result.Success is still false. The only risk is directing operator
// attention to vCenter when the real cause is elsewhere, so the rules are
// intentionally conservative.
func IsVCenterDegradedError(err error) bool {
	if err == nil {
		return false
	}
	// Typed sentinel — survives most wrapping chains.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	// Pod stuck in vCenter clone step: waitForPodStatus returned
	// "timed out waiting for status [active] (last seen "provisioning") after …"
	if strings.Contains(msg, "provisioning") && strings.Contains(msg, "timed out") {
		return true
	}
	// String form of DeadlineExceeded after wrapping.
	if strings.Contains(msg, "context deadline exceeded") {
		return true
	}
	return false
}
