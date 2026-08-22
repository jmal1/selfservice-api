package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
)

// SuspendVMPayload is the expected shape of job.Payload for vm_suspend.
type SuspendVMPayload struct {
	PodID   string `json:"pod_id"`
	PodVMID string `json:"pod_vm_id"`
	Reason  string `json:"reason,omitempty"`
}

// SuspendVM executes a VM suspend operation: saves state via SuspendVM_Task in
// vCenter, then marks the VM as suspended in the database. Resume is handled
// by a normal vm_start job (PowerOnVM resumes from a vCenter suspend checkpoint
// without a fresh boot).
func (p *Provisioner) SuspendVM(ctx context.Context, job *models.Job) error {
	var payload SuspendVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_suspend payload: %w", err)
	}

	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		p.logger.Error("vm_suspend: get pod VM failed", "job_id", job.ID, "pod_vm_id", podVMID, "error", err)
		return fmt.Errorf("get VM %s: %w", podVMID, err)
	}

	if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
		return fmt.Errorf("VM %q has no vCenter reference", podVM.DisplayName)
	}
	moref := *podVM.VCenterVMID
	if err := p.validatePersistedVMPlacement(ctx, podVMID, moref); err != nil {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"refuse suspend for VM %s after placement drift: %w",
			podVMID,
			err,
		)}
	}

	reason := payload.Reason
	if reason == "" {
		reason = "manual suspend"
	}

	p.publishProgress(job.ID, "vm_suspend", fmt.Sprintf("Suspending %s", podVM.DisplayName))

	if err := p.vc.SuspendVM(ctx, moref); err != nil {
		p.logger.Error("vm_suspend: SuspendVM failed", "job_id", job.ID, "vm_name", podVM.DisplayName, "error", err)
		return fmt.Errorf("failed to suspend %s: %w", podVM.DisplayName, err)
	}

	if err := p.db.SetVMSuspended(ctx, podVMID, time.Now(), reason); err != nil {
		p.logger.Error("vm_suspend: SetVMSuspended failed", "job_id", job.ID, "vm_name", podVM.DisplayName, "error", err)
		return fmt.Errorf("record suspend for %s: %w", podVM.DisplayName, err)
	}

	p.logger.Info("VM suspended", "job_id", job.ID, "vm_name", podVM.DisplayName, "vm_id", podVMID, "moref", moref, "reason", reason)
	return nil
}
