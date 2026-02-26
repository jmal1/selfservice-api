package models

import "github.com/google/uuid"

// CreatePodRequest is the API request to create a new pod.
type CreatePodRequest struct {
	Name string       `json:"name" validate:"required,min=1,max=64"`
	VMs  []VMRequest  `json:"vms" validate:"required,min=1,max=10,dive"`
}

// VMRequest describes a VM to create within a pod.
type VMRequest struct {
	TemplateID  uuid.UUID `json:"template_id" validate:"required"`
	DisplayName string    `json:"display_name" validate:"required,min=1,max=64"`
	VCPUs       *int      `json:"vcpus,omitempty" validate:"omitempty,min=1,max=16"`
	RAMMB       *int      `json:"ram_mb,omitempty" validate:"omitempty,min=512,max=65536"`
	DiskGB      *int      `json:"disk_gb,omitempty" validate:"omitempty,min=10,max=500"`
}

// AddVMRequest is the API request to add a VM to an existing pod.
type AddVMRequest struct {
	TemplateID  uuid.UUID `json:"template_id" validate:"required"`
	DisplayName string    `json:"display_name" validate:"required,min=1,max=64"`
	VCPUs       *int      `json:"vcpus,omitempty" validate:"omitempty,min=1,max=16"`
	RAMMB       *int      `json:"ram_mb,omitempty" validate:"omitempty,min=512,max=65536"`
	DiskGB      *int      `json:"disk_gb,omitempty" validate:"omitempty,min=10,max=500"`
}

// CreateTemplateRequest is the admin API request to create a template.
type CreateTemplateRequest struct {
	Name            string `json:"name" validate:"required,min=1,max=128"`
	VCenterTemplate string `json:"vcenter_template" validate:"required"`
	OSType          string `json:"os_type" validate:"required,oneof=linux windows"`
	DefaultVCPUs    int    `json:"default_vcpus" validate:"required,min=1,max=16"`
	DefaultRAMMB    int    `json:"default_ram_mb" validate:"required,min=512,max=65536"`
	DefaultDiskGB   int    `json:"default_disk_gb" validate:"required,min=10,max=500"`
	MinVCPUs        int    `json:"min_vcpus" validate:"required,min=1"`
	MinRAMMB        int    `json:"min_ram_mb" validate:"required,min=512"`
	Description     string `json:"description,omitempty"`
	IconURL         string `json:"icon_url,omitempty"`
}

// UpdateTemplateRequest is the admin API request to update a template.
type UpdateTemplateRequest struct {
	Name         *string `json:"name,omitempty" validate:"omitempty,min=1,max=128"`
	Description  *string `json:"description,omitempty"`
	IconURL      *string `json:"icon_url,omitempty"`
	DefaultVCPUs *int    `json:"default_vcpus,omitempty" validate:"omitempty,min=1,max=16"`
	DefaultRAMMB *int    `json:"default_ram_mb,omitempty" validate:"omitempty,min=512,max=65536"`
	DefaultDiskGB *int   `json:"default_disk_gb,omitempty" validate:"omitempty,min=10,max=500"`
	IsActive     *bool   `json:"is_active,omitempty"`
}

// UpdateQuotaRequest is the admin API request to update user quotas.
type UpdateQuotaRequest struct {
	MaxVCPUs *int `json:"max_vcpus,omitempty" validate:"omitempty,min=1"`
	MaxRAMMB *int `json:"max_ram_mb,omitempty" validate:"omitempty,min=512"`
	MaxPods  *int `json:"max_pods,omitempty" validate:"omitempty,min=1"`
}

// SetTemplateAccessRequest sets access rules for a template.
type SetTemplateAccessRequest struct {
	Rules []AccessRule `json:"rules" validate:"required,dive"`
}

// AccessRule defines who can access a template.
type AccessRule struct {
	UserID *uuid.UUID `json:"user_id,omitempty"`
	Role   *string    `json:"role,omitempty" validate:"omitempty,oneof=student instructor admin"`
}

// AddVLANRequest is the admin API request to add VLANs to the pool.
type AddVLANRequest struct {
	VLANTag   int    `json:"vlan_tag" validate:"required,min=1,max=4094"`
	Subnet    string `json:"subnet" validate:"required"`
	HostScope string `json:"host_scope" validate:"required,oneof=all switch1"`
}

// UpdateVLANRequest is the admin API request to update a VLAN pool entry.
type UpdateVLANRequest struct {
	HostScope *string `json:"host_scope,omitempty" validate:"omitempty,oneof=all switch1"`
}

// PodResponse extends Pod with computed fields for API responses.
type PodResponse struct {
	Pod
	OwnerUsername string `json:"owner_username,omitempty"`
}

// JobStatusResponse is returned when polling job status.
type JobStatusResponse struct {
	ID        uuid.UUID `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	CreatedAt string    `json:"created_at"`
	Result    any       `json:"result,omitempty"`
}

// ResourceUsage shows a user's current resource consumption.
type ResourceUsage struct {
	UsedVCPUs  int `json:"used_vcpus"`
	UsedRAMMB  int `json:"used_ram_mb"`
	ActivePods int `json:"active_pods"`
	MaxVCPUs   int `json:"max_vcpus"`
	MaxRAMMB   int `json:"max_ram_mb"`
	MaxPods    int `json:"max_pods"`
}

// MeResponse is returned by GET /auth/me.
type MeResponse struct {
	User          User          `json:"user"`
	ResourceUsage ResourceUsage `json:"resource_usage"`
}
