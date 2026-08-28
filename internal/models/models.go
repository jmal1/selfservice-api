package models

import (
	"encoding/json"
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
	Kind            string    `json:"kind" db:"kind"`
	AssignIP        bool      `json:"assign_ip" db:"assign_ip"`
	IsActive        bool      `json:"is_active" db:"is_active"`
	// IsInternal flags fixture / synthetic templates (e.g. synthetic-noop
	// used by the lifecycle monitor) so they're hidden from the public
	// /api/templates listing while still being resolvable by ID from
	// pod-create / vm-add when granted via template_access rules.
	// Admin-only /admin/templates still shows internal rows.
	// Migration 000019.
	IsInternal bool `json:"is_internal" db:"is_internal"`
	// Visibility controls whether instructors can stage a template without
	// students seeing it. Valid values: 'public' (students see it) or
	// 'instructor_only' (hidden from student list and pod-create).
	// Instructors and admins see all regardless of visibility.
	// Migration 000029.
	Visibility string `json:"visibility" db:"visibility"`
	// TemplateState drives the wizard lifecycle (migration 000018).
	// See models.TemplateState* constants and internal/templates/lifecycle.go
	// for allowed transitions. Defaults to 'active' for legacy rows.
	TemplateState  string     `json:"template_state" db:"template_state"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty" db:"created_by"`
	VCenterVMID    string     `json:"vcenter_vm_id" db:"vcenter_vm_id"`
	SourceType     string     `json:"source_type" db:"source_type"`
	SourceRef      string     `json:"source_ref" db:"source_ref"`
	StagingNetwork string     `json:"staging_network" db:"staging_network"`
	// UnattendMode drives ISO-install automation (migration 000023). Only
	// meaningful when SourceType == TemplateSourceISO. See the
	// models.UnattendMode* constants and internal/unattend.
	UnattendMode string `json:"unattend_mode" db:"unattend_mode"`
	// UnattendConfig holds mode-specific knobs (locale, timezone, extra
	// packages...) as raw JSON so the generators can evolve without a
	// migration. Never contains a password — per-template credentials live
	// in DefaultUsername/DefaultPassword.
	UnattendConfig json.RawMessage `json:"unattend_config,omitempty" db:"unattend_config"`
	// GuestID is the vSphere GuestOS identifier used when creating the
	// blank VM for an ISO install (e.g. "ubuntu64Guest"). Ignored by every
	// other source type.
	GuestID   string    `json:"guest_id" db:"guest_id"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
	// TrustTier controls periodic smoke-clone revalidation (migration 000027).
	// See TemplateTrustTier* constants. Default is "untrusted".
	TrustTier            string     `json:"trust_tier" db:"trust_tier"`
	LastValidatedAt      *time.Time `json:"last_validated_at,omitempty" db:"last_validated_at"`
	LastValidationResult *string    `json:"last_validation_result,omitempty" db:"last_validation_result"`
	// GuestCredentialsVerifiedAt is set only after a fresh smoke clone accepts
	// the generated credential. Legacy customized templates remain nil until
	// they pass verification under this contract.
	GuestCredentialsVerifiedAt *time.Time `json:"guest_credentials_verified_at,omitempty" db:"guest_credentials_verified_at"`
	// Pinning (migration 000030): instructors can pin templates to emphasize them.
	// Pinned items appear in a dedicated section above the normal list.
	Pinned   bool       `json:"pinned" db:"pinned"`
	PinOrder int        `json:"pin_order" db:"pin_order"`
	PinnedAt *time.Time `json:"pinned_at,omitempty" db:"pinned_at"`
	PinnedBy *uuid.UUID `json:"pinned_by,omitempty" db:"pinned_by"`
}

// VCenterRef returns the vCenter reference to clone FROM for this
// template, preferring VCenterVMID (a managed-object moref like
// "vm-8942", set by wizard-published templates) over VCenterTemplate
// (a friendly inventory name, the legacy path used by manually-
// registered templates). Returns "" if neither is set.
//
// Callers pass the result as CloneVMParams.TemplateName, which
// cloneVMInner resolves to a *object.VirtualMachine — see
// vcenter/client.go for moref-vs-name detection.
func (t *Template) VCenterRef() string {
	if t.VCenterVMID != "" {
		return t.VCenterVMID
	}
	return t.VCenterTemplate
}

// Template kind constants — keep in sync with the CHECK constraint in
// migration 000016_template_kind.up.sql.
const (
	// TemplateKindCloneWithCustomize is the default; provisioner clones the
	// vCenter template, runs guest customization (sysprep / cloud-init), and
	// assigns an IP from the pod VLAN.
	TemplateKindCloneWithCustomize = "clone_with_customize"

	// TemplateKindCloneNoCustomize clones the template and attaches the NIC,
	// but does NOT run guest customization. Guest is responsible for its own
	// hostname/network config. Use when the source VM is already prepared with
	// the correct settings.
	TemplateKindCloneNoCustomize = "clone_no_customize"

	// TemplateKindRegisteredExistingVM treats the named vCenter VM as the
	// canonical template; per-pod copies are always linked clones. Static
	// credentials live in default_username/default_password.
	TemplateKindRegisteredExistingVM = "registered_existing_vm"
)

// Template lifecycle state constants — keep in sync with the CHECK
// constraint in migration 000018_template_lifecycle.up.sql and the state
// machine in internal/templates/lifecycle.go.
//
// New templates start at `draft` (metadata only, no VM exists) and walk
// forward through provisioning/configuring/generalizing to `ready`, then
// publish to `active` to make them available to students. `error` is the
// terminal failure state; the operator can recover via cancel→draft.
const (
	TemplateStateDraft        = "draft"
	TemplateStateProvisioning = "provisioning"
	TemplateStateConfiguring  = "configuring"
	TemplateStateGeneralizing = "generalizing"
	TemplateStateReady        = "ready"
	// TemplateStateVerifying runs an automated smoke test (clone the
	// base-image, boot it, wait for Tools + IP, then destroy the clone)
	// as a hard gate before a template can be published. A template only
	// reaches `active` if this passes; on failure it returns to `ready`.
	TemplateStateVerifying = "verifying"
	TemplateStateActive    = "active"
	TemplateStateError     = "error"
)

// AllTemplateStates is the canonical list of valid template lifecycle
// states. Keep in lockstep with the TemplateState* constants above.
var AllTemplateStates = []string{
	TemplateStateDraft,
	TemplateStateProvisioning,
	TemplateStateConfiguring,
	TemplateStateGeneralizing,
	TemplateStateReady,
	TemplateStateVerifying,
	TemplateStateActive,
	TemplateStateError,
}

// TemplateTrustTier constants — keep in sync with the CHECK constraint
// added by migration 000027_l1_trust_tier.up.sql.
const (
	// TemplateTrustTierL1 marks a first-class template. Periodic smoke-clone
	// revalidation now covers every active clone_with_customize template because
	// the clone gate depends on a durable guest credential marker regardless of
	// trust tier.
	TemplateTrustTierL1 = "l1"

	// TemplateTrustTierDerived indicates the template inherits its quality
	// signal from a parent template. Its guest credential marker is still
	// periodically smoke-validated when the template uses clone_with_customize.
	TemplateTrustTierDerived = "derived"

	// TemplateTrustTierUntrusted is the default for all templates. Sandbox
	// clone_with_customize templates still get guest credential smoke
	// revalidation so new clones are not permanently blocked by a missing marker.
	TemplateTrustTierUntrusted = "untrusted"
)

// Template source type constants — keep in sync with the CHECK constraint
// in migration 000018_template_lifecycle.up.sql.
const (
	// TemplateSourceManual is the legacy path: the VM was created in
	// vCenter directly (or by another tool) and an admin filled in the
	// template form. No machine-readable source reference.
	TemplateSourceManual = "manual"

	// TemplateSourceCloneTemplate clones an existing published template
	// (a row in this table whose template_state='active'). source_ref
	// holds the source template's UUID.
	TemplateSourceCloneTemplate = "clone_template"

	// TemplateSourceCloneVCenter clones an arbitrary vCenter VM by MoRef.
	// source_ref holds the MoRef. Use when the source isn't already a
	// Crucible template (e.g. an instructor's hand-built reference VM).
	TemplateSourceCloneVCenter = "clone_vcenter"

	// TemplateSourceISO mounts an ISO and starts a clean install.
	// source_ref holds the ISO's datastore path.
	TemplateSourceISO = "iso"
)

// CanonicalStagingNetwork is the ONLY network a template build VM is ever
// attached to. It is the isolated VLAN 30 staging port group (present on
// every ESXi host) and is a deliberate network-segmentation control: build
// VMs run untrusted, half-configured OS images and must never touch the
// management or production networks. This is NOT operator-configurable — the
// wizard does not accept a staging_network override and the value is forced
// server-side on every draft. It matches the `staging_network` column DEFAULT
// in migration 000021_staging_network_pg_vm_lab.up.sql; keep the two in sync.
const CanonicalStagingNetwork = "PG-VM-Lab"

// Unattended-install mode constants — keep in sync with the CHECK
// constraint templates_unattend_mode_check in migration
// 000023_image_uploads.up.sql and the generators in internal/unattend.
//
// Only meaningful when source_type='iso'.
const (
	// UnattendModeManual performs no automation: the blank VM boots the
	// install ISO and an operator drives the installer over the WebMKS
	// console. ProvisionTemplate skips WaitForTools and parks the template
	// in `configuring`. This is the always-available fallback.
	UnattendModeManual = "manual"

	// UnattendModeCloudInitCIData attaches a second CD-ROM containing a
	// cloud-init NoCloud seed (volume label CIDATA, files user-data and
	// meta-data). Used for Ubuntu Server subiquity autoinstall.
	UnattendModeCloudInitCIData = "cloudinit_cidata"

	// UnattendModeDebianPreseed remasters the source install ISO to embed
	// /preseed.cfg plus the boot parameters debian-installer needs to read
	// it (it will not read a second CD unaided). Used for Kali/Debian.
	UnattendModeDebianPreseed = "debian_preseed"

	// UnattendModeWindowsAutounattend attaches a seed ISO with
	// autounattend.xml at its root, which Windows Setup auto-detects on any
	// removable media root.
	UnattendModeWindowsAutounattend = "windows_autounattend"
)

// AllUnattendModes is the canonical ordered list, for validation and for
// populating the wizard's mode selector.
var AllUnattendModes = []string{
	UnattendModeManual,
	UnattendModeCloudInitCIData,
	UnattendModeDebianPreseed,
	UnattendModeWindowsAutounattend,
}

// ValidUnattendMode reports whether s is a known unattend mode.
func ValidUnattendMode(s string) bool {
	for _, m := range AllUnattendModes {
		if m == s {
			return true
		}
	}
	return false
}

// Image upload lifecycle constants — keep in sync with the CHECK
// constraint on image_uploads.status in migration 000023.
//
// pending -> uploading -> uploaded -> importing -> imported
// with `error` reachable from any non-terminal state.
const (
	// ImageUploadPending is the row's state between creating the DB record
	// and the browser starting its first part PUT.
	ImageUploadPending = "pending"

	// ImageUploadUploading means at least one part has been presigned and
	// the client is streaming. Rows stuck here for >2h are leak candidates.
	ImageUploadUploading = "uploading"

	// ImageUploadUploaded means the multipart upload was completed and the
	// object's real size was confirmed via Stat. Ready to import.
	ImageUploadUploaded = "uploaded"

	// ImageUploadImporting means the image_import worker job is streaming
	// MinIO -> vCenter.
	ImageUploadImporting = "importing"

	// ImageUploadImported is terminal success. For ISOs datastore_path is
	// set; for OVAs vcenter_vm_id is set. The MinIO object has been
	// released at this point (stagingv01 disk is small — see migration
	// 000023 header).
	ImageUploadImported = "imported"

	// ImageUploadError is terminal failure. error_message explains why and
	// the MinIO object is deliberately retained so a retry is cheap.
	ImageUploadError = "error"
)

// Image kind constants. Derived server-side from the filename extension —
// never trusted from the client.
const (
	// ImageKindISO is installation media mounted as a CD-ROM.
	ImageKindISO = "iso"

	// ImageKindOVA is a packaged virtual appliance imported via OVF. The
	// resulting VM lands in the Templates folder and is therefore already
	// selectable through the existing clone_vcenter source type — OVA needs
	// no new template source type.
	ImageKindOVA = "ova"
)

// ImageUpload is a staged ISO/OVA on its way from the operator's browser
// into vCenter. See migration 000023_image_uploads.up.sql.
type ImageUpload struct {
	ID             uuid.UUID  `json:"id" db:"id"`
	Filename       string     `json:"filename" db:"filename"`
	Kind           string     `json:"kind" db:"kind"`
	SizeBytes      int64      `json:"size_bytes" db:"size_bytes"`
	ChecksumSHA256 string     `json:"checksum_sha256" db:"checksum_sha256"`
	ObjectKey      string     `json:"object_key" db:"object_key"`
	UploadID       string     `json:"upload_id" db:"upload_id"`
	Status         string     `json:"status" db:"status"`
	DatastorePath  string     `json:"datastore_path" db:"datastore_path"`
	VCenterVMID    string     `json:"vcenter_vm_id" db:"vcenter_vm_id"`
	ErrorMessage   string     `json:"error_message" db:"error_message"`
	UploadedBy     *uuid.UUID `json:"uploaded_by,omitempty" db:"uploaded_by"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at" db:"updated_at"`
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
	ID               uuid.UUID  `json:"id" db:"id"`
	OwnerID          uuid.UUID  `json:"owner_id" db:"owner_id"`
	Name             string     `json:"name" db:"name"`
	Salt             string     `json:"salt" db:"salt"`
	VLANID           int        `json:"vlan_id" db:"vlan_id"`
	Subnet           string     `json:"subnet" db:"subnet"`
	Status           string     `json:"status" db:"status"`
	ErrorMessage     *string    `json:"error_message,omitempty" db:"error_message"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty" db:"expires_at"`
	BlueprintID      *uuid.UUID `json:"blueprint_id,omitempty" db:"blueprint_id"`
	AllowVMAdditions bool       `json:"allow_vm_additions" db:"allow_vm_additions"`
	CreatedAt        time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at" db:"updated_at"`
	VMs              []PodVM    `json:"vms,omitempty"`
	Owner            *User      `json:"owner,omitempty"`
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
	// GuestCredentialsVerifiedAt binds credential disclosure and readiness to
	// an explicit successful guest authentication, not the VM status alone.
	GuestCredentialsVerifiedAt *time.Time `json:"guest_credentials_verified_at,omitempty" db:"guest_credentials_verified_at"`
	// GuestCredentialsVerifiedVMID binds acceptance to the exact vCenter clone.
	GuestCredentialsVerifiedVMID *string   `json:"guest_credentials_verified_vm_id,omitempty" db:"guest_credentials_verified_vm_id"`
	BootOrder                    int       `json:"boot_order" db:"boot_order"`
	CreatedAt                    time.Time `json:"created_at" db:"created_at"`
	TemplateName                 string    `json:"template_name,omitempty"`
	TemplateKind                 string    `json:"template_kind,omitempty"`
	OSType                       string    `json:"os_type,omitempty"`
	// Activity tracking — set by migration 000025.
	LastConsoleAt  *time.Time `json:"last_console_at,omitempty" db:"last_console_at"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty" db:"last_activity_at"`
	SuspendedAt    *time.Time `json:"suspended_at,omitempty" db:"suspended_at"`
	SuspendReason  *string    `json:"suspend_reason,omitempty" db:"suspend_reason"`
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
	// Pinning (migration 000030): instructors can pin blueprints to emphasize them.
	// Pinned items appear in a dedicated section above the normal list.
	Pinned   bool       `json:"pinned" db:"pinned"`
	PinOrder int        `json:"pin_order" db:"pin_order"`
	PinnedAt *time.Time `json:"pinned_at,omitempty" db:"pinned_at"`
	PinnedBy *uuid.UUID `json:"pinned_by,omitempty" db:"pinned_by"`
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
	ID          uuid.UUID       `json:"id" db:"id"`
	Type        string          `json:"type" db:"type"`
	Payload     json.RawMessage `json:"payload" db:"payload"`
	Status      string          `json:"status" db:"status"`
	ClaimedBy   *string         `json:"claimed_by,omitempty" db:"claimed_by"`
	ClaimedAt   *time.Time      `json:"claimed_at,omitempty" db:"claimed_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty" db:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty" db:"completed_at"`
	Result      json.RawMessage `json:"result,omitempty" db:"result"`
	RetryCount  int             `json:"retry_count" db:"retry_count"`
	MaxRetries  int             `json:"max_retries" db:"max_retries"`
	// NextAttemptAt, when non-nil, is the earliest time ClaimJob will
	// return this job.  Set by the worker when rescheduling a retryable
	// failure with exponential backoff.  Migration 000027.
	NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty" db:"next_attempt_at"`
	RollbackSteps json.RawMessage `json:"rollback_steps" db:"rollback_steps"`
	CreatedAt     time.Time       `json:"created_at" db:"created_at"`
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
	PodStatusPending       = "pending"
	PodStatusProvisioning  = "provisioning"
	PodStatusActive        = "active"
	PodStatusDestroying    = "destroying"
	PodStatusDestroyFailed = "destroy_failed"
	PodStatusDestroyed     = "destroyed"
	PodStatusError         = "error"

	PodErrorManualCleanupRequiredPrefix = "manual_cleanup_required:"
	PodErrorCancelledBeforeProvisioning = "pod cancelled before provisioning started"
)

// VM status constants.
const (
	VMStatusPending     = "pending"
	VMStatusCloning     = "cloning"
	VMStatusConfiguring = "configuring"
	VMStatusRunning     = "running"
	VMStatusStopped     = "stopped"
	VMStatusSuspended   = "suspended" // saved-state checkpoint, resumable via vm_start
	VMStatusError       = "error"
	VMStatusDeleted     = "deleted"
)

// Job type constants.
const (
	JobTypePodCreate        = "pod_create"
	JobTypePodDestroy       = "pod_destroy"
	JobTypeVMStart          = "vm_start"
	JobTypeVMStop           = "vm_stop"
	JobTypeVMRestart        = "vm_restart"
	JobTypeVMDestroy        = "vm_destroy"
	JobTypeVMAdd            = "vm_add"
	JobTypeVMReset          = "vm_reset"
	JobTypeVMSnapshot       = "vm_snapshot"
	JobTypeVMRevert         = "vm_revert"
	JobTypeVMSnapshotDelete = "vm_snapshot_delete"

	// Template wizard (T4) jobs. template_provision creates the staging VM
	// from a source (existing template, vCenter VM, or ISO) and powers it on.
	// template_generalize runs sysprep / cloud-init clean inside the running
	// VM via GuestOperations, then snapshots the powered-off VM as base-image.
	JobTypeTemplateProvision  = "template_provision"
	JobTypeTemplateGeneralize = "template_generalize"
	JobTypeTemplateVerify     = "template_verify"
	// JobTypeTemplateRevalidate is enqueued by the L1 trust-tier reconciler
	// for periodic smoke-clone checks on already-published templates.
	// On failure the template stays published; only the metric and
	// last_validation_result are updated (alert-only policy, migration 000027).
	JobTypeTemplateRevalidate = "template_revalidate"
	// JobTypeTemplateHealthConfirm performs a fresh deep check after a
	// separately observed deep failure. Its delayed jobs are durable and
	// idempotent across worker restart and leader failover.
	JobTypeTemplateHealthConfirm = "template_health_confirm"
	// JobTypeTemplateReplicaBuild constructs and acceptance-checks one retained
	// compute-scoped source replica without changing provisioning allowlists.
	JobTypeTemplateReplicaBuild = "template_replica_build"

	// JobTypeImageImport streams a staged ISO/OVA out of MinIO and into
	// vCenter — ISOs are uploaded to the NAS-BackupsAndISOS datastore,
	// OVAs are deployed via OVF import into the Templates folder. Payload
	// is provisioner.ImageImportPayload.
	JobTypeImageImport = "image_import"

	// JobTypeVMSuspend saves a VM's state to disk and parks it, freeing
	// cluster resources. The VM can be resumed via a normal vm_start job
	// (PowerOnVM resumes from suspend). Payload is provisioner.SuspendVMPayload.
	JobTypeVMSuspend = "vm_suspend"
)

// Job status constants.
const (
	JobStatusPending    = "pending"
	JobStatusClaimed    = "claimed"
	JobStatusInProgress = "in_progress"
	JobStatusCompleted  = "completed"
	JobStatusFailed     = "failed"
	JobStatusRollback   = "rollback"
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
