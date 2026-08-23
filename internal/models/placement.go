package models

import (
	"time"

	"github.com/google/uuid"
)

const (
	TemplateSourceReplicaPending   = "pending"
	TemplateSourceReplicaReady     = "ready"
	TemplateSourceReplicaUnhealthy = "unhealthy"
	TemplateSourceReplicaDisabled  = "disabled"

	VMPlacementDRSDisabled = "disabled"
	VMPlacementStandalone  = "standalone"
)

// TemplateSourceReplica identifies one real source VM for a logical template
// in one immutable vCenter compute resource.
type TemplateSourceReplica struct {
	ID                   uuid.UUID  `json:"id" db:"id"`
	TemplateID           uuid.UUID  `json:"template_id" db:"template_id"`
	SourceVMMoref        string     `json:"source_vm_moref" db:"source_vm_moref"`
	ComputeResourceType  string     `json:"compute_resource_type" db:"compute_resource_type"`
	ComputeResourceMoref string     `json:"compute_resource_moref" db:"compute_resource_moref"`
	ComputeResourcePath  string     `json:"compute_resource_path" db:"compute_resource_path"`
	Status               string     `json:"status" db:"status"`
	LastValidatedAt      *time.Time `json:"last_validated_at,omitempty" db:"last_validated_at"`
	LastValidationError  *string    `json:"last_validation_error,omitempty" db:"last_validation_error"`
	CreatedAt            time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at" db:"updated_at"`
}

// VMPlacement is the immutable source and destination decision recorded before
// a standard-switch or clone mutation.
type VMPlacement struct {
	PodVMID               uuid.UUID  `json:"pod_vm_id" db:"pod_vm_id"`
	JobID                 uuid.UUID  `json:"job_id" db:"job_id"`
	TemplateID            uuid.UUID  `json:"template_id" db:"template_id"`
	SourceReplicaID       *uuid.UUID `json:"source_replica_id,omitempty" db:"source_replica_id"`
	SourceRef             string     `json:"source_ref" db:"source_ref"`
	ComputeResourceType   string     `json:"compute_resource_type" db:"compute_resource_type"`
	ComputeResourceMoref  string     `json:"compute_resource_moref" db:"compute_resource_moref"`
	ResourcePoolMoref     string     `json:"resource_pool_moref" db:"resource_pool_moref"`
	HostMoref             string     `json:"host_moref" db:"host_moref"`
	HostName              string     `json:"host_name" db:"host_name"`
	DRSControl            string     `json:"drs_control" db:"drs_control"`
	ObservedFreeMemoryMB  int64      `json:"observed_free_memory_mb" db:"observed_free_memory_mb"`
	ReservedMemoryMB      int64      `json:"reserved_memory_mb" db:"reserved_memory_mb"`
	CapacityReservationMB int64      `json:"capacity_reservation_mb" db:"capacity_reservation_mb"`
	CapacityObservedAt    time.Time  `json:"capacity_observed_at" db:"capacity_observed_at"`
	CapacityReleasedAt    *time.Time `json:"capacity_released_at,omitempty" db:"capacity_released_at"`
	AdmittedHeadroomMB    int64      `json:"admitted_headroom_mb" db:"admitted_headroom_mb"`
	LegacyAdoptionPending bool       `json:"legacy_adoption_pending" db:"legacy_adoption_pending"`
	CreatedAt             time.Time  `json:"created_at" db:"created_at"`
}
