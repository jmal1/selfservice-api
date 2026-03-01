package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// --- VM Destroy ---

// DestroyVMPayload is the expected shape of job.Payload for vm_destroy.
type DestroyVMPayload struct {
	PodID   string `json:"pod_id"`
	PodVMID string `json:"pod_vm_id"`
}

// DestroyVM destroys a single VM and auto-cleans up the pod if empty.
func (p *Provisioner) DestroyVM(ctx context.Context, job *models.Job) error {
	var payload DestroyVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_destroy payload: %w", err)
	}

	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}
	podID, err := parseUUID(payload.PodID)
	if err != nil {
		return fmt.Errorf("invalid pod_id: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}

	// Power off VM if it has a vCenter reference
	if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" {
		moref := *podVM.VCenterVMID

		p.publishProgress(job.ID, "vm_poweroff", "Powering off VM")
		if err := p.vc.PowerOffVM(ctx, moref); err != nil {
			p.logger.Warn("power off VM failed (may already be off)", "moref", moref, "error", err)
		}

		p.publishProgress(job.ID, "vm_destroy", "Destroying VM in vCenter")
		if err := p.vc.DestroyVM(ctx, moref); err != nil {
			return fmt.Errorf("destroy VM %s: %w", moref, err)
		}
	}

	// Mark VM as deleted
	if err := p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusDeleted); err != nil {
		return fmt.Errorf("update VM status: %w", err)
	}

	p.logger.Info("VM destroyed", "vm_id", podVMID, "pod_id", podID)

	// Auto-cleanup: check if pod has any remaining active VMs
	remaining, err := p.db.CountActiveVMsInPod(ctx, podID)
	if err != nil {
		p.logger.Warn("failed to count remaining VMs", "pod_id", podID, "error", err)
		return nil // VM is deleted, don't fail the job for this
	}

	if remaining == 0 {
		p.logger.Info("no remaining VMs in pod, queueing pod destruction", "pod_id", podID)
		p.publishProgress(job.ID, "pod_auto_cleanup", "Pod has no VMs left — queueing cleanup")

		destroyPayload, _ := json.Marshal(map[string]string{"pod_id": podID.String()})
		destroyJob, err := p.db.CreateJob(ctx, models.JobTypePodDestroy, destroyPayload)
		if err != nil {
			p.logger.Error("failed to queue pod auto-destroy", "pod_id", podID, "error", err)
			return nil
		}
		if p.nats != nil {
			_ = p.nats.PublishJobCreated(destroyJob.ID, destroyJob.Type)
		}
	}

	return nil
}

// --- VM Add ---

// AddVMPayload is the expected shape of job.Payload for vm_add.
type AddVMPayload struct {
	PodID        string `json:"pod_id"`
	PodVMID      string `json:"pod_vm_id"`
	TemplateName string `json:"template_name"`
	VMName       string `json:"vm_name"`
	DisplayName  string `json:"display_name"`
}

// AddVM clones and powers on a new VM in an existing pod.
func (p *Provisioner) AddVM(ctx context.Context, job *models.Job) error {
	var payload AddVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse vm_add payload: %w", err)
	}

	podID, err := parseUUID(payload.PodID)
	if err != nil {
		return fmt.Errorf("invalid pod_id: %w", err)
	}
	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return fmt.Errorf("invalid pod_vm_id: %w", err)
	}

	pod, err := p.db.GetPodByID(ctx, podID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}

	pgName := fmt.Sprintf("Pod-VLAN%d", pod.VLANID)

	// Determine OS type and generate credentials
	password := ""
	osType := ""
	tmpl, tmplErr := p.db.GetTemplateByID(ctx, podVM.TemplateID)
	if tmplErr == nil {
		osType = tmpl.OSType
	}
	if osType == "linux" || osType == "windows" {
		password = generatePassword(12)
	}

	// Resume support: skip clone if VM was already cloned (e.g., job retry after worker restart)
	moref := ""
	if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" {
		moref = *podVM.VCenterVMID
		p.logger.Info("resuming VM add — already cloned", "vm", payload.VMName, "moref", moref)
	} else {
		// Step 1: Clone VM
		p.publishProgress(job.ID, "vm_clone", fmt.Sprintf("Cloning %s from %s", payload.VMName, payload.TemplateName))
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusCloning)

		var err error
		moref, err = p.vc.CloneVM(ctx, vcenter.CloneVMParams{
			TemplateName: payload.TemplateName,
			VMName:       payload.VMName,
			VCPUs:        int32(podVM.VCPUs),
			RAMmb:        int64(podVM.RAMMB),
			Network:      pgName,
			OSType:       osType,
			Password:     password,
		})
		if err != nil {
			_ = p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusError)
			return fmt.Errorf("clone VM: %w", err)
		}

		_ = p.db.UpdatePodVM(ctx, podVMID, moref, payload.VMName, models.VMStatusConfiguring)

		// Store generated credentials
		genUser := "student"
		if osType == "windows" {
			genUser = "Student"
		}
		_ = p.db.UpdatePodVMCredentials(ctx, podVMID, genUser, password)
	}

	// Step 2: Power on
	p.publishProgress(job.ID, "vm_poweron", fmt.Sprintf("Powering on %s", payload.VMName))
	if err := p.vc.PowerOnVM(ctx, moref); err != nil {
		_ = p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusError)
		return fmt.Errorf("power on VM: %w", err)
	}

	// Step 3: Wait for IP
	ip, err := p.vc.WaitForIP(ctx, moref, 5*time.Minute)
	if err != nil {
		p.logger.Warn("timeout waiting for VM IP", "vm", payload.VMName, "error", err)
	} else {
		_ = p.db.UpdatePodVMIP(ctx, podVMID, ip)
	}

	_ = p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusRunning)

	p.logger.Info("VM added successfully", "vm_id", podVMID, "pod_id", podID, "moref", moref)
	return nil
}
