package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/engine"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Connect to database
	pool, err := database.Connect(ctx, cfg.Database)
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Run migrations
	if err := database.RunMigrations(cfg.Database.DSN()); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("database migrations applied")

	// Connect to NATS
	natsClient, err := events.NewClient(cfg.NATS, logger)
	if err != nil {
		logger.Error("NATS connection failed", "error", err)
		os.Exit(1)
	}
	defer natsClient.Close()

	// Engine ID for logging
	engineID, err := os.Hostname()
	if err != nil {
		engineID = "engine-unknown"
	}

	// Create K8s client (nil if running outside cluster — e.g., local dev)
	k8sCfg := engine.K8sConfig{
		Namespace:   getEnv("ENGINE_NAMESPACE", "selfservice"),
		RunnerImage: getEnv("RUNNER_IMAGE", "ghcr.io/jmal1/selfservice-crucible-runner:latest"),
		RunnerNode:  getEnv("RUNNER_NODE", "k3sv03"),
		TrunkNIC:    getEnv("RUNNER_TRUNK_NIC", "ens224"),
		EngineURL:   getEnv("ENGINE_CALLBACK_URL", "http://crucible-engine.selfservice.svc.cluster.local:8081"),
		// Crucible's GHCR packages are private, so the runner Job needs the same
		// pull secret every Helm-managed workload gets. Defaulted rather than
		// left empty: an unset value here fails at pull time with a 401 that
		// looks like a registry outage, ~10 minutes after the run starts.
		ImagePullSecrets: splitAndTrim(getEnv("RUNNER_IMAGE_PULL_SECRETS", "ghcr-pull-secret")),
	}

	var k8sClient *engine.K8sClient
	k8sClient, err = engine.NewK8sClient(k8sCfg, logger)
	if err != nil {
		logger.Warn("k8s client not available — running without K8s provisioning", "error", err)
	}

	// Create engine
	queries := engine.NewQueries(pool)
	eng := engine.New(queries, natsClient, k8sClient, engineID, k8sCfg.EngineURL, logger)

	// Runner metrics. Optional: without a Pushgateway URL every recorder call
	// is a nil-safe no-op and the engine behaves exactly as before. The
	// recorder must be attached to BOTH the engine (job outcomes, callbacks)
	// and the K8s client (provision timings, cleanup failures, orphans) —
	// they record from different call sites.
	var runnerMetrics *engine.RunnerMetrics
	if pgURL := getEnv("ENGINE_PUSHGATEWAY_URL", ""); pgURL != "" {
		runnerMetrics = engine.NewRunnerMetrics(pgURL, getEnv("ENGINE_PUSHGATEWAY_JOB", "crucible_engine"), nil)
		eng = eng.WithRunnerMetrics(runnerMetrics)
		if k8sClient != nil {
			k8sClient = k8sClient.WithRunnerMetrics(runnerMetrics)
		}
		logger.Info("runner metrics enabled", "pushgateway", pgURL)
	} else {
		logger.Info("runner metrics disabled (ENGINE_PUSHGATEWAY_URL unset)")
	}

	// Wire vCenter client + GuestOperations dispatcher so workflows with
	// execution_mode=vmware_tools can run in-process. If vCenter config is
	// missing or connection fails we keep going — kali_runner workflows
	// still work, vmware_tools ones will surface a clean per-workflow error.
	if cfg.VCenter.URL != "" {
		vcClient := vcenter.New(vcenter.Config{
			URL:                  cfg.VCenter.URL,
			User:                 cfg.VCenter.User,
			Password:             cfg.VCenter.Password,
			Datacenter:           cfg.VCenter.Datacenter,
			Datastore:            cfg.VCenter.Datastore,
			VMFolder:             cfg.VCenter.VMFolder,
			ResourcePools:        cfg.VCenter.ResourcePools,
			Hosts:                cfg.VCenter.Hosts,
			HostReservedMemoryMB: cfg.VCenter.HostReservedMemoryMB,
			Insecure:             cfg.VCenter.Insecure,
		}, logger)
		if err := vcClient.Connect(ctx); err != nil {
			logger.Warn("vCenter connection failed; vmware_tools workflows will be rejected",
				"error", err)
		} else {
			defer vcClient.Disconnect(ctx)
			if _, err := vcClient.ResolveProvisioningHosts(ctx); err != nil {
				logger.Warn("VCENTER_HOSTS failed strict inventory resolution; vmware_tools workflows will be rejected",
					"error", err)
			} else {
				dispatcher := engine.NewVMwareToolsDispatcher(vcClient, queries, eng, logger)
				eng.WithVMwareToolsDispatcher(dispatcher)
				logger.Info("vmware_tools dispatcher configured", "vcenter_url", cfg.VCenter.URL)
			}
		}
	} else {
		logger.Info("vCenter config not provided; vmware_tools workflows will be rejected")
	}

	logger.Info("starting crucible-engine",
		"engine_id", engineID,
		"database", cfg.Database.Host,
		"nats", cfg.NATS.URL,
	)

	// Recover any runs orphaned by a previous engine instance
	if err := eng.RecoverStaleRuns(ctx); err != nil {
		logger.Error("failed to recover stale runs", "error", err)
	}

	// Subscribe to run creation notifications via NATS
	_, err = natsClient.SubscribeRaw("testing.runs.created", func(evt events.Event) {
		logger.Info("received run notification", "run_id", evt.JobID)
		eng.ProcessPendingRuns(ctx)
	})
	if err != nil {
		logger.Error("NATS subscription failed", "error", err)
		os.Exit(1)
	}

	// Start callback HTTP server for runner pods
	callbackPort := getEnvInt("ENGINE_CALLBACK_PORT", 8081)
	engine.StartCallbackServer(ctx, eng, callbackPort, logger)

	// Start timeout watchdog
	go eng.StartTimeoutWatchdog(ctx)

	// Start orphan cleanup (every 5 min)
	go eng.StartOrphanCleanup(ctx)

	// Flush accumulated runner metrics to Pushgateway. Without this the
	// recorders above only ever mutate in-process counters and nothing
	// reaches Prometheus. No-op when metrics are disabled.
	go runnerMetrics.RunPusher(ctx, 30*time.Second, logger)

	// Polling fallback: check for runs every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Process any pending runs immediately
	go eng.ProcessPendingRuns(ctx)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				eng.ProcessPendingRuns(ctx)
			}
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down crucible-engine")
	cancel()
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// splitAndTrim parses a comma-separated env value into a clean slice, dropping
// empty entries so a trailing comma or an all-whitespace value yields nil
// rather than a secret reference named "".
func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
