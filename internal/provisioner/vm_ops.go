package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
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

// VMCloneCleanupTarget identifies one external clone by immutable vCenter
// MoRef. It is staged on the parent provisioning job before database adoption.
type VMCloneCleanupTarget struct {
	PodID       string `json:"pod_id"`
	PodVMID     string `json:"pod_vm_id"`
	VCenterVMID string `json:"vcenter_vm_id"`
}

type vmCloneHandoffStore interface {
	StageVMCloneCleanup(ctx context.Context, jobID uuid.UUID, workerID string, target []byte) error
}

func validateVMCloneCleanupTarget(target *VMCloneCleanupTarget) (uuid.UUID, uuid.UUID, error) {
	if target == nil {
		return uuid.Nil, uuid.Nil, errors.New("cleanup target is missing")
	}
	podID, err := parseUUID(target.PodID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid cleanup pod_id: %w", err)
	}
	podVMID, err := parseUUID(target.PodVMID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("invalid cleanup pod_vm_id: %w", err)
	}
	if target.VCenterVMID == "" {
		return uuid.Nil, uuid.Nil, errors.New("cleanup target has no exact vCenter MoRef")
	}
	return podID, podVMID, nil
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
	if payload.CleanupOnly && payload.VCenterVMID == "" {
		return &manualCleanupRequiredError{
			err: fmt.Errorf("cleanup-only vm_destroy for VM %s has no exact vCenter MoRef; manual cleanup required", podVMID),
		}
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
			return fmt.Errorf("destroy VM %s: %w", moref, err)
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
	PodID          string                   `json:"pod_id"`
	PodVMID        string                   `json:"pod_vm_id"`
	TemplateName   string                   `json:"template_name"`
	VMName         string                   `json:"vm_name"`
	DisplayName    string                   `json:"display_name"`
	CleanupOnly    bool                     `json:"cleanup_only,omitempty"`
	CleanupTarget  *VMCloneCleanupTarget    `json:"cleanup_target,omitempty"`
	CleanupDone    bool                     `json:"cleanup_completed,omitempty"`
	CloneOperation *models.VMCloneOperation `json:"clone_operation,omitempty"`
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

func (p *Provisioner) stageVMCloneCleanup(
	ctx context.Context,
	job *models.Job,
	podID, podVMID uuid.UUID,
	moref string,
) error {
	if moref == "" {
		return &manualCleanupRequiredError{
			err: fmt.Errorf(
				"cannot safely clean stale VM %s for pod %s without an exact vCenter MoRef; manual cleanup required",
				podVMID,
				podID,
			),
		}
	}
	target, err := json.Marshal(VMCloneCleanupTarget{
		PodID:       podID.String(),
		PodVMID:     podVMID.String(),
		VCenterVMID: moref,
	})
	if err != nil {
		return &manualCleanupRequiredError{
			err: fmt.Errorf("marshal exact VM cleanup target %s: %w", moref, err),
		}
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	destroyed, err := persistVMCloneHandoff(
		ctx,
		p.db,
		job.ID,
		workerID,
		target,
		moref,
	)
	if err != nil {
		return err
	}
	if destroyed {
		return &compensatedJobError{
			err: fmt.Errorf("job lease was lost after cloning %s; exact clone was destroyed and recorded", moref),
		}
	}
	return nil
}

func persistVMCloneHandoff(
	ctx context.Context,
	store vmCloneHandoffStore,
	jobID uuid.UUID,
	workerID string,
	target []byte,
	moref string,
) (bool, error) {
	err := persistCloneOperationWrite(ctx, func(writeCtx context.Context) error {
		return store.StageVMCloneCleanup(writeCtx, jobID, workerID, target)
	})
	if errors.Is(err, database.ErrVMCloneAlreadyDestroyed) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stage exact clone %s for successor cleanup: %w", moref, err)
	}
	return false, nil
}

func (p *Provisioner) cleanupStagedVMClone(ctx context.Context, jobID uuid.UUID, workerID string) error {
	job, err := p.db.GetJob(ctx, jobID)
	if err != nil {
		return &compensationRetryError{err: fmt.Errorf("load staged VM cleanup target: %w", err)}
	}
	if job == nil {
		return &manualCleanupRequiredError{
			err: fmt.Errorf("cleanup job %s disappeared; manual cleanup required", jobID),
		}
	}
	var payload struct {
		CleanupTarget         *VMCloneCleanupTarget    `json:"cleanup_target"`
		CleanupHandoffTargets []VMCloneCleanupTarget   `json:"cleanup_handoff_targets"`
		CloneOperation        *models.VMCloneOperation `json:"clone_operation"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return &compensationRetryError{err: fmt.Errorf("parse staged VM cleanup target: %w", err)}
	}
	if payload.CloneOperation != nil {
		if err := reconcileCloneOperationForCleanup(
			ctx,
			p.db,
			p.vc,
			jobID,
			workerID,
			payload.CloneOperation,
		); err != nil {
			return err
		}
		job, err = p.db.GetJob(ctx, jobID)
		if err != nil {
			return &compensationRetryError{err: fmt.Errorf("reload reconciled VM cleanup target: %w", err)}
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return &compensationRetryError{err: fmt.Errorf("parse reconciled VM cleanup target: %w", err)}
		}
	}
	var targets []VMCloneCleanupTarget
	if payload.CleanupTarget != nil {
		targets = append(targets, *payload.CleanupTarget)
	}
	targets = append(targets, payload.CleanupHandoffTargets...)
	if len(targets) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(targets))
	for i := range targets {
		target := &targets[i]
		_, podVMID, err := validateVMCloneCleanupTarget(target)
		if err != nil {
			return &manualCleanupRequiredError{
				err: fmt.Errorf("staged VM cleanup target is invalid: %v; manual cleanup required", err),
			}
		}
		key := target.PodVMID + "\x00" + target.VCenterVMID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		_ = p.vc.PowerOffVM(ctx, target.VCenterVMID)
		if err := p.vc.DestroyVM(ctx, target.VCenterVMID); err != nil {
			return &compensationRetryError{
				err: fmt.Errorf("destroy exact stale VM %s: %w", target.VCenterVMID, err),
			}
		}
		if err := p.db.CompleteVMCloneCleanup(ctx, jobID, podVMID, target.VCenterVMID); err != nil {
			return &compensationRetryError{
				err: fmt.Errorf("complete exact stale VM cleanup %s: %w", target.VCenterVMID, err),
			}
		}
		p.logger.Info("exact stale VM clone cleanup completed",
			"job_id", jobID, "pod_vm_id", podVMID, "moref", target.VCenterVMID)
	}
	return nil
}

func (p *Provisioner) jobCompensationCompleted(ctx context.Context, jobID uuid.UUID) (bool, error) {
	job, err := p.db.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, fmt.Errorf("cleanup job %s disappeared", jobID)
	}
	var payload struct {
		CleanupDone bool `json:"cleanup_completed"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return false, err
	}
	return payload.CleanupDone, nil
}

func (p *Provisioner) failVMAddWithCleanup(
	ctx context.Context,
	job *models.Job,
	podID, podVMID uuid.UUID,
	moref, reason string,
) error {
	_, _ = p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusPending, models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusError,
	)
	if err := p.stageVMCloneCleanup(ctx, job, podID, podVMID, moref); err != nil {
		return err
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	if err := p.cleanupStagedVMClone(ctx, job.ID, workerID); err != nil {
		return err
	}
	completed, err := p.jobCompensationCompleted(ctx, job.ID)
	if err != nil {
		return &compensationRetryError{
			err: fmt.Errorf("verify completed vm_add compensation: %w", err),
		}
	}
	if !completed {
		return &compensationRetryError{
			err: errors.New("vm_add cleanup lacks durable completion proof; manual resolution may be required"),
		}
	}
	return &compensatedJobError{err: fmt.Errorf("vm_add failed and exact clone cleanup completed: %s", reason)}
}

func newVMAddCompensatedError(reason string) error {
	return &compensatedJobError{
		err: fmt.Errorf("vm_add stopped before cloning; no external resource required cleanup: %s", reason),
	}
}

func (p *Provisioner) runVMAddCleanup(ctx context.Context, job *models.Job) error {
	var payload AddVMPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse cleanup-only vm_add payload: %w", err)
	}
	podVMID, err := parseUUID(payload.PodVMID)
	if err != nil {
		return &manualCleanupRequiredError{err: fmt.Errorf("invalid cleanup pod_vm_id: %w", err)}
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	if _, err := p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusPending, models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusError,
	); err != nil {
		return &compensationRetryError{err: fmt.Errorf("mark cleanup-only VM as error: %w", err)}
	}
	if err := p.cleanupStagedVMClone(ctx, job.ID, workerID); err != nil {
		return err
	}
	completed, err := p.jobCompensationCompleted(ctx, job.ID)
	if err != nil {
		return &compensationRetryError{
			err: fmt.Errorf("verify cleanup-only vm_add completion: %w", err),
		}
	}
	if !completed {
		return &manualCleanupRequiredError{
			err: errors.New("cleanup-only vm_add lacks durable completion proof; manual resolution may be required"),
		}
	}
	return &compensatedJobError{
		err: errors.New("vm_add failed; compensation completed"),
	}
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
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	if payload.CleanupOnly {
		return p.runVMAddCleanup(ctx, job)
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
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				*podVM.VCenterVMID,
				fmt.Sprintf("pod entered %s", pod.Status),
			)
		}
		p.logger.Warn("stale vm_add job skipped because pod is not active",
			"pod_id", podID, "pod_status", pod.Status, "pod_vm_id", podVMID, "job_id", job.ID)
		return newVMAddCompensatedError(fmt.Sprintf("pod entered %s", pod.Status))
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
			if currentMoref == "" {
				return newVMAddCompensatedError(fmt.Sprintf("VM entered %s", current.Status))
			}
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				currentMoref,
				fmt.Sprintf("VM entered %s", current.Status),
			)
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
		moref, err = executeDurableVMClone(ctx, p.db, p.vc, job.ID, workerID, podID, podVMID, vcenter.CloneVMParams{
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
			if moref != "" {
				return p.failVMAddWithCleanup(
					ctx,
					job,
					podID,
					podVMID,
					moref,
					jobErr.Error(),
				)
			}
			p.recordVMAddFailure(ctx, job, podVMID, jobErr)
			if errors.Is(err, vcenter.ErrAmbiguousVMOwnership) {
				return &manualCleanupRequiredError{err: jobErr}
			}
			return jobErr
		}
		applied, err = p.db.AdoptPodVMClone(
			ctx,
			job.ID,
			workerID,
			podVMID,
			[]string{models.VMStatusCloning},
			moref,
			payload.VMName,
			models.VMStatusConfiguring,
		)
		if err != nil {
			return &compensationRetryError{
				err: fmt.Errorf("resolve atomically staged clone adoption for %s: %w", moref, err),
			}
		}
		if !applied {
			p.logger.Warn("vm_add lost its provisioning state after clone; removing stale clone",
				"pod_id", podID, "pod_vm_id", podVMID, "moref", moref)
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				moref,
				"lost VM lifecycle ownership after clone",
			)
		}
		if err := p.db.DisarmVMCloneCleanup(ctx, job.ID, workerID, podVMID, moref); err != nil {
			return &compensationRetryError{
				err: fmt.Errorf("disarm adopted clone %s: %w", moref, err),
				target: &VMCloneCleanupTarget{
					PodID:       podID.String(),
					PodVMID:     podVMID.String(),
					VCenterVMID: moref,
				},
			}
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
		return p.failVMAddWithCleanup(
			ctx,
			job,
			podID,
			podVMID,
			moref,
			"lost VM lifecycle ownership after power-on",
		)
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
