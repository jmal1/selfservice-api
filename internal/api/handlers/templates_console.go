package handlers

// templates_console.go — browser console access for template-build VMs.
//
// During the wizard's interactive phases (provisioning / configuring /
// generalizing) the staging VM is a real powered-on vCenter VM, but
// instructors don't have vCenter accounts to open its console.
//
// TemplateBuildConsoleWS exposes the same WebMKS-proxied console that
// pod-VM consoles use (see VMConsoleWS in console.go) but scoped to a
// template ID and authorized by template ownership.
//
// Auth: admin OR template.created_by == userID. Templates without a
// created_by (legacy admin-created rows where the column is NULL) are
// admin-only.
//
// State gating: only allowed when template_state is one of
//   provisioning, configuring, generalizing
// AND vcenter_vm_id is non-empty. Outside these states the staging VM
// either doesn't exist yet (draft) or has been converted to the
// published template artifact and may no longer be a usable VM
// (ready/active), or sysprep is mid-flight (generalizing close to end).

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// templateConsoleStates is the canonical list of lifecycle states during
// which a template-build VM console is accessible. Keep in sync with the
// design note in docs/architecture/wiki.md and the Phase G plan.
var templateConsoleStates = map[string]bool{
	models.TemplateStateProvisioning: true,
	models.TemplateStateConfiguring:  true,
	models.TemplateStateGeneralizing: true,
}

// templateConsoleAuth is the shared auth + state-gating path for both
// the WS endpoint and the ticket-builder endpoint. Returns the loaded
// template on success, or nil + an HTTP-ready error message + status
// code on rejection. The handlers then either run the proxy or build a
// URL response from the same template object.
func (h *Handler) templateConsoleAuth(r *http.Request) (*models.Template, string, int) {
	ctx := r.Context()
	userID := middleware.UserIDFromContext(ctx)
	role := middleware.RoleFromContext(ctx)

	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		return nil, "invalid template id", http.StatusBadRequest
	}

	tmpl, err := h.db.GetTemplateByID(ctx, templateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "template not found", http.StatusNotFound
		}
		h.logger.Error("template console: get template failed", "error", err, "template_id", templateID)
		return nil, "internal error", http.StatusInternalServerError
	}
	if tmpl == nil {
		return nil, "template not found", http.StatusNotFound
	}

	if msg, status := validateTemplateConsoleAccess(tmpl, userID, role); status != 0 {
		return nil, msg, status
	}
	return tmpl, "", 0
}

// validateTemplateConsoleAccess returns ("", 0) when the caller is
// allowed to open a build-VM console for the given template; otherwise
// it returns the HTTP error message + status code to send to the
// client.
//
// Split out from templateConsoleAuth so the auth + state-gating
// rules are unit-testable without a database fixture. The DB load
// itself is exercised by the integration suite and Phase G9 cluster
// verification.
func validateTemplateConsoleAccess(tmpl *models.Template, userID uuid.UUID, role string) (string, int) {
	// Auth: admin OR created_by == userID.
	// Templates with NULL created_by are admin-only.
	if role != models.RoleAdmin {
		if tmpl.CreatedBy == nil || *tmpl.CreatedBy != userID {
			return "forbidden", http.StatusForbidden
		}
	}

	// State gating
	if !templateConsoleStates[tmpl.TemplateState] {
		return fmt.Sprintf("template is in %q state; console is only available during provisioning, configuring, or generalizing", tmpl.TemplateState), http.StatusConflict
	}

	// Staging VM must actually exist in vCenter
	if tmpl.VCenterVMID == "" {
		return "template has no staging vCenter VM (provisioning may not have completed)", http.StatusConflict
	}

	return "", 0
}

// TemplateBuildConsoleWS opens a WebSocket console session against the
// template's staging VM. Same wire-level contract as VMConsoleWS so the
// frontend WMKS embed can be shared.
//
// URL: WS /api/v1/admin/templates/{templateID}/console/ws
func (h *Handler) TemplateBuildConsoleWS(w http.ResponseWriter, r *http.Request) {
	tmpl, errMsg, status := h.templateConsoleAuth(r)
	if tmpl == nil {
		http.Error(w, errMsg, status)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())

	h.runWebMKSProxy(w, r, runWebMKSProxyArgs{
		VCenterVMID:     tmpl.VCenterVMID,
		DisplayName:     tmpl.Name,
		AuditOpenEvent:  "template.console.open",
		AuditCloseEvent: "template.console.close",
		AuditOpts: []audit.Option{
			audit.Resource("template", tmpl.ID),
			audit.IP(r.RemoteAddr),
			audit.Detail("template_name", tmpl.Name),
			audit.Detail("template_state", tmpl.TemplateState),
			audit.Detail("moref", tmpl.VCenterVMID),
		},
		LogFields: []any{"user", userID, "template", tmpl.Name, "template_id", tmpl.ID, "state", tmpl.TemplateState},
	})
}

// TemplateConsoleTicketResponse mirrors the shape returned by the
// pod-VM console-ticket endpoint so the same frontend WMKS embed
// helper can be reused.
type TemplateConsoleTicketResponse struct {
	// WSURL is the relative WebSocket path the browser should connect
	// to. The frontend builds an absolute URL by prefixing the page's
	// origin (with ws:// or wss:// based on protocol).
	WSURL string `json:"ws_url"`

	// TemplateState is included so the UI can decide whether to render
	// the console at all (e.g., disable the open-console button if
	// state moved on between page-load and click).
	TemplateState string `json:"template_state"`

	// VMName is the display name for the console window title.
	VMName string `json:"vm_name"`
}

// TemplateBuildConsoleTicket returns the WebSocket URL + metadata the
// UI needs to open the console. Doing this in a separate request
// (rather than the UI constructing the URL itself) keeps the auth +
// state gating server-side and gives us one place to add future
// concerns like single-use tokens or per-session TTL.
//
// URL: GET /api/v1/admin/templates/{templateID}/console/ticket
func (h *Handler) TemplateBuildConsoleTicket(w http.ResponseWriter, r *http.Request) {
	tmpl, errMsg, status := h.templateConsoleAuth(r)
	if tmpl == nil {
		http.Error(w, errMsg, status)
		return
	}

	respondJSON(w, http.StatusOK, TemplateConsoleTicketResponse{
		WSURL:         fmt.Sprintf("/api/v1/admin/templates/%s/console/ws", tmpl.ID),
		TemplateState: tmpl.TemplateState,
		VMName:        tmpl.Name,
	})
}
