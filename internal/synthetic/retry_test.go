package synthetic

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestIsVCenterDegradedError_NilIsAlwaysFalse guards the nil-safety contract.
func TestIsVCenterDegradedError_NilIsAlwaysFalse(t *testing.T) {
	if IsVCenterDegradedError(nil) {
		t.Error("IsVCenterDegradedError(nil) = true, want false")
	}
}

// TestIsVCenterDegradedError_DeadlineExceededSentinel tests the typed sentinel.
func TestIsVCenterDegradedError_DeadlineExceededSentinel(t *testing.T) {
	if !IsVCenterDegradedError(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded should be classified as vCenter degraded")
	}
}

// TestIsVCenterDegradedError_WrappedDeadlineExceeded tests detection through
// fmt.Errorf wrapping, which loses the typed sentinel.
func TestIsVCenterDegradedError_WrappedDeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("wait for active: %w", context.DeadlineExceeded)
	if !IsVCenterDegradedError(wrapped) {
		t.Error("wrapped context.DeadlineExceeded should be classified as vCenter degraded")
	}
}

// TestIsVCenterDegradedError_ProvisioningStuck tests the exact production
// error pattern observed in the logs:
//
//	"timed out waiting for status [active] (last seen "provisioning") after 3m0s"
func TestIsVCenterDegradedError_ProvisioningStuck(t *testing.T) {
	// Exact string from waitForPodStatus wrapped by runPodLifecycle.
	err := errors.New(`wait for active failed: timed out waiting for status [active] (last seen "provisioning") after 3m0s`)
	if !IsVCenterDegradedError(err) {
		t.Errorf("pod stuck in provisioning should be classified as vCenter degraded: %v", err)
	}
}

// TestIsVCenterDegradedError_ContextDeadlineExceededString tests the string
// form of the deadline exceeded error, which appears after error wrapping
// loses the typed sentinel.
func TestIsVCenterDegradedError_ContextDeadlineExceededString(t *testing.T) {
	err := errors.New(`Post "https://vcenter.lab.jmal.io/sdk": context deadline exceeded`)
	if !IsVCenterDegradedError(err) {
		t.Errorf("explicit vcenter SDK timeout should be classified as vCenter degraded: %v", err)
	}
}

// TestIsVCenterDegradedError_NonVCenterErrors confirms that generic failures
// do NOT get misclassified as vCenter degradation.
func TestIsVCenterDegradedError_NonVCenterErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"403 forbidden", errors.New("admin endpoint returned 403, want 200")},
		{"template not found", errors.New(`template "synthetic-noop" not found among 5 accessible templates`)},
		{"pod entered error state", errors.New(`pod entered terminal failure status "error"`)},
		{"connection refused", errors.New("dial tcp 10.10.10.1:443: connect: connection refused")},
		{"generic API error", errors.New("POST /pods returned 500: internal server error")},
		{"empty error", errors.New("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsVCenterDegradedError(tc.err) {
				t.Errorf("error %q was incorrectly classified as vCenter degraded", tc.err)
			}
		})
	}
}

// TestDefaultRetryConfig_HasSensibleDefaults pins the production defaults so
// any change requires updating both the code and this test.
func TestDefaultRetryConfig_HasSensibleDefaults(t *testing.T) {
	cfg := DefaultRetryConfig()
	if cfg.MaxAttempts != 2 {
		t.Errorf("MaxAttempts = %d, want 2 (one retry)", cfg.MaxAttempts)
	}
	if cfg.Backoff != 30*time.Second {
		t.Errorf("Backoff = %v, want 30s", cfg.Backoff)
	}
}
