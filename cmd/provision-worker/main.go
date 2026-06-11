package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/provisioner"
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
	queries := database.NewQueries(pool)

	// Connect to NATS
	natsClient, err := events.NewClient(cfg.NATS, logger)
	if err != nil {
		logger.Error("NATS connection failed", "error", err)
		os.Exit(1)
	}
	defer natsClient.Close()

	// Initialize vCenter client
	vcClient := vcenter.New(vcenter.Config{
		URL:           cfg.VCenter.URL,
		User:          cfg.VCenter.User,
		Password:      cfg.VCenter.Password,
		Datacenter:    cfg.VCenter.Datacenter,
		Datastore:     cfg.VCenter.Datastore,
		VMFolder:      cfg.VCenter.VMFolder,
		ResourcePools: cfg.VCenter.ResourcePools,
		Hosts:         cfg.VCenter.Hosts,
		Insecure:      cfg.VCenter.Insecure,
	}, logger)

	if err := vcClient.Connect(ctx); err != nil {
		logger.Error("vCenter connection failed", "error", err)
		os.Exit(1)
	}
	defer vcClient.Disconnect(ctx)

	// Initialize OPNsense clients
	opnCfg := opnsense.Config{
		BaseURL:     cfg.OPNsense.BaseURL,
		APIKey:      cfg.OPNsense.APIKey,
		APISecret:   cfg.OPNsense.APISecret,
		SSHHost:     cfg.OPNsense.SSHHost,
		SSHUser:     cfg.OPNsense.SSHUser,
		SSHPassword: cfg.OPNsense.SSHPassword,
	}
	opnClient := opnsense.New(opnCfg, logger)
	opnSSH := opnsense.NewSSHClient(opnCfg, logger)

	// Create provisioner
	prov := provisioner.New(queries, vcClient, opnClient, opnSSH, natsClient, logger)

	// Optional: enable destroy_failed Pushgateway metric. When the
	// WORKER_PUSHGATEWAY_URL env is set, the worker publishes
	// crucible_pods_destroy_failed_count every 5 minutes so the
	// CruciblePodsStuckInDestroyFailed alert can fire even before the
	// lifecycle synthetic detects user-visible breakage. Empty disables it.
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		prov.DestroyFailedPusher = &provisioner.DestroyFailedPusher{
			BaseURL:        pgURL,
			Job:            job,
			GroupingLabels: map[string]string{"layer": "api"},
		}
		logger.Info("destroy_failed pushgateway enabled", "url", pgURL, "job", job)
	}

	// Optional: vCenter orphan reconciler. Scans the configured Student-VMs
	// folder against pod_vms on an hourly tick. Auto-destroys synthetic-noop-*
	// VMs whose parent pod is already terminal; logs + counts everything else
	// for operator review via the crucible_vcenter_orphans_* metric family.
	//
	// Disabled by default. Set WORKER_ORPHAN_RECONCILER_ENABLED=true to opt in.
	// WORKER_ORPHAN_RECONCILER_FOLDER overrides the scanned folder (defaults
	// to cfg.VCenter.VMFolder). WORKER_ORPHAN_RECONCILER_INTERVAL accepts any
	// Go duration ("1h", "30m"); empty falls back to 1 hour.
	orphanEnabled := strings.EqualFold(os.Getenv("WORKER_ORPHAN_RECONCILER_ENABLED"), "true")
	orphanInterval := time.Hour
	if v := os.Getenv("WORKER_ORPHAN_RECONCILER_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			orphanInterval = parsed
		} else {
			logger.Warn("invalid WORKER_ORPHAN_RECONCILER_INTERVAL; using default 1h", "value", v, "error", err)
		}
	}
	orphanFolder := os.Getenv("WORKER_ORPHAN_RECONCILER_FOLDER")
	if orphanFolder == "" {
		orphanFolder = cfg.VCenter.VMFolder
	}
	orphanMinAge := time.Hour
	if v := os.Getenv("WORKER_ORPHAN_RECONCILER_MIN_AGE"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			orphanMinAge = parsed
		}
	}
	var orphanPusher *provisioner.OrphanCountPusher
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		orphanPusher = &provisioner.OrphanCountPusher{
			BaseURL:        pgURL,
			Job:            job,
			GroupingLabels: map[string]string{"layer": "api"},
		}
	}
	orphanCfg := provisioner.OrphanReconcilerConfig{
		Folder: orphanFolder,
		MinAge: orphanMinAge,
		Pusher: orphanPusher,
	}

	// Worker ID for job claiming
	workerID, err := os.Hostname()
	if err != nil {
		workerID = "worker-unknown"
	}

	logger.Info("starting provision worker",
		"worker_id", workerID,
		"vcenter", cfg.VCenter.URL,
		"opnsense", cfg.OPNsense.BaseURL,
	)

	// Recover any jobs that were abandoned by a previous worker instance
	recovered, err := queries.RecoverStaleJobs(ctx)
	if err != nil {
		logger.Error("failed to recover stale jobs", "error", err)
	} else if recovered > 0 {
		logger.Info("recovered stale jobs", "count", recovered)
	}

	// Retry any pods stuck in destroy_failed from previous runs
	prov.RetryFailedDestroys(ctx)

	// Subscribe to job notifications from NATS
	_, err = natsClient.SubscribeJobCreated(func(jobID string, jobType string) {
		logger.Info("received job notification", "job_id", jobID, "type", jobType)
		processJobs(ctx, queries, prov, workerID, logger)
	})
	if err != nil {
		logger.Error("NATS subscription failed", "error", err)
		os.Exit(1)
	}

	// Polling fallback: check for jobs every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Retry failed destroys every 5 minutes
	retryTicker := time.NewTicker(5 * time.Minute)
	defer retryTicker.Stop()

	// vCenter orphan reconciler ticker (opt-in; nil-safe).
	var orphanTickerC <-chan time.Time
	if orphanEnabled {
		t := time.NewTicker(orphanInterval)
		defer t.Stop()
		orphanTickerC = t.C
		logger.Info("vcenter orphan reconciler enabled",
			"folder", orphanCfg.Folder, "interval", orphanInterval, "min_age", orphanMinAge)
	}

	// Start expiration cron (checks for expired pods every 5 minutes)
	go prov.StartExpirationCron(ctx)

	// Immediately process any pending/recovered jobs
	go processJobs(ctx, queries, prov, workerID, logger)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processJobs(ctx, queries, prov, workerID, logger)
			case <-retryTicker.C:
				prov.RetryFailedDestroys(ctx)
			case <-orphanTickerC:
				if _, err := prov.ReconcileVCenterOrphans(ctx, orphanCfg); err != nil {
					logger.Error("orphan reconcile failed", "error", err)
				}
			}
		}
	}()

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down worker")
	cancel()
}

// processJobs claims and processes available jobs via the provisioner.
func processJobs(ctx context.Context, queries *database.Queries, prov *provisioner.Provisioner, workerID string, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}

		job, err := queries.ClaimJob(ctx, workerID)
		if err != nil {
			logger.Error("claim job failed", "error", err)
			return
		}
		if job == nil {
			return // no pending jobs
		}

		logger.Info("claimed job", "job_id", job.ID, "type", job.Type)

		if err := prov.ProcessJob(ctx, job); err != nil {
			logger.Error("job failed", "job_id", job.ID, "type", job.Type, "error", err)
		} else {
			logger.Info("job completed", "job_id", job.ID, "type", job.Type)
		}
	}
}
