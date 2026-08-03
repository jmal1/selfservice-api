package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmal1/selfservice-api/internal/runner"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("crucible-runner starting")

	// Load configuration
	configPath := runner.DefaultConfigPath
	if v := os.Getenv("CRUCIBLE_RUNNER_CONFIG"); v != "" {
		configPath = v
	}

	cfg, err := runner.LoadConfig(configPath)
	if err != nil {
		logger.Error("failed to load config", "path", configPath, "error", err)
		os.Exit(1)
	}

	logger.Info("config loaded",
		"run_id", cfg.RunID,
		"workflows", len(cfg.Workflows),
		"target_ip", cfg.Target.IP,
		"callback_url", cfg.CallbackURL,
		"action_library_bytes", len(cfg.ActionLibrary),
	)

	// Materialise the engine-generated action library before anything can source
	// actions.sh. This is what makes library actions (http_get, port_open, …)
	// callable; without it every one of them fails with exit 127.
	if err := runner.MaterializeActionLibrary(cfg.ActionLibrary); err != nil {
		logger.Error("failed to write action library", "path", runner.ActionLibraryPath, "error", err)
		os.Exit(1)
	}

	// Create context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Create callback client
	callback := runner.NewCallbackClient(cfg.CallbackURL, cfg.CallbackToken, logger)

	// Start heartbeat goroutine
	runStart := time.Now()
	heartbeatCtx, heartbeatCancel := context.WithCancel(ctx)
	defer heartbeatCancel()

	executor := runner.NewExecutor(cfg, callback, logger)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				elapsed := int(time.Since(runStart).Seconds())
				callback.SendHeartbeat("executing", "", elapsed)
			}
		}
	}()

	// Execute all workflows
	logger.Info("beginning workflow execution", "count", len(cfg.Workflows))
	results, err := executor.RunAll(ctx)

	// Stop heartbeat
	heartbeatCancel()

	// Determine final run status
	finalStatus := "completed"
	if err != nil {
		finalStatus = "failed"
		logger.Error("run execution error", "error", err)
	} else {
		for _, r := range results {
			if r.Status != "pass" && r.Status != "skipped" {
				finalStatus = "completed" // still completed, even with failures
				break
			}
		}
	}

	// Report completion to engine
	if err := callback.ReportComplete(finalStatus, results); err != nil {
		logger.Error("failed to report completion", "error", err)
		// Write results to fallback file
		writeFallbackResults(results, logger)
	}

	// Summary
	passed, failed, errored := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case "pass":
			passed++
		case "fail":
			failed++
		default:
			errored++
		}
	}

	logger.Info("crucible-runner finished",
		"status", finalStatus,
		"total", len(results),
		"passed", passed,
		"failed", failed,
		"errored", errored,
		"duration", fmt.Sprintf("%.1fs", time.Since(runStart).Seconds()),
	)
}

// writeFallbackResults writes results to a local file when the callback fails.
func writeFallbackResults(results []runner.WorkflowRunResult, logger *slog.Logger) {
	fallbackPath := "/tmp/crucible-fallback-results.json"
	f, err := os.Create(fallbackPath)
	if err != nil {
		logger.Error("failed to create fallback file", "error", err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		logger.Error("failed to write fallback results", "error", err)
	} else {
		logger.Info("wrote fallback results", "path", fallbackPath)
	}
}
