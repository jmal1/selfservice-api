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

const replicaBuildAmbiguousSubmissionDeadline = 30 * time.Minute

var errTemplateReplicaBuildTaskFailed = errors.New("template replica build vCenter task failed")

func terminalReplicaBuildTaskError(err error) error {
	if errors.Is(err, vcenter.ErrCloneTaskFailed) {
		return fmt.Errorf("%w: %w", errTemplateReplicaBuildTaskFailed, err)
	}
	return err
}

type TemplateReplicaBuildPayload struct {
	BuildID     uuid.UUID `json:"build_id"`
	TemplateID  uuid.UUID `json:"template_id"`
	CleanupOnly bool      `json:"cleanup_only,omitempty"`
}

type templateReplicaBuildStore interface {
	GetTemplateReplicaBuildForJob(context.Context, uuid.UUID, string) (*models.TemplateReplicaBuild, error)
	SaveTemplateReplicaBuildState(context.Context, *models.TemplateReplicaBuild, string, string) error
	EnsurePendingReplicaForBuild(context.Context, *models.TemplateReplicaBuild, string) error
	FinalizeTemplateReplicaBuild(context.Context, *models.TemplateReplicaBuild, string) error
	FailTemplateReplicaBuild(context.Context, *models.TemplateReplicaBuild, string, string, string, string, string) error
}

type templateReplicaBuildClient interface {
	StartReplicaBuildClone(context.Context, vcenter.ReplicaBuildCloneParams, func(context.Context) error) (string, string, error)
	FindReplicaBuildVM(context.Context, vcenter.ReplicaBuildCloneParams) (string, error)
	WaitReplicaBuildCloneTask(context.Context, string) (string, error)
	ValidateReplicaBuildClone(context.Context, vcenter.ReplicaBuildCloneParams) error
	StartReplicaBuildSnapshot(context.Context, string, string, string, string, func(context.Context) error) (string, error)
	FindReplicaSnapshot(context.Context, string, string) (string, error)
	WaitReplicaBuildSnapshotTask(context.Context, string) (string, error)
	StartReplicaBuildCanary(context.Context, vcenter.ReplicaBuildCloneParams, func(context.Context) error) (string, error)
	FindReplicaBuildCanary(context.Context, vcenter.ReplicaBuildCloneParams) (string, error)
	WaitReplicaBuildCleanupTask(context.Context, string) error
	ValidateReplicaBuildCanary(context.Context, vcenter.ReplicaBuildCloneParams) error
	StartReplicaBuildCanaryCleanup(context.Context, string, string, string, func(context.Context) error) (string, bool, error)
	StartReplicaBuildResidueCleanup(context.Context, string, string, string, func(context.Context) error) (string, bool, error)
	ReplicaBuildCanaryExists(context.Context, vcenter.ReplicaBuildCloneParams) (bool, error)
	ReplicaBuildRetainedVMExists(context.Context, vcenter.ReplicaBuildCloneParams) (bool, error)
}

func replicaBuildTarget(build *models.TemplateReplicaBuild) vcenter.ReplicaBuildTarget {
	return vcenter.ReplicaBuildTarget{
		ComputeResourceType:  build.ComputeResourceType,
		ComputeResourceMoref: build.ComputeResourceMoref,
		ComputeResourcePath:  build.ComputeResourcePath,
		HostMoref:            build.HostMoref,
		HostName:             build.HostName,
		ResourcePoolMoref:    build.ResourcePoolMoref,
		ResourcePoolPath:     build.ResourcePoolPath,
		DatastoreMoref:       build.DatastoreMoref,
		DatastoreName:        build.DatastoreName,
		FolderMoref:          build.FolderMoref,
		FolderPath:           build.FolderPath,
		ProvisionDatastore:   build.ProvisionDatastore,
	}
}

func retainedReplicaBuildParams(build *models.TemplateReplicaBuild) vcenter.ReplicaBuildCloneParams {
	return vcenter.ReplicaBuildCloneParams{
		BuildID:             build.ID.String(),
		OperationID:         build.OperationID,
		Kind:                vcenter.ReplicaBuildDestinationKind,
		TemplateID:          build.TemplateID.String(),
		SourceReplicaID:     build.SourceReplicaID.String(),
		SourceVMMoref:       build.SourceVMMoref,
		SourceSnapshotName:  build.SourceSnapshotName,
		SourceSnapshotMoref: build.SourceSnapshotMoref,
		DestinationName:     build.DestinationName,
		DestinationVMMoref:  build.DestinationVMMoref,
		DestinationSnapshot: build.DestinationSnapshot,
		Target:              replicaBuildTarget(build),
	}
}

func canaryReplicaBuildParams(build *models.TemplateReplicaBuild) vcenter.ReplicaBuildCloneParams {
	params := retainedReplicaBuildParams(build)
	params.SourceOperationID = build.OperationID
	params.OperationID = build.CanaryOperationID
	params.Kind = vcenter.ReplicaBuildCanaryKind
	params.SourceVMMoref = build.DestinationVMMoref
	params.SourceSnapshotMoref = build.DestinationSnapshot
	params.DestinationName = build.DestinationName + "-canary"
	params.DestinationVMMoref = build.CanaryVMMoref
	params.UseProvisionDatastore = true
	return params
}

func saveReplicaBuildState(
	ctx context.Context,
	store templateReplicaBuildStore,
	build *models.TemplateReplicaBuild,
	workerID string,
	mutate func(),
) error {
	expected := build.Phase
	mutate()
	if expected != build.Phase && isReplicaBuildSubmittingPhase(build.Phase) {
		now := time.Now().UTC()
		build.SubmissionStartedAt = &now
	}
	if err := store.SaveTemplateReplicaBuildState(ctx, build, expected, workerID); err != nil {
		return fmt.Errorf("persist replica build phase %s -> %s: %w", expected, build.Phase, err)
	}
	return nil
}

func isReplicaBuildSubmittingPhase(phase string) bool {
	switch phase {
	case models.TemplateReplicaBuildPhaseCloneSubmitting,
		models.TemplateReplicaBuildPhaseSnapshotSubmitting,
		models.TemplateReplicaBuildPhaseCanarySubmitting,
		models.TemplateReplicaBuildPhaseCleanupSubmitting,
		models.TemplateReplicaBuildPhaseResidueSubmitting:
		return true
	default:
		return false
	}
}

func replicaBuildReconcileWait(phase string) error {
	return fmt.Errorf("resource temporarily unavailable: replica build %s is awaiting ambiguous vCenter submission reconciliation", phase)
}

func replicaBuildSubmissionExpired(build *models.TemplateReplicaBuild) bool {
	return build.SubmissionStartedAt != nil &&
		time.Since(*build.SubmissionStartedAt) > replicaBuildAmbiguousSubmissionDeadline
}

func executeTemplateReplicaBuild(
	ctx context.Context,
	store templateReplicaBuildStore,
	client templateReplicaBuildClient,
	job *models.Job,
	workerID string,
) error {
	build, err := store.GetTemplateReplicaBuildForJob(ctx, job.ID, workerID)
	if err != nil {
		return terminalReplicaBuildTaskError(err)
	}
	if build.Status == models.TemplateReplicaBuildReady {
		return nil
	}
	for {
		switch build.Phase {
		case models.TemplateReplicaBuildPhasePending:
			if build.SourceSnapshotMoref == "" {
				snapshotRef, err := client.FindReplicaSnapshot(
					ctx,
					build.SourceVMMoref,
					build.SourceSnapshotName,
				)
				if err != nil {
					return err
				}
				if snapshotRef == "" {
					return fmt.Errorf("source snapshot %q was not found", build.SourceSnapshotName)
				}
				if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
					build.SourceSnapshotMoref = snapshotRef
				}); err != nil {
					return err
				}
			}
			started := time.Now().UTC()
			armed := false
			taskRef, snapshotRef, err := client.StartReplicaBuildClone(
				ctx,
				retainedReplicaBuildParams(build),
				func(armCtx context.Context) error {
					err := saveReplicaBuildState(armCtx, store, build, workerID, func() {
						build.Status = models.TemplateReplicaBuildRunning
						build.Phase = models.TemplateReplicaBuildPhaseCloneSubmitting
						build.StartedAt = &started
					})
					armed = err == nil
					return err
				},
			)
			if err != nil {
				if armed {
					return fmt.Errorf("replica build clone submission outcome is ambiguous: %w", err)
				}
				return err
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CloneTaskRef = taskRef
				build.SourceSnapshotMoref = snapshotRef
				build.Phase = models.TemplateReplicaBuildPhaseCloneSubmitted
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCloneSubmitting:
			moref, err := client.FindReplicaBuildVM(ctx, retainedReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if moref == "" {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf("%w: clone submission did not produce a marked VM within %s",
						vcenter.ErrReplicaBuildAmbiguous,
						replicaBuildAmbiguousSubmissionDeadline)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			snapshotRef, err := client.FindReplicaSnapshot(ctx, build.SourceVMMoref, build.SourceSnapshotName)
			if err != nil {
				return err
			}
			if snapshotRef == "" || snapshotRef != build.SourceSnapshotMoref {
				return fmt.Errorf(
					"%w: source snapshot changed during clone reconciliation",
					vcenter.ErrReplicaBuildAmbiguous,
				)
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.DestinationVMMoref = moref
				build.Phase = models.TemplateReplicaBuildPhaseValidating
			}); err != nil {
				return err
			}
			if err := store.EnsurePendingReplicaForBuild(ctx, build, workerID); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCloneSubmitted:
			moref, err := client.WaitReplicaBuildCloneTask(ctx, build.CloneTaskRef)
			if err != nil {
				reconciled, reconcileErr := client.FindReplicaBuildVM(ctx, retainedReplicaBuildParams(build))
				if reconcileErr != nil {
					return terminalReplicaBuildTaskError(errors.Join(err, reconcileErr))
				}
				if reconciled != "" {
					if saveErr := saveReplicaBuildState(ctx, store, build, workerID, func() {
						build.DestinationVMMoref = reconciled
						if !errors.Is(err, vcenter.ErrCloneTaskFailed) {
							build.Phase = models.TemplateReplicaBuildPhaseValidating
						}
					}); saveErr != nil {
						return errors.Join(err, saveErr)
					}
					if reserveErr := store.EnsurePendingReplicaForBuild(ctx, build, workerID); reserveErr != nil {
						return errors.Join(err, reserveErr)
					}
					if errors.Is(err, vcenter.ErrCloneTaskFailed) {
						return terminalReplicaBuildTaskError(err)
					}
					continue
				}
				return terminalReplicaBuildTaskError(err)
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.DestinationVMMoref = moref
				build.Phase = models.TemplateReplicaBuildPhaseValidating
			}); err != nil {
				return err
			}
			if err := store.EnsurePendingReplicaForBuild(ctx, build, workerID); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseValidating:
			if build.ResultReplicaID == nil {
				if err := store.EnsurePendingReplicaForBuild(ctx, build, workerID); err != nil {
					return err
				}
			}
			if err := client.ValidateReplicaBuildClone(ctx, retainedReplicaBuildParams(build)); err != nil {
				return fmt.Errorf("validate retained replica clone: %w", err)
			}
			armed := false
			taskRef, err := client.StartReplicaBuildSnapshot(
				ctx,
				build.DestinationVMMoref,
				build.ID.String(),
				build.OperationID,
				build.SourceSnapshotName,
				func(armCtx context.Context) error {
					err := saveReplicaBuildState(armCtx, store, build, workerID, func() {
						build.Phase = models.TemplateReplicaBuildPhaseSnapshotSubmitting
					})
					armed = err == nil
					return err
				},
			)
			if err != nil {
				if armed {
					return fmt.Errorf("replica build snapshot submission outcome is ambiguous: %w", err)
				}
				return err
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.SnapshotTaskRef = taskRef
				build.Phase = models.TemplateReplicaBuildPhaseSnapshotSubmitted
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseSnapshotSubmitting:
			snapshotRef, err := client.FindReplicaSnapshot(ctx, build.DestinationVMMoref, build.SourceSnapshotName)
			if err != nil {
				return err
			}
			if snapshotRef == "" {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf("%w: snapshot submission did not produce %q within %s",
						vcenter.ErrReplicaBuildAmbiguous,
						build.SourceSnapshotName,
						replicaBuildAmbiguousSubmissionDeadline)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.DestinationSnapshot = snapshotRef
				build.Phase = models.TemplateReplicaBuildPhaseCanaryPrepared
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseSnapshotSubmitted:
			snapshotRef, err := client.WaitReplicaBuildSnapshotTask(ctx, build.SnapshotTaskRef)
			if err != nil {
				reconciled, reconcileErr := client.FindReplicaSnapshot(ctx, build.DestinationVMMoref, build.SourceSnapshotName)
				if reconcileErr != nil || reconciled == "" {
					return terminalReplicaBuildTaskError(errors.Join(err, reconcileErr))
				}
				snapshotRef = reconciled
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.DestinationSnapshot = snapshotRef
				build.Phase = models.TemplateReplicaBuildPhaseCanaryPrepared
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCanaryPrepared:
			armed := false
			taskRef, err := client.StartReplicaBuildCanary(
				ctx,
				canaryReplicaBuildParams(build),
				func(armCtx context.Context) error {
					err := saveReplicaBuildState(armCtx, store, build, workerID, func() {
						build.Phase = models.TemplateReplicaBuildPhaseCanarySubmitting
					})
					armed = err == nil
					return err
				},
			)
			if err != nil {
				if armed {
					return fmt.Errorf("replica acceptance canary submission outcome is ambiguous: %w", err)
				}
				return err
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CanaryTaskRef = taskRef
				build.Phase = models.TemplateReplicaBuildPhaseCanarySubmitted
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCanarySubmitting:
			moref, err := client.FindReplicaBuildCanary(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if moref == "" {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf("%w: canary submission did not produce a marked VM within %s",
						vcenter.ErrReplicaBuildAmbiguous,
						replicaBuildAmbiguousSubmissionDeadline)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CanaryVMMoref = moref
				build.Phase = models.TemplateReplicaBuildPhaseCleanupPrepared
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCanarySubmitted:
			moref, err := client.WaitReplicaBuildCloneTask(ctx, build.CanaryTaskRef)
			if err != nil {
				reconciled, reconcileErr := client.FindReplicaBuildCanary(ctx, canaryReplicaBuildParams(build))
				if reconcileErr != nil {
					return terminalReplicaBuildTaskError(errors.Join(err, reconcileErr))
				}
				if reconciled == "" {
					return terminalReplicaBuildTaskError(err)
				}
				moref = reconciled
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CanaryVMMoref = moref
				build.Phase = models.TemplateReplicaBuildPhaseCleanupPrepared
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCleanupPrepared:
			if err := client.ValidateReplicaBuildCanary(ctx, canaryReplicaBuildParams(build)); err != nil {
				return fmt.Errorf("validate linked-clone acceptance canary: %w", err)
			}
			armed := false
			taskRef, gone, err := client.StartReplicaBuildCanaryCleanup(
				ctx,
				build.CanaryVMMoref,
				build.ID.String(),
				build.CanaryOperationID,
				func(armCtx context.Context) error {
					err := saveReplicaBuildState(armCtx, store, build, workerID, func() {
						build.Phase = models.TemplateReplicaBuildPhaseCleanupSubmitting
					})
					armed = err == nil
					return err
				},
			)
			if err != nil {
				if armed {
					return fmt.Errorf("acceptance canary cleanup submission outcome is ambiguous: %w", err)
				}
				return err
			}
			if gone {
				now := time.Now().UTC()
				if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
					build.CleanupCompletedAt = &now
					build.Phase = models.TemplateReplicaBuildPhaseFinalizing
				}); err != nil {
					return err
				}
				continue
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupTaskRef = taskRef
				build.Phase = models.TemplateReplicaBuildPhaseCleanupSubmitted
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCleanupSubmitting:
			exists, err := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if exists {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf("%w: canary cleanup did not remove the exact VM within %s",
						vcenter.ErrReplicaBuildAmbiguous,
						replicaBuildAmbiguousSubmissionDeadline)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseFinalizing
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseCleanupSubmitted:
			if err := client.WaitReplicaBuildCleanupTask(ctx, build.CleanupTaskRef); err != nil {
				exists, findErr := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
				if findErr != nil {
					return errors.Join(err, findErr)
				}
				if exists {
					if errors.Is(err, vcenter.ErrCloneTaskFailed) {
						if saveErr := saveReplicaBuildState(ctx, store, build, workerID, func() {
							build.CleanupTaskRef = ""
							build.Phase = models.TemplateReplicaBuildPhaseCleanupPrepared
						}); saveErr != nil {
							return errors.Join(err, saveErr)
						}
						return replicaBuildReconcileWait(build.Phase)
					}
					return err
				}
			}
			exists, err := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if exists {
				return errors.New("acceptance canary still exists after successful destroy task")
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseFinalizing
			}); err != nil {
				return err
			}

		case models.TemplateReplicaBuildPhaseFinalizing:
			return store.FinalizeTemplateReplicaBuild(ctx, build, workerID)

		case models.TemplateReplicaBuildPhaseReady:
			return nil

		default:
			return fmt.Errorf("unsupported template replica build phase %q", build.Phase)
		}
	}
}

func cleanupTemplateReplicaBuild(
	ctx context.Context,
	store templateReplicaBuildStore,
	client templateReplicaBuildClient,
	build *models.TemplateReplicaBuild,
	workerID string,
) error {
	if build.CanaryVMMoref == "" && build.CleanupCompletedAt == nil {
		switch build.Phase {
		case models.TemplateReplicaBuildPhaseCanarySubmitting:
			moref, err := client.FindReplicaBuildCanary(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if moref == "" {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf(
						"%w: armed canary has no persisted task or marked VM",
						vcenter.ErrReplicaBuildAmbiguous,
					)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CanaryVMMoref = moref
			}); err != nil {
				return err
			}
		case models.TemplateReplicaBuildPhaseCanarySubmitted:
			moref, err := client.FindReplicaBuildCanary(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if moref == "" && build.CanaryTaskRef != "" {
				moref, err = client.WaitReplicaBuildCloneTask(ctx, build.CanaryTaskRef)
				if err != nil && !errors.Is(err, vcenter.ErrCloneTaskFailed) {
					return err
				}
			}
			if moref != "" {
				if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
					build.CanaryVMMoref = moref
				}); err != nil {
					return err
				}
				break
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
			}); err != nil {
				return err
			}
		case models.TemplateReplicaBuildPhaseCleanupSubmitting,
			models.TemplateReplicaBuildPhaseCleanupSubmitted:
			return fmt.Errorf(
				"%w: canary cleanup phase lost its exact VM identity",
				vcenter.ErrReplicaBuildAmbiguous,
			)
		default:
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				switch build.Phase {
				case models.TemplateReplicaBuildPhaseResiduePrepared,
					models.TemplateReplicaBuildPhaseResidueSubmitting,
					models.TemplateReplicaBuildPhaseResidueSubmitted,
					models.TemplateReplicaBuildPhaseResidueCleaned:
					// Preserve retained-residue reconciliation already in progress.
				default:
					build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
				}
			}); err != nil {
				return err
			}
		}
	}
	if build.CanaryVMMoref != "" && build.CleanupCompletedAt == nil {
		switch build.Phase {
		case models.TemplateReplicaBuildPhaseCleanupSubmitting:
			exists, err := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if exists {
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf("%w: canary cleanup submission remains ambiguous", vcenter.ErrReplicaBuildAmbiguous)
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
			}); err != nil {
				return err
			}
		case models.TemplateReplicaBuildPhaseCleanupSubmitted:
			if err := client.WaitReplicaBuildCleanupTask(ctx, build.CleanupTaskRef); err != nil {
				exists, findErr := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
				if findErr != nil {
					return errors.Join(err, findErr)
				}
				if exists {
					if errors.Is(err, vcenter.ErrCloneTaskFailed) {
						if saveErr := saveReplicaBuildState(ctx, store, build, workerID, func() {
							build.CleanupTaskRef = ""
							build.Phase = models.TemplateReplicaBuildPhaseCleanupPrepared
						}); saveErr != nil {
							return errors.Join(err, saveErr)
						}
						return replicaBuildReconcileWait(build.Phase)
					}
					return err
				}
			}
			exists, err := client.ReplicaBuildCanaryExists(ctx, canaryReplicaBuildParams(build))
			if err != nil {
				return err
			}
			if exists {
				return errors.New("acceptance canary still exists after cleanup task")
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
			}); err != nil {
				return err
			}
		default:
			taskRef, gone, err := client.StartReplicaBuildCanaryCleanup(
				ctx,
				build.CanaryVMMoref,
				build.ID.String(),
				build.CanaryOperationID,
				func(armCtx context.Context) error {
					return saveReplicaBuildState(armCtx, store, build, workerID, func() {
						build.Phase = models.TemplateReplicaBuildPhaseCleanupSubmitting
					})
				},
			)
			if err != nil {
				return err
			}
			if !gone {
				if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
					build.CleanupTaskRef = taskRef
					build.Phase = models.TemplateReplicaBuildPhaseCleanupSubmitted
				}); err != nil {
					return err
				}
				return replicaBuildReconcileWait(build.Phase)
			}
			now := time.Now().UTC()
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.CleanupCompletedAt = &now
				build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
			}); err != nil {
				return err
			}
		}
	}
	if build.DestinationVMMoref == "" {
		moref, err := client.FindReplicaBuildVM(ctx, retainedReplicaBuildParams(build))
		if err != nil {
			return err
		}
		if moref != "" {
			if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
				build.DestinationVMMoref = moref
			}); err != nil {
				return err
			}
		} else {
			switch build.ResumePhase {
			case models.TemplateReplicaBuildPhasePending:
				// No vCenter submission was armed.
			case models.TemplateReplicaBuildPhaseCloneSubmitting:
				if replicaBuildSubmissionExpired(build) {
					return fmt.Errorf(
						"%w: armed clone has no persisted task or marked VM",
						vcenter.ErrReplicaBuildAmbiguous,
					)
				}
				return replicaBuildReconcileWait(build.ResumePhase)
			case models.TemplateReplicaBuildPhaseCloneSubmitted:
				if build.CloneTaskRef == "" {
					return fmt.Errorf("%w: submitted clone has no task reference", vcenter.ErrReplicaBuildAmbiguous)
				}
				taskMoref, taskErr := client.WaitReplicaBuildCloneTask(ctx, build.CloneTaskRef)
				if taskErr == nil && taskMoref != "" {
					if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
						build.DestinationVMMoref = taskMoref
					}); err != nil {
						return err
					}
				} else if !errors.Is(taskErr, vcenter.ErrCloneTaskFailed) {
					return taskErr
				}
			default:
				return fmt.Errorf(
					"%w: phase %s lost the retained replica identity",
					vcenter.ErrReplicaBuildAmbiguous,
					build.ResumePhase,
				)
			}
		}
	}
	if build.DestinationVMMoref == "" {
		return store.FailTemplateReplicaBuild(
			ctx,
			build,
			workerID,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseResidueCleaned,
			"cleanup_complete",
			"replica build cleanup completed without a retained VM",
		)
	}
	if build.Phase == models.TemplateReplicaBuildPhaseResidueSubmitting {
		exists, err := client.ReplicaBuildRetainedVMExists(ctx, retainedReplicaBuildParams(build))
		if err != nil {
			return err
		}
		if exists {
			if replicaBuildSubmissionExpired(build) {
				return fmt.Errorf(
					"%w: retained replica cleanup submission remains ambiguous",
					vcenter.ErrReplicaBuildAmbiguous,
				)
			}
			return replicaBuildReconcileWait(build.Phase)
		}
		now := time.Now().UTC()
		build.ResidueCleanedAt = &now
		return store.FailTemplateReplicaBuild(
			ctx,
			build,
			workerID,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseResidueCleaned,
			"cleanup_complete",
			"replica build residue was destroyed by operator request",
		)
	}
	if build.Phase == models.TemplateReplicaBuildPhaseResidueSubmitted {
		if err := client.WaitReplicaBuildCleanupTask(ctx, build.ResidueCleanupTaskRef); err != nil {
			exists, findErr := client.ReplicaBuildRetainedVMExists(ctx, retainedReplicaBuildParams(build))
			if findErr != nil {
				return errors.Join(err, findErr)
			}
			if exists {
				if errors.Is(err, vcenter.ErrCloneTaskFailed) {
					if saveErr := saveReplicaBuildState(ctx, store, build, workerID, func() {
						build.ResidueCleanupTaskRef = ""
						build.Phase = models.TemplateReplicaBuildPhaseResiduePrepared
					}); saveErr != nil {
						return errors.Join(err, saveErr)
					}
					return replicaBuildReconcileWait(build.Phase)
				}
				return err
			}
		}
		exists, err := client.ReplicaBuildRetainedVMExists(ctx, retainedReplicaBuildParams(build))
		if err != nil {
			return err
		}
		if exists {
			return errors.New("retained replica still exists after cleanup task")
		}
		now := time.Now().UTC()
		build.ResidueCleanedAt = &now
		return store.FailTemplateReplicaBuild(
			ctx,
			build,
			workerID,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseResidueCleaned,
			"cleanup_complete",
			"replica build residue was destroyed by operator request",
		)
	}
	taskRef, gone, err := client.StartReplicaBuildResidueCleanup(
		ctx,
		build.DestinationVMMoref,
		build.ID.String(),
		build.OperationID,
		func(armCtx context.Context) error {
			return saveReplicaBuildState(armCtx, store, build, workerID, func() {
				build.Phase = models.TemplateReplicaBuildPhaseResidueSubmitting
			})
		},
	)
	if err != nil {
		return err
	}
	if gone {
		now := time.Now().UTC()
		build.ResidueCleanedAt = &now
		return store.FailTemplateReplicaBuild(
			ctx,
			build,
			workerID,
			models.TemplateReplicaBuildFailed,
			models.TemplateReplicaBuildPhaseResidueCleaned,
			"cleanup_complete",
			"replica build residue was already absent",
		)
	}
	if err := saveReplicaBuildState(ctx, store, build, workerID, func() {
		build.ResidueCleanupTaskRef = taskRef
		build.Phase = models.TemplateReplicaBuildPhaseResidueSubmitted
	}); err != nil {
		return err
	}
	return replicaBuildReconcileWait(build.Phase)
}

func runTemplateReplicaBuild(
	ctx context.Context,
	store templateReplicaBuildStore,
	client templateReplicaBuildClient,
	job *models.Job,
	workerID string,
) error {
	var payload TemplateReplicaBuildPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template replica build payload: %w", err)
	}
	if payload.BuildID == uuid.Nil || payload.TemplateID == uuid.Nil {
		return errors.New("template replica build_id and template_id are required")
	}
	build, err := store.GetTemplateReplicaBuildForJob(ctx, job.ID, workerID)
	if err != nil {
		return err
	}
	if build.ID != payload.BuildID || build.TemplateID != payload.TemplateID {
		return errors.New("template replica build payload does not match persisted operation")
	}
	if payload.CleanupOnly {
		return cleanupTemplateReplicaBuild(ctx, store, client, build, workerID)
	}
	return executeTemplateReplicaBuild(ctx, store, client, job, workerID)
}

func (p *Provisioner) BuildTemplateSourceReplica(ctx context.Context, job *models.Job) error {
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	started := time.Now()
	err = runTemplateReplicaBuild(ctx, p.db, p.vc, job, workerID)
	if metrics, ok := p.pipeline.(interface {
		RecordTemplateReplicaBuild(string, time.Duration)
	}); ok {
		result := MetricResultSuccess
		if err != nil {
			result = MetricResultError
		}
		metrics.RecordTemplateReplicaBuild(result, time.Since(started))
	}
	if err == nil || ctx.Err() != nil ||
		errors.Is(err, database.ErrTemplateReplicaBuildLeaseLost) {
		return err
	}
	retryable, _ := ClassifyError(err, job.Type)
	if retryable && !errors.Is(err, errTemplateReplicaBuildTaskFailed) &&
		job.RetryCount < job.MaxRetries {
		return err
	}
	build, loadErr := p.db.GetTemplateReplicaBuildForJob(ctx, job.ID, workerID)
	if loadErr != nil {
		return errors.Join(err, loadErr)
	}
	status := models.TemplateReplicaBuildFailed
	phase := models.TemplateReplicaBuildPhaseFailed
	code := "validation_failed"
	if errors.Is(err, errTemplateReplicaBuildTaskFailed) {
		code = "task_failed"
	}
	if errors.Is(err, vcenter.ErrReplicaBuildAmbiguous) ||
		build.DestinationVMMoref != "" && build.ResidueCleanedAt == nil {
		status = models.TemplateReplicaBuildCleanupRequired
		phase = models.TemplateReplicaBuildPhaseCleanupRequired
		code = "manual_cleanup_required"
	}
	if failErr := p.db.FailTemplateReplicaBuild(
		context.WithoutCancel(ctx),
		build,
		workerID,
		status,
		phase,
		code,
		err.Error(),
	); failErr != nil {
		return errors.Join(err, failErr)
	}
	return err
}
