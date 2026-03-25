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
	"github.com/jmal1/selfservice-api/internal/engine"
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

	// Create engine
	queries := engine.NewQueries(pool)
	eng := engine.New(queries, natsClient, engineID, logger)

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

	// Start timeout watchdog
	go eng.StartTimeoutWatchdog(ctx)

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
