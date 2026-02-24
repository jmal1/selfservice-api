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

	// Worker ID for job claiming
	workerID, err := os.Hostname()
	if err != nil {
		workerID = "worker-unknown"
	}

	logger.Info("starting provision worker", "worker_id", workerID)

	// Subscribe to job notifications from NATS
	_, err = natsClient.SubscribeJobCreated(func(jobID string, jobType string) {
		logger.Info("received job notification", "job_id", jobID, "type", jobType)
		processJobs(ctx, queries, natsClient, workerID, logger)
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
				processJobs(ctx, queries, natsClient, workerID, logger)
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

// processJobs claims and processes available jobs.
func processJobs(ctx context.Context, queries *database.Queries, natsClient *events.Client, workerID string, logger *slog.Logger) {
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

		// Update status to in_progress
		if err := queries.UpdateJobStatus(ctx, job.ID, "in_progress", nil); err != nil {
			logger.Error("update job status failed", "error", err)
			continue
		}

		// Dispatch based on job type
		// TODO: Phase 2 — implement actual provisioning workflows
		switch job.Type {
		case "pod_create":
			logger.Info("pod_create job received — provisioning not yet implemented", "job_id", job.ID)
			queries.UpdateJobStatus(ctx, job.ID, "failed", []byte(`{"error":"provisioning worker not yet implemented"}`))
		case "pod_destroy":
			logger.Info("pod_destroy job received — teardown not yet implemented", "job_id", job.ID)
			queries.UpdateJobStatus(ctx, job.ID, "failed", []byte(`{"error":"teardown worker not yet implemented"}`))
		default:
			logger.Warn("unknown job type", "type", job.Type, "job_id", job.ID)
			queries.UpdateJobStatus(ctx, job.ID, "failed", []byte(`{"error":"unknown job type"}`))
		}

		natsClient.PublishJobStatus(job.ID, "completed", "job processed")
	}
}
