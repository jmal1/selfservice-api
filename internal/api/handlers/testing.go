package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
)

// --- Student Testing Routes ---

// GetTestingDashboard returns assessment offers grouped by reachable pod VM,
// plus recent runs for the pod.
func (h *Handler) GetTestingDashboard(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify pod ownership (or admin)
	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "not your pod")
		return
	}

	targets, err := h.db.GetTestingTargetsForPod(r.Context(), podID)
	if err != nil {
		h.logger.Error("failed to get testing targets for pod", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to load assessments")
		return
	}

	// Get recent runs
	runs, err := h.db.GetRecentRunsForPod(r.Context(), podID, 10)
	if err != nil {
		h.logger.Warn("failed to get recent runs", "pod_id", podID, "error", err)
		runs = []models.Run{}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"targets":     targets,
		"recent_runs": runs,
	})
}

// CreateTestingRun triggers a new assessment run for a pod against a specific VM.
func (h *Handler) CreateTestingRun(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify pod ownership
	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "not your pod")
		return
	}

	// Parse request
	var req struct {
		PlaylistID    *uuid.UUID  `json:"playlist_id"`
		TargetPodVMID *uuid.UUID  `json:"target_pod_vm_id"`
		WorkflowIDs   []uuid.UUID `json:"workflow_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.PlaylistID == nil && len(req.WorkflowIDs) == 0 {
		respondError(w, r, http.StatusBadRequest, "playlist_id or workflow_ids required")
		return
	}
	// The engine resolves a run's workflows solely from its playlist
	// (see engine.executeRun), and runs.playlist_id is the only selection we
	// persist — there is nowhere to record an ad-hoc workflow list. Accepting
	// workflow_ids therefore produced a 202 for a run that was guaranteed to
	// fail asynchronously with "run <id> has no playlist", which is far worse
	// than refusing it outright. Reject it honestly until the engine supports
	// ad-hoc selection. No caller relies on this: the UI only ever sends
	// playlist_id.
	if req.PlaylistID == nil {
		respondError(w, r, http.StatusBadRequest, "ad-hoc workflow_ids are not supported yet; supply playlist_id")
		return
	}
	if req.TargetPodVMID == nil {
		respondError(w, r, http.StatusBadRequest, "target_pod_vm_id is required")
		return
	}

	target, err := h.db.GetRunnablePodVMTarget(r.Context(), podID, *req.TargetPodVMID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, sql.ErrNoRows) {
			respondError(w, r, http.StatusBadRequest, "target_pod_vm_id is not a runnable VM on this pod")
			return
		}
		h.logger.Error("lookup target VM failed", "pod_id", podID, "pod_vm_id", *req.TargetPodVMID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	offered, err := h.db.PlaylistOfferedForPodVM(r.Context(), podID, *req.TargetPodVMID, *req.PlaylistID)
	if err != nil {
		h.logger.Error("validate playlist for VM failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !offered {
		respondError(w, r, http.StatusBadRequest, "playlist is not offered for the selected VM")
		return
	}

	// Check for active run on this pod
	hasActive, err := h.db.HasActiveRun(r.Context(), podID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if hasActive {
		respondError(w, r, http.StatusConflict, "an assessment is already running on this pod")
		return
	}

	// Rate limit: 3 runs per hour
	recentCount, err := h.db.CountRecentRuns(r.Context(), podID, userID, 1*time.Hour)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if recentCount >= 3 {
		w.Header().Set("Retry-After", "3600")
		respondError(w, r, http.StatusTooManyRequests, "maximum 3 runs per hour")
		return
	}

	// Generate callback token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	callbackToken := hex.EncodeToString(tokenBytes)

	targetID := target.PodVMID
	// Create the run
	run := &models.Run{
		PodID:         podID,
		PlaylistID:    req.PlaylistID,
		TriggeredBy:   userID,
		CallbackToken: callbackToken,
		Status:        models.RunStatusPending,
		TargetPodVMID: &targetID,
		TargetVMName:  target.DisplayName,
		TargetVMIP:    target.IPAddress,
	}

	if err := h.db.CreateRun(r.Context(), run); err != nil {
		h.logger.Error("failed to create run", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to create run")
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
		respondError(w, r, http.StatusBadRequest, "invalid pod ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "not your pod")
		return
	}

	runs, err := h.db.GetRunsForPod(r.Context(), podID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "failed to load runs")
		return
	}

	respondJSON(w, http.StatusOK, runs)
}

// validateTestingRunAccess returns 0 when the caller may read the run for the
// given pod owner. Students can only read their own runs; instructors and
// admins can read any run.
func validateTestingRunAccess(podOwnerID, userID uuid.UUID, role string) int {
	if podOwnerID == userID || middleware.HasMinRole(role, models.RoleInstructor) {
		return 0
	}
	return http.StatusForbidden
}

// validateTestingRunPodMatch returns 0 when the run belongs to the pod in the
// route, otherwise NotFound to avoid leaking another pod's run metadata.
func validateTestingRunPodMatch(runPodID, podID uuid.UUID) int {
	if runPodID == podID {
		return 0
	}
	return http.StatusNotFound
}

// GetTestingRun returns a single run with results (owner/instructor/admin view).
func (h *Handler) GetTestingRun(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod ID")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid run ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	if status := validateTestingRunAccess(pod.OwnerID, userID, role); status != 0 {
		respondError(w, r, http.StatusForbidden, "not your pod")
		return
	}

	run, err := h.db.GetRunWithResults(r.Context(), runID)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "run not found")
		return
	}
	if status := validateTestingRunPodMatch(run.PodID, podID); status != 0 {
		respondError(w, r, status, "run not found")
		return
	}

	// Strip instructor-only fields for non-instructor callers.
	if !middleware.HasMinRole(role, models.RoleInstructor) {
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
		respondError(w, r, http.StatusBadRequest, "invalid pod ID")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid run ID")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "not your pod")
		return
	}

	run, err := h.db.GetRun(r.Context(), runID)
	if err != nil {
		respondError(w, r, http.StatusNotFound, "run not found")
		return
	}
	if status := validateTestingRunPodMatch(run.PodID, podID); status != 0 {
		respondError(w, r, status, "run not found")
		return
	}

	// Only cancel pending/provisioning/running
	switch run.Status {
	case models.RunStatusPending, models.RunStatusProvisioning, models.RunStatusRunning:
		errMsg := fmt.Sprintf("Cancelled by %s", middleware.UsernameFromContext(r.Context()))
		if err := h.db.UpdateRunStatus(r.Context(), runID, models.RunStatusCancelled, &errMsg); err != nil {
			respondError(w, r, http.StatusInternalServerError, "failed to cancel")
			return
		}
		respondJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
	default:
		respondError(w, r, http.StatusConflict, "run has already finished")
	}
}
