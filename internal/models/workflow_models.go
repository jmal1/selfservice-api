package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Workflow status constants
const (
	WorkflowStatusDraft         = "draft"
	WorkflowStatusPendingReview = "pending_review"
	WorkflowStatusApproved      = "approved"
	WorkflowStatusActive        = "active"
)

// Workflow execution modes
const (
	ExecModeKaliRunner  = "kali_runner"
	ExecModeVMwareTools = "vmware_tools"
)

// Workflow creation modes
const (
	CreationModeVisual = "visual"
	CreationModeScript = "script"
)

// Run status constants
const (
	RunStatusPending      = "pending"
	RunStatusProvisioning = "provisioning"
	RunStatusRunning      = "running"
	RunStatusCompleted    = "completed"
	RunStatusFailed       = "failed"
	RunStatusCancelled    = "cancelled"
	RunStatusTimeout      = "timeout"
)

// Workflow result status constants
const (
	ResultStatusPending   = "pending"
	ResultStatusRunning   = "running"
	ResultStatusPass      = "pass"
	ResultStatusFail      = "fail"
	ResultStatusError     = "error"
	ResultStatusTimeout   = "timeout"
	ResultStatusSkipped   = "skipped"
	ResultStatusCancelled = "cancelled"
)

// Playlist scoring modes
const (
	ScoringModePassFail = "pass_fail"
	ScoringModePoints   = "points"
)

// Workflow represents an instructor-authored assessment script.
type Workflow struct {
	ID                uuid.UUID       `json:"id" db:"id"`
	Name              string          `json:"name" db:"name"`
	Slug              string          `json:"slug" db:"slug"`
	Description       string          `json:"description" db:"description"`
	Category          string          `json:"category" db:"category"`
	ExecutionMode     string          `json:"execution_mode" db:"execution_mode"`
	Script            string          `json:"script" db:"script"`
	SetupScript       *string         `json:"setup_script" db:"setup_script"`
	TimeoutSeconds    int             `json:"timeout_seconds" db:"timeout_seconds"`
	TargetOS          *string         `json:"target_os" db:"target_os"`
	TargetVM          *string         `json:"target_vm" db:"target_vm"`
	GuestInterpreter  *string         `json:"guest_interpreter" db:"guest_interpreter"`
	RequiredServices  []string        `json:"required_services" db:"required_services"`
	Metadata          json.RawMessage `json:"metadata" db:"metadata"`
	VisibleToStudents bool            `json:"visible_to_students" db:"visible_to_students"`
	Status            string          `json:"status" db:"status"`
	CreationMode      string          `json:"creation_mode" db:"creation_mode"`
	CreatedBy         uuid.UUID       `json:"created_by" db:"created_by"`
	ApprovedBy        *uuid.UUID      `json:"approved_by" db:"approved_by"`
	IsActive          bool            `json:"is_active" db:"is_active"`
	CreatedAt         time.Time       `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at" db:"updated_at"`
	// Loaded via join
	Actions []Action `json:"actions,omitempty" db:"-"`
}

// Action represents an ordered step within a workflow.
type Action struct {
	ID              uuid.UUID       `json:"id" db:"id"`
	WorkflowID      uuid.UUID       `json:"workflow_id" db:"workflow_id"`
	Name            string          `json:"name" db:"name"`
	Description     string          `json:"description" db:"description"`
	ActionType      string          `json:"action_type" db:"action_type"`
	Params          json.RawMessage `json:"params" db:"params"`
	ExecutionOrder  int             `json:"execution_order" db:"execution_order"`
	TimeoutSeconds  int             `json:"timeout_seconds" db:"timeout_seconds"`
	StudentFailHint *string         `json:"student_fail_hint" db:"student_fail_hint"`
	Points          *int            `json:"points" db:"points"`
	Penalty         *int            `json:"penalty" db:"penalty"`
	CreatedAt       time.Time       `json:"created_at" db:"created_at"`
}

// WorkflowVersion is an immutable snapshot of a workflow for run pinning.
type WorkflowVersion struct {
	ID         uuid.UUID       `json:"id" db:"id"`
	WorkflowID uuid.UUID      `json:"workflow_id" db:"workflow_id"`
	Version    int             `json:"version" db:"version"`
	Script     string          `json:"script" db:"script"`
	Actions    json.RawMessage `json:"actions" db:"actions"`
	CreatedBy  uuid.UUID       `json:"created_by" db:"created_by"`
	CreatedAt  time.Time       `json:"created_at" db:"created_at"`
}

// Playlist is a named collection of workflows.
type Playlist struct {
	ID          uuid.UUID `json:"id" db:"id"`
	Name        string    `json:"name" db:"name"`
	Slug        string    `json:"slug" db:"slug"`
	Description string    `json:"description" db:"description"`
	ScoringMode string    `json:"scoring_mode" db:"scoring_mode"`
	CreatedBy   uuid.UUID `json:"created_by" db:"created_by"`
	IsActive    bool      `json:"is_active" db:"is_active"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
	// Loaded via join
	Workflows []Workflow `json:"workflows,omitempty" db:"-"`
}

// PlaylistWorkflow is the join table for playlist membership.
type PlaylistWorkflow struct {
	PlaylistID     uuid.UUID `json:"playlist_id" db:"playlist_id"`
	WorkflowID     uuid.UUID `json:"workflow_id" db:"workflow_id"`
	ExecutionOrder int       `json:"execution_order" db:"execution_order"`
}

// PlaylistAccess controls which roles/users can run a playlist.
type PlaylistAccess struct {
	ID         uuid.UUID  `json:"id" db:"id"`
	PlaylistID uuid.UUID  `json:"playlist_id" db:"playlist_id"`
	Role       *string    `json:"role" db:"role"`
	UserID     *uuid.UUID `json:"user_id" db:"user_id"`
	CreatedAt  time.Time  `json:"created_at" db:"created_at"`
}

// BlueprintPlaylist links a default playlist to a blueprint.
type BlueprintPlaylist struct {
	BlueprintID uuid.UUID `json:"blueprint_id" db:"blueprint_id"`
	PlaylistID  uuid.UUID `json:"playlist_id" db:"playlist_id"`
	IsDefault   bool      `json:"is_default" db:"is_default"`
}

// Run represents one execution of a playlist against a pod.
type Run struct {
	ID              uuid.UUID  `json:"id" db:"id"`
	PodID           uuid.UUID  `json:"pod_id" db:"pod_id"`
	PlaylistID      *uuid.UUID `json:"playlist_id" db:"playlist_id"`
	TriggeredBy     uuid.UUID  `json:"triggered_by" db:"triggered_by"`
	RunnerVMID      *string    `json:"runner_vm_id" db:"runner_vm_id"`
	RunnerVMName    *string    `json:"runner_vm_name" db:"runner_vm_name"`
	CallbackToken   string     `json:"-" db:"callback_token"`
	Status          string     `json:"status" db:"status"`
	TotalWorkflows  int        `json:"total_workflows" db:"total_workflows"`
	PassedWorkflows int        `json:"passed_workflows" db:"passed_workflows"`
	FailedWorkflows int        `json:"failed_workflows" db:"failed_workflows"`
	TotalPoints     *int       `json:"total_points" db:"total_points"`
	EarnedPoints    *int       `json:"earned_points" db:"earned_points"`
	ErrorMessage    *string    `json:"error_message" db:"error_message"`
	StartedAt       *time.Time `json:"started_at" db:"started_at"`
	CompletedAt     *time.Time `json:"completed_at" db:"completed_at"`
	CreatedAt       time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at" db:"updated_at"`
	// Loaded via join
	Results []WorkflowResult `json:"results,omitempty" db:"-"`
}

// WorkflowResult is the per-workflow outcome within a run.
type WorkflowResult struct {
	ID                uuid.UUID       `json:"id" db:"id"`
	RunID             uuid.UUID       `json:"run_id" db:"run_id"`
	WorkflowID        uuid.UUID       `json:"workflow_id" db:"workflow_id"`
	WorkflowVersionID *uuid.UUID      `json:"workflow_version_id" db:"workflow_version_id"`
	ExecutionOrder    int             `json:"execution_order" db:"execution_order"`
	ExecutionMode     string          `json:"execution_mode" db:"execution_mode"`
	Status            string          `json:"status" db:"status"`
	StudentMessage    *string         `json:"student_message" db:"student_message"`
	InstructorOutput  json.RawMessage `json:"instructor_output,omitempty" db:"instructor_output"`
	ActionResults     json.RawMessage `json:"action_results,omitempty" db:"action_results"`
	PointsAwarded     *int            `json:"points_awarded" db:"points_awarded"`
	DurationMs        *int            `json:"duration_ms" db:"duration_ms"`
	StartedAt         *time.Time      `json:"started_at" db:"started_at"`
	CompletedAt       *time.Time      `json:"completed_at" db:"completed_at"`
	CreatedAt         time.Time       `json:"created_at" db:"created_at"`
	// Loaded via join
	WorkflowName string `json:"workflow_name,omitempty" db:"-"`
}
