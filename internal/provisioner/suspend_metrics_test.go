package provisioner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestSuspendMetrics_LastRunSeededAtConstruction is the regression test for a
// false-positive alert: crucible_idle_evaluator_last_run_timestamp was pushed
// as 0 before the evaluator's first tick, so
// `time() - gauge > 1h` fired on EVERY worker restart and stayed lit until the
// first pass -- up to a full 15m interval after each deploy.
//
// The gauge must be present (an absent series can never satisfy a "too old"
// alert, so dropping it would be worse) AND recent at construction.
func TestSuspendMetrics_LastRunSeededAtConstruction(t *testing.T) {
	before := time.Now().Add(-2 * time.Second)
	m := NewSuspendMetrics("", "crucible_test", nil)
	after := time.Now().Add(2 * time.Second)

	body := string(m.serialize())

	const name = "crucible_idle_evaluator_last_run_timestamp"
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, name+" ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("%s is absent from the push body; a \"value too old\" alert cannot "+
			"match an absent series, so it must always be emitted.\nbody:\n%s", name, body)
	}

	var got float64
	if _, err := fmt.Sscanf(line, name+" %g", &got); err != nil {
		t.Fatalf("could not parse %q: %v", line, err)
	}
	if got < float64(before.Unix()) || got > float64(after.Unix()) {
		t.Errorf("%s = %v, want a fresh timestamp in [%d, %d]. A zero/stale value makes "+
			"CrucibleIdleEvaluatorStale fire on every worker restart.",
			name, got, before.Unix(), after.Unix())
	}
}

// TestRunSuspendMetricsPusher_LeaderGated is the regression test for the
// multi-replica clobber bug: every worker replica ran the pusher against the
// same Pushgateway grouping key, but only the leader advances the last-run
// timestamp. Non-leader replicas kept overwriting the leader's fresh heartbeat
// with their frozen process-start seed, so crucible_idle_evaluator_last_run_timestamp
// never advanced and CrucibleIdleEvaluatorStale fired permanently while the
// evaluator was healthy. A non-leader must therefore never push.
func TestRunSuspendMetricsPusher_LeaderGated(t *testing.T) {
	var pushes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushes.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	t.Run("non-leader never pushes", func(t *testing.T) {
		pushes.Store(0)
		m := NewSuspendMetrics(srv.URL, "crucible_test", nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			m.RunSuspendMetricsPusher(ctx, 5*time.Millisecond, func() bool { return false }, nil)
			close(done)
		}()
		time.Sleep(60 * time.Millisecond) // ~12 ticks would have elapsed
		cancel()                          // triggers the shutdown flush path too
		<-done
		if n := pushes.Load(); n != 0 {
			t.Fatalf("non-leader pushed %d times; must be 0 (including the shutdown flush)", n)
		}
	})

	t.Run("leader pushes", func(t *testing.T) {
		pushes.Store(0)
		m := NewSuspendMetrics(srv.URL, "crucible_test", nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			m.RunSuspendMetricsPusher(ctx, 5*time.Millisecond, func() bool { return true }, nil)
			close(done)
		}()
		time.Sleep(60 * time.Millisecond)
		cancel()
		<-done
		if n := pushes.Load(); n == 0 {
			t.Fatal("leader never pushed; want at least one push")
		}
	})

	t.Run("nil isLeader pushes (single-replica/back-compat)", func(t *testing.T) {
		pushes.Store(0)
		m := NewSuspendMetrics(srv.URL, "crucible_test", nil)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			m.RunSuspendMetricsPusher(ctx, 5*time.Millisecond, nil, nil)
			close(done)
		}()
		time.Sleep(60 * time.Millisecond)
		cancel()
		<-done
		if n := pushes.Load(); n == 0 {
			t.Fatal("nil isLeader must behave as always-push; want at least one push")
		}
	})
}
