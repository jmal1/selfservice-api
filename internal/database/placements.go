package database

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmal1/selfservice-api/internal/models"
)

func scanTemplateSourceReplica(row pgx.Row, replica *models.TemplateSourceReplica) error {
	return row.Scan(
		&replica.ID,
		&replica.TemplateID,
		&replica.SourceVMMoref,
		&replica.ComputeResourceType,
		&replica.ComputeResourceMoref,
		&replica.ComputeResourcePath,
		&replica.Status,
		&replica.LastValidatedAt,
		&replica.LastValidationError,
		&replica.CreatedAt,
		&replica.UpdatedAt,
	)
}

// ListTemplateSourceReplicas returns every configured source for a logical
// template. Callers must not fall back to the legacy source when this returns
// any rows but none are ready.
func (q *Queries) ListTemplateSourceReplicas(
	ctx context.Context,
	templateID uuid.UUID,
) ([]models.TemplateSourceReplica, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, template_id, source_vm_moref, compute_resource_type,
		       compute_resource_moref, compute_resource_path, status,
		       last_validated_at, last_validation_error, created_at, updated_at
		FROM template_source_replicas
		WHERE template_id = $1
		ORDER BY compute_resource_path, compute_resource_moref
	`, templateID)
	if err != nil {
		return nil, fmt.Errorf("list template source replicas: %w", err)
	}
	defer rows.Close()

	replicas := make([]models.TemplateSourceReplica, 0)
	for rows.Next() {
		var replica models.TemplateSourceReplica
		if err := scanTemplateSourceReplica(rows, &replica); err != nil {
			return nil, err
		}
		replicas = append(replicas, replica)
	}
	return replicas, rows.Err()
}

func (q *Queries) CreateTemplateSourceReplica(
	ctx context.Context,
	replica *models.TemplateSourceReplica,
) error {
	if replica == nil || replica.TemplateID == uuid.Nil ||
		replica.SourceVMMoref == "" ||
		replica.ComputeResourceType == "" ||
		replica.ComputeResourceMoref == "" {
		return errors.New("template source replica identity is incomplete")
	}
	if replica.ID == uuid.Nil {
		replica.ID = uuid.New()
	}
	if replica.Status == "" {
		replica.Status = models.TemplateSourceReplicaPending
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO template_source_replica_policies (template_id)
		VALUES ($1)
		ON CONFLICT (template_id) DO NOTHING
	`, replica.TemplateID); err != nil {
		return fmt.Errorf("enable template source replica policy: %w", err)
	}
	if err := scanTemplateSourceReplica(tx.QueryRow(ctx, `
		INSERT INTO template_source_replicas (
			id, template_id, source_vm_moref, compute_resource_type,
			compute_resource_moref, compute_resource_path, status,
			last_validated_at, last_validation_error
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, template_id, source_vm_moref, compute_resource_type,
		          compute_resource_moref, compute_resource_path, status,
		          last_validated_at, last_validation_error, created_at, updated_at
	`, replica.ID, replica.TemplateID, replica.SourceVMMoref,
		replica.ComputeResourceType, replica.ComputeResourceMoref,
		replica.ComputeResourcePath, replica.Status, replica.LastValidatedAt,
		replica.LastValidationError), replica); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TemplateSourceReplicaModeEnabled is monotonic for a template. Deleting all
// replica rows must never silently restore the legacy single-source path.
func (q *Queries) TemplateSourceReplicaModeEnabled(
	ctx context.Context,
	templateID uuid.UUID,
) (bool, error) {
	var enabled bool
	if err := q.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM template_source_replica_policies WHERE template_id = $1
		)
	`, templateID).Scan(&enabled); err != nil {
		return false, fmt.Errorf("read template source replica policy: %w", err)
	}
	return enabled, nil
}

func (q *Queries) DeleteTemplateSourceReplica(
	ctx context.Context,
	templateID, replicaID uuid.UUID,
) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		DELETE FROM template_source_replicas
		WHERE id = $1 AND template_id = $2
	`, replicaID, templateID)
	if err != nil {
		return false, fmt.Errorf("delete template source replica: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (q *Queries) UpdateTemplateSourceReplicaValidation(
	ctx context.Context,
	templateID, replicaID uuid.UUID,
	status string,
	validatedAt *time.Time,
	validationError *string,
) (bool, error) {
	switch status {
	case models.TemplateSourceReplicaPending, models.TemplateSourceReplicaReady,
		models.TemplateSourceReplicaUnhealthy, models.TemplateSourceReplicaDisabled:
	default:
		return false, fmt.Errorf("invalid template source replica status %q", status)
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE template_source_replicas
		SET status = $3,
		    last_validated_at = $4,
		    last_validation_error = $5
		WHERE id = $1 AND template_id = $2
	`, replicaID, templateID, status, validatedAt, validationError)
	if err != nil {
		return false, fmt.Errorf("update template source replica validation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func scanVMPlacement(row pgx.Row, placement *models.VMPlacement) error {
	return row.Scan(
		&placement.PodVMID,
		&placement.JobID,
		&placement.TemplateID,
		&placement.SourceReplicaID,
		&placement.SourceRef,
		&placement.ComputeResourceType,
		&placement.ComputeResourceMoref,
		&placement.ResourcePoolMoref,
		&placement.HostMoref,
		&placement.HostName,
		&placement.DRSControl,
		&placement.ObservedFreeMemoryMB,
		&placement.ReservedMemoryMB,
		&placement.CreatedAt,
	)
}

const vmPlacementSelectCols = `pod_vm_id, job_id, template_id, source_replica_id,
	source_ref, compute_resource_type, compute_resource_moref, resource_pool_moref,
	host_moref, host_name, drs_control, observed_free_memory_mb, reserved_memory_mb,
	created_at`

func queryVMPlacements(
	ctx context.Context,
	tx pgx.Tx,
	jobID uuid.UUID,
) ([]models.VMPlacement, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+vmPlacementSelectCols+`
		FROM vm_placements
		WHERE job_id = $1
		ORDER BY pod_vm_id
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var placements []models.VMPlacement
	for rows.Next() {
		var placement models.VMPlacement
		if err := scanVMPlacement(rows, &placement); err != nil {
			return nil, err
		}
		placements = append(placements, placement)
	}
	return placements, rows.Err()
}

func validateVMPlacementCandidate(placement models.VMPlacement, jobID uuid.UUID) error {
	if placement.PodVMID == uuid.Nil || placement.TemplateID == uuid.Nil {
		return errors.New("VM placement requires pod_vm_id and template_id")
	}
	if placement.JobID != uuid.Nil && placement.JobID != jobID {
		return fmt.Errorf("VM placement job_id %s does not match job %s", placement.JobID, jobID)
	}
	if placement.SourceRef == "" ||
		placement.ComputeResourceType == "" ||
		placement.ComputeResourceMoref == "" ||
		placement.ResourcePoolMoref == "" ||
		placement.HostMoref == "" ||
		placement.HostName == "" {
		return errors.New("VM placement contains an incomplete source or destination identity")
	}
	switch placement.DRSControl {
	case models.VMPlacementDRSDisabled, models.VMPlacementStandalone:
	default:
		return fmt.Errorf("VM placement has invalid DRS control %q", placement.DRSControl)
	}
	if placement.ObservedFreeMemoryMB < 0 || placement.ReservedMemoryMB < 0 {
		return errors.New("VM placement memory observations cannot be negative")
	}
	return nil
}

func samePlacementScope(existing, candidate models.VMPlacement) bool {
	return existing.PodVMID == candidate.PodVMID &&
		existing.TemplateID == candidate.TemplateID
}

// LoadVMPlacementPlan returns a complete prior plan for the requested pod VMs.
// It returns nil when no placement has been persisted and fails closed on a
// partial plan.
func (q *Queries) LoadVMPlacementPlan(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	podVMIDs []uuid.UUID,
) ([]models.VMPlacement, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockPlacementJob(ctx, tx, jobID, workerID); err != nil {
		return nil, err
	}
	existing, err := queryVMPlacements(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, tx.Commit(ctx)
	}
	if err := requirePlacementSet(existing, podVMIDs); err != nil {
		return nil, err
	}
	return existing, tx.Commit(ctx)
}

// PrepareVMPlacementPlan atomically records all source and destination
// identities for a provisioning job. Existing plans win so retries never make
// a new placement decision.
func (q *Queries) PrepareVMPlacementPlan(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	candidates []models.VMPlacement,
) ([]models.VMPlacement, error) {
	if len(candidates) == 0 {
		return nil, errors.New("VM placement plan is empty")
	}
	podVMIDs := make([]uuid.UUID, 0, len(candidates))
	seen := make(map[uuid.UUID]struct{}, len(candidates))
	for i := range candidates {
		if err := validateVMPlacementCandidate(candidates[i], jobID); err != nil {
			return nil, fmt.Errorf("validate VM placement %d: %w", i, err)
		}
		if _, duplicate := seen[candidates[i].PodVMID]; duplicate {
			return nil, fmt.Errorf("duplicate VM placement for %s", candidates[i].PodVMID)
		}
		seen[candidates[i].PodVMID] = struct{}{}
		podVMIDs = append(podVMIDs, candidates[i].PodVMID)
		candidates[i].JobID = jobID
	}

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockPlacementJob(ctx, tx, jobID, workerID); err != nil {
		return nil, err
	}

	existing, err := queryVMPlacements(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		if err := requirePlacementSet(existing, podVMIDs); err != nil {
			return nil, err
		}
		byVM := make(map[uuid.UUID]models.VMPlacement, len(candidates))
		for _, candidate := range candidates {
			byVM[candidate.PodVMID] = candidate
		}
		for _, placement := range existing {
			if !samePlacementScope(placement, byVM[placement.PodVMID]) {
				return nil, fmt.Errorf("persisted placement scope changed for pod VM %s", placement.PodVMID)
			}
		}
		return existing, tx.Commit(ctx)
	}

	for _, placement := range candidates {
		var templateMatches bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pod_vms
				WHERE id = $1 AND template_id = $2
			)
		`, placement.PodVMID, placement.TemplateID).Scan(&templateMatches); err != nil {
			return nil, err
		}
		if !templateMatches {
			return nil, fmt.Errorf(
				"pod VM %s is not linked to template %s",
				placement.PodVMID,
				placement.TemplateID,
			)
		}

		if placement.SourceReplicaID != nil {
			var replicaMatches bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM template_source_replicas
					WHERE id = $1
					  AND template_id = $2
					  AND source_vm_moref = $3
					  AND compute_resource_type = $4
					  AND compute_resource_moref = $5
					  AND status = 'ready'
				)
			`, *placement.SourceReplicaID, placement.TemplateID, placement.SourceRef,
				placement.ComputeResourceType, placement.ComputeResourceMoref).Scan(&replicaMatches); err != nil {
				return nil, err
			}
			if !replicaMatches {
				return nil, fmt.Errorf(
					"source replica %s is not a ready immutable source for template %s",
					*placement.SourceReplicaID,
					placement.TemplateID,
				)
			}
		} else {
			var replicaModeEnabled bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM template_source_replica_policies WHERE template_id = $1
				)
			`, placement.TemplateID).Scan(&replicaModeEnabled); err != nil {
				return nil, err
			}
			if replicaModeEnabled {
				return nil, fmt.Errorf(
					"legacy source fallback is prohibited for template %s because source-replica mode is enabled",
					placement.TemplateID,
				)
			}
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO vm_placements (
				pod_vm_id, job_id, template_id, source_replica_id, source_ref,
				compute_resource_type, compute_resource_moref, resource_pool_moref,
				host_moref, host_name, drs_control, observed_free_memory_mb,
				reserved_memory_mb
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		`, placement.PodVMID, jobID, placement.TemplateID, placement.SourceReplicaID,
			placement.SourceRef, placement.ComputeResourceType,
			placement.ComputeResourceMoref, placement.ResourcePoolMoref,
			placement.HostMoref, placement.HostName, placement.DRSControl,
			placement.ObservedFreeMemoryMB, placement.ReservedMemoryMB); err != nil {
			return nil, fmt.Errorf("persist VM placement for %s: %w", placement.PodVMID, err)
		}
	}

	persisted, err := queryVMPlacements(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	if err := requirePlacementSet(persisted, podVMIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit VM placement plan: %w", err)
	}
	return persisted, nil
}

func lockPlacementJob(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, workerID string) error {
	var locked bool
	err := tx.QueryRow(ctx, `
		SELECT true
		FROM jobs
		WHERE id = $1
		  AND claimed_by = $2
		  AND type IN ('pod_create', 'vm_add')
		  AND status IN ('claimed', 'in_progress')
		FOR UPDATE
	`, jobID, workerID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: job %s cannot persist VM placement for %s", ErrJobLeaseLost, jobID, workerID)
	}
	if err != nil {
		return fmt.Errorf("lock VM placement job: %w", err)
	}
	return nil
}

func requirePlacementSet(placements []models.VMPlacement, podVMIDs []uuid.UUID) error {
	want := make([]string, 0, len(podVMIDs))
	for _, id := range podVMIDs {
		want = append(want, id.String())
	}
	got := make([]string, 0, len(placements))
	for _, placement := range placements {
		got = append(got, placement.PodVMID.String())
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(want) != len(got) {
		return fmt.Errorf("persisted VM placement plan is partial: got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("persisted VM placement plan targets %v, want %v", got, want)
		}
	}
	return nil
}

func (q *Queries) GetVMPlacement(ctx context.Context, podVMID uuid.UUID) (*models.VMPlacement, error) {
	var placement models.VMPlacement
	err := scanVMPlacement(q.pool.QueryRow(ctx, `
		SELECT `+vmPlacementSelectCols+`
		FROM vm_placements
		WHERE pod_vm_id = $1
	`, podVMID), &placement)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get VM placement: %w", err)
	}
	return &placement, nil
}
