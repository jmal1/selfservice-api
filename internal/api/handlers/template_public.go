package handlers

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TemplatePublic is the student-facing DTO for a template. It contains all
// publicly-readable template fields except DefaultPassword, which is stripped
// entirely to prevent accidental serialization.
//
// This DTO is used on all student-reachable template endpoints (e.g. GET
// /api/v1/templates) to ensure credentials never leak.
type TemplatePublic struct {
	ID              uuid.UUID       `json:"id"`
	Name            string          `json:"name"`
	VCenterTemplate string          `json:"vcenter_template"`
	OSType          string          `json:"os_type"`
	DefaultVCPUs    int             `json:"default_vcpus"`
	DefaultRAMMB    int             `json:"default_ram_mb"`
	DefaultDiskGB   int             `json:"default_disk_gb"`
	MinVCPUs        int             `json:"min_vcpus"`
	MinRAMMB        int             `json:"min_ram_mb"`
	Description     string          `json:"description"`
	IconURL         string          `json:"icon_url"`
	DefaultUsername string          `json:"default_username"`
	Kind            string          `json:"kind"`
	AssignIP        bool            `json:"assign_ip"`
	IsActive        bool            `json:"is_active"`
	IsInternal      bool            `json:"is_internal"`
	TemplateState   string          `json:"template_state"`
	CreatedBy       *uuid.UUID      `json:"created_by,omitempty"`
	VCenterVMID     string          `json:"vcenter_vm_id"`
	SourceType      string          `json:"source_type"`
	SourceRef       string          `json:"source_ref"`
	StagingNetwork  string          `json:"staging_network"`
	UnattendMode    string          `json:"unattend_mode"`
	UnattendConfig  json.RawMessage `json:"unattend_config,omitempty"`
	GuestID         string          `json:"guest_id"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// newTemplatePublic converts a models.Template to a TemplatePublic by copying
// all fields except DefaultPassword, which is omitted entirely.
func newTemplatePublic(t models.Template) TemplatePublic {
	return TemplatePublic{
		ID:              t.ID,
		Name:            t.Name,
		VCenterTemplate: t.VCenterTemplate,
		OSType:          t.OSType,
		DefaultVCPUs:    t.DefaultVCPUs,
		DefaultRAMMB:    t.DefaultRAMMB,
		DefaultDiskGB:   t.DefaultDiskGB,
		MinVCPUs:        t.MinVCPUs,
		MinRAMMB:        t.MinRAMMB,
		Description:     t.Description,
		IconURL:         t.IconURL,
		DefaultUsername: t.DefaultUsername,
		Kind:            t.Kind,
		AssignIP:        t.AssignIP,
		IsActive:        t.IsActive,
		IsInternal:      t.IsInternal,
		TemplateState:   t.TemplateState,
		CreatedBy:       t.CreatedBy,
		VCenterVMID:     t.VCenterVMID,
		SourceType:      t.SourceType,
		SourceRef:       t.SourceRef,
		StagingNetwork:  t.StagingNetwork,
		UnattendMode:    t.UnattendMode,
		UnattendConfig:  t.UnattendConfig,
		GuestID:         t.GuestID,
		CreatedAt:       t.CreatedAt,
		UpdatedAt:       t.UpdatedAt,
	}
}

// newTemplatePublicList converts a slice of models.Template to []TemplatePublic.
func newTemplatePublicList(ts []models.Template) []TemplatePublic {
	result := make([]TemplatePublic, len(ts))
	for i, t := range ts {
		result[i] = newTemplatePublic(t)
	}
	return result
}
