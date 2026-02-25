package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
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

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				processJobs(ctx, queries, prov, workerID, logger)
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
