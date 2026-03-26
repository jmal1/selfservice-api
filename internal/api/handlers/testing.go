package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
)

// --- Student Testing Routes ---

// GetTestingDashboard returns playlists assigned to a pod's VM (template defaults or blueprint overrides).
func (h *Handler) GetTestingDashboard(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify pod ownership (or admin)
	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "not your pod", http.StatusForbidden)
		return
	}

	// Resolve playlists for this pod (template defaults → blueprint overrides)
	playlists, err := h.db.GetPlaylistsForPod(r.Context(), podID)
	if err != nil {
		h.logger.Error("failed to get playlists for pod", "pod_id", podID, "error", err)
		http.Error(w, "failed to load playlists", http.StatusInternalServerError)
		return
	}

	// Get recent runs
	runs, err := h.db.GetRecentRunsForPod(r.Context(), podID, 10)
	if err != nil {
		h.logger.Warn("failed to get recent runs", "pod_id", podID, "error", err)
		runs = []models.Run{}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"playlists": playlists,
		"recent_runs": runs,
	})
}

// CreateTestingRun triggers a new assessment run for a pod.
func (h *Handler) CreateTestingRun(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify pod ownership
	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "not your pod", http.StatusForbidden)
		return
	}

	// Parse request
	var req struct {
		PlaylistID  *uuid.UUID  `json:"playlist_id"`
		WorkflowIDs []uuid.UUID `json:"workflow_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.PlaylistID == nil && len(req.WorkflowIDs) == 0 {
		http.Error(w, "playlist_id or workflow_ids required", http.StatusBadRequest)
		return
	}

	// Check for active run on this pod
	hasActive, err := h.db.HasActiveRun(r.Context(), podID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if hasActive {
		http.Error(w, "an assessment is already running on this pod", http.StatusConflict)
		return
	}

	// Rate limit: 3 runs per hour
	recentCount, err := h.db.CountRecentRuns(r.Context(), podID, userID, 1*time.Hour)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if recentCount >= 3 {
		w.Header().Set("Retry-After", "3600")
		http.Error(w, "maximum 3 runs per hour", http.StatusTooManyRequests)
		return
	}

	// Generate callback token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	callbackToken := hex.EncodeToString(tokenBytes)

	// Create the run
	run := &models.Run{
		PodID:         podID,
		PlaylistID:    req.PlaylistID,
		TriggeredBy:   userID,
		CallbackToken: callbackToken,
		Status:        models.RunStatusPending,
	}

	if err := h.db.CreateRun(r.Context(), run); err != nil {
		h.logger.Error("failed to create run", "error", err)
		http.Error(w, "failed to create run", http.StatusInternalServerError)
		return
	}

	// Notify engine via NATS
	h.events.PublishRaw("testing.runs.created", events.Event{
		Type:  "run.created",
		JobID: run.ID.String(),
		PodID: podID.String(),
	})

	respondJSON(w, http.StatusAccepted, map[string]any{
		"run_id":  run.ID,
		"status":  "pending",
		"message": "Assessment run queued. Runner provisioning will begin shortly.",
	})
}

// ListTestingRuns returns past runs for a pod.
func (h *Handler) ListTestingRuns(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "not your pod", http.StatusForbidden)
		return
	}

	runs, err := h.db.GetRunsForPod(r.Context(), podID)
	if err != nil {
		http.Error(w, "failed to load runs", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, runs)
}

// GetTestingRun returns a single run with results (student-filtered).
func (h *Handler) GetTestingRun(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod ID", http.StatusBadRequest)
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		http.Error(w, "invalid run ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "not your pod", http.StatusForbidden)
		return
	}

	run, err := h.db.GetRunWithResults(r.Context(), runID)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	if run.PodID != podID {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Strip instructor-only fields for non-admin users
	if role != models.RoleAdmin && role != models.RoleInstructor {
		for i := range run.Results {
			run.Results[i].InstructorOutput = nil
			run.Results[i].ActionResults = nil
		}
	}

	respondJSON(w, http.StatusOK, run)
}

// CancelTestingRun cancels a pending or running assessment.
func (h *Handler) CancelTestingRun(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod ID", http.StatusBadRequest)
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		http.Error(w, "invalid run ID", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "not your pod", http.StatusForbidden)
		return
	}

	run, err := h.db.GetRun(r.Context(), runID)
	if err != nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Only cancel pending/provisioning/running
	switch run.Status {
	case models.RunStatusPending, models.RunStatusProvisioning, models.RunStatusRunning:
		errMsg := fmt.Sprintf("Cancelled by %s", middleware.UsernameFromContext(r.Context()))
		if err := h.db.UpdateRunStatus(r.Context(), runID, models.RunStatusCancelled, &errMsg); err != nil {
			http.Error(w, "failed to cancel", http.StatusInternalServerError)
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	default:
		http.Error(w, "run has already finished", http.StatusConflict)
	}
}
