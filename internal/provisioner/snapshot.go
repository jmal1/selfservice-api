package provisioner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmal1/selfservice-api/internal/models"
)

// SnapshotVMPayload is the expected shape of job.Payload for vm_snapshot.
type SnapshotVMPayload struct {
	PodVMID     string `json:"pod_vm_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SnapshotVM creates a vCenter snapshot and records it in the database.
func (p *Provisioner) SnapshotVM(ctx context.Context, job *models.Job) error {
	var payload SnapshotVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_snapshot payload: %w", err)
	}

	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("pod VM %s has no vCenter moref", podVMID)
	}

	p.publishProgress(job.ID, "snapshot_create", fmt.Sprintf("Creating snapshot %q for VM %s", payload.Name, podVMID))

	snapMoref, err := p.vc.CreateVMSnapshot(ctx, *podVM.VCenterVMID, payload.Name, payload.Description)
	if err != nil {
		return fmt.Errorf("create vCenter snapshot: %w", err)
	}

	if err := p.db.CreateVMSnapshot(ctx, &models.VMSnapshot{
		PodVMID:           podVMID,
		Name:              payload.Name,
		Description:       payload.Description,
		VCenterSnapshotID: snapMoref,
		IsInitial:         false,
	}); err != nil {
		return fmt.Errorf("save snapshot record: %w", err)
	}

	p.logger.Info("snapshot created", "pod_vm_id", podVMID, "snapshot", payload.Name, "moref", snapMoref)
	return nil
}

// RevertVMPayload is the expected shape of job.Payload for vm_revert.
type RevertVMPayload struct {
	PodVMID    string `json:"pod_vm_id"`
	SnapshotID string `json:"snapshot_id"`
}

// RevertVM reverts a VM to a previously taken snapshot.
func (p *Provisioner) RevertVM(ctx context.Context, job *models.Job) error {
	var payload RevertVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_revert payload: %w", err)
	}

	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}

	snapID, err := parseUUID(payload.SnapshotID)
	if err != nil {
		return fmt.Errorf("invalid snapshot_id: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("pod VM %s has no vCenter moref", podVMID)
	}

	snap, err := p.db.GetVMSnapshot(ctx, snapID)
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}
	if snap == nil {
		return fmt.Errorf("snapshot %s not found", snapID)
	}

	p.publishProgress(job.ID, "snapshot_revert", fmt.Sprintf("Reverting VM %s to snapshot %q", podVMID, snap.Name))

	if err := p.vc.RevertToSnapshot(ctx, *podVM.VCenterVMID, snap.VCenterSnapshotID); err != nil {
		return fmt.Errorf("revert to snapshot: %w", err)
	}

	if err := p.db.UpdatePodVMIP(ctx, podVMID, ""); err != nil {
		return fmt.Errorf("clear VM IP after revert: %w", err)
	}

	p.logger.Info("VM reverted to snapshot", "pod_vm_id", podVMID, "snapshot_id", snapID, "snapshot", snap.Name)
	return nil
}

// DeleteSnapshotPayload is the expected shape of job.Payload for vm_snapshot_delete.
type DeleteSnapshotPayload struct {
	SnapshotID string `json:"snapshot_id"`
}

// DeleteSnapshot removes a snapshot from vCenter and the database.
func (p *Provisioner) DeleteSnapshot(ctx context.Context, job *models.Job) error {
	var payload DeleteSnapshotPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_snapshot_delete payload: %w", err)
	}

	snapID, err := parseUUID(payload.SnapshotID)
	if err != nil {
		return fmt.Errorf("invalid snapshot_id: %w", err)
	}

	snap, err := p.db.GetVMSnapshot(ctx, snapID)
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}
	if snap == nil {
		return fmt.Errorf("snapshot %s not found", snapID)
	}

	podVM, err := p.db.GetPodVM(ctx, snap.PodVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("pod VM %s has no vCenter moref", snap.PodVMID)
	}

	p.publishProgress(job.ID, "snapshot_delete", fmt.Sprintf("Deleting snapshot %q from VM %s", snap.Name, snap.PodVMID))

	if err := p.vc.RemoveVMSnapshot(ctx, *podVM.VCenterVMID, snap.VCenterSnapshotID); err != nil {
		return fmt.Errorf("remove vCenter snapshot: %w", err)
	}

	if err := p.db.DeleteVMSnapshot(ctx, snapID); err != nil {
		return fmt.Errorf("delete snapshot record: %w", err)
	}

	p.logger.Info("snapshot deleted", "snapshot_id", snapID, "snapshot", snap.Name, "pod_vm_id", snap.PodVMID)
	return nil
}
