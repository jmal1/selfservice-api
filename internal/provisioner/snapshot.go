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
		p.logger.Error("snapshot failed: get pod VM", "job_id", job.ID, "pod_vm_id", podVMID, "error", err)
		return fmt.Errorf("get pod VM %s: %w", podVMID, err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("VM %q has no vCenter reference", podVM.DisplayName)
	}

	p.publishProgress(job.ID, "snapshot_create", fmt.Sprintf("Creating snapshot %q for %s", payload.Name, podVM.DisplayName))

	snapMoref, err := p.vc.CreateVMSnapshot(ctx, *podVM.VCenterVMID, payload.Name, payload.Description)
	if err != nil {
		p.logger.Error("snapshot failed: vCenter create", "job_id", job.ID, "vm_name", podVM.DisplayName, "snapshot_name", payload.Name, "error", err)
		return fmt.Errorf("failed to create snapshot %q on %s: %w", payload.Name, podVM.DisplayName, err)
	}

	if err := p.db.CreateVMSnapshot(ctx, &models.VMSnapshot{
		PodVMID:           podVMID,
		Name:              payload.Name,
		Description:       payload.Description,
		VCenterSnapshotID: snapMoref,
		IsInitial:         false,
	}); err != nil {
		p.logger.Error("snapshot failed: save record", "job_id", job.ID, "vm_name", podVM.DisplayName, "snapshot_name", payload.Name, "error", err)
		return fmt.Errorf("save snapshot record for %s: %w", podVM.DisplayName, err)
	}

	p.logger.Info("snapshot created", "job_id", job.ID, "pod_vm_id", podVMID, "vm_name", podVM.DisplayName, "snapshot", payload.Name, "moref", snapMoref)
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
		p.logger.Error("revert failed: get pod VM", "job_id", job.ID, "pod_vm_id", podVMID, "error", err)
		return fmt.Errorf("get pod VM %s: %w", podVMID, err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("VM %q has no vCenter reference", podVM.DisplayName)
	}

	snap, err := p.db.GetVMSnapshot(ctx, snapID)
	if err != nil {
		p.logger.Error("revert failed: get snapshot", "job_id", job.ID, "snapshot_id", snapID, "error", err)
		return fmt.Errorf("get snapshot %s: %w", snapID, err)
	}
	if snap == nil {
		return fmt.Errorf("snapshot %s not found", snapID)
	}

	p.publishProgress(job.ID, "snapshot_revert", fmt.Sprintf("Reverting %s to snapshot %q", podVM.DisplayName, snap.Name))

	if err := p.vc.RevertToSnapshot(ctx, *podVM.VCenterVMID, snap.VCenterSnapshotID); err != nil {
		p.logger.Error("revert failed: vCenter revert", "job_id", job.ID, "vm_name", podVM.DisplayName, "snapshot_name", snap.Name, "error", err)
		return fmt.Errorf("failed to revert %s to snapshot %q: %w", podVM.DisplayName, snap.Name, err)
	}

	if err := p.db.UpdatePodVMIP(ctx, podVMID, ""); err != nil {
		p.logger.Warn("revert: failed to clear VM IP", "job_id", job.ID, "pod_vm_id", podVMID, "error", err)
	}

	p.logger.Info("VM reverted to snapshot", "job_id", job.ID, "pod_vm_id", podVMID, "vm_name", podVM.DisplayName, "snapshot_id", snapID, "snapshot", snap.Name)
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
		p.logger.Error("delete snapshot failed: get snapshot", "job_id", job.ID, "snapshot_id", snapID, "error", err)
		return fmt.Errorf("get snapshot %s: %w", snapID, err)
	}
	if snap == nil {
		return fmt.Errorf("snapshot %s not found", snapID)
	}

	podVM, err := p.db.GetPodVM(ctx, snap.PodVMID)
	if err != nil {
		p.logger.Error("delete snapshot failed: get pod VM", "job_id", job.ID, "pod_vm_id", snap.PodVMID, "error", err)
		return fmt.Errorf("get pod VM for snapshot: %w", err)
	}
	if podVM.VCenterVMID == nil {
		return fmt.Errorf("VM %q has no vCenter reference", podVM.DisplayName)
	}

	p.publishProgress(job.ID, "snapshot_delete", fmt.Sprintf("Deleting snapshot %q from %s", snap.Name, podVM.DisplayName))

	if err := p.vc.RemoveVMSnapshot(ctx, *podVM.VCenterVMID, snap.VCenterSnapshotID); err != nil {
		p.logger.Error("delete snapshot failed: vCenter remove", "job_id", job.ID, "vm_name", podVM.DisplayName, "snapshot_name", snap.Name, "error", err)
		return fmt.Errorf("failed to remove snapshot %q from %s: %w", snap.Name, podVM.DisplayName, err)
	}

	if err := p.db.DeleteVMSnapshot(ctx, snapID); err != nil {
		p.logger.Error("delete snapshot failed: remove record", "job_id", job.ID, "snapshot_id", snapID, "error", err)
		return fmt.Errorf("delete snapshot record: %w", err)
	}

	p.logger.Info("snapshot deleted", "job_id", job.ID, "snapshot_id", snapID, "vm_name", podVM.DisplayName, "snapshot", snap.Name, "pod_vm_id", snap.PodVMID)
	return nil
}
