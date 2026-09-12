package provisioner

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// DestroyPodPayload is the expected shape of job.Payload for pod_destroy.
type DestroyPodPayload struct {
	PodID  string `json:"pod_id"`
	Reason string `json:"reason,omitempty"`
}

// DestroyPod executes the pod destruction workflow.
// Destruction is best-effort — we continue even if individual steps fail
// because we want to clean up as much as possible.
// If vCenter operations fail, the pod is marked "destroy_failed" so the
// retry sweep can attempt cleanup again later.
func (p *Provisioner) DestroyPod(ctx context.Context, job *models.Job) error {
	var payload DestroyPodPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse pod_destroy payload: %w", err)
	}

	podID, err := parseUUID(payload.PodID)
	if err != nil {
		return fmt.Errorf("invalid pod_id: %w", err)
	}
	claimOwner, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	if payload.Reason == "suspended_too_long" {
		eligible, checkErr := p.db.PodVMsAllSuspendedPastRetention(ctx, podID)
		if checkErr != nil {
			return fmt.Errorf("recheck suspended-too-long pod: %w", checkErr)
		}
		if !eligible {
			return fmt.Errorf("suspended-too-long destroy no longer eligible: %w", database.ErrPodDestroyNotNeeded)
		}
	}

	pod, err := p.db.PreparePodDestroy(ctx, podID, job.ID, claimOwner)
	if err != nil {
		if stderrors.Is(err, database.ErrPodAlreadyDestroyed) ||
			stderrors.Is(err, database.ErrPodDestroyJobObsolete) {
			p.logger.Info("pod destroy job has no remaining work", "pod_id", podID, "job_id", job.ID, "reason", err)
			return nil
		}
		return fmt.Errorf("prepare pod destroy: %w", err)
	}

	vlanTag := pod.VLANID
	pgName := fmt.Sprintf("Pod-VLAN%d", vlanTag)
	subnet := pod.Subnet
	var errors []error

	p.publishProgress(job.ID, "destroying", "Starting pod destruction")

	// --- Step 1: Power off all VMs ---
	p.publishProgress(job.ID, "vm_poweroff", "Powering off all VMs")
	vms, err := p.db.ListPodVMs(ctx, pod.ID)
	if err != nil {
		errors = append(errors, fmt.Errorf("list pod VMs: %w", err))
	} else {
		for _, vm := range vms {
			if vm.VCenterVMID != nil && *vm.VCenterVMID != "" {
				if err := p.vc.PowerOffVM(ctx, *vm.VCenterVMID); err != nil {
					p.logger.Warn("failed to power off VM", "moref", *vm.VCenterVMID, "error", err)
					// Continue — VM may already be off
				}
			}
		}
	}

	// --- Step 2: Destroy all VMs ---
	p.publishProgress(job.ID, "vm_destroy", "Destroying all VMs")
	if vms != nil {
		for _, vm := range vms {
			if vm.VCenterVMID != nil && *vm.VCenterVMID != "" {
				if err := p.vc.DestroyVM(ctx, *vm.VCenterVMID); err != nil {
					p.logger.Warn("failed to destroy VM", "moref", *vm.VCenterVMID, "error", err)
					errors = append(errors, fmt.Errorf("destroy VM %s: %w", *vm.VCenterVMID, err))
					continue
				}
			}
			// Mark deleted only after vCenter confirms destruction or absence.
			// A disallowed-host guard must leave the persisted VM reference intact
			// for manual escalation rather than hiding a live ESXi2 VM.
			_ = p.db.UpdatePodVMStatus(ctx, vm.ID, "deleted")
		}
	}

	// --- Step 3: Delete only port groups owned by the durable create receipt ---
	p.publishProgress(job.ID, "portgroup_delete", fmt.Sprintf("Deleting port group %s", pgName))
	receiptRecord, err := p.db.GetPodPortGroupReceipt(ctx, pod.ID)
	if err != nil {
		if stderrors.Is(err, database.ErrPortGroupReceiptNotFound) {
			// Create can compensate before PersistPodPortGroupReceipt runs
			// (placement/DHCP/firewall failures). No receipt means no durable
			// switch ownership — skip keyed deletion; do not invent name-based
			// deletes. Continue network cleanup so the destroy finalizer can
			// release the VLAN instead of trapping destroy_failed forever.
			p.logger.Info("no durable port group receipt; skipping receipt-gated port group delete",
				"pod_id", pod.ID, "name", pgName)
		} else {
			p.logger.Warn("failed to load durable port group receipt", "name", pgName, "error", err)
			errors = append(errors, fmt.Errorf("load port group receipt: %w", err))
			if portGroupReceiptRequiresManualCleanup(err) {
				return p.failPodDestroyManual(ctx, pod.ID, errors)
			}
			return p.failPodDestroy(ctx, pod.ID, errors)
		}
	} else {
		var receipt vcenter.PortGroupReceipt
		if err := json.Unmarshal(receiptRecord.Receipt, &receipt); err != nil {
			errors = append(errors, fmt.Errorf("parse port group receipt: %w", err))
			return p.failPodDestroyManual(ctx, pod.ID, errors)
		}
		if receiptRecord.RemovedAt == nil {
			if receiptRecord.State == database.PodPortGroupReceiptPlanned {
				if err := p.db.MarkPodPortGroupRemoved(ctx, pod.ID, receiptRecord.Receipt); err != nil {
					errors = append(errors, err)
					return p.failPodDestroy(ctx, pod.ID, errors)
				}
			} else if receiptRecord.State != database.PodPortGroupReceiptApplying &&
				receiptRecord.State != database.PodPortGroupReceiptActive {
				errors = append(errors, fmt.Errorf(
					"invalid durable port group receipt state %q",
					receiptRecord.State,
				))
				return p.failPodDestroyManual(ctx, pod.ID, errors)
			}
		}
		if receiptRecord.RemovedAt == nil && receiptRecord.State != database.PodPortGroupReceiptPlanned {
			receipt, err = vcenter.PortGroupReceiptWithKeys(receipt, receiptRecord.Keys)
			if err != nil {
				errors = append(errors, fmt.Errorf("load stable port group identities: %w", err))
				return p.failPodDestroyManual(ctx, pod.ID, errors)
			}
			keys, captureErr := p.vc.CapturePortGroupKeys(ctx, receipt)
			if captureErr != nil {
				errors = append(errors, fmt.Errorf("capture stable port group identities: %w", captureErr))
				if portGroupReceiptRequiresManualCleanup(captureErr) {
					return p.failPodDestroyManual(ctx, pod.ID, errors)
				}
				return p.failPodDestroy(ctx, pod.ID, errors)
			}
			if err := p.db.PersistPodPortGroupKeys(ctx, pod.ID, receiptRecord.Receipt, keys); err != nil {
				errors = append(errors, err)
				if portGroupReceiptRequiresManualCleanup(err) {
					return p.failPodDestroyManual(ctx, pod.ID, errors)
				}
				return p.failPodDestroy(ctx, pod.ID, errors)
			}
			for host, key := range receiptRecord.Keys {
				if _, exists := keys[host]; !exists {
					keys[host] = key
				}
			}
			receipt, err = vcenter.PortGroupReceiptWithKeys(receipt, keys)
			if err != nil {
				errors = append(errors, err)
				return p.failPodDestroyManual(ctx, pod.ID, errors)
			}
			if err := p.db.WithVCenterPortGroupMutationLock(ctx, func(lockCtx context.Context) error {
				if err := p.vc.DeletePortGroupMutation(lockCtx, receipt); err != nil {
					return err
				}
				return p.db.MarkPodPortGroupRemoved(lockCtx, pod.ID, receiptRecord.Receipt)
			}); err != nil {
				p.logger.Warn("failed to delete port groups", "name", pgName, "error", err)
				errors = append(errors, fmt.Errorf("delete port groups: %w", err))
				// Keep the VLAN and interface allocated so a retry can safely remove
				// exactly the receipt-owned port groups before releasing network state.
				if portGroupReceiptRequiresManualCleanup(err) {
					return p.failPodDestroyManual(ctx, pod.ID, errors)
				}
				return p.failPodDestroy(ctx, pod.ID, errors)
			}
		}
	} // end receipt present

	// --- Step 4: Remove DHCP subnet ---
	p.publishProgress(job.ID, "dhcp_delete", fmt.Sprintf("Removing DHCP subnet %s", subnet))
	existingDHCP, _ := p.opn.GetDHCPSubnetByNetwork(ctx, subnet)
	if existingDHCP != nil {
		if err := p.opn.DeleteDHCPSubnet(ctx, existingDHCP.UUID); err != nil {
			p.logger.Warn("failed to delete DHCP subnet", "subnet", subnet, "error", err)
			errors = append(errors, fmt.Errorf("delete DHCP: %w", err))
		} else {
			_ = p.opn.ReconfigureDHCP(ctx)
		}
	}

	// --- Step 5: Remove generated firewall rules before unassigning interface ---
	p.publishProgress(job.ID, "firewall_delete", "Removing generated firewall rules")
	ifName, findErr := p.opnSSH.FindInterfaceByVLAN(ctx, int(vlanTag))
	if findErr != nil {
		p.logger.Warn("failed to find interface for firewall cleanup", "vlan", vlanTag, "error", findErr)
		cleanupErr := fmt.Errorf("find interface for firewall cleanup: %w", findErr)
		errors = append(errors, cleanupErr)
		return p.failPodDestroy(ctx, pod.ID, errors)
	} else if ifName != "" {
		if removed, cleanupErr := deletePodFirewallRules(
			ctx,
			p.opn,
			ifName,
			subnet,
			defaultMaxFirewallRules,
			defaultFirewallCleanupLimit,
		); cleanupErr != nil {
			p.logger.Warn("failed to clean generated firewall rules",
				"vlan", vlanTag, "interface", ifName, "removed", removed, "error", cleanupErr)
			errors = append(errors, fmt.Errorf("delete firewall rules: %w", cleanupErr))
			// Keep the interface assignment intact so the retry can identify
			// the exact generated signature and finish bounded cleanup.
			return p.failPodDestroy(ctx, pod.ID, errors)
		}
	}

	// --- Step 6: Remove OPNsense interface (SSH) ---
	p.publishProgress(job.ID, "interface_delete", "Removing OPNsense interface")
	unassignedIf, err := p.opnSSH.UnassignInterfaceByVLAN(ctx, int(vlanTag))
	if err != nil {
		p.logger.Warn("failed to unassign interface", "vlan", vlanTag, "error", err)
		errors = append(errors, fmt.Errorf("unassign interface: %w", err))
	}
	// Remove interface from Kea's listened interfaces
	if unassignedIf != "" {
		if err := p.opn.RemoveDHCPInterface(ctx, unassignedIf); err != nil {
			p.logger.Warn("failed to remove DHCP interface", "interface", unassignedIf, "error", err)
		}
		_ = p.opn.RestartDHCP(ctx)
	}

	// --- Step 7: Delete VLAN ---
	p.publishProgress(job.ID, "vlan_delete", fmt.Sprintf("Deleting VLAN %d", vlanTag))
	existingVLAN, _ := p.opn.GetVLANByTag(ctx, vlanTag)
	if existingVLAN != nil {
		if err := p.opn.DeleteVLAN(ctx, existingVLAN.UUID); err != nil {
			p.logger.Warn("failed to delete VLAN", "tag", vlanTag, "error", err)
			errors = append(errors, fmt.Errorf("delete VLAN: %w", err))
		} else {
			_ = p.opn.ReconfigureVLANs(ctx)
		}
	}

	// --- Step 8: Mark pod as destroyed and release VLAN ---
	if len(errors) > 0 {
		return p.failPodDestroy(ctx, pod.ID, errors)
	}

	if err := p.db.FinalizePodDestroy(ctx, pod.ID, job.ID, claimOwner); err != nil {
		return fmt.Errorf("finalize pod %s destruction: %w", pod.ID, err)
	}
	p.publishProgress(job.ID, "destroyed", "Pod destroyed successfully")
	p.logger.Info("pod destroyed successfully", "pod_id", pod.ID, "vlan", vlanTag)
	return nil
}

func (p *Provisioner) failPodDestroy(ctx context.Context, podID uuid.UUID, errors []error) error {
	destroyErr := fmt.Errorf("pod destroy incomplete with %d errors: %v", len(errors), errors)
	if err := p.db.UpdatePodStatus(ctx, podID, models.PodStatusDestroyFailed, destroyErr.Error()); err != nil {
		return stderrors.Join(destroyErr, fmt.Errorf("mark pod %s destroy_failed: %w", podID, err))
	}
	p.logger.Warn("pod destruction incomplete, marked destroy_failed",
		"pod_id", podID, "error_count", len(errors))
	return destroyErr
}

func (p *Provisioner) failPodDestroyManual(ctx context.Context, podID uuid.UUID, errors []error) error {
	destroyErr := fmt.Errorf("pod destroy incomplete with %d errors: %v", len(errors), errors)
	manualErr := fmt.Errorf(
		"%s %w; durable port group ownership cannot be proven, so the VLAN remains allocated and manual cleanup is required",
		models.PodErrorManualCleanupRequiredPrefix,
		destroyErr,
	)
	if err := p.db.UpdatePodStatus(ctx, podID, models.PodStatusDestroyFailed, manualErr.Error()); err != nil {
		return &manualCleanupRequiredError{
			err: stderrors.Join(manualErr, fmt.Errorf("mark pod %s destroy_failed: %w", podID, err)),
		}
	}
	p.logger.Warn("pod destruction requires manual cleanup",
		"pod_id", podID, "error_count", len(errors))
	return &manualCleanupRequiredError{
		err: manualErr,
	}
}

func portGroupReceiptRequiresManualCleanup(err error) bool {
	// ErrPortGroupReceiptNotFound is intentionally NOT manual cleanup: create
	// may compensate before any receipt exists. Destroy skips keyed portgroup
	// deletion and continues so FinalizePodDestroy can release the VLAN.
	return stderrors.Is(err, vcenter.ErrInvalidPortGroupReceipt) ||
		stderrors.Is(err, vcenter.ErrLegacyPortGroupReceipt) ||
		stderrors.Is(err, database.ErrPortGroupReceiptConflict) ||
		stderrors.Is(err, vcenter.ErrHostNotAllowed) ||
		stderrors.Is(err, vcenter.ErrAmbiguousHostIdentity)
}

// RetryFailedDestroys finds pods stuck in "destroy_failed" and requeues their
// authoritative durable destroy job. Workers then claim the retry normally.
//
// After the sweep, the post-retry count is pushed to Pushgateway (if a
// DestroyFailedPusher is configured) so the CruciblePodsStuckInDestroyFailed
// alert reflects steady-state, not pre-retry state. A push failure is logged
// but never blocks the retry itself.
func (p *Provisioner) RetryFailedDestroys(ctx context.Context) {
	pods, err := p.db.ListRetryableDestroyFailedPods(ctx)
	if err != nil {
		p.logger.Error("failed to list destroy_failed pods", "error", err)
		return
	}
	if len(pods) == 0 {
		p.publishCurrentDestroyFailedCount(ctx)
		return
	}

	p.logger.Info("retrying failed destroys", "count", len(pods))
	for _, pod := range pods {
		if err := p.db.RecordDestroyFailedRetry(ctx, pod.ID); err != nil {
			p.logger.Warn("failed to reserve destroy retry", "pod_id", pod.ID, "error", err)
			continue
		}
		payload, _ := json.Marshal(DestroyPodPayload{PodID: pod.ID.String()})
		job, queued, err := p.db.RequeueFailedPodDestroyJob(ctx, pod.ID, payload)
		if err != nil {
			p.logger.Warn("failed to requeue destroy", "pod_id", pod.ID, "error", err)
			continue
		}
		if !queued {
			p.logger.Info("destroy retry already queued", "pod_id", pod.ID, "job_id", job.ID)
			continue
		}
		p.logger.Info("requeued failed destroy", "pod_id", pod.ID, "job_id", job.ID, "vlan", pod.VLANID)
		if p.nats != nil {
			if err := p.nats.PublishJobCreated(job.ID, job.Type); err != nil {
				p.logger.Warn("failed to publish destroy retry event", "pod_id", pod.ID, "job_id", job.ID, "error", err)
			}
		}
	}

	// Re-count every destroy_failed pod so ownership-ambiguous failures remain
	// visible to alerting even though the automated sweep does not retry them.
	p.publishCurrentDestroyFailedCount(ctx)
}

func (p *Provisioner) publishCurrentDestroyFailedCount(ctx context.Context) {
	count, err := p.db.CountDestroyFailedPods(ctx)
	if err != nil {
		p.logger.Warn("failed to count destroy_failed pods", "error", err)
		return
	}
	p.publishDestroyFailedCount(ctx, count)
}

// publishDestroyFailedCount pushes the count to Pushgateway when a pusher
// is configured. Failures log at WARN — Pushgateway is best-effort.
func (p *Provisioner) publishDestroyFailedCount(ctx context.Context, count int) {
	if p.DestroyFailedPusher == nil {
		return
	}
	if err := p.DestroyFailedPusher.Push(ctx, count); err != nil {
		p.logger.Warn("destroy_failed metric push failed", "error", err, "count", count)
		return
	}
	p.logger.Info("destroy_failed metric pushed", "count", count)
}
