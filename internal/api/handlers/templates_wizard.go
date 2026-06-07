package handlers

// templates_wizard.go - T4 Template Creation Wizard handlers.
//
// These power the multi-step instructor flow (Draft → Provision →
// Configure → Generalize → Ready → Active) defined in
// future/Template-Creation-Workflow.md. All endpoints accept the
// `instructor` role in addition to `admin`; the routes file wraps
// the group with RequireRole(RoleInstructor).
//
// Lifecycle moves go through internal/templates.CanTransition first so
// the API surface mirrors the database CHECK constraint 1:1 and the
// failure mode for an illegal move is a clean 409 with a machine-readable
// "current_state" + "allowed_next_states" payload the UI can render.

import (
	stdcontext "context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/templates"
)

// CreateTemplateDraftRequest is the wizard step 1 payload.
//
// Source identifies what we're cloning from:
//   - source_type=clone_template, source_ref=<template UUID>
//   - source_type=clone_vcenter, source_ref=<vCenter VM moref>
//   - source_type=iso, source_ref=<datastore path> (Phase 2 stub)
//
// StagingNetwork defaults to "LabVMs-VLAN30" if omitted (configurable per
// site; see current/01-Network-Configuration.md). VCPUs / RAMmb / DiskGB
// override the source's hardware; zero = inherit.
type CreateTemplateDraftRequest struct {
	Name            string `json:"name"`
	OSType          string `json:"os_type"`
	Description     string `json:"description,omitempty"`
	IconURL         string `json:"icon_url,omitempty"`
	DefaultUsername string `json:"default_username,omitempty"`
	DefaultPassword string `json:"default_password,omitempty"`

	SourceType     string `json:"source_type"`
	SourceRef      string `json:"source_ref"`
	StagingNetwork string `json:"staging_network,omitempty"`

	VCPUs  int `json:"vcpus,omitempty"`
	RAMMB  int `json:"ram_mb,omitempty"`
	DiskGB int `json:"disk_gb,omitempty"`
}

// WizardStateResponse is what GET /admin/templates/:id/wizard-state returns.
// The UI uses AllowedNextStates to decide which action buttons to render.
type WizardStateResponse struct {
	TemplateID         uuid.UUID `json:"template_id"`
	TemplateState      string    `json:"template_state"`
	AllowedNextStates  []string  `json:"allowed_next_states"`
	VCenterVMID        string    `json:"vcenter_vm_id,omitempty"`
	SourceType         string    `json:"source_type,omitempty"`
	SourceRef          string    `json:"source_ref,omitempty"`
	StagingNetwork     string    `json:"staging_network,omitempty"`
}

// vmNameSlugRe matches characters that aren't safe in a vCenter VM name.
// We strip everything but [A-Za-z0-9-] and collapse runs of dashes.
var vmNameSlugRe = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// AdminCreateTemplateDraft (POST /admin/templates/draft) — wizard step 1.
//
// Creates a template row in `draft` state. No vCenter work yet; the row
// is just metadata. The instructor moves to step 2 by POSTing to
// /admin/templates/:id/provision below.
//
// Allowed for instructors and admins (lab-instructors group).
func (h *Handler) AdminCreateTemplateDraft(w http.ResponseWriter, r *http.Request) {
	var req CreateTemplateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.OSType == "" {
		http.Error(w, "name and os_type are required", http.StatusBadRequest)
		return
	}
	if req.SourceType == "" {
		http.Error(w, "source_type is required (clone_template, clone_vcenter, or iso)", http.StatusBadRequest)
		return
	}
	switch req.SourceType {
	case models.TemplateSourceCloneTemplate, models.TemplateSourceCloneVCenter, models.TemplateSourceISO:
		// ok
	default:
		http.Error(w, "source_type must be clone_template, clone_vcenter, or iso", http.StatusBadRequest)
		return
	}
	if req.SourceType != models.TemplateSourceISO && req.SourceRef == "" {
		http.Error(w, "source_ref is required for source_type "+req.SourceType, http.StatusBadRequest)
		return
	}

	stagingNetwork := req.StagingNetwork
	if stagingNetwork == "" {
		stagingNetwork = "LabVMs-VLAN30" // canonical staging port group; see vault docs
	}

	userID := middleware.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}

	// Stage 1: create the row using the existing CreateTemplate path so
	// we get the standard kind/assign_ip handling, then a follow-up
	// UPDATE moves it from the default 'active' backfill state to
	// 'draft' and records the wizard-specific fields.
	//
	// Doing this in two steps (rather than a brand-new SQL INSERT)
	// keeps the column list in queries.go as the single source of
	// truth and avoids drift between this handler and CreateTemplate.
	tmpl, err := h.db.CreateTemplate(r.Context(), models.CreateTemplateRequest{
		Name:            req.Name,
		VCenterTemplate: "", // wizard-managed; populated when provisioning completes
		OSType:          req.OSType,
		DefaultVCPUs:    req.VCPUs,
		DefaultRAMMB:    req.RAMMB,
		DefaultDiskGB:   req.DiskGB,
		MinVCPUs:        req.VCPUs,
		MinRAMMB:        req.RAMMB,
		Description:     req.Description,
		IconURL:         req.IconURL,
		DefaultUsername: req.DefaultUsername,
		DefaultPassword: req.DefaultPassword,
		Kind:            models.TemplateKindCloneWithCustomize,
	})
	if err != nil {
		h.logger.Error("create template draft failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Stage 2: flip state to draft, set source/staging metadata,
	// record creator. Done as a direct query so we don't pollute
	// UpdateTemplate (which is for user-facing edits and enforces
	// optimistic locking that doesn't apply here).
	if _, err := h.db.Pool().Exec(r.Context(), `
		UPDATE templates
		SET template_state = $2,
		    source_type = $3,
		    source_ref = $4,
		    staging_network = $5,
		    created_by = $6,
		    is_active = false
		WHERE id = $1
	`, tmpl.ID, models.TemplateStateDraft, req.SourceType, req.SourceRef, stagingNetwork, userID); err != nil {
		h.logger.Error("set draft state failed", "error", err, "template_id", tmpl.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Refetch so the response reflects all six new fields.
	fresh, _ := h.db.GetTemplateByID(r.Context(), tmpl.ID)
	if fresh == nil {
		fresh = tmpl
	}

	audit.Log(r.Context(), h.db, "template.draft.create",
		audit.Resource("template", tmpl.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("source_type", req.SourceType),
		audit.Detail("source_ref", req.SourceRef),
	)

	h.invalidateTemplatesFolderCache()
	respondJSON(w, http.StatusCreated, fresh)
}

// AdminProvisionTemplate (POST /admin/templates/:id/provision) — wizard step 2.
//
// Transitions draft → provisioning and enqueues a template_provision
// job. Returns 202 Accepted with the new job ID and updated wizard
// state.
func (h *Handler) AdminProvisionTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateDraft)
	if !ok {
		return
	}

	// Build payload from the template row. The instructor doesn't
	// override hardware here — they pick it during draft creation.
	vmName := buildTemplateVMName(tmpl.Name)
	payload := map[string]any{
		"template_id":     tmpl.ID,
		"source_type":     tmpl.SourceType,
		"source_ref":      tmpl.SourceRef,
		"vm_name":         vmName,
		"staging_network": tmpl.StagingNetwork,
		"vcpus":           tmpl.DefaultVCPUs,
		"ram_mb":          tmpl.DefaultRAMMB,
	}
	if !h.advanceTemplateAndEnqueue(w, r, tmpl, models.TemplateStateDraft, models.TemplateStateProvisioning,
		models.JobTypeTemplateProvision, payload, "template.provision") {
		return
	}
}

// AdminGeneralizeTemplate (POST /admin/templates/:id/generalize) — wizard step 4.
//
// Transitions configuring → generalizing and enqueues template_generalize.
// Body: {"guest_password":"..."}. Username defaults to template.default_username.
func (h *Handler) AdminGeneralizeTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GuestUsername string `json:"guest_username,omitempty"`
		GuestPassword string `json:"guest_password"`
	}
	if r.Body != http.NoBody {
		_ = json.NewDecoder(r.Body).Decode(&req) // body is optional; pulled from template if empty
	}

	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateConfiguring)
	if !ok {
		return
	}
	if tmpl.VCenterVMID == "" {
		http.Error(w, "template has no vCenter VM yet (provisioning never completed)", http.StatusConflict)
		return
	}

	guestUser := req.GuestUsername
	if guestUser == "" {
		guestUser = tmpl.DefaultUsername
	}
	guestPass := req.GuestPassword
	if guestPass == "" {
		guestPass = tmpl.DefaultPassword
	}
	if guestUser == "" || guestPass == "" {
		http.Error(w,
			"guest_username and guest_password are required (provide in body or set default_username/default_password on the template)",
			http.StatusBadRequest)
		return
	}

	payload := map[string]any{
		"template_id":    tmpl.ID,
		"os_type":        tmpl.OSType,
		"guest_username": guestUser,
		"guest_password": guestPass,
		"vm_moref":       tmpl.VCenterVMID,
		"snapshot_name":  "base-image",
	}
	if !h.advanceTemplateAndEnqueue(w, r, tmpl, models.TemplateStateConfiguring, models.TemplateStateGeneralizing,
		models.JobTypeTemplateGeneralize, payload, "template.generalize") {
		return
	}
}

// AdminPublishTemplate (POST /admin/templates/:id/publish) — wizard step 5.
// ready → active, flips is_active so it shows in /api/v1/templates for students.
func (h *Handler) AdminPublishTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateReady)
	if !ok {
		return
	}
	if !h.stateOnlyTransition(w, r, tmpl, models.TemplateStateReady, models.TemplateStateActive, "template.publish",
		true /* setIsActive */) {
		return
	}
}

// AdminUnpublishTemplate (POST /admin/templates/:id/unpublish).
// active → ready, hides from students but keeps the base-image snapshot.
func (h *Handler) AdminUnpublishTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateActive)
	if !ok {
		return
	}
	if !h.stateOnlyTransition(w, r, tmpl, models.TemplateStateActive, models.TemplateStateReady, "template.unpublish",
		false /* setIsActive */) {
		return
	}
}

// AdminRetryTemplate (POST /admin/templates/:id/retry) — error → draft.
// Resets a failed template back to draft so the instructor can re-run the
// wizard. Does NOT destroy the staging VM if one exists — that's a separate
// concern; the operator can hit /cancel first if they want the VM gone.
func (h *Handler) AdminRetryTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateError)
	if !ok {
		return
	}
	if !h.stateOnlyTransition(w, r, tmpl, models.TemplateStateError, models.TemplateStateDraft, "template.retry",
		false) {
		return
	}
}

// AdminCancelTemplate (POST /admin/templates/:id/cancel) — early-cancel
// from configuring or ready back to draft. Future enhancement: enqueue a
// cleanup job that destroys the staging VM; for now we just flip the
// state and surface a warning in the response that the VM is still in
// vCenter.
func (h *Handler) AdminCancelTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}
	tmpl, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil || tmpl == nil {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}

	// Both configuring and ready can cancel to draft.
	from := tmpl.TemplateState
	if from != models.TemplateStateConfiguring && from != models.TemplateStateReady {
		h.writeStateConflict(w, tmpl, fmt.Sprintf("cancel only valid from configuring or ready, got %s", from))
		return
	}
	if err := templates.CanTransition(from, models.TemplateStateDraft); err != nil {
		h.writeStateConflict(w, tmpl, err.Error())
		return
	}
	if err := h.db.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, from, models.TemplateStateDraft); err != nil {
		h.handleLifecycleUpdateErr(w, tmpl, err)
		return
	}
	audit.Log(r.Context(), h.db, "template.cancel",
		audit.Resource("template", tmpl.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("from_state", from),
	)

	fresh, _ := h.db.GetTemplateByID(r.Context(), tmpl.ID)
	resp := h.wizardState(fresh)
	resp.VCenterVMID = tmpl.VCenterVMID // surface so UI can prompt for manual cleanup if non-empty
	respondJSON(w, http.StatusOK, map[string]any{
		"state": resp,
		"warning": ifNonEmpty(tmpl.VCenterVMID,
			"staging VM "+tmpl.VCenterVMID+" still exists in vCenter; delete manually if not needed"),
	})
}

// AdminGetWizardState (GET /admin/templates/:id/wizard-state) — UI uses
// this to drive button enable/disable state and to refresh after async
// jobs complete (alternative to a WebSocket).
func (h *Handler) AdminGetWizardState(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}
	tmpl, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil || tmpl == nil {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, h.wizardState(tmpl))
}

// --- shared helpers ---

// requireTemplateInState loads the template by URL param, asserts the
// current state matches `wantState`, and emits a 409 with the canonical
// state-conflict response on mismatch.
func (h *Handler) requireTemplateInState(w http.ResponseWriter, r *http.Request, wantState string) (*models.Template, bool) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return nil, false
	}
	tmpl, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "template not found", http.StatusNotFound)
		} else {
			h.logger.Error("load template failed", "error", err, "template_id", templateID)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return nil, false
	}
	if tmpl == nil {
		http.Error(w, "template not found", http.StatusNotFound)
		return nil, false
	}
	if tmpl.TemplateState != wantState {
		h.writeStateConflict(w, tmpl, fmt.Sprintf("expected state %q, got %q", wantState, tmpl.TemplateState))
		return nil, false
	}
	return tmpl, true
}

// advanceTemplateAndEnqueue performs a CanTransition check, transitions
// the row, enqueues a worker job, and returns the wizard state. Common
// path for provision + generalize. Returns false (and writes the
// response) on any failure.
func (h *Handler) advanceTemplateAndEnqueue(w http.ResponseWriter, r *http.Request, tmpl *models.Template,
	from, to, jobType string, payload map[string]any, auditAction string) bool {
	if err := templates.CanTransition(from, to); err != nil {
		h.writeStateConflict(w, tmpl, err.Error())
		return false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("marshal job payload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if err := h.db.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, from, to); err != nil {
		h.handleLifecycleUpdateErr(w, tmpl, err)
		return false
	}
	job, err := h.db.CreateJob(r.Context(), jobType, body)
	if err != nil {
		// Best-effort rollback: try to move state back. If that fails
		// we're in an inconsistent state — surface it loud so the
		// operator hits /retry rather than retry-stuck.
		_ = h.db.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, to, from)
		h.logger.Error("enqueue job failed", "error", err, "job_type", jobType)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if h.events != nil {
		if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
		}
	}
	audit.Log(r.Context(), h.db, auditAction,
		audit.Resource("template", tmpl.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
		audit.Detail("from_state", from),
		audit.Detail("to_state", to),
	)
	fresh, _ := h.db.GetTemplateByID(r.Context(), tmpl.ID)
	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"state":  h.wizardState(fresh),
	})
	return true
}

// stateOnlyTransition handles transitions that don't need a worker job
// (publish, unpublish, retry). setIsActive=true also flips is_active to
// match the new state (used by publish).
func (h *Handler) stateOnlyTransition(w http.ResponseWriter, r *http.Request, tmpl *models.Template,
	from, to, auditAction string, setIsActive bool) bool {
	if err := templates.CanTransition(from, to); err != nil {
		h.writeStateConflict(w, tmpl, err.Error())
		return false
	}
	if err := h.db.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, from, to); err != nil {
		h.handleLifecycleUpdateErr(w, tmpl, err)
		return false
	}
	if setIsActive {
		if _, err := h.db.Pool().Exec(r.Context(),
			`UPDATE templates SET is_active = true WHERE id = $1`, tmpl.ID); err != nil {
			h.logger.Warn("flip is_active failed (state already changed)", "error", err, "template_id", tmpl.ID)
		}
	} else if to == models.TemplateStateReady && from == models.TemplateStateActive {
		// Unpublish: hide from students.
		if _, err := h.db.Pool().Exec(r.Context(),
			`UPDATE templates SET is_active = false WHERE id = $1`, tmpl.ID); err != nil {
			h.logger.Warn("clear is_active failed", "error", err, "template_id", tmpl.ID)
		}
	}
	audit.Log(r.Context(), h.db, auditAction,
		audit.Resource("template", tmpl.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("from_state", from),
		audit.Detail("to_state", to),
	)
	h.invalidateTemplatesFolderCache()
	fresh, _ := h.db.GetTemplateByID(r.Context(), tmpl.ID)
	respondJSON(w, http.StatusOK, h.wizardState(fresh))
	return true
}

// writeStateConflict emits the canonical 409 response with the current
// row state and the allowed next moves so the UI can re-render buttons
// without a follow-up GET.
func (h *Handler) writeStateConflict(w http.ResponseWriter, tmpl *models.Template, reason string) {
	allowed, _ := templates.AllowedNextStates(tmpl.TemplateState)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":               "state_conflict",
		"reason":              reason,
		"current_state":       tmpl.TemplateState,
		"allowed_next_states": allowed,
	})
}

// handleLifecycleUpdateErr translates DB errors from
// UpdateTemplateLifecycleState into appropriate HTTP responses.
func (h *Handler) handleLifecycleUpdateErr(w http.ResponseWriter, tmpl *models.Template, err error) {
	if errors.Is(err, database.ErrTemplateStale) {
		// Another worker / handler moved the state between our load and our update.
		// Re-fetch using a fresh background context — the request context may
		// already be cancelled by the time we write this response.
		fresh, _ := h.db.GetTemplateByID(stdcontext.Background(), tmpl.ID)
		if fresh == nil {
			fresh = tmpl
		}
		h.writeStateConflict(w, fresh, "template state changed since you loaded it; refresh and retry")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}
	h.logger.Error("lifecycle state update failed", "error", err, "template_id", tmpl.ID)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (h *Handler) wizardState(tmpl *models.Template) WizardStateResponse {
	if tmpl == nil {
		return WizardStateResponse{}
	}
	allowed, _ := templates.AllowedNextStates(tmpl.TemplateState)
	return WizardStateResponse{
		TemplateID:        tmpl.ID,
		TemplateState:     tmpl.TemplateState,
		AllowedNextStates: allowed,
		VCenterVMID:       tmpl.VCenterVMID,
		SourceType:        tmpl.SourceType,
		SourceRef:         tmpl.SourceRef,
		StagingNetwork:    tmpl.StagingNetwork,
	}
}

// buildTemplateVMName produces a vCenter-safe VM name from the template's
// human-readable name: lower-case, alphanumeric-or-dash, with a 6-char
// random suffix for uniqueness within the Templates folder.
func buildTemplateVMName(humanName string) string {
	slug := strings.ToLower(humanName)
	slug = vmNameSlugRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "tpl"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	return fmt.Sprintf("tpl-%s-%s", slug, hex.EncodeToString(suffix))
}

func ifNonEmpty(s, msg string) string {
	if s == "" {
		return ""
	}
	return msg
}
