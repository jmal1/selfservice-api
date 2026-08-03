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
	"github.com/jmal1/selfservice-api/internal/objectstore"
	"github.com/jmal1/selfservice-api/internal/vcenter"
	vsphereHealth "github.com/jmal1/selfservice-api/internal/vsphere/health"
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
	if vc, ok := vcClient.(*vcenter.Client); ok && vc != nil && cfg.VCenter.TemplatesFolder != "" {
		handler.WithVCenterFolders(vc, cfg.VCenter.TemplatesFolder)
		logger.Info("vCenter folder enumeration enabled", "folder", cfg.VCenter.TemplatesFolder)
	}

	// Image upload (Epic A). Optional: without an object store every
	// /admin/images endpoint answers 503 "image upload not configured"
	// rather than nil-panicking, so the gateway still serves everything else.
	if cfg.ObjectStore.Endpoint != "" {
		objects, err := objectstore.New(objectstore.Config{
			Endpoint:  cfg.ObjectStore.Endpoint,
			AccessKey: cfg.ObjectStore.AccessKey,
			SecretKey: cfg.ObjectStore.SecretKey,
			Bucket:    cfg.ObjectStore.Bucket,
			Prefix:    cfg.ObjectStore.Prefix,
			UseSSL:    cfg.ObjectStore.UseSSL,
		})
		if err != nil {
			logger.Error("object store init failed — image upload disabled", "error", err)
		} else {
			handler.WithImageStore(objects)
			logger.Info("image upload enabled",
				"endpoint", cfg.ObjectStore.Endpoint, "bucket", cfg.ObjectStore.Bucket)
		}
	} else {
		logger.Info("image upload disabled (object store not configured)")
	}

	// ISO datastore browsing for the template wizard's ISO picker.
	if vc, ok := vcClient.(*vcenter.Client); ok && vc != nil && cfg.VCenter.ISODatastore != "" {
		handler.WithVCenterISOs(vc, cfg.VCenter.ISODatastore)
		logger.Info("vCenter ISO browsing enabled", "datastore", cfg.VCenter.ISODatastore)
	}

	// Pre-declare so the in-process vSphere probe (started below) can be
	// passed into the /admin/health dependency bag. The bag is wired
	// *after* the probe is constructed; nil here means /admin/health
	// reports vcenter as "not_configured" (consistent with the rest of
	// the codebase: missing optional dep -> graceful degrade, not crash).
	var vsphereProbeHandle *vsphereHealth.Probe

	// Start the vCenter credentials health probe (OP-1). Runs in-process so
	// it shares the api-gateway pod lifecycle. The probe creates a fresh
	// govmomi client every cycle so cached sessions cannot mask a rotated
	// SSO password — the exact failure mode we hit on 2026-06-07.
	if cfg.VCenter.URL != "" && cfg.VCenter.User != "" && cfg.VCenter.Password != "" {
		probe, err := vsphereHealth.New(vsphereHealth.Config{
			VCenterURL:           cfg.VCenter.URL,
			User:                 cfg.VCenter.User,
			Password:             cfg.VCenter.Password,
			Insecure:             cfg.VCenter.Insecure,
			PushgatewayURL:       cfg.VCenter.HealthPushgatewayURL,
			Job:                  "crucible_vsphere_health",
			GroupingLabels:       map[string]string{"layer": "vsphere"},
		}, logger)
		if err != nil {
			logger.Warn("vsphere health probe disabled", "error", err)
		} else {
			interval := cfg.VCenter.HealthCheckInterval
			if interval == 0 {
				interval = 5 * time.Minute
			}
			go probe.RunPeriodic(ctx, interval)
			vsphereProbeHandle = probe
			logger.Info("vsphere health probe started",
				"interval", interval,
				"pushgateway_configured", cfg.VCenter.HealthPushgatewayURL != "")
		}
	}

	// Wire the /admin/health dependency bag. Each field is optional —
	// missing values cause that probe to report "not_configured" rather
	// than failing the whole endpoint. The engine URL falls back to the
	// in-cluster service DNS used everywhere else in the codebase.
	engineHealthURL := getenvOrDefault("ENGINE_HEALTH_URL",
		"http://selfservice-engine.selfservice.svc.cluster.local:8081/healthz")
	handler.WithHealthDeps(handlers.HealthDeps{
		Pool:            pool,
		NATS:            natsClient,
		VSphere:         vsphereProbeHandle,
		OPNsenseBaseURL: cfg.OPNsense.BaseURL,
		EngineHealthURL: engineHealthURL,
	})
	logger.Info("admin health endpoint wired",
		"opnsense_configured", cfg.OPNsense.BaseURL != "",
		"vsphere_probe_configured", vsphereProbeHandle != nil,
		"engine_url", engineHealthURL)

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

// getenvOrDefault returns the value of the named environment variable, or
// def if the variable is unset or empty. Used for optional wiring like
// the engine /healthz URL where the in-cluster service DNS is the right
// default but ops may want to override (e.g. point at a sidecar in dev).
func getenvOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
