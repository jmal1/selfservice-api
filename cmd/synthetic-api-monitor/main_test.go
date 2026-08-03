package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
	"github.com/jmal1/selfservice-api/internal/synthetic/checks"
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

// TestResolveRunnerConfig_MissingEnvWarnsButNeverFails is the T1-5 regression
// guard, and it is the most important test in this file.
//
// The tempting implementation returns an error when SYNTHETIC_RUNNER_TEMPLATE
// or SYNTHETIC_RUNNER_PLAYLIST_ID is missing — "fail fast on bad config" is
// normally right. Here it is exactly wrong, and the reason is not obvious
// from reading the function.
//
// An unregistered check does not go stale; its Prometheus series ceases to
// exist. PushResults POSTs the whole crucible_synthetic_check_success family
// each cycle and Pushgateway replaces a family wholesale on POST. Every alert
// we have is shaped `1 - crucible_synthetic_check_success > 0`, which cannot
// match an ABSENT series.
//
// So an early return would leave the entire Epic D path — engine dispatch,
// Multus, macvlan DHCP, the Kali image pull, the runner callback — INVISIBLE
// to alerting rather than red. And because Pushgateway retains the last
// pushed value indefinitely, a CronJob that begins failing this way leaves a
// stale-but-GREEN series behind it. Silent loss of coverage is strictly worse
// than an outage, because nothing ever tells you.
//
// This has already bitten twice (the elevated checks in T1-5, and the
// SYNTHETIC_LIFECYCLE_ENABLED gate). Hence a test rather than a comment.
func TestResolveRunnerConfig_MissingEnvWarnsButNeverFails(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantWarnEnvs []string
	}{
		{
			"nothing configured at all",
			map[string]string{},
			[]string{"SYNTHETIC_RUNNER_TEMPLATE", "SYNTHETIC_RUNNER_PLAYLIST_ID"},
		},
		{
			"template set, playlist missing",
			map[string]string{"SYNTHETIC_RUNNER_TEMPLATE": "synthetic-noop"},
			[]string{"SYNTHETIC_RUNNER_PLAYLIST_ID"},
		},
		{
			"playlist set, template missing",
			map[string]string{"SYNTHETIC_RUNNER_PLAYLIST_ID": "0000-uuid"},
			[]string{"SYNTHETIC_RUNNER_TEMPLATE"},
		},
		{
			"fully configured warns about nothing",
			map[string]string{
				"SYNTHETIC_RUNNER_TEMPLATE":    "synthetic-noop",
				"SYNTHETIC_RUNNER_PLAYLIST_ID": "0000-uuid",
			},
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, warnings, err := resolveRunnerConfig(func(k string) string { return tt.env[k] })

			if err != nil {
				t.Fatalf("resolveRunnerConfig returned error %v.\n"+
					"Missing config MUST NOT stop runner_smoke being registered: an "+
					"absent series cannot match `1 - crucible_synthetic_check_success > 0`, "+
					"so the Epic D path would be invisible rather than red.", err)
			}

			var gotEnvs []string
			for _, w := range warnings {
				gotEnvs = append(gotEnvs, w.env)
				if w.msg == "" {
					t.Errorf("warning for %s has an empty message", w.env)
				}
			}
			if strings.Join(gotEnvs, ",") != strings.Join(tt.wantWarnEnvs, ",") {
				t.Errorf("warned about %v, want %v", gotEnvs, tt.wantWarnEnvs)
			}

			// The config must still be usable enough to construct a check,
			// so registration can proceed and the RunFn can report the failure.
			if cfg.ReadyTimeout <= 0 || cfg.RunTimeout <= 0 || cfg.DestroyTimeout <= 0 {
				t.Errorf("timeouts must keep their defaults so the check can run and fail "+
					"cleanly: ready=%v run=%v destroy=%v",
					cfg.ReadyTimeout, cfg.RunTimeout, cfg.DestroyTimeout)
			}
		})
	}
}

// TestResolveRunnerConfig_RejectsUnparseableDurations is the deliberate
// counterpart: a typo'd duration IS a hard error.
//
// The distinction is not arbitrary. A missing template/playlist can be
// reported through a check result, so it becomes a red series. A malformed
// duration cannot — it would silently fall back to a default, and the
// operator who set SYNTHETIC_RUNNER_RUN_TIMEOUT=10 (no unit) would never
// learn their value was ignored.
func TestResolveRunnerConfig_RejectsUnparseableDurations(t *testing.T) {
	for _, key := range []string{
		"SYNTHETIC_RUNNER_READY_TIMEOUT",
		"SYNTHETIC_RUNNER_RUN_TIMEOUT",
		"SYNTHETIC_RUNNER_DESTROY_TIMEOUT",
	} {
		t.Run(key, func(t *testing.T) {
			env := map[string]string{
				"SYNTHETIC_RUNNER_TEMPLATE":    "synthetic-noop",
				"SYNTHETIC_RUNNER_PLAYLIST_ID": "0000-uuid",
				key:                            "10", // missing unit
			}
			_, _, err := resolveRunnerConfig(func(k string) string { return env[k] })
			if err == nil {
				t.Fatalf("%s=%q must be rejected; silently falling back to the default "+
					"means the operator's value is ignored with no signal", key, "10")
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q must name the offending env var", err)
			}
		})
	}
}

// TestResolveRunnerConfig_AppliesValidOverrides guards that the override path
// is actually wired, not just validated. A parse that succeeds but is never
// assigned is the dead-wiring pattern that has bitten this repo seven times.
func TestResolveRunnerConfig_AppliesValidOverrides(t *testing.T) {
	env := map[string]string{
		"SYNTHETIC_RUNNER_TEMPLATE":        "synthetic-noop",
		"SYNTHETIC_RUNNER_PLAYLIST_ID":     "0000-uuid",
		"SYNTHETIC_RUNNER_READY_TIMEOUT":   "3m",
		"SYNTHETIC_RUNNER_RUN_TIMEOUT":     "7m",
		"SYNTHETIC_RUNNER_DESTROY_TIMEOUT": "45s",
	}
	cfg, warnings, err := resolveRunnerConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("fully configured input warned: %v", warnings)
	}
	if cfg.ReadyTimeout != 3*time.Minute {
		t.Errorf("ReadyTimeout = %v, want 3m — override parsed but never assigned", cfg.ReadyTimeout)
	}
	if cfg.RunTimeout != 7*time.Minute {
		t.Errorf("RunTimeout = %v, want 7m — override parsed but never assigned", cfg.RunTimeout)
	}
	if cfg.DestroyTimeout != 45*time.Second {
		t.Errorf("DestroyTimeout = %v, want 45s — override parsed but never assigned", cfg.DestroyTimeout)
	}
	if cfg.TemplateName != "synthetic-noop" || cfg.PlaylistID != "0000-uuid" {
		t.Errorf("template/playlist not carried into config: %+v", cfg)
	}
}

// TestResolveMode_RunnerModeIsNotShadowedByLifecycle is the regression guard
// for a defect that shipped and was only caught by running the thing for real.
//
// SYNTHETIC_LIFECYCLE_ENABLED=true is set once, globally, in values.yaml. Any
// CronJob built from the shared env block inherits it. The mode selection used
// to be an ordered if/else chain that tested lifecycle BEFORE runner mode, so
// a runner CronJob deployed exactly as designed silently registered the
// ordinary api check set instead of runner_smoke.
//
// The failure was invisible by construction: resolvePushLayer DOES honour
// runner mode, so the push grouping said layer="runner" while the checks were
// the api set. A brand-new, entirely green "runner" layer would have appeared
// on the dashboard while the Kali-runner path was never exercised — and no
// alert can fire for a series that does not exist.
func TestResolveMode_RunnerModeIsNotShadowedByLifecycle(t *testing.T) {
	// Exactly the env a runner CronJob gets when it inherits the shared block.
	env := map[string]string{
		"SYNTHETIC_LIFECYCLE_ENABLED":  "true",
		"SYNTHETIC_LIFECYCLE_TEMPLATE": "synthetic-noop",
		"SYNTHETIC_RUNNER_MODE":        "true",
		"SYNTHETIC_RUNNER_TEMPLATE":    "synthetic-noop",
		"SYNTHETIC_RUNNER_PLAYLIST_ID": "5e7c0a00-0000-4000-a000-000000000001",
	}
	m, err := resolveMode(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("resolveMode: %v", err)
	}
	if m.kind != modeRunner {
		t.Fatalf("kind = %v, want modeRunner.\n"+
			"SYNTHETIC_LIFECYCLE_ENABLED is inherited from the shared env block by every "+
			"CronJob, so if it can shadow runner mode then the runner CronJob silently "+
			"registers the api check set, pushes it under layer=\"runner\", and "+
			"runner_smoke never runs while the board reads green.", m.kind)
	}
	if m.lifecycleEnabled {
		t.Error("lifecycleEnabled = true in a replacement mode; pod_lifecycle would run " +
			"alongside runner_smoke and the runner grouping would carry a check the " +
			"main monitor already owns")
	}
	if len(m.warnings) == 0 {
		t.Error("no warning emitted for the ignored SYNTHETIC_LIFECYCLE_ENABLED.\n" +
			"Silently ignoring inherited config is how this bug hid in the first place: " +
			"the warning is the line that makes a misconfigured deploy diagnosable from " +
			"the log instead of from a multi-hour investigation.")
	}
}

// TestResolveMode_JanitorAndRunnerTogetherIsRefused asserts the binary refuses
// an ambiguous configuration instead of silently resolving it by declaration
// order. Both flags replace the ENTIRE check set, so quietly picking one means
// the other's checks are absent from Prometheus — and absence cannot match
// `1 - crucible_synthetic_check_success > 0`.
func TestResolveMode_JanitorAndRunnerTogetherIsRefused(t *testing.T) {
	env := map[string]string{
		"SYNTHETIC_JANITOR_MODE": "true",
		"SYNTHETIC_RUNNER_MODE":  "true",
	}
	if _, err := resolveMode(func(k string) string { return env[k] }); err == nil {
		t.Fatal("two replacement modes set at once must be refused, not resolved by " +
			"declaration order")
	}
}

// TestResolveMode_DefaultAndJanitorStillBehave pins the pre-existing behaviour
// so the refactor that introduced resolveMode cannot have changed it.
func TestResolveMode_DefaultAndJanitorStillBehave(t *testing.T) {
	tests := []struct {
		name          string
		env           map[string]string
		wantKind      modeKind
		wantLifecycle bool
	}{
		{
			name:     "nothing set",
			env:      map[string]string{},
			wantKind: modeDefault,
		},
		{
			name:          "lifecycle only",
			env:           map[string]string{"SYNTHETIC_LIFECYCLE_ENABLED": "true"},
			wantKind:      modeDefault,
			wantLifecycle: true,
		},
		{
			name:     "janitor replaces the set",
			env:      map[string]string{"SYNTHETIC_JANITOR_MODE": "true"},
			wantKind: modeJanitor,
		},
		{
			name: "janitor also wins over inherited lifecycle",
			env: map[string]string{
				"SYNTHETIC_JANITOR_MODE":      "true",
				"SYNTHETIC_LIFECYCLE_ENABLED": "true",
			},
			wantKind: modeJanitor,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := resolveMode(func(k string) string { return tt.env[k] })
			if err != nil {
				t.Fatalf("resolveMode: %v", err)
			}
			if m.kind != tt.wantKind {
				t.Errorf("kind = %v, want %v", m.kind, tt.wantKind)
			}
			if m.lifecycleEnabled != tt.wantLifecycle {
				t.Errorf("lifecycleEnabled = %v, want %v", m.lifecycleEnabled, tt.wantLifecycle)
			}
		})
	}
}

// TestSessionTokenTTL_OutlivesEveryCheckBudget guards the defect found by D2
// step 5: the session JWT was minted for checkTimeout*(len(checks.All())+1)
// == 4m30s while lifecycleSafeTimeout granted runner_smoke a 21-minute budget,
// so a slow or hung runner failed with "401 unauthorized" instead of its real
// error -- pointing the on-call at Authentik rather than the Kali runner.
//
// The invariant: the token must outlive the largest budget the runner will
// actually let a single check consume.
func TestSessionTokenTTL_OutlivesEveryCheckBudget(t *testing.T) {
	cases := []struct {
		name          string
		longest, base time.Duration
		active        int
	}{
		{"runner mode: one check with a 21m budget", 21 * time.Minute, 30 * time.Second, 1},
		{"default mode with pod_lifecycle (11m budget)", 11 * time.Minute, 30 * time.Second, 13},
		{"default mode, no expensive checks", 30 * time.Second, 30 * time.Second, 8},
		{"janitor mode", 30 * time.Second, 30 * time.Second, 1},
		{"degenerate: no active checks", 30 * time.Second, 30 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionTokenTTL(tc.longest, tc.base, tc.active)
			if got <= tc.longest {
				t.Fatalf("TTL %s does not outlive the longest check budget %s: the token "+
					"expires mid-check and the check reports 401 instead of its real failure",
					got, tc.longest)
			}
		})
	}
}

// TestSessionTokenTTL_BeatsTheShippedBug pins the specific production numbers.
// The premise assertions matter: if checks.All() ever grows enough that the old
// formula would have been adequate, this test would pass vacuously.
func TestSessionTokenTTL_BeatsTheShippedBug(t *testing.T) {
	const base = 30 * time.Second
	const runnerBudget = 21 * time.Minute

	buggy := base * time.Duration(len(checks.All())+1)
	if buggy >= runnerBudget {
		t.Fatalf("premise no longer holds: old formula %s already covered the %s runner budget",
			buggy, runnerBudget)
	}
	if got := sessionTokenTTL(runnerBudget, base, 1); got <= buggy {
		t.Fatalf("sessionTokenTTL returned %s, no better than the buggy %s", got, buggy)
	}
}

// TestSessionTokenTTL_IsDerivedFromTheRunnerBudgetNotAllChecks is the wiring
// guard (the W2-7 class): the pure function can be perfectly correct while
// main() still feeds it the wrong inputs. Asserts the call site reads
// runner.CheckTimeout, and that no mint is derived from checks.All() again.
func TestSessionTokenTTL_IsDerivedFromTheRunnerBudgetNotAllChecks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "sessionTokenTTL(runner.CheckTimeout, checkTimeout, len(activeChecks))") {
		t.Error("session TTL is no longer derived from runner.CheckTimeout and the ACTIVE check " +
			"set; a replacement mode (runner/janitor) will mint a token too short for its own budget")
	}
	if strings.Contains(body, "len(checks.All())+1") {
		t.Error("a session token is being minted from checks.All() again: in runner or janitor mode " +
			"that is the wrong check set, and it reintroduces the 4m30s-token-vs-21m-budget bug")
	}
}