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
	ErrPodJobRequiresSerialization = errors.New("pod and VM jobs require serialized enqueue")
	ErrPodNotFound                 = errors.New("pod not found")
	ErrPodVMNotFound               = errors.New("pod VM not found")
	ErrPodJobRejected              = errors.New("pod does not accept new mutator jobs")
	ErrPodDestroyNotNeeded         = errors.New("pod destroy is no longer needed")
	ErrPodExtensionRejected        = errors.New("pod cannot be extended")
	ErrPodAlreadyDestroyed         = errors.New("pod is already destroyed")
	ErrPodDestroyJobObsolete       = errors.New("pod destroy job is not authoritative")
	ErrPodDestroyOwnershipLost     = errors.New("pod no longer owns its exact VLAN")
	ErrPodDestroyBlockedByMutator  = errors.New("pod destroy is blocked by nonterminal mutator work")
	ErrPodDestroyExclusionInvalid  = errors.New("pod destroy mutator exclusion is invalid")
)

// serializedPodJobTypes is the exhaustive set of durable jobs whose payload
// targets a pod or one of its VMs. Keep this list in lockstep with the producer
// inventory guard in internal/ci.
var serializedPodJobTypes = []string{
	models.JobTypePodCreate,
	models.JobTypePodDestroy,
	models.JobTypeVMAdd,
	models.JobTypeVMDestroy,
	models.JobTypeVMStart,
	models.JobTypeVMStop,
	models.JobTypeVMRestart,
	models.JobTypeVMReset,
	models.JobTypeVMSnapshot,
	models.JobTypeVMRevert,
	models.JobTypeVMSnapshotDelete,
	models.JobTypeVMSuspend,
}

var serializedExistingVMJobTypes = map[string]struct{}{
	models.JobTypeVMDestroy:        {},
	models.JobTypeVMStart:          {},
	models.JobTypeVMStop:           {},
	models.JobTypeVMRestart:        {},
	models.JobTypeVMReset:          {},
	models.JobTypeVMSnapshot:       {},
	models.JobTypeVMRevert:         {},
	models.JobTypeVMSnapshotDelete: {},
	models.JobTypeVMSuspend:        {},
}

type PodJobRejectedError struct {
	PodID  uuid.UUID
	Status string
	Type   string
}

type PodDestroyBlockedError struct {
	PodID     uuid.UUID
	JobID     uuid.UUID
	JobType   string
	JobStatus string
}

func (e *PodDestroyBlockedError) Error() string {
	return fmt.Sprintf(
		"pod %s destroy blocked by %s job %s in status %q: %v",
		e.PodID,
		e.JobType,
		e.JobID,
		e.JobStatus,
		ErrPodDestroyBlockedByMutator,
	)
}

func (e *PodDestroyBlockedError) Unwrap() error {
	return ErrPodDestroyBlockedByMutator
}

type PodDestroyVMJobExclusion struct {
	JobID      uuid.UUID
	PodVMID    uuid.UUID
	ClaimOwner string
}

func (e *PodJobRejectedError) Error() string {
	return fmt.Sprintf("pod %s in status %q rejects %s: %v", e.PodID, e.Status, e.Type, ErrPodJobRejected)
}

func (e *PodJobRejectedError) Unwrap() error {
	return ErrPodJobRejected
}

func isSerializedPodJobType(jobType string) bool {
	for _, protected := range serializedPodJobTypes {
		if jobType == protected {
			return true
		}
	}
	return false
}

func serializedPodMutatorTypes() []string {
	jobTypes := make([]string, 0, len(serializedPodJobTypes)-1)
	for _, jobType := range serializedPodJobTypes {
		if jobType != models.JobTypePodDestroy {
			jobTypes = append(jobTypes, jobType)
		}
	}
	return jobTypes
}

func podRejectsMutatorJob(status string) bool {
	switch status {
	case models.PodStatusDestroying,
		models.PodStatusDestroyFailed,
		models.PodStatusDestroyed,
		models.PodStatusError,
		"cancelled":
		return true
	default:
		return false
	}
}

type podJobPayloadTarget struct {
	PodID   string `json:"pod_id"`
	PodVMID string `json:"pod_vm_id"`
}

func validatePodJobPayload(payload []byte, podID uuid.UUID, podVMID *uuid.UUID) error {
	var target podJobPayloadTarget
	if err := json.Unmarshal(payload, &target); err != nil {
		return fmt.Errorf("decode serialized pod job payload: %w", err)
	}

	if target.PodID != podID.String() {
		return fmt.Errorf("serialized pod job payload pod_id %q does not match locked pod %s", target.PodID, podID)
	}
	if podVMID != nil && target.PodVMID != podVMID.String() {
		return fmt.Errorf("serialized VM job payload pod_vm_id %q does not match VM %s", target.PodVMID, *podVMID)
	}
	return nil
}

func podHasAuthoritativeDestroyJob(ctx context.Context, tx pgx.Tx, podID uuid.UUID) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM jobs
			WHERE type = $1
			  AND payload->>'pod_id' = $2
		)
	`, models.JobTypePodDestroy, podID.String()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check authoritative pod_destroy job for %s: %w", podID, err)
	}
	return exists, nil
}

// CreatePodCreateJobTx inserts the initial pod_create job in the same
// transaction that creates its pod. The newly inserted pod row is already
// exclusively owned by that transaction, but the explicit row lock and status
// check keep this producer on the same invariant as every later enqueue.
func (q *Queries) CreatePodCreateJobTx(
	ctx context.Context,
	tx pgx.Tx,
	podID uuid.UUID,
	payload []byte,
) (*models.Job, error) {
	if err := validatePodJobPayload(payload, podID, nil); err != nil {
		return nil, err
	}

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod %s for pod_create enqueue: %w", podID, ErrPodNotFound)
		}
		return nil, fmt.Errorf("lock pod %s for pod_create enqueue: %w", podID, err)
	}
	if status != models.PodStatusPending {
		return nil, &PodJobRejectedError{PodID: podID, Status: status, Type: models.JobTypePodCreate}
	}

	job, err := createJob(ctx, tx, models.JobTypePodCreate, payload)
	if err != nil {
		return nil, fmt.Errorf("insert pod_create job for %s: %w", podID, err)
	}
	return job, nil
}

// CreateVMJob resolves the VM's authoritative parent, locks that pod, and
// revalidates its lifecycle state before inserting work.
//
// Lock ordering is deliberate: workers may lock job -> pod. Producers lock
// pod -> insert a new job, but never lock an existing job, so there is no
// opposing job/pod lock cycle.
func (q *Queries) CreateVMJob(
	ctx context.Context,
	podID, podVMID uuid.UUID,
	jobType string,
	payload []byte,
) (*models.Job, error) {
	if _, ok := serializedExistingVMJobTypes[jobType]; !ok {
		return nil, fmt.Errorf("%s is not a serialized VM job type", jobType)
	}
	if err := validatePodJobPayload(payload, podID, &podVMID); err != nil {
		return nil, err
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin %s enqueue: %w", jobType, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod %s for %s enqueue: %w", podID, jobType, ErrPodNotFound)
		}
		return nil, fmt.Errorf("lock pod %s for %s enqueue: %w", podID, jobType, err)
	}
	if podRejectsMutatorJob(status) {
		return nil, &PodJobRejectedError{PodID: podID, Status: status, Type: jobType}
	}
	destroyExists, err := podHasAuthoritativeDestroyJob(ctx, tx, podID)
	if err != nil {
		return nil, err
	}
	if destroyExists {
		return nil, &PodJobRejectedError{PodID: podID, Status: status, Type: jobType}
	}

	var vmStatus string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM pod_vms
		WHERE id = $1
		  AND pod_id = $2
		FOR UPDATE
	`, podVMID, podID).Scan(&vmStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolve VM %s in pod %s for %s enqueue: %w", podVMID, podID, jobType, ErrPodVMNotFound)
		}
		return nil, fmt.Errorf("lock VM %s in pod %s for %s enqueue: %w", podVMID, podID, jobType, err)
	}
	if vmStatus == models.VMStatusDeleted {
		return nil, fmt.Errorf("VM %s in pod %s is deleted: %w", podVMID, podID, ErrPodJobRejected)
	}

	job, err := createJob(ctx, tx, jobType, payload)
	if err != nil {
		return nil, fmt.Errorf("insert %s job for VM %s in pod %s: %w", jobType, podVMID, podID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit %s job for VM %s in pod %s: %w", jobType, podVMID, podID, err)
	}
	return job, nil
}

// CreateVMAddJob creates the pending VM row and its vm_add job atomically
// under the parent pod lock. A terminal transition or queued destroy therefore
// leaves neither new work nor an orphan VM row.
func (q *Queries) CreateVMAddJob(
	ctx context.Context,
	vm *models.PodVM,
	payload []byte,
) (*models.Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin vm_add enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := q.createVMAddJobTx(ctx, tx, vm, payload)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit vm_add job for VM %s in pod %s: %w", vm.ID, vm.PodID, err)
	}
	return job, nil
}

// CreateVMAddJobTx creates the pending VM and job under the caller's
// transaction. Callers that enforce owner quotas lock the user row first, then
// this helper locks the pod so all provisioning uses user -> pod lock order.
func (q *Queries) CreateVMAddJobTx(
	ctx context.Context,
	tx pgx.Tx,
	vm *models.PodVM,
	payload []byte,
) (*models.Job, error) {
	return q.createVMAddJobTx(ctx, tx, vm, payload)
}

func (q *Queries) createVMAddJobTx(
	ctx context.Context,
	tx pgx.Tx,
	vm *models.PodVM,
	payload []byte,
) (*models.Job, error) {
	if vm.ID == uuid.Nil {
		return nil, errors.New("vm_add requires a preallocated pod VM id")
	}
	if err := validatePodJobPayload(payload, vm.PodID, &vm.ID); err != nil {
		return nil, err
	}

	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, vm.PodID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod %s for vm_add enqueue: %w", vm.PodID, ErrPodNotFound)
		}
		return nil, fmt.Errorf("lock pod %s for vm_add enqueue: %w", vm.PodID, err)
	}
	if status != models.PodStatusActive {
		return nil, &PodJobRejectedError{PodID: vm.PodID, Status: status, Type: models.JobTypeVMAdd}
	}
	destroyExists, err := podHasAuthoritativeDestroyJob(ctx, tx, vm.PodID)
	if err != nil {
		return nil, err
	}
	if destroyExists {
		return nil, &PodJobRejectedError{PodID: vm.PodID, Status: status, Type: models.JobTypeVMAdd}
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO pod_vms (
			id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at
	`, vm.ID, vm.PodID, vm.TemplateID, vm.DisplayName, vm.VCPUs, vm.RAMMB, vm.DiskGB, vm.Status).Scan(
		&vm.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert pending VM %s in pod %s: %w", vm.ID, vm.PodID, err)
	}

	job, err := createJob(ctx, tx, models.JobTypeVMAdd, payload)
	if err != nil {
		return nil, fmt.Errorf("insert vm_add job for VM %s in pod %s: %w", vm.ID, vm.PodID, err)
	}
	return job, nil
}

type podDestroyRequirement int

const (
	podDestroyAlways podDestroyRequirement = iota
	podDestroyIfExpired
	podDestroyIfEmpty
	podDestroyIfFailed
)

// CreatePodDestroyJob is the explicit user-requested/error-cleanup producer.
func (q *Queries) CreatePodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyAlways, nil)
}

// CreateExpiredPodDestroyJob revalidates expiration while holding the same pod
// lock used for insertion, so an extension that committed first wins.
func (q *Queries) CreateExpiredPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyIfExpired, nil)
}

// CreateEmptyPodDestroyJob revalidates that no non-deleted VM remains while
// holding the pod lock shared with CreateVMAddJob.
func (q *Queries) CreateEmptyPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
	exclusion *PodDestroyVMJobExclusion,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyIfEmpty, exclusion)
}

// RequeueFailedPodDestroyJob is the destroy_failed sweeper producer. It keeps
// partial cleanup on the original durable job ID and revalidates exact VLAN
// ownership before making that job claimable again.
func (q *Queries) RequeueFailedPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, queued bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyIfFailed, nil)
}

// createPodDestroyJob serializes every pod_destroy producer on the pod row.
// The first destroy job ID is authoritative forever, across every job status.
// Terminal work is restarted on that same ID only while the pod remains
// recoverable and owns its exact VLAN allocation.
func (q *Queries) createPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
	requirement podDestroyRequirement,
	exclusion *PodDestroyVMJobExclusion,
) (_ *models.Job, created bool, err error) {
	if err := validatePodJobPayload(payload, podID, nil); err != nil {
		return nil, false, err
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin pod_destroy enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var vlanTag int
	var subnet string
	var expired bool
	if err := tx.QueryRow(ctx, `
		SELECT status, vlan_id, subnet, expires_at IS NOT NULL AND expires_at < now()
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&status, &vlanTag, &subnet, &expired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("lock pod %s for pod_destroy enqueue: %w", podID, ErrPodNotFound)
		}
		return nil, false, fmt.Errorf("lock pod %s for pod_destroy enqueue: %w", podID, err)
	}

	// Do not add FOR UPDATE here. Workers lock job before pod; locking an
	// existing job while holding pod would create the inverse order. The pod
	// lock already serializes every conforming producer, and READ COMMITTED
	// gives this statement visibility into the preceding producer's commit.
	var existing models.Job
	err = tx.QueryRow(ctx, `
		SELECT id, type, payload, status, retry_count, max_retries, rollback_steps, created_at
		FROM jobs
		WHERE type = $1
		  AND payload->>'pod_id' = $2
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, models.JobTypePodDestroy, podID.String()).Scan(
		&existing.ID,
		&existing.Type,
		&existing.Payload,
		&existing.Status,
		&existing.RetryCount,
		&existing.MaxRetries,
		&existing.RollbackSteps,
		&existing.CreatedAt,
	)
	if err == nil {
		if (existing.Status == models.JobStatusCompleted || existing.Status == models.JobStatusFailed) &&
			podDestroyCanRestart(status, requirement) {
			required, requirementErr := podDestroyRequirementSatisfied(ctx, tx, podID, status, expired, requirement)
			if requirementErr != nil {
				return nil, false, requirementErr
			}
			ownsResources, ownershipErr := podOwnsExactVLAN(ctx, tx, podID, vlanTag, subnet)
			if ownershipErr != nil {
				return nil, false, ownershipErr
			}
			if required && ownsResources {
				if err := rejectPodDestroyWithNonterminalMutator(ctx, tx, podID, requirement, exclusion); err != nil {
					return nil, false, err
				}
				// This is the only pod->existing-job write. It is safe from the
				// worker job->pod order because workers can own only
				// pending/claimed/in_progress jobs, while this conditional update
				// accepts only the already-observed terminal status.
				tag, updateErr := tx.Exec(ctx, `
					UPDATE jobs
					SET status = $2,
					    claimed_by = NULL,
					    claimed_at = NULL,
					    started_at = NULL,
					    completed_at = NULL,
					    result = NULL,
					    next_attempt_at = NULL,
					    retry_count = 0
					WHERE id = $1
					  AND status = $3
				`, existing.ID, models.JobStatusPending, existing.Status)
				if updateErr != nil {
					return nil, false, fmt.Errorf("requeue authoritative pod_destroy job %s for pod %s: %w", existing.ID, podID, updateErr)
				}
				if tag.RowsAffected() != 1 {
					return nil, false, fmt.Errorf("requeue authoritative pod_destroy job %s for pod %s: terminal status changed", existing.ID, podID)
				}
				existing.Status = models.JobStatusPending
				existing.ClaimedBy = nil
				existing.ClaimedAt = nil
				existing.StartedAt = nil
				existing.CompletedAt = nil
				existing.Result = nil
				existing.NextAttemptAt = nil
				if err := tx.Commit(ctx); err != nil {
					return nil, false, fmt.Errorf("commit pod_destroy requeue for %s: %w", podID, err)
				}
				return &existing, true, nil
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("commit pod_destroy reuse for %s: %w", podID, err)
		}
		return &existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("find authoritative pod_destroy job for %s: %w", podID, err)
	}

	// Error pods remain destroyable: provisioning may have failed after
	// allocating infrastructure, and pod_destroy is their cleanup path.
	switch status {
	case models.PodStatusDestroying,
		models.PodStatusDestroyed,
		"cancelled":
		return nil, false, &PodJobRejectedError{PodID: podID, Status: status, Type: models.JobTypePodDestroy}
	case models.PodStatusDestroyFailed:
		if requirement != podDestroyIfFailed {
			return nil, false, &PodJobRejectedError{PodID: podID, Status: status, Type: models.JobTypePodDestroy}
		}
	}

	required, err := podDestroyRequirementSatisfied(ctx, tx, podID, status, expired, requirement)
	if err != nil {
		return nil, false, err
	}
	if !required {
		return nil, false, fmt.Errorf("pod %s no longer satisfies pod_destroy requirement: %w", podID, ErrPodDestroyNotNeeded)
	}
	if requirement == podDestroyIfFailed {
		ownsResources, ownershipErr := podOwnsExactVLAN(ctx, tx, podID, vlanTag, subnet)
		if ownershipErr != nil {
			return nil, false, ownershipErr
		}
		if !ownsResources {
			return nil, false, fmt.Errorf("destroy_failed pod %s no longer owns its exact VLAN: %w", podID, ErrPodDestroyNotNeeded)
		}
	}

	if err := rejectPodDestroyWithNonterminalMutator(ctx, tx, podID, requirement, exclusion); err != nil {
		return nil, false, err
	}
	job, err := createJob(ctx, tx, models.JobTypePodDestroy, payload)
	if err != nil {
		return nil, false, fmt.Errorf("insert pod_destroy job for %s: %w", podID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit pod_destroy job for %s: %w", podID, err)
	}
	return job, true, nil
}

func rejectPodDestroyWithNonterminalMutator(
	ctx context.Context,
	tx pgx.Tx,
	podID uuid.UUID,
	requirement podDestroyRequirement,
	exclusion *PodDestroyVMJobExclusion,
) error {
	var excludedJobID uuid.UUID
	if exclusion != nil {
		if requirement != podDestroyIfEmpty {
			return fmt.Errorf("pod %s exclusion is only valid for empty-pod cleanup: %w", podID, ErrPodDestroyExclusionInvalid)
		}
		var valid bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM jobs j
				JOIN pod_vms vm
				  ON vm.id = $2
				 AND vm.pod_id = $4
				  AND vm.status = $7
				WHERE j.id = $1
				  AND j.type = $5
				  AND j.status = $6
				  AND j.claimed_by = $3
				  AND j.payload->>'pod_id' = $4::text
				  AND j.payload->>'pod_vm_id' = $2::text
			)
		`,
			exclusion.JobID,
			exclusion.PodVMID,
			exclusion.ClaimOwner,
			podID,
			models.JobTypeVMDestroy,
			models.JobStatusInProgress,
			models.VMStatusDeleted,
		).Scan(&valid); err != nil {
			return fmt.Errorf("verify current vm_destroy exclusion for pod %s: %w", podID, err)
		}
		if !valid {
			return fmt.Errorf(
				"job %s is not the owned in_progress vm_destroy for deleted VM %s in pod %s: %w",
				exclusion.JobID,
				exclusion.PodVMID,
				podID,
				ErrPodDestroyExclusionInvalid,
			)
		}
		excludedJobID = exclusion.JobID
	}

	var blocker PodDestroyBlockedError
	err := tx.QueryRow(ctx, `
		SELECT j.id, j.type, j.status
		FROM jobs j
		WHERE j.type = ANY($1)
		  AND j.status = ANY($2)
		  AND ($3::uuid = $4::uuid OR j.id != $3::uuid)
		  AND (
		    j.payload->>'pod_id' = $5
		    OR j.payload->>'pod_vm_id' IN (
		      SELECT id::text
		      FROM pod_vms
		      WHERE pod_id = $6
		    )
		  )
		ORDER BY j.created_at ASC, j.id ASC
		LIMIT 1
	`,
		serializedPodMutatorTypes(),
		[]string{models.JobStatusPending, models.JobStatusClaimed, models.JobStatusInProgress},
		excludedJobID,
		uuid.Nil,
		podID.String(),
		podID,
	).Scan(&blocker.JobID, &blocker.JobType, &blocker.JobStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan nonterminal mutator work for pod %s: %w", podID, err)
	}
	blocker.PodID = podID
	return &blocker
}

func podDestroyCanRestart(status string, requirement podDestroyRequirement) bool {
	switch status {
	case models.PodStatusPending,
		models.PodStatusProvisioning,
		models.PodStatusActive,
		models.PodStatusError,
		models.PodStatusDestroying:
		return true
	case models.PodStatusDestroyFailed:
		return requirement == podDestroyIfFailed
	default:
		return false
	}
}

func podDestroyRequirementSatisfied(
	ctx context.Context,
	tx pgx.Tx,
	podID uuid.UUID,
	status string,
	expired bool,
	requirement podDestroyRequirement,
) (bool, error) {
	switch requirement {
	case podDestroyAlways:
		return true, nil
	case podDestroyIfExpired:
		return status == models.PodStatusActive && expired, nil
	case podDestroyIfEmpty:
		var empty bool
		if err := tx.QueryRow(ctx, `
			SELECT NOT EXISTS (
				SELECT 1
				FROM pod_vms
				WHERE pod_id = $1
				  AND status != $2
			)
		`, podID, models.VMStatusDeleted).Scan(&empty); err != nil {
			return false, fmt.Errorf("revalidate empty pod %s before pod_destroy enqueue: %w", podID, err)
		}
		return empty, nil
	case podDestroyIfFailed:
		return status == models.PodStatusDestroyFailed, nil
	default:
		return false, fmt.Errorf("unknown pod_destroy requirement %d", requirement)
	}
}

func podOwnsExactVLAN(
	ctx context.Context,
	tx pgx.Tx,
	podID uuid.UUID,
	vlanTag int,
	subnet string,
) (bool, error) {
	var owns bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM vlan_pool
			WHERE pod_id = $1
			  AND vlan_tag = $2
			  AND subnet = $3
		)
	`, podID, vlanTag, subnet).Scan(&owns); err != nil {
		return false, fmt.Errorf("verify exact VLAN ownership for pod %s: %w", podID, err)
	}
	return owns, nil
}

// PreparePodDestroy is the worker-entry half of the serialization contract.
// The worker logically owns the job first, then this transaction locks the pod,
// verifies the authoritative job ID and exact VLAN ownership, and transitions
// the pod to destroying before any infrastructure side effect.
func (q *Queries) PreparePodDestroy(
	ctx context.Context,
	podID, jobID uuid.UUID,
	claimOwner string,
) (*models.Pod, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pod destroy preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockOwnedPodDestroyJob(ctx, tx, podID, jobID, claimOwner); err != nil {
		return nil, err
	}

	pod := &models.Pod{ID: podID}
	if err := tx.QueryRow(ctx, `
		SELECT status, vlan_id, subnet
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&pod.Status, &pod.VLANID, &pod.Subnet); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod %s for destroy preparation: %w", podID, ErrPodNotFound)
		}
		return nil, fmt.Errorf("lock pod %s for destroy preparation: %w", podID, err)
	}
	if pod.Status == models.PodStatusDestroyed {
		return nil, fmt.Errorf("pod %s: %w", podID, ErrPodAlreadyDestroyed)
	}

	var authoritativeID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM jobs
		WHERE type = $1
		  AND payload->>'pod_id' = $2
		ORDER BY created_at ASC, id ASC
		LIMIT 1
	`, models.JobTypePodDestroy, podID.String()).Scan(&authoritativeID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("pod %s has no authoritative destroy job: %w", podID, ErrPodDestroyJobObsolete)
		}
		return nil, fmt.Errorf("find authoritative destroy job for pod %s: %w", podID, err)
	}
	if authoritativeID != jobID {
		return nil, fmt.Errorf("pod %s authoritative destroy job is %s, not %s: %w", podID, authoritativeID, jobID, ErrPodDestroyJobObsolete)
	}

	ownsResources, err := podOwnsExactVLAN(ctx, tx, podID, pod.VLANID, pod.Subnet)
	if err != nil {
		return nil, err
	}
	if !ownsResources {
		return nil, fmt.Errorf("pod %s VLAN %d/%s: %w", podID, pod.VLANID, pod.Subnet, ErrPodDestroyOwnershipLost)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE pods
		SET status = $2,
		    error_message = '',
		    updated_at = now()
		WHERE id = $1
	`, podID, models.PodStatusDestroying); err != nil {
		return nil, fmt.Errorf("mark pod %s destroying before cleanup: %w", podID, err)
	}
	pod.Status = models.PodStatusDestroying
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pod destroy preparation for %s: %w", podID, err)
	}
	return pod, nil
}

func lockOwnedPodDestroyJob(
	ctx context.Context,
	tx pgx.Tx,
	podID, jobID uuid.UUID,
	claimOwner string,
) error {
	var lockedID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM jobs
		WHERE id = $1
		  AND type = $2
		  AND payload->>'pod_id' = $3
		  AND status = $4
		  AND claimed_by = $5
		FOR UPDATE
	`, jobID, models.JobTypePodDestroy, podID.String(), models.JobStatusInProgress, claimOwner).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pod_destroy job %s is not owned in_progress by %s: %w", jobID, claimOwner, ErrJobLeaseLost)
		}
		return fmt.Errorf("lock owned pod_destroy job %s: %w", jobID, err)
	}
	return nil
}

// ExtendPod atomically locks the pod, rejects existing destroy intent, records
// the attestation, and updates expiration. It shares the pod serialization row
// with expiration enqueue, so exactly one operation wins.
func (q *Queries) ExtendPod(
	ctx context.Context,
	podID, userID uuid.UUID,
	newExpiry time.Time,
) (*models.PodAttestation, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin pod extension: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var status string
	var previousExpiry *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT status, expires_at
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&status, &previousExpiry); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("lock pod %s for extension: %w", podID, ErrPodNotFound)
		}
		return nil, fmt.Errorf("lock pod %s for extension: %w", podID, err)
	}
	if status != models.PodStatusActive {
		return nil, fmt.Errorf("pod %s has status %q: %w", podID, status, ErrPodExtensionRejected)
	}

	destroyExists, err := podHasAuthoritativeDestroyJob(ctx, tx, podID)
	if err != nil {
		return nil, err
	}
	if destroyExists {
		return nil, fmt.Errorf("pod %s has authoritative destroy intent: %w", podID, ErrPodExtensionRejected)
	}

	attestation := &models.PodAttestation{
		PodID:             podID,
		UserID:            userID,
		PreviousExpiresAt: previousExpiry,
		NewExpiresAt:      newExpiry,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO pod_attestations (pod_id, user_id, previous_expires_at, new_expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, podID, userID, previousExpiry, newExpiry).Scan(&attestation.ID, &attestation.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert pod extension attestation for %s: %w", podID, err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE pods
		SET expires_at = $2,
		    updated_at = now()
		WHERE id = $1
		  AND status = $3
	`, podID, newExpiry, models.PodStatusActive); err != nil {
		return nil, fmt.Errorf("update pod %s expiration: %w", podID, err)
	} else if tag.RowsAffected() != 1 {
		return nil, fmt.Errorf("update pod %s expiration: %w", podID, ErrPodExtensionRejected)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pod %s extension: %w", podID, err)
	}
	return attestation, nil
}
