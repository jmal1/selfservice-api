package models

import (
	"time"

	"github.com/google/uuid"
)

// Briefing owner kinds. A Single VM template is an ordinary template owner.
const (
	BriefingOwnerTemplate  = "template"
	BriefingOwnerBlueprint = "blueprint"
)

// BriefingMaxBodyBytes matches the CHECK on briefing_revisions.body_md.
const BriefingMaxBodyBytes = 262144

// BriefingMeta is the instructor view of one template's or blueprint's
// briefing. HeadRevision is 0 when nothing has been saved yet.
type BriefingMeta struct {
	OwnerKind         string     `json:"owner_kind"`
	OwnerID           uuid.UUID  `json:"owner_id"`
	HeadRevision      int        `json:"head_revision"`
	PublishedRevision *int       `json:"published_revision"`
	UpdatedAt         *time.Time `json:"updated_at"`
}

// BriefingRevision is one immutable save. BodyMD is omitted from history lists.
type BriefingRevision struct {
	Revision     int        `json:"revision"`
	BodyMD       string     `json:"body_md,omitempty"`
	BodySHA256   string     `json:"body_sha256"`
	Message      string     `json:"message"`
	AuthorID     *uuid.UUID `json:"author_id"`
	Author       string     `json:"author"`
	RestoredFrom *int       `json:"restored_from"`
	IsPublished  bool       `json:"is_published"`
	CreatedAt    time.Time  `json:"created_at"`
}

// BriefingSection is one published briefing shown on a pod. Scope is
// "blueprint" or "template"; the template fields are set only for templates.
type BriefingSection struct {
	Scope        string     `json:"scope"`
	Title        string     `json:"title"`
	TemplateID   *uuid.UUID `json:"template_id,omitempty"`
	TemplateName string     `json:"template_name,omitempty"`
	VMNames      []string   `json:"vm_names,omitempty"`
	BodyMD       string     `json:"body_md"`
	Revision     int        `json:"revision"`
	UpdatedAt    time.Time  `json:"updated_at"`
}
