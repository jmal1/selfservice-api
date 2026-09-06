package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/objectstore"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/provisioner"
	"github.com/jmal1/selfservice-api/internal/vcenter"
	"github.com/jmal1/selfservice-api/internal/worklease"
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
	queries := database.NewQueries(pool)

	// Work leases (Postgres row claim + heartbeat) gate exclusive reconcilers.
	// There is no process-wide leader: each named reconciler is claimed independently.
	// Job claiming remains multi-worker via FOR UPDATE SKIP LOCKED + job leases.
	leaseStore := queries
	workerHost, _ := os.Hostname()
	workerID := workerHost + "-" + uuid.NewString()
	logger.Info("work-lease coordination enabled", "worker_id", workerID)

	// Connect to NATS
	natsClient, err := events.NewClient(cfg.NATS, logger)
	if err != nil {
		logger.Error("NATS connection failed", "error", err)
		os.Exit(1)
	}
	defer natsClient.Close()

	// Initialize vCenter client
	vcClient := vcenter.New(vcenter.Config{
		URL:        cfg.VCenter.URL,
		User:       cfg.VCenter.User,
		Password:   cfg.VCenter.Password,
		Datacenter: cfg.VCenter.Datacenter,
		Datastore:  cfg.VCenter.Datastore,
		VMFolder:   cfg.VCenter.VMFolder,
		// Template build VMs (source_type=iso creates a blank shell here)
		// belong beside every other template, NOT in VMFolder — that folder
		// holds ephemeral student pod VMs and is what the orphan reconciler
		// scans. Without this the ISO path resolved an empty folder path and
		// failed before creating anything.
		TemplateFolder:       cfg.VCenter.TemplatesFolder,
		ResourcePools:        cfg.VCenter.ResourcePools,
		Hosts:                cfg.VCenter.Hosts,
		HostReservedMemoryMB: cfg.VCenter.HostReservedMemoryMB,
		Insecure:             cfg.VCenter.Insecure,
	}, logger)

	if err := vcClient.Connect(ctx); err != nil {
		logger.Error("vCenter connection failed", "error", err)
		os.Exit(1)
	}
	defer vcClient.Disconnect(ctx)
	resolvedHosts, err := vcClient.ResolveProvisioningHosts(ctx)
	if err != nil {
		logger.Error("VCENTER_HOSTS failed strict inventory resolution", "error", err)
		os.Exit(1)
	}
	for _, host := range resolvedHosts {
		logger.Info("provisioning host allowlisted",
			"host", host.Name,
			"inventory_path", host.InventoryPath,
			"moref", host.MoRef,
			"compute_moref", host.ComputeMoRef)
	}

	// Initialize OPNsense clients
	opnCfg := opnsense.Config{
		BaseURL:     cfg.OPNsense.BaseURL,
		APIKey:      cfg.OPNsense.APIKey,
		APISecret:   cfg.OPNsense.APISecret,
		SSHHost:     cfg.OPNsense.SSHHost,
		SSHUser:     cfg.OPNsense.SSHUser,
		SSHPassword: cfg.OPNsense.SSHPassword,
		SSHHostKey:  cfg.OPNsense.SSHHostKey,
	}
	opnClient := opnsense.New(opnCfg, logger)
	opnSSH, err := opnsense.NewSSHClient(opnCfg, logger)
	if err != nil {
		logger.Error("OPNsense SSH configuration failed strict validation", "error", err)
		os.Exit(1)
	}

	// Create provisioner
	prov := provisioner.New(queries, vcClient, opnClient, opnSSH, natsClient, logger)

	var pipeline *provisioner.PipelineMetrics
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PIPELINE_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_pipeline"
		}
		pipeline = provisioner.NewPipelineMetrics(pgURL, job, map[string]string{"layer": "api"})
		prov.EnablePipelineMetrics(pipeline)
		logger.Info("pipeline metrics enabled", "url", pgURL, "job", job)
		go pipeline.RunPusher(ctx, 30*time.Second, logger)
	}

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

	// Optional: image_import support. Requires an object store to stage the
	// browser-uploaded ISO/OVA. When unset the worker still starts and serves
	// every other job type; image_import jobs then fail with an actionable
	// message instead of nil-panicking part-way through a multi-GB stream.
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
			logger.Error("object store init failed; image_import disabled", "error", err)
		} else {
			// Imported OVAs land in the first configured Student-VMs pool.
			// An empty explicit value still uses the canonical configured-pool
			// resolver; vCenter default placement is never allowed.
			ovaPool := ""
			if len(cfg.VCenter.ResourcePools) > 0 {
				ovaPool = cfg.VCenter.ResourcePools[0]
			}
			if pipeline != nil {
				prov.EnableImageImport(objects, pipeline, provisioner.ImageImportConfig{
					ISODatastore:    cfg.VCenter.ISODatastore,
					ISOFolder:       cfg.VCenter.ISOFolder,
					OVAFolder:       cfg.VCenter.TemplatesFolder,
					OVADatastore:    cfg.VCenter.Datastore,
					OVAResourcePool: ovaPool,
				})
			} else {
				prov.EnableImageImport(objects, nil, provisioner.ImageImportConfig{
					ISODatastore:    cfg.VCenter.ISODatastore,
					ISOFolder:       cfg.VCenter.ISOFolder,
					OVAFolder:       cfg.VCenter.TemplatesFolder,
					OVADatastore:    cfg.VCenter.Datastore,
					OVAResourcePool: ovaPool,
				})
			}
			logger.Info("image_import enabled",
				"endpoint", cfg.ObjectStore.Endpoint,
				"bucket", cfg.ObjectStore.Bucket,
				"iso_datastore", cfg.VCenter.ISODatastore)
			// Note: stuck-upload reconciler is integrated into the main select
			// loop below (stuckUploadTickerC) so it can be gated by IsLeader().
		}
	}

	// Capture stuck-upload config for use in the main select loop.
	stuckUploadInterval := envDuration(logger, "WORKER_STUCK_UPLOAD_INTERVAL", 5*time.Minute)
	stuckUploadStaleThreshold := envDuration(logger, "WORKER_STUCK_UPLOAD_STALE_THRESHOLD", 30*time.Minute)

	templateReconcilerEnabled := true
	if v := os.Getenv("WORKER_PIPELINE_RECONCILER_ENABLED"); v != "" {
		templateReconcilerEnabled = strings.EqualFold(v, "true")
	}
	templateReconcilerInterval := envDuration(logger, "WORKER_PIPELINE_RECONCILER_INTERVAL", 5*time.Minute)
	templateReconcilerStaleThreshold := envDuration(logger, "WORKER_PIPELINE_RECONCILER_STALE_THRESHOLD", 2*time.Hour)
	var templateReconcilerTickerC <-chan time.Time
	if templateReconcilerEnabled {
		t := time.NewTicker(templateReconcilerInterval)
		defer t.Stop()
		templateReconcilerTickerC = t.C
		logger.Info("template reconciler enabled",
			"interval", templateReconcilerInterval,
			"stale_threshold", templateReconcilerStaleThreshold)
		go func() {
			if _, err := prov.ReconcileTemplateMetrics(ctx, provisioner.TemplateReconcilerConfig{
				Interval:       templateReconcilerInterval,
				StaleThreshold: templateReconcilerStaleThreshold,
			}); err != nil {
				logger.Error("initial template reconcile failed", "error", err)
			}
		}()
	}

	// Retry-pending gauge: keep crucible_job_retry_pending accurate. Jobs
	// sleeping between retry attempts are invisible to ClaimJob but still exist
	// in the DB; without this ticker the gauge never fires.
	// Only runs when a pipeline (Pushgateway) is configured; otherwise there is
	// nowhere to push and the query is pointless.
	var retryPendingTickerC <-chan time.Time
	if pipeline != nil {
		t := time.NewTicker(envDuration(logger, "WORKER_RETRY_PENDING_INTERVAL", 30*time.Second))
		defer t.Stop()
		retryPendingTickerC = t.C
		logger.Info("retry-pending reconciler enabled",
			"interval", envDuration(logger, "WORKER_RETRY_PENDING_INTERVAL", 30*time.Second))
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
	var orphanScheduler *provisioner.OrphanReconcilerScheduler
	if orphanEnabled {
		orphanScheduler = provisioner.NewOrphanReconcilerScheduler(
			alwaysWorkLeaseLeadership(ctx),
			func(runCtx context.Context) (provisioner.ReconcileCounts, error) {
				return runOrphanLeased(runCtx, leaseStore, worklease.VCenterOrphan, workerID, logger,
					func(leaseCtx context.Context) (provisioner.ReconcileCounts, error) {
						return prov.ReconcileVCenterOrphans(leaseCtx, orphanCfg)
					})
			},
			logger,
		)
	}

	// Optional: Templates-folder orphan reconciler. Destroys leftover wizard
	// staging VMs for error/draft rows idle longer than inactiveAge, and
	// unmatched disposable inventory names with no DB owner.
	// Disabled by default. Set WORKER_TEMPLATE_ORPHAN_RECONCILER_ENABLED=true.
	templateOrphanEnabled := strings.EqualFold(os.Getenv("WORKER_TEMPLATE_ORPHAN_RECONCILER_ENABLED"), "true")
	templateOrphanInterval := time.Hour
	if v := os.Getenv("WORKER_TEMPLATE_ORPHAN_RECONCILER_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			templateOrphanInterval = parsed
		} else {
			logger.Warn("invalid WORKER_TEMPLATE_ORPHAN_RECONCILER_INTERVAL; using default 1h", "value", v, "error", err)
		}
	}
	templateOrphanFolder := os.Getenv("WORKER_TEMPLATE_ORPHAN_RECONCILER_FOLDER")
	if templateOrphanFolder == "" {
		templateOrphanFolder = cfg.VCenter.TemplatesFolder
	}
	templateOrphanInactiveAge := provisioner.DefaultTemplateOrphanInactiveAge
	if v := os.Getenv("WORKER_TEMPLATE_ORPHAN_RECONCILER_INACTIVE_AGE"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			templateOrphanInactiveAge = parsed
		}
	}
	templateOrphanMinAge := time.Hour
	if v := os.Getenv("WORKER_TEMPLATE_ORPHAN_RECONCILER_MIN_AGE"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			templateOrphanMinAge = parsed
		}
	}
	var templateOrphanPusher *provisioner.TemplateOrphanCountPusher
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		templateOrphanPusher = &provisioner.TemplateOrphanCountPusher{
			BaseURL:        pgURL,
			Job:            job,
			GroupingLabels: map[string]string{"layer": "api"},
		}
	}
	templateOrphanCfg := provisioner.TemplateOrphanReconcilerConfig{
		Folder:      templateOrphanFolder,
		InactiveAge: templateOrphanInactiveAge,
		MinAge:      templateOrphanMinAge,
		Pusher:      templateOrphanPusher,
	}
	var templateOrphanScheduler *provisioner.OrphanReconcilerScheduler
	if templateOrphanEnabled {
		templateOrphanScheduler = provisioner.NewOrphanReconcilerScheduler(
			alwaysWorkLeaseLeadership(ctx),
			func(runCtx context.Context) (provisioner.ReconcileCounts, error) {
				return runOrphanLeased(runCtx, leaseStore, worklease.TemplateOrphan, workerID, logger,
					func(leaseCtx context.Context) (provisioner.ReconcileCounts, error) {
						counts, err := prov.ReconcileTemplateOrphans(leaseCtx, templateOrphanCfg)
						return provisioner.ReconcileCounts{
							InventoryVMs:    counts.InventoryVMs,
							Unknown:         counts.UnknownDryRun,
							SkippedRecent:   counts.SkippedRecent,
							Destroyed:       counts.Destroyed,
							DestroyFailures: counts.DestroyFailures,
						}, err
					})
			},
			logger,
		)
	}

	// Pod-VM DHCP IP reconciler. Keeps pod_vms.ip_address in sync with the
	// live guest address for running, IP-assigned VMs — catching both a
	// missed capture at provisioning (slow Windows OOBE) and later DHCP lease
	// changes. Safe (only ever writes a live routable IPv4), so it's enabled
	// by default. Set WORKER_IP_RECONCILER_ENABLED=false to opt out;
	// WORKER_IP_RECONCILER_INTERVAL overrides the default 2m cadence.
	ipReconcilerEnabled := true
	if v := os.Getenv("WORKER_IP_RECONCILER_ENABLED"); v != "" {
		ipReconcilerEnabled = strings.EqualFold(v, "true")
	}
	ipReconcilerInterval := 2 * time.Minute
	if v := os.Getenv("WORKER_IP_RECONCILER_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			ipReconcilerInterval = parsed
		} else {
			logger.Warn("invalid WORKER_IP_RECONCILER_INTERVAL; using default 2m", "value", v, "error", err)
		}
	}
	var ipReconcilerPusher *provisioner.IPReconcileCountPusher
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		ipReconcilerPusher = &provisioner.IPReconcileCountPusher{
			BaseURL:        pgURL,
			Job:            job,
			GroupingLabels: map[string]string{"layer": "api"},
		}
	}
	ipReconcilerCfg := provisioner.IPReconcilerConfig{Pusher: ipReconcilerPusher}

	// Optional: OPNsense network reconciler. Every interval it re-asserts the
	// VLAN, OPNsense interface, Kea DHCP subnet + binding, firewall pass rule
	// and outbound NAT for every active pod, and releases leaked vlan_pool
	// allocations from destroyed pods. Self-heals the class of drift that caused
	// the 2026-07-07 pod DHCP + internet outage. Enabled by default; set
	// WORKER_NETWORK_RECONCILER_ENABLED=false to opt out.
	// WORKER_NETWORK_RECONCILER_INTERVAL overrides the default 5m cadence.
	networkReconcilerEnabled := true
	if v := os.Getenv("WORKER_NETWORK_RECONCILER_ENABLED"); v != "" {
		networkReconcilerEnabled = strings.EqualFold(v, "true")
	}
	networkReconcilerInterval := 5 * time.Minute
	if v := os.Getenv("WORKER_NETWORK_RECONCILER_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			networkReconcilerInterval = parsed
		} else {
			logger.Warn("invalid WORKER_NETWORK_RECONCILER_INTERVAL; using default 5m", "value", v, "error", err)
		}
	}
	networkMaxFirewallRules := 4096
	if v := os.Getenv("WORKER_NETWORK_RECONCILER_MAX_FIREWALL_RULES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			networkMaxFirewallRules = parsed
		} else {
			logger.Warn("invalid WORKER_NETWORK_RECONCILER_MAX_FIREWALL_RULES; using default 4096", "value", v)
		}
	}
	networkFirewallCleanupLimit := 100
	if v := os.Getenv("WORKER_NETWORK_RECONCILER_FIREWALL_CLEANUP_LIMIT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			networkFirewallCleanupLimit = parsed
		} else {
			logger.Warn("invalid WORKER_NETWORK_RECONCILER_FIREWALL_CLEANUP_LIMIT; using default 100", "value", v)
		}
	}
	contentFilterEnabled := strings.EqualFold(os.Getenv("WORKER_CONTENT_FILTER_ENABLED"), "true")
	contentFilterSourceNetwork := os.Getenv("WORKER_CONTENT_FILTER_SOURCE_NETWORK")
	if contentFilterSourceNetwork == "" {
		contentFilterSourceNetwork = "10.100.0.0/16"
	}
	contentFilterAllowlist := splitNonEmpty(os.Getenv("WORKER_CONTENT_FILTER_ALLOWLIST"))
	var networkReconcilerPusher *provisioner.NetworkReconcilePusher
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		networkReconcilerPusher = &provisioner.NetworkReconcilePusher{
			BaseURL:        pgURL,
			Job:            job,
			GroupingLabels: map[string]string{"layer": "api"},
		}
	}
	networkReconcilerCfg := provisioner.NetworkReconcilerConfig{
		MaxFirewallRules:     networkMaxFirewallRules,
		FirewallCleanupLimit: networkFirewallCleanupLimit,
		ContentFilter: provisioner.ContentFilterConfig{
			Enabled:             contentFilterEnabled,
			SourceNetwork:       contentFilterSourceNetwork,
			CategoryFeedBaseURL: os.Getenv("WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL"),
			Allowlist:           contentFilterAllowlist,
		},
		Pusher: networkReconcilerPusher,
	}

	// Idle VM suspend evaluator. Checks for running pod VMs that have been
	// idle (no console activity AND low CPU/net) for longer than the configured
	// threshold and suspends them to free cluster resources.
	//
	// DRY-RUN IS ON BY DEFAULT. The evaluator logs what it would suspend but
	// makes no vCenter mutations until WORKER_IDLE_EVALUATOR_DRY_RUN=false is
	// set explicitly, after an operator has validated the decisions on live data.
	// Suspending a student mid-exam is the failure mode that matters; the code
	// ships defaulting to safe.
	//
	// WORKER_IDLE_EVALUATOR_ENABLED=false disables the loop entirely.
	// WORKER_IDLE_EVALUATOR_INTERVAL overrides the default 15-minute cadence.
	idleEvalEnabled := true
	if v := os.Getenv("WORKER_IDLE_EVALUATOR_ENABLED"); v != "" {
		idleEvalEnabled = strings.EqualFold(v, "true")
	}
	idleEvalInterval := 15 * time.Minute
	if v := os.Getenv("WORKER_IDLE_EVALUATOR_INTERVAL"); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil && parsed > 0 {
			idleEvalInterval = parsed
		} else {
			logger.Warn("invalid WORKER_IDLE_EVALUATOR_INTERVAL; using default 15m", "value", v, "error", err)
		}
	}
	dryRunResult := provisioner.ParseDryRunEnv(os.Getenv("WORKER_IDLE_EVALUATOR_DRY_RUN"))
	switch dryRunResult.Outcome {
	case provisioner.DryRunOffExplicit:
		logger.Info("idle evaluator: dry-run OFF — VM suspension is LIVE")
	case provisioner.DryRunOnDefault:
		logger.Info("idle evaluator: dry-run ON (default — WORKER_IDLE_EVALUATOR_DRY_RUN not set)")
	case provisioner.DryRunOnUnrecognised:
		logger.Warn("idle evaluator: WORKER_IDLE_EVALUATOR_DRY_RUN value is not recognised — "+
			"dry-run remains ON and no VM will be suspended; "+
			"the only accepted value to disable dry-run is \"false\"",
			"value", dryRunResult.Raw,
			"accepted_to_disable", "false")
	}
	var idleEvalPusher *provisioner.SuspendMetrics
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		idleEvalPusher = provisioner.NewSuspendMetrics(pgURL, job, map[string]string{"layer": "api"})
	}
	idleEvalCfg := provisioner.IdleEvaluatorConfig{
		DryRun: dryRunResult.DryRun,
		Pusher: idleEvalPusher,
	}

	// Template credential revalidation reconciler. A short scheduler poll
	// queries persisted due state and enqueues template_revalidate jobs for
	// active clone_with_customize templates whose last_validated_at is NULL or
	// older than the configured interval.
	// Enabled by default; set WORKER_L1_VALIDATION_ENABLED=false to opt out.
	// WORKER_L1_VALIDATION_INTERVAL overrides the default 168h (weekly) cadence.
	// WORKER_L1_VALIDATION_SCHEDULER_INTERVAL controls the persisted-state poll
	// cadence and defaults to 5m. It is intentionally much shorter than the
	// validation interval: process uptime is not the scheduling clock.
	l1ValidationEnabled := true
	if v := os.Getenv("WORKER_L1_VALIDATION_ENABLED"); v != "" {
		l1ValidationEnabled = strings.EqualFold(v, "true")
	}
	l1ValidationEnabled = cloneSchedulerEnabled(
		cfg.Provisioning.WorkerClaimsEnabled,
		l1ValidationEnabled,
	)
	l1ValidationInterval := envDuration(logger, "WORKER_L1_VALIDATION_INTERVAL", 168*time.Hour)
	l1ValidationSchedulerInterval := envDuration(logger, "WORKER_L1_VALIDATION_SCHEDULER_INTERVAL", 5*time.Minute)
	l1ValidationCfg := provisioner.L1TrustValidationReconcilerConfig{
		Interval: l1ValidationInterval,
	}
	var l1ValidationSchedulerMetrics *provisioner.L1ValidationSchedulerMetrics
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		l1ValidationSchedulerMetrics = provisioner.NewL1ValidationSchedulerMetrics(
			pgURL,
			job,
			map[string]string{"layer": "api"},
		)
	}
	var l1ValidationScheduler *provisioner.L1TrustValidationScheduler
	if l1ValidationEnabled {
		reconcile := func(runCtx context.Context) (provisioner.L1TrustValidationCounts, error) {
			var counts provisioner.L1TrustValidationCounts
			err := worklease.RunExclusive(runCtx, leaseStore, worklease.L1Validation, workerID, logger,
				func(leaseCtx context.Context, _ *worklease.Lease) error {
					cfg := l1ValidationCfg
					cfg.IsLeader = func() bool { return leaseCtx.Err() == nil }
					var e error
					counts, e = prov.ReconcileL1TrustValidation(leaseCtx, cfg)
					return e
				})
			if errors.Is(err, worklease.ErrNotClaimed) {
				return counts, provisioner.ErrReconcileNotOwned
			}
			return counts, err
		}
		l1ValidationScheduler = provisioner.NewL1TrustValidationScheduler(
			func() bool { return true }, reconcile, l1ValidationSchedulerMetrics, logger)
	}

	// Template health reconciler. Checks every student-visible template
	// structurally (vCenter object exists) on every 12-hour cycle, and performs
	// one full deep check (clone → power-on → wait-for-IP → destroy) per cycle
	// rotating across templates. Anti-flap: 3 retries with exponential backoff,
	// 2 consecutive-cycle confirmation before unhealthy. Immediate recovery.
	//
	// MUST be added to the leader-gated set when leader election lands
	// (parallel lane). For now the enabled flag defaults to false so deployers
	// opt in explicitly rather than hitting vCenter by surprise.
	//
	// Env knobs:
	//   WORKER_TEMPLATE_HEALTH_ENABLED=true          — opt in
	//   WORKER_TEMPLATE_HEALTH_INTERVAL              — default 12h
	//   WORKER_TEMPLATE_HEALTH_DEEP_TIMEOUT          — default 10m
	healthReconcilerEnabled := strings.EqualFold(os.Getenv("WORKER_TEMPLATE_HEALTH_ENABLED"), "true")
	healthReconcilerEnabled = cloneSchedulerEnabled(
		cfg.Provisioning.WorkerClaimsEnabled,
		healthReconcilerEnabled,
	)
	healthReconcilerInterval := envDuration(logger, "WORKER_TEMPLATE_HEALTH_INTERVAL", 12*time.Hour)
	healthReconcilerDeepTimeout := envDuration(logger, "WORKER_TEMPLATE_HEALTH_DEEP_TIMEOUT", 10*time.Minute)
	healthConfirmationBackoff := envDuration(logger, "WORKER_TEMPLATE_HEALTH_CONFIRMATION_BACKOFF", 5*time.Minute)
	healthConfirmationReconcileInterval := envDuration(logger, "WORKER_TEMPLATE_HEALTH_CONFIRMATION_RECONCILE_INTERVAL", time.Minute)
	var healthReconcilerPusher *provisioner.TemplateHealthPusher
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		job := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if job == "" {
			job = "crucible_provision_worker"
		}
		healthReconcilerPusher = provisioner.NewTemplateHealthPusher(pgURL, job,
			map[string]string{"layer": "api"})
	}
	healthReconcilerCfg := provisioner.TemplateHealthReconcilerConfig{
		Interval:            healthReconcilerInterval,
		DeepCheckTimeout:    healthReconcilerDeepTimeout,
		TemplateFolder:      cfg.VCenter.TemplatesFolder,
		MaxRetries:          3,
		RetryBaseDelay:      1 * time.Second,
		ConfirmationBackoff: healthConfirmationBackoff,
		Pusher:              healthReconcilerPusher,
	}
	prov.ConfigureTemplateHealth(healthReconcilerCfg)

	logger.Info("starting provision worker",
		"worker_id", workerID,
		"vcenter", cfg.VCenter.URL,
		"opnsense", cfg.OPNsense.BaseURL,
		"provisioning_claims_enabled", cfg.Provisioning.WorkerClaimsEnabled,
	)

	// Recover only jobs whose ownership heartbeat lease has expired.
	recovered, err := queries.RecoverStaleJobs(ctx, provisioner.JobLeaseDuration)
	if err != nil {
		logger.Error("failed to recover stale jobs", "error", err)
	} else if recovered > 0 {
		logger.Info("recovered stale jobs", "count", recovered)
	}

	// Retry any pods stuck in destroy_failed from previous runs (work-lease gated).
	runLeased(ctx, leaseStore, worklease.RetryFailedDestroys, workerID, logger, func(leaseCtx context.Context) error {
		prov.RetryFailedDestroys(leaseCtx)
		return nil
	})

	jobRuns := &jobRunner{}

	// Subscribe to job notifications from NATS
	_, err = natsClient.SubscribeJobCreated(func(jobID string, jobType string) {
		logger.Info("received job notification", "job_id", jobID, "type", jobType)
		jobRuns.Go(func() {
			processJobs(ctx, queries, prov, workerID, cfg.Provisioning.WorkerClaimsEnabled, logger)
		})
	})
	if err != nil {
		logger.Error("NATS subscription failed", "error", err)
		os.Exit(1)
	}

	// Polling fallback: check for jobs every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	leaseRecoveryTicker := time.NewTicker(provisioner.JobLeaseRecoveryInterval)
	defer leaseRecoveryTicker.Stop()

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
		// Leadership may already be held before the scheduler is initialized.
		// Observe current state directly instead of relying only on Changes().
		orphanScheduler.Start(ctx)
	}

	// Templates-folder orphan reconciler ticker (opt-in; nil-safe).
	var templateOrphanTickerC <-chan time.Time
	if templateOrphanEnabled {
		t := time.NewTicker(templateOrphanInterval)
		defer t.Stop()
		templateOrphanTickerC = t.C
		logger.Info("template orphan reconciler enabled",
			"folder", templateOrphanCfg.Folder,
			"interval", templateOrphanInterval,
			"inactive_age", templateOrphanInactiveAge,
			"min_age", templateOrphanMinAge)
		templateOrphanScheduler.Start(ctx)
	}

	// Pod-VM IP reconciler ticker (enabled by default; nil-safe).
	var ipReconcilerTickerC <-chan time.Time
	if ipReconcilerEnabled {
		t := time.NewTicker(ipReconcilerInterval)
		defer t.Stop()
		ipReconcilerTickerC = t.C
		logger.Info("pod-vm ip reconciler enabled", "interval", ipReconcilerInterval)
	}

	// OPNsense network reconciler ticker (enabled by default; nil-safe).
	var networkReconcilerTickerC <-chan time.Time
	if networkReconcilerEnabled {
		t := time.NewTicker(networkReconcilerInterval)
		defer t.Stop()
		networkReconcilerTickerC = t.C
		logger.Info("opnsense network reconciler enabled", "interval", networkReconcilerInterval)
		// Initial run is attempted via startupCatchup below (work-lease gated).
	}

	// Idle VM suspend evaluator ticker (disabled by default via dry-run; nil-safe).
	// IdleEval is held continuously so the same replica both evaluates and pushes
	// shared Pushgateway gauges (split leases would clobber last-run heartbeats).
	var idleEvalTickerC <-chan time.Time
	var holdIdle atomic.Bool
	var idleLeaseCtx atomic.Value // context.Context while IdleEval is held
	if idleEvalEnabled {
		t := time.NewTicker(idleEvalInterval)
		defer t.Stop()
		idleEvalTickerC = t.C
		logger.Info("idle vm evaluator enabled",
			"interval", idleEvalInterval,
			"dry_run", dryRunResult.DryRun)
		go holdWorkLease(ctx, leaseStore, worklease.IdleEval, workerID, logger, &holdIdle, &idleLeaseCtx,
			func(leaseCtx context.Context) {
				if _, err := prov.EvaluateIdleVMs(leaseCtx, idleEvalCfg); err != nil && leaseCtx.Err() == nil {
					logger.Error("idle vm evaluator catch-up failed", "error", err)
				}
			})
		if idleEvalPusher != nil {
			go idleEvalPusher.RunSuspendMetricsPusher(ctx, 30*time.Second, holdIdle.Load, logger)
		}
	}

	// L1 trust-validation scheduler ticker. This short poll is only a trigger;
	// each pass queries persisted last_validated_at state to decide what is due.
	var l1ValidationTickerC <-chan time.Time
	if l1ValidationEnabled {
		t := time.NewTicker(l1ValidationSchedulerInterval)
		defer t.Stop()
		l1ValidationTickerC = t.C
		logger.Info("l1 trust validation reconciler enabled",
			"validation_interval", l1ValidationInterval,
			"scheduler_interval", l1ValidationSchedulerInterval)
		// Do not rely on the acquisition event: leadership may already be held
		// by the time this goroutine is ready to consume Changes().
		l1ValidationScheduler.Start(ctx)
	}

	// Expiration cron ticker: work-lease gated in the select loop.
	expirationCronTicker := time.NewTicker(5 * time.Minute)
	defer expirationCronTicker.Stop()

	// Stuck-upload reconciler ticker: work-lease gated.
	var stuckUploadTickerC <-chan time.Time
	if cfg.ObjectStore.Endpoint != "" {
		t := time.NewTicker(stuckUploadInterval)
		defer t.Stop()
		stuckUploadTickerC = t.C
		logger.Info("stuck-upload reconciler enabled (work-lease gated)",
			"interval", stuckUploadInterval, "stale_threshold", stuckUploadStaleThreshold)
	}
	// Template health reconciler ticker (opt-in; nil-safe).
	var healthReconcilerTickerC <-chan time.Time
	var healthConfirmationTickerC <-chan time.Time
	if healthReconcilerEnabled {
		t := time.NewTicker(healthReconcilerInterval)
		defer t.Stop()
		healthReconcilerTickerC = t.C
		confirmationTicker := time.NewTicker(healthConfirmationReconcileInterval)
		defer confirmationTicker.Stop()
		healthConfirmationTickerC = confirmationTicker.C
		logger.Info("template health reconciler enabled",
			"interval", healthReconcilerInterval,
			"deep_timeout", healthReconcilerDeepTimeout,
			"confirmation_backoff", healthConfirmationBackoff,
			"confirmation_reconcile_interval", healthConfirmationReconcileInterval)
	}

	// Immediately process any pending/recovered jobs
	jobRuns.Go(func() {
		processJobs(ctx, queries, prov, workerID, cfg.Provisioning.WorkerClaimsEnabled, logger)
	})

	go func() {
		for {
			select {
			case <-ctx.Done():
				return

			// ── Job-claim loop: NOT gated by leader election ──────────────────
			// All replicas claim jobs via SELECT ... FOR UPDATE SKIP LOCKED.
			case <-ticker.C:
				jobRuns.Go(func() {
					processJobs(ctx, queries, prov, workerID, cfg.Provisioning.WorkerClaimsEnabled, logger)
				})

			case <-leaseRecoveryTicker.C:
				recovered, err := queries.RecoverStaleJobs(
					ctx,
					provisioner.JobLeaseDuration,
				)
				if err != nil {
					logger.Error("expired job lease recovery failed", "error", err)
				} else if recovered > 0 {
					logger.Warn("recovered expired job leases", "count", recovered)
					jobRuns.Go(func() {
						processJobs(ctx, queries, prov, workerID, cfg.Provisioning.WorkerClaimsEnabled, logger)
					})
				}

			// ── Periodic reconcilers: each named work-lease (DB claim+heartbeat) ──
			case <-retryTicker.C:
				runLeased(ctx, leaseStore, worklease.RetryFailedDestroys, workerID, logger, func(leaseCtx context.Context) error {
					prov.RetryFailedDestroys(leaseCtx)
					return nil
				})
			case <-expirationCronTicker.C:
				runLeased(ctx, leaseStore, worklease.ExpireStale, workerID, logger, func(leaseCtx context.Context) error {
					prov.ExpireStale(leaseCtx)
					return nil
				})
			case <-stuckUploadTickerC:
				runLeased(ctx, leaseStore, worklease.StuckUploads, workerID, logger, func(leaseCtx context.Context) error {
					_, err := prov.ReconcileStuckImageUploads(leaseCtx, stuckUploadStaleThreshold)
					return err
				})
			case <-orphanTickerC:
				orphanScheduler.Tick(ctx)
			case <-templateOrphanTickerC:
				templateOrphanScheduler.Tick(ctx)
			case <-ipReconcilerTickerC:
				runLeased(ctx, leaseStore, worklease.PodVMIP, workerID, logger, func(leaseCtx context.Context) error {
					_, err := prov.ReconcilePodVMIPs(leaseCtx, ipReconcilerCfg)
					return err
				})
			case <-networkReconcilerTickerC:
				runLeased(ctx, leaseStore, worklease.NetworkReconcile, workerID, logger, func(leaseCtx context.Context) error {
					_, err := prov.ReconcileNetwork(leaseCtx, networkReconcilerCfg)
					return err
				})
			case <-idleEvalTickerC:
				if !holdIdle.Load() {
					break
				}
				leaseCtx, _ := idleLeaseCtx.Load().(context.Context)
				if leaseCtx == nil || leaseCtx.Err() != nil {
					break
				}
				if _, err := prov.EvaluateIdleVMs(leaseCtx, idleEvalCfg); err != nil && leaseCtx.Err() == nil {
					logger.Error("idle vm evaluator failed", "error", err)
				}
			case <-templateReconcilerTickerC:
				runLeased(ctx, leaseStore, worklease.TemplateMetrics, workerID, logger, func(leaseCtx context.Context) error {
					if _, err := prov.ReconcileTemplateMetrics(leaseCtx, provisioner.TemplateReconcilerConfig{
						Interval:       templateReconcilerInterval,
						StaleThreshold: templateReconcilerStaleThreshold,
					}); err != nil {
						return err
					}
					return prov.ReconcileTemplateReplicaBuildMetrics(leaseCtx, templateReconcilerStaleThreshold)
				})
			case <-retryPendingTickerC:
				runLeased(ctx, leaseStore, worklease.RetryPending, workerID, logger, func(leaseCtx context.Context) error {
					return prov.ReconcileRetryPending(leaseCtx)
				})
			case <-l1ValidationTickerC:
				l1ValidationScheduler.Tick(ctx)

			case <-healthReconcilerTickerC:
				runLeased(ctx, leaseStore, worklease.TemplateHealth, workerID, logger, func(leaseCtx context.Context) error {
					_, err := prov.ReconcileTemplateHealth(leaseCtx, healthReconcilerCfg)
					return err
				})
			case <-healthConfirmationTickerC:
				runLeased(ctx, leaseStore, worklease.TemplateHealthConfirm, workerID, logger, func(leaseCtx context.Context) error {
					_, err := prov.ReconcileTemplateHealthConfirmations(leaseCtx, healthReconcilerCfg)
					return err
				})
			}
		}
	}()

	// Startup catch-up: claim each lease once so deploy/reboot does not wait a
	// full ticker interval. Losers of the claim no-op.
	go startupWorkLeaseCatchup(ctx, leaseStore, workerID, logger, prov, vcClient, cfg,
		networkReconcilerEnabled, networkReconcilerCfg,
		healthReconcilerEnabled, healthReconcilerCfg)

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down worker")
	shutdownDeadline := time.Now().Add(2 * time.Minute)
	cancel()
	if l1ValidationScheduler != nil {
		l1ValidationScheduler.Stop()
	}
	if orphanScheduler != nil {
		orphanScheduler.Stop()
	}
	if templateOrphanScheduler != nil {
		templateOrphanScheduler.Stop()
	}
	if !jobRuns.StopAndWait(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached; durable job recovery will resume unfinished work")
	}
	if l1ValidationScheduler != nil &&
		!l1ValidationScheduler.WaitTimeout(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached while waiting for L1 trust validation")
	}
	if orphanScheduler != nil &&
		!orphanScheduler.WaitTimeout(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached while waiting for orphan reconciliation")
	}
	if templateOrphanScheduler != nil &&
		!templateOrphanScheduler.WaitTimeout(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached while waiting for template orphan reconciliation")
	}
	if !closeBeforeDeadline(pool, time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached while closing database pool")
	}
}

type jobRunner struct {
	mu       sync.Mutex
	stopping bool
	wg       sync.WaitGroup
}

func (r *jobRunner) Go(run func()) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return false
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		run()
	}()
	return true
}

func (r *jobRunner) StopAndWait(timeout time.Duration) bool {
	r.mu.Lock()
	r.stopping = true
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

type closer interface {
	Close()
}

func closeBeforeDeadline(resource closer, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		resource.Close()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func cloneSchedulerEnabled(provisioningClaimsEnabled, configured bool) bool {
	return provisioningClaimsEnabled && configured
}

// processJobs claims and processes available jobs via the provisioner.
func processJobs(ctx context.Context, queries *database.Queries, prov *provisioner.Provisioner, workerID string, provisioningClaimsEnabled bool, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}

		claimID := newJobClaimID(workerID)
		job, err := queries.ClaimJob(ctx, claimID, provisioningClaimsEnabled)
		if err != nil {
			logger.Error("claim job failed", "error", err)
			return
		}
		if job == nil {
			return // no pending jobs
		}

		logger.Info("claimed job", "job_id", job.ID, "type", job.Type, "claim_id", claimID)

		if err := prov.ProcessJob(ctx, job); err != nil {
			logger.Error("job failed", "job_id", job.ID, "type", job.Type, "error", err)
		} else {
			logger.Info("job completed", "job_id", job.ID, "type", job.Type)
		}
	}
}

func newJobClaimID(workerID string) string {
	return workerID + ":" + uuid.NewString()
}

// envDuration reads a time.Duration from the environment, falling back to def
// when unset, unparseable, or non-positive. A bad value is logged and ignored
// rather than being fatal: a typo in one tuning knob must not stop the worker
// from starting and processing jobs.
func envDuration(logger *slog.Logger, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}

	parsed, err := time.ParseDuration(v)
	if err != nil || parsed <= 0 {
		logger.Warn("invalid duration in env; using default", "key", key, "value", v, "default", def, "error", err)
		return def
	}
	return parsed
}

func splitNonEmpty(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func alwaysWorkLeaseLeadership(ctx context.Context) func() provisioner.OrphanLeadershipState {
	return func() provisioner.OrphanLeadershipState {
		return provisioner.OrphanLeadershipState{
			IsLeader:   true,
			Generation: 1,
			Context:    ctx,
		}
	}
}

func runOrphanLeased(
	ctx context.Context,
	store worklease.Store,
	name, workerID string,
	logger *slog.Logger,
	fn func(context.Context) (provisioner.ReconcileCounts, error),
) (provisioner.ReconcileCounts, error) {
	var counts provisioner.ReconcileCounts
	err := worklease.RunExclusive(ctx, store, name, workerID, logger,
		func(leaseCtx context.Context, _ *worklease.Lease) error {
			var e error
			counts, e = fn(leaseCtx)
			return e
		})
	if errors.Is(err, worklease.ErrNotClaimed) {
		return counts, nil
	}
	return counts, err
}

func runLeased(
	ctx context.Context,
	store worklease.Store,
	name, workerID string,
	logger *slog.Logger,
	fn func(context.Context) error,
) {
	if err := worklease.TryRun(ctx, store, name, workerID, logger, func(leaseCtx context.Context, _ *worklease.Lease) error {
		return fn(leaseCtx)
	}); err != nil {
		logger.Error("leased reconciler failed", "name", name, "error", err)
	}
}

var cancelledLeaseCtx = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

func holdWorkLease(
	ctx context.Context,
	store worklease.Store,
	name, workerID string,
	logger *slog.Logger,
	held *atomic.Bool,
	leaseCtxOut *atomic.Value,
	onAcquire func(context.Context),
) {
	for ctx.Err() == nil {
		err := worklease.RunExclusive(ctx, store, name, workerID, logger,
			func(leaseCtx context.Context, _ *worklease.Lease) error {
				held.Store(true)
				if leaseCtxOut != nil {
					leaseCtxOut.Store(leaseCtx)
				}
				defer func() {
					held.Store(false)
					if leaseCtxOut != nil {
						leaseCtxOut.Store(cancelledLeaseCtx)
					}
				}()
				if onAcquire != nil {
					onAcquire(leaseCtx)
				}
				<-leaseCtx.Done()
				return nil
			})
		if errors.Is(err, worklease.ErrNotClaimed) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

func startupWorkLeaseCatchup(
	ctx context.Context,
	store worklease.Store,
	workerID string,
	logger *slog.Logger,
	prov *provisioner.Provisioner,
	vcClient *vcenter.Client,
	cfg *config.Config,
	networkEnabled bool,
	networkCfg provisioner.NetworkReconcilerConfig,
	healthEnabled bool,
	healthCfg provisioner.TemplateHealthReconcilerConfig,
) {
	runLeased(ctx, store, worklease.ExpireStale, workerID, logger, func(leaseCtx context.Context) error {
		prov.ExpireStale(leaseCtx)
		return nil
	})
	if networkEnabled {
		runLeased(ctx, store, worklease.NetworkReconcile, workerID, logger, func(leaseCtx context.Context) error {
			_, err := prov.ReconcileNetwork(leaseCtx, networkCfg)
			return err
		})
	}
	if healthEnabled {
		runLeased(ctx, store, worklease.TemplateHealth, workerID, logger, func(leaseCtx context.Context) error {
			orphanMinAge := time.Duration(healthCfg.MaxRetries+1) * healthCfg.DeepCheckTimeout
			if orphanMinAge < time.Hour {
				orphanMinAge = time.Hour
			}
			if _, err := vcClient.SweepHealthCheckOrphans(
				leaseCtx,
				cfg.VCenter.TemplatesFolder,
				time.Now().Add(-orphanMinAge),
			); err != nil {
				logger.Warn("health-check orphan sweep failed", "error", err)
			}
			if err := prov.ReplaceTemplateHealthSnapshot(leaseCtx, healthCfg); err != nil {
				return err
			}
			if _, err := prov.ReconcileTemplateHealthConfirmations(leaseCtx, healthCfg); err != nil {
				return err
			}
			counts, ran, err := prov.ReconcileTemplateHealthIfDue(leaseCtx, healthCfg)
			if err != nil {
				return err
			}
			if ran {
				logger.Info("template health catch-up cycle complete",
					"templates", counts.Templates,
					"healthy", counts.Healthy,
					"unhealthy", counts.Unhealthy,
					"deep_checked", counts.DeepChecked)
			}
			return nil
		})
	}
}

