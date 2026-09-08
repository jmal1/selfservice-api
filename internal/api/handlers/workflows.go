package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/actionlibrary"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/workflowvalidation"
)

type workflowActivationStore interface {
	ActivateWorkflow(context.Context, uuid.UUID) error
}

func (h *Handler) workflowActivationStore() workflowActivationStore {
	if h.workflowActivationDB != nil {
		return h.workflowActivationDB
	}
	return h.db
}

// normalizeScriptLineEndings converts CRLF and bare CR to LF so scripts
// authored on Windows don't ship to the linux runner with literal carriage
// returns (which break shebangs, conditionals, and trigger shellcheck SC1017
// on every line).
func normalizeScriptLineEndings(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// normalizeScriptPtr applies normalizeScriptLineEndings through a *string,
// leaving nil pointers untouched. Used for PATCH-style request bodies where
// "field absent" and "field empty" mean different things.
func normalizeScriptPtr(p *string) *string {
	if p == nil {
		return nil
	}
	v := normalizeScriptLineEndings(*p)
	return &v
}

// normalizeActionScripts applies normalizeScriptLineEndings to every Script
// field in a slice of actions in place, so workflow create/update payloads
// can't introduce CRLF endings through their nested action list. Returns the
// same slice for chaining at the call site.
func normalizeActionScripts(actions []models.Action) []models.Action {
	for i := range actions {
		actions[i].Script = normalizeScriptLineEndings(actions[i].Script)
	}
	return actions
}

// --- Admin Workflow Routes ---

// AdminListWorkflows returns all workflows (admin/instructor view).
func (h *Handler) AdminListWorkflows(w http.ResponseWriter, r *http.Request) {
	workflows, err := h.db.ListWorkflows(r.Context())
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "failed to list workflows")
		return
	}
	respondJSON(w, http.StatusOK, workflows)
}

// AdminGetWorkflow returns a single workflow with actions.
func (h *Handler) AdminGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	wf, err := h.db.GetWorkflowWithActions(r.Context(), id)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "workflow not found")
		return
	}
	respondJSON(w, http.StatusOK, wf)
}

// AdminCreateWorkflow creates a new workflow in draft status.
func (h *Handler) AdminCreateWorkflow(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	var req struct {
		Name           string          `json:"name"`
		Slug           string          `json:"slug"`
		Description    string          `json:"description"`
		Category       string          `json:"category"`
		ExecutionMode  string          `json:"execution_mode"`
		Script         string          `json:"script"`
		SetupScript    *string         `json:"setup_script"`
		TimeoutSeconds int             `json:"timeout_seconds"`
		CreationMode   string          `json:"creation_mode"`
		Actions        []models.Action `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Slug == "" {
		respondError(w, r, http.StatusBadRequest, "name and slug are required")
		return
	}
	// The slug goes into URLs and the runner's config. It has the same shape
	// rule as an action slug, minus the callable requirement.
	if err := actionlibrary.ValidateSlug(req.Slug); err != nil {
		respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 300
	}
	if req.ExecutionMode == "" {
		req.ExecutionMode = models.ExecModeKaliRunner
	}
	if req.CreationMode == "" {
		req.CreationMode = models.CreationModeVisual
	}
	// These are Postgres CHECK constraints. Without this, a typo surfaces as a
	// 500 with the constraint name buried in the API log rather than a message
	// naming the two legal values.
	if req.ExecutionMode != models.ExecModeKaliRunner && req.ExecutionMode != models.ExecModeVMwareTools {
		respondError(w, r, http.StatusBadRequest, fmt.Sprintf("execution_mode must be %q or %q", models.ExecModeKaliRunner, models.ExecModeVMwareTools))
		return
	}
	if req.CreationMode != models.CreationModeVisual && req.CreationMode != models.CreationModeScript {
		respondError(w, r, http.StatusBadRequest, fmt.Sprintf("creation_mode must be %q or %q", models.CreationModeVisual, models.CreationModeScript))
		return
	}

	wf := &models.Workflow{
		Name:           req.Name,
		Slug:           req.Slug,
		Description:    req.Description,
		Category:       req.Category,
		ExecutionMode:  req.ExecutionMode,
		Script:         normalizeScriptLineEndings(req.Script),
		SetupScript:    normalizeScriptPtr(req.SetupScript),
		TimeoutSeconds: req.TimeoutSeconds,
		CreationMode:   req.CreationMode,
		Status:         models.WorkflowStatusDraft,
		CreatedBy:      userID,
		IsActive:       true,
		Actions:        normalizeActionScripts(req.Actions),
	}

	if err := h.db.CreateWorkflow(r.Context(), wf); err != nil {
		h.logger.Error("failed to create workflow", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to create workflow")
		return
	}

	respondJSON(w, http.StatusCreated, wf)
}

// AdminUpdateWorkflow updates a workflow. Reviewed workflows return to draft.
func (h *Handler) AdminUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	var req struct {
		Name           *string         `json:"name"`
		Description    *string         `json:"description"`
		Category       *string         `json:"category"`
		Script         *string         `json:"script"`
		SetupScript    *string         `json:"setup_script"`
		TimeoutSeconds *int            `json:"timeout_seconds"`
		CreationMode   *string         `json:"creation_mode"`
		Actions        []models.Action `json:"actions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.db.UpdateWorkflow(r.Context(), id, req.Name, req.Description, req.Category,
		normalizeScriptPtr(req.Script), normalizeScriptPtr(req.SetupScript),
		req.TimeoutSeconds, req.CreationMode, normalizeActionScripts(req.Actions)); err != nil {
		h.logger.Error("failed to update workflow", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to update workflow")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// AdminDeleteWorkflow deletes a workflow and its actions.
func (h *Handler) AdminDeleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	if err := h.db.DeleteWorkflow(r.Context(), id); err != nil {
		h.logger.Error("failed to delete workflow", "error", err)
		respondError(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// AdminSubmitWorkflow moves a workflow from draft to pending_review.
func (h *Handler) AdminSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	if err := h.db.TransitionWorkflowStatus(r.Context(), id,
		models.WorkflowStatusDraft, models.WorkflowStatusPendingReview); err != nil {
		respondError(w, r, http.StatusConflict, "workflow is not in draft status")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "pending_review"})
}

// AdminApproveWorkflow approves a workflow (different user than creator, or admin self-approve).
func (h *Handler) AdminApproveWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	wf, err := h.db.GetWorkflow(r.Context(), id)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "workflow not found")
		return
	}

	// Approver must be different from creator (unless admin)
	if wf.CreatedBy == userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "cannot approve your own workflow — another instructor must review")
		return
	}

	if err := h.db.ApproveWorkflow(r.Context(), id, userID); err != nil {
		respondError(w, r, http.StatusConflict, "workflow is not in pending_review status")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// AdminActivateWorkflow activates an approved workflow.
func (h *Handler) AdminActivateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid workflow ID")
		return
	}

	if err := h.workflowActivationStore().ActivateWorkflow(r.Context(), id); err != nil {
		var validationErr *workflowvalidation.RunActionError
		switch {
		case errors.As(err, &validationErr):
			respondError(w, r, http.StatusUnprocessableEntity, "workflow cannot be activated: "+validationErr.Error())
		case errors.Is(err, database.ErrWorkflowNotApproved):
			respondError(w, r, http.StatusConflict, database.ErrWorkflowNotApproved.Error())
		default:
			h.logger.Error("failed to activate workflow", "error", err, "workflow_id", id)
			respondError(w, r, http.StatusInternalServerError, "failed to activate workflow")
		}
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// AdminImportWorkflows bulk imports workflows from JSON.
func (h *Handler) AdminImportWorkflows(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	var workflows []models.Workflow
	if err := json.NewDecoder(r.Body).Decode(&workflows); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid JSON")
		return
	}

	imported, skipped := 0, 0
	for i := range workflows {
		workflows[i].CreatedBy = userID
		workflows[i].Status = models.WorkflowStatusDraft
		workflows[i].IsActive = true
		workflows[i].Script = normalizeScriptLineEndings(workflows[i].Script)
		workflows[i].SetupScript = normalizeScriptPtr(workflows[i].SetupScript)
		workflows[i].Actions = normalizeActionScripts(workflows[i].Actions)
		if err := h.db.CreateWorkflow(r.Context(), &workflows[i]); err != nil {
			skipped++
			continue
		}
		imported++
	}

	respondJSON(w, http.StatusOK, map[string]int{"imported": imported, "skipped": skipped})
}

// AdminExportWorkflows exports all workflows as JSON.
func (h *Handler) AdminExportWorkflows(w http.ResponseWriter, r *http.Request) {
	workflows, err := h.db.ListWorkflowsWithActions(r.Context())
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "failed to export")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=workflows.json")
	json.NewEncoder(w).Encode(workflows)
}

// --- Admin Action Library Routes ---

func (h *Handler) AdminListActions(w http.ResponseWriter, r *http.Request) {
	actions, err := h.db.ListLibraryActions(r.Context())
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "failed to list actions")
		return
	}
	respondJSON(w, http.StatusOK, actions)
}

func (h *Handler) AdminGetAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid action ID")
		return
	}
	action, err := h.db.GetLibraryAction(r.Context(), id)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "action not found")
		return
	}
	respondJSON(w, http.StatusOK, action)
}

func (h *Handler) AdminCreateAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name               string          `json:"name"`
		Slug               string          `json:"slug"`
		Description        string          `json:"description"`
		ActionType         string          `json:"action_type"`
		ActionCategory     string          `json:"action_category"`
		Params             json.RawMessage `json:"params"`
		Script             string          `json:"script"`
		InputContext       json.RawMessage `json:"input_context"`
		OutputContext      json.RawMessage `json:"output_context"`
		TimeoutSeconds     int             `json:"timeout_seconds"`
		StudentFailHint    *string         `json:"student_fail_hint"`
		Points             *int            `json:"points"`
		Penalty            *int            `json:"penalty"`
		SupportedPlatforms json.RawMessage `json:"supported_platforms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.Slug == "" {
		respondError(w, r, http.StatusBadRequest, "name and slug are required")
		return
	}
	if req.SupportedPlatforms == nil {
		req.SupportedPlatforms = json.RawMessage(`["any"]`)
	}
	if msg, ok := h.validateLibraryAction(r, req.Slug, req.Script, req.SupportedPlatforms, uuid.Nil); !ok {
		respondError(w, r, http.StatusBadRequest, msg)
		return
	}
	if req.ActionType == "" {
		req.ActionType = "command"
	}
	if req.ActionCategory == "" {
		req.ActionCategory = "general"
	}
	if req.TimeoutSeconds <= 0 {
		req.TimeoutSeconds = 60
	}
	if req.Params == nil {
		req.Params = json.RawMessage("{}")
	}
	if req.InputContext == nil {
		req.InputContext = json.RawMessage("[]")
	}
	if req.OutputContext == nil {
		req.OutputContext = json.RawMessage("[]")
	}

	action := &models.Action{
		Name:               req.Name,
		Slug:               &req.Slug,
		Description:        req.Description,
		ActionType:         req.ActionType,
		ActionCategory:     req.ActionCategory,
		Params:             req.Params,
		Script:             normalizeScriptLineEndings(req.Script),
		InputContext:       req.InputContext,
		OutputContext:      req.OutputContext,
		TimeoutSeconds:     req.TimeoutSeconds,
		StudentFailHint:    req.StudentFailHint,
		Points:             req.Points,
		Penalty:            req.Penalty,
		IsLibrary:          true,
		SupportedPlatforms: req.SupportedPlatforms,
	}

	if err := h.db.CreateLibraryAction(r.Context(), action); err != nil {
		h.logger.Error("failed to create action", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to create action")
		return
	}
	respondJSON(w, http.StatusCreated, action)
}

func (h *Handler) AdminUpdateAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid action ID")
		return
	}

	var req struct {
		Name               *string          `json:"name"`
		Slug               *string          `json:"slug"`
		Description        *string          `json:"description"`
		ActionType         *string          `json:"action_type"`
		ActionCategory     *string          `json:"action_category"`
		Params             *json.RawMessage `json:"params"`
		Script             *string          `json:"script"`
		InputContext       *json.RawMessage `json:"input_context"`
		OutputContext      *json.RawMessage `json:"output_context"`
		TimeoutSeconds     *int             `json:"timeout_seconds"`
		StudentFailHint    *string          `json:"student_fail_hint"`
		Points             *int             `json:"points"`
		Penalty            *int             `json:"penalty"`
		SupportedPlatforms *json.RawMessage `json:"supported_platforms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	// The update is a partial COALESCE, so validation has to run against the
	// merged result rather than the request alone: a new script must be checked
	// against the existing slug, and a new slug against the existing script.
	if req.Slug != nil || req.Script != nil || req.SupportedPlatforms != nil {
		existing, err := h.db.GetLibraryAction(r.Context(), id)
		if err != nil {
			respondError(w, r, http.StatusNotFound, "action not found")
			return
		}
		slug := ""
		if existing.Slug != nil {
			slug = *existing.Slug
		}
		if req.Slug != nil {
			slug = *req.Slug
		}
		script := existing.Script
		if req.Script != nil {
			script = *req.Script
		}
		platforms := existing.SupportedPlatforms
		if req.SupportedPlatforms != nil {
			platforms = *req.SupportedPlatforms
		}
		if msg, ok := h.validateLibraryAction(r, slug, script, platforms, id); !ok {
			respondError(w, r, http.StatusBadRequest, msg)
			return
		}
		if msg, ok := h.validateActionSlugRename(r, existing.Slug, req.Slug); !ok {
			respondError(w, r, http.StatusConflict, msg)
			return
		}
	}

	// Convert json.RawMessage pointer to string pointer for the COALESCE query
	var paramsStr *string
	if req.Params != nil {
		s := string(*req.Params)
		paramsStr = &s
	}

	if err := h.db.UpdateLibraryAction(r.Context(), id, req.Name, req.Slug, req.Description,
		req.ActionType, req.ActionCategory, paramsStr, normalizeScriptPtr(req.Script),
		req.InputContext, req.OutputContext, req.TimeoutSeconds, req.StudentFailHint,
		req.Points, req.Penalty, req.SupportedPlatforms); err != nil {
		h.logger.Error("failed to update action", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to update action")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// validateLibraryAction rejects an action that would break the generated action
// library rather than merely fail on its own.
//
// The generated library is one bash file sourced by every runner pod, and
// buildActionLibrary refuses to render it at all if any slug is unusable or two
// slugs collide. So a single malformed action does not produce one red check --
// it produces a total, unattributable failure of every assessment in flight.
// These three checks are the same ones the engine and the runner perform, moved
// forward to the one moment where a human is present to read the error.
//
// Returns a client-facing message and false when the action must be rejected.
func (h *Handler) validateLibraryAction(r *http.Request, slug, script string, platforms json.RawMessage, excludeID uuid.UUID) (string, bool) {
	if err := actionlibrary.ValidateSlug(slug); err != nil {
		return err.Error(), false
	}
	callable, err := actionlibrary.CallableName(slug)
	if err != nil {
		return err.Error(), false
	}

	conflict, err := h.db.LibraryActionCallableConflict(r.Context(), callable, excludeID)
	if err != nil {
		h.logger.Error("failed to check action callable conflict", "error", err)
		return "failed to validate action slug", false
	}
	if conflict != "" {
		return fmt.Sprintf("slug %q generates the same runner function %q as existing action %q; one of them must be renamed", slug, callable, conflict), false
	}

	// A windows action's body is PowerShell and is excluded from the bash
	// library entirely, so bash-parsing it would reject every legitimate one.
	if bytes.Contains(platforms, []byte(`"windows"`)) {
		return "", true
	}
	if err := actionlibrary.ValidateBody(callable, script); err != nil {
		return err.Error(), false
	}
	return "", true
}

// validateActionSlugRename blocks a rename that would silently break workflows.
//
// The slug is the only handle a workflow script has on an action: renaming it
// renames the generated shell function, and every workflow still calling the
// old name fails with exit 127 at run time, inside a student's assessment.
// Nothing else in the system would notice, because there is no foreign key
// between a workflow's bash and the action catalog.
func (h *Handler) validateActionSlugRename(r *http.Request, existing, requested *string) (string, bool) {
	if requested == nil || existing == nil || *requested == *existing {
		return "", true
	}
	oldCallable, err := actionlibrary.CallableName(*existing)
	if err != nil {
		// The old slug was never callable, so nothing can be referencing it.
		return "", true
	}
	callers, err := h.db.WorkflowsCallingAction(r.Context(), oldCallable)
	if err != nil {
		h.logger.Error("failed to check workflows calling action", "error", err)
		return "failed to validate slug rename", false
	}
	if len(callers) == 0 {
		return "", true
	}
	return fmt.Sprintf(
		"cannot rename slug %q to %q: %d workflow(s) still call %s(), including %s. Update those scripts first, or create a new action and retire this one.",
		*existing, *requested, len(callers), oldCallable, strings.Join(callers, ", "),
	), false
}

func (h *Handler) AdminDeleteAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid action ID")
		return
	}
	if err := h.db.DeleteLibraryAction(r.Context(), id); err != nil {
		respondError(w, r, http.StatusNotFound, "action not found or not a library action")
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
