package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jmal1/selfservice-api/internal/api/handlers"
	"github.com/jmal1/selfservice-api/internal/api/routes"
	"github.com/jmal1/selfservice-api/internal/auth"
	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load config
	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Run database migrations
	logger.Info("running database migrations")
	if err := database.RunMigrations(cfg.Database.DSN()); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("migrations complete")

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

	// Initialize OIDC auth provider
	// JWT secret: in production, fetch from Vault. For now, use env var.
	jwtSecret := []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		logger.Error("JWT_SECRET environment variable is required")
		os.Exit(1)
	}

	authProvider, err := auth.NewProvider(ctx, cfg.OIDC, queries, jwtSecret, logger)
	if err != nil {
		logger.Error("OIDC provider init failed", "error", err)
		os.Exit(1)
	}

	// Initialize vCenter client for console access (optional — console won't work without it)
	var vcClient handlers.VCenterConsole
	if cfg.VCenter.URL != "" && cfg.VCenter.User != "" {
		vc := vcenter.New(vcenter.Config{
			URL:        cfg.VCenter.URL,
			User:       cfg.VCenter.User,
			Password:   cfg.VCenter.Password,
			Datacenter: cfg.VCenter.Datacenter,
			Insecure:   cfg.VCenter.Insecure,
		}, logger)
		if err := vc.Connect(ctx); err != nil {
			logger.Warn("vCenter connection failed — console access disabled", "error", err)
		} else {
			vcClient = vc
			logger.Info("vCenter connected for console access")
		}
	}

	// Create handlers and router
	handler := handlers.NewHandler(queries, natsClient, vcClient, logger, cfg.Server.AllowedOrigins)
	router := routes.Setup(handler, authProvider, queries, cfg.Server.AllowedOrigins)

	// Start HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		logger.Info("shutting down server")
		cancel()

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown error", "error", err)
		}
	}()

	logger.Info("starting API gateway", "addr", addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}

	logger.Info("server stopped")
}
