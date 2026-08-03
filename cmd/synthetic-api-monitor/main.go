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
	// envLifecycleReadyTimeout overrides the default 8m timeout for waiting
	// on PodStatusActive. Increase if vCenter is slow; decrease for tests.
	envLifecycleReadyTimeout = "SYNTHETIC_LIFECYCLE_READY_TIMEOUT"
	// envLifecycleDestroyTimeout overrides the default 90s timeout for
	// waiting on PodStatusDestroyed.
	envLifecycleDestroyTimeout = "SYNTHETIC_LIFECYCLE_DESTROY_TIMEOUT"

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
	// covers. WARNING: if SYNTHETIC_RUNNER_TEMPLATE or
	// SYNTHETIC_RUNNER_PLAYLIST_ID is unset the binary exits with a clear
	// error rather than silently registering nothing — a misconfigured
	// deploy would leave the entire engine dispatch→Kali path unmonitored.
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
	// envRunnerReadyTimeout overrides the default 8m timeout for waiting
	// on pod active. Increase if vCenter is slow; decrease for tests.
	envRunnerReadyTimeout = "SYNTHETIC_RUNNER_READY_TIMEOUT"
	// envRunnerRunTimeout overrides the default 10m timeout for waiting on
	// a terminal run state. Increase if Kali runner provisioning or workflow
	// execution is slow; decrease for tests.
	envRunnerRunTimeout = "SYNTHETIC_RUNNER_RUN_TIMEOUT"
	// envRunnerDestroyTimeout overrides the default 90s timeout for waiting
	// on pod destroyed.
	envRunnerDestroyTimeout = "SYNTHETIC_RUNNER_DESTROY_TIMEOUT"

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
	layer, err := resolvePushLayer(envBool(envRunnerMode), os.Getenv(envLayer))
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

	// Mint a session JWT good for one full check cycle. We mint a fresh one
	// each cycle so a token leaked from a single run cannot be used long.
	mintCookie := func() (string, error) {
		// TTL is the per-cycle check timeout * checks + a safety margin.
		return synthetic.MintSessionToken([]byte(jwtSecret), userID, username, role,
			checkTimeout*time.Duration(len(checks.All())+1))
	}

	cookie, err := mintCookie()
	if err != nil {
		return fmt.Errorf("mint initial session token: %w", err)
	}
	client := synthetic.NewClient(baseURL, cookie)
	pg := synthetic.NewPushgateway(pushgatewayURL, job, map[string]string{
		"layer": layer,
	})

	// Second, elevated identity. See envInstructorUserID for why this is a
	// separate user row rather than a higher role on the primary one.
	instructorID := os.Getenv(envInstructorUserID)
	instructorName := envOr(envInstructorUsername, "synthetic-instructor")
	var instructorClient *synthetic.Client
	mintInstructorCookie := func() (string, error) {
		return synthetic.MintSessionToken([]byte(jwtSecret), instructorID, instructorName,
			"instructor", checkTimeout*time.Duration(len(checks.All())+1))
	}
	if instructorID != "" {
		instructorCookie, err := mintInstructorCookie()
		if err != nil {
			return fmt.Errorf("mint instructor session token: %w", err)
		}
		instructorClient = synthetic.NewClient(baseURL, instructorCookie)
	}

	// Build the active check list. The expensive pod_lifecycle check is
	// gated by SYNTHETIC_LIFECYCLE_ENABLED so the binary can be rolled out
	// before the synthetic-noop template exists.
	//
	// SYNTHETIC_JANITOR_MODE takes precedence: when set, the binary acts as
	// a standalone orphan sweeper and registers ONLY the synthetic_janitor
	// check. This lets a separate daily CronJob reuse the same image,
	// secrets, and pushgateway plumbing as the regular monitor without
	// running any of the cheap probes (which the */10 monitor already
	// covers).
	activeChecks := checks.All()
	if envBool(envJanitorMode) {
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
	} else if envBool(envLifecycleEnabled) {
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
	} else if envBool(envRunnerMode) {
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
	// pod_lifecycle needs its own timeout budget — it polls for minutes.
	// Use the larger of (configured CheckTimeout) or (ready + destroy + 60s).
	runner.CheckTimeout = lifecycleSafeTimeout(checkTimeout, activeChecks)

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
		// Refresh cookie each cycle in loop mode; for one-shot, we already have one.
		client.SessionCookie = cookie
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
		cookie, err = mintCookie()
		if err != nil {
			return fmt.Errorf("re-mint session token: %w", err)
		}
		if instructorClient != nil {
			ic, err := mintInstructorCookie()
			if err != nil {
				return fmt.Errorf("re-mint instructor session token: %w", err)
			}
			// Elevated checks hold this pointer, so mutating in place
			// refreshes them too.
			instructorClient.SessionCookie = ic
		}
	}
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

// lifecycleSafeTimeout returns a CheckTimeout that fits the most expensive
// registered check. pod_lifecycle can legitimately run for ~10 minutes and
// runner_smoke for ~20 minutes; the per-check timeout MUST exceed those sums
// or the checks will always fail mid-run.
//
// It takes the MAXIMUM envelope over every registered check rather than
// returning on the first match. Returning early makes the result depend on
// registration order, so a future change that registers pod_lifecycle and
// runner_smoke together would hand runner_smoke an 11-minute budget for a
// 21-minute job — and it would fail every single cycle with a timeout that
// looks exactly like a genuinely broken Kali runner.
func lifecycleSafeTimeout(base time.Duration, all []synthetic.Check) time.Duration {
	// Worst-case envelopes for the default configs:
	//   pod_lifecycle: 8m ready + 90s destroy + ~90s pre-clean + HTTP overhead
	//   runner_smoke:  8m ready + 10m run + 90s destroy + ~90s pre-clean + overhead
	envelopes := map[string]time.Duration{
		"pod_lifecycle": 11 * time.Minute,
		"runner_smoke":  21 * time.Minute,
	}

	longest := base
	for _, c := range all {
		if e, ok := envelopes[c.Name()]; ok && e > longest {
			longest = e
		}
	}
	return longest
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
