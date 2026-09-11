// Package provisioner — vCenter folder orphan reconciler.
//
// Background: a VM directory can survive on the configured datastore even
// after the Crucible pod that owned it is destroyed in the database. The
// existing destroy path is MoRef/DB-driven (see destroy.go RetryFailedDestroys)
// and cannot find a VM whose pod_vms row was hard-deleted, whose destroy task
// reported success while the actual datastore files survived (e.g. NFS APD
// during destroy), or whose pod row was wiped without the vCenter delete
// completing.
//
// The reconciler scans a single vCenter VM folder, correlates each inventory
// VM by MoRef against pod_vms, and acts conservatively:
//
//   - Linked to an active pod         → leave alone
//   - Linked to a terminal pod whose
//     name matches the synthetic
//     prefix (default "synthetic-noop-")
//     and pod is older than MinAge   → power off + destroy
//   - Linked to a terminal pod, non-
//     synthetic name                  → log + count (dry-run, operator review)
//   - No DB row at all                → log + count (dry-run, may be a manual VM)
//
// All counts are emitted as a single Prometheus gauge family via
// OrphanCountPusher → Pushgateway, so a steady-state non-zero "unknown" or
// "destroyed_other" count is alertable without ever auto-deleting a VM the
// operator didn't intend to delete.
package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// DefaultSyntheticPodNamePrefix matches the prefix used by the in-process
// synthetic check (see internal/synthetic/checks/lifecycle.go). Any vCenter
// VM whose linked pod name starts with this prefix is safe to auto-destroy
// once the pod itself has reached a terminal state.
const DefaultSyntheticPodNamePrefix = "synthetic-noop-"

// OrphanReconcilerConfig configures one ReconcileVCenterOrphans run.
type OrphanReconcilerConfig struct {
	// Folder is the vCenter folder path or short name to scan. Empty
	// falls back to "Student-VMs" — the production default that matches
	// VCENTER_VM_FOLDER's default in internal/config/config.go.
	Folder string

	// SyntheticPodNamePrefix gates auto-destroy. Empty falls back to
	// DefaultSyntheticPodNamePrefix. A VM whose linked pod's name does
	// NOT start with this prefix is never auto-destroyed; it's logged
	// and counted as DestroyedOther for operator review.
	SyntheticPodNamePrefix string

	// MinAge is a safety window. A VM whose linked pod transitioned to
	// a terminal state within MinAge is skipped (left for the in-flight
	// destroy path to finish). Likewise, a freshly-created VM with no
	// DB row at all is left alone until at least MinAge has passed since
	// the inventory scan — to avoid trampling a clone whose pod_vms row
	// is mid-update. Empty falls back to 1 hour.
	MinAge time.Duration

	// Pusher, if set, receives the run's ReconcileCounts after the scan
	// completes. Push failures are logged but never returned.
	Pusher *OrphanCountPusher
}

// ReconcileCounts summarises one reconciler pass for both logs and metrics.
type ReconcileCounts struct {
	InventoryVMs       int
	Active             int
	DestroyedSynthetic int // would-be auto-destroy targets
	DestroyedOther     int // dry-run only (operator review)
	Unknown            int // no pod_vms row at all (dry-run only)
	SkippedRecent      int // within MinAge grace window
	Destroyed          int // successfully destroyed this run
	DestroyFailures    int // attempted but failed
}

// orphanVCenter is the small subset of *vcenter.Client the reconciler uses.
// Defining it here keeps the production wiring trivial (the real client
// satisfies the interface automatically) while letting tests inject a fake
// without standing up a govmomi simulator.
type orphanVCenter interface {
	ListVMsInFolder(ctx context.Context, folderPath string) ([]vcenter.FolderVM, error)
	PowerOffVM(ctx context.Context, moref string) error
	DestroyVM(ctx context.Context, moref string) error
}

// orphanDB is the database subset the reconciler needs.
type orphanDB interface {
	ListPodVMLinksByVCenterID(ctx context.Context) (map[string]database.PodVMLink, error)
}

// ReconcileVCenterOrphans is the Provisioner-bound entry point. Production
// callers (cmd/provision-worker) invoke this on a long ticker.
func (p *Provisioner) ReconcileVCenterOrphans(ctx context.Context, cfg OrphanReconcilerConfig) (ReconcileCounts, error) {
	return reconcileVCenterOrphans(ctx, p.vc, p.db, p.logger, cfg, time.Now)
}

// reconcileVCenterOrphans is the pure implementation, factored out so tests
// can inject fakes for the vCenter client, the database, and the clock.
func reconcileVCenterOrphans(
	ctx context.Context,
	vc orphanVCenter,
	db orphanDB,
	logger *slog.Logger,
	cfg OrphanReconcilerConfig,
	now func() time.Time,
) (ReconcileCounts, error) {
	if cfg.Folder == "" {
		cfg.Folder = "Student-VMs"
	}
	if cfg.SyntheticPodNamePrefix == "" {
		cfg.SyntheticPodNamePrefix = DefaultSyntheticPodNamePrefix
	}
	if cfg.MinAge <= 0 {
		cfg.MinAge = 1 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "vcenter_orphan_reconciler", "folder", cfg.Folder)

	vms, err := vc.ListVMsInFolder(ctx, cfg.Folder)
	if err != nil {
		return ReconcileCounts{}, fmt.Errorf("list vCenter folder %q: %w", cfg.Folder, err)
	}
	links, err := db.ListPodVMLinksByVCenterID(ctx)
	if err != nil {
		return ReconcileCounts{}, fmt.Errorf("list pod_vms by vcenter_vm_id: %w", err)
	}

	var counts ReconcileCounts
	counts.InventoryVMs = len(vms)
	cutoff := now().Add(-cfg.MinAge)

	for _, vm := range vms {
		link, hasLink := links[vm.MoRef]

		if !hasLink {
			// No DB row at all — could be a manually-created VM, a leftover
			// from a long-ago hard-deleted pod, or (worst case) a clone whose
			// pod_vms row hasn't been written yet. Never auto-destroy: just
			// log and surface the count to operators.
			counts.Unknown++
			log.Warn("orphan: vCenter VM has no pod_vms row (dry-run)",
				"moref", vm.MoRef, "name", vm.Name, "power_state", vm.PowerState)
			continue
		}

		if link.PodStatus != "destroyed" && link.PodStatus != "destroy_failed" {
			counts.Active++
			continue
		}

		// Pod is terminal. Honor MinAge so an in-flight destroy job has time
		// to finish without the reconciler racing against it.
		if link.PodUpdatedAt.After(cutoff) {
			counts.SkippedRecent++
			log.Debug("orphan: pod recently transitioned; deferring",
				"moref", vm.MoRef, "name", vm.Name,
				"pod_status", link.PodStatus, "pod_updated_at", link.PodUpdatedAt)
			continue
		}

		if strings.HasPrefix(link.PodName, cfg.SyntheticPodNamePrefix) {
			counts.DestroyedSynthetic++
			log.Info("orphan: destroying synthetic VM linked to terminal pod",
				"moref", vm.MoRef, "name", vm.Name,
				"pod_name", link.PodName, "pod_status", link.PodStatus)
			if err := destroyOrphanVM(ctx, vc, vm.MoRef); err != nil {
				counts.DestroyFailures++
				log.Warn("orphan: destroy failed",
					"moref", vm.MoRef, "name", vm.Name, "error", err)
				continue
			}
			counts.Destroyed++
			continue
		}

		// Non-synthetic pod with terminal status but the on-disk VM survived.
		// This is the exact failure mode that produced the 1a257d-* and
		// 7581c5-* dirs in March 2026. Surface it via the metric and rely
		// on operator review instead of auto-destruction — non-synthetic
		// pods may have student data we don't want to lose to a buggy sweep.
		counts.DestroyedOther++
		log.Warn("orphan: non-synthetic VM linked to terminal pod (dry-run)",
			"moref", vm.MoRef, "name", vm.Name,
			"pod_name", link.PodName, "pod_status", link.PodStatus)
	}

	log.Info("orphan reconcile complete",
		"inventory", counts.InventoryVMs,
		"active", counts.Active,
		"destroyed_synthetic", counts.DestroyedSynthetic,
		"destroyed_other_dryrun", counts.DestroyedOther,
		"unknown_dryrun", counts.Unknown,
		"skipped_recent", counts.SkippedRecent,
		"destroyed", counts.Destroyed,
		"destroy_failures", counts.DestroyFailures,
	)

	if cfg.Pusher != nil {
		if pushErr := cfg.Pusher.Push(ctx, cfg.Folder, counts); pushErr != nil {
			log.Warn("orphan-count metric push failed", "error", pushErr)
		}
	}

	return counts, nil
}

// destroyOrphanVM powers off then destroys. PowerOff is best-effort because
// the VM may already be off; DestroyVM is the operation we actually care
// about and its error is the one we return.
func destroyOrphanVM(ctx context.Context, vc orphanVCenter, moref string) error {
	_ = vc.PowerOffVM(ctx, moref) // best-effort; DestroyVM will fail loudly if VM is still running
	return vc.DestroyVM(ctx, moref)
}
