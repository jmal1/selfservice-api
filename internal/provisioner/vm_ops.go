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

// VMCloneCleanupTarget identifies one external clone and the complete immutable
// placement decision that created it. Cleanup verifies this identity again
// before every destructive retry.
type VMCloneCleanupTarget struct {
	PodID                string `json:"pod_id"`
	PodVMID              string `json:"pod_vm_id"`
	VCenterVMID          string `json:"vcenter_vm_id"`
	LogicalTemplateID    string `json:"logical_template_id"`
	SourceReplicaID      string `json:"source_replica_id,omitempty"`
	SourceRef            string `json:"source_ref"`
	ComputeResourceType  string `json:"compute_resource_type"`
	ComputeResourceMoref string `json:"compute_resource_moref"`
	ResourcePoolMoref    string `json:"resource_pool_moref"`
	HostMoref            string `json:"host_moref"`
	DRSControl           string `json:"drs_control"`
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

func validateVMCloneCleanupIdentity(target *VMCloneCleanupTarget) error {
	if _, _, err := validateVMCloneCleanupTarget(target); err != nil {
		return err
	}
	if _, err := uuid.Parse(target.LogicalTemplateID); err != nil {
		return fmt.Errorf("cleanup target has invalid logical_template_id: %w", err)
	}
	if target.SourceReplicaID != "" {
		if _, err := uuid.Parse(target.SourceReplicaID); err != nil {
			return fmt.Errorf("cleanup target has invalid source_replica_id: %w", err)
		}
	}
	if target.SourceRef == "" {
		return errors.New("cleanup target has no exact source reference")
	}
	if target.ComputeResourceType != "ClusterComputeResource" &&
		target.ComputeResourceType != "ComputeResource" {
		return fmt.Errorf("cleanup target has unsupported compute resource type %q", target.ComputeResourceType)
	}
	if target.ComputeResourceMoref == "" || target.ResourcePoolMoref == "" || target.HostMoref == "" {
		return errors.New("cleanup target has incomplete compute, pool, or host identity")
	}
	if target.DRSControl != vcenter.DRSControlDisabled &&
		target.DRSControl != vcenter.DRSControlStandalone {
		return fmt.Errorf("cleanup target has unsupported DRS control %q", target.DRSControl)
	}
	return nil
}

func cloneCleanupTargetFromPlacement(
	podID uuid.UUID,
	moref string,
	placement models.VMPlacement,
) *VMCloneCleanupTarget {
	sourceReplicaID := ""
	if placement.SourceReplicaID != nil {
		sourceReplicaID = placement.SourceReplicaID.String()
	}
	return &VMCloneCleanupTarget{
		PodID:                podID.String(),
		PodVMID:              placement.PodVMID.String(),
		VCenterVMID:          moref,
		LogicalTemplateID:    placement.TemplateID.String(),
		SourceReplicaID:      sourceReplicaID,
		SourceRef:            placement.SourceRef,
		ComputeResourceType:  placement.ComputeResourceType,
		ComputeResourceMoref: placement.ComputeResourceMoref,
		ResourcePoolMoref:    placement.ResourcePoolMoref,
		HostMoref:            placement.HostMoref,
		DRSControl:           placement.DRSControl,
	}
}

func cloneCleanupTargetFromOperation(
	op *models.VMCloneOperation,
	moref string,
) *VMCloneCleanupTarget {
	if op == nil {
		return nil
	}
	return &VMCloneCleanupTarget{
		PodID:                op.PodID,
		PodVMID:              op.PodVMID,
		VCenterVMID:          moref,
		LogicalTemplateID:    op.LogicalTemplateID,
		SourceReplicaID:      op.SourceReplicaID,
		SourceRef:            op.SourceRef,
		ComputeResourceType:  op.ComputeResourceType,
		ComputeResourceMoref: op.ComputeResourceMoref,
		ResourcePoolMoref:    op.PoolMoref,
		HostMoref:            op.HostMoref,
		DRSControl:           op.DRSControl,
	}
}

func mergeCloneCleanupIdentity(
	actual, expected *VMCloneCleanupTarget,
) (*VMCloneCleanupTarget, error) {
	if _, _, err := validateVMCloneCleanupTarget(actual); err != nil {
		return nil, err
	}
	if err := validateVMCloneCleanupIdentity(expected); err != nil {
		return nil, fmt.Errorf("persisted cleanup identity is invalid: %w", err)
	}
	if actual.PodID != expected.PodID ||
		actual.PodVMID != expected.PodVMID ||
		actual.VCenterVMID != expected.VCenterVMID {
		return nil, errors.New("cleanup target does not match its persisted pod, VM, and vCenter identity")
	}
	type identityField struct {
		name     string
		actual   string
		expected string
	}
	for _, field := range []identityField{
		{"logical template", actual.LogicalTemplateID, expected.LogicalTemplateID},
		{"source replica", actual.SourceReplicaID, expected.SourceReplicaID},
		{"source reference", actual.SourceRef, expected.SourceRef},
		{"compute resource type", actual.ComputeResourceType, expected.ComputeResourceType},
		{"compute resource", actual.ComputeResourceMoref, expected.ComputeResourceMoref},
		{"resource pool", actual.ResourcePoolMoref, expected.ResourcePoolMoref},
		{"host", actual.HostMoref, expected.HostMoref},
		{"DRS control", actual.DRSControl, expected.DRSControl},
	} {
		if field.actual != "" && field.actual != field.expected {
			return nil, fmt.Errorf(
				"cleanup target %s %q does not match persisted identity %q",
				field.name,
				field.actual,
				field.expected,
			)
		}
	}
	return expected, nil
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

func shouldCompensateVMAddFailure(
	job *models.Job,
	jobErr error,
	vmRunningPersisted bool,
) bool {
	return jobErr != nil &&
		!vmRunningPersisted &&
		!jobRetryAvailable(job, jobErr) &&
		!isCompensatedJobError(jobErr) &&
		!isCompensationRetry(jobErr) &&
		!isManualCleanupRequired(jobErr)
}

func (p *Provisioner) resolvePodCloneCleanupIdentity(
	ctx context.Context,
	podID, podVMID uuid.UUID,
	target *VMCloneCleanupTarget,
) (*VMCloneCleanupTarget, error) {
	placement, err := p.db.GetVMPlacement(ctx, podVMID)
	if err != nil {
		return nil, fmt.Errorf("load persisted placement for cleanup target %s: %w", podVMID, err)
	}
	if placement == nil {
		return nil, &manualCleanupRequiredError{err: fmt.Errorf(
			"pod VM %s has no persisted placement identity; automatic cleanup is unsafe",
			podVMID,
		)}
	}
	expected := cloneCleanupTargetFromPlacement(podID, target.VCenterVMID, *placement)
	resolved, err := mergeCloneCleanupIdentity(target, expected)
	if err != nil {
		return nil, &manualCleanupRequiredError{err: fmt.Errorf(
			"cleanup target identity mismatch for pod VM %s: %w",
			podVMID,
			err,
		)}
	}
	return resolved, nil
}

func (p *Provisioner) destroyExactCloneTarget(
	ctx context.Context,
	target *VMCloneCleanupTarget,
) error {
	if err := validateVMCloneCleanupIdentity(target); err != nil {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"exact clone cleanup identity is invalid: %w",
			err,
		)}
	}
	err := p.vc.DestroyVMWithPlacement(
		ctx,
		target.VCenterVMID,
		target.HostMoref,
		target.ComputeResourceType,
		target.ComputeResourceMoref,
		target.DRSControl,
		vcenter.VMCloneIdentity{
			LogicalTemplateID:    target.LogicalTemplateID,
			SourceReplicaID:      target.SourceReplicaID,
			SourceRef:            target.SourceRef,
			PodVMID:              target.PodVMID,
			ComputeResourceType:  target.ComputeResourceType,
			ComputeResourceMoref: target.ComputeResourceMoref,
			ResourcePoolMoref:    target.ResourcePoolMoref,
			HostMoref:            target.HostMoref,
		},
	)
	if err == nil {
		return nil
	}
	p.recordVMPlacementDrift(err)
	return classifyPlacementValidationFailure(err)
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
	placement, err := p.db.GetVMPlacement(ctx, podVMID)
	if err != nil {
		return &compensationRetryError{err: fmt.Errorf(
			"load persisted placement before staging exact clone %s: %w",
			moref,
			err,
		)}
	}
	if placement == nil {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"pod VM %s has no persisted placement identity for exact clone %s",
			podVMID,
			moref,
		)}
	}
	cleanupTarget := cloneCleanupTargetFromPlacement(podID, moref, *placement)
	if err := validateVMCloneCleanupIdentity(cleanupTarget); err != nil {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"persisted placement for exact clone %s is incomplete: %w",
			moref,
			err,
		)}
	}
	target, err := json.Marshal(cleanupTarget)
	if err != nil {
		return &manualCleanupRequiredError{
			err: fmt.Errorf("marshal exact VM cleanup target %s: %w", moref, err),
		}
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return &compensationRetryError{
			err:    err,
			target: cleanupTarget,
		}
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
		return &compensationRetryError{
			err:    err,
			target: cleanupTarget,
		}
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
		podID, podVMID, err := validateVMCloneCleanupTarget(target)
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

		resolved, err := p.resolvePodCloneCleanupIdentity(ctx, podID, podVMID, target)
		if err != nil {
			if isManualCleanupRequired(err) {
				return err
			}
			return &compensationRetryError{err: err, target: target}
		}
		if err := p.destroyExactCloneTarget(ctx, resolved); err != nil {
			if isManualCleanupRequired(err) {
				return err
			}
			return &compensationRetryError{
				err:    fmt.Errorf("destroy exact stale VM %s: %w", resolved.VCenterVMID, err),
				target: resolved,
			}
		}
		if err := p.db.CompleteVMCloneCleanup(ctx, jobID, podVMID, resolved.VCenterVMID); err != nil {
			return &compensationRetryError{
				err:    fmt.Errorf("complete exact stale VM cleanup %s: %w", resolved.VCenterVMID, err),
				target: resolved,
			}
		}
		p.logger.Info("exact stale VM clone cleanup completed",
			"job_id", jobID, "pod_vm_id", podVMID, "moref", resolved.VCenterVMID)
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
	moref string,
	cause error,
) error {
	if err := p.stageVMCloneCleanup(ctx, job, podID, podVMID, moref); err != nil {
		if isCompensatedJobError(err) {
			if isManualCleanupRequired(cause) || isCompensationRetry(cause) {
				return cause
			}
			return &compensatedJobError{err: fmt.Errorf("%w; exact clone cleanup completed", cause)}
		}
		if isManualCleanupRequired(err) {
			return err
		}
		var retryErr *compensationRetryError
		if errors.As(err, &retryErr) {
			return retryErr
		}
		return &compensationRetryError{
			err: fmt.Errorf("%v; stage exact clone %s for cleanup: %w", cause, moref, err),
		}
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	if err := p.cleanupStagedVMClone(ctx, job.ID, workerID); err != nil {
		return err
	}
	if err := p.finalizeVMAddCompensation(ctx, job, podVMID, true); err != nil {
		return err
	}
	if isManualCleanupRequired(cause) || isCompensationRetry(cause) {
		return cause
	}
	return &compensatedJobError{err: fmt.Errorf("vm_add failed and exact clone cleanup completed: %w", cause)}
}

func (p *Provisioner) finalizeVMAddCompensation(
	ctx context.Context,
	job *models.Job,
	podVMID uuid.UUID,
	recordCompletion bool,
) error {
	if err := p.releaseVMPlacementCapacity(ctx, job, []uuid.UUID{podVMID}); err != nil {
		return &compensationRetryError{
			err: fmt.Errorf("release compensated vm_add capacity: %w", err),
		}
	}
	if _, err := p.db.UpdatePodVMStatusFrom(
		ctx,
		podVMID,
		[]string{models.VMStatusPending, models.VMStatusCloning, models.VMStatusConfiguring},
		models.VMStatusError,
	); err != nil {
		return &compensationRetryError{err: fmt.Errorf("mark compensated VM as error: %w", err)}
	}
	if recordCompletion {
		workerID, _, err := claimedJobLease(job)
		if err != nil {
			return &compensationRetryError{err: err}
		}
		if err := p.db.MarkJobCompensationCompleted(ctx, job.ID, workerID); err != nil {
			return &compensationRetryError{
				err: fmt.Errorf("record completed vm_add compensation: %w", err),
			}
		}
	}
	return nil
}

func (p *Provisioner) completeVMAddWithoutClone(
	ctx context.Context,
	job *models.Job,
	podVMID uuid.UUID,
	reason string,
) error {
	if err := p.finalizeVMAddCompensation(ctx, job, podVMID, false); err != nil {
		return err
	}
	return newVMAddCompensatedError(reason)
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
	if err := p.cleanupStagedVMClone(ctx, job.ID, workerID); err != nil {
		return err
	}
	if err := p.finalizeVMAddCompensation(ctx, job, podVMID, true); err != nil {
		return err
	}
	return &compensatedJobError{
		err: errors.New("vm_add failed; compensation completed"),
	}
}

// AddVM clones and powers on a new VM in an existing pod.
func (p *Provisioner) AddVM(ctx context.Context, job *models.Job) (retErr error) {
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
	moref := ""
	vmRunningPersisted := false
	clonePreamblePending := payload.CloneOperation != nil
	defer func() {
		if clonePreamblePending {
			retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)
		}
		if !shouldCompensateVMAddFailure(job, retErr, vmRunningPersisted) {
			return
		}
		if moref != "" {
			retErr = p.failVMAddWithCleanup(ctx, job, podID, podVMID, moref, retErr)
			return
		}
		if finalizeErr := p.finalizeVMAddCompensation(ctx, job, podVMID, false); finalizeErr != nil {
			retErr = combineProvisioningAndCleanupErrors(retErr, finalizeErr)
			return
		}
		retErr = &compensatedJobError{err: retErr}
	}()

	pod, err := p.db.GetPodByID(ctx, podID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}

	podVM, err := p.db.GetPodVM(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("get pod VM: %w", err)
	}
	if podVM.VCenterVMID != nil {
		moref = *podVM.VCenterVMID
	}
	vmRunningPersisted = podVM.Status == models.VMStatusRunning
	if pod.Status != models.PodStatusActive {
		if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" {
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				*podVM.VCenterVMID,
				fmt.Errorf("pod entered %s", pod.Status),
			)
		}
		p.logger.Warn("stale vm_add job skipped because pod is not active",
			"pod_id", podID, "pod_status", pod.Status, "pod_vm_id", podVMID, "job_id", job.ID)
		if payload.CloneOperation != nil {
			return persistedClonePreambleFailure(
				job,
				payload.CloneOperation,
				fmt.Errorf("pod entered %s before persisted clone reconciliation", pod.Status),
			)
		}
		return p.completeVMAddWithoutClone(
			ctx,
			job,
			podVMID,
			fmt.Sprintf("pod entered %s", pod.Status),
		)
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
				if payload.CloneOperation != nil {
					return persistedClonePreambleFailure(
						job,
						payload.CloneOperation,
						fmt.Errorf("VM entered %s before persisted clone reconciliation", current.Status),
					)
				}
				return p.completeVMAddWithoutClone(
					ctx,
					job,
					podVMID,
					fmt.Sprintf("VM entered %s", current.Status),
				)
			}
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				currentMoref,
				fmt.Errorf("VM entered %s", current.Status),
			)
		}
		if current.Status == models.VMStatusRunning && currentMoref != "" {
			if payload.CloneOperation != nil {
				return persistedClonePreambleFailure(
					job,
					payload.CloneOperation,
					fmt.Errorf(
						"VM %s is running while clone operation %s remains persisted",
						podVMID,
						payload.CloneOperation.OperationID,
					),
				)
			}
			vmRunningPersisted = true
			if err := p.releaseVMPlacementCapacity(ctx, job, []uuid.UUID{podVMID}); err != nil {
				return fmt.Errorf("release running added VM capacity reservation: %w", err)
			}
			return nil
		}
		p.logger.Warn("stale vm_add job skipped because VM is no longer provisionable",
			"pod_id", podID, "pod_vm_id", podVMID, "vm_status", podVM.Status, "job_id", job.ID)
		if payload.CloneOperation != nil {
			return persistedClonePreambleFailure(
				job,
				payload.CloneOperation,
				fmt.Errorf(
					"VM entered non-provisionable state %s before persisted clone reconciliation",
					current.Status,
				),
			)
		}
		return nil
	}

	pgName := fmt.Sprintf("Pod-VLAN%d", pod.VLANID)

	// Resolve and durably persist credentials before clone submission. Retries
	// must reuse the exact password already written to guestinfo instead of
	// generating a new display-only value for an adopted clone.
	osType := ""
	tmpl, tmplErr := p.db.GetTemplateByID(ctx, podVM.TemplateID)
	if tmplErr == nil {
		osType = tmpl.OSType
	}
	if tmplErr != nil {
		return fmt.Errorf("get template for VM placement: %w", tmplErr)
	}
	storedUsername, storedPassword, credentialErr := provisionedPodVMCredentials(
		tmpl.Kind,
		osType,
		podVM,
		tmpl,
		generatePassword,
	)
	if credentialErr != nil {
		return fmt.Errorf("resolve credentials for added VM %s: %w", podVMID, credentialErr)
	}
	if err := p.db.UpdatePodVMCredentials(
		ctx,
		podVMID,
		storedUsername,
		storedPassword,
	); err != nil {
		return fmt.Errorf("persist credentials for added VM %s before clone: %w", podVMID, err)
	}
	customizationPassword := ""
	if shouldGenerateGuestPassword(tmpl.Kind, osType) {
		customizationPassword = storedPassword
	}
	receiptRecord, err := p.db.GetPodPortGroupReceipt(ctx, podID)
	if err != nil {
		return fmt.Errorf("load pod port group receipt for VM placement: %w", err)
	}
	if receiptRecord.RemovedAt != nil {
		return fmt.Errorf("pod port group receipt was already removed")
	}
	var receipt vcenter.PortGroupReceipt
	if err := json.Unmarshal(receiptRecord.Receipt, &receipt); err != nil {
		return fmt.Errorf("parse pod port group receipt for VM placement: %w", err)
	}
	if receipt.Name != pgName || receipt.VLANID != int(pod.VLANID) || len(receipt.Hosts) == 0 {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"pod port group receipt is incomplete or mismatched for %s",
			pgName,
		)}
	}
	targetHosts := make([]string, 0, len(receipt.Hosts))
	for _, host := range receipt.Hosts {
		targetHosts = append(targetHosts, host.HostMoRef)
	}
	placements, err := p.prepareVMPlacementPlan(ctx, job, []vmPlacementSpec{{
		PodVMID:   podVMID,
		SourceRef: payload.TemplateName,
	}}, pgName, false, targetHosts)
	if err != nil {
		jobErr := classifyPlacementValidationFailure(fmt.Errorf("plan added VM placement: %w", err))
		if jobRetryAvailable(job, jobErr) {
			return jobErr
		}
		if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" &&
			!isManualCleanupRequired(jobErr) {
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				*podVM.VCenterVMID,
				jobErr,
			)
		}
		if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
			if payload.CloneOperation != nil {
				return persistedClonePreambleFailure(job, payload.CloneOperation, jobErr)
			}
			return p.completeVMAddWithoutClone(ctx, job, podVMID, jobErr.Error())
		}
		return jobErr
	}
	if err := p.vc.ValidateDRSPlacementPrivileges(ctx, vcenter.DRSPlacementTargets(placements)); err != nil {
		jobErr := fmt.Errorf("validate mandatory DRS placement privileges for added VM: %w", err)
		if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" {
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				*podVM.VCenterVMID,
				jobErr,
			)
		}
		return p.completeVMAddWithoutClone(ctx, job, podVMID, jobErr.Error())
	}
	if err := p.enforceExistingVMPlacements(ctx, job.ID, workerID, placements); err != nil {
		if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" &&
			!isManualCleanupRequired(err) {
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				*podVM.VCenterVMID,
				err,
			)
		}
		if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
			return p.completeVMAddWithoutClone(ctx, job, podVMID, err.Error())
		}
		return err
	}
	placement := placements[0]
	// Resume support: skip clone if VM was already cloned (e.g., job retry after worker restart)
	if podVM.VCenterVMID != nil && *podVM.VCenterVMID != "" &&
		payload.CloneOperation == nil {
		moref = *podVM.VCenterVMID
		p.logger.Info("resuming VM add — already cloned", "vm", payload.VMName, "moref", moref)
	} else {
		// Step 1: Clone VM
		p.publishProgress(job.ID, "vm_clone", fmt.Sprintf("Cloning %s from %s", payload.VMName, payload.TemplateName))

		var err error
		existingVMMoref := moref
		cloneParams := cloneParamsFromPlacement(vcenter.CloneVMParams{
			TemplateName: payload.TemplateName,
			VMName:       payload.VMName,
			VCPUs:        int32(podVM.VCPUs),
			RAMmb:        int64(podVM.RAMMB),
			Network:      pgName,
			OSType:       osType,
			Password:     customizationPassword,
		}, placement)
		moref, err = executeDurableVMClone(
			ctx,
			p.db,
			p.vc,
			job.ID,
			workerID,
			podID,
			podVMID,
			cloneParams,
		)
		if err != nil {
			jobErr := classifyCloneOperationFailure(fmt.Errorf("clone VM: %w", err))
			if cloneForwardRetryAvailable(job, jobErr) {
				return jobErr
			}
			if isManualCleanupRequired(jobErr) || isCompensationRetry(jobErr) {
				if moref == "" {
					return jobErr
				}
				return p.failVMAddWithCleanup(
					ctx,
					job,
					podID,
					podVMID,
					moref,
					jobErr,
				)
			}
			if moref != "" {
				return p.failVMAddWithCleanup(
					ctx,
					job,
					podID,
					podVMID,
					moref,
					jobErr,
				)
			}
			if isCloneForwardRetry(jobErr) {
				return cloneForwardFailureToCompensation(jobErr)
			}
			if payload.CloneOperation != nil {
				return persistedClonePreambleFailure(job, payload.CloneOperation, jobErr)
			}
			return p.completeVMAddWithoutClone(ctx, job, podVMID, jobErr.Error())
		}
		if existingVMMoref != "" && existingVMMoref != moref {
			return p.failVMAddWithCleanup(
				ctx,
				job,
				podID,
				podVMID,
				moref,
				fmt.Errorf(
					"refuse to overwrite existing added VM reference %s with resumed clone %s",
					existingVMMoref,
					moref,
				),
			)
		}
		clonePreamblePending = false
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
				errors.New("lost VM lifecycle ownership after clone"),
			)
		}
		if err := p.db.DisarmVMCloneCleanup(ctx, job.ID, workerID, podVMID, moref); err != nil {
			return &compensationRetryError{
				err:    fmt.Errorf("disarm adopted clone %s: %w", moref, err),
				target: cloneCleanupTargetFromPlacement(podID, moref, placement),
			}
		}

	}

	if err := p.verifyPersistedVMPlacement(ctx, moref, placement); err != nil {
		classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
			"refuse to resume persisted VM %s after placement validation failed: %w",
			moref,
			err,
		))
		if jobRetryAvailable(job, classifiedErr) {
			return classifiedErr
		}
		if isManualCleanupRequired(classifiedErr) {
			return classifiedErr
		}
		return p.failVMAddWithCleanup(
			ctx,
			job,
			podID,
			podVMID,
			moref,
			classifiedErr,
		)
	}

	// Step 2: Power on
	p.publishProgress(job.ID, "vm_poweron", fmt.Sprintf("Powering on %s", payload.VMName))
	if err := p.vc.PowerOnVM(ctx, moref); err != nil {
		jobErr := classifyPlacementValidationFailure(fmt.Errorf("power on VM: %w", err))
		if jobRetryAvailable(job, jobErr) {
			return jobErr
		}
		if isManualCleanupRequired(jobErr) {
			return jobErr
		}
		return p.failVMAddWithCleanup(
			ctx,
			job,
			podID,
			podVMID,
			moref,
			jobErr,
		)
	}

	// Step 3: prove customized credentials were consumed before the VM can
	// become success-shaped. Static credential kinds intentionally bypass this
	// generated-credential gate.
	if err := waitForPodVMCredentialReady(
		ctx,
		p.vc,
		tmpl.Kind,
		osType,
		moref,
		storedUsername,
		storedPassword,
		podGuestCredentialReadyTimeout,
		podGuestCredentialRetryInterval,
	); err != nil {
		return p.failVMAddWithCleanup(
			ctx,
			job,
			podID,
			podVMID,
			moref,
			fmt.Errorf("verify added VM guest credentials: %w", err),
		)
	}

	// Step 4: Wait for IP
	ip, err := p.vc.WaitForIP(ctx, moref, 5*time.Minute)
	if err != nil {
		p.logger.Warn("timeout waiting for VM IP", "vm", payload.VMName, "error", err)
	} else {
		_ = p.db.UpdatePodVMIP(ctx, podVMID, ip)
	}

	if err := p.releaseVMPlacementCapacity(ctx, job, []uuid.UUID{podVMID}); err != nil {
		return fmt.Errorf("release running added VM capacity reservation: %w", err)
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
			vmRunningPersisted = true
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
			errors.New("lost VM lifecycle ownership after power-on"),
		)
	}
	vmRunningPersisted = true

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
