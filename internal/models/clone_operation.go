package models

import "time"

const (
	VMCloneOperationPrepared   = "prepared"
	VMCloneOperationSubmitting = "submitting"
	VMCloneOperationSubmitted  = "submitted"
)

// VMCloneOperation is the durable identity for one externally submitted
// vCenter clone. It lives in jobs.payload until the clone is adopted or its
// cleanup is durably completed.
type VMCloneOperation struct {
	OperationID string    `json:"operation_id"`
	PodID       string    `json:"pod_id"`
	PodVMID     string    `json:"pod_vm_id"`
	TargetName  string    `json:"target_name"`
	SourceRef   string    `json:"source_ref"`
	HostMoref   string    `json:"host_moref"`
	HostName    string    `json:"host_name"`
	PoolMoref   string    `json:"pool_moref"`
	TaskRef     string    `json:"task_ref,omitempty"`
	Phase       string    `json:"phase"`
	PreparedAt  time.Time `json:"prepared_at"`
}
