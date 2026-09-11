// Package provisioner — pod-VM DHCP IP reconciler.
//
// Background: pod VMs get their address from DHCP on the pod VLAN. The
// provisioner records that address once, at create time, by polling
// WaitForIP (see create.go). Two things break that single capture:
//
//   - A slow first boot (Windows Server OOBE/specialize, especially) can
//     exceed the WaitForIP window, so the VM comes up a minute later with an
//     IP that was never written to pod_vms.ip_address. The UI gates the
//     "Access" button on a non-empty ip_address, so the VM looks unreachable
//     even though it's perfectly healthy.
//   - Because the lease is DHCP, the address can change at any time (renew to
//     a different offer, VLAN churn, guest restart). A one-time capture goes
//     stale silently.
//
// This reconciler runs on a short ticker in the provision-worker. For every
// running pod VM whose template assigns an IP, it reads the live guest address
// from vCenter and updates pod_vms.ip_address when it differs. It is
// deliberately conservative: it never blanks a recorded address on a transient
// empty read (VMware Tools briefly not reporting), only ever writing a
// non-empty, routable IPv4 that differs from what's stored.
package provisioner

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// IPReconcilerConfig configures one ReconcilePodVMIPs run.
type IPReconcilerConfig struct {
	// Pusher, if set, receives the run's IPReconcileCounts after the pass
	// completes. Push failures are logged but never returned.
	Pusher *IPReconcileCountPusher
}

// IPReconcileCounts summarises one IP-reconciler pass for logs and metrics.
type IPReconcileCounts struct {
	Candidates int // running, IP-assigned pod VMs examined
	Updated    int // ip_address written to a new live value
	Unchanged  int // live IP already matches what's stored
	NoIP       int // guest reported no usable IPv4 this pass (skipped)
	Errors     int // vCenter read or DB write failed for this VM
}

// ipReconcileVCenter is the vCenter subset the IP reconciler uses. The real
// *vcenter.Client satisfies it automatically; tests inject a fake.
type ipReconcileVCenter interface {
	GetGuestInfo(ctx context.Context, moref string) (*vcenter.GuestInfo, error)
}

// ipReconcileDB is the database subset the IP reconciler needs.
type ipReconcileDB interface {
	ListActivePodVMsForIPRefresh(ctx context.Context) ([]database.PodVMIPCandidate, error)
	UpdatePodVMIP(ctx context.Context, id uuid.UUID, ip string) error
}

// ReconcilePodVMIPs is the Provisioner-bound entry point. The provision-worker
// invokes this on a short ticker.
func (p *Provisioner) ReconcilePodVMIPs(ctx context.Context, cfg IPReconcilerConfig) (IPReconcileCounts, error) {
	return reconcilePodVMIPs(ctx, p.vc, p.db, p.logger, cfg)
}

// reconcilePodVMIPs is the pure implementation, factored out so tests can
// inject fakes for the vCenter client and the database.
func reconcilePodVMIPs(
	ctx context.Context,
	vc ipReconcileVCenter,
	db ipReconcileDB,
	logger *slog.Logger,
	cfg IPReconcilerConfig,
) (IPReconcileCounts, error) {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "podvm_ip_reconciler")

	cands, err := db.ListActivePodVMsForIPRefresh(ctx)
	if err != nil {
		return IPReconcileCounts{}, fmt.Errorf("list pod VMs for IP refresh: %w", err)
	}

	var counts IPReconcileCounts
	counts.Candidates = len(cands)

	for _, c := range cands {
		info, err := vc.GetGuestInfo(ctx, c.VCenterVMID)
		if err != nil {
			counts.Errors++
			log.Warn("ip reconcile: guest info read failed",
				"pod_vm_id", c.PodVMID, "moref", c.VCenterVMID, "error", err)
			continue
		}
		ip := info.IPAddress
		if ip == "" {
			// Tools not reporting a routable IPv4 right now; leave the stored
			// value alone rather than blanking a good address.
			counts.NoIP++
			continue
		}
		if ip == c.CurrentIP {
			counts.Unchanged++
			continue
		}
		if err := db.UpdatePodVMIP(ctx, c.PodVMID, ip); err != nil {
			counts.Errors++
			log.Warn("ip reconcile: update failed",
				"pod_vm_id", c.PodVMID, "moref", c.VCenterVMID, "new_ip", ip, "error", err)
			continue
		}
		counts.Updated++
		log.Info("ip reconcile: pod VM address refreshed",
			"pod_vm_id", c.PodVMID, "moref", c.VCenterVMID,
			"old_ip", c.CurrentIP, "new_ip", ip)
	}

	log.Info("pod vm ip reconcile complete",
		"candidates", counts.Candidates,
		"updated", counts.Updated,
		"unchanged", counts.Unchanged,
		"no_ip", counts.NoIP,
		"errors", counts.Errors,
	)

	if cfg.Pusher != nil {
		if pushErr := cfg.Pusher.Push(ctx, counts); pushErr != nil {
			log.Warn("ip-reconcile metric push failed", "error", pushErr)
		}
	}

	return counts, nil
}
