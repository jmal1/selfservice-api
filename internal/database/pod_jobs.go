package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	if vm.ID == uuid.Nil {
		return nil, errors.New("vm_add requires a preallocated pod VM id")
	}
	if err := validatePodJobPayload(payload, vm.PodID, &vm.ID); err != nil {
		return nil, err
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin vm_add enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit vm_add job for VM %s in pod %s: %w", vm.ID, vm.PodID, err)
	}
	return job, nil
}

type podDestroyRequirement int

const (
	podDestroyAlways podDestroyRequirement = iota
	podDestroyIfExpired
	podDestroyIfEmpty
)

// CreatePodDestroyJob is the explicit user-requested/error-cleanup producer.
func (q *Queries) CreatePodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyAlways)
}

// CreateExpiredPodDestroyJob revalidates expiration while holding the same pod
// lock used for insertion, so an extension that committed first wins.
func (q *Queries) CreateExpiredPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyIfExpired)
}

// CreateEmptyPodDestroyJob revalidates that no non-deleted VM remains while
// holding the pod lock shared with CreateVMAddJob.
func (q *Queries) CreateEmptyPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
) (_ *models.Job, created bool, err error) {
	return q.createPodDestroyJob(ctx, podID, payload, podDestroyIfEmpty)
}

// createPodDestroyJob serializes every pod_destroy producer on the pod row.
// The first destroy job is authoritative forever, across every job status.
// Reusing even completed or failed work prevents a delayed producer from
// launching a second cleanup against a VLAN that may already be reassigned.
func (q *Queries) createPodDestroyJob(
	ctx context.Context,
	podID uuid.UUID,
	payload []byte,
	requirement podDestroyRequirement,
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
	var expired bool
	if err := tx.QueryRow(ctx, `
		SELECT status, expires_at IS NOT NULL AND expires_at < now()
		FROM pods
		WHERE id = $1
		FOR UPDATE
	`, podID).Scan(&status, &expired); err != nil {
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
		models.PodStatusDestroyFailed,
		models.PodStatusDestroyed,
		"cancelled":
		return nil, false, &PodJobRejectedError{PodID: podID, Status: status, Type: models.JobTypePodDestroy}
	}

	switch requirement {
	case podDestroyIfExpired:
		if status != models.PodStatusActive || !expired {
			return nil, false, fmt.Errorf("pod %s is not currently expired: %w", podID, ErrPodDestroyNotNeeded)
		}
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
			return nil, false, fmt.Errorf("revalidate empty pod %s before pod_destroy enqueue: %w", podID, err)
		}
		if !empty {
			return nil, false, fmt.Errorf("pod %s still has active VMs: %w", podID, ErrPodDestroyNotNeeded)
		}
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
