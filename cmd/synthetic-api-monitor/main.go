// Binary synthetic-api-monitor is the customer-emulating monitor that exercises
// the live Crucible API on a schedule and pushes results to Prometheus
// Pushgateway. See internal/synthetic/check.go for the framework and
// internal/synthetic/checks/registry.go for the check catalog.
//
// This binary is designed to run in two modes:
//
//   - One-shot (default): run all checks once, push results, exit with 0 even
//     if checks failed. The K8s CronJob schedule (every 10 minutes in prod)
//     drives the cadence. Exit-0-on-check-failure is intentional: a check
//     failure is signaled via the pushed metric, NOT the pod exit status,
//     because K8s would otherwise mark the CronJob as failed and we'd lose
//     the metric push on subsequent restart backoffs.
//
//   - Loop mode (SYNTHETIC_LOOP_INTERVAL set): run forever, sleeping
//     SYNTHETIC_LOOP_INTERVAL between cycles. Useful for local development
//     against a docker-compose stack.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
	"github.com/jmal1/selfservice-api/internal/synthetic/checks"
)

// Environment variables consumed by the binary. Documented here as the source
// of truth; the K8s manifest in deploy/helm/selfservice/templates/synthetic-cronjob.yaml
// MUST stay in sync.
const (
	envBaseURL        = "SYNTHETIC_BASE_URL"        // e.g. https://crucible.jmal.io
	envJWTSecret      = "SYNTHETIC_JWT_SECRET"      // same HMAC the API uses
	envUserID         = "SYNTHETIC_USER_ID"         // UUID of synthetic@lab.jmal.io DB row
	envUsername       = "SYNTHETIC_USERNAME"        // synthetic
	envRole           = "SYNTHETIC_ROLE"            // student (default) | instructor | admin
	envPushgatewayURL = "SYNTHETIC_PUSHGATEWAY_URL" // e.g. http://pushgateway.observability:9091
	envJob            = "SYNTHETIC_JOB"             // pushgateway job label, default crucible_synthetic_api
	envLayer          = "SYNTHETIC_LAYER"           // grouping label `layer`, default api
	envLoopInterval   = "SYNTHETIC_LOOP_INTERVAL"   // optional duration; if set, runs forever
	envCheckTimeout   = "SYNTHETIC_CHECK_TIMEOUT"   // optional duration, default 30s

	// envLifecycleEnabled enables the (expensive) pod_lifecycle check that
	// creates + destroys a real pod. OFF by default so the binary is safe to
	// roll out before SYNTHETIC_LIFECYCLE_TEMPLATE points at a valid row.
	envLifecycleEnabled = "SYNTHETIC_LIFECYCLE_ENABLED"
	// envLifecycleTemplate is the templates.name to clone (NOT the
	// vcenter_template). The synthetic-noop template (see plan §Phase 2) is
	// the intended value in production.
	envLifecycleTemplate = "SYNTHETIC_LIFECYCLE_TEMPLATE"
	// envLifecycleReadyTimeout overrides the default 2m timeout for waiting
	// on PodStatusActive. The default (2m = 3.2× the ~37s observed median)
	// leaves room for a full retry within the 10-minute CronJob schedule.
	envLifecycleReadyTimeout = "SYNTHETIC_LIFECYCLE_READY_TIMEOUT"
	// envLifecycleDestroyTimeout overrides the default 90s timeout for
	// waiting on PodStatusDestroyed.
	envLifecycleDestroyTimeout = "SYNTHETIC_LIFECYCLE_DESTROY_TIMEOUT"
	// envLifecycleMaxAttempts overrides the default 2 attempts for the
	// pod_lifecycle check. Set to 1 to disable retry.
	envLifecycleMaxAttempts = "SYNTHETIC_LIFECYCLE_MAX_ATTEMPTS"
	// envLifecycleRetryBackoff overrides the default 30s pause between
	// pod_lifecycle retry attempts.
	envLifecycleRetryBackoff = "SYNTHETIC_LIFECYCLE_RETRY_BACKOFF"

	// envJanitorMode, when truthy, replaces the entire check set with the
	// single synthetic_janitor check. This is the standalone defense-in-depth
	// sweep that runs from its own (daily) CronJob — its purpose is to
	// destroy orphan synthetic pods regardless of whether the lifecycle
	// check itself is healthy. All non-janitor env vars (base URL,
	// pushgateway, JWT secret, etc.) still apply.
	envJanitorMode = "SYNTHETIC_JANITOR_MODE"
	// envJanitorMaxAge overrides the janitor's orphan-age cutoff. Defaults
	// to 1h. Lower this when investigating accumulation; raise it during
	// long-running manual debugging sessions where you want synthetic pods
	// to stick around.
	envJanitorMaxAge = "SYNTHETIC_JANITOR_MAX_AGE"

	// envRunnerMode, when truthy, replaces the entire check set with the
	// single runner_smoke check. This lets a separate less-frequent CronJob
	// reuse the same image/secrets/pushgateway plumbing as the regular
	// monitor without running the cheap probes the */10 CronJob already
	// covers.
	//
	// If SYNTHETIC_RUNNER_TEMPLATE or SYNTHETIC_RUNNER_PLAYLIST_ID is unset
	// the check is STILL registered, with a warning, and fails loudly at
	// runtime. It must not stop the binary: an unregistered check's series
	// ceases to exist, and every alert is shaped
	// `1 - crucible_synthetic_check_success > 0`, which cannot match an
	// absent series. Exiting would therefore make the Kali-runner path
	// invisible rather than red — and because Pushgateway retains the last
	// pushed value indefinitely, the stale series would keep reading GREEN.
	envRunnerMode = "SYNTHETIC_RUNNER_MODE"
	// envRunnerTemplate is the templates.name to clone (NOT vcenter_template).
	// Getting this wrong causes every runner_smoke run to fail at
	// resolve-template with "template not found", firing every CronJob tick.
	envRunnerTemplate = "SYNTHETIC_RUNNER_TEMPLATE"
	// envRunnerPlaylistID is the UUID of the playlist to run. The playlist
	// endpoint (/api/v1/admin/playlists) requires RoleInstructor, so the
	// student synthetic user cannot resolve a playlist by name at runtime;
	// the UUID must be supplied directly. Getting this wrong means every
	// POST /testing/run returns 400 (bad playlist) and no runner is ever
	// dispatched — the engine dispatch path is unmonitored until fixed.
	envRunnerPlaylistID = "SYNTHETIC_RUNNER_PLAYLIST_ID"
	// envRunnerReadyTimeout overrides the default 2m timeout for waiting on
	// pod active. The default (2m) leaves room for a full retry within the
	// 30-minute runner CronJob schedule.
	envRunnerReadyTimeout = "SYNTHETIC_RUNNER_READY_TIMEOUT"
	// envRunnerRunTimeout overrides the default 10m timeout for waiting on
	// a terminal run state. Increase if Kali runner provisioning or workflow
	// execution is slow; decrease for tests.
	envRunnerRunTimeout = "SYNTHETIC_RUNNER_RUN_TIMEOUT"
	// envRunnerDestroyTimeout overrides the default 90s timeout for waiting
	// on pod destroyed.
	envRunnerDestroyTimeout = "SYNTHETIC_RUNNER_DESTROY_TIMEOUT"
	// envRunnerMaxAttempts overrides the default 2 attempts for runner_smoke.
	// Set to 1 to disable retry.
	envRunnerMaxAttempts = "SYNTHETIC_RUNNER_MAX_ATTEMPTS"
	// envRunnerRetryBackoff overrides the default 30s pause between
	// runner_smoke retry attempts.
	envRunnerRetryBackoff = "SYNTHETIC_RUNNER_RETRY_BACKOFF"

	// envInstructorUserID is the UUID of the dedicated instructor-role row
	// (synthetic-instructor). When set, the monitor mints a SECOND session
	// cookie for it and registers the checks in checks.Elevated().
	//
	// It is deliberately a separate user rather than an elevated role on
	// the primary synthetic user: admin_list_users_403, admin_audit_403,
	// wiki_index_rbac, pod_testing_dashboard_404 and image_upload_rbac all
	// assert that the primary identity is REFUSED. Raising its role would
	// leave all five passing while proving nothing.
	//
	// Unset is a supported configuration — the elevated checks are skipped
	// with a warning, so the binary rolls out safely before the DB row
	// exists.
	envInstructorUserID = "SYNTHETIC_INSTRUCTOR_USER_ID"
	// envInstructorUsername defaults to synthetic-instructor and MUST match
	// the users row, because handlers and the audit log resolve the row by
	// the JWT's user id.
	envInstructorUsername = "SYNTHETIC_INSTRUCTOR_USERNAME"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("synthetic monitor failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	baseURL := mustEnv(envBaseURL)
	jwtSecret := mustEnv(envJWTSecret)
	userID := mustEnv(envUserID)
	username := envOr(envUsername, "synthetic")
	role := envOr(envRole, "student")
	pushgatewayURL := mustEnv(envPushgatewayURL)
	job := envOr(envJob, "crucible_synthetic_api")

	// Resolve the mode ONCE, up front, and derive everything from it. The push
	// grouping and the registered check set must never be able to disagree
	// about which mode this is: a grouping that says layer="runner" while the
	// api check set is registered is exactly the silent failure resolveMode
	// documents.
	mode, err := resolveMode(os.Getenv)
	if err != nil {
		return err
	}
	layer, err := resolvePushLayer(mode.kind == modeRunner, os.Getenv(envLayer))
	if err != nil {
		return err
	}

	checkTimeout := 30 * time.Second
	if v := os.Getenv(envCheckTimeout); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", envCheckTimeout, v, err)
		}
		checkTimeout = d
	}

	// Session cookies are minted AFTER the active check set and the runner's
	// per-check budget are known -- see sessionTokenTTL for why deriving the
	// TTL from checks.All() was a real production defect. The clients are
	// constructed here with an empty cookie and filled in by mintSessions
	// below; nothing between here and that call issues a request.
	client := synthetic.NewClient(baseURL, "")
	pg := synthetic.NewPushgateway(pushgatewayURL, job, map[string]string{
		"layer": layer,
	})

	// Second, elevated identity. See envInstructorUserID for why this is a
	// separate user row rather than a higher role on the primary one.
	instructorID := os.Getenv(envInstructorUserID)
	instructorName := envOr(envInstructorUsername, "synthetic-instructor")
	var instructorClient *synthetic.Client
	if instructorID != "" {
		instructorClient = synthetic.NewClient(baseURL, "")
	}

	// Build the active check list. See resolveMode for why this is not an
	// ordered if/else chain over the raw env vars.
	for _, w := range mode.warnings {
		logger.Warn(w.msg, "env", w.env)
	}

	activeChecks := checks.All()
	switch mode.kind {
	case modeJanitor:
		cfg := checks.DefaultJanitorConfig()
		cfg.Logger = logger.With("component", "synthetic_janitor")
		if v := os.Getenv(envJanitorMaxAge); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("invalid %s=%q: %w", envJanitorMaxAge, v, err)
			}
			cfg.MaxAge = d
		}
		logger.Info("janitor mode: registering synthetic_janitor only",
			"max_age", cfg.MaxAge,
		)
		activeChecks = []synthetic.Check{checks.Janitor(cfg)}

	case modeRunner:
		cfg, warnings, err := resolveRunnerConfig(os.Getenv)
		if err != nil {
			return err
		}
		for _, w := range warnings {
			logger.Warn(w.msg,
				"env", w.env,
				"consequence", runnerUnmonitoredConsequence,
			)
		}
		cfg.Logger = logger.With("component", "runner_smoke")
		logger.Info("runner mode: registering runner_smoke only",
			"template", cfg.TemplateName,
			"playlist_id", cfg.PlaylistID,
			"ready_timeout", cfg.ReadyTimeout,
			"run_timeout", cfg.RunTimeout,
			"destroy_timeout", cfg.DestroyTimeout,
		)
		activeChecks = []synthetic.Check{checks.RunnerSmoke(cfg)}

	case modeDefault:
		if mode.lifecycleEnabled {
			tmpl := os.Getenv(envLifecycleTemplate)
			if tmpl == "" {
				return fmt.Errorf("%s=true but %s is empty", envLifecycleEnabled, envLifecycleTemplate)
			}
			cfg := checks.DefaultPodLifecycleConfig(tmpl)
			cfg.Logger = logger.With("component", "pod_lifecycle")
			if v := os.Getenv(envLifecycleReadyTimeout); v != "" {
				d, err := time.ParseDuration(v)
				if err != nil {
					return fmt.Errorf("invalid %s=%q: %w", envLifecycleReadyTimeout, v, err)
				}
				cfg.ReadyTimeout = d
			}
			if v := os.Getenv(envLifecycleDestroyTimeout); v != "" {
				d, err := time.ParseDuration(v)
				if err != nil {
					return fmt.Errorf("invalid %s=%q: %w", envLifecycleDestroyTimeout, v, err)
				}
				cfg.DestroyTimeout = d
			}
			logger.Info("registering pod_lifecycle check",
				"template", tmpl,
				"ready_timeout", cfg.ReadyTimeout,
				"destroy_timeout", cfg.DestroyTimeout,
			)
			activeChecks = append(activeChecks, checks.PodLifecycle(cfg))
		}
	}

	// Elevated (instructor-role) checks. Skipped loudly rather than
	// silently: without them the admin surface is covered only by the
	// student-side 403 assertions, which cannot distinguish "the route
	// works" from "the route 503s because a dependency was never wired".
	// Also skipped in runner mode: that CronJob is a dedicated runner sweep
	// and elevated checks are already covered by the main */10 monitor.
	if !envBool(envJanitorMode) && !envBool(envRunnerMode) {
		elevatedCfg := checks.ElevatedConfig{Client: instructorClient}

		// Registered UNCONDITIONALLY, and deliberately outside the
		// if/else below. A log line is not monitoring: nothing alerts on
		// the absence of a Prometheus series, so skipping the elevated
		// checks would drop coverage while the board stayed green. This
		// emits a series that is always present and goes to 0 instead.
		activeChecks = append(activeChecks, checks.ElevatedIdentityConfigured(elevatedCfg))

		if instructorClient != nil {
			elevated := checks.Elevated(elevatedCfg)
			names := make([]string, 0, len(elevated))
			for _, c := range elevated {
				names = append(names, c.Name())
			}
			logger.Info("registering elevated checks",
				"instructor_user", instructorName,
				"checks", strings.Join(names, ","),
			)
			activeChecks = append(activeChecks, elevated...)
		} else {
			logger.Warn("elevated checks DISABLED: no instructor identity configured",
				"env", envInstructorUserID,
				"consequence", "the authenticated admin surface (/admin/images, /admin/vcenter/isos, wizard-state) is unmonitored; a 503 from an unwired dependency will look identical to a healthy deploy",
				"alert", "elevated_identity_configured will report 0 until this is fixed",
			)
		}
	}

	runner := synthetic.NewRunner(client, pg, activeChecks, logger)
	// pod_lifecycle and runner_smoke need their own timeout budget AND retry
	// policies. lifecycleSafeTimeout picks the largest per-attempt envelope.
	runner.CheckTimeout = lifecycleSafeTimeout(checkTimeout, activeChecks)

	// Wire retry policies for the expensive, vCenter-dependent checks.
	// Cheap contract checks (403, 404, healthz) are deliberately excluded:
	// retrying them would mask real regressions instead of catching them.
	if mode.kind == modeDefault {
		if hasPodLifecycle(activeChecks) {
			retryCfg, err := resolveRetryConfig(os.Getenv, envLifecycleMaxAttempts, envLifecycleRetryBackoff)
			if err != nil {
				return err
			}
			runner.SetRetry("pod_lifecycle", retryCfg)
			logger.Info("pod_lifecycle retry policy",
				"max_attempts", retryCfg.MaxAttempts,
				"backoff", retryCfg.Backoff,
			)
		}
	}
	if mode.kind == modeRunner {
		retryCfg, err := resolveRetryConfig(os.Getenv, envRunnerMaxAttempts, envRunnerRetryBackoff)
		if err != nil {
			return err
		}
		runner.SetRetry("runner_smoke", retryCfg)
		logger.Info("runner_smoke retry policy",
			"max_attempts", retryCfg.MaxAttempts,
			"backoff", retryCfg.Backoff,
		)
	}

	// Mint the session JWTs now that the real per-cycle budget is known. A
	// fresh pair is minted each cycle so a token leaked from a single run
	// cannot be used long.
	sessionTTL := sessionTokenTTL(runner.CheckTimeout, checkTimeout, len(activeChecks))
	mintSessions := func() error {
		c, err := synthetic.MintSessionToken([]byte(jwtSecret), userID, username, role, sessionTTL)
		if err != nil {
			return fmt.Errorf("mint session token: %w", err)
		}
		client.SessionCookie = c
		if instructorClient != nil {
			ic, err := synthetic.MintSessionToken([]byte(jwtSecret), instructorID, instructorName,
				"instructor", sessionTTL)
			if err != nil {
				return fmt.Errorf("mint instructor session token: %w", err)
			}
			// Elevated checks hold this pointer, so mutating in place
			// refreshes them too.
			instructorClient.SessionCookie = ic
		}
		return nil
	}
	if err := mintSessions(); err != nil {
		return err
	}
	logger.Info("session tokens minted",
		"ttl", sessionTTL,
		"longest_check_budget", runner.CheckTimeout,
		"active_checks", len(activeChecks),
	)

	// Phase 4: probe /auth/me once at startup so a stale synthetic UUID
	// (i.e. the secret's user-id no longer matches a row in users) surfaces
	// as a single clear warning instead of cascading 404s. Other checks
	// continue regardless so a temporary DB blip on this one probe does not
	// suppress the rest of the run.
	probeSyntheticUser(client, logger)

	// Handle SIGTERM cleanly so a CronJob delete doesn't drop in-flight pushes.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	loopInterval := 0 * time.Second
	if v := os.Getenv(envLoopInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid %s=%q: %w", envLoopInterval, v, err)
		}
		loopInterval = d
	}

	for {
		results := runner.RunOnce(ctx)
		failed := 0
		for _, r := range results {
			if !r.Success {
				failed++
			}
		}
		logger.Info("synthetic cycle complete",
			"checks", len(results),
			"failed", failed,
		)
		if loopInterval == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(loopInterval):
		}
		if err := mintSessions(); err != nil {
			return err
		}
	}
}

// sessionTokenTTL returns how long the synthetic session JWT must stay valid to
// cover one full check cycle.
//
// It MUST be derived from the timeouts the runner actually enforces, not from
// the default check set. This used to be minted as
//
//	checkTimeout * (len(checks.All()) + 1)  ==  30s * 9  ==  4m30s
//
// while lifecycleSafeTimeout grants runner_smoke a 21-minute budget. Any
// runner_smoke run exceeding 4m30s therefore died with "401 unauthorized"
// instead of its real error.
//
// That is the worst possible failure mode for this particular check. A hung or
// dead Kali runner is precisely what runner_smoke exists to detect, and a cold
// ~3GB image pull on k3sv03 legitimately takes minutes -- so both the real
// incident and the benign-but-slow case reported an auth error, sending the
// on-call to Authentik and the JWT secret instead of to the runner.
// pod_lifecycle has the same shape: an 11-minute budget against the same token.
//
// The envelope is "the single most expensive check runs its full budget, and
// every other check takes the base timeout", plus a margin for HTTP overhead,
// pre-clean and the Pushgateway write. It stays bounded by the work it
// authorizes, so a leaked token still expires promptly.
func sessionTokenTTL(longestCheck, base time.Duration, activeChecks int) time.Duration {
	if activeChecks < 1 {
		activeChecks = 1
	}
	return longestCheck + base*time.Duration(activeChecks) + 2*time.Minute
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "FATAL: %s is required\n", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool returns true for "1", "true", "yes" (case-insensitive).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// runnerUnmonitoredConsequence spells out, in the log line itself, what is
// left unmonitored when runner_smoke is misconfigured. Whoever reads this at
// 2am should not have to open the source to find out what they lost.
const runnerUnmonitoredConsequence = "engine dispatch through Multus macvlan DHCP to Kali image pull is unmonitored until this is set"

// configWarning is a non-fatal configuration problem: the check still gets
// registered, it just cannot pass.
type configWarning struct {
	env string
	msg string
}

// resolveRunnerConfig builds the runner_smoke config from the environment.
//
// It returns WARNINGS, not errors, for missing template/playlist. That
// distinction is the whole point of this function and it is load-bearing:
//
// An unregistered check does not go stale — its Prometheus series ceases to
// exist. PushResults POSTs the whole crucible_synthetic_check_success family
// each cycle and Pushgateway replaces a family wholesale on POST. Every alert
// we have is shaped `1 - crucible_synthetic_check_success > 0`, which cannot
// match an ABSENT series.
//
// So if this returned an error and the binary exited, a misconfigured deploy
// would leave the entire Epic D path (engine dispatch → Multus → macvlan DHCP
// → Kali image pull → callback) INVISIBLE to alerting rather than red. Worse,
// Pushgateway retains the last pushed value indefinitely, so a CronJob that
// starts failing this way leaves a stale-but-GREEN series behind it.
//
// Instead the check is always registered and its RunFn fails on every tick
// with a message naming the missing env var — red, obvious, and self-describing.
//
// Genuinely unparseable durations DO return an error: those are typos in
// otherwise-present config, they cannot be reported through a check result,
// and failing fast is the only way to surface them.
//
// getenv is injected so this is testable without mutating process state.
func resolveRunnerConfig(getenv func(string) string) (checks.RunnerSmokeConfig, []configWarning, error) {
	var warnings []configWarning

	tmpl := getenv(envRunnerTemplate)
	if tmpl == "" {
		warnings = append(warnings, configWarning{
			env: envRunnerTemplate,
			msg: "SYNTHETIC_RUNNER_TEMPLATE is empty: runner_smoke will report failure every tick",
		})
	}
	playlistID := getenv(envRunnerPlaylistID)
	if playlistID == "" {
		warnings = append(warnings, configWarning{
			env: envRunnerPlaylistID,
			msg: "SYNTHETIC_RUNNER_PLAYLIST_ID is empty: runner_smoke will report failure every tick",
		})
	}

	cfg := checks.DefaultRunnerSmokeConfig(tmpl, playlistID)

	for _, o := range []struct {
		env    string
		target *time.Duration
	}{
		{envRunnerReadyTimeout, &cfg.ReadyTimeout},
		{envRunnerRunTimeout, &cfg.RunTimeout},
		{envRunnerDestroyTimeout, &cfg.DestroyTimeout},
	} {
		v := getenv(o.env)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, warnings, fmt.Errorf("invalid %s=%q: %w", o.env, v, err)
		}
		*o.target = d
	}

	return cfg, warnings, nil
}

// mainLayer is the Pushgateway grouping used by the primary */10 monitor,
// which registers the full check catalog.
const mainLayer = "api"

// runnerLayer is the grouping used by the dedicated runner_smoke CronJob.
const runnerLayer = "runner"

// resolvePushLayer picks the Pushgateway grouping label for this process.
//
// This is a destructive-action guard, not a preference. Pushgateway REPLACES
// an entire metric family within a grouping on POST (see PushResults), and
// runner mode registers ONLY runner_smoke. If it shared the main "api"
// grouping, its push would DELETE the other api-layer check series outright.
//
// That is the T1-5 failure mode in its most damaging form: every alert we
// have is shaped `1 - crucible_synthetic_check_success > 0`, which cannot
// match a series that is absent. The board would read a clean green while the
// entire API surface had silently stopped being tested.
//
// Defaulting per-mode makes the ordinary deploy correct without anyone having
// to remember an extra env var; refusing an explicit collision covers the
// case where someone sets it by hand.
func resolvePushLayer(runnerMode bool, explicit string) (string, error) {
	if !runnerMode {
		if explicit == "" {
			return mainLayer, nil
		}
		return explicit, nil
	}

	layer := explicit
	if layer == "" {
		layer = runnerLayer
	}
	if layer == mainLayer {
		return "", fmt.Errorf(
			"%s=true with %s=%q would push a single-check family into the main %q "+
				"grouping and delete every other %s-layer check series; use a distinct "+
				"layer (default %q)",
			envRunnerMode, envLayer, layer, mainLayer, mainLayer, runnerLayer)
	}
	return layer, nil
}

// WorstCaseCycle computes the maximum wall-clock time a synthetic check can
// consume in one CronJob pod when all retry attempts are exhausted.
//
// perAttempt is the sum of all per-attempt timeouts for the check being
// measured. For pod_lifecycle that is readyTimeout + destroyTimeout. For
// runner_smoke it is readyTimeout + runTimeout + destroyTimeout. overhead
// (60 s) covers pre-clean HTTP round-trips, Pushgateway push, and logging;
// it is fixed and intentionally not caller-configurable so the guard cannot
// be silently weakened.
//
// The result must stay below the CronJob schedule. When concurrencyPolicy is
// Forbid, a run that exceeds the schedule causes the NEXT cycle to be
// silently skipped — during exactly the vCenter degradation that the retry
// facility exists to absorb.
func WorstCaseCycle(perAttempt, backoff time.Duration, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	const overhead = 60 * time.Second
	return time.Duration(attempts)*perAttempt +
		time.Duration(attempts-1)*backoff + overhead
}

// lifecycleSafeTimeout returns a CheckTimeout that fits the most expensive
// registered check per single attempt. pod_lifecycle can legitimately run for
// up to ~3.5 minutes and runner_smoke for ~14.5 minutes per attempt (with
// readyTimeout=150s for lifecycle, readyTimeout=2m + runTimeout=10m for
// runner); the per-check timeout MUST exceed those per-attempt sums or checks
// will always fail mid-run.
//
// The retry loop itself is NOT accounted for here — the runner's retry loop
// in runOne calls execOnce repeatedly, each with this CheckTimeout as the
// per-attempt ceiling.
//
// It takes the MAXIMUM envelope over every registered check rather than
// returning on the first match. Returning early makes the result depend on
// registration order, so a future change that registers pod_lifecycle and
// runner_smoke together would hand runner_smoke an inadequate budget.
func lifecycleSafeTimeout(base time.Duration, all []synthetic.Check) time.Duration {
	// Per-attempt envelopes (= readyTimeout + destroyTimeout + ~60s overhead,
	// or readyTimeout + runTimeout + destroyTimeout + ~60s for runner_smoke).
	// pod_lifecycle: 150s + 90s + 60s = 300s = 5 min
	// runner_smoke:  120s + 600s + 90s + 60s = 870s ≈ 15 min
	envelopes := map[string]time.Duration{
		"pod_lifecycle": 5 * time.Minute,
		"runner_smoke":  15 * time.Minute,
	}

	longest := base
	for _, c := range all {
		if e, ok := envelopes[c.Name()]; ok && e > longest {
			longest = e
		}
	}
	return longest
}

// hasPodLifecycle reports whether the active check list contains pod_lifecycle.
func hasPodLifecycle(all []synthetic.Check) bool {
	for _, c := range all {
		if c.Name() == "pod_lifecycle" {
			return true
		}
	}
	return false
}

// resolveRetryConfig reads max-attempts and backoff from the environment,
// falling back to DefaultRetryConfig values. Returns an error only for
// unparseable durations — missing values use the defaults.
func resolveRetryConfig(getenv func(string) string, attemptsEnv, backoffEnv string) (synthetic.RetryConfig, error) {
	cfg := synthetic.DefaultRetryConfig()
	if v := getenv(attemptsEnv); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("invalid %s=%q: must be a positive integer", attemptsEnv, v)
		}
		cfg.MaxAttempts = n
	}
	if v := getenv(backoffEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid %s=%q: %w", backoffEnv, v, err)
		}
		cfg.Backoff = d
	}
	return cfg, nil
}

// probeSyntheticUser hits /auth/me once at startup. A 404 means the
// SYNTHETIC_USER_ID secret no longer matches a row in users (most commonly
// because someone re-created the synthetic user via the UI or OIDC). Logging
// it as a structured warning with a runbook URL is the simplest defensive
// fix; the AuthMe check will still record its own failure metric.
func probeSyntheticUser(client *synthetic.Client, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := client.Do(ctx, http.MethodGet, "/auth/me", nil)
	if err != nil {
		logger.Warn("startup auth probe failed",
			"error", err,
			"note", "monitor will continue; AuthMe check will report cleanly",
		)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		logger.Warn("synthetic user secret out of sync",
			"status", 404,
			"hint", "SYNTHETIC_USER_ID no longer matches a row in users; rotate the secret to current UUID",
			"runbook", "https://github.com/jmal1/Homelab/blob/master/future/Synthetic-Monitoring.md#synthetic-user-secret-out-of-sync",
		)
		return
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn("startup auth probe unexpected status",
			"status", resp.StatusCode,
		)
	}
}

// (Used by tests of binary-level helpers.)
var _ = strconv.Itoa

// modeKind is which check set the binary registers this run.
type modeKind int

const (
	// modeDefault registers checks.All(), optionally plus pod_lifecycle.
	modeDefault modeKind = iota
	// modeJanitor replaces the set with synthetic_janitor only.
	modeJanitor
	// modeRunner replaces the set with runner_smoke only.
	modeRunner
)

// resolvedMode is the outcome of reading the mode env vars.
type resolvedMode struct {
	kind             modeKind
	lifecycleEnabled bool
	warnings         []configWarning
}

// resolveMode decides which check set to register.
//
// WHY THIS IS NOT AN ORDERED if/else CHAIN OVER THE RAW ENV VARS
//
// It used to be, and that shipped a live defect that this function exists to
// make impossible.
//
// SYNTHETIC_LIFECYCLE_ENABLED=true is set once, globally, in values.yaml,
// because the */10 monitor wants it. Any new CronJob built from the same env
// block inherits it. The old chain tested lifecycle BEFORE runner mode, so a
// runner CronJob deployed exactly as intended would silently fall into the
// lifecycle branch: SYNTHETIC_RUNNER_MODE was read, found true, and then never
// acted on.
//
// The result was not a crash or an error. It was worse:
//
//   - resolvePushLayer DOES honour runner mode, so the push grouping would say
//     layer="runner".
//   - The checks actually registered would be the ordinary api set.
//   - runner_smoke would never run, and its series would never exist.
//
// So a brand-new "runner" layer would appear on the board, entirely green,
// while the thing it was created to monitor — engine dispatch through Multus
// macvlan DHCP to the Kali image pull — was never exercised at all. Every
// alert is shaped `1 - crucible_synthetic_check_success > 0` and cannot match
// an absent series, so nothing would ever have said so.
//
// Two properties fix that class of bug rather than this one instance:
//
//  1. The REPLACEMENT modes (janitor, runner) are resolved first and are
//     mutually exclusive. Setting both is refused outright rather than
//     resolved by declaration order, because "whichever I wrote first wins" is
//     not a contract anyone can hold in their head.
//  2. SYNTHETIC_LIFECYCLE_ENABLED is an ADDITIVE flag that only applies to the
//     default mode, and being ignored by a replacement mode is WARNED about
//     rather than shadowed silently. That warning is the line that would have
//     turned this multi-hour investigation into a five-second one.
func resolveMode(getenv func(string) string) (resolvedMode, error) {
	envTruthy := func(k string) bool {
		switch strings.ToLower(strings.TrimSpace(getenv(k))) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}

	janitor := envTruthy(envJanitorMode)
	runner := envTruthy(envRunnerMode)
	lifecycle := envTruthy(envLifecycleEnabled)

	if janitor && runner {
		return resolvedMode{}, fmt.Errorf(
			"%s and %s are both true, but each replaces the entire check set; "+
				"set exactly one (refusing to guess, because silently picking one "+
				"would leave the other's check set absent from Prometheus and no "+
				"alert can match a series that does not exist)",
			envJanitorMode, envRunnerMode)
	}

	m := resolvedMode{kind: modeDefault, lifecycleEnabled: lifecycle}
	switch {
	case janitor:
		m.kind = modeJanitor
	case runner:
		m.kind = modeRunner
	}

	if lifecycle && m.kind != modeDefault {
		which := envJanitorMode
		if m.kind == modeRunner {
			which = envRunnerMode
		}
		m.lifecycleEnabled = false
		m.warnings = append(m.warnings, configWarning{
			env: envLifecycleEnabled,
			msg: "SYNTHETIC_LIFECYCLE_ENABLED is ignored because " + which +
				" replaces the whole check set; this is expected when a replacement-mode " +
				"CronJob inherits the shared env block, but if you meant to run pod_lifecycle " +
				"here it is NOT running",
		})
	}

	return m, nil
}
