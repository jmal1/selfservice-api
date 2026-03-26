package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// --- Admin Workflow Routes ---

// AdminListWorkflows returns all workflows (admin/instructor view).
func (h *Handler) AdminListWorkflows(w http.ResponseWriter, r *http.Request) {
	workflows, err := h.db.ListWorkflows(r.Context())
	if err != nil {
		http.Error(w, "failed to list workflows", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, workflows)
}

// AdminGetWorkflow returns a single workflow with actions.
func (h *Handler) AdminGetWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		http.Error(w, "invalid workflow ID", http.StatusBadRequest)
		return
	}

	wf, err := h.db.GetWorkflowWithActions(r.Context(), id)
	if err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
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
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.Slug == "" {
		http.Error(w, "name and slug are required", http.StatusBadRequest)
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

	wf := &models.Workflow{
		Name:           req.Name,
		Slug:           req.Slug,
		Description:    req.Description,
		Category:       req.Category,
		ExecutionMode:  req.ExecutionMode,
		Script:         req.Script,
		SetupScript:    req.SetupScript,
		TimeoutSeconds: req.TimeoutSeconds,
		CreationMode:   req.CreationMode,
		Status:         models.WorkflowStatusDraft,
		CreatedBy:      userID,
		IsActive:       true,
		Actions:        req.Actions,
	}

	if err := h.db.CreateWorkflow(r.Context(), wf); err != nil {
		h.logger.Error("failed to create workflow", "error", err)
		http.Error(w, "failed to create workflow", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusCreated, wf)
}

// AdminUpdateWorkflow updates a workflow. If active, creates a new version.
func (h *Handler) AdminUpdateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		http.Error(w, "invalid workflow ID", http.StatusBadRequest)
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
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.UpdateWorkflow(r.Context(), id, req.Name, req.Description, req.Category,
		req.Script, req.SetupScript, req.TimeoutSeconds, req.CreationMode, req.Actions); err != nil {
		h.logger.Error("failed to update workflow", "error", err)
		http.Error(w, "failed to update workflow", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// AdminSubmitWorkflow moves a workflow from draft to pending_review.
func (h *Handler) AdminSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		http.Error(w, "invalid workflow ID", http.StatusBadRequest)
		return
	}

	if err := h.db.TransitionWorkflowStatus(r.Context(), id,
		models.WorkflowStatusDraft, models.WorkflowStatusPendingReview); err != nil {
		http.Error(w, "workflow is not in draft status", http.StatusConflict)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "pending_review"})
}

// AdminApproveWorkflow approves a workflow (different user than creator, or admin self-approve).
func (h *Handler) AdminApproveWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		http.Error(w, "invalid workflow ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	wf, err := h.db.GetWorkflow(r.Context(), id)
	if err != nil {
		http.Error(w, "workflow not found", http.StatusNotFound)
		return
	}

	// Approver must be different from creator (unless admin)
	if wf.CreatedBy == userID && role != models.RoleAdmin {
		http.Error(w, "cannot approve your own workflow — another instructor must review", http.StatusForbidden)
		return
	}

	if err := h.db.ApproveWorkflow(r.Context(), id, userID); err != nil {
		http.Error(w, "workflow is not in pending_review status", http.StatusConflict)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

// AdminActivateWorkflow activates an approved workflow.
func (h *Handler) AdminActivateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "workflowID"))
	if err != nil {
		http.Error(w, "invalid workflow ID", http.StatusBadRequest)
		return
	}

	if err := h.db.TransitionWorkflowStatus(r.Context(), id,
		models.WorkflowStatusApproved, models.WorkflowStatusActive); err != nil {
		http.Error(w, "workflow is not in approved status", http.StatusConflict)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "active"})
}

// AdminImportWorkflows bulk imports workflows from JSON.
func (h *Handler) AdminImportWorkflows(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	var workflows []models.Workflow
	if err := json.NewDecoder(r.Body).Decode(&workflows); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	imported, skipped := 0, 0
	for i := range workflows {
		workflows[i].CreatedBy = userID
		workflows[i].Status = models.WorkflowStatusDraft
		workflows[i].IsActive = true
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
		http.Error(w, "failed to export", http.StatusInternalServerError)
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
		http.Error(w, "failed to list actions", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, actions)
}

func (h *Handler) AdminGetAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		http.Error(w, "invalid action ID", http.StatusBadRequest)
		return
	}
	action, err := h.db.GetLibraryAction(r.Context(), id)
	if err != nil {
		http.Error(w, "action not found", http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, action)
}

func (h *Handler) AdminCreateAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string          `json:"name"`
		Slug            string          `json:"slug"`
		Description     string          `json:"description"`
		ActionType      string          `json:"action_type"`
		ActionCategory  string          `json:"action_category"`
		Params          json.RawMessage `json:"params"`
		Script          string          `json:"script"`
		InputContext    json.RawMessage `json:"input_context"`
		OutputContext   json.RawMessage `json:"output_context"`
		TimeoutSeconds  int             `json:"timeout_seconds"`
		StudentFailHint *string         `json:"student_fail_hint"`
		Points          *int            `json:"points"`
		Penalty         *int            `json:"penalty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Slug == "" {
		http.Error(w, "name and slug are required", http.StatusBadRequest)
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
		Name:            req.Name,
		Slug:            &req.Slug,
		Description:     req.Description,
		ActionType:      req.ActionType,
		ActionCategory:  req.ActionCategory,
		Params:          req.Params,
		Script:          req.Script,
		InputContext:    req.InputContext,
		OutputContext:   req.OutputContext,
		TimeoutSeconds:  req.TimeoutSeconds,
		StudentFailHint: req.StudentFailHint,
		Points:          req.Points,
		Penalty:         req.Penalty,
		IsLibrary:       true,
	}

	if err := h.db.CreateLibraryAction(r.Context(), action); err != nil {
		h.logger.Error("failed to create action", "error", err)
		http.Error(w, "failed to create action", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusCreated, action)
}

func (h *Handler) AdminUpdateAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		http.Error(w, "invalid action ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Name            *string          `json:"name"`
		Slug            *string          `json:"slug"`
		Description     *string          `json:"description"`
		ActionType      *string          `json:"action_type"`
		ActionCategory  *string          `json:"action_category"`
		Params          *json.RawMessage `json:"params"`
		Script          *string          `json:"script"`
		InputContext    *json.RawMessage `json:"input_context"`
		OutputContext   *json.RawMessage `json:"output_context"`
		TimeoutSeconds  *int             `json:"timeout_seconds"`
		StudentFailHint *string          `json:"student_fail_hint"`
		Points          *int             `json:"points"`
		Penalty         *int             `json:"penalty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Convert json.RawMessage pointer to string pointer for the COALESCE query
	var paramsStr *string
	if req.Params != nil {
		s := string(*req.Params)
		paramsStr = &s
	}

	if err := h.db.UpdateLibraryAction(r.Context(), id, req.Name, req.Slug, req.Description,
		req.ActionType, req.ActionCategory, paramsStr, req.Script,
		req.InputContext, req.OutputContext, req.TimeoutSeconds, req.StudentFailHint,
		req.Points, req.Penalty); err != nil {
		h.logger.Error("failed to update action", "error", err)
		http.Error(w, "failed to update action", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *Handler) AdminDeleteAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "actionID"))
	if err != nil {
		http.Error(w, "invalid action ID", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteLibraryAction(r.Context(), id); err != nil {
		http.Error(w, "action not found or not a library action", http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
