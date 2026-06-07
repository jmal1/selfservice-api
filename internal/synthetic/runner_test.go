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
	severity Severity
	run      func(ctx context.Context, c *Client) (int, error)
}

func (f fakeCheck) Name() string                                    { return f.name }
func (f fakeCheck) Title() string                                   { return f.title }
func (f fakeCheck) Description() string                             { return f.desc }
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
