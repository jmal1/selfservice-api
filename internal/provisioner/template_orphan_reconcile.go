package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// DefaultTemplateOrphanInactiveAge is how long an error/draft wizard may sit
// with a leftover staging VM before the reconciler destroys it.
const DefaultTemplateOrphanInactiveAge = models.TemplateOrphanInactiveAge

// Template orphan disposable name prefixes. Inventory VMs without a DB owner
// are only auto-destroyed when the name matches one of these Crucible-owned
// conventions; everything else is metrics-only.
const (
	templateOrphanPrefixTpl         = "tpl-"
	templateOrphanPrefixHealthCheck = vcenter.HealthCheckClonePrefix // crucible-healthcheck-
	templateOrphanPrefixSmoke       = "smoke-"
)

// TemplateOrphanReconcilerConfig configures one ReconcileTemplateOrphans run.
type TemplateOrphanReconcilerConfig struct {
	// Folder is the Templates inventory path. Empty falls back to the
	// vCenter client's configured TemplateFolder, then to a safe empty
	// string that makes ListVMsInFolder fail closed.
	Folder string

	// InactiveAge gates the DB pass for error/draft rows with a leftover
	// moref. Empty falls back to DefaultTemplateOrphanInactiveAge (72h).
	InactiveAge time.Duration

	// MinAge is the inventory grace window for unmatched disposable VMs
	// (and unknown CreatedAt → retain). Empty falls back to 1 hour.
	MinAge time.Duration

	// Pusher, if set, receives the run's counts after the scan completes.
	Pusher *TemplateOrphanCountPusher
}

// TemplateOrphanCounts summarises one reconciler pass for logs and metrics.
type TemplateOrphanCounts struct {
	InventoryVMs       int
	StaleFailed        int // error-state DB candidates destroyed
	StaleDraft         int // draft-state DB candidates destroyed
	InventoryDestroyed int // unmatched disposable inventory destroys
	SkippedOwned       int
	SkippedRecent      int
	UnknownDryRun      int
	// SkippedPoweredOn counts DB candidates left alone because the staging VM
	// was still running, and SkippedUndetermined those whose power state could
	// not be read. Both need an operator, so they are surfaced rather than
	// folded into the generic skip counters.
	SkippedPoweredOn    int
	SkippedUndetermined int
	Destroyed           int
	DestroyFailures     int
}

type templateOrphanVCenter interface {
	ListVMsInFolder(ctx context.Context, folderPath string) ([]vcenter.FolderVM, error)
	PowerOffVM(ctx context.Context, moref string) error
	DestroyVM(ctx context.Context, moref string) error
	// GetGuestInfo is a single non-blocking property read. The DB pass uses it
	// to avoid destroying a staging VM that is still running — see the guard in
	// reconcileTemplateOrphans.
	GetGuestInfo(ctx context.Context, moref string) (*vcenter.GuestInfo, error)
}

type templateOrphanDB interface {
	ListStaleWizardTemplateVMs(ctx context.Context, olderThan time.Duration) ([]database.StaleWizardTemplateVM, error)
	ClaimStaleWizardTemplateVM(ctx context.Context, row database.StaleWizardTemplateVM) (bool, error)
	ListOwnedTemplateFolderMorefs(ctx context.Context) (map[string]struct{}, error)
	ClearTemplateVCenterVMIfMatch(ctx context.Context, id uuid.UUID, expectedMoref string) error
}

// ReconcileTemplateOrphans is the Provisioner-bound entry point.
func (p *Provisioner) ReconcileTemplateOrphans(ctx context.Context, cfg TemplateOrphanReconcilerConfig) (TemplateOrphanCounts, error) {
	return reconcileTemplateOrphans(ctx, p.vc, p.db, p.logger, cfg, time.Now)
}

func reconcileTemplateOrphans(
	ctx context.Context,
	vc templateOrphanVCenter,
	db templateOrphanDB,
	logger *slog.Logger,
	cfg TemplateOrphanReconcilerConfig,
	now func() time.Time,
) (TemplateOrphanCounts, error) {
	if cfg.InactiveAge <= 0 {
		cfg.InactiveAge = DefaultTemplateOrphanInactiveAge
	}
	if cfg.MinAge <= 0 {
		cfg.MinAge = time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "template_orphan_reconciler", "folder", cfg.Folder)

	var counts TemplateOrphanCounts

	stale, err := db.ListStaleWizardTemplateVMs(ctx, cfg.InactiveAge)
	if err != nil {
		return counts, fmt.Errorf("list stale wizard templates: %w", err)
	}
	for _, row := range stale {
		claimed, claimErr := db.ClaimStaleWizardTemplateVM(ctx, row)
		if claimErr != nil {
			counts.DestroyFailures++
			log.Warn("template orphan: claim failed", "template_id", row.ID, "error", claimErr)
			continue
		}
		if !claimed {
			counts.SkippedRecent++
			continue
		}
		poweredOn, perr := templateOrphanVMPoweredOn(ctx, vc, row.VCenterVMID)
		if perr != nil {
			counts.SkippedUndetermined++
			log.Warn("template orphan: cannot determine staging VM power state; leaving it in place",
				"template_id", row.ID, "name", row.Name, "state", row.State,
				"moref", row.VCenterVMID, "error", perr)
			continue
		}
		if poweredOn {
			log.Warn("template orphan: stale powered-on wizard VM exceeded 72h claim window; powering off before destroy",
				"template_id", row.ID, "name", row.Name, "state", row.State,
				"moref", row.VCenterVMID, "updated_at", row.UpdatedAt)
		}
		log.Info("template orphan: destroying stale wizard staging VM",
			"template_id", row.ID, "name", row.Name, "state", row.State,
			"moref", row.VCenterVMID, "updated_at", row.UpdatedAt)
		if err := destroyTemplateOrphanVM(ctx, vc, row.VCenterVMID); err != nil {
			counts.DestroyFailures++
			log.Warn("template orphan: destroy stale wizard VM failed",
				"template_id", row.ID, "moref", row.VCenterVMID, "error", err)
			continue
		}
		if err := db.ClearTemplateVCenterVMIfMatch(ctx, row.ID, row.VCenterVMID); err != nil {
			counts.DestroyFailures++
			log.Warn("template orphan: clear vcenter_vm_id failed after destroy",
				"template_id", row.ID, "moref", row.VCenterVMID, "error", err)
			continue
		}
		counts.Destroyed++
		switch row.State {
		case models.TemplateStateError:
			counts.StaleFailed++
		case models.TemplateStateDraft:
			counts.StaleDraft++
		}
	}

	if cfg.Folder == "" {
		log.Warn("template orphan: Templates folder unset; skipping inventory pass")
	} else {
		vms, err := vc.ListVMsInFolder(ctx, cfg.Folder)
		if err != nil {
			return counts, fmt.Errorf("list Templates folder %q: %w", cfg.Folder, err)
		}
		owned, err := db.ListOwnedTemplateFolderMorefs(ctx)
		if err != nil {
			return counts, fmt.Errorf("list owned template morefs: %w", err)
		}
		counts.InventoryVMs = len(vms)
		cutoff := now().Add(-cfg.MinAge)

		for _, vm := range vms {
			if _, ok := owned[vm.MoRef]; ok {
				counts.SkippedOwned++
				continue
			}
			if !isDisposableTemplateOrphanName(vm.Name) {
				counts.UnknownDryRun++
				log.Warn("template orphan: unmatched Templates VM (dry-run)",
					"moref", vm.MoRef, "name", vm.Name, "power_state", vm.PowerState)
				continue
			}
			// Known CreatedAt inside the MinAge window → defer. Unknown age is
			// eligible: disposable prefix is the safety gate, and deleted-template
			// leftovers often lack a reliable inventiry timestamp.
			if vm.CreatedAt != nil && vm.CreatedAt.After(cutoff) {
				counts.SkippedRecent++
				log.Debug("template orphan: disposable inventory VM within MinAge; deferring",
					"moref", vm.MoRef, "name", vm.Name, "created_at", vm.CreatedAt)
				continue
			}

			log.Info("template orphan: destroying unmatched disposable inventory VM",
				"moref", vm.MoRef, "name", vm.Name)
			if err := destroyTemplateOrphanVM(ctx, vc, vm.MoRef); err != nil {
				counts.DestroyFailures++
				log.Warn("template orphan: inventory destroy failed",
					"moref", vm.MoRef, "name", vm.Name, "error", err)
				continue
			}
			counts.Destroyed++
			counts.InventoryDestroyed++
		}
	}

	log.Info("template orphan reconcile complete",
		"inventory", counts.InventoryVMs,
		"stale_failed", counts.StaleFailed,
		"stale_draft", counts.StaleDraft,
		"inventory_destroyed", counts.InventoryDestroyed,
		"skipped_owned", counts.SkippedOwned,
		"skipped_recent", counts.SkippedRecent,
		"skipped_powered_on", counts.SkippedPoweredOn,
		"skipped_undetermined", counts.SkippedUndetermined,
		"unknown_dryrun", counts.UnknownDryRun,
		"destroyed", counts.Destroyed,
		"destroy_failures", counts.DestroyFailures,
	)

	if cfg.Pusher != nil {
		if pushErr := cfg.Pusher.Push(ctx, cfg.Folder, counts); pushErr != nil {
			log.Warn("template-orphan metric push failed", "error", pushErr)
		}
	}
	return counts, nil
}

func destroyTemplateOrphanVM(ctx context.Context, vc templateOrphanVCenter, moref string) error {
	_ = vc.PowerOffVM(ctx, moref)
	return vc.DestroyVM(ctx, moref)
}

// templateOrphanVMPoweredOn reports whether the staging VM is running. A VM
// that no longer exists is reported as not-powered-on so the DB pass proceeds
// and clears the dangling moref instead of skipping the row forever.
func templateOrphanVMPoweredOn(ctx context.Context, vc templateOrphanVCenter, moref string) (bool, error) {
	info, err := vc.GetGuestInfo(ctx, moref)
	if err != nil {
		if vcenter.IsVMNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	if info == nil {
		return false, nil
	}
	return info.PoweredOn, nil
}

func isDisposableTemplateOrphanName(name string) bool {
	return strings.HasPrefix(name, templateOrphanPrefixTpl) ||
		strings.HasPrefix(name, templateOrphanPrefixHealthCheck) ||
		strings.HasPrefix(name, templateOrphanPrefixSmoke)
}
