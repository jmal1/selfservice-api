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
	envBaseURL       = "SYNTHETIC_BASE_URL"        // e.g. https://crucible.jmal.io
	envJWTSecret     = "SYNTHETIC_JWT_SECRET"      // same HMAC the API uses
	envUserID        = "SYNTHETIC_USER_ID"         // UUID of synthetic@lab.jmal.io DB row
	envUsername      = "SYNTHETIC_USERNAME"        // synthetic
	envRole          = "SYNTHETIC_ROLE"            // student (default) | instructor | admin
	envPushgatewayURL = "SYNTHETIC_PUSHGATEWAY_URL" // e.g. http://pushgateway.observability:9091
	envJob           = "SYNTHETIC_JOB"             // pushgateway job label, default crucible_synthetic_api
	envLayer         = "SYNTHETIC_LAYER"           // grouping label `layer`, default api
	envLoopInterval  = "SYNTHETIC_LOOP_INTERVAL"   // optional duration; if set, runs forever
	envCheckTimeout  = "SYNTHETIC_CHECK_TIMEOUT"   // optional duration, default 30s

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
	layer := envOr(envLayer, "api")

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

	// Build the active check list. The expensive pod_lifecycle check is
	// gated by SYNTHETIC_LIFECYCLE_ENABLED so the binary can be rolled out
	// before the synthetic-noop template exists.
	activeChecks := checks.All()
	if envBool(envLifecycleEnabled) {
		tmpl := os.Getenv(envLifecycleTemplate)
		if tmpl == "" {
			return fmt.Errorf("%s=true but %s is empty", envLifecycleEnabled, envLifecycleTemplate)
		}
		cfg := checks.DefaultPodLifecycleConfig(tmpl)
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

// lifecycleSafeTimeout returns a CheckTimeout that fits the most expensive
// registered check. pod_lifecycle can legitimately run for ~10 minutes; the
// per-check timeout MUST exceed its sum of ready + destroy budgets or the
// check will always fail mid-run.
func lifecycleSafeTimeout(base time.Duration, all []synthetic.Check) time.Duration {
	const lifecycleName = "pod_lifecycle"
	// 11 minutes is the worst-case envelope for the default config
	// (8m ready + 90s destroy + ~90s of pre-clean + HTTP overhead).
	const lifecycleEnvelope = 11 * time.Minute
	for _, c := range all {
		if c.Name() == lifecycleName && base < lifecycleEnvelope {
			return lifecycleEnvelope
		}
	}
	return base
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
