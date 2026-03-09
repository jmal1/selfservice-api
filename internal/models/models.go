package models

import (
	"time"

	"github.com/google/uuid"
)

// User represents a portal user, synced from OIDC claims on login.
type User struct {
	ID          uuid.UUID `json:"id" db:"id"`
	OIDCSub     string    `json:"oidc_sub" db:"oidc_sub"`
	Username    string    `json:"username" db:"username"`
	Email       string    `json:"email" db:"email"`
	DisplayName string    `json:"display_name" db:"display_name"`
	Role        string    `json:"role" db:"role"`
	MaxVCPUs    int       `json:"max_vcpus" db:"max_vcpus"`
	MaxRAMMB    int       `json:"max_ram_mb" db:"max_ram_mb"`
	MaxPods     int       `json:"max_pods" db:"max_pods"`
	IsActive    bool      `json:"is_active" db:"is_active"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" db:"updated_at"`
}

// Template represents a VM template available for provisioning.
type Template struct {
	ID              uuid.UUID `json:"id" db:"id"`
	Name            string    `json:"name" db:"name"`
	VCenterTemplate string    `json:"vcenter_template" db:"vcenter_template"`
	OSType          string    `json:"os_type" db:"os_type"`
	DefaultVCPUs    int       `json:"default_vcpus" db:"default_vcpus"`
	DefaultRAMMB    int       `json:"default_ram_mb" db:"default_ram_mb"`
	DefaultDiskGB   int       `json:"default_disk_gb" db:"default_disk_gb"`
	MinVCPUs        int       `json:"min_vcpus" db:"min_vcpus"`
	MinRAMMB        int       `json:"min_ram_mb" db:"min_ram_mb"`
	Description     string    `json:"description" db:"description"`
	IconURL         string    `json:"icon_url" db:"icon_url"`
	DefaultUsername string    `json:"default_username" db:"default_username"`
	DefaultPassword string    `json:"default_password" db:"default_password"`
	IsActive        bool      `json:"is_active" db:"is_active"`
	CreatedAt       time.Time `json:"created_at" db:"created_at"`
}

// TemplateAccess controls which users/roles can use a template.
type TemplateAccess struct {
	ID         uuid.UUID  `json:"id" db:"id"`
	TemplateID uuid.UUID  `json:"template_id" db:"template_id"`
	UserID     *uuid.UUID `json:"user_id,omitempty" db:"user_id"`
	Role       *string    `json:"role,omitempty" db:"role"`
}

// Pod represents a student's isolated environment (1 VLAN + N VMs).
type Pod struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	OwnerID      uuid.UUID  `json:"owner_id" db:"owner_id"`
	Name         string     `json:"name" db:"name"`
	Salt         string     `json:"salt" db:"salt"`
	VLANID       int        `json:"vlan_id" db:"vlan_id"`
	Subnet       string     `json:"subnet" db:"subnet"`
	Status       string     `json:"status" db:"status"`
	ErrorMessage *string    `json:"error_message,omitempty" db:"error_message"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	BlueprintID      *uuid.UUID `json:"blueprint_id,omitempty" db:"blueprint_id"`
	AllowVMAdditions bool       `json:"allow_vm_additions" db:"allow_vm_additions"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at" db:"updated_at"`
	VMs          []PodVM    `json:"vms,omitempty"`
	Owner        *User      `json:"owner,omitempty"`
}

// PodVM represents a virtual machine within a pod.
type PodVM struct {
	ID                uuid.UUID `json:"id" db:"id"`
	PodID             uuid.UUID `json:"pod_id" db:"pod_id"`
	TemplateID        uuid.UUID `json:"template_id" db:"template_id"`
	DisplayName       string    `json:"display_name" db:"display_name"`
	VCenterVMName     *string   `json:"vcenter_vm_name,omitempty" db:"vcenter_vm_name"`
	VCenterVMID       *string   `json:"vcenter_vm_id,omitempty" db:"vcenter_vm_id"`
	VCPUs             int       `json:"vcpus" db:"vcpus"`
	RAMMB             int       `json:"ram_mb" db:"ram_mb"`
	DiskGB            int       `json:"disk_gb" db:"disk_gb"`
	IPAddress         *string   `json:"ip_address,omitempty" db:"ip_address"`
	Status            string    `json:"status" db:"status"`
	DefaultUsername   string    `json:"default_username" db:"default_username"`
	DefaultPassword   string    `json:"default_password" db:"default_password"`
	GeneratedUsername string    `json:"generated_username" db:"generated_username"`
	GeneratedPassword string    `json:"generated_password" db:"generated_password"`
	BootOrder         int       `json:"boot_order" db:"boot_order"`
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	TemplateName      string    `json:"template_name,omitempty"`
	OSType            string    `json:"os_type,omitempty"`
}

// VMSnapshot represents a point-in-time snapshot of a VM.
type VMSnapshot struct {
	ID                uuid.UUID `json:"id"`
	PodVMID           uuid.UUID `json:"pod_vm_id"`
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	VCenterSnapshotID string    `json:"vcenter_snapshot_id"`
	IsInitial         bool      `json:"is_initial"`
	CreatedAt         time.Time `json:"created_at"`
}

// PodAttestation records each pod lifetime extension for audit trail.
type PodAttestation struct {
	ID                uuid.UUID  `json:"id" db:"id"`
	PodID             uuid.UUID  `json:"pod_id" db:"pod_id"`
	UserID            uuid.UUID  `json:"user_id" db:"user_id"`
	PreviousExpiresAt *time.Time `json:"previous_expires_at,omitempty" db:"previous_expires_at"`
	NewExpiresAt      time.Time  `json:"new_expires_at" db:"new_expires_at"`
	CreatedAt         time.Time  `json:"created_at" db:"created_at"`
}

// MaxUserSnapshots is the maximum number of user-created snapshots per VM.
const MaxUserSnapshots = 2

// Blueprint is a reusable pod template created by instructors/admins.
type Blueprint struct {
	ID               uuid.UUID     `json:"id" db:"id"`
	Name             string        `json:"name" db:"name"`
	Description      string        `json:"description" db:"description"`
	CreatedBy        uuid.UUID     `json:"created_by" db:"created_by"`
	AllowVMAdditions bool          `json:"allow_vm_additions" db:"allow_vm_additions"`
	IsActive         bool          `json:"is_active" db:"is_active"`
	CreatedAt        time.Time     `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at" db:"updated_at"`
	VMs              []BlueprintVM `json:"vms,omitempty"`
	Creator          *User         `json:"creator,omitempty"`
}

// BlueprintVM defines a VM within a blueprint.
type BlueprintVM struct {
	ID           uuid.UUID `json:"id" db:"id"`
	BlueprintID  uuid.UUID `json:"blueprint_id" db:"blueprint_id"`
	TemplateID   uuid.UUID `json:"template_id" db:"template_id"`
	DisplayName  string    `json:"display_name" db:"display_name"`
	VCPUs        *int      `json:"vcpus,omitempty" db:"vcpus"`
	RAMMB        *int      `json:"ram_mb,omitempty" db:"ram_mb"`
	DiskGB       *int      `json:"disk_gb,omitempty" db:"disk_gb"`
	BootOrder    int       `json:"boot_order" db:"boot_order"`
	Quantity     int       `json:"quantity" db:"quantity"`
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	TemplateName string    `json:"template_name,omitempty"`
}

// BlueprintAccess controls which users/roles can deploy a blueprint.
type BlueprintAccess struct {
	ID          uuid.UUID  `json:"id" db:"id"`
	BlueprintID uuid.UUID  `json:"blueprint_id" db:"blueprint_id"`
	UserID      *uuid.UUID `json:"user_id,omitempty" db:"user_id"`
	Role        *string    `json:"role,omitempty" db:"role"`
}

// Job represents a durable task in the job queue.
type Job struct {
	ID            uuid.UUID  `json:"id" db:"id"`
	Type          string     `json:"type" db:"type"`
	Payload       []byte     `json:"payload" db:"payload"`
	Status        string     `json:"status" db:"status"`
	ClaimedBy     *string    `json:"claimed_by,omitempty" db:"claimed_by"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty" db:"claimed_at"`
	StartedAt     *time.Time `json:"started_at,omitempty" db:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty" db:"completed_at"`
	Result        []byte     `json:"result,omitempty" db:"result"`
	RetryCount    int        `json:"retry_count" db:"retry_count"`
	MaxRetries    int        `json:"max_retries" db:"max_retries"`
	RollbackSteps []byte     `json:"rollback_steps" db:"rollback_steps"`
	CreatedAt     time.Time  `json:"created_at" db:"created_at"`
}

// AuditLog records user actions for compliance and debugging.
type AuditLog struct {
	ID              int64      `json:"id" db:"id"`
	UserID          *uuid.UUID `json:"user_id,omitempty" db:"user_id"`
	UserDisplayName *string    `json:"user_display_name,omitempty" db:"user_display_name"`
	UserEmail       *string    `json:"user_email,omitempty" db:"user_email"`
	Action          string     `json:"action" db:"action"`
	ResourceType    *string    `json:"resource_type,omitempty" db:"resource_type"`
	ResourceID      *uuid.UUID `json:"resource_id,omitempty" db:"resource_id"`
	Details         []byte     `json:"details,omitempty" db:"details"`
	IPAddress       *string    `json:"ip_address,omitempty" db:"ip_address"`
	CreatedAt       time.Time  `json:"created_at" db:"created_at"`
}

// VLANPoolEntry represents a VLAN in the allocation pool.
type VLANPoolEntry struct {
	ID          int        `json:"id" db:"id"`
	VLANTag     int        `json:"vlan_tag" db:"vlan_tag"`
	Subnet      string     `json:"subnet" db:"subnet"`
	HostScope   string     `json:"host_scope" db:"host_scope"`
	PodID       *uuid.UUID `json:"pod_id,omitempty" db:"pod_id"`
	AllocatedAt *time.Time `json:"allocated_at,omitempty" db:"allocated_at"`
}

// Pod status constants.
const (
	PodStatusPending        = "pending"
	PodStatusProvisioning   = "provisioning"
	PodStatusActive         = "active"
	PodStatusDestroying     = "destroying"
	PodStatusDestroyFailed  = "destroy_failed"
	PodStatusDestroyed      = "destroyed"
	PodStatusError          = "error"
)

// VM status constants.
const (
	VMStatusPending     = "pending"
	VMStatusCloning     = "cloning"
	VMStatusConfiguring = "configuring"
	VMStatusRunning     = "running"
	VMStatusStopped     = "stopped"
	VMStatusError       = "error"
	VMStatusDeleted     = "deleted"
)

// Job type constants.
const (
	JobTypePodCreate  = "pod_create"
	JobTypePodDestroy = "pod_destroy"
	JobTypeVMStart    = "vm_start"
	JobTypeVMStop     = "vm_stop"
	JobTypeVMRestart  = "vm_restart"
	JobTypeVMDestroy        = "vm_destroy"
	JobTypeVMAdd            = "vm_add"
	JobTypeVMReset          = "vm_reset"
	JobTypeVMSnapshot       = "vm_snapshot"
	JobTypeVMRevert         = "vm_revert"
	JobTypeVMSnapshotDelete = "vm_snapshot_delete"
)

// Job status constants.
const (
	JobStatusPending   = "pending"
	JobStatusClaimed   = "claimed"
	JobStatusInProgress = "in_progress"
	JobStatusCompleted = "completed"
	JobStatusFailed    = "failed"
	JobStatusRollback  = "rollback"
)

// User role constants.
const (
	RoleStudent    = "student"
	RoleInstructor = "instructor"
	RoleAdmin      = "admin"
)

// Default quotas per role.
var DefaultQuotas = map[string]struct {
	MaxVCPUs int
	MaxRAMMB int
	MaxPods  int
}{
	RoleStudent:    {MaxVCPUs: 4, MaxRAMMB: 8192, MaxPods: 2},
	RoleInstructor: {MaxVCPUs: 20, MaxRAMMB: 40960, MaxPods: 10},
	RoleAdmin:      {MaxVCPUs: 999, MaxRAMMB: 999999, MaxPods: 999},
}
