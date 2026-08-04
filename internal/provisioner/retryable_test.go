package provisioner

import (
	"errors"
	"testing"
	"time"
)

// TestClassifyError_RetryableExamples verifies the primary retryable cases,
// with special attention to the transient clone fault that motivates this fix.
func TestClassifyError_RetryableExamples(t *testing.T) {
	cases := []struct {
		name       string
		err        string
		jobType    string
		wantOK     bool
		wantReason string
	}{
		// ---- Primary target -----------------------------------------------
		{
			name:       "virtual disk transient clone fault (template_provision)",
			err:        "clone source VM: wait clone task: The virtual disk is either corrupted or not a supported format.",
			jobType:    "template_provision",
			wantOK:     true,
			wantReason: RetryReasonTransientClone,
		},
		{
			name:       "virtual disk transient clone fault (pod_create)",
			err:        "clone VM: wait clone task: The virtual disk is either corrupted or not a supported format.",
			jobType:    "pod_create",
			wantOK:     true,
			wantReason: RetryReasonTransientClone,
		},
		{
			name:    "virtual disk fault on non-clone job is NOT retryable",
			err:     "The virtual disk is either corrupted or not a supported format.",
			jobType: "vm_start",
			wantOK:  false,
		},
		// ---- Connection / network -----------------------------------------
		{
			name:    "connection reset by peer",
			err:     "vcenter: connection reset by peer",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonConnection,
		},
		{
			name:    "EOF",
			err:     "vcenter: EOF",
			jobType: "pod_create",
			wantOK:  true, wantReason: RetryReasonConnection,
		},
		{
			name:    "context deadline exceeded",
			err:     "wait clone task: context deadline exceeded",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonConnection,
		},
		// ---- Timeout / task faults ----------------------------------------
		{
			name:    "task timeout",
			err:     "clone: task timeout after 60s",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonTimeout,
		},
		{
			name:    "ServerFaultCode parameter fault",
			err:     "clone task: ServerFaultCode: A specified parameter was not correct: relocateSpec.dataMovement",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonTimeout,
		},
		// ---- Host / storage availability ----------------------------------
		{
			name:    "resource temporarily unavailable",
			err:     "vcenter: resource temporarily unavailable",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonUnavailable,
		},
		{
			name:    "host in maintenance",
			err:     "host is in maintenance mode",
			jobType: "template_provision",
			wantOK:  true, wantReason: RetryReasonUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ClassifyError(errors.New(tc.err), tc.jobType)
			if ok != tc.wantOK {
				t.Errorf("ClassifyError(%q, %q) retryable = %v, want %v", tc.err, tc.jobType, ok, tc.wantOK)
			}
			if tc.wantOK && reason != tc.wantReason {
				t.Errorf("ClassifyError(%q, %q) reason = %q, want %q", tc.err, tc.jobType, reason, tc.wantReason)
			}
		})
	}
}

// TestClassifyError_NonRetryableRealExamples tests the four real production
// non-retryable failures listed in the task spec.  Each must return false.
func TestClassifyError_NonRetryableRealExamples(t *testing.T) {
	cases := []struct {
		name string
		err  string
	}{
		{
			name: "invalid ISO datastore path",
			err:  `source_ref "my-iso.iso" is not a valid installer ISO datastore path for source_type=iso (want "[datastore] path/to/installer.iso"): ...`,
		},
		{
			name: "folder not found",
			err:  `create blank VM: find folder "": folder '' not found`,
		},
		{
			name: "resource pool resolves to multiple instances",
			err:  "resolve default resource pool prod-cluster: default resource pool resolves to multiple instances, please specify",
		},
		{
			name: "guest auth failure",
			err:  "run generalize script: Failed to authenticate with the guest operating system using the supplied credentials",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, _ := ClassifyError(errors.New(tc.err), "template_provision")
			if ok {
				t.Errorf("ClassifyError(%q) = retryable, want NOT retryable", tc.err)
			}
		})
	}
}

// TestClassifyError_SafeDefault asserts that an unrecognised error is not
// retried.  This is the most important property: the wrong "retryable" verdict
// turns a clear, fast failure into a slow, confusing 15-minute wait.
func TestClassifyError_SafeDefault(t *testing.T) {
	cases := []string{
		"something completely unexpected happened",
		"disk full",
		"permission denied on datastore",
		"out of memory",
		"",
	}
	for _, s := range cases {
		ok, reason := ClassifyError(errors.New(s), "template_provision")
		if ok {
			t.Errorf("ClassifyError(%q) = retryable (reason=%q), want NOT retryable (safe default)", s, reason)
		}
	}
	if ok, _ := ClassifyError(nil, "template_provision"); ok {
		t.Error("ClassifyError(nil) = retryable, want false")
	}
}

// TestRetryBackoff verifies that the backoff grows and stays under the cap.
func TestRetryBackoff(t *testing.T) {
	// attempt 0 (first retry after first failure): ~30 s
	d0 := RetryBackoff(0)
	if d0 < 30*time.Second || d0 > 45*time.Second {
		t.Errorf("RetryBackoff(0) = %s, want [30s, 45s]", d0)
	}
	// attempt 1: ~60 s
	d1 := RetryBackoff(1)
	if d1 < 60*time.Second || d1 > 90*time.Second {
		t.Errorf("RetryBackoff(1) = %s, want [60s, 90s]", d1)
	}
	// attempt 2: ~120 s
	d2 := RetryBackoff(2)
	if d2 < 120*time.Second || d2 > 180*time.Second {
		t.Errorf("RetryBackoff(2) = %s, want [120s, 180s]", d2)
	}
	// must grow
	if d1 <= d0 {
		t.Errorf("backoff must grow: d1=%s <= d0=%s", d1, d0)
	}
	// cap: large attempt number stays at or near retryBackoffMax
	dBig := RetryBackoff(20)
	if dBig > retryBackoffMax+retryBackoffMax/4+time.Second {
		t.Errorf("RetryBackoff(20) = %s, want <= cap + max_jitter", dBig)
	}
}
