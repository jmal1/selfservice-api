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
	"bytes"
	stdcontext "context"
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
	"github.com/jmal1/selfservice-api/internal/unattend"
)

// CreateTemplateDraftRequest is the wizard step 1 payload.
//
// Source identifies what we're cloning from:
//   - source_type=clone_template, source_ref=<template UUID>
//   - source_type=clone_vcenter, source_ref=<vCenter VM moref>
//   - source_type=iso, source_ref=<datastore path> (Phase 2 stub)
//
// The staging network is NOT part of this payload: every build VM is forced
// onto models.CanonicalStagingNetwork server-side as a non-negotiable network
// isolation control (see AdminCreateTemplateDraft). VCPUs / RAMmb / DiskGB
// override the source's hardware; zero = inherit.
type CreateTemplateDraftRequest struct {
	Name            string `json:"name"`
	OSType          string `json:"os_type"`
	Description     string `json:"description,omitempty"`
	IconURL         string `json:"icon_url,omitempty"`
	DefaultUsername string `json:"default_username,omitempty"`
	DefaultPassword string `json:"default_password,omitempty"`

	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`

	VCPUs  int `json:"vcpus,omitempty"`
	RAMMB  int `json:"ram_mb,omitempty"`
	DiskGB int `json:"disk_gb,omitempty"`

	// ISO-only fields. GuestID is the vSphere guest OS identifier (e.g.
	// "ubuntu64Guest"). UnattendMode/UnattendConfig drive automated install;
	// omit or leave empty for a manual console install.
	GuestID        string          `json:"guest_id,omitempty"`
	UnattendMode   string          `json:"unattend_mode,omitempty"`
	UnattendConfig json.RawMessage `json:"unattend_config,omitempty"`
}

// ResolvedCredentials is the wire type for
// GET /admin/templates/{id}/resolved-credentials.
//
// SECURITY: the raw password is NEVER included. HasPassword tells the UI
// whether a password exists without exposing its value.
//
// Source indicates which rung of the resolution ladder provided the
// credentials so the UI can show a context-aware message:
//   - "template"        — default_username / default_password on the row
//   - "unattend_config" — credentials parsed from the ISO build's unattend_config
//   - "none"            — no complete pair was found; wizard must prompt
//
// ("request" is also a valid source but is only returned by the shared
// helper when an explicit override is passed — it never appears on the
// GET endpoint response.)
type ResolvedCredentials struct {
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
	Source      string `json:"source"`
}

// WizardStateResponse is what GET /admin/templates/:id/wizard-state returns.
// The UI uses AllowedNextStates to decide which action buttons to render.
//
// When TemplateState == "error", LastJobType and LastJobError tell the UI
// which step failed (template_provision vs template_generalize) and what
// the worker reported, so it can mark the right step as errored and show
// the actual error instead of "check worker logs".
type WizardStateResponse struct {
	TemplateID        uuid.UUID `json:"template_id"`
	TemplateState     string    `json:"template_state"`
	AllowedNextStates []string  `json:"allowed_next_states"`
	VCenterVMID       string    `json:"vcenter_vm_id,omitempty"`
	SourceType        string    `json:"source_type,omitempty"`
	SourceRef         string    `json:"source_ref,omitempty"`
	StagingNetwork    string    `json:"staging_network,omitempty"`
	LastJobType       string    `json:"last_job_type,omitempty"`
	LastJobStatus     string    `json:"last_job_status,omitempty"`
	LastJobError      string    `json:"last_job_error,omitempty"`

	// Build-VM access fields (Phase H). Populated for the wizard's
	// `provisioning` / `configuring` / `generalizing` states so the
	// instructor can SSH/RDP into the staging VM and see the bootstrap
	// credentials if the OS locks them out. These are NOT secret data —
	// they're the same defaults the template generalize step uses, and
	// the wizard endpoint is already gated to template-owner + admin.
	//
	// BuildVMName + BuildVMIP come from a non-blocking vCenter
	// property collector call; either may be empty while the guest
	// boots or before VMware Tools reports an IP.
	OSType          string `json:"os_type,omitempty"`
	TemplateKind    string `json:"template_kind,omitempty"`
	AssignIP        bool   `json:"assign_ip"`
	BuildVMName     string `json:"build_vm_name,omitempty"`
	BuildVMIP       string `json:"build_vm_ip,omitempty"`
	BuildVMPowerOn  bool   `json:"build_vm_power_on,omitempty"`
	BuildVMTools    bool   `json:"build_vm_tools_running,omitempty"`
	DefaultUsername string `json:"default_username,omitempty"`
	DefaultPassword string `json:"default_password,omitempty"`
	// UnattendMode surfaces the ISO automation family so the UI can tell
	// the operator whether a hands-off or a manual console install is
	// expected. Only meaningful when SourceType == iso.
	UnattendMode string `json:"unattend_mode,omitempty"`
	// ConfiguringHint is set for ISO templates in the `configuring` state
	// to tell the operator what to expect (manual console vs. automated).
	ConfiguringHint string `json:"configuring_hint,omitempty"`
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
	if req.SourceRef == "" {
		switch req.SourceType {
		case models.TemplateSourceISO:
			http.Error(w,
				`source_ref is required for iso: provide a datastore path, e.g. [NAS-BackupsAndISOS] ISOs/kali.iso`,
				http.StatusBadRequest)
		default:
			http.Error(w, "source_ref is required for source_type "+req.SourceType, http.StatusBadRequest)
		}
		return
	}
	if err := validateUnattendMode(req.UnattendMode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Guest OS ID is only meaningful for an ISO build (the clone paths inherit
	// the source VM's guest OS). Requiring and shape-checking it here turns the
	// opaque provision-time "guest ID required" vCenter fault into a clear,
	// early 400 — the failure an instructor hit following the Mint recipe when
	// the wizard shipped no way to set it. Validation is deliberately wide (see
	// models.ValidGuestID): any catalog OS or any "<name>Guest"-shaped custom
	// identifier is accepted, so uncommon or future OSes still work.
	if req.SourceType == models.TemplateSourceISO {
		req.GuestID = strings.TrimSpace(req.GuestID)
		if req.GuestID == "" {
			http.Error(w,
				`guest_id is required for an ISO build — pick a Guest OS in the wizard (e.g. "ubuntu64Guest" for Ubuntu/Mint, "debian12_64Guest" for Debian, "windows11_64Guest" for Windows 11). See GET /admin/templates/guest-os-catalog for the full list.`,
				http.StatusBadRequest)
			return
		}
		if !models.ValidGuestID(req.GuestID) {
			http.Error(w,
				fmt.Sprintf(`guest_id %q is not a valid vSphere guest OS identifier; it must look like "<name>Guest" (e.g. "ubuntu64Guest", "debian12_64Guest", "windows11_64Guest"). See GET /admin/templates/guest-os-catalog for the full list.`, req.GuestID),
				http.StatusBadRequest)
			return
		}
	}
	unattendConfig, err := normalizeUnattendConfig(req.UnattendConfig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	unattendMode := req.UnattendMode
	if unattendMode == "" {
		unattendMode = models.UnattendModeManual
	}

	// Network isolation control: build VMs ALWAYS land on the canonical
	// VLAN 30 staging port group, never on an operator-supplied network.
	// The request has no staging_network field; there is nothing to honor
	// or fall back from.
	stagingNetwork := models.CanonicalStagingNetwork

	// For clone_template: inherit missing credentials from the source
	// template so that a template-from-template workflow never silently
	// produces a draft with an empty username. An explicitly supplied
	// value in the request always wins over the inherited one.
	//
	// This block runs before the auth check (it is pure request
	// validation + a single optional DB read) so that the guard can be
	// exercised in unit tests without an auth context. The h.db nil
	// guard makes those tests safe when the handler is constructed
	// without a database (test-only path).
	if req.SourceType == models.TemplateSourceCloneTemplate {
		if (req.DefaultUsername == "" || req.DefaultPassword == "") && h.db != nil {
			if srcID, parseErr := uuid.Parse(req.SourceRef); parseErr == nil {
				if srcTmpl, _ := h.db.GetTemplateByID(r.Context(), srcID); srcTmpl != nil {
					inheritSourceTemplateCredentials(&req, srcTmpl)
				}
			}
		}
		// Guard: a clone_template draft that still has an empty
		// default_username will produce a pod nobody can log into —
		// the same silent defect ValidateLinuxTemplateContract catches
		// at publish time, but caught here instead so it never reaches
		// the publish gate. iso is exempt (credentials arrive via
		// unattend_config during the configuring phase); clone_vcenter
		// is exempt (the author sets them during configuring). Only
		// clone_template can be checked eagerly because we have the
		// source row.
		if req.DefaultUsername == "" {
			http.Error(w,
				"clone_template draft requires default_username: the source template has no "+
					"default_username, so the derived template would be unloggable. "+
					"Set default_username in the request or update the source template first.",
				http.StatusBadRequest)
			return
		}
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
		    is_active = false,
		    guest_id = $7,
		    unattend_mode = $8,
		    unattend_config = $9
		WHERE id = $1
	`, tmpl.ID, models.TemplateStateDraft, req.SourceType, req.SourceRef, stagingNetwork, userID,
		req.GuestID, unattendMode, unattendConfig); err != nil {
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
//
// Preflight gate: before enqueuing, runs all 11 preflight checks. If any
// block-severity check fails, returns 409 with the full result list.
// Warnings never block. An admin may set override_preflight_blocks=true in
// the request body to bypass a blocking failure (logged + audited).
func (h *Handler) AdminProvisionTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateDraft)
	if !ok {
		return
	}

	// Parse optional body for the admin override flag.
	var req ProvisionWithPreflightRequest
	if r.Body != nil && r.Body != http.NoBody {
		_ = json.NewDecoder(r.Body).Decode(&req) // body is optional; ignore decode errors
	}

	// Build the target VM name up front so PF-09 (name-free check) can
	// verify it before the job is enqueued.
	vmName := buildTemplateVMName(tmpl.Name, tmpl.ID)

	// Preflight gate. If vcPreflight is nil (not configured), the gate is a
	// no-op and provisioning proceeds unchanged.
	if _, blocked := h.runPreflightGate(w, r, tmpl, vmName, req.OverridePreflightBlocks); blocked {
		return
	}

	payload := buildProvisionPayload(tmpl, vmName)
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

	guestUser, guestPass, _ := resolveGuestCredentials(tmpl, req.GuestUsername, req.GuestPassword)
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

// linuxContractPublishError decides whether a template may be published, and
// builds the operator-facing message when it may not.
//
// Split out from AdminPublishTemplate so the decision and its wording are
// unit-testable: the handler itself needs a live database to reach this
// point, which would otherwise make the gate untestable and therefore easy
// to silently delete. Returns blocked=false for every non-Linux template.
// unattendCredentials recovers the guest account an unattended ISO install
// created, from the template's unattend_config.
//
// For a cloudinit_cidata / autounattend build these ARE the real credentials:
// the platform generated them, rendered them into the seed ISO, and the
// installer created that account inside the guest. Nothing else in the system
// knows them -- they are deliberately not copied to default_password, because
// that column is the per-clone student credential rather than the build one.
//
// Returns ("", "") for anything it cannot parse; callers treat that as "no
// fallback available" and fall through to the normal required-field error.
func unattendCredentials(tmpl *models.Template) (string, string) {
	if len(tmpl.UnattendConfig) == 0 {
		return "", ""
	}
	var spec struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(tmpl.UnattendConfig, &spec); err != nil {
		return "", ""
	}
	return spec.Username, spec.Password
}

// resolveGuestCredentials implements the credential resolution ladder shared
// by AdminGeneralizeTemplate and AdminGetResolvedCredentials. Priority:
//
//  1. reqUsername / reqPassword (explicit operator override) → source "request"
//  2. tmpl.DefaultUsername / DefaultPassword → source "template"
//  3. Credentials from tmpl.UnattendConfig → source "unattend_config"
//
// Returns source "none" when no complete username+password pair is available.
// A partial pair (username without password or vice versa) is treated as
// "none" — a half-authenticated session is as unusable as no credentials.
func resolveGuestCredentials(tmpl *models.Template, reqUsername, reqPassword string) (username, password, source string) {
	fromReq := reqUsername != "" || reqPassword != ""

	username = reqUsername
	password = reqPassword

	// Rung 2: template row.
	if username == "" {
		username = tmpl.DefaultUsername
	}
	if password == "" {
		password = tmpl.DefaultPassword
	}

	// Rung 3: unattend_config (ISO builds store the generated account here).
	usedUnattend := false
	if username == "" || password == "" {
		if u, p := unattendCredentials(tmpl); u != "" && p != "" {
			if username == "" {
				username = u
				usedUnattend = true
			}
			if password == "" {
				password = p
				usedUnattend = true
			}
		}
	}

	if username == "" || password == "" {
		return "", "", "none"
	}
	switch {
	case usedUnattend:
		source = "unattend_config"
	case fromReq:
		source = "request"
	default:
		source = "template"
	}
	return
}

// inheritSourceTemplateCredentials copies default_username and default_password
// from src into req when those fields were omitted in the wizard draft request.
// An explicitly supplied value in req always wins. src == nil is a no-op.
func inheritSourceTemplateCredentials(req *CreateTemplateDraftRequest, src *models.Template) {
	if src == nil {
		return
	}
	if req.DefaultUsername == "" {
		req.DefaultUsername = src.DefaultUsername
	}
	if req.DefaultPassword == "" {
		req.DefaultPassword = src.DefaultPassword
	}
}

// AdminGetResolvedCredentials (GET /admin/templates/:id/resolved-credentials)
// tells the wizard UI whether a complete credential pair can be resolved for
// the generalize step — and from where — without exposing the password value.
//
// The resolution follows the same ladder as AdminGeneralizeTemplate so they
// cannot drift apart. Source is one of "template", "unattend_config", or
// "none" on this read-only endpoint (the "request" source only appears when
// an explicit override is sent to the generalize endpoint itself).
func (h *Handler) AdminGetResolvedCredentials(w http.ResponseWriter, r *http.Request) {
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
	resolvedUser, resolvedPass, source := resolveGuestCredentials(tmpl, "", "")
	respondJSON(w, http.StatusOK, ResolvedCredentials{
		Username:    resolvedUser,
		HasPassword: resolvedPass != "",
		Source:      source,
	})
}

func credentialContractPublishError(tmpl *models.Template) (string, bool) {
	violations := templates.ValidateTemplateCredentialContract(tmpl)
	if len(violations) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(violations))
	for _, v := range violations {
		parts = append(parts, fmt.Sprintf("%s: %s (fix: %s)", v.Field, v.Problem, v.Fix))
	}
	return "template would publish unusable student credentials, so the student would be unable to log in — " +
		strings.Join(parts, "; "), true
}

// AdminPublishTemplate (POST /admin/templates/:id/publish) — wizard step 5.
//
// Two gates run here, and they catch different failures:
//
//   - The publish credential contract (synchronous, below) catches a template
//     that boots fine but would hand the student unusable credentials.
//   - The hard smoke gate (asynchronous) means this no longer flips the
//     template live directly. It transitions ready → verifying and enqueues a
//     template_verify job that clones the base-image, boots it, and only
//     promotes the template to `active` (setting is_active) if that succeeds.
//     On smoke failure the template returns to `ready`.
//
// Together they guarantee no student ever clones a template that was never
// proven to boot, or that boots but rejects the credentials it hands out.
func (h *Handler) AdminPublishTemplate(w http.ResponseWriter, r *http.Request) {
	tmpl, ok := h.requireTemplateInState(w, r, models.TemplateStateReady)
	if !ok {
		return
	}
	if tmpl.VCenterVMID == "" {
		http.Error(w, "template has no vCenter VM / base-image to verify (generalize never completed)", http.StatusConflict)
		return
	}
	// Publish credential contract gate. The smoke test below proves the VM
	// boots; it does NOT prove the credentials Crucible surfaces are usable.
	// For clone_no_customize / registered_existing_vm, blank static template
	// credentials on any OS yield a VM that boots but hands the student no
	// usable login. For customized Linux templates, the cloud-init default
	// user must still line up with the injected "student" account. Catch
	// those latent lockouts here rather than after publish.
	if msg, blocked := credentialContractPublishError(tmpl); blocked {
		http.Error(w, msg, http.StatusConflict)
		return
	}
	payload := map[string]any{
		"template_id": tmpl.ID,
		"vm_moref":    tmpl.VCenterVMID,
	}
	if !h.advanceTemplateAndEnqueue(w, r, tmpl, models.TemplateStateReady, models.TemplateStateVerifying,
		models.JobTypeTemplateVerify, payload, "template.publish") {
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
	resp := h.wizardState(r.Context(), fresh)
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
	respondJSON(w, http.StatusOK, h.wizardState(r.Context(), tmpl))
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
	tmpl, err := h.provisionStore().GetTemplateByID(r.Context(), templateID)
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
	pdb := h.provisionStore()
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
	if err := pdb.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, from, to); err != nil {
		h.handleLifecycleUpdateErr(w, tmpl, err)
		return false
	}
	job, err := pdb.CreateJob(r.Context(), jobType, body)
	if err != nil {
		// Best-effort rollback: try to move state back. If that fails
		// we're in an inconsistent state — surface it loud so the
		// operator hits /retry rather than retry-stuck.
		_ = pdb.UpdateTemplateLifecycleState(r.Context(), tmpl.ID, to, from)
		h.logger.Error("enqueue job failed", "error", err, "job_type", jobType)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}
	if h.events != nil {
		if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
		}
	}
	// Audit is best-effort; skip when h.db is nil (test environments that
	// inject a fake provDB but omit the real *database.Queries).
	if h.db != nil {
		audit.Log(r.Context(), h.db, auditAction,
			audit.Resource("template", tmpl.ID),
			audit.IP(r.RemoteAddr),
			audit.Detail("job_id", job.ID.String()),
			audit.Detail("from_state", from),
			audit.Detail("to_state", to),
		)
	}
	fresh, _ := pdb.GetTemplateByID(r.Context(), tmpl.ID)
	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"state":  h.wizardState(r.Context(), fresh),
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
	respondJSON(w, http.StatusOK, h.wizardState(r.Context(), fresh))
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

func (h *Handler) wizardState(ctx stdcontext.Context, tmpl *models.Template) WizardStateResponse {
	if tmpl == nil {
		return WizardStateResponse{}
	}
	allowed, _ := templates.AllowedNextStates(tmpl.TemplateState)
	resp := WizardStateResponse{
		TemplateID:        tmpl.ID,
		TemplateState:     tmpl.TemplateState,
		AllowedNextStates: allowed,
		VCenterVMID:       tmpl.VCenterVMID,
		SourceType:        tmpl.SourceType,
		SourceRef:         tmpl.SourceRef,
		StagingNetwork:    tmpl.StagingNetwork,
		// Build-VM access (Phase H): plumb the template's OS/kind/defaults
		// always. The wizard UI only renders the access panel during
		// transient build states, so a leak into non-build states would
		// just be a no-op — but keeping these populated makes the field
		// behavior predictable from the API's perspective.
		OSType:          tmpl.OSType,
		TemplateKind:    tmpl.Kind,
		AssignIP:        tmpl.AssignIP,
		DefaultUsername: tmpl.DefaultUsername,
		DefaultPassword: tmpl.DefaultPassword,
		UnattendMode:    tmpl.UnattendMode,
	}
	// ISO + configuring: tell the operator whether to expect an automated
	// install or to drive the installer at the console themselves.
	if tmpl.TemplateState == models.TemplateStateConfiguring && tmpl.SourceType == models.TemplateSourceISO {
		if tmpl.UnattendMode == "" || tmpl.UnattendMode == models.UnattendModeManual {
			resp.ConfiguringHint = "manual-install: connect to the VM console to drive the OS installer"
		} else {
			resp.ConfiguringHint = "automated-install: the provisioner will advance the template when the installer completes"
		}
	}
	// Live VM info from vCenter (Phase H): best-effort, never blocks the
	// wizard response on a transient vCenter hiccup. Only queried during
	// the build-time states where a staging VM actually exists.
	if h.vc != nil && tmpl.VCenterVMID != "" && isBuildState(tmpl.TemplateState) {
		info, err := h.vc.GetGuestInfo(ctx, tmpl.VCenterVMID)
		switch {
		case err != nil:
			h.logger.Warn("wizardState: GetGuestInfo failed",
				"template_id", tmpl.ID, "moref", tmpl.VCenterVMID, "error", err)
		case info != nil:
			resp.BuildVMName = info.Name
			resp.BuildVMIP = info.IPAddress
			resp.BuildVMPowerOn = info.PoweredOn
			resp.BuildVMTools = info.ToolsRunning
		}
	}
	// Surface the most recent worker job for this template so the UI can
	// (a) mark the right wizard step as the errored one — without this it
	// can only guess from template_state + vcenter_vm_id and gets it wrong
	// when template_provision fails AFTER the clone succeeded — and (b)
	// show the actual error string instead of "check worker logs".
	if h.db == nil {
		return resp
	}
	job, err := h.db.GetLatestJobForTemplate(ctx, tmpl.ID)
	if err != nil {
		h.logger.Warn("wizardState: GetLatestJobForTemplate failed", "template_id", tmpl.ID, "error", err)
		return resp
	}
	if job == nil {
		return resp
	}
	resp.LastJobType = job.Type
	resp.LastJobStatus = job.Status
	// job.Result is a JSON blob ({"error": "..."} on failure, or a success
	// envelope). Pull out the error string if present so the wizard can
	// render it inline.
	if len(job.Result) > 0 {
		var parsed struct {
			Error string `json:"error"`
		}
		if jerr := json.Unmarshal(job.Result, &parsed); jerr == nil && parsed.Error != "" {
			resp.LastJobError = parsed.Error
		}
	}
	return resp
}

// isBuildState reports whether the template is in a state where a real
// staging VM exists in vCenter. The wizard only queries vCenter for live
// VM info during these states — `draft` has no VM at all, `ready` /
// `active` have a (converted-to-template) VM that's powered off and
// useless for SSH/RDP, and `error` could be either pre- or post-clone
// but we leave it alone to avoid spurious vCenter calls during an
// already-failed build.
func isBuildState(state string) bool {
	switch state {
	case models.TemplateStateProvisioning,
		models.TemplateStateConfiguring,
		models.TemplateStateGeneralizing:
		return true
	}
	return false
}

// buildTemplateVMName produces a vCenter-safe VM name from the template's
// human-readable name: lower-case, alphanumeric-or-dash, with a 6-char
// suffix DERIVED FROM THE TEMPLATE ID.
//
// The suffix is deterministic (first 6 hex of the template UUID) on purpose.
// It used to be random, which meant every Provision/Retry cycle produced a
// *new* staging VM name; the idempotent clone therefore cloned a fresh VM on
// each retry and orphaned the previous one in the Templates folder. With a
// stable name, a retry reuses the same staging VM (CloneTemplateSourceVM
// returns the existing moref on a name collision), so a template can only ever
// have one staging VM — no orphans, and no two concurrent jobs racing to clone
// different VMs from the same source.
func buildTemplateVMName(humanName string, templateID uuid.UUID) string {
	slug := strings.ToLower(humanName)
	slug = vmNameSlugRe.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "tpl"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	suffix := strings.ReplaceAll(templateID.String(), "-", "")
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return fmt.Sprintf("tpl-%s-%s", slug, suffix)
}

func ifNonEmpty(s, msg string) string {
	if s == "" {
		return ""
	}
	return msg
}

// validateUnattendMode checks that mode is one of the known UnattendMode*
// constants. An empty string is accepted (the column default is 'manual').
// A non-empty unknown value is always rejected with an error — silently
// coercing to 'manual' would make an automated install never happen.
func validateUnattendMode(mode string) error {
	if mode == "" {
		return nil
	}
	if !models.ValidUnattendMode(mode) {
		return fmt.Errorf("unattend_mode %q is not valid; must be one of: %s",
			mode, strings.Join(models.AllUnattendModes, ", "))
	}
	return nil
}

// normalizeUnattendConfig validates raw against the unattend.Spec schema and
// returns it byte-for-byte unchanged. A nil or empty input is normalised to {}
// so callers can write the result directly to the unattend_config NOT NULL
// JSONB column.
//
// Unknown fields are rejected deliberately. encoding/json matches field names
// case-insensitively but does NOT ignore separators, so "timeZone" or
// "aptProxy" would unmarshal to the zero value with no error and no log line —
// the template would then silently build without the staging apt cache (slow at
// best, a hard failure on a host with no direct internet) and nothing in the
// failure would point at JSON casing. Failing here turns that whole class of
// typo into a 400 at authoring time, while the instructor is still looking at
// the form.
func normalizeUnattendConfig(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var spec unattend.Spec
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("unattend_config is not valid: %w", err)
	}
	return raw, nil
}

// buildProvisionPayload assembles the template_provision job payload from
// the template row. Extracted so it can be unit-tested without a database
// fixture. All four ISO-specific fields (DiskGB, GuestID, UnattendMode,
// UnattendConfig) are always included; the provisioner ignores them for
// non-ISO source types.
func buildProvisionPayload(tmpl *models.Template, vmName string) map[string]any {
	return map[string]any{
		"template_id":     tmpl.ID,
		"source_type":     tmpl.SourceType,
		"source_ref":      tmpl.SourceRef,
		"vm_name":         vmName,
		"staging_network": tmpl.StagingNetwork,
		"vcpus":           tmpl.DefaultVCPUs,
		"ram_mb":          tmpl.DefaultRAMMB,
		"disk_gb":         tmpl.DefaultDiskGB,
		"guest_id":        tmpl.GuestID,
		"unattend_mode":   tmpl.UnattendMode,
		"unattend_config": tmpl.UnattendConfig,
	}
}
