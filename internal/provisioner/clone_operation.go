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

var (
	cloneOperationHandoffTimeout     = 2 * time.Minute
	cloneSubmissionReconcileDeadline = 30 * time.Minute
)

type cloneOperationStore interface {
	GetVMCloneOperation(
		ctx context.Context,
		jobID uuid.UUID,
		workerID string,
	) (*models.VMCloneOperation, error)
	PrepareVMCloneOperation(
		ctx context.Context,
		jobID uuid.UUID,
		workerID string,
		candidate models.VMCloneOperation,
	) (*models.VMCloneOperation, error)
	ArmVMCloneOperation(ctx context.Context, jobID uuid.UUID, workerID, operationID string) error
	AbandonUnsubmittedVMCloneOperation(
		ctx context.Context,
		jobID uuid.UUID,
		workerID, operationID string,
	) error
	PersistVMCloneTask(ctx context.Context, jobID uuid.UUID, workerID, operationID, taskRef string) error
	StageVMCloneCleanup(ctx context.Context, jobID uuid.UUID, workerID string, target []byte) error
	CompleteVMCloneOperationWithoutResource(
		ctx context.Context,
		jobID uuid.UUID,
		workerID, operationID string,
	) error
}

type cloneOperationClient interface {
	ResolveClonePlacement(ctx context.Context, params vcenter.CloneVMParams) (vcenter.CloneVMParams, error)
	StartCloneVMOperation(
		ctx context.Context,
		params vcenter.CloneVMParams,
		arm func(context.Context) error,
	) (string, error)
	WaitCloneVMTask(ctx context.Context, taskRef string) (string, error)
	FindVMByCloneOperation(ctx context.Context, params vcenter.CloneVMParams) (string, error)
	ValidateVMPlacement(ctx context.Context, vmMoref, expectedHostMoref string) error
	ConfigureClonedVM(ctx context.Context, moref string, params vcenter.CloneVMParams) error
}

func cloneOperationParams(op *models.VMCloneOperation, params vcenter.CloneVMParams) vcenter.CloneVMParams {
	params.OperationID = op.OperationID
	params.PodVMID = op.PodVMID
	params.LogicalTemplateID = op.LogicalTemplateID
	params.TemplateName = op.SourceRef
	params.SourceReplicaID = op.SourceReplicaID
	params.ComputeResourceType = op.ComputeResourceType
	params.ComputeResourceMoRef = op.ComputeResourceMoref
	params.VMName = op.TargetName
	params.HostMoRef = op.HostMoref
	params.HostName = op.HostName
	params.ResourcePoolMoRef = op.PoolMoref
	params.DRSControl = op.DRSControl
	return params
}

func persistCloneOperationWrite(
	ctx context.Context,
	write func(context.Context) error,
) error {
	handoffCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneOperationHandoffTimeout)
	defer cancel()
	for failures := 0; ; failures++ {
		if failures > 0 {
			timer := time.NewTimer(cleanupRescheduleBackoff(failures))
			select {
			case <-handoffCtx.Done():
				timer.Stop()
				return handoffCtx.Err()
			case <-timer.C:
			}
		}
		writeCtx, cancelWrite := context.WithTimeout(handoffCtx, cleanupRescheduleWriteTimeout)
		err := write(writeCtx)
		cancelWrite()
		if err == nil {
			return nil
		}
		if errors.Is(err, database.ErrJobLeaseLost) ||
			errors.Is(err, database.ErrVMCloneAlreadyDestroyed) {
			return err
		}
		if handoffCtx.Err() != nil {
			return fmt.Errorf("clone operation handoff deadline: %w", err)
		}
	}
}

func cloneRecoveryError(err error, target *VMCloneCleanupTarget) error {
	return &compensationRetryError{err: err, target: target}
}

type cloneForwardRetryError struct {
	err    error
	target *VMCloneCleanupTarget
}

func (e *cloneForwardRetryError) Error() string {
	return e.err.Error()
}

func (e *cloneForwardRetryError) Unwrap() error {
	return e.err
}

func newCloneForwardRetryError(err error, target *VMCloneCleanupTarget) error {
	return &cloneForwardRetryError{err: err, target: target}
}

func isCloneForwardRetry(err error) bool {
	var retryErr *cloneForwardRetryError
	return errors.As(err, &retryErr)
}

func cloneForwardRetryAvailable(job *models.Job, err error) bool {
	return !isCompensationRetry(err) &&
		!isManualCleanupRequired(err) &&
		jobRetryAvailable(job, err)
}

func cloneForwardFailureToCompensation(err error) error {
	var retryErr *cloneForwardRetryError
	if !errors.As(err, &retryErr) {
		return err
	}
	return cloneRecoveryError(err, retryErr.target)
}

func persistedClonePreambleFailure(
	job *models.Job,
	op *models.VMCloneOperation,
	err error,
) error {
	if op == nil {
		return err
	}
	if err == nil {
		err = errors.New("handler exited before reconciling its persisted clone operation")
	}
	if isCompensationRetry(err) || isManualCleanupRequired(err) {
		return err
	}
	classified := classifyCloneOperationFailure(err)
	if cloneForwardRetryAvailable(job, classified) {
		return classified
	}
	if isManualCleanupRequired(classified) || isCompensationRetry(classified) {
		return classified
	}
	if isCloneForwardRetry(classified) {
		return cloneForwardFailureToCompensation(classified)
	}
	return cloneRecoveryError(fmt.Errorf(
		"persisted clone operation %s requires exact compensation after preamble failure: %w",
		op.OperationID,
		classified,
	), nil)
}

func executeDurableVMClone(
	ctx context.Context,
	store cloneOperationStore,
	client cloneOperationClient,
	jobID uuid.UUID,
	workerID string,
	podID, podVMID uuid.UUID,
	params vcenter.CloneVMParams,
) (string, error) {
	if _, err := uuid.Parse(params.LogicalTemplateID); err != nil {
		return "", fmt.Errorf("durable clone requires a logical template ID: %w", err)
	}
	op, err := store.GetVMCloneOperation(ctx, jobID, workerID)
	if err != nil {
		return "", fmt.Errorf("load durable clone operation: %w", err)
	}
	if op == nil {
		resolvedParams, resolveErr := client.ResolveClonePlacement(ctx, params)
		if resolveErr != nil {
			return "", fmt.Errorf("resolve durable clone placement: %w", resolveErr)
		}
		params = resolvedParams
		candidate := models.VMCloneOperation{
			OperationID:          uuid.NewString(),
			PodID:                podID.String(),
			PodVMID:              podVMID.String(),
			LogicalTemplateID:    params.LogicalTemplateID,
			TargetName:           params.VMName,
			SourceReplicaID:      params.SourceReplicaID,
			SourceRef:            params.TemplateName,
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoref: params.ComputeResourceMoRef,
			HostMoref:            params.HostMoRef,
			HostName:             params.HostName,
			PoolMoref:            params.ResourcePoolMoRef,
			DRSControl:           params.DRSControl,
			Phase:                models.VMCloneOperationPrepared,
			PreparedAt:           time.Now().UTC(),
		}
		op, err = store.PrepareVMCloneOperation(ctx, jobID, workerID, candidate)
		if err != nil {
			return "", fmt.Errorf("prepare durable clone operation: %w", err)
		}
	}
	if op == nil ||
		op.PodID != podID.String() ||
		op.PodVMID != podVMID.String() ||
		op.LogicalTemplateID != params.LogicalTemplateID ||
		op.TargetName != params.VMName {
		return "", &manualCleanupRequiredError{err: errors.New(
			"persisted clone operation does not match the requested logical VM scope",
		)}
	}
	if op.LogicalTemplateID == "" ||
		op.ComputeResourceType == "" || op.ComputeResourceMoref == "" ||
		op.HostMoref == "" || op.HostName == "" || op.PoolMoref == "" ||
		op.DRSControl == "" {
		return "", &manualCleanupRequiredError{err: errors.New(
			"persisted clone operation is missing immutable host placement; automatic recovery is unsafe",
		)}
	}
	params = cloneOperationParams(op, params)

	var taskRef string
	switch op.Phase {
	case models.VMCloneOperationPrepared:
		armed := false
		taskRef, err = client.StartCloneVMOperation(ctx, params, func(armCtx context.Context) error {
			if err := store.ArmVMCloneOperation(armCtx, jobID, workerID, op.OperationID); err != nil {
				return err
			}
			armed = true
			return nil
		})
		if err != nil {
			if !armed {
				if abandonErr := persistCloneOperationWrite(ctx, func(writeCtx context.Context) error {
					return store.AbandonUnsubmittedVMCloneOperation(
						writeCtx,
						jobID,
						workerID,
						op.OperationID,
					)
				}); abandonErr != nil {
					return "", cloneRecoveryError(fmt.Errorf(
						"abandon unsubmitted clone operation %s after %v: %w",
						op.OperationID,
						err,
						abandonErr,
					), nil)
				}
				return "", fmt.Errorf("prepare clone submission: %w", err)
			}
			return "", cloneRecoveryError(fmt.Errorf(
				"clone submission outcome is ambiguous for operation %s: %w",
				op.OperationID,
				err,
			), nil)
		}
		if err := persistCloneOperationWrite(ctx, func(writeCtx context.Context) error {
			return store.PersistVMCloneTask(writeCtx, jobID, workerID, op.OperationID, taskRef)
		}); err != nil {
			return "", cloneRecoveryError(fmt.Errorf(
				"persist clone task %s for operation %s: %w",
				taskRef,
				op.OperationID,
				err,
			), nil)
		}
	case models.VMCloneOperationSubmitting:
		if op.TaskRef == "" {
			moref, reconcileErr := client.FindVMByCloneOperation(ctx, params)
			if reconcileErr != nil {
				if errors.Is(reconcileErr, vcenter.ErrAmbiguousVMOwnership) {
					return "", &manualCleanupRequiredError{err: reconcileErr}
				}
				target := cloneCleanupTarget(podID, podVMID, moref, params)
				if target != nil {
					if err := stageCloneCleanupTarget(ctx, store, jobID, workerID, target); err != nil {
						if isManualCleanupRequired(err) {
							return moref, err
						}
						return moref, cloneRecoveryError(err, target)
					}
				}
				classifiedErr := classifyPlacementValidationFailure(reconcileErr)
				if isManualCleanupRequired(classifiedErr) {
					return moref, classifiedErr
				}
				return moref, cloneRecoveryError(fmt.Errorf(
					"reconcile ambiguous clone operation %s: %w",
					op.OperationID,
					classifiedErr,
				), target)
			}
			if moref == "" {
				return "", cloneRecoveryError(fmt.Errorf(
					"clone operation %s remains unresolved; no duplicate clone was submitted",
					op.OperationID,
				), nil)
			}
			return stageAndConfigureClone(ctx, store, client, jobID, workerID, podID, podVMID, moref, params)
		}
		taskRef = op.TaskRef
	case models.VMCloneOperationSubmitted:
		if op.TaskRef == "" {
			return "", cloneRecoveryError(fmt.Errorf(
				"submitted clone operation %s has no task reference",
				op.OperationID,
			), nil)
		}
		taskRef = op.TaskRef
	default:
		return "", fmt.Errorf("unsupported clone operation phase %q", op.Phase)
	}

	moref, err := client.WaitCloneVMTask(ctx, taskRef)
	if err != nil {
		if errors.Is(err, vcenter.ErrCloneTaskFailed) {
			return "", cloneRecoveryError(fmt.Errorf(
				"durable clone operation %s task %s failed terminally: %w",
				op.OperationID,
				taskRef,
				err,
			), nil)
		}
		return "", newCloneForwardRetryError(fmt.Errorf(
			"wait for durable clone operation %s task %s: %w",
			op.OperationID,
			taskRef,
			err,
		), nil)
	}
	return stageAndConfigureClone(ctx, store, client, jobID, workerID, podID, podVMID, moref, params)
}

func cloneCleanupTarget(
	podID, podVMID uuid.UUID,
	moref string,
	params vcenter.CloneVMParams,
) *VMCloneCleanupTarget {
	if moref == "" {
		return nil
	}
	return &VMCloneCleanupTarget{
		PodID:                podID.String(),
		PodVMID:              podVMID.String(),
		VCenterVMID:          moref,
		LogicalTemplateID:    params.LogicalTemplateID,
		SourceReplicaID:      params.SourceReplicaID,
		SourceRef:            params.TemplateName,
		ComputeResourceType:  params.ComputeResourceType,
		ComputeResourceMoref: params.ComputeResourceMoRef,
		ResourcePoolMoref:    params.ResourcePoolMoRef,
		HostMoref:            params.HostMoRef,
		DRSControl:           params.DRSControl,
	}
}

func stageCloneCleanupTarget(
	ctx context.Context,
	store cloneOperationStore,
	jobID uuid.UUID,
	workerID string,
	target *VMCloneCleanupTarget,
) error {
	if err := validateVMCloneCleanupIdentity(target); err != nil {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"clone cleanup target has incomplete immutable placement: %w",
			err,
		)}
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return fmt.Errorf("marshal clone cleanup target: %w", err)
	}
	if err := persistCloneOperationWrite(ctx, func(writeCtx context.Context) error {
		return store.StageVMCloneCleanup(writeCtx, jobID, workerID, raw)
	}); err != nil {
		return fmt.Errorf("stage exact clone %s: %w", target.VCenterVMID, err)
	}
	return nil
}

func stageAndConfigureClone(
	ctx context.Context,
	store cloneOperationStore,
	client cloneOperationClient,
	jobID uuid.UUID,
	workerID string,
	podID, podVMID uuid.UUID,
	moref string,
	params vcenter.CloneVMParams,
) (string, error) {
	target := cloneCleanupTarget(podID, podVMID, moref, params)
	if err := stageCloneCleanupTarget(ctx, store, jobID, workerID, target); err != nil {
		if isManualCleanupRequired(err) {
			return moref, err
		}
		return moref, cloneRecoveryError(err, target)
	}
	if err := client.ValidateVMPlacement(ctx, moref, params.HostMoRef); err != nil {
		validationErr := classifyPlacementValidationFailure(fmt.Errorf(
			"clone %s placement validation failed: %w",
			moref,
			err,
		))
		if isManualCleanupRequired(validationErr) {
			return moref, validationErr
		}
		return moref, cloneRecoveryError(validationErr, target)
	}
	if err := client.ConfigureClonedVM(ctx, moref, params); err != nil {
		classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
			"configure exact clone %s: %w",
			moref,
			err,
		))
		if isManualCleanupRequired(classifiedErr) {
			return moref, classifiedErr
		}
		return moref, cloneRecoveryError(classifiedErr, target)
	}
	return moref, nil
}

func reconcileCloneOperationForCleanup(
	ctx context.Context,
	store cloneOperationStore,
	client cloneOperationClient,
	jobID uuid.UUID,
	workerID string,
	op *models.VMCloneOperation,
) error {
	_, err := reconcileCloneOperationTargetForCleanup(ctx, store, client, jobID, workerID, op)
	return err
}

func reconcileCloneOperationTargetForCleanup(
	ctx context.Context,
	store cloneOperationStore,
	client cloneOperationClient,
	jobID uuid.UUID,
	workerID string,
	op *models.VMCloneOperation,
) (*VMCloneCleanupTarget, error) {
	if op == nil {
		return nil, nil
	}
	if op.Phase == models.VMCloneOperationPrepared {
		if err := store.AbandonUnsubmittedVMCloneOperation(
			ctx,
			jobID,
			workerID,
			op.OperationID,
		); err != nil {
			return nil, cloneRecoveryError(fmt.Errorf(
				"abandon prepared clone operation %s during cleanup: %w",
				op.OperationID,
				err,
			), nil)
		}
		return nil, nil
	}
	if op.LogicalTemplateID == "" ||
		op.ComputeResourceType == "" || op.ComputeResourceMoref == "" ||
		op.HostMoref == "" || op.HostName == "" || op.PoolMoref == "" ||
		op.DRSControl == "" {
		return nil, &manualCleanupRequiredError{err: fmt.Errorf(
			"clone operation %s has no persisted host placement; automatic recovery is unsafe",
			op.OperationID,
		)}
	}
	params := cloneOperationParams(op, vcenter.CloneVMParams{})
	var (
		moref   string
		waitErr error
	)
	if op.TaskRef != "" {
		moref, waitErr = client.WaitCloneVMTask(ctx, op.TaskRef)
	}
	if moref == "" {
		found, err := client.FindVMByCloneOperation(ctx, params)
		moref = found
		if err != nil {
			if errors.Is(err, vcenter.ErrAmbiguousVMOwnership) {
				return nil, &manualCleanupRequiredError{err: err}
			}
			target := cloneCleanupTargetFromOperation(op, moref)
			if target != nil {
				if stageErr := stageCloneCleanupTarget(ctx, store, jobID, workerID, target); stageErr != nil {
					if isManualCleanupRequired(stageErr) {
						return target, stageErr
					}
					return target, cloneRecoveryError(stageErr, target)
				}
			}
			classifiedErr := classifyPlacementValidationFailure(err)
			if isManualCleanupRequired(classifiedErr) {
				return target, classifiedErr
			}
			return target, cloneRecoveryError(fmt.Errorf(
				"reconcile clone operation %s: %w",
				op.OperationID,
				classifiedErr,
			), target)
		}
	}
	if moref == "" {
		if errors.Is(waitErr, vcenter.ErrCloneTaskFailed) {
			if err := store.CompleteVMCloneOperationWithoutResource(
				ctx,
				jobID,
				workerID,
				op.OperationID,
			); err != nil {
				return nil, cloneRecoveryError(fmt.Errorf("complete failed clone operation %s: %w", op.OperationID, err), nil)
			}
			return nil, nil
		}
		if !op.PreparedAt.IsZero() && time.Since(op.PreparedAt) >= cloneSubmissionReconcileDeadline {
			return nil, &manualCleanupRequiredError{err: fmt.Errorf(
				"clone operation %s did not expose a task or marked VM before its reconciliation deadline",
				op.OperationID,
			)}
		}
		if waitErr != nil {
			return nil, cloneRecoveryError(fmt.Errorf("resume clone operation %s: %w", op.OperationID, waitErr), nil)
		}
		return nil, cloneRecoveryError(fmt.Errorf(
			"clone operation %s is still unresolved; retaining cleanup intent",
			op.OperationID,
		), nil)
	}
	target := cloneCleanupTargetFromOperation(op, moref)
	if err := stageCloneCleanupTarget(ctx, store, jobID, workerID, target); err != nil {
		if isManualCleanupRequired(err) {
			return target, err
		}
		return target, cloneRecoveryError(err, target)
	}
	if err := client.ValidateVMPlacement(ctx, moref, op.HostMoref); err != nil {
		classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
			"clone operation %s resolved to VM %s whose placement validation failed: %w",
			op.OperationID,
			moref,
			err,
		))
		if isManualCleanupRequired(classifiedErr) {
			return target, classifiedErr
		}
		return target, cloneRecoveryError(classifiedErr, target)
	}
	return target, nil
}
