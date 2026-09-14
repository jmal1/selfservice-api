package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// --- Testing / Run Queries ---

// GetTestingTargetsForPod returns each reachable pod VM with the playlists
// offered against it. Same-template twins each appear once so the student can
// choose which machine to grade. Playlists use the two-level model per VM:
// blueprint_vm_playlists for slots whose blueprint_vms.template_id matches the
// VM, else template_playlists defaults for that template.
//
// Reachability matches grading: skip deleted/error rows and rows with no IP.
func (q *Queries) GetTestingTargetsForPod(ctx context.Context, podID uuid.UUID) ([]models.TestingTarget, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, COALESCE(pv.display_name, ''), COALESCE(pv.ip_address, ''),
		       pv.template_id, COALESCE(t.name, ''), pv.status
		FROM pod_vms pv
		JOIN templates t ON t.id = pv.template_id
		WHERE pv.pod_id = $1
		  AND pv.status NOT IN ('deleted', 'error')
		  AND COALESCE(pv.ip_address, '') <> ''
		ORDER BY CASE WHEN pv.status = 'running' THEN 0 ELSE 1 END,
		         pv.boot_order ASC, pv.created_at ASC, pv.id ASC
	`, podID)
	if err != nil {
		return nil, fmt.Errorf("list testing target VMs: %w", err)
	}
	defer rows.Close()

	var targets []models.TestingTarget
	for rows.Next() {
		var t models.TestingTarget
		if err := rows.Scan(&t.PodVMID, &t.DisplayName, &t.IPAddress,
			&t.TemplateID, &t.TemplateName, &t.Status); err != nil {
			return nil, fmt.Errorf("scan testing target VM: %w", err)
		}
		playlists, err := q.GetPlaylistsForPodVM(ctx, podID, t.PodVMID)
		if err != nil {
			return nil, err
		}
		if playlists == nil {
			playlists = []models.Playlist{}
		}
		t.Playlists = playlists
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if targets == nil {
		targets = []models.TestingTarget{}
	}
	return targets, nil
}

// GetPlaylistsForPodVM resolves playlists offered for one pod VM using the
// two-level model scoped to that VM's template:
// 1. blueprint_vm_playlists for slots whose blueprint_vms row uses this template
// 2. else template_playlists defaults for the VM's template_id
//
// The override join keys blueprint_vm_playlists.vm_slot (INT, migration 000013)
// against blueprint_vms.boot_order (INT, migration 000010).
func (q *Queries) GetPlaylistsForPodVM(ctx context.Context, podID, podVMID uuid.UUID) ([]models.Playlist, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, name, slug, description, scoring_mode, created_by,
		       is_active, created_at, updated_at
		FROM (
			SELECT DISTINCT ON (p.id)
			       p.id, p.name, p.slug, p.description, p.scoring_mode, p.created_by,
			       p.is_active, p.created_at, p.updated_at, bvp.execution_order
			FROM playlists p
			JOIN blueprint_vm_playlists bvp ON p.id = bvp.playlist_id
			JOIN pods pod ON pod.id = $1
			JOIN pod_vms pv ON pv.id = $2 AND pv.pod_id = pod.id
			JOIN blueprint_vms bv ON bv.blueprint_id = pod.blueprint_id
			                     AND bv.boot_order = bvp.vm_slot
			                     AND bv.template_id = pv.template_id
			WHERE bvp.blueprint_id = pod.blueprint_id
			  AND p.is_active = true
			ORDER BY p.id, bvp.execution_order
		) overrides
		ORDER BY execution_order, name
	`, podID, podVMID)
	if err != nil {
		return nil, fmt.Errorf("get blueprint playlists for VM: %w", err)
	}
	defer rows.Close()

	var playlists []models.Playlist
	for rows.Next() {
		var p models.Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.ScoringMode,
			&p.CreatedBy, &p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan playlist: %w", err)
		}
		playlists = append(playlists, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(playlists) > 0 {
		return playlists, nil
	}

	rows2, err := q.pool.Query(ctx, `
		SELECT p.id, p.name, p.slug, p.description, p.scoring_mode, p.created_by,
		       p.is_active, p.created_at, p.updated_at
		FROM playlists p
		JOIN template_playlists tp ON p.id = tp.playlist_id
		JOIN pod_vms pv ON pv.id = $2 AND pv.template_id = tp.template_id
		WHERE pv.pod_id = $1
		  AND p.is_active = true
		ORDER BY tp.execution_order, p.name
	`, podID, podVMID)
	if err != nil {
		return nil, fmt.Errorf("get template playlists for VM: %w", err)
	}
	defer rows2.Close()

	for rows2.Next() {
		var p models.Playlist
		if err := rows2.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.ScoringMode,
			&p.CreatedBy, &p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan template playlist: %w", err)
		}
		playlists = append(playlists, p)
	}
	return playlists, rows2.Err()
}

// GetRunnablePodVMTarget loads identity for a pod VM that is eligible to be
// graded (belongs to the pod, not deleted/error, has an IP).
func (q *Queries) GetRunnablePodVMTarget(ctx context.Context, podID, podVMID uuid.UUID) (models.TestingTarget, error) {
	var t models.TestingTarget
	err := q.pool.QueryRow(ctx, `
		SELECT pv.id, COALESCE(pv.display_name, ''), COALESCE(pv.ip_address, ''),
		       pv.template_id, COALESCE(tmpl.name, ''), pv.status
		FROM pod_vms pv
		JOIN templates tmpl ON tmpl.id = pv.template_id
		WHERE pv.id = $2
		  AND pv.pod_id = $1
		  AND pv.status NOT IN ('deleted', 'error')
		  AND COALESCE(pv.ip_address, '') <> ''
	`, podID, podVMID).Scan(&t.PodVMID, &t.DisplayName, &t.IPAddress,
		&t.TemplateID, &t.TemplateName, &t.Status)
	if err != nil {
		return t, err
	}
	return t, nil
}

// PlaylistOfferedForPodVM reports whether playlistID is in the offer set for
// the given pod VM (same rules as GetPlaylistsForPodVM).
func (q *Queries) PlaylistOfferedForPodVM(ctx context.Context, podID, podVMID, playlistID uuid.UUID) (bool, error) {
	playlists, err := q.GetPlaylistsForPodVM(ctx, podID, podVMID)
	if err != nil {
		return false, err
	}
	for _, p := range playlists {
		if p.ID == playlistID {
			return true, nil
		}
	}
	return false, nil
}

// HasActiveRun checks if a pod has a run in pending/provisioning/running status.
func (q *Queries) HasActiveRun(ctx context.Context, podID uuid.UUID) (bool, error) {
	var count int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE pod_id = $1 AND status IN ('pending', 'provisioning', 'running')
	`, podID).Scan(&count)
	return count > 0, err
}

// CountRecentRuns counts runs triggered by a user on a pod within a time window.
func (q *Queries) CountRecentRuns(ctx context.Context, podID, userID uuid.UUID, window time.Duration) (int, error) {
	var count int
	cutoff := time.Now().Add(-window)
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE pod_id = $1 AND triggered_by = $2 AND created_at > $3
	`, podID, userID, cutoff).Scan(&count)
	return count, err
}

// CreateRun inserts a new run record, including the chosen target VM so
// attribution exists before the engine finishes (and so the engine grades the
// VM the student selected, not an ambiguous primary).
func (q *Queries) CreateRun(ctx context.Context, run *models.Run) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO runs (
			pod_id, playlist_id, triggered_by, callback_token, status,
			target_pod_vm_id, target_vm_name, target_vm_ip
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at
	`, run.PodID, run.PlaylistID, run.TriggeredBy, run.CallbackToken, run.Status,
		run.TargetPodVMID, run.TargetVMName, run.TargetVMIP,
	).Scan(&run.ID, &run.CreatedAt, &run.UpdatedAt)
}

const runAttributionSelect = `
		SELECT r.id, r.pod_id, r.playlist_id, r.triggered_by, r.status,
		       r.target_pod_vm_id, r.target_vm_name, r.target_vm_ip,
		       COALESCE(u.username, '') AS triggered_by_username,
		       COALESCE(u.display_name, '') AS triggered_by_display_name,
		       p.owner_id,
		       COALESCE(po.username, '') AS pod_owner_username,
		       COALESCE(po.display_name, '') AS pod_owner_display_name,
		       COALESCE(p.name, '') AS pod_name,
		       COALESCE(p.status, '') AS pod_status,
		       COALESCE(pl.name, '') AS playlist_name,
		       r.total_workflows, r.passed_workflows, r.failed_workflows,
		       r.error_message, r.started_at, r.completed_at, r.created_at, r.updated_at
		FROM runs r
		LEFT JOIN users u ON r.triggered_by = u.id
		LEFT JOIN pods p ON r.pod_id = p.id
		LEFT JOIN users po ON p.owner_id = po.id
		LEFT JOIN playlists pl ON r.playlist_id = pl.id
`

const listAllRunsQuery = runAttributionSelect + `
		ORDER BY r.created_at DESC LIMIT 200
`

const getRunForAdminQuery = runAttributionSelect + `
		WHERE r.id = $1
`

const getRunWithResultsQuery = `
		SELECT r.id, r.pod_id, r.playlist_id, r.triggered_by, r.runner_vm_id, r.runner_vm_name,
		       r.target_pod_vm_id, r.target_vm_name, r.target_vm_ip, r.callback_token,
		       r.status, r.total_workflows, r.passed_workflows, r.failed_workflows,
		       r.error_message, r.started_at, r.completed_at, r.created_at, r.updated_at,
		       COALESCE(u.username, '') AS triggered_by_username,
		       COALESCE(u.display_name, '') AS triggered_by_display_name,
		       p.owner_id,
		       COALESCE(po.username, '') AS pod_owner_username,
		       COALESCE(po.display_name, '') AS pod_owner_display_name,
		       COALESCE(p.name, '') AS pod_name,
		       COALESCE(p.status, '') AS pod_status,
		       COALESCE(pl.name, '') AS playlist_name
		FROM runs r
		LEFT JOIN users u ON r.triggered_by = u.id
		LEFT JOIN pods p ON r.pod_id = p.id
		LEFT JOIN users po ON p.owner_id = po.id
		LEFT JOIN playlists pl ON r.playlist_id = pl.id
		WHERE r.id = $1
`

func scanRunAttribution(scan func(dest ...any) error, r *models.Run) error {
	return scan(
		&r.ID, &r.PodID, &r.PlaylistID, &r.TriggeredBy, &r.Status,
		&r.TargetPodVMID, &r.TargetVMName, &r.TargetVMIP,
		&r.TriggeredByUsername, &r.TriggeredByDisplayName,
		&r.PodOwnerID,
		&r.PodOwnerUsername, &r.PodOwnerDisplayName,
		&r.PodName, &r.PodStatus, &r.PlaylistName,
		&r.TotalWorkflows, &r.PassedWorkflows, &r.FailedWorkflows,
		&r.ErrorMessage, &r.StartedAt, &r.CompletedAt, &r.CreatedAt, &r.UpdatedAt,
	)
}

// GetRecentRunsForPod returns the N most recent runs for a pod.
func (q *Queries) GetRecentRunsForPod(ctx context.Context, podID uuid.UUID, limit int) ([]models.Run, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, pod_id, playlist_id, triggered_by, status,
		       target_pod_vm_id, target_vm_name, target_vm_ip,
		       total_workflows, passed_workflows, failed_workflows,
		       error_message, started_at, completed_at, created_at, updated_at
		FROM runs WHERE pod_id = $1
		ORDER BY created_at DESC LIMIT $2
	`, podID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Non-nil empty slice so respondJSON encodes [] not null — clients (and
	// UI synthetics) treat a bare null as a contract break.
	runs := make([]models.Run, 0)
	for rows.Next() {
		var r models.Run
		if err := rows.Scan(&r.ID, &r.PodID, &r.PlaylistID, &r.TriggeredBy, &r.Status,
			&r.TargetPodVMID, &r.TargetVMName, &r.TargetVMIP,
			&r.TotalWorkflows, &r.PassedWorkflows, &r.FailedWorkflows,
			&r.ErrorMessage, &r.StartedAt, &r.CompletedAt,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// GetRunsForPod returns all runs for a pod.
func (q *Queries) GetRunsForPod(ctx context.Context, podID uuid.UUID) ([]models.Run, error) {
	return q.GetRecentRunsForPod(ctx, podID, 100)
}

// GetRun returns a single run by ID.
func (q *Queries) GetRun(ctx context.Context, runID uuid.UUID) (*models.Run, error) {
	var r models.Run
	err := q.pool.QueryRow(ctx, `
		SELECT id, pod_id, playlist_id, triggered_by, runner_vm_id, runner_vm_name,
		       target_pod_vm_id, target_vm_name, target_vm_ip,
		       callback_token, status, total_workflows, passed_workflows, failed_workflows,
		       error_message, started_at, completed_at, created_at, updated_at
		FROM runs WHERE id = $1
	`, runID).Scan(&r.ID, &r.PodID, &r.PlaylistID, &r.TriggeredBy,
		&r.RunnerVMID, &r.RunnerVMName,
		&r.TargetPodVMID, &r.TargetVMName, &r.TargetVMIP, &r.CallbackToken, &r.Status,
		&r.TotalWorkflows, &r.PassedWorkflows, &r.FailedWorkflows,
		&r.ErrorMessage, &r.StartedAt, &r.CompletedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRunForAdmin returns a single run with attribution joins for the admin view.
func (q *Queries) GetRunForAdmin(ctx context.Context, runID uuid.UUID) (*models.Run, error) {
	var r models.Run
	if err := scanRunAttribution(q.pool.QueryRow(ctx, getRunForAdminQuery, runID).Scan, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRunWithResults returns a run with all workflow results.
func (q *Queries) GetRunWithResults(ctx context.Context, runID uuid.UUID) (*models.Run, error) {
	var run models.Run
	if err := q.pool.QueryRow(ctx, getRunWithResultsQuery, runID).Scan(
		&run.ID, &run.PodID, &run.PlaylistID, &run.TriggeredBy, &run.RunnerVMID, &run.RunnerVMName,
		&run.TargetPodVMID, &run.TargetVMName, &run.TargetVMIP, &run.CallbackToken,
		&run.Status, &run.TotalWorkflows, &run.PassedWorkflows, &run.FailedWorkflows,
		&run.ErrorMessage, &run.StartedAt, &run.CompletedAt, &run.CreatedAt, &run.UpdatedAt,
		&run.TriggeredByUsername, &run.TriggeredByDisplayName, &run.PodOwnerID,
		&run.PodOwnerUsername, &run.PodOwnerDisplayName, &run.PodName, &run.PodStatus, &run.PlaylistName,
	); err != nil {
		return nil, err
	}

	rows, err := q.pool.Query(ctx, `
		SELECT wr.id, wr.run_id, wr.workflow_id, wr.workflow_version_id,
		       wr.execution_order, wr.execution_mode, wr.status,
		       wr.student_message, wr.instructor_output, wr.action_results,
		       wr.points_awarded, wr.duration_ms, wr.started_at, wr.completed_at,
		       wr.created_at, w.name
		FROM workflow_results wr
		JOIN workflows w ON wr.workflow_id = w.id
		WHERE wr.run_id = $1
		ORDER BY wr.execution_order
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var wr models.WorkflowResult
		if err := rows.Scan(&wr.ID, &wr.RunID, &wr.WorkflowID, &wr.WorkflowVersionID,
			&wr.ExecutionOrder, &wr.ExecutionMode, &wr.Status,
			&wr.StudentMessage, &wr.InstructorOutput, &wr.ActionResults,
			&wr.PointsAwarded, &wr.DurationMs, &wr.StartedAt, &wr.CompletedAt,
			&wr.CreatedAt, &wr.WorkflowName); err != nil {
			return nil, err
		}
		run.Results = append(run.Results, wr)
	}

	return &run, nil
}

// UpdateRunStatus updates the status of a run (used by API cancel handler).
func (q *Queries) UpdateRunStatus(ctx context.Context, runID uuid.UUID, status string, errorMsg *string) error {
	var completedAt *time.Time
	if status == models.RunStatusCompleted || status == models.RunStatusFailed ||
		status == models.RunStatusTimeout || status == models.RunStatusCancelled {
		now := time.Now()
		completedAt = &now
	}
	_, err := q.pool.Exec(ctx, `
		UPDATE runs SET status = $2, error_message = $3, completed_at = $4, updated_at = NOW()
		WHERE id = $1
	`, runID, status, errorMsg, completedAt)
	return err
}

// ListAllRuns returns all runs (admin view).
func (q *Queries) ListAllRuns(ctx context.Context) ([]models.Run, error) {
	rows, err := q.pool.Query(ctx, listAllRunsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []models.Run
	for rows.Next() {
		var r models.Run
		if err := scanRunAttribution(rows.Scan, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// RunsListFilter holds filter parameters for ListAllRunsFiltered.
type RunsListFilter struct {
	TriggeredBy    *uuid.UUID // exact match on triggered_by user UUID
	TriggeredByStr string     // substring match on triggered_by username/display_name
	PodOwner       *uuid.UUID // exact match on pod owner UUID
	PodOwnerStr    string     // substring match on pod owner username/display_name
	Status         string     // exact match on run status
	From           *time.Time // created_at >= From
	To             *time.Time // created_at <= To
	Limit          int        // default 200, max 1000
	Offset         int        // pagination offset
}

// ListAllRunsFiltered returns runs matching the provided filters (admin view).
// Unmatched or empty filters are ignored gracefully.
// Returns an empty slice if no runs match (never panics or 500s).
func (q *Queries) ListAllRunsFiltered(ctx context.Context, filter RunsListFilter) ([]models.Run, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 200
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	query := runAttributionSelect + " WHERE 1=1"
	args := []interface{}{}
	argIndex := 1

	// Filter by triggered_by UUID or username/display_name substring
	if filter.TriggeredBy != nil {
		query += fmt.Sprintf(" AND r.triggered_by = $%d", argIndex)
		args = append(args, *filter.TriggeredBy)
		argIndex++
	} else if filter.TriggeredByStr != "" {
		query += fmt.Sprintf(" AND (u.username ILIKE $%d OR u.display_name ILIKE $%d)", argIndex, argIndex+1)
		pattern := "%" + filter.TriggeredByStr + "%"
		args = append(args, pattern, pattern)
		argIndex += 2
	}

	// Filter by pod owner UUID or username/display_name substring
	if filter.PodOwner != nil {
		query += fmt.Sprintf(" AND p.owner_id = $%d", argIndex)
		args = append(args, *filter.PodOwner)
		argIndex++
	} else if filter.PodOwnerStr != "" {
		query += fmt.Sprintf(" AND (po.username ILIKE $%d OR po.display_name ILIKE $%d)", argIndex, argIndex+1)
		pattern := "%" + filter.PodOwnerStr + "%"
		args = append(args, pattern, pattern)
		argIndex += 2
	}

	// Filter by status
	if filter.Status != "" {
		query += fmt.Sprintf(" AND r.status = $%d", argIndex)
		args = append(args, filter.Status)
		argIndex++
	}

	// Filter by date range
	if filter.From != nil {
		query += fmt.Sprintf(" AND r.created_at >= $%d", argIndex)
		args = append(args, *filter.From)
		argIndex++
	}
	if filter.To != nil {
		query += fmt.Sprintf(" AND r.created_at <= $%d", argIndex)
		args = append(args, *filter.To)
		argIndex++
	}

	query += fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d OFFSET $%d", argIndex, argIndex+1)
	args = append(args, filter.Limit, filter.Offset)

	rows, err := q.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []models.Run
	for rows.Next() {
		var r models.Run
		if err := scanRunAttribution(rows.Scan, &r); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// --- Workflow CRUD Queries ---

// ListWorkflows returns all workflows with creator and approver attribution.
func (q *Queries) ListWorkflows(ctx context.Context) ([]models.Workflow, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT w.id, w.name, w.slug, w.description, w.category, w.execution_mode,
		       w.timeout_seconds, w.status, w.creation_mode, w.visible_to_students,
		       w.created_by, w.approved_by, w.is_active, w.created_at, w.updated_at,
		       cu.id, COALESCE(cu.username, ''), COALESCE(cu.email, ''), COALESCE(cu.display_name, ''), COALESCE(cu.role, ''),
		       au.id, COALESCE(au.username, ''), COALESCE(au.email, ''), COALESCE(au.display_name, ''), COALESCE(au.role, '')
		FROM workflows w
		LEFT JOIN users cu ON cu.id = w.created_by
		LEFT JOIN users au ON au.id = w.approved_by
		ORDER BY w.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workflows []models.Workflow
	for rows.Next() {
		var w models.Workflow
		var creatorID, approverID *uuid.UUID
		var creatorUsername, creatorEmail, creatorDisplayName, creatorRole string
		var approverUsername, approverEmail, approverDisplayName, approverRole string
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Category,
			&w.ExecutionMode, &w.TimeoutSeconds, &w.Status, &w.CreationMode,
			&w.VisibleToStudents, &w.CreatedBy, &w.ApprovedBy, &w.IsActive,
			&w.CreatedAt, &w.UpdatedAt,
			&creatorID, &creatorUsername, &creatorEmail, &creatorDisplayName, &creatorRole,
			&approverID, &approverUsername, &approverEmail, &approverDisplayName, &approverRole,
		); err != nil {
			return nil, err
		}
		if creatorID != nil {
			w.Creator = &models.User{
				ID:          *creatorID,
				Username:    creatorUsername,
				Email:       creatorEmail,
				DisplayName: creatorDisplayName,
				Role:        creatorRole,
			}
		}
		if approverID != nil {
			w.Approver = &models.User{
				ID:          *approverID,
				Username:    approverUsername,
				Email:       approverEmail,
				DisplayName: approverDisplayName,
				Role:        approverRole,
			}
		}
		workflows = append(workflows, w)
	}
	return workflows, nil
}

// GetWorkflow returns a single workflow by ID.
func (q *Queries) GetWorkflow(ctx context.Context, id uuid.UUID) (*models.Workflow, error) {
	var w models.Workflow
	err := q.pool.QueryRow(ctx, `
		SELECT id, name, slug, description, category, execution_mode, script,
		       setup_script, timeout_seconds, status, creation_mode, visible_to_students,
		       created_by, approved_by, is_active, created_at, updated_at
		FROM workflows WHERE id = $1
	`, id).Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Category,
		&w.ExecutionMode, &w.Script, &w.SetupScript, &w.TimeoutSeconds,
		&w.Status, &w.CreationMode, &w.VisibleToStudents, &w.CreatedBy,
		&w.ApprovedBy, &w.IsActive, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// GetWorkflowWithActions returns a workflow with its actions.
func (q *Queries) GetWorkflowWithActions(ctx context.Context, id uuid.UUID) (*models.Workflow, error) {
	wf, err := q.GetWorkflow(ctx, id)
	if err != nil {
		return nil, err
	}

	rows, err := q.pool.Query(ctx, `
		SELECT id, workflow_id, name, description, action_type, params,
		       execution_order, timeout_seconds, student_fail_hint, points, penalty,
		       supported_platforms, created_at
		FROM actions WHERE workflow_id = $1 ORDER BY execution_order
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var a models.Action
		if err := rows.Scan(&a.ID, &a.WorkflowID, &a.Name, &a.Description, &a.ActionType,
			&a.Params, &a.ExecutionOrder, &a.TimeoutSeconds, &a.StudentFailHint,
			&a.Points, &a.Penalty, &a.SupportedPlatforms, &a.CreatedAt); err != nil {
			return nil, err
		}
		wf.Actions = append(wf.Actions, a)
	}
	return wf, nil
}

// CreateWorkflow inserts a new workflow and its actions.
func (q *Queries) CreateWorkflow(ctx context.Context, wf *models.Workflow) error {
	err := q.pool.QueryRow(ctx, `
		INSERT INTO workflows (name, slug, description, category, execution_mode, script,
		       setup_script, timeout_seconds, creation_mode, visible_to_students, status, created_by, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id, created_at, updated_at
	`, wf.Name, wf.Slug, wf.Description, wf.Category, wf.ExecutionMode, wf.Script,
		wf.SetupScript, wf.TimeoutSeconds, wf.CreationMode, wf.VisibleToStudents,
		wf.Status, wf.CreatedBy, wf.IsActive,
	).Scan(&wf.ID, &wf.CreatedAt, &wf.UpdatedAt)
	if err != nil {
		return err
	}

	// Insert actions
	for i, a := range wf.Actions {
		paramsJSON := a.Params
		if paramsJSON == nil {
			paramsJSON = json.RawMessage("{}")
		}
		_, err := q.pool.Exec(ctx, `
			INSERT INTO actions (workflow_id, name, description, action_type, params,
			       execution_order, timeout_seconds, student_fail_hint, points, penalty)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, wf.ID, a.Name, a.Description, a.ActionType, paramsJSON,
			i, a.TimeoutSeconds, a.StudentFailHint, a.Points, a.Penalty)
		if err != nil {
			return fmt.Errorf("insert action %d: %w", i, err)
		}
	}
	return nil
}

// UpdateWorkflow updates a workflow and returns any reviewed revision to draft.
func (q *Queries) UpdateWorkflow(ctx context.Context, id uuid.UUID, name, description, category,
	script, setupScript *string, timeoutSeconds *int, creationMode *string, actions []models.Action) error {

	contentChanged := workflowEditRequiresReview(
		name, description, category, script, setupScript, timeoutSeconds, creationMode,
	)
	_, err := q.pool.Exec(ctx, `
		UPDATE workflows SET
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			category = COALESCE($4, category),
			script = COALESCE($5, script),
			setup_script = COALESCE($6, setup_script),
			timeout_seconds = COALESCE($7, timeout_seconds),
			creation_mode = COALESCE($8, creation_mode),
			status = CASE WHEN $9 THEN 'draft' ELSE status END,
			approved_by = CASE WHEN $9 THEN NULL ELSE approved_by END,
			updated_at = NOW()
		WHERE id = $1
	`, id, name, description, category, script, setupScript, timeoutSeconds, creationMode, contentChanged)
	return err
}

func workflowEditRequiresReview(name, description, category, script, setupScript *string,
	timeoutSeconds *int, creationMode *string) bool {
	return name != nil || description != nil || category != nil ||
		script != nil || setupScript != nil || timeoutSeconds != nil ||
		creationMode != nil
}

// TransitionWorkflowStatus atomically transitions a workflow between states.
func (q *Queries) TransitionWorkflowStatus(ctx context.Context, id uuid.UUID, from, to string) error {
	result, err := q.pool.Exec(ctx, `
		UPDATE workflows SET status = $3, updated_at = NOW()
		WHERE id = $1 AND status = $2
	`, id, from, to)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("workflow not in %s status", from)
	}
	return nil
}

// ApproveWorkflow approves a workflow and records the approver.
func (q *Queries) ApproveWorkflow(ctx context.Context, id, approverID uuid.UUID) error {
	result, err := q.pool.Exec(ctx, `
		UPDATE workflows SET status = 'approved', approved_by = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'pending_review'
	`, id, approverID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("workflow not in pending_review status")
	}
	return nil
}

// DeleteWorkflow deletes a workflow and its associated actions.
// Only drafts can be deleted; active/approved workflows must be deactivated first.
func (q *Queries) DeleteWorkflow(ctx context.Context, id uuid.UUID) error {
	// Delete actions first (foreign key)
	_, err := q.pool.Exec(ctx, `DELETE FROM actions WHERE workflow_id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to delete workflow actions: %w", err)
	}
	// Remove from any playlist associations
	_, err = q.pool.Exec(ctx, `DELETE FROM playlist_workflows WHERE workflow_id = $1`, id)
	if err != nil {
		return fmt.Errorf("failed to remove playlist associations: %w", err)
	}
	result, err := q.pool.Exec(ctx, `DELETE FROM workflows WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("workflow not found")
	}
	return nil
}

// ListWorkflowsWithActions returns all workflows with their actions (for export).
func (q *Queries) ListWorkflowsWithActions(ctx context.Context) ([]models.Workflow, error) {
	workflows, err := q.ListWorkflows(ctx)
	if err != nil {
		return nil, err
	}
	for i := range workflows {
		wf, err := q.GetWorkflowWithActions(ctx, workflows[i].ID)
		if err != nil {
			return nil, err
		}
		workflows[i].Actions = wf.Actions
	}
	return workflows, nil
}

// --- Action Library CRUD Queries ---

// ListLibraryActions returns all standalone library actions.
func (q *Queries) ListLibraryActions(ctx context.Context) ([]models.Action, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, workflow_id, name, slug, description, action_type, action_category,
		       params, script, input_context, output_context, execution_order,
		       timeout_seconds, student_fail_hint, points, penalty, is_library,
		       supported_platforms, created_at, updated_at
		FROM actions WHERE is_library = true
		ORDER BY action_category, name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var actions []models.Action
	for rows.Next() {
		var a models.Action
		if err := rows.Scan(&a.ID, &a.WorkflowID, &a.Name, &a.Slug, &a.Description,
			&a.ActionType, &a.ActionCategory, &a.Params, &a.Script,
			&a.InputContext, &a.OutputContext, &a.ExecutionOrder,
			&a.TimeoutSeconds, &a.StudentFailHint, &a.Points, &a.Penalty,
			&a.IsLibrary, &a.SupportedPlatforms, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	return actions, nil
}

// CreateLibraryAction inserts a new standalone action.
func (q *Queries) CreateLibraryAction(ctx context.Context, a *models.Action) error {
	platforms := a.SupportedPlatforms
	if platforms == nil {
		platforms = json.RawMessage(`["any"]`)
	}
	return q.pool.QueryRow(ctx, `
		INSERT INTO actions (name, slug, description, action_type, action_category,
		       params, script, input_context, output_context, timeout_seconds,
		       student_fail_hint, points, penalty, is_library, supported_platforms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, true, $14)
		RETURNING id, created_at, updated_at
	`, a.Name, a.Slug, a.Description, a.ActionType, a.ActionCategory,
		a.Params, a.Script, a.InputContext, a.OutputContext, a.TimeoutSeconds,
		a.StudentFailHint, a.Points, a.Penalty, platforms,
	).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)
}

// GetLibraryAction returns a single library action by ID.
func (q *Queries) GetLibraryAction(ctx context.Context, id uuid.UUID) (*models.Action, error) {
	var a models.Action
	err := q.pool.QueryRow(ctx, `
		SELECT id, workflow_id, name, slug, description, action_type, action_category,
		       params, script, input_context, output_context, execution_order,
		       timeout_seconds, student_fail_hint, points, penalty, is_library,
		       supported_platforms, created_at, updated_at
		FROM actions WHERE id = $1 AND is_library = true
	`, id).Scan(&a.ID, &a.WorkflowID, &a.Name, &a.Slug, &a.Description,
		&a.ActionType, &a.ActionCategory, &a.Params, &a.Script,
		&a.InputContext, &a.OutputContext, &a.ExecutionOrder,
		&a.TimeoutSeconds, &a.StudentFailHint, &a.Points, &a.Penalty,
		&a.IsLibrary, &a.SupportedPlatforms, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateLibraryAction updates a library action using COALESCE for partial updates.
func (q *Queries) UpdateLibraryAction(ctx context.Context, id uuid.UUID, name, slug, description,
	actionType, actionCategory *string, params, script *string,
	inputContext, outputContext *json.RawMessage,
	timeoutSeconds *int, studentFailHint *string, points, penalty *int,
	supportedPlatforms *json.RawMessage) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE actions SET
			name = COALESCE($2, name),
			slug = COALESCE($3, slug),
			description = COALESCE($4, description),
			action_type = COALESCE($5, action_type),
			action_category = COALESCE($6, action_category),
			params = COALESCE($7, params),
			script = COALESCE($8, script),
			input_context = COALESCE($9, input_context),
			output_context = COALESCE($10, output_context),
			timeout_seconds = COALESCE($11, timeout_seconds),
			student_fail_hint = COALESCE($12, student_fail_hint),
			points = COALESCE($13, points),
			penalty = COALESCE($14, penalty),
			supported_platforms = COALESCE($15, supported_platforms),
			updated_at = NOW()
		WHERE id = $1 AND is_library = true
	`, id, name, slug, description, actionType, actionCategory,
		params, script, inputContext, outputContext,
		timeoutSeconds, studentFailHint, points, penalty, supportedPlatforms)
	return err
}

// DeleteLibraryAction deletes a standalone action (not workflow-bound).
func (q *Queries) DeleteLibraryAction(ctx context.Context, id uuid.UUID) error {
	result, err := q.pool.Exec(ctx, `DELETE FROM actions WHERE id = $1 AND is_library = true`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("action not found or not a library action")
	}
	return nil
}

// --- Playlist CRUD Queries ---

// ListPlaylists returns all playlists.
func (q *Queries) ListPlaylists(ctx context.Context) ([]models.Playlist, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, name, slug, description, scoring_mode, created_by, is_active, created_at, updated_at
		FROM playlists ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []models.Playlist
	for rows.Next() {
		var p models.Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.ScoringMode,
			&p.CreatedBy, &p.IsActive, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		playlists = append(playlists, p)
	}
	return playlists, nil
}

// GetPlaylistWithWorkflows returns a playlist with its workflows.
func (q *Queries) GetPlaylistWithWorkflows(ctx context.Context, id uuid.UUID) (*models.Playlist, error) {
	var p models.Playlist
	err := q.pool.QueryRow(ctx, `
		SELECT id, name, slug, description, scoring_mode, created_by, is_active, created_at, updated_at
		FROM playlists WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.Slug, &p.Description, &p.ScoringMode,
		&p.CreatedBy, &p.IsActive, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}

	rows, err := q.pool.Query(ctx, `
		SELECT w.id, w.name, w.slug, w.description, w.category, w.execution_mode,
		       w.timeout_seconds, w.status, w.visible_to_students, w.is_active
		FROM workflows w
		JOIN playlist_workflows pw ON w.id = pw.workflow_id
		WHERE pw.playlist_id = $1
		ORDER BY pw.execution_order
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var w models.Workflow
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.Category,
			&w.ExecutionMode, &w.TimeoutSeconds, &w.Status, &w.VisibleToStudents,
			&w.IsActive); err != nil {
			return nil, err
		}
		p.Workflows = append(p.Workflows, w)
	}
	return &p, nil
}

// CreatePlaylist creates a playlist and sets its workflow membership.
func (q *Queries) CreatePlaylist(ctx context.Context, pl *models.Playlist, workflowIDs []uuid.UUID) error {
	err := q.pool.QueryRow(ctx, `
		INSERT INTO playlists (name, slug, description, scoring_mode, created_by, is_active)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at
	`, pl.Name, pl.Slug, pl.Description, pl.ScoringMode, pl.CreatedBy, pl.IsActive,
	).Scan(&pl.ID, &pl.CreatedAt, &pl.UpdatedAt)
	if err != nil {
		return err
	}

	for i, wfID := range workflowIDs {
		_, err := q.pool.Exec(ctx, `
			INSERT INTO playlist_workflows (playlist_id, workflow_id, execution_order)
			VALUES ($1, $2, $3)
		`, pl.ID, wfID, i)
		if err != nil {
			return fmt.Errorf("insert playlist_workflow: %w", err)
		}
	}
	return nil
}

// UpdatePlaylist updates a playlist's metadata and optionally its workflow membership.
func (q *Queries) UpdatePlaylist(ctx context.Context, id uuid.UUID, name, description *string,
	isActive *bool, workflowIDs []uuid.UUID) error {

	_, err := q.pool.Exec(ctx, `
		UPDATE playlists SET
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			is_active = COALESCE($4, is_active),
			updated_at = NOW()
		WHERE id = $1
	`, id, name, description, isActive)
	if err != nil {
		return err
	}

	// Replace workflow membership if provided
	if workflowIDs != nil {
		_, err = q.pool.Exec(ctx, `DELETE FROM playlist_workflows WHERE playlist_id = $1`, id)
		if err != nil {
			return err
		}
		for i, wfID := range workflowIDs {
			_, err = q.pool.Exec(ctx, `
				INSERT INTO playlist_workflows (playlist_id, workflow_id, execution_order)
				VALUES ($1, $2, $3)
			`, id, wfID, i)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// DeactivatePlaylist soft-deletes a playlist.
func (q *Queries) DeactivatePlaylist(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `UPDATE playlists SET is_active = false, updated_at = NOW() WHERE id = $1`, id)
	return err
}

// SetTemplatePlaylists replaces the playlists assigned to a template.
func (q *Queries) SetTemplatePlaylists(ctx context.Context, templateID uuid.UUID, playlistIDs []uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM template_playlists WHERE template_id = $1`, templateID)
	if err != nil {
		return err
	}
	for i, plID := range playlistIDs {
		_, err = q.pool.Exec(ctx, `
			INSERT INTO template_playlists (template_id, playlist_id, execution_order)
			VALUES ($1, $2, $3)
		`, templateID, plID, i)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetTemplatePlaylists returns playlist IDs assigned to a template.
func (q *Queries) GetTemplatePlaylists(ctx context.Context, templateID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT playlist_id FROM template_playlists
		WHERE template_id = $1 ORDER BY execution_order
	`, templateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// SetBlueprintVMPlaylists replaces the playlist overrides for a blueprint VM slot.
func (q *Queries) SetBlueprintVMPlaylists(ctx context.Context, blueprintID uuid.UUID, vmSlot int, playlistIDs []uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM blueprint_vm_playlists WHERE blueprint_id = $1 AND vm_slot = $2`, blueprintID, vmSlot)
	if err != nil {
		return err
	}
	for i, plID := range playlistIDs {
		_, err = q.pool.Exec(ctx, `
			INSERT INTO blueprint_vm_playlists (blueprint_id, vm_slot, playlist_id, execution_order)
			VALUES ($1, $2, $3, $4)
		`, blueprintID, vmSlot, plID, i)
		if err != nil {
			return err
		}
	}
	return nil
}

// BlueprintVMPlaylistsResolvedRow represents a row in the GetBlueprintVMPlaylistsResolved result.
type BlueprintVMPlaylistsResolvedRow struct {
	VMSlot         int       `json:"vm_slot"`
	PlaylistID     uuid.UUID `json:"playlist_id"`
	PlaylistName   string    `json:"name"`
	PlaylistSlug   string    `json:"slug"`
	Source         string    `json:"source"` // "blueprint_override" or "template_default"
	ExecutionOrder int       `json:"execution_order"`
}

// GetBlueprintVMPlaylistsResolved returns the resolved playlists for each VM slot on a blueprint,
// with explicit source labeling ("blueprint_override" or "template_default").
// Returns sql.ErrNoRows if the blueprint does not exist.
func (q *Queries) GetBlueprintVMPlaylistsResolved(ctx context.Context, blueprintID uuid.UUID) ([]BlueprintVMPlaylistsResolvedRow, error) {
	// First, verify the blueprint exists
	var exists bool
	err := q.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blueprints WHERE id = $1)`, blueprintID).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}

	// Get all VM slots and their playlists with override+default resolution
	// Union of:
	// 1. Blueprint overrides (source = 'blueprint_override')
	// 2. Template defaults that have no blueprint override (source = 'template_default')
	rows, err := q.pool.Query(ctx, `
		-- Get blueprint overrides
		SELECT bvp.vm_slot, p.id, p.name, p.slug, 'blueprint_override' as source, bvp.execution_order
		FROM blueprint_vm_playlists bvp
		JOIN playlists p ON p.id = bvp.playlist_id
		WHERE bvp.blueprint_id = $1
		  AND p.is_active = true

		UNION ALL

		-- Get template defaults where no override exists
		SELECT DISTINCT bv.boot_order, p.id, p.name, p.slug, 'template_default', tp.execution_order
		FROM blueprint_vms bv
		JOIN blueprint_templates bt ON bt.id = bv.template_id
		JOIN template_playlists tp ON tp.template_id = bt.id
		JOIN playlists p ON p.id = tp.playlist_id
		WHERE bv.blueprint_id = $1
		  AND p.is_active = true
		  AND NOT EXISTS (
			SELECT 1 FROM blueprint_vm_playlists bvp
			WHERE bvp.blueprint_id = $1
			  AND bvp.vm_slot = bv.boot_order
		  )

		ORDER BY vm_slot, execution_order
	`, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []BlueprintVMPlaylistsResolvedRow
	for rows.Next() {
		var row BlueprintVMPlaylistsResolvedRow
		if err := rows.Scan(&row.VMSlot, &row.PlaylistID, &row.PlaylistName, &row.PlaylistSlug, &row.Source, &row.ExecutionOrder); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
