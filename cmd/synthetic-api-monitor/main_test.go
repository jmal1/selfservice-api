package main

import (
	"strings"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// names builds a check set carrying only names — the timeout-envelope logic
// dispatches on Name() alone, so nothing else needs to be real.
func names(ns ...string) []synthetic.Check {
	out := make([]synthetic.Check, 0, len(ns))
	for _, n := range ns {
		out = append(out, synthetic.CheckFunc{NameVal: n})
	}
	return out
}

// TestLifecycleSafeTimeout_TakesTheLongestEnvelopeRegardlessOfOrder is the
// regression guard for an ordering-dependent bug: the original implementation
// returned on the FIRST check whose envelope exceeded the base, so a check set
// containing both pod_lifecycle and runner_smoke would hand runner_smoke an
// 11-minute budget for a 21-minute job.
//
// The consequence is nastier than a plain timeout. runner_smoke would fail
// every single cycle with "context deadline exceeded" — indistinguishable
// from a genuinely broken Kali runner — and would send someone to k3sv03,
// Multus and the image pull for a bug that lives in this file.
func TestLifecycleSafeTimeout_TakesTheLongestEnvelopeRegardlessOfOrder(t *testing.T) {
	base := 30 * time.Second

	tests := []struct {
		name   string
		checks []synthetic.Check
		want   time.Duration
	}{
		{"no expensive checks keeps the base", names("healthz", "auth_me"), base},
		{"pod_lifecycle alone", names("healthz", "pod_lifecycle"), 11 * time.Minute},
		{"runner_smoke alone", names("runner_smoke"), 21 * time.Minute},
		{
			// Ordering guard: lifecycle first would have short-circuited.
			"both, lifecycle registered first",
			names("healthz", "pod_lifecycle", "runner_smoke"),
			21 * time.Minute,
		},
		{
			"both, runner registered first",
			names("runner_smoke", "pod_lifecycle"),
			21 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lifecycleSafeTimeout(base, tt.checks); got != tt.want {
				t.Errorf("lifecycleSafeTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLifecycleSafeTimeout_NeverShrinksAnExplicitBase guards the other
// direction: an operator who deliberately raised SYNTHETIC_CHECK_TIMEOUT must
// not have it silently clamped back down to a hardcoded envelope.
func TestLifecycleSafeTimeout_NeverShrinksAnExplicitBase(t *testing.T) {
	base := 45 * time.Minute
	if got := lifecycleSafeTimeout(base, names("pod_lifecycle", "runner_smoke")); got != base {
		t.Errorf("lifecycleSafeTimeout() = %v, want the larger explicit base %v", got, base)
	}
}

// TestResolvePushLayer_RunnerModeCannotClobberTheApiGrouping is the guard for
// a destructive misconfiguration.
//
// Pushgateway replaces a whole metric family within a grouping on POST, and
// runner mode registers ONLY runner_smoke. Sharing the "api" grouping would
// therefore delete the other api-layer check series. Because every alert is
// shaped `1 - crucible_synthetic_check_success > 0` and cannot match an
// ABSENT series, the dashboards would go green rather than red — monitoring
// would be destroyed silently, which is strictly worse than it going down.
func TestResolvePushLayer_RunnerModeCannotClobberTheApiGrouping(t *testing.T) {
	t.Run("runner mode defaults to its own grouping", func(t *testing.T) {
		got, err := resolvePushLayer(true, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == mainLayer {
			t.Fatalf("runner mode defaulted to the main grouping %q; its single-check "+
				"push would delete every other api-layer series", mainLayer)
		}
		if got != runnerLayer {
			t.Errorf("resolvePushLayer(true, \"\") = %q, want %q", got, runnerLayer)
		}
	})

	t.Run("runner mode REFUSES an explicit api layer", func(t *testing.T) {
		_, err := resolvePushLayer(true, mainLayer)
		if err == nil {
			t.Fatalf("resolvePushLayer(true, %q) returned no error; this configuration "+
				"silently deletes the main monitor's check series", mainLayer)
		}
		// The message has to name the consequence, or whoever hits it at
		// 2am will just override the layer and re-break it.
		if !strings.Contains(err.Error(), "delete") {
			t.Errorf("error does not explain the consequence: %v", err)
		}
	})

	t.Run("runner mode honours a distinct explicit layer", func(t *testing.T) {
		got, err := resolvePushLayer(true, "runner-canary")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "runner-canary" {
			t.Errorf("resolvePushLayer() = %q, want %q", got, "runner-canary")
		}
	})

	t.Run("normal mode is unchanged", func(t *testing.T) {
		got, err := resolvePushLayer(false, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != mainLayer {
			t.Errorf("resolvePushLayer(false, \"\") = %q, want %q", got, mainLayer)
		}

		got, err = resolvePushLayer(false, "janitor")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "janitor" {
			t.Errorf("resolvePushLayer(false, \"janitor\") = %q, want %q", got, "janitor")
		}
	})
}