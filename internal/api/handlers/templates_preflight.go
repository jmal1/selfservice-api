package handlers

// templates_preflight.go — AdminPreflightTemplate endpoint and the
// preflight-gate that runs inside AdminProvisionTemplate.
//
// AdminPreflightTemplate (POST /admin/templates/:id/preflight) is the
// stand-alone check endpoint. It runs all 11 checks and returns their
// results. No state is mutated.
//
// The gate in AdminProvisionTemplate calls runPreflightForTemplate before
// enqueuing. If any block-severity check fails, it returns 409 with the
// full result list rather than enqueuing a job that will fail minutes later.
// Warnings never block.
//
// Admin override
// -----------
// An admin (not instructor) may POST {"override_preflight_blocks":true} to
// bypass a blocking failure. This is an escape hatch for the case where the
// checker is wrong about the environment. When it is used:
//   - We log at Warn level with the list of overridden check IDs.
//   - The audit trail records the override alongside the normal job-creation
//     audit entry.
// Instructors cannot use this flag; the middleware.RequireRole(RoleInstructor)
// guard on the route does not prevent it for admins because RoleAdmin ≥
// RoleInstructor, but the handler explicitly checks for RoleAdmin.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter/preflight"
)

// PreflightConfig holds the vCenter settings the preflight checks need but
// that are not stored on the template row itself. Wire this in via
// Handler.WithPreflightVCenter.
type PreflightConfig struct {
	DatastoreName               string
	TemplateFolder              string
	ConfiguredResourcePoolPaths []string
}

// AdminPreflightTemplate (POST /admin/templates/:id/preflight) runs all
// preflight checks and returns the structured results. Does not mutate state.
// Returns 200 with the results even when checks fail — the caller interprets
// OK/Severity. Returns 503 if preflight is not configured (no vCenter wired).
func (h *Handler) AdminPreflightTemplate(w http.ResponseWriter, r *http.Request) {
	if h.vcPreflight == nil {
		http.Error(w, "preflight checks not configured (vCenter not wired)", http.StatusServiceUnavailable)
		return
	}
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
	params := h.buildPreflightParams(tmpl, "")
	results := preflight.RunAll(r.Context(), h.vcPreflight, params)
	respondJSON(w, http.StatusOK, map[string]any{
		"template_id":      tmpl.ID,
		"any_block_failed": preflight.AnyBlockFailed(results),
		"results":          results,
	})
}

// ProvisionWithPreflightRequest is the optional body for
// AdminProvisionTemplate. The body is optional for backwards compatibility
// (callers that POST no body get normal preflight blocking behaviour).
type ProvisionWithPreflightRequest struct {
	// OverridePreflightBlocks bypasses failing block-severity preflight
	// checks and proceeds with provisioning anyway. Only effective when
	// the caller has RoleAdmin; instructors receive 403 if they set this.
	OverridePreflightBlocks bool `json:"override_preflight_blocks,omitempty"`
}

// runPreflightGate executes preflight and enforces the block/override policy.
// Returns (results, blocked) where blocked=true means the caller must abort.
// When blocked=false the caller may proceed (either all checks passed, or an
// admin used the override).
//
// Side-effects when the override is used:
//   - A Warn-level log entry lists every overridden check ID.
//   - An audit entry is written.
func (h *Handler) runPreflightGate(w http.ResponseWriter, r *http.Request, tmpl *models.Template,
	vmName string, override bool) (results []preflight.Result, blocked bool) {

	if h.vcPreflight == nil {
		// preflight not configured — skip. This is the case during tests that
		// don't wire vCenter; in production it should always be wired.
		return nil, false
	}

	params := h.buildPreflightParams(tmpl, vmName)
	results = preflight.RunAll(r.Context(), h.vcPreflight, params)

	if !preflight.AnyBlockFailed(results) {
		return results, false
	}

	role := middleware.RoleFromContext(r.Context())
	if override && role == models.RoleAdmin {
		// Log loudly that block checks were bypassed.
		var overridden []string
		for _, res := range results {
			if res.Severity == "block" && !res.OK {
				overridden = append(overridden, res.ID)
			}
		}
		h.logger.Warn("preflight block override used",
			"template_id", tmpl.ID,
			"user", middleware.UsernameFromContext(r.Context()),
			"overridden_checks", strings.Join(overridden, ","),
		)
		if h.db != nil {
			audit.Log(r.Context(), h.db, "template.preflight.override",
				audit.Resource("template", tmpl.ID),
				audit.IP(r.RemoteAddr),
				audit.Detail("overridden_checks", strings.Join(overridden, ",")),
			)
		}
		return results, false // proceed despite blocks
	}

	if override && role != models.RoleAdmin {
		// Non-admin tried to use the override. Return 403 with explanation.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   "forbidden",
			"reason":  "override_preflight_blocks requires admin role",
			"results": results,
		})
		return results, true
	}

	// Block: return 409 with the full result list so the UI can render each
	// check's Detail and Fix without further API calls.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":             "preflight_blocked",
		"reason":            "one or more block-severity preflight checks failed",
		"preflight_blocked": true,
		"results":           results,
	})
	return results, true
}

// buildPreflightParams assembles a preflight.Params from the template row and
// the handler's configured preflight config.
func (h *Handler) buildPreflightParams(tmpl *models.Template, vmName string) preflight.Params {
	p := preflight.Params{
		TargetVMName:  vmName,
		DatastoreName: h.preflightCfg.DatastoreName,
		StagingPortGroup: tmpl.StagingNetwork,
		SourceType:    tmpl.SourceType,
		GuestUsername: tmpl.DefaultUsername,
		GuestPassword: tmpl.DefaultPassword,
		ConfiguredResourcePoolPaths: h.preflightCfg.ConfiguredResourcePoolPaths,
		TargetFolderPath: h.preflightCfg.TemplateFolder,
	}

	switch tmpl.SourceType {
	case models.TemplateSourceCloneVCenter:
		p.SourceMoref = tmpl.SourceRef
	case models.TemplateSourceCloneTemplate:
		// source_ref holds the Crucible template UUID; the vcenter_vm_id
		// of that template row is the actual moref. At draft time the wizard
		// stores the source template's vcenter_vm_id in source_ref when the
		// source is clone_template. We use it directly here.
		p.SourceMoref = tmpl.SourceRef
	case models.TemplateSourceISO:
		p.ISORef = tmpl.SourceRef
	}

	return p
}
