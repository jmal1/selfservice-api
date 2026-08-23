package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/config"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/leader"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/objectstore"
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
	queries := database.NewQueries(pool)

	// Leader election: gate all periodic reconcilers behind a session-scoped
	// Postgres advisory lock so exactly one replica runs them at a time.
	// The job-claim loop (processJobs) is intentionally NOT gated — all replicas
	// must claim jobs via SELECT ... FOR UPDATE SKIP LOCKED.
	//
	// Set WORKER_LEADER_ELECTION_ENABLED=false to disable (single-replica mode).
	leaderElectionEnabled := true
	if v := os.Getenv("WORKER_LEADER_ELECTION_ENABLED"); v != "" {
		leaderElectionEnabled = strings.EqualFold(v, "true")
	}
	leaderRetry := envDuration(logger, "WORKER_LEADER_RETRY_INTERVAL", 5*time.Second)

	elec := leader.NewPostgresElector(cfg.Database.DSN(), leader.WorkerLockKey, leaderRetry, logger)
	if !leaderElectionEnabled {
		// When disabled, behave as a permanent leader (single-replica operation).
		logger.Warn("leader election disabled; this replica will run all reconcilers unconditionally")
		elec = leader.NewAlwaysLeader(logger)
	}
	go elec.Run(ctx)

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
	}
	opnClient := opnsense.New(opnCfg, logger)
	opnSSH := opnsense.NewSSHClient(opnCfg, logger)

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

	// Leader metrics: push crucible_worker_is_leader and
	// crucible_worker_leader_transitions_total to Pushgateway, labelled by pod.
	// This feeds the CrucibleWorkerNoLeader alert (sum != 1 for 15m).
	if pgURL := os.Getenv("WORKER_PUSHGATEWAY_URL"); pgURL != "" {
		workerJob := os.Getenv("WORKER_PUSHGATEWAY_JOB")
		if workerJob == "" {
			workerJob = "crucible_provision_worker"
		}
		podName, _ := os.Hostname()
		leaderPusher := &leader.Pusher{
			BaseURL: pgURL,
			Job:     workerJob,
			Pod:     podName,
		}
		go leaderPusher.RunPusher(ctx, elec, 30*time.Second)
		logger.Info("leader metrics pusher enabled", "pod", podName, "url", pgURL)
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

	// L1 trust-tier revalidation reconciler. A short scheduler poll queries
	// persisted due state and enqueues
	// template_revalidate jobs for active L1 templates whose last_validated_at
	// is NULL or older than the configured interval.
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
		IsLeader: elec.IsLeader,
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
			return prov.ReconcileL1TrustValidation(runCtx, l1ValidationCfg)
		}
		if l1ValidationSchedulerMetrics == nil {
			l1ValidationScheduler = provisioner.NewL1TrustValidationScheduler(
				elec.IsLeader, reconcile, nil, logger)
		} else {
			l1ValidationScheduler = provisioner.NewL1TrustValidationScheduler(
				elec.IsLeader, reconcile, l1ValidationSchedulerMetrics, logger)
		}
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

	// Worker ID for job claiming
	workerHost, err := os.Hostname()
	if err != nil {
		workerHost = "worker-unknown"
	}
	workerID := workerHost + "-" + uuid.NewString()

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

	// Retry any pods stuck in destroy_failed from previous runs
	prov.RetryFailedDestroys(ctx)

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
		// Initial run: handled via elec.Changes() in the main select loop below
		// so the first pass fires as soon as leadership is elected, not speculatively
		// before the lock is acquired.
	}

	// Idle VM suspend evaluator ticker (disabled by default via dry-run; nil-safe).
	var idleEvalTickerC <-chan time.Time
	if idleEvalEnabled {
		t := time.NewTicker(idleEvalInterval)
		defer t.Stop()
		idleEvalTickerC = t.C
		logger.Info("idle vm evaluator enabled",
			"interval", idleEvalInterval,
			"dry_run", dryRunResult.DryRun)
		if idleEvalPusher != nil {
			// Leader-gate the push: only the replica that runs the evaluator
			// (and thus advances the last-run timestamp) may write the shared
			// Pushgateway grouping key. See RunSuspendMetricsPusher for why a
			// non-leader push pins crucible_idle_evaluator_last_run_timestamp
			// to a stale start-time seed and makes CrucibleIdleEvaluatorStale
			// fire forever.
			go idleEvalPusher.RunSuspendMetricsPusher(ctx, 30*time.Second, elec.IsLeader, logger)
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

	// Expiration cron ticker: gated by leader election (integrated into the main
	// select loop below). ExpireStale is called immediately on leadership
	// acquisition via elec.Changes(), then on each 5-minute tick.
	expirationCronTicker := time.NewTicker(5 * time.Minute)
	defer expirationCronTicker.Stop()

	// Stuck-upload reconciler ticker: gated by leader election (integrated into
	// the main select loop below). Only active when an object store is configured.
	var stuckUploadTickerC <-chan time.Time
	if cfg.ObjectStore.Endpoint != "" {
		t := time.NewTicker(stuckUploadInterval)
		defer t.Stop()
		stuckUploadTickerC = t.C
		logger.Info("stuck-upload reconciler enabled (leader-gated)",
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

			// ── Periodic reconcilers: ALL gated by leader election ─────────────
			// When not leader the tick fires but the body is a cheap no-op.
			// This keeps the ticker alive so the leader can take over without
			// needing a restart.
			case <-retryTicker.C:
				if !elec.IsLeader() {
					continue
				}
				prov.RetryFailedDestroys(ctx)
			case <-expirationCronTicker.C:
				if !elec.IsLeader() {
					continue
				}
				prov.ExpireStale(ctx)
			case <-stuckUploadTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileStuckImageUploads(ctx, stuckUploadStaleThreshold); err != nil {
					logger.Error("stuck-upload reconcile failed", "error", err)
				}
			case <-orphanTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileVCenterOrphans(ctx, orphanCfg); err != nil {
					logger.Error("orphan reconcile failed", "error", err)
				}
			case <-ipReconcilerTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcilePodVMIPs(ctx, ipReconcilerCfg); err != nil {
					logger.Error("pod-vm ip reconcile failed", "error", err)
				}
			case <-networkReconcilerTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileNetwork(ctx, networkReconcilerCfg); err != nil {
					logger.Error("network reconcile failed", "error", err)
				}
			case <-idleEvalTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.EvaluateIdleVMs(ctx, idleEvalCfg); err != nil {
					logger.Error("idle vm evaluation failed", "error", err)
				}
			case <-templateReconcilerTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileTemplateMetrics(ctx, provisioner.TemplateReconcilerConfig{
					Interval:       templateReconcilerInterval,
					StaleThreshold: templateReconcilerStaleThreshold,
				}); err != nil {
					logger.Error("template reconcile failed", "error", err)
				}
			case <-retryPendingTickerC:
				if !elec.IsLeader() {
					continue
				}
				if err := prov.ReconcileRetryPending(ctx); err != nil {
					logger.Error("retry-pending reconcile failed", "error", err)
				}
			case <-l1ValidationTickerC:
				l1ValidationScheduler.Tick(ctx)

			case <-healthReconcilerTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileTemplateHealth(ctx, healthReconcilerCfg); err != nil {
					logger.Error("template health reconcile failed", "error", err)
				}
			case <-healthConfirmationTickerC:
				if !elec.IsLeader() {
					continue
				}
				if _, err := prov.ReconcileTemplateHealthConfirmations(ctx, healthReconcilerCfg); err != nil {
					logger.Error("template health confirmation schedule reconcile failed", "error", err)
				}

			// ── Leadership change notification ────────────────────────────────
			// elec.Changes() fires true when this replica acquires the lock
			// (startup or failover) and false when it loses it. On acquisition
			// we run immediate passes for any reconcilers that need prompt
			// startup behaviour — equivalent to the old "initial run" goroutines
			// but racefree because leadership is confirmed before we reach here.
			// The channel is buffered (size 1), so the event is safe even if the
			// select loop is busy; it will be delivered on the next iteration.
			case isLeader := <-elec.Changes():
				if l1ValidationScheduler != nil {
					l1ValidationScheduler.LeadershipChanged(ctx, isLeader)
				}
				if !isLeader {
					continue
				}
				// Immediately expire any stale pods now that we are leader.
				// This is fast (DB-only) so we run it inline.
				prov.ExpireStale(ctx)
				// Network reconcile touches the OPNsense API — run in a goroutine
				// so it cannot stall the select loop on a slow firewall response.
				if networkReconcilerEnabled {
					go func() {
						if _, err := prov.ReconcileNetwork(ctx, networkReconcilerCfg); err != nil {
							logger.Error("network reconcile on leader acquisition failed", "error", err)
						}
					}()
				}
				// Health-check clones from a crashed previous leader are swept
				// here rather than at startup so exactly one replica does it.
				// The catch-up cycle runs after the sweep, in the same
				// goroutine, so it can never race the sweep into deleting its
				// own in-flight clone.
				if healthReconcilerEnabled {
					go func() {
						orphanMinAge := time.Duration(healthReconcilerCfg.MaxRetries+1) * healthReconcilerCfg.DeepCheckTimeout
						if orphanMinAge < time.Hour {
							orphanMinAge = time.Hour
						}
						if _, err := vcClient.SweepHealthCheckOrphans(
							ctx,
							cfg.VCenter.TemplatesFolder,
							time.Now().Add(-orphanMinAge),
						); err != nil {
							logger.Warn("health-check orphan sweep failed", "error", err)
						}
						// Replace any series retained from the previous worker
						// before IfDue can skip a fresh vCenter cycle.
						if err := prov.ReplaceTemplateHealthSnapshot(ctx, healthReconcilerCfg); err != nil {
							logger.Error("template health startup snapshot replacement failed", "error", err)
						}
						if _, err := prov.ReconcileTemplateHealthConfirmations(ctx, healthReconcilerCfg); err != nil {
							logger.Error("template health confirmation schedule repair on leader acquisition failed", "error", err)
						}
						// The 12h ticker is created at process start and reset
						// by every restart. This service deploys several times
						// a day, so without a catch-up pass the reconciler
						// would never actually fire. IfDue consults persisted
						// state, so frequent deploys do not each trigger a
						// vCenter clone.
						counts, ran, err := prov.ReconcileTemplateHealthIfDue(ctx, healthReconcilerCfg)
						if err != nil {
							logger.Error("template health catch-up on leader acquisition failed", "error", err)
							return
						}
						if ran {
							logger.Info("template health catch-up cycle complete",
								"templates", counts.Templates,
								"healthy", counts.Healthy,
								"unhealthy", counts.Unhealthy,
								"deep_checked", counts.DeepChecked)
						} else {
							logger.Info("template health catch-up skipped; a cycle ran within the interval",
								"interval", healthReconcilerCfg.Interval)
						}
					}()
				}
				// Idle-VM evaluator: run one pass immediately on leadership
				// acquisition. The 15m ticker is created at process start and is
				// not reset by failover, so a newly-elected leader would wait up
				// to a full interval before its first pass. During that window it
				// keeps pushing its stale process-start seed for
				// crucible_idle_evaluator_last_run_timestamp, which can trip
				// CrucibleIdleEvaluatorStale on a mid-life failover. Running a
				// pass now refreshes the heartbeat immediately. Goroutine because
				// it touches vCenter and must not stall the select loop.
				if idleEvalEnabled {
					go func() {
						if _, err := prov.EvaluateIdleVMs(ctx, idleEvalCfg); err != nil {
							logger.Error("idle vm evaluation on leader acquisition failed", "error", err)
						}
					}()
				}

			}
		}
	}()

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
	if !jobRuns.StopAndWait(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached; durable job recovery will resume unfinished work")
	}
	if l1ValidationScheduler != nil &&
		!l1ValidationScheduler.WaitTimeout(time.Until(shutdownDeadline)) {
		logger.Error("worker shutdown deadline reached while waiting for L1 trust validation")
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
