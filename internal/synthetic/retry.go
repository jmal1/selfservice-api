package synthetic

import (
	"context"
	"errors"
	"fmt"
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

// CheckAttemptOverhead is the fixed allowance for HTTP round-trips, orphan
// cleanup, result processing, and logging outside a check's configured polling
// phases. Keeping this explicit and shared prevents per-attempt contexts from
// drifting away from the phase timeouts they are intended to protect.
const CheckAttemptOverhead = 60 * time.Second

// CheckCleanupReserve is the portion of CheckAttemptOverhead reserved for the
// final best-effort DELETE after ordinary work stops.
const CheckCleanupReserve = 30 * time.Second

// RunnerSessionTokenMargin keeps the runner-only JWT valid beyond the Job
// deadline and final cleanup reserve without allowing it to reach the next
// scheduled run.
const RunnerSessionTokenMargin = 30 * time.Second

// CheckAttemptTimeout returns the outer context budget for one check attempt.
func CheckAttemptTimeout(phases ...time.Duration) time.Duration {
	total := CheckAttemptOverhead
	for _, phase := range phases {
		total += phase
	}
	return total
}

// RetryCycleTimeout returns the maximum time the Runner can authorize for one
// check, including every attempt context and each configured retry backoff.
func RetryCycleTimeout(perAttempt time.Duration, cfg RetryConfig) time.Duration {
	attempts := cfg.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	return time.Duration(attempts)*perAttempt +
		time.Duration(attempts-1)*cfg.Backoff
}

// RunnerSessionTokenTTL derives the runner-only JWT lifetime from the
// Kubernetes Job deadline. The token must survive SIGTERM cleanup at the hard
// deadline, but must expire before the next scheduled runner starts.
func RunnerSessionTokenTTL(activeDeadline, scheduleInterval time.Duration) (time.Duration, error) {
	if activeDeadline <= 0 {
		return 0, fmt.Errorf("runner active deadline must be positive")
	}
	if scheduleInterval <= 0 {
		return 0, fmt.Errorf("runner schedule interval must be positive")
	}
	ttl := activeDeadline + CheckCleanupReserve + RunnerSessionTokenMargin
	if ttl >= scheduleInterval {
		return 0, fmt.Errorf(
			"runner session TTL %s must be shorter than schedule interval %s",
			ttl, scheduleInterval,
		)
	}
	return ttl, nil
}

// DefaultRetryConfig returns production-safe retry defaults for the expensive
// vCenter-dependent checks.
//
// MaxAttempts=2 (one retry) combined with the 150-second ReadyTimeout gives a
// worst-case pod_lifecycle cycle time of:
//
//	2 × (ReadyTimeout + DestroyTimeout + per-attempt overhead) + Backoff
//	= 2 × (150s + 90s + 60s) + 30s
//	= 630s = 10.5 minutes < 12-minute CronJob schedule
//
// For runner_smoke (hourly CronJob), the same config with the chart-aligned
// 8-minute ReadyTimeout and 10-minute RunTimeout gives:
//
//	2 × (480s + 600s + 90s + 60s overhead) + 30s
//	= 2490s = 41.5 minutes < 45-minute job deadline < hourly schedule
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
