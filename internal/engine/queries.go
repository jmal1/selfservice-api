package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/runner"
)

// Queries provides database operations for the workflow engine.
type Queries struct {
	pool *pgxpool.Pool
}

// NewQueries creates a new Queries instance.
func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

// ClaimPendingRun atomically claims the oldest pending run for processing.
// Returns nil if no pending runs are available.
func (q *Queries) ClaimPendingRun(ctx context.Context, engineID string) (*models.Run, error) {
	var run models.Run
	err := q.pool.QueryRow(ctx, `
		UPDATE runs
		SET status = $1, started_at = NOW(), updated_at = NOW()
		WHERE id = (
			SELECT id FROM runs
			WHERE status = $2
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, pod_id, playlist_id, triggered_by, callback_token, status,
		          total_workflows, passed_workflows, failed_workflows,
		          error_message, started_at, completed_at, created_at, updated_at
	`, models.RunStatusProvisioning, models.RunStatusPending).Scan(
		&run.ID, &run.PodID, &run.PlaylistID, &run.TriggeredBy,
		&run.CallbackToken, &run.Status,
		&run.TotalWorkflows, &run.PassedWorkflows, &run.FailedWorkflows,
		&run.ErrorMessage, &run.StartedAt, &run.CompletedAt,
		&run.CreatedAt, &run.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim pending run: %w", err)
	}
	return &run, nil
}

// UpdateRunStatus updates the status of a run.
func (q *Queries) UpdateRunStatus(ctx context.Context, runID uuid.UUID, status string, errorMsg *string) error {
	var completedAt *time.Time
	if status == models.RunStatusCompleted || status == models.RunStatusFailed ||
		status == models.RunStatusTimeout || status == models.RunStatusCancelled {
		now := time.Now()
		completedAt = &now
	}

	_, err := q.pool.Exec(ctx, `
		UPDATE runs
		SET status = $2, error_message = $3, completed_at = $4, updated_at = NOW()
		WHERE id = $1
	`, runID, status, errorMsg, completedAt)
	return err
}

// UpdateRunCounts updates the pass/fail counts on a run.
func (q *Queries) UpdateRunCounts(ctx context.Context, runID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE runs SET
			total_workflows = (SELECT COUNT(*) FROM workflow_results WHERE run_id = $1),
			passed_workflows = (SELECT COUNT(*) FROM workflow_results WHERE run_id = $1 AND status = 'pass'),
			failed_workflows = (SELECT COUNT(*) FROM workflow_results WHERE run_id = $1 AND status IN ('fail', 'error', 'timeout')),
			updated_at = NOW()
		WHERE id = $1
	`, runID)
	return err
}

// FindStaleRuns returns runs stuck in provisioning or running for longer than maxAge.
func (q *Queries) FindStaleRuns(ctx context.Context, maxAge time.Duration) ([]models.Run, error) {
	cutoff := time.Now().Add(-maxAge)
	rows, err := q.pool.Query(ctx, `
		SELECT id, pod_id, playlist_id, triggered_by, runner_vm_id, runner_vm_name,
		       callback_token, status, total_workflows, passed_workflows, failed_workflows,
		       error_message, started_at, completed_at, created_at, updated_at
		FROM runs
		WHERE status IN ($1, $2)
		  AND started_at < $3
	`, models.RunStatusProvisioning, models.RunStatusRunning, cutoff)
	if err != nil {
		return nil, fmt.Errorf("find stale runs: %w", err)
	}
	defer rows.Close()

	var runs []models.Run
	for rows.Next() {
		var r models.Run
		if err := rows.Scan(
			&r.ID, &r.PodID, &r.PlaylistID, &r.TriggeredBy,
			&r.RunnerVMID, &r.RunnerVMName, &r.CallbackToken, &r.Status,
			&r.TotalWorkflows, &r.PassedWorkflows, &r.FailedWorkflows,
			&r.ErrorMessage, &r.StartedAt, &r.CompletedAt,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan stale run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// GetWorkflowsForPlaylist returns all active workflows in a playlist, ordered.
func (q *Queries) GetWorkflowsForPlaylist(ctx context.Context, playlistID uuid.UUID) ([]models.Workflow, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT w.id, w.name, w.slug, w.description, w.category, w.execution_mode,
		       w.script, w.setup_script, w.timeout_seconds, w.target_os, w.target_vm,
		       w.guest_interpreter, w.status, w.creation_mode, w.created_by, w.is_active,
		       w.created_at, w.updated_at
		FROM workflows w
		JOIN playlist_workflows pw ON w.id = pw.workflow_id
		WHERE pw.playlist_id = $1
		  AND w.is_active = true
		ORDER BY pw.execution_order ASC
	`, playlistID)
	if err != nil {
		return nil, fmt.Errorf("get workflows for playlist: %w", err)
	}
	defer rows.Close()

	var workflows []models.Workflow
	for rows.Next() {
		var w models.Workflow
		if err := rows.Scan(
			&w.ID, &w.Name, &w.Slug, &w.Description, &w.Category, &w.ExecutionMode,
			&w.Script, &w.SetupScript, &w.TimeoutSeconds, &w.TargetOS, &w.TargetVM,
			&w.GuestInterpreter, &w.Status, &w.CreationMode, &w.CreatedBy, &w.IsActive,
			&w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow: %w", err)
		}
		workflows = append(workflows, w)
	}
	return workflows, nil
}

// GetActionsForWorkflow returns all actions for a workflow, ordered.
func (q *Queries) GetActionsForWorkflow(ctx context.Context, workflowID uuid.UUID) ([]models.Action, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, workflow_id, name, description, action_type, params,
		       execution_order, timeout_seconds, student_fail_hint, points, penalty, created_at
		FROM actions
		WHERE workflow_id = $1
		ORDER BY execution_order ASC
	`, workflowID)
	if err != nil {
		return nil, fmt.Errorf("get actions for workflow: %w", err)
	}
	defer rows.Close()

	var actions []models.Action
	for rows.Next() {
		var a models.Action
		if err := rows.Scan(
			&a.ID, &a.WorkflowID, &a.Name, &a.Description, &a.ActionType, &a.Params,
			&a.ExecutionOrder, &a.TimeoutSeconds, &a.StudentFailHint,
			&a.Points, &a.Penalty, &a.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan action: %w", err)
		}
		actions = append(actions, a)
	}
	return actions, nil
}

// CreateWorkflowVersion creates an immutable version snapshot.
func (q *Queries) CreateWorkflowVersion(ctx context.Context, wv *models.WorkflowVersion) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO workflow_versions (workflow_id, version, script, actions, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at
	`, wv.WorkflowID, wv.Version, wv.Script, wv.Actions, wv.CreatedBy).Scan(&wv.ID, &wv.CreatedAt)
}

// GetLatestWorkflowVersion returns the latest version number for a workflow.
func (q *Queries) GetLatestWorkflowVersion(ctx context.Context, workflowID uuid.UUID) (int, error) {
	var version int
	err := q.pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(version), 0) FROM workflow_versions WHERE workflow_id = $1
	`, workflowID).Scan(&version)
	return version, err
}

// InsertWorkflowResult inserts a pending workflow result for a run.
func (q *Queries) InsertWorkflowResult(ctx context.Context, wr *models.WorkflowResult) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO workflow_results (run_id, workflow_id, workflow_version_id, execution_order, execution_mode, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at
	`, wr.RunID, wr.WorkflowID, wr.WorkflowVersionID, wr.ExecutionOrder, wr.ExecutionMode, wr.Status,
	).Scan(&wr.ID, &wr.CreatedAt)
}

// UpdateWorkflowResult updates a workflow result with execution outcome.
func (q *Queries) UpdateWorkflowResult(ctx context.Context, resultID uuid.UUID, status string,
	studentMsg *string, instructorOutput, actionResults []byte, durationMs *int) error {

	now := time.Now()
	_, err := q.pool.Exec(ctx, `
		UPDATE workflow_results
		SET status = $2, student_message = $3, instructor_output = $4,
		    action_results = $5, duration_ms = $6, completed_at = $7
		WHERE id = $1
	`, resultID, status, studentMsg, instructorOutput, actionResults, durationMs, now)
	return err
}

// GenerateCallbackToken creates a cryptographically random callback token.
func GenerateCallbackToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate callback token: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// GetPodVLANTag returns the VLAN tag assigned to a pod.
//
// Despite its name, pods.vlan_id stores the VLAN *tag* (e.g. 119), not a
// foreign key into vlan_pool.id — see CreatePod/DeployBlueprint, which both do
// `UPDATE pods SET vlan_id = $1` with the tag returned by CheckoutVLAN. Reading
// the column directly is therefore both correct and cheaper than a join.
//
// This previously read `JOIN vlans v ON p.vlan_id = v.id`, which was wrong
// twice over: there has never been a table called `vlans` (it is `vlan_pool`),
// and joining a tag against a serial primary key would have silently returned
// some *other* pod's VLAN even if the table name had been right. Every
// kali_runner assessment failed at this line. See TestEngineSQLReferencesOnlyRealTables.
func (q *Queries) GetPodVLANTag(ctx context.Context, podID uuid.UUID) (int, error) {
	var vlanTag int
	err := q.pool.QueryRow(ctx, `
		SELECT vlan_id FROM pods WHERE id = $1
	`, podID).Scan(&vlanTag)
	if err != nil {
		return 0, fmt.Errorf("get pod VLAN tag: %w", err)
	}
	if vlanTag == 0 {
		return 0, fmt.Errorf("pod %s has no VLAN allocated (vlan_id=0)", podID)
	}
	return vlanTag, nil
}

// GetRunTargetInfo retrieves the primary target VM info and pod network config
// for building the runner config. Returns the target (first VM with credentials)
// and pod network metadata.
func (q *Queries) GetRunTargetInfo(ctx context.Context, podID uuid.UUID) (runner.TargetConfig, runner.PodConfig, error) {
	var target runner.TargetConfig
	var pod runner.PodConfig

	// Get the first VM in this pod with generated credentials
	err := q.pool.QueryRow(ctx, `
		SELECT COALESCE(pv.ip_address, ''), COALESCE(t.os_type, 'linux'),
		       COALESCE(pv.generated_username, ''), COALESCE(pv.generated_password, '')
		FROM pod_vms pv
		JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1
		ORDER BY pv.created_at ASC
		LIMIT 1
	`, podID).Scan(&target.IP, &target.OS, &target.Username, &target.Password)
	if err != nil {
		return target, pod, fmt.Errorf("get target VM: %w", err)
	}

	// Get pod network info.
	//
	// Reads pods.subnet directly rather than joining a VLAN table: pods.subnet
	// is populated at checkout time (see CreatePod) and pods.vlan_id holds the
	// VLAN tag itself, not a foreign key.
	//
	// This previously read `SELECT v.subnet, COALESCE(p.pod_index, 0) FROM pods p
	// JOIN vlans v ON p.vlan_id = v.id`, which could never have run: there is no
	// `vlans` table (it is `vlan_pool`) and `pods.pod_index` was dropped by
	// migration 000003. Pod index is now carried by the VLAN tag, which is the
	// pod's unique numeric network identifier.
	err = q.pool.QueryRow(ctx, `
		SELECT COALESCE(p.subnet, ''), COALESCE(p.vlan_id, 0)
		FROM pods p
		WHERE p.id = $1
	`, podID).Scan(&pod.Subnet, &pod.Index)
	if err != nil {
		return target, pod, fmt.Errorf("get pod network: %w", err)
	}

	return target, pod, nil
}

// GetVMwareToolsTarget loads the moref + guest credentials needed to dispatch
// a vmware_tools workflow against a pod's primary VM.
//
// Resolution rules:
//   - moref comes from pod_vms.vcenter_vm_id (required; if missing the VM
//     wasn't provisioned through us and we can't talk to it via govmomi)
//   - generated credentials win over the template's defaults (this matches
//     the kali_runner path so behavior is consistent across modes)
//   - falls back to templates.default_username/default_password for VMs that
//     don't get per-pod credential generation (e.g. registered_existing_vm
//     kind from Track T3, where the static creds are the source of truth)
//
// Returns ErrNoVMwareToolsTarget if no eligible VM is found, so the caller
// can produce a clean per-workflow error rather than a generic 500.
func (q *Queries) GetVMwareToolsTarget(ctx context.Context, podID uuid.UUID) (string, string, string, string, error) {
	var moref, osType, username, password string
	err := q.pool.QueryRow(ctx, `
		SELECT
			COALESCE(pv.vcenter_vm_id, ''),
			COALESCE(t.os_type, 'linux'),
			COALESCE(NULLIF(pv.generated_username, ''), t.default_username, ''),
			COALESCE(NULLIF(pv.generated_password, ''), t.default_password, '')
		FROM pod_vms pv
		JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1
		ORDER BY pv.created_at ASC
		LIMIT 1
	`, podID).Scan(&moref, &osType, &username, &password)
	if err != nil {
		return "", "", "", "", fmt.Errorf("get vmware_tools target: %w", err)
	}
	return moref, osType, username, password, nil
}

// SetRunnerPodName stores the K8s Job name on the run for later cleanup.
func (q *Queries) SetRunnerPodName(ctx context.Context, runID uuid.UUID, jobName string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE runs SET runner_vm_name = $2, updated_at = NOW()
		WHERE id = $1
	`, runID, jobName)
	return err
}
