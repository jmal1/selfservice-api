package handlers

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
)

// Handler holds shared dependencies for all API handlers.
type Handler struct {
	db     *database.Queries
	events *events.Client
	logger *slog.Logger
}

// NewHandler creates a new Handler.
func NewHandler(db *database.Queries, events *events.Client, logger *slog.Logger) *Handler {
	return &Handler{db: db, events: events, logger: logger}
}

// --- Pod Handlers ---

// ListPods returns the user's pods (admin: all pods).
func (h *Handler) ListPods(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	pods, err := h.db.ListPodsByOwner(r.Context(), userID)
	if err != nil {
		h.logger.Error("list pods failed", "error", err, "user_id", userID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, pods)
}

// GetPod returns a specific pod with its VMs.
func (h *Handler) GetPod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}

	// Verify ownership (or admin)
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	respondJSON(w, http.StatusOK, pod)
}

// CreatePod queues a pod creation job.
func (h *Handler) CreatePod(w http.ResponseWriter, r *http.Request) {
	var req models.CreatePodRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || len(req.VMs) == 0 {
		http.Error(w, "name and at least one VM are required", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Fetch user for quota check
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusInternalServerError)
		return
	}

	// Check quotas
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		h.logger.Error("get resource usage failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Resolve templates and calculate requested resources
	var totalVCPUs, totalRAM int
	for _, vm := range req.VMs {
		templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		var found *models.Template
		for _, t := range templates {
			if t.ID == vm.TemplateID {
				found = &t
				break
			}
		}
		if found == nil {
			http.Error(w, "template not found or not accessible: "+vm.TemplateID.String(), http.StatusBadRequest)
			return
		}

		vcpus := found.DefaultVCPUs
		if vm.VCPUs != nil {
			vcpus = *vm.VCPUs
		}
		ram := found.DefaultRAMMB
		if vm.RAMMB != nil {
			ram = *vm.RAMMB
		}
		totalVCPUs += vcpus
		totalRAM += ram
	}

	if usage.ActivePods+1 > user.MaxPods {
		http.Error(w, "pod limit exceeded", http.StatusConflict)
		return
	}
	if usage.UsedVCPUs+totalVCPUs > user.MaxVCPUs {
		http.Error(w, "vCPU quota exceeded", http.StatusConflict)
		return
	}
	if usage.UsedRAMMB+totalRAM > user.MaxRAMMB {
		http.Error(w, "RAM quota exceeded", http.StatusConflict)
		return
	}

	// Compute expiration based on role
	var expiresAt *time.Time
	switch role {
	case models.RoleStudent:
		t := time.Now().Add(7 * 24 * time.Hour)
		expiresAt = &t
	case models.RoleInstructor:
		t := time.Now().Add(30 * 24 * time.Hour)
		expiresAt = &t
	}

	// Create job payload
	type createPayload struct {
		UserID    string                 `json:"user_id"`
		PodName   string                 `json:"pod_name"`
		VMs       []models.VMRequest     `json:"vms"`
		ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	}
	payload, _ := json.Marshal(createPayload{
		UserID:    userID.String(),
		PodName:   req.Name,
		VMs:       req.VMs,
		ExpiresAt: expiresAt,
	})

	// Insert job
	job, err := h.db.CreateJob(r.Context(), models.JobTypePodCreate, payload)
	if err != nil {
		h.logger.Error("create job failed", "error", err)
		http.Error(w, "failed to queue provisioning job", http.StatusInternalServerError)
		return
	}

	// Notify workers via NATS
	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
	}

	// Audit
	h.db.InsertAuditLog(r.Context(), models.AuditLog{
		UserID:       &userID,
		Action:       "pod.create.requested",
		ResourceType: strPtr("job"),
		ResourceID:   &job.ID,
		Details:      payload,
		IPAddress:    strPtr(r.RemoteAddr),
	})

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// DeletePod queues a pod destruction job.
func (h *Handler) DeletePod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if pod.Status == models.PodStatusDestroying || pod.Status == models.PodStatusDestroyed {
		http.Error(w, "pod is already being destroyed", http.StatusConflict)
		return
	}

	payload, _ := json.Marshal(map[string]string{"pod_id": podID.String()})
	job, err := h.db.CreateJob(r.Context(), models.JobTypePodDestroy, payload)
	if err != nil {
		h.logger.Error("create destroy job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// --- Template Handlers ---

// ListTemplates returns templates accessible to the current user.
func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list templates failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, templates)
}

// --- Job Handlers ---

// GetJobStatus returns the current status of a job.
func (h *Handler) GetJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		http.Error(w, "invalid job id", http.StatusBadRequest)
		return
	}

	job, err := h.db.GetJob(r.Context(), jobID)
	if err != nil || job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	respondJSON(w, http.StatusOK, models.JobStatusResponse{
		ID:        job.ID,
		Type:      job.Type,
		Status:    job.Status,
		CreatedAt: job.CreatedAt.Format(time.RFC3339),
	})
}

// --- Auth Handlers ---

// GetMe returns the current user's profile and resource usage.
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		h.logger.Error("get resource usage failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	usage.MaxVCPUs = user.MaxVCPUs
	usage.MaxRAMMB = user.MaxRAMMB
	usage.MaxPods = user.MaxPods

	respondJSON(w, http.StatusOK, models.MeResponse{
		User:          *user,
		ResourceUsage: *usage,
	})
}

// --- Admin Handlers ---

// AdminListUsers returns all users (admin only).
func (h *Handler) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("admin list users failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, users)
}

// AdminUpdateQuotas updates a user's resource quotas.
func (h *Handler) AdminUpdateQuotas(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		http.Error(w, "invalid user id", http.StatusBadRequest)
		return
	}

	var req models.UpdateQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.UpdateUserQuotas(r.Context(), userID, req); err != nil {
		h.logger.Error("update quotas failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AdminListTemplates returns all templates including inactive (admin only).
func (h *Handler) AdminListTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := h.db.ListAllTemplates(r.Context())
	if err != nil {
		h.logger.Error("admin list templates failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, templates)
}

// AdminCreateTemplate creates a new template.
func (h *Handler) AdminCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req models.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.VCenterTemplate == "" || req.OSType == "" {
		http.Error(w, "name, vcenter_template, and os_type are required", http.StatusBadRequest)
		return
	}

	tmpl, err := h.db.CreateTemplate(r.Context(), req)
	if err != nil {
		h.logger.Error("create template failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusCreated, tmpl)
}

// AdminSetTemplateAccess sets access rules for a template.
func (h *Handler) AdminSetTemplateAccess(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}

	var req models.SetTemplateAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.SetTemplateAccess(r.Context(), templateID, req.Rules); err != nil {
		h.logger.Error("set template access failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Health returns service health status.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Helpers ---

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func strPtr(s string) *string {
	return &s
}
