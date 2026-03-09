package provisioner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

// PowerVMPayload is the expected shape of job.Payload for vm_start/stop/restart.
type PowerVMPayload struct {
	PodID   string `json:"pod_id"`
	PodVMID string `json:"pod_vm_id"`
}

// PowerVM executes a VM power operation (start, stop, restart).
func (p *Provisioner) PowerVM(ctx context.Context, job *models.Job, action string) error {
	var payload PowerVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse power payload: %w", err)
	}

	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}

	if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
		return fmt.Errorf("VM %s has no vCenter reference", podVMID)
	}
	moref := *podVM.VCenterVMID

	p.publishProgress(job.ID, action, fmt.Sprintf("Performing %s on VM", action))

	switch action {
	case "start":
		if err := p.vc.PowerOnVM(ctx, moref); err != nil {
			return fmt.Errorf("power on: %w", err)
		}
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, "running")

	case "stop":
		if err := p.vc.PowerOffVM(ctx, moref); err != nil {
			return fmt.Errorf("power off: %w", err)
		}
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, "stopped")

	case "restart":
		if err := p.vc.RestartVM(ctx, moref); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, "running")

	case "reset":
		if err := p.vc.ResetVM(ctx, moref); err != nil {
			return fmt.Errorf("reset: %w", err)
		}
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, "running")

	default:
		return fmt.Errorf("unknown power action: %s", action)
	}

	p.logger.Info("VM power operation completed", "vm_id", podVMID, "action", action, "moref", moref)
	return nil
}

// parseUUID is a helper to parse a string UUID.
func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}
