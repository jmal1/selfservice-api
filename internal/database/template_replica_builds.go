package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmal1/selfservice-api/internal/models"
)

var (
	ErrTemplateReplicaBuildConflict    = errors.New("template replica build conflicts with an existing operation")
	ErrTemplateReplicaBuildLeaseLost   = errors.New("template replica build worker lease lost")
	ErrTemplateReplicaBuildJobObsolete = errors.New("template replica build job is no longer linked to its operation")
)

func lockTemplateReplicaBuildJob(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
	workerID string,
) error {
	var one int
	err := tx.QueryRow(ctx, `
		SELECT 1
		FROM jobs
		WHERE id = $1 AND claimed_by = $2
		  AND status IN ('claimed', 'in_progress')
		FOR UPDATE
	`, jobID, workerID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTemplateReplicaBuildLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock template replica build job: %w", err)
	}
	return nil
}

func scanTemplateReplicaBuild(row pgx.Row, build *models.TemplateReplicaBuild) error {
	return row.Scan(
		&build.ID,
		&build.TemplateID,
		&build.SourceReplicaID,
		&build.ResultReplicaID,
		&build.JobID,
		&build.IdempotencyKey,
		&build.OperationID,
		&build.CanaryOperationID,
		&build.SourceVMMoref,
		&build.SourceSnapshotName,
		&build.SourceSnapshotMoref,
		&build.DestinationName,
		&build.ComputeResourceType,
		&build.ComputeResourceMoref,
		&build.ComputeResourcePath,
		&build.HostMoref,
		&build.HostName,
		&build.ResourcePoolMoref,
		&build.ResourcePoolPath,
		&build.DatastoreMoref,
		&build.DatastoreName,
		&build.FolderMoref,
		&build.FolderPath,
		&build.ProvisionDatastore,
		&build.Status,
		&build.Phase,
		&build.ResumePhase,
		&build.CloneTaskRef,
		&build.DestinationVMMoref,
		&build.SnapshotTaskRef,
		&build.DestinationSnapshot,
		&build.CanaryTaskRef,
		&build.CanaryVMMoref,
		&build.CleanupTaskRef,
		&build.CleanupCompletedAt,
		&build.ResidueCleanupTaskRef,
		&build.ResidueCleanedAt,
		&build.LastErrorCode,
		&build.LastError,
		&build.StartedAt,
		&build.SubmissionStartedAt,
		&build.CompletedAt,
		&build.CreatedAt,
		&build.UpdatedAt,
	)
}

const templateReplicaBuildColumns = `
	id, template_id, source_replica_id, result_replica_id, job_id,
	idempotency_key, operation_id, canary_operation_id, source_vm_moref,
	source_snapshot_name, source_snapshot_moref, destination_name,
	compute_resource_type, compute_resource_moref, compute_resource_path,
	host_moref, host_name, resource_pool_moref, resource_pool_path,
	datastore_moref, datastore_name, folder_moref, folder_path,
	provision_datastore, status, phase, resume_phase, clone_task_ref,
	destination_vm_moref, snapshot_task_ref, destination_snapshot_moref,
	canary_task_ref, canary_vm_moref, cleanup_task_ref,
	cleanup_completed_at, residue_cleanup_task_ref, residue_cleaned_at,
	last_error_code, last_error, started_at, submission_started_at,
	completed_at, created_at, updated_at
`

func sameTemplateReplicaBuildRequest(a, b *models.TemplateReplicaBuild) bool {
	return a.TemplateID == b.TemplateID &&
		sameUUIDPointer(a.SourceReplicaID, b.SourceReplicaID) &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.SourceVMMoref == b.SourceVMMoref &&
		a.SourceSnapshotName == b.SourceSnapshotName &&
		a.DestinationName == b.DestinationName &&
		a.ComputeResourceType == b.ComputeResourceType &&
		a.ComputeResourceMoref == b.ComputeResourceMoref &&
		a.ComputeResourcePath == b.ComputeResourcePath &&
		a.HostMoref == b.HostMoref &&
		a.HostName == b.HostName &&
		a.ResourcePoolMoref == b.ResourcePoolMoref &&
		a.ResourcePoolPath == b.ResourcePoolPath &&
		a.DatastoreMoref == b.DatastoreMoref &&
		a.DatastoreName == b.DatastoreName &&
		a.FolderMoref == b.FolderMoref &&
		a.FolderPath == b.FolderPath &&
		a.ProvisionDatastore == b.ProvisionDatastore
}

func sameUUIDPointer(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (q *Queries) GetTemplateReplicaBuildByIdempotencyKey(
	ctx context.Context,
	templateID uuid.UUID,
	key string,
) (*models.TemplateReplicaBuild, error) {
	var build models.TemplateReplicaBuild
	err := scanTemplateReplicaBuild(q.pool.QueryRow(ctx, `
		SELECT `+templateReplicaBuildColumns+`
		FROM template_source_replica_builds
		WHERE template_id = $1 AND idempotency_key = $2
	`, templateID, key), &build)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get template replica build by idempotency key: %w", err)
	}
	return &build, nil
}

func (q *Queries) HasUnsafeTemplateReplicaBuildForDeletion(
	ctx context.Context,
	templateID uuid.UUID,
) (bool, error) {
	var unsafe bool
	if err := q.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM template_source_replica_builds
			WHERE template_id = $1
			  AND (
				status IN ('pending', 'running', 'cleanup_required', 'ready', 'retiring')
				OR (destination_vm_moref <> '' AND residue_cleaned_at IS NULL)
				OR result_replica_id IS NOT NULL
			  )
		)
	`, templateID).Scan(&unsafe); err != nil {
		return false, fmt.Errorf("check unsafe template replica builds: %w", err)
	}
	return unsafe, nil
}

// CreateTemplateReplicaBuild inserts the immutable operation and its first job
// in one transaction. A reused idempotency key returns the original operation
// only when every immutable input is identical.
func (q *Queries) CreateTemplateReplicaBuild(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	userID uuid.UUID,
) (*models.TemplateReplicaBuild, bool, error) {
	if build == nil || build.TemplateID == uuid.Nil || build.SourceReplicaID == nil ||
		*build.SourceReplicaID == uuid.Nil ||
		build.IdempotencyKey == "" || build.OperationID == "" ||
		build.CanaryOperationID == "" {
		return nil, false, errors.New("template replica build identity is incomplete")
	}
	if build.ID == uuid.Nil {
		build.ID = uuid.New()
	}
	if build.Status == "" {
		build.Status = models.TemplateReplicaBuildPending
	}
	if build.Phase == "" {
		build.Phase = models.TemplateReplicaBuildPhasePending
	}
	if build.ResumePhase == "" {
		build.ResumePhase = models.TemplateReplicaBuildPhasePending
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, templateReplicaBuildLifecycleLockKey(build.TemplateID)); err != nil {
		return nil, false, fmt.Errorf("lock template replica build admission: %w", err)
	}

	var existing models.TemplateReplicaBuild
	err = scanTemplateReplicaBuild(tx.QueryRow(ctx, `
		SELECT `+templateReplicaBuildColumns+`
		FROM template_source_replica_builds
		WHERE template_id = $1 AND idempotency_key = $2
	`, build.TemplateID, build.IdempotencyKey), &existing)
	if err == nil {
		if !sameTemplateReplicaBuildRequest(&existing, build) {
			return nil, false, ErrTemplateReplicaBuildConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return &existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("load idempotent replica build: %w", err)
	}

	var anchorStatus, anchorSource string
	if err := tx.QueryRow(ctx, `
		SELECT status, source_vm_moref
		FROM template_source_replicas
		WHERE id = $1 AND template_id = $2
		FOR SHARE
	`, *build.SourceReplicaID, build.TemplateID).Scan(&anchorStatus, &anchorSource); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrTemplateNotFound
		}
		return nil, false, fmt.Errorf("lock source replica anchor: %w", err)
	}
	if anchorStatus != models.TemplateSourceReplicaReady || anchorSource != build.SourceVMMoref {
		return nil, false, fmt.Errorf(
			"%w: source replica anchor is not ready or no longer matches %s",
			ErrTemplateReplicaBuildConflict,
			build.SourceVMMoref,
		)
	}
	var occupied bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM template_source_replicas
			WHERE template_id = $1
			  AND compute_resource_type = $2
			  AND compute_resource_moref = $3
		)
	`, build.TemplateID, build.ComputeResourceType, build.ComputeResourceMoref).Scan(&occupied); err != nil {
		return nil, false, fmt.Errorf("check replica build destination occupancy: %w", err)
	}
	if occupied {
		return nil, false, fmt.Errorf(
			"%w: destination compute resource already has a source replica",
			ErrTemplateReplicaBuildConflict,
		)
	}

	var insertedID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO template_source_replica_builds (
			id, template_id, source_replica_id, idempotency_key, operation_id,
			canary_operation_id, source_vm_moref, source_snapshot_name,
			destination_name, compute_resource_type, compute_resource_moref,
			compute_resource_path, host_moref, host_name, resource_pool_moref,
			resource_pool_path, datastore_moref, datastore_name, folder_moref,
			folder_path, provision_datastore, status, phase
		)
		VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19, $20, $21, $22, $23
		)
		ON CONFLICT (template_id, idempotency_key) DO NOTHING
		RETURNING id
	`, build.ID, build.TemplateID, *build.SourceReplicaID, build.IdempotencyKey,
		build.OperationID, build.CanaryOperationID, build.SourceVMMoref,
		build.SourceSnapshotName, build.DestinationName, build.ComputeResourceType,
		build.ComputeResourceMoref, build.ComputeResourcePath, build.HostMoref,
		build.HostName, build.ResourcePoolMoref, build.ResourcePoolPath,
		build.DatastoreMoref, build.DatastoreName, build.FolderMoref,
		build.FolderPath, build.ProvisionDatastore, build.Status, build.Phase,
	).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existing models.TemplateReplicaBuild
		if err := scanTemplateReplicaBuild(tx.QueryRow(ctx, `
			SELECT `+templateReplicaBuildColumns+`
			FROM template_source_replica_builds
			WHERE template_id = $1 AND idempotency_key = $2
		`, build.TemplateID, build.IdempotencyKey), &existing); err != nil {
			return nil, false, fmt.Errorf("load idempotent replica build: %w", err)
		}
		if !sameTemplateReplicaBuildRequest(&existing, build) {
			return nil, false, ErrTemplateReplicaBuildConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, err
		}
		return &existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("insert template replica build: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"build_id":    build.ID,
		"template_id": build.TemplateID,
		"user_id":     userID,
	})
	if err != nil {
		return nil, false, err
	}
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO jobs (type, payload, max_retries)
		VALUES ($1, $2, 20)
		RETURNING id
	`, models.JobTypeTemplateReplicaBuild, payload).Scan(&jobID); err != nil {
		return nil, false, fmt.Errorf("enqueue template replica build: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE template_source_replica_builds SET job_id = $2 WHERE id = $1
	`, build.ID, jobID); err != nil {
		return nil, false, fmt.Errorf("attach template replica build job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	build.JobID = &jobID
	return build, true, nil
}

func (q *Queries) GetTemplateReplicaBuild(
	ctx context.Context,
	templateID, buildID uuid.UUID,
) (*models.TemplateReplicaBuild, error) {
	var build models.TemplateReplicaBuild
	err := scanTemplateReplicaBuild(q.pool.QueryRow(ctx, `
		SELECT `+templateReplicaBuildColumns+`
		FROM template_source_replica_builds
		WHERE id = $1 AND template_id = $2
	`, buildID, templateID), &build)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get template replica build: %w", err)
	}
	return &build, nil
}

func (q *Queries) GetTemplateReplicaBuildForJob(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
) (*models.TemplateReplicaBuild, error) {
	var build models.TemplateReplicaBuild
	err := scanTemplateReplicaBuild(q.pool.QueryRow(ctx, `
		SELECT `+templateReplicaBuildColumns+`
		FROM template_source_replica_builds b
		WHERE b.job_id = $1
		  AND EXISTS (
			SELECT 1 FROM jobs j
			WHERE j.id = b.job_id
			  AND j.claimed_by = $2
			  AND j.status IN ('claimed', 'in_progress')
		  )
	`, jobID, workerID), &build)
	if errors.Is(err, pgx.ErrNoRows) {
		var stillOwned bool
		if ownershipErr := q.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM jobs
				WHERE id = $1
				  AND claimed_by = $2
				  AND status IN ('claimed', 'in_progress')
			)
		`, jobID, workerID).Scan(&stillOwned); ownershipErr != nil {
			return nil, fmt.Errorf("check obsolete template replica build job: %w", ownershipErr)
		}
		if stillOwned {
			return nil, fmt.Errorf("%w: job %s", ErrTemplateReplicaBuildJobObsolete, jobID)
		}
		return nil, fmt.Errorf("%w: job %s", ErrTemplateReplicaBuildLeaseLost, jobID)
	}
	if err != nil {
		return nil, fmt.Errorf("load template replica build job: %w", err)
	}
	return &build, nil
}

// SaveTemplateReplicaBuildState is a claim-fenced compare-and-swap. The caller
// must supply the phase it read so a superseded worker cannot overwrite a newer
// reconciliation result.
func (q *Queries) SaveTemplateReplicaBuildState(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	expectedPhase, workerID string,
) error {
	if build == nil || build.JobID == nil {
		return errors.New("template replica build has no job identity")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTemplateReplicaBuildJob(ctx, tx, *build.JobID, workerID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE template_source_replica_builds b
		SET status = $4,
		    phase = $5,
		    resume_phase = $6,
		    source_snapshot_moref = $7,
		    clone_task_ref = $8,
		    destination_vm_moref = $9,
		    snapshot_task_ref = $10,
		    destination_snapshot_moref = $11,
		    canary_task_ref = $12,
		    canary_vm_moref = $13,
		    cleanup_task_ref = $14,
		    cleanup_completed_at = $15,
		    residue_cleanup_task_ref = $16,
		    residue_cleaned_at = $17,
		    last_error_code = $18,
		    last_error = $19,
		    started_at = $20,
		    submission_started_at = $21,
		    completed_at = $22
		WHERE b.id = $1
		  AND b.job_id = $2
		  AND b.phase = $3
	`, build.ID, *build.JobID, expectedPhase, build.Status, build.Phase,
		build.ResumePhase, build.SourceSnapshotMoref, build.CloneTaskRef, build.DestinationVMMoref,
		build.SnapshotTaskRef, build.DestinationSnapshot, build.CanaryTaskRef,
		build.CanaryVMMoref, build.CleanupTaskRef, build.CleanupCompletedAt,
		build.ResidueCleanupTaskRef, build.ResidueCleanedAt, build.LastErrorCode,
		build.LastError, build.StartedAt, build.SubmissionStartedAt,
		build.CompletedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: build %s phase %s", ErrTemplateReplicaBuildLeaseLost, build.ID, expectedPhase)
	}
	return tx.Commit(ctx)
}

func (q *Queries) EnsurePendingReplicaForBuild(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	workerID string,
) error {
	if build == nil || build.JobID == nil || build.DestinationVMMoref == "" {
		return errors.New("pending replica build identity is incomplete")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockTemplateReplicaBuildJob(ctx, tx, *build.JobID, workerID); err != nil {
		return err
	}
	if build.ResultReplicaID == nil {
		replicaID := uuid.New()
		var actualID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO template_source_replicas (
				id, template_id, source_vm_moref, compute_resource_type,
				compute_resource_moref, compute_resource_path, status
			)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')
			RETURNING id
		`, replicaID, build.TemplateID, build.DestinationVMMoref,
			build.ComputeResourceType, build.ComputeResourceMoref,
			build.ComputeResourcePath).Scan(&actualID)
		if err != nil {
			return fmt.Errorf("reserve pending source replica: %w", err)
		}
		var sourceMoref string
		if err := tx.QueryRow(ctx, `
			SELECT source_vm_moref FROM template_source_replicas WHERE id = $1
		`, actualID).Scan(&sourceMoref); err != nil {
			return err
		}
		if sourceMoref != build.DestinationVMMoref {
			return ErrTemplateReplicaBuildConflict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE template_source_replica_builds
			SET result_replica_id = $2
			WHERE id = $1 AND result_replica_id IS NULL
		`, build.ID, actualID); err != nil {
			return err
		}
		build.ResultReplicaID = &actualID
	}
	return tx.Commit(ctx)
}

// FinalizeTemplateReplicaBuild atomically rechecks the ready source anchor,
// marks the accepted destination replica ready, and closes the build. No
// readiness is visible before exact canary cleanup has been persisted.
func (q *Queries) FinalizeTemplateReplicaBuild(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	workerID string,
) error {
	if build == nil || build.JobID == nil || build.SourceReplicaID == nil ||
		build.ResultReplicaID == nil ||
		build.CleanupCompletedAt == nil || build.DestinationSnapshot == "" {
		return errors.New("template replica build is not acceptance-complete")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTemplateReplicaBuildJob(ctx, tx, *build.JobID, workerID); err != nil {
		return err
	}

	var anchorStatus, anchorRef string
	if err := tx.QueryRow(ctx, `
		SELECT status, source_vm_moref
		FROM template_source_replicas
		WHERE id = $1 AND template_id = $2
		FOR SHARE
	`, *build.SourceReplicaID, build.TemplateID).Scan(&anchorStatus, &anchorRef); err != nil {
		return fmt.Errorf("recheck source anchor: %w", err)
	}
	if anchorStatus != models.TemplateSourceReplicaReady || anchorRef != build.SourceVMMoref {
		return fmt.Errorf("%w: source anchor drifted before finalization", ErrTemplateReplicaBuildConflict)
	}

	now := time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE template_source_replicas r
		SET status = 'ready', last_validated_at = $4, last_validation_error = NULL
		WHERE r.id = $1 AND r.template_id = $2
		  AND r.source_vm_moref = $3
		  AND r.status = 'pending'
	`, *build.ResultReplicaID, build.TemplateID, build.DestinationVMMoref,
		now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTemplateReplicaBuildLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO template_source_replica_policies (template_id)
		VALUES ($1)
		ON CONFLICT (template_id) DO NOTHING
	`, build.TemplateID); err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `
		UPDATE template_source_replica_builds
		SET status = 'ready', phase = 'ready', completed_at = $3,
		    last_error_code = '', last_error = ''
		WHERE id = $1 AND job_id = $2 AND phase = 'finalizing'
	`, build.ID, *build.JobID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTemplateReplicaBuildLeaseLost
	}
	return tx.Commit(ctx)
}

func (q *Queries) FailTemplateReplicaBuild(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	workerID string,
	status, phase, code, message string,
) error {
	if build == nil || build.JobID == nil {
		return errors.New("template replica build has no job identity")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTemplateReplicaBuildJob(ctx, tx, *build.JobID, workerID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE template_source_replicas r
		SET status = 'unhealthy', last_validated_at = now(),
		    last_validation_error = $3
		FROM template_source_replica_builds b
		WHERE b.id = $1 AND b.job_id = $2 AND r.id = b.result_replica_id
		  AND b.status <> 'retiring'
		  AND r.status <> 'ready'
	`, build.ID, *build.JobID, message); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE template_source_replica_builds
		SET status = $2, phase = $3, last_error_code = $4,
		    resume_phase = CASE
		        WHEN status IN ('pending', 'running')
		         AND $7 IN (
		            'pending', 'clone_submitting', 'clone_submitted', 'validating',
		            'snapshot_submitting', 'snapshot_submitted', 'canary_prepared',
		            'canary_submitting', 'canary_submitted', 'cleanup_prepared',
		            'cleanup_submitting', 'cleanup_submitted', 'finalizing'
		         )
		        THEN $7
		        ELSE resume_phase
		    END,
		    last_error = $5, completed_at = now()
		WHERE id = $1 AND job_id = $6 AND status NOT IN ('ready', 'retired')
	`, build.ID, status, phase, code, message, *build.JobID, build.Phase)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTemplateReplicaBuildLeaseLost
	}
	return tx.Commit(ctx)
}

// CompleteTemplateReplicaBuildCleanup atomically persists exact vCenter
// absence proof and releases only this build's non-ready replica reservation.
func (q *Queries) CompleteTemplateReplicaBuildCleanup(
	ctx context.Context,
	build *models.TemplateReplicaBuild,
	workerID, message string,
) error {
	if build == nil || build.JobID == nil || build.ResidueCleanedAt == nil {
		return errors.New("template replica cleanup proof is incomplete")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, templateReplicaBuildLifecycleLockKey(build.TemplateID)); err != nil {
		return fmt.Errorf("lock template replica cleanup completion: %w", err)
	}
	if err := lockTemplateReplicaBuildJob(ctx, tx, *build.JobID, workerID); err != nil {
		return err
	}

	var (
		resultReplicaID *uuid.UUID
		persistedStatus string
	)
	if err := tx.QueryRow(ctx, `
		SELECT result_replica_id, status
		FROM template_source_replica_builds
		WHERE id = $1 AND job_id = $2
		FOR UPDATE
	`, build.ID, *build.JobID).Scan(&resultReplicaID, &persistedStatus); err != nil {
		return fmt.Errorf("lock replica build cleanup result: %w", err)
	}
	retiring := persistedStatus == models.TemplateReplicaBuildRetiring
	if retiring && resultReplicaID == nil {
		return fmt.Errorf("%w: retiring build has no result replica", ErrTemplateReplicaBuildConflict)
	}
	if resultReplicaID != nil {
		var resultStatus string
		if err := tx.QueryRow(ctx, `
			SELECT status
			FROM template_source_replicas
			WHERE id = $1
			  AND template_id = $2
			  AND source_vm_moref = $3
			  AND compute_resource_type = $4
			  AND compute_resource_moref = $5
			FOR UPDATE
		`, *resultReplicaID, build.TemplateID, build.DestinationVMMoref,
			build.ComputeResourceType, build.ComputeResourceMoref).Scan(&resultStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf(
					"%w: cleanup result replica identity does not match the persisted build",
					ErrTemplateReplicaBuildConflict,
				)
			}
			return fmt.Errorf("lock cleanup result replica: %w", err)
		}
		if retiring {
			if resultStatus != models.TemplateSourceReplicaDisabled {
				return fmt.Errorf(
					"%w: retiring result replica %s has status %q",
					ErrTemplateReplicaBuildConflict,
					*resultReplicaID,
					resultStatus,
				)
			}
			if build.SourceReplicaID == nil || *build.SourceReplicaID == *resultReplicaID {
				return fmt.Errorf("%w: retirement has no distinct source anchor", ErrTemplateReplicaBuildConflict)
			}
			var anchorReady bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM template_source_replicas
					WHERE id = $1
					  AND template_id = $2
					  AND source_vm_moref = $3
					  AND status = 'ready'
				)
			`, *build.SourceReplicaID, build.TemplateID, build.SourceVMMoref).Scan(&anchorReady); err != nil {
				return err
			}
			if !anchorReady {
				return fmt.Errorf("%w: retirement would leave no compatible ready source anchor", ErrTemplateReplicaBuildConflict)
			}
			var referenced bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM vm_placements WHERE source_replica_id = $1
				)
			`, *resultReplicaID).Scan(&referenced); err != nil {
				return err
			}
			if referenced {
				return fmt.Errorf("%w: retiring result replica is referenced by a VM placement", ErrTemplateReplicaBuildConflict)
			}
			var dependentBuildID uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT id
				FROM template_source_replica_builds
				WHERE id <> $1
				  AND source_replica_id = $2
				  AND NOT (
					status = 'retired'
					OR (
						status = 'failed'
						AND result_replica_id IS NULL
						AND (destination_vm_moref = '' OR residue_cleaned_at IS NOT NULL)
					)
				  )
				LIMIT 1
				FOR UPDATE
			`, build.ID, *resultReplicaID).Scan(&dependentBuildID)
			if err == nil {
				return fmt.Errorf(
					"%w: retiring result replica is the source anchor for build %s",
					ErrTemplateReplicaBuildConflict,
					dependentBuildID,
				)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE template_source_replica_builds
				SET source_replica_id = NULL
				WHERE id <> $1
				  AND source_replica_id = $2
				  AND (
					status = 'retired'
					OR (
						status = 'failed'
						AND result_replica_id IS NULL
						AND (destination_vm_moref = '' OR residue_cleaned_at IS NOT NULL)
					)
				  )
			`, build.ID, *resultReplicaID); err != nil {
				return fmt.Errorf("release terminal downstream build source references: %w", err)
			}
		} else if resultStatus == models.TemplateSourceReplicaReady {
			return fmt.Errorf(
				"%w: non-retirement cleanup cannot remove ready result replica %s",
				ErrTemplateReplicaBuildConflict,
				*resultReplicaID,
			)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE template_source_replica_builds
			SET result_replica_id = NULL
			WHERE id = $1 AND job_id = $2 AND result_replica_id = $3
		`, build.ID, *build.JobID, *resultReplicaID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrTemplateReplicaBuildLeaseLost
		}
		tag, err = tx.Exec(ctx, `
			DELETE FROM template_source_replicas
			WHERE id = $1
			  AND template_id = $2
			  AND source_vm_moref = $3
			  AND compute_resource_type = $4
			  AND compute_resource_moref = $5
			  AND status = $6
		`, *resultReplicaID, build.TemplateID, build.DestinationVMMoref,
			build.ComputeResourceType, build.ComputeResourceMoref, resultStatus)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf(
				"%w: cleanup result replica %s no longer matches immutable build identity",
				ErrTemplateReplicaBuildConflict,
				*resultReplicaID,
			)
		}
	}

	status := models.TemplateReplicaBuildFailed
	phase := models.TemplateReplicaBuildPhaseResidueCleaned
	code := "cleanup_complete"
	if retiring {
		status = models.TemplateReplicaBuildRetired
		phase = models.TemplateReplicaBuildPhaseRetired
		code = "retirement_complete"
	}
	tag, err := tx.Exec(ctx, `
		UPDATE template_source_replica_builds
		SET status = $3,
		    phase = $4,
		    result_replica_id = NULL,
		    residue_cleaned_at = $5,
		    last_error_code = $6,
		    last_error = $7,
		    completed_at = now()
		WHERE id = $1 AND job_id = $2
		  AND status = $8
	`, build.ID, *build.JobID, status, phase, build.ResidueCleanedAt,
		code, message, persistedStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTemplateReplicaBuildLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	build.ResultReplicaID = nil
	build.Status = status
	build.Phase = phase
	build.LastErrorCode = code
	build.LastError = message
	return nil
}

func (q *Queries) RestartTemplateReplicaBuild(
	ctx context.Context,
	templateID, buildID uuid.UUID,
	cleanupOnly bool,
) (*models.TemplateReplicaBuild, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
	`, templateReplicaBuildLifecycleLockKey(templateID)); err != nil {
		return nil, fmt.Errorf("lock template replica build restart: %w", err)
	}
	var observedJobID *uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT job_id
		FROM template_source_replica_builds
		WHERE id = $1 AND template_id = $2
	`, buildID, templateID).Scan(&observedJobID); err != nil {
		return nil, err
	}
	var currentJobStatus string
	if observedJobID != nil {
		if err := tx.QueryRow(ctx, `
			SELECT status FROM jobs WHERE id = $1 FOR UPDATE
		`, *observedJobID).Scan(&currentJobStatus); err != nil {
			return nil, fmt.Errorf("lock current replica build job: %w", err)
		}
	}
	var build models.TemplateReplicaBuild
	if err := scanTemplateReplicaBuild(tx.QueryRow(ctx, `
		SELECT `+templateReplicaBuildColumns+`
		FROM template_source_replica_builds
		WHERE id = $1 AND template_id = $2
		FOR UPDATE
	`, buildID, templateID), &build); err != nil {
		return nil, err
	}
	if !sameUUIDPointer(build.JobID, observedJobID) {
		return nil, fmt.Errorf(
			"%w: linked job changed during restart",
			ErrTemplateReplicaBuildConflict,
		)
	}
	if build.SourceReplicaID == nil {
		return nil, fmt.Errorf(
			"%w: terminal build source reference has been released",
			ErrTemplateReplicaBuildConflict,
		)
	}
	if !cleanupOnly {
		var sourceID uuid.UUID
		if err := tx.QueryRow(ctx, `
			SELECT id
			FROM template_source_replicas
			WHERE id = $1
			  AND template_id = $2
			  AND source_vm_moref = $3
			  AND status = 'ready'
			FOR SHARE
		`, *build.SourceReplicaID, build.TemplateID, build.SourceVMMoref).Scan(&sourceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf(
					"%w: source replica is no longer ready",
					ErrTemplateReplicaBuildConflict,
				)
			}
			return nil, fmt.Errorf("lock restart source replica: %w", err)
		}
	}
	if build.JobID != nil {
		switch currentJobStatus {
		case models.JobStatusPending, models.JobStatusClaimed,
			models.JobStatusInProgress, models.JobStatusRollback:
			return nil, fmt.Errorf(
				"%w: linked job %s is still %s",
				ErrTemplateReplicaBuildConflict,
				*build.JobID,
				currentJobStatus,
			)
		}
	}

	retireReady := cleanupOnly &&
		(build.Status == models.TemplateReplicaBuildReady ||
			build.Status == models.TemplateReplicaBuildRetiring)
	if retireReady {
		if build.SourceReplicaID == nil || build.ResultReplicaID == nil ||
			*build.SourceReplicaID == *build.ResultReplicaID ||
			build.DestinationVMMoref == "" {
			return nil, fmt.Errorf("%w: ready build retirement identity is incomplete", ErrTemplateReplicaBuildConflict)
		}
		var anchorReady bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM template_source_replicas
				WHERE id = $1
				  AND template_id = $2
				  AND source_vm_moref = $3
				  AND status = 'ready'
			)
		`, *build.SourceReplicaID, build.TemplateID, build.SourceVMMoref).Scan(&anchorReady); err != nil {
			return nil, err
		}
		if !anchorReady {
			return nil, fmt.Errorf(
				"%w: retirement would leave no compatible ready source anchor",
				ErrTemplateReplicaBuildConflict,
			)
		}
		var resultStatus string
		if err := tx.QueryRow(ctx, `
			SELECT status
			FROM template_source_replicas
			WHERE id = $1
			  AND template_id = $2
			  AND source_vm_moref = $3
			  AND compute_resource_type = $4
			  AND compute_resource_moref = $5
			FOR UPDATE
		`, *build.ResultReplicaID, build.TemplateID, build.DestinationVMMoref,
			build.ComputeResourceType, build.ComputeResourceMoref).Scan(&resultStatus); err != nil {
			return nil, fmt.Errorf("lock ready retirement result: %w", err)
		}
		expectedResultStatus := models.TemplateSourceReplicaReady
		if build.Status == models.TemplateReplicaBuildRetiring {
			expectedResultStatus = models.TemplateSourceReplicaDisabled
		}
		if resultStatus != expectedResultStatus {
			return nil, fmt.Errorf(
				"%w: retirement result replica status is %q",
				ErrTemplateReplicaBuildConflict,
				resultStatus,
			)
		}
		var referenced bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM vm_placements WHERE source_replica_id = $1
			)
		`, *build.ResultReplicaID).Scan(&referenced); err != nil {
			return nil, err
		}
		if referenced {
			return nil, fmt.Errorf(
				"%w: ready result replica is referenced by a VM placement",
				ErrTemplateReplicaBuildConflict,
			)
		}
		var dependentBuildID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id
			FROM template_source_replica_builds
			WHERE id <> $1
			  AND source_replica_id = $2
			  AND NOT (
				status = 'retired'
				OR (
					status = 'failed'
					AND result_replica_id IS NULL
					AND (destination_vm_moref = '' OR residue_cleaned_at IS NOT NULL)
				)
			  )
			LIMIT 1
			FOR UPDATE
		`, build.ID, *build.ResultReplicaID).Scan(&dependentBuildID)
		if err == nil {
			return nil, fmt.Errorf(
				"%w: ready result replica is the source anchor for build %s",
				ErrTemplateReplicaBuildConflict,
				dependentBuildID,
			)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if build.Status == models.TemplateReplicaBuildReady {
			tag, err := tx.Exec(ctx, `
				UPDATE template_source_replicas
				SET status = 'disabled',
				    last_validation_error = 'retirement requested'
				WHERE id = $1 AND status = 'ready'
			`, *build.ResultReplicaID)
			if err != nil {
				return nil, err
			}
			if tag.RowsAffected() != 1 {
				return nil, ErrTemplateReplicaBuildConflict
			}
		}
	} else {
		if build.Status != models.TemplateReplicaBuildFailed &&
			build.Status != models.TemplateReplicaBuildCleanupRequired {
			return nil, ErrTemplateReplicaBuildConflict
		}
		if !models.IsTemplateReplicaBuildForwardPhase(build.ResumePhase) {
			return nil, fmt.Errorf(
				"%w: invalid forward resume phase %q",
				ErrTemplateReplicaBuildConflict,
				build.ResumePhase,
			)
		}
	}
	if !cleanupOnly && build.Status == models.TemplateReplicaBuildCleanupRequired {
		return nil, ErrTemplateReplicaBuildConflict
	}
	if !cleanupOnly && build.LastErrorCode == "task_failed" {
		return nil, ErrTemplateReplicaBuildConflict
	}
	if !cleanupOnly && build.ResidueCleanedAt != nil {
		return nil, ErrTemplateReplicaBuildConflict
	}
	payload, err := json.Marshal(map[string]any{
		"build_id":     build.ID,
		"template_id":  build.TemplateID,
		"cleanup_only": cleanupOnly,
		"retire_ready": retireReady,
	})
	if err != nil {
		return nil, err
	}
	var jobID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO jobs (type, payload, max_retries)
		VALUES ($1, $2, 20)
		RETURNING id
	`, models.JobTypeTemplateReplicaBuild, payload).Scan(&jobID); err != nil {
		return nil, err
	}
	status := models.TemplateReplicaBuildRunning
	phase := build.ResumePhase
	if cleanupOnly {
		status = models.TemplateReplicaBuildCleanupRequired
		phase = build.ResumePhase
	}
	errorCode := ""
	if retireReady {
		status = models.TemplateReplicaBuildRetiring
		phase = models.TemplateReplicaBuildPhaseResiduePrepared
		build.ResidueCleanupTaskRef = ""
		build.ResidueCleanedAt = nil
		errorCode = "retirement_requested"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE template_source_replica_builds
		SET job_id = $2, status = $3, phase = $4, completed_at = NULL,
		    residue_cleanup_task_ref = $5, residue_cleaned_at = $6,
		    last_error_code = $7, last_error = ''
		WHERE id = $1
	`, build.ID, jobID, status, phase, build.ResidueCleanupTaskRef,
		build.ResidueCleanedAt, errorCode); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	build.JobID = &jobID
	build.Status = status
	build.Phase = phase
	build.CompletedAt = nil
	build.LastErrorCode = errorCode
	build.LastError = ""
	return &build, nil
}

func (q *Queries) CountTemplateReplicaBuildsByPhase(
	ctx context.Context,
	staleAfter time.Duration,
) (map[string]int, int, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT phase, COUNT(*)
		FROM template_source_replica_builds
		WHERE status IN ('pending', 'running', 'cleanup_required', 'retiring')
		GROUP BY phase
	`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var phase string
		var count int
		if err := rows.Scan(&phase, &count); err != nil {
			return nil, 0, err
		}
		counts[phase] = count
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var stuck int
	if err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM template_source_replica_builds
		WHERE status IN ('pending', 'running', 'cleanup_required', 'retiring')
		  AND updated_at < now() - ($1 * interval '1 second')
	`, staleAfter.Seconds()).Scan(&stuck); err != nil {
		return nil, 0, err
	}
	return counts, stuck, nil
}
