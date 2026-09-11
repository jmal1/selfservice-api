package models

import (
	"time"

	"github.com/google/uuid"
)

const (
	TemplateReplicaBuildPending         = "pending"
	TemplateReplicaBuildRunning         = "running"
	TemplateReplicaBuildReady           = "ready"
	TemplateReplicaBuildFailed          = "failed"
	TemplateReplicaBuildCleanupRequired = "cleanup_required"
	TemplateReplicaBuildRetiring        = "retiring"
	TemplateReplicaBuildRetired         = "retired"

	TemplateReplicaBuildPhasePending            = "pending"
	TemplateReplicaBuildPhaseCloneSubmitting    = "clone_submitting"
	TemplateReplicaBuildPhaseCloneSubmitted     = "clone_submitted"
	TemplateReplicaBuildPhaseValidating         = "validating"
	TemplateReplicaBuildPhaseSnapshotSubmitting = "snapshot_submitting"
	TemplateReplicaBuildPhaseSnapshotSubmitted  = "snapshot_submitted"
	TemplateReplicaBuildPhaseCanaryPrepared     = "canary_prepared"
	TemplateReplicaBuildPhaseCanarySubmitting   = "canary_submitting"
	TemplateReplicaBuildPhaseCanarySubmitted    = "canary_submitted"
	TemplateReplicaBuildPhaseCleanupPrepared    = "cleanup_prepared"
	TemplateReplicaBuildPhaseCleanupSubmitting  = "cleanup_submitting"
	TemplateReplicaBuildPhaseCleanupSubmitted   = "cleanup_submitted"
	TemplateReplicaBuildPhaseResiduePrepared    = "residue_prepared"
	TemplateReplicaBuildPhaseResidueSubmitting  = "residue_submitting"
	TemplateReplicaBuildPhaseResidueSubmitted   = "residue_submitted"
	TemplateReplicaBuildPhaseResidueCleaned     = "residue_cleaned"
	TemplateReplicaBuildPhaseFinalizing         = "finalizing"
	TemplateReplicaBuildPhaseReady              = "ready"
	TemplateReplicaBuildPhaseFailed             = "failed"
	TemplateReplicaBuildPhaseCleanupRequired    = "cleanup_required"
	TemplateReplicaBuildPhaseRetired            = "retired"
)

func IsTemplateReplicaBuildForwardPhase(phase string) bool {
	switch phase {
	case TemplateReplicaBuildPhasePending,
		TemplateReplicaBuildPhaseCloneSubmitting,
		TemplateReplicaBuildPhaseCloneSubmitted,
		TemplateReplicaBuildPhaseValidating,
		TemplateReplicaBuildPhaseSnapshotSubmitting,
		TemplateReplicaBuildPhaseSnapshotSubmitted,
		TemplateReplicaBuildPhaseCanaryPrepared,
		TemplateReplicaBuildPhaseCanarySubmitting,
		TemplateReplicaBuildPhaseCanarySubmitted,
		TemplateReplicaBuildPhaseCleanupPrepared,
		TemplateReplicaBuildPhaseCleanupSubmitting,
		TemplateReplicaBuildPhaseCleanupSubmitted,
		TemplateReplicaBuildPhaseFinalizing:
		return true
	default:
		return false
	}
}

// TemplateReplicaBuild is the durable control-plane record for constructing
// and accepting one retained source replica in an explicitly named vCenter
// inventory destination.
type TemplateReplicaBuild struct {
	ID                    uuid.UUID  `json:"id" db:"id"`
	TemplateID            uuid.UUID  `json:"template_id" db:"template_id"`
	SourceReplicaID       *uuid.UUID `json:"source_replica_id,omitempty" db:"source_replica_id"`
	ResultReplicaID       *uuid.UUID `json:"result_replica_id,omitempty" db:"result_replica_id"`
	JobID                 *uuid.UUID `json:"job_id,omitempty" db:"job_id"`
	IdempotencyKey        string     `json:"idempotency_key" db:"idempotency_key"`
	OperationID           string     `json:"operation_id" db:"operation_id"`
	CanaryOperationID     string     `json:"canary_operation_id" db:"canary_operation_id"`
	SourceVMMoref         string     `json:"source_vm_moref" db:"source_vm_moref"`
	SourceSnapshotName    string     `json:"source_snapshot_name" db:"source_snapshot_name"`
	SourceSnapshotMoref   string     `json:"source_snapshot_moref,omitempty" db:"source_snapshot_moref"`
	DestinationName       string     `json:"destination_name" db:"destination_name"`
	ComputeResourceType   string     `json:"compute_resource_type" db:"compute_resource_type"`
	ComputeResourceMoref  string     `json:"compute_resource_moref" db:"compute_resource_moref"`
	ComputeResourcePath   string     `json:"compute_resource_path" db:"compute_resource_path"`
	HostMoref             string     `json:"host_moref" db:"host_moref"`
	HostName              string     `json:"host_name" db:"host_name"`
	ResourcePoolMoref     string     `json:"resource_pool_moref" db:"resource_pool_moref"`
	ResourcePoolPath      string     `json:"resource_pool_path" db:"resource_pool_path"`
	DatastoreMoref        string     `json:"datastore_moref" db:"datastore_moref"`
	DatastoreName         string     `json:"datastore_name" db:"datastore_name"`
	FolderMoref           string     `json:"folder_moref" db:"folder_moref"`
	FolderPath            string     `json:"folder_path" db:"folder_path"`
	ProvisionDatastore    string     `json:"provision_datastore" db:"provision_datastore"`
	Status                string     `json:"status" db:"status"`
	Phase                 string     `json:"phase" db:"phase"`
	ResumePhase           string     `json:"resume_phase,omitempty" db:"resume_phase"`
	CloneTaskRef          string     `json:"clone_task_ref,omitempty" db:"clone_task_ref"`
	DestinationVMMoref    string     `json:"destination_vm_moref,omitempty" db:"destination_vm_moref"`
	SnapshotTaskRef       string     `json:"snapshot_task_ref,omitempty" db:"snapshot_task_ref"`
	DestinationSnapshot   string     `json:"destination_snapshot_moref,omitempty" db:"destination_snapshot_moref"`
	CanaryTaskRef         string     `json:"canary_task_ref,omitempty" db:"canary_task_ref"`
	CanaryVMMoref         string     `json:"canary_vm_moref,omitempty" db:"canary_vm_moref"`
	CleanupTaskRef        string     `json:"cleanup_task_ref,omitempty" db:"cleanup_task_ref"`
	CleanupCompletedAt    *time.Time `json:"cleanup_completed_at,omitempty" db:"cleanup_completed_at"`
	ResidueCleanupTaskRef string     `json:"residue_cleanup_task_ref,omitempty" db:"residue_cleanup_task_ref"`
	ResidueCleanedAt      *time.Time `json:"residue_cleaned_at,omitempty" db:"residue_cleaned_at"`
	LastErrorCode         string     `json:"last_error_code,omitempty" db:"last_error_code"`
	LastError             string     `json:"last_error,omitempty" db:"last_error"`
	StartedAt             *time.Time `json:"started_at,omitempty" db:"started_at"`
	SubmissionStartedAt   *time.Time `json:"submission_started_at,omitempty" db:"submission_started_at"`
	CompletedAt           *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	CreatedAt             time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at" db:"updated_at"`
}
