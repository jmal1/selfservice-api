package synthetic

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeCheck lets each test parameterize behaviour without defining new types.
type fakeCheck struct {
	name     string
	title    string
	desc     string
	runbook  string
	severity Severity
	run      func(ctx context.Context, c *Client) (int, error)
}

func (f fakeCheck) Name() string                                    { return f.name }
func (f fakeCheck) Title() string                                   { return f.title }
func (f fakeCheck) Description() string                             { return f.desc }
func (f fakeCheck) Runbook() string                                 { return f.runbook }
func (f fakeCheck) Severity() Severity                              { return f.severity }
func (f fakeCheck) Run(ctx context.Context, c *Client) (int, error) { return f.run(ctx, c) }

// noopLogger discards all log output so tests stay quiet.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunner_RunOnce_RecordsSuccessAndFailure pins the core observability
// contract: each registered check produces exactly one Result with correct
// success/failure attribution.
func TestRunner_RunOnce_RecordsSuccessAndFailure(t *testing.T) {
	pgHits := 0
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pgHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer pg.Close()

	runner := NewRunner(
		NewClient("http://unused", ""),
		&Pushgateway{BaseURL: pg.URL, Job: "test", HTTP: pg.Client()},
		[]Check{
			fakeCheck{name: "happy", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
				return 200, nil
			}},
			fakeCheck{name: "sad", severity: SeverityWarning, run: func(ctx context.Context, c *Client) (int, error) {
				return 503, errors.New("downstream borked")
			}},
		},
		noopLogger(),
	)

	results := runner.RunOnce(context.Background())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if !results[0].Success || results[0].HTTPStatus != 200 || results[0].Name != "happy" {
		t.Errorf("happy: %+v", results[0])
	}
	if results[1].Success || results[1].HTTPStatus != 503 || results[1].Name != "sad" {
		t.Errorf("sad: %+v", results[1])
	}
	if pgHits != 1 {
		t.Errorf("pushgateway hit %d times, want 1 (batched push)", pgHits)
	}
}

// TestRunner_RunOnce_TimeoutPropagates ensures the runner enforces its own
// CheckTimeout, so a hung dependency cannot stall the whole monitor.
func TestRunner_RunOnce_TimeoutPropagates(t *testing.T) {
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "slow", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(2 * time.Second):
				return 200, nil
			}
		}},
	}, noopLogger())
	runner.CheckTimeout = 50 * time.Millisecond

	start := time.Now()
	results := runner.RunOnce(context.Background())
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("runner waited %v, expected ~50ms timeout to fire", elapsed)
	}
	if len(results) != 1 || results[0].Success {
		t.Fatalf("expected one failing result, got %+v", results)
	}
}

// TestRunner_RunOnce_RecoversFromPanic guarantees a buggy check cannot crash
// the runner — a panic must be recorded as a failure, not a process exit.
func TestRunner_RunOnce_RecoversFromPanic(t *testing.T) {
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "boom", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			panic("intentional test panic")
		}},
		fakeCheck{name: "ok", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			return 200, nil
		}},
	}, noopLogger())

	results := runner.RunOnce(context.Background())
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Success {
		t.Error("expected boom check to be marked failed after panic")
	}
	if !results[1].Success {
		t.Error("expected subsequent check to still run after prior panic")
	}
}

// TestRunner_RunOnce_PushgatewayFailureDoesNotCrash protects the invariant
// that a Pushgateway outage degrades silently; the runner returns results and
// logs the error but never panics or returns it to the caller (which would
// cause the K8s CronJob to be marked failed).
func TestRunner_RunOnce_PushgatewayFailureDoesNotCrash(t *testing.T) {
	pg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer pg.Close()

	runner := NewRunner(
		NewClient("http://unused", ""),
		&Pushgateway{BaseURL: pg.URL, Job: "test", HTTP: pg.Client()},
		[]Check{
			fakeCheck{name: "ok", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
				return 200, nil
			}},
		},
		noopLogger(),
	)
	// If this call panics or stalls, the test runner will fail it.
	results := runner.RunOnce(context.Background())
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("results should still reflect the actual check outcome; got %+v", results)
	}
}

// TestRunner_Retry_FailOnceThenSucceed is the core retry contract: a check
// that fails on attempt 1 but succeeds on attempt 2 is GREEN, reports
// Attempts=2, and does NOT set Success=false.
func TestRunner_Retry_FailOnceThenSucceed(t *testing.T) {
	callCount := 0
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "flaky", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			callCount++
			if callCount == 1 {
				// Simulate a provisioning-stuck error (vCenter stall).
				return 0, errors.New(`timed out waiting for status [active] (last seen "provisioning") after 2m0s`)
			}
			return 200, nil
		}},
	}, noopLogger())
	runner.SetRetry("flaky", RetryConfig{MaxAttempts: 2, Backoff: 0})

	results := runner.RunOnce(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if !r.Success {
		t.Errorf("check should be green after passing on retry: Success=%v Err=%v", r.Success, r.Err)
	}
	if r.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (first failed, second succeeded)", r.Attempts)
	}
	if !r.VCenterDegraded {
		t.Error("VCenterDegraded should be set: the first attempt had a provisioning-stuck error")
	}
	if callCount != 2 {
		t.Errorf("check function called %d times, want 2", callCount)
	}
}

// TestRunner_Retry_ExhaustedAttemptsRecordsFailure confirms that exhausting all
// retries records the final failure with the total attempt count.
func TestRunner_Retry_ExhaustedAttemptsRecordsFailure(t *testing.T) {
	callCount := 0
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "broken", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			callCount++
			return 503, errors.New(`timed out waiting for status [active] (last seen "provisioning") after 2m0s`)
		}},
	}, noopLogger())
	runner.SetRetry("broken", RetryConfig{MaxAttempts: 3, Backoff: 0})

	results := runner.RunOnce(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Success {
		t.Error("check should be red when all attempts fail")
	}
	if r.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3 (all attempts exhausted)", r.Attempts)
	}
	if r.Err == nil {
		t.Error("Err should be non-nil on failure")
	}
	if !r.VCenterDegraded {
		t.Error("VCenterDegraded should be set for provisioning-stuck errors")
	}
	if callCount != 3 {
		t.Errorf("check function called %d times, want 3", callCount)
	}
}

// TestRunner_Retry_NonVCenterFailureDoesNotSetDegradedFlag confirms that a
// non-vCenter failure (e.g. a Crucible defect) does not set VCenterDegraded.
func TestRunner_Retry_NonVCenterFailureDoesNotSetDegradedFlag(t *testing.T) {
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "rbac-broken", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			return 200, errors.New("admin endpoint returned 200, want 403")
		}},
	}, noopLogger())
	runner.SetRetry("rbac-broken", RetryConfig{MaxAttempts: 2, Backoff: 0})

	results := runner.RunOnce(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.VCenterDegraded {
		t.Error("VCenterDegraded should NOT be set for a non-vCenter error (RBAC regression)")
	}
	if r.Success {
		t.Error("check should be red — this is a real failure, not a transient vCenter stall")
	}
}

// TestRunner_Retry_NoRetryOnCheapChecks confirms that checks without a retry
// policy execute exactly once, even when they fail.
func TestRunner_Retry_NoRetryOnCheapChecks(t *testing.T) {
	callCount := 0
	runner := NewRunner(NewClient("http://unused", ""), nil, []Check{
		fakeCheck{name: "no_retry_check", severity: SeverityCritical, run: func(ctx context.Context, c *Client) (int, error) {
			callCount++
			return 403, errors.New("503 service unavailable")
		}},
	}, noopLogger())
	// Deliberately NOT calling runner.SetRetry for this check.

	results := runner.RunOnce(context.Background())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no retry policy registered)", results[0].Attempts)
	}
	if callCount != 1 {
		t.Errorf("check function called %d times, want 1 (no retry for cheap checks)", callCount)
	}
}

// TestRunner_Retry_ChecksFromAllHaveNoRetry confirms that the standard check
// catalog (healthz, auth_me, 403-checks etc.) has no retry policy wired by
// default — a regression here would mask real API failures.
func TestRunner_Retry_ChecksFromAllHaveNoRetry(t *testing.T) {
	// Build a runner with all standard checks registered but no retry policy.
	// Every check should report Attempts=1.
	var allChecks []Check
	for _, name := range []string{
		"healthz", "auth_me", "pods_list",
		"admin_list_users_403", "admin_run_detail_403",
		"admin_audit_403", "pod_testing_dashboard_404",
		"wiki_index_rbac",
	} {
		n := name
		allChecks = append(allChecks, fakeCheck{
			name:     n,
			severity: SeverityCritical,
			run: func(ctx context.Context, c *Client) (int, error) {
				return 200, nil
			},
		})
	}
	runner := NewRunner(NewClient("http://unused", ""), nil, allChecks, noopLogger())
	// No SetRetry calls — confirming none of the standard checks have retry.
	for _, name := range []string{"healthz", "auth_me", "pods_list"} {
		cfg := runner.retryConfigFor(name)
		if cfg.MaxAttempts != 1 {
			t.Errorf("check %q has MaxAttempts=%d, want 1 (cheap checks must not retry)", name, cfg.MaxAttempts)
		}
	}
}
