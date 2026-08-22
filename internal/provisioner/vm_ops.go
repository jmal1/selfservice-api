package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// --- VM Destroy ---

// DestroyVMPayload is the expected shape of job.Payload for vm_destroy.
type DestroyVMPayload struct {
	PodID       string `json:"pod_id"`
	PodVMID     string `json:"pod_vm_id"`
	VCenterVMID string `json:"vcenter_vm_id,omitempty"`
	CleanupOnly bool   `json:"cleanup_only,omitempty"`
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
	if err != nil && payload.VCenterVMID == "" {
		return fmt.Errorf("get pod VM: %w", err)
	}

	// Power off VM if it has a vCenter reference
	moref := payload.VCenterVMID
	if moref == "" && podVM != nil && podVM.VCenterVMID != nil {
		moref = *podVM.VCenterVMID
	}
	if moref != "" {

		p.publishProgress(job.ID, "vm_poweroff", "Powering off VM")
		if err := p.vc.PowerOffVM(ctx, moref); err != nil {
			p.logger.Warn("power off VM failed (may already be off)", "moref", moref, "error", err)
		}

		p.publishProgress(job.ID, "vm_destroy", "Destroying VM in vCenter")
		if err := p.vc.DestroyVM(ctx, moref); err != nil {
			if !vcenterObjectNotFound(err) {
				return fmt.Errorf("destroy VM %s: %w", moref, err)
			}
		}
	}

	if payload.CleanupOnly {
		if moref != "" {
			if _, err := p.db.ClearPodVMVCenterReference(ctx, podVMID, moref); err != nil {
				return fmt.Errorf("clear stale VM reference: %w", err)
			}
		}
		p.logger.Info("stale VM clone cleanup completed", "vm_id", podVMID, "pod_id", podID, "moref", moref)
		return nil
	}

	// Mark VM as deleted
	if err := p.db.UpdatePodVMStatus(ctx, podVMID, models.VMStatusDeleted); err != nil {
		return fmt.Errorf("update VM status: %w", err)
	}
	if moref != "" {
		if _, err := p.db.ClearPodVMVCenterReference(ctx, podVMID, moref); err != nil {
			return fmt.Errorf("clear destroyed VM reference: %w", err)
		}
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

func shouldMarkVMAddError(job *models.Job, jobErr error) bool {
	retryable, _ := ClassifyError(jobErr, job.Type)
	return !retryable || job.RetryCount >= job.MaxRetries
}

func podVMHasLiveClone(vm *models.PodVM, moref string) bool {
	if vm == nil || vm.VCenterVMID == nil || *vm.VCenterVMID != moref {
		return false
	}
	switch vm.Status {
	case "cloned", models.VMStatusConfiguring, models.VMStatusRunning:
		return true
	default:
		return false
	}
}

func vcenterObjectNotFound(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

func staleVMCloneCleanupPayload(podID, podVMID uuid.UUID, moref string) ([]byte, error) {
	return json.Marshal(DestroyVMPayload{
		PodID:       podID.String(),
		PodVMID:     podVMID.String(),
		VCenterVMID: moref,
		CleanupOnly: true,
	})
}

// cleanupStaleVMClone removes a clone after its provisioning job loses
// ownership of the pod_vms row. If immediate cleanup fails, it persists an
// independently claimable vm_destroy job carrying the exact MoRef.
func (p *Provisioner) cleanupStaleVMClone(
	ctx context.Context,
	podID, podVMID uuid.UUID,
	vmName, moref string,
) error {
	if moref == "" {
		resolved, err := p.vc.ResolveVMByName(ctx, vmName)
		if err != nil {
			if vcenterObjectNotFound(err) {
				return nil
			}
			return fmt.Errorf("resolve stale VM clone for cleanup: %w", err)
		}
		moref = resolved
	}

	recorded, recordErr := p.db.SetPodVMVCenterReference(ctx, podVMID, moref, vmName)
	if recordErr != nil {
		p.logger.Warn("failed to persist stale VM clone reference before cleanup",
			"pod_id", podID, "pod_vm_id", podVMID, "moref", moref, "error", recordErr)
	}

	_ = p.vc.PowerOffVM(ctx, moref)
	destroyErr := p.vc.DestroyVM(ctx, moref)
	if destroyErr == nil || vcenterObjectNotFound(destroyErr) {
		if recorded {
			if _, err := p.db.ClearPodVMVCenterReference(ctx, podVMID, moref); err != nil {
				return fmt.Errorf("clear stale VM clone reference after cleanup: %w", err)
			}
		}
		return nil
	}

	payload, err := staleVMCloneCleanupPayload(podID, podVMID, moref)
	if err != nil {
		return fmt.Errorf("marshal stale VM cleanup job: %w", err)
	}
	cleanupJob, queueErr := p.db.CreateJob(ctx, models.JobTypeVMDestroy, payload)
	if queueErr != nil {
		return fmt.Errorf("stale VM clone cleanup failed: destroy %s: %v; queue durable cleanup: %w", moref, destroyErr, queueErr)
	}
	if p.nats != nil {
		_ = p.nats.PublishJobCreated(cleanupJob.ID, cleanupJob.Type)
	}
	p.logger.Warn("queued durable cleanup for stale VM clone",
		"pod_id", podID, "pod_vm_id", podVMID, "moref", moref, "job_id", cleanupJob.ID, "destroy_error", destroyErr)
	return nil
}

func (p *Provisioner) recordVMAddFailure(ctx context.Context, job *models.Job, podVMID uuid.UUID, jobErr error) {
	if !shouldMarkVMAddError(job, jobErr) {
		return
	}
	_, _ = p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusError,
	)
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
	if pod.Status != models.PodStatusActive {
		if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" {
			return p.cleanupStaleVMClone(ctx, podID, podVMID, payload.VMName, *podVM.VCenterVMID)
		}
		p.logger.Warn("stale vm_add job skipped because pod is not active",
			"pod_id", podID, "pod_status", pod.Status, "pod_vm_id", podVMID, "job_id", job.ID)
		return nil
	}
	applied, err := p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusPending, models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusCloning,
	)
	if err != nil {
		return fmt.Errorf("guard pod VM provisioning state: %w", err)
	}
	if !applied {
		current, lookupErr := p.db.GetPodVM(ctx, podVMID)
		if lookupErr != nil {
			return fmt.Errorf("reload stale pod VM after state changed: %w", lookupErr)
		}
		currentMoref := ""
		if current.VCenterVMID != nil {
			currentMoref = *current.VCenterVMID
		}
		if current.Status == models.VMStatusDeleted || current.Status == models.VMStatusError {
			return p.cleanupStaleVMClone(ctx, podID, podVMID, payload.VMName, currentMoref)
		}
		p.logger.Warn("stale vm_add job skipped because VM is no longer provisionable",
			"pod_id", podID, "pod_vm_id", podVMID, "vm_status", podVM.Status, "job_id", job.ID)
		return nil
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
			jobErr := fmt.Errorf("clone VM: %w", err)
			p.recordVMAddFailure(ctx, job, podVMID, jobErr)
			return jobErr
		}

		applied, err = p.db.UpdatePodVMFrom(
			ctx,
			podVMID,
			[]string{models.VMStatusCloning},
			moref,
			payload.VMName,
			models.VMStatusConfiguring,
		)
		if err != nil {
			if current, lookupErr := p.db.GetPodVM(ctx, podVMID); lookupErr == nil &&
				podVMHasLiveClone(current, moref) {
				if current.Status == models.VMStatusRunning {
					return nil
				}
				applied = true
			} else {
				if cleanupErr := p.cleanupStaleVMClone(ctx, podID, podVMID, payload.VMName, moref); cleanupErr != nil {
					jobErr := fmt.Errorf("vm_add clone persistence cleanup failed: record cloned VM: %v; cleanup clone: %w", err, cleanupErr)
					p.recordVMAddFailure(ctx, job, podVMID, jobErr)
					return jobErr
				}
				jobErr := fmt.Errorf("record cloned VM after compensating cleanup: %w", err)
				p.recordVMAddFailure(ctx, job, podVMID, jobErr)
				return jobErr
			}
		}
		if !applied {
			current, lookupErr := p.db.GetPodVM(ctx, podVMID)
			if lookupErr == nil && podVMHasLiveClone(current, moref) {
				if current.Status == models.VMStatusRunning {
					return nil
				}
				applied = true
			}
		}
		if !applied {
			p.logger.Warn("vm_add lost its provisioning state after clone; removing stale clone",
				"pod_id", podID, "pod_vm_id", podVMID, "moref", moref)
			return p.cleanupStaleVMClone(ctx, podID, podVMID, payload.VMName, moref)
		}

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
		jobErr := fmt.Errorf("power on VM: %w", err)
		p.recordVMAddFailure(ctx, job, podVMID, jobErr)
		return jobErr
	}

	// Step 3: Wait for IP
	ip, err := p.vc.WaitForIP(ctx, moref, 5*time.Minute)
	if err != nil {
		p.logger.Warn("timeout waiting for VM IP", "vm", payload.VMName, "error", err)
	} else {
		_ = p.db.UpdatePodVMIP(ctx, podVMID, ip)
	}

	applied, err = p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusRunning,
	)
	if err != nil {
		return fmt.Errorf("mark added VM running: %w", err)
	}
	if !applied {
		current, lookupErr := p.db.GetPodVM(ctx, podVMID)
		if lookupErr == nil && podVMHasLiveClone(current, moref) &&
			current.Status == models.VMStatusRunning {
			return nil
		}
		p.logger.Warn("vm_add finished after VM entered a terminal state; not marking it running",
			"pod_id", podID, "pod_vm_id", podVMID, "job_id", job.ID)
		return p.cleanupStaleVMClone(ctx, podID, podVMID, payload.VMName, moref)
	}

	// Take initial snapshot for restore-to-original (non-fatal if fails)
	p.publishProgress(job.ID, "initial_snapshot", "Creating initial snapshot for restore-to-original")
	snapMoref, snapErr := p.vc.CreateVMSnapshot(ctx, moref, "initial", "Auto-created at provisioning")
	if snapErr != nil {
		p.logger.Warn("failed to create initial snapshot (non-fatal)", "vm", payload.VMName, "error", snapErr)
	} else {
		snap := &models.VMSnapshot{
			PodVMID:           podVMID,
			Name:              "initial",
			Description:       "Original state at provisioning",
			VCenterSnapshotID: snapMoref,
			IsInitial:         true,
		}
		if dbErr := p.db.CreateVMSnapshot(ctx, snap); dbErr != nil {
			p.logger.Warn("failed to record initial snapshot in DB (non-fatal)", "vm", payload.VMName, "error", dbErr)
		}
	}

	p.logger.Info("VM added successfully", "vm_id", podVMID, "pod_id", podID, "moref", moref)
	return nil
}
