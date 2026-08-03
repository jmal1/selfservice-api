package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRunnerMetrics_SerializeFormat verifies that every metric family has
// # HELP and # TYPE lines, and that metric names exactly match the spec.
func TestRunnerMetrics_SerializeFormat(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)
	m.RecordRunnerJob("success")
	m.RecordRunnerJob("failed")
	m.ObserveRunnerProvision(5 * time.Second)
	m.ObserveRunnerExecution("kali_runner", 30*time.Second)
	m.SetRunnerActive(2)
	m.RecordCleanupFailure("secret")
	m.RecordOrphansCleaned(3)
	m.RecordCallback("complete", "success")

	out := string(m.serialize())

	requiredHELP := []string{
		"# HELP crucible_runner_job_total",
		"# HELP crucible_runner_provision_duration_seconds_sum",
		"# HELP crucible_runner_provision_duration_seconds_count",
		"# HELP crucible_runner_provision_duration_seconds_max",
		"# HELP crucible_runner_execution_duration_seconds_sum",
		"# HELP crucible_runner_execution_duration_seconds_count",
		"# HELP crucible_runner_active",
		"# HELP crucible_runner_cleanup_failed_total",
		"# HELP crucible_runner_orphans_cleaned_total",
		"# HELP crucible_engine_callback_total",
	}
	requiredTYPE := []string{
		"# TYPE crucible_runner_job_total counter",
		"# TYPE crucible_runner_provision_duration_seconds_sum counter",
		"# TYPE crucible_runner_provision_duration_seconds_count counter",
		"# TYPE crucible_runner_provision_duration_seconds_max gauge",
		"# TYPE crucible_runner_execution_duration_seconds_sum counter",
		"# TYPE crucible_runner_execution_duration_seconds_count counter",
		"# TYPE crucible_runner_active gauge",
		"# TYPE crucible_runner_cleanup_failed_total counter",
		"# TYPE crucible_runner_orphans_cleaned_total counter",
		"# TYPE crucible_engine_callback_total counter",
	}
	requiredSamples := []string{
		`crucible_runner_job_total{result="success"} 1`,
		`crucible_runner_job_total{result="failed"} 1`,
		`crucible_runner_provision_duration_seconds_sum 5`,
		`crucible_runner_provision_duration_seconds_count 1`,
		`crucible_runner_provision_duration_seconds_max 5`,
		`crucible_runner_execution_duration_seconds_sum{execution_mode="kali_runner"} 30`,
		`crucible_runner_execution_duration_seconds_count{execution_mode="kali_runner"} 1`,
		`crucible_runner_active 2`,
		`crucible_runner_cleanup_failed_total{resource="secret"} 1`,
		`crucible_runner_orphans_cleaned_total 3`,
		`crucible_engine_callback_total{endpoint="complete",result="success"} 1`,
	}

	for _, want := range requiredHELP {
		if !strings.Contains(out, want) {
			t.Errorf("missing HELP line %q\n--- got ---\n%s", want, out)
		}
	}
	for _, want := range requiredTYPE {
		if !strings.Contains(out, want) {
			t.Errorf("missing TYPE line %q\n--- got ---\n%s", want, out)
		}
	}
	for _, want := range requiredSamples {
		if !strings.Contains(out, want) {
			t.Errorf("missing sample %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestRunnerMetrics_CounterPairsNotHistogram is a deliberate regression guard.
// Duration families must be _sum/_count counter pairs, never histograms.
// If someone converts them to histograms, the dashboards break silently
// because histogram_quantile() over missing _bucket series returns empty.
func TestRunnerMetrics_CounterPairsNotHistogram(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)
	m.ObserveRunnerProvision(10 * time.Second)
	m.ObserveRunnerExecution("kali_runner", 5*time.Second)

	out := string(m.serialize())

	// Required counter pairs must be present.
	required := []string{
		"crucible_runner_provision_duration_seconds_sum",
		"crucible_runner_provision_duration_seconds_count",
		"crucible_runner_execution_duration_seconds_sum",
		"crucible_runner_execution_duration_seconds_count",
	}
	for _, name := range required {
		if !strings.Contains(out, name) {
			t.Errorf("missing required counter pair series %q\n--- got ---\n%s", name, out)
		}
	}

	// No histogram artifacts allowed.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "_bucket") {
			t.Errorf("output contains _bucket (histogram artifact): %q", line)
		}
		if strings.Contains(line, `le=`) {
			t.Errorf("output contains le= label (histogram artifact): %q", line)
		}
	}
}

// TestRunnerMetrics_Accumulates verifies that counters increment monotonically
// and that different label values are tracked independently.
func TestRunnerMetrics_Accumulates(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)

	for i := 0; i < 3; i++ {
		m.RecordRunnerJob("success")
	}
	m.RecordRunnerJob("failed")

	out := string(m.serialize())

	if !strings.Contains(out, `crucible_runner_job_total{result="success"} 3`) {
		t.Errorf("success counter should be 3\n--- got ---\n%s", out)
	}
	if !strings.Contains(out, `crucible_runner_job_total{result="failed"} 1`) {
		t.Errorf("failed counter should be 1\n--- got ---\n%s", out)
	}

	// Add another success — must go to 4, not reset.
	m.RecordRunnerJob("success")
	out2 := string(m.serialize())
	if !strings.Contains(out2, `crucible_runner_job_total{result="success"} 4`) {
		t.Errorf("success counter should be 4 after second batch\n--- got ---\n%s", out2)
	}
}

// TestRunnerMetrics_ProvisionMaxTracksWorst verifies that _max reflects the
// largest observed duration, not the most recent one.
func TestRunnerMetrics_ProvisionMaxTracksWorst(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)

	m.ObserveRunnerProvision(10 * time.Second)
	m.ObserveRunnerProvision(30 * time.Second) // largest
	m.ObserveRunnerProvision(5 * time.Second)  // later but smaller

	out := string(m.serialize())

	if !strings.Contains(out, "crucible_runner_provision_duration_seconds_max 30") {
		t.Errorf("_max should be 30, not the latest 5\n--- got ---\n%s", out)
	}
	if !strings.Contains(out, "crucible_runner_provision_duration_seconds_sum 45") {
		t.Errorf("_sum should be 45\n--- got ---\n%s", out)
	}
	if !strings.Contains(out, "crucible_runner_provision_duration_seconds_count 3") {
		t.Errorf("_count should be 3\n--- got ---\n%s", out)
	}
}

// TestRunnerMetrics_NilSafe verifies that every method on a nil *RunnerMetrics
// is a no-op and does not panic.
func TestRunnerMetrics_NilSafe(t *testing.T) {
	var m *RunnerMetrics

	// All of these must complete without panicking.
	m.RecordRunnerJob("success")
	m.ObserveRunnerProvision(time.Second)
	m.ObserveRunnerExecution("kali_runner", time.Second)
	m.SetRunnerActive(5)
	m.incRunnerActive(1)
	m.incRunnerActive(-1)
	m.RecordCleanupFailure("job")
	m.RecordOrphansCleaned(3)
	m.RecordCallback("complete", "success")
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("nil Push should return nil, got %v", err)
	}
}

// TestRunnerMetrics_PushSurfacesErrorOnFailure verifies that Push surfaces
// errors (non-2xx response, unreachable target) as return values rather than
// panicking. Callers are expected to log and continue — metrics must never
// break the operation they measure. This matches the behavior of
// PipelineMetrics.Push and DestroyFailedPusher.Push.
func TestRunnerMetrics_PushSurfacesErrorOnFailure(t *testing.T) {
	t.Run("non2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("text format parsing error"))
		}))
		defer srv.Close()

		m := NewRunnerMetrics(srv.URL, "crucible_engine", nil)
		m.RecordRunnerJob("success")
		err := m.Push(context.Background())
		if err == nil {
			t.Fatal("expected error on non-2xx response")
		}
		if !strings.Contains(err.Error(), "400") {
			t.Errorf("error should mention status 400, got: %v", err)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		// Port 0 on loopback is not listening; dialing it must fail.
		m := NewRunnerMetrics("http://127.0.0.1:0", "crucible_engine", nil)
		m.RecordRunnerJob("success")
		err := m.Push(context.Background())
		if err == nil {
			t.Fatal("expected error pushing to unreachable target")
		}
		// No panic — that is the primary assertion.
	})
}

// TestRunnerMetrics_PushNoopWithoutBaseURL verifies that Push is a no-op when
// BaseURL is empty (matches DestroyFailedPusher and PipelineMetrics behavior).
func TestRunnerMetrics_PushNoopWithoutBaseURL(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)
	m.RecordRunnerJob("success")
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("expected nil err when BaseURL empty, got %v", err)
	}
}

// TestRunnerMetrics_PushPostsBody verifies that Push sends the serialized
// metric body to the Pushgateway via HTTP POST.
func TestRunnerMetrics_PushPostsBody(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewRunnerMetrics(srv.URL, "crucible_engine", nil)
	m.RecordRunnerJob("success")
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(gotBody, `crucible_runner_job_total{result="success"} 1`) {
		t.Fatalf("pushed body missing expected sample:\n%s", gotBody)
	}
}

// TestRunnerMetrics_EmptyFamiliesEmitNoSamples verifies that an empty metric
// family still emits its HELP/TYPE header but no sample lines.
func TestRunnerMetrics_EmptyFamiliesEmitNoSamples(t *testing.T) {
	m := NewRunnerMetrics("", "", nil)
	out := string(m.serialize())

	if !strings.Contains(out, "# TYPE crucible_runner_job_total counter") {
		t.Fatal("expected TYPE line for empty crucible_runner_job_total family")
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "crucible_runner_job_total{") {
			t.Fatalf("empty family emitted a sample: %q", line)
		}
	}
}

// TestRunnerMetrics_RunPusherActuallyPushes proves the flush loop transmits.
// Every Record*/Observe* method only mutates in-process counters, so without a
// working pusher the entire crucible_runner_* family is absent from Prometheus
// -- and an absent series looks exactly like a zero one on a dashboard. This
// test is the guard that the loop is wired and reaches the network.
func TestRunnerMetrics_RunPusherActuallyPushes(t *testing.T) {
	bodies := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case bodies <- string(b):
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := NewRunnerMetrics(srv.URL, "crucible_engine", nil)
	m.RecordCleanupFailure("secret")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.RunPusher(ctx, 5*time.Millisecond, nil)

	select {
	case body := <-bodies:
		if !strings.Contains(body, "crucible_runner_cleanup_failed_total") {
			t.Errorf("pushed body missing the recorded metric:\n%s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunPusher never pushed; recorded metrics would never reach Prometheus")
	}
}

// TestRunnerMetrics_RunPusherNoopWithoutBaseURL proves the loop exits
// immediately when metrics are disabled rather than spinning forever or
// dialing a non-existent gateway. main() calls this unconditionally on a
// possibly-nil recorder, so it must also survive a nil receiver.
func TestRunnerMetrics_RunPusherNoopWithoutBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *RunnerMetrics
	}{
		{"nil receiver", nil},
		{"empty base url", NewRunnerMetrics("", "", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.m.RunPusher(context.Background(), time.Millisecond, nil)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("RunPusher did not return immediately when disabled")
			}
		})
	}
}
