// retryable.go — error classification and retry backoff for the job worker.
//
// Design rules (not to be changed without evidence):
//
//   - Default is NOT retryable.  An unrecognised error silently becoming
//     retryable would turn a clear, fast failure into a slow, confusing
//     one (3 attempts × up to 5 minutes each = 15 minutes of mystery).
//
//   - Deterministic failures are explicitly listed and never retried,
//     even if their text overlaps with transient patterns.  Every entry
//     in deterministicPhrases has a matching real production job failure.
//
//   - The primary target of this file is the vCenter transient clone
//     rejection ("virtual disk is either corrupted or not a supported
//     format") that was misdiagnosed three times as a CloneSpec bug.
//     Evidence from the live system (2026-08-03):
//
//   - job d62777e7 cloned student-ubuntu-2404 at 20:36:11 → FAILED
//
//   - job a3ba9028, same source + params, at 20:37:19 → SUCCEEDED
//     68 seconds apart, identical spec.  The fault is environmental and
//     transient; a retry is the correct fix.
package provisioner

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// Retry reason labels used in the crucible_job_retries_total metric.
const (
	RetryReasonTransientClone = "transient_clone"
	RetryReasonConnection     = "connection"
	RetryReasonTimeout        = "timeout"
	RetryReasonUnavailable    = "unavailable"
	RetryReasonCleanup        = "cleanup"
)

var errVMPlacementCapacityRelease = errors.New("VM placement capacity release unavailable")

func jobRetryAvailable(job *models.Job, jobErr error) bool {
	retryable, _ := ClassifyError(jobErr, job.Type)
	return retryable && job.RetryCount < job.MaxRetries
}

// deterministicPhrases are substrings that identify errors that will never
// succeed on retry.  Each entry corresponds to a real production failure.
// Checked before the retryable lists so a string that matches both always
// loses to "not retryable".
var deterministicPhrases = []string{
	// source_ref "..." is not a valid installer ISO datastore path for source_type=iso
	"is not a valid installer ISO datastore path for source_type=iso",
	// create blank VM: find folder "": folder '' not found
	`folder '' not found`,
	// resolve default resource pool ...: default resource pool resolves to
	// multiple instances, please specify
	"resolves to multiple instances, please specify",
	// Failed to authenticate with the guest operating system using the
	// supplied credentials
	"Failed to authenticate with the guest operating system",
	// Validation errors set before any vCenter call.
	"template_id is required",
	"vm_name is required",
	"staging_network is required",
	"is required",
	// Wrong lifecycle state — retrying won't help until the operator resets.
	"expected \"provisioning\"",
	"expected \"generalizing\"",
	"expected \"verifying\"",
	// Template / source not found in our DB — not a transient vCenter fault.
	"not found in database",
	"template_id not found",
}

// ClassifyError reports whether jobErr is safe to retry, and if so,
// returns a short reason label for the retry metrics.
//
// jobType is one of the models.JobType* constants; it constrains which
// errors are retryable (e.g. the disk-format fault is only retryable on
// the job types that perform a vCenter clone).
//
// The safe default is (false, "") — callers must never retry an
// unrecognised error.
func ClassifyError(err error, jobType string) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}
	var compensatedErr *compensatedJobError
	if errors.As(err, &compensatedErr) {
		return false, ""
	}
	var manualErr *manualCleanupRequiredError
	if errors.As(err, &manualErr) {
		return false, ""
	}
	var compensationErr *compensationRetryError
	if errors.As(err, &compensationErr) {
		return true, RetryReasonCleanup
	}
	if isPodCreateCleanupRetry(err) {
		if jobType == "pod_create" {
			return true, RetryReasonCleanup
		}
		return false, ""
	}
	s := err.Error()

	// Deterministic failures: check first and never retry.
	for _, phrase := range deterministicPhrases {
		if strings.Contains(s, phrase) {
			return false, ""
		}
	}
	if errors.Is(err, vcenter.ErrPlacementValidationUnavailable) {
		return true, RetryReasonUnavailable
	}
	if errors.Is(err, errVMPlacementCapacityRelease) {
		return true, RetryReasonUnavailable
	}
	if strings.Contains(s, "stale VM clone") {
		return true, RetryReasonCleanup
	}

	// -----------------------------------------------------------------------
	// Retryable: transient vCenter clone-validation rejection.
	//
	// "The virtual disk is either corrupted or not a supported format."
	// appears fast (~1 s task lifetime) when vSphere's internal inventory
	// state is momentarily inconsistent.  The same clone succeeds seconds
	// later with no spec change.  Only clone-performing job types.
	// -----------------------------------------------------------------------
	if strings.Contains(s, "virtual disk is either corrupted or not a supported format") {
		switch jobType {
		case "template_provision", "template_replica_build", "pod_create", "vm_add":
			return true, RetryReasonTransientClone
		}
		// Not from a clone job — don't assume it's transient.
		return false, ""
	}

	// -----------------------------------------------------------------------
	// Retryable: network / connection-level transients.
	// -----------------------------------------------------------------------
	connectionPhrases := []string{
		"connection reset by peer",
		"connection refused",
		"EOF",
		"io: read/write on closed pipe",
		"context deadline exceeded",
		"read tcp",
		"write tcp",
	}
	for _, phrase := range connectionPhrases {
		if strings.Contains(strings.ToLower(s), strings.ToLower(phrase)) {
			return true, RetryReasonConnection
		}
	}

	// -----------------------------------------------------------------------
	// Retryable: vCenter task timeouts and transient task-parameter faults.
	// -----------------------------------------------------------------------
	timeoutPhrases := []string{
		"task timeout",
		"timed out waiting",
		// vCenter returns this on a task result (not our own validation)
		// when an internal pre-check fails transiently.
		"ServerFaultCode: A specified parameter was not correct",
	}
	for _, phrase := range timeoutPhrases {
		if strings.Contains(s, phrase) {
			return true, RetryReasonTimeout
		}
	}

	// -----------------------------------------------------------------------
	// Retryable: host-in-maintenance, APD, resource availability faults.
	// -----------------------------------------------------------------------
	unavailablePhrases := []string{
		"resource temporarily unavailable",
		"host is in maintenance mode",
		"all paths down",
		"APD",
	}
	for _, phrase := range unavailablePhrases {
		if strings.Contains(strings.ToLower(s), strings.ToLower(phrase)) {
			return true, RetryReasonUnavailable
		}
	}

	// Safe default: unknown errors are not retried.
	return false, ""
}

// retryBackoffBase is the delay before attempt 1 (the first retry).
// Subsequent delays double: 30 s, 60 s, 120 s, capped at retryBackoffMax.
const (
	retryBackoffBase = 30 * time.Second
	retryBackoffMax  = 5 * time.Minute
)

// RetryBackoff returns the delay before attempt number (retryCount + 1).
// retryCount is the value already stored in jobs.retry_count (0 on the
// first failure, 1 on the second, etc.).  The delay doubles each time
// and is capped at retryBackoffMax, then a uniform jitter of up to 25%
// of the computed delay is added to spread load.
func RetryBackoff(retryCount int) time.Duration {
	delay := retryBackoffBase
	for i := 0; i < retryCount && delay < retryBackoffMax; i++ {
		if delay > retryBackoffMax/2 {
			delay = retryBackoffMax
			break
		}
		delay *= 2
	}
	// Jitter: up to 25% of delay.
	jitterBound := int64(delay / 4)
	if jitterBound > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(jitterBound))
		if err == nil {
			delay += time.Duration(n.Int64())
		}
	}
	return delay
}

// FriendlyError converts a known vCenter fault into an actionable message
// suitable for display to an instructor.  The raw error is preserved in the
// job result under "raw_error" for admin inspection.
//
// For unknown errors the original message is returned unchanged.
func FriendlyError(err error, retryCount, maxRetries int) string {
	if err == nil {
		return ""
	}
	s := err.Error()

	if strings.Contains(s, "virtual disk is either corrupted or not a supported format") {
		if retryCount >= maxRetries {
			dur := RetryBackoff(0) * time.Duration(maxRetries) // approximate
			return fmt.Sprintf(
				"vSphere rejected the clone with a transient storage/inventory error "+
					"(%d attempt(s) over ~%s).  The error is intermittent — a manual "+
					"retry (Template → Retry) usually succeeds.  If it persists, check "+
					"datastore health and vCenter task history for the source VM.",
				retryCount+1, dur.Round(time.Second),
			)
		}
		return "vSphere rejected the clone (transient storage/inventory error). " +
			"Scheduled for retry — this usually succeeds on the next attempt."
	}

	return s
}
