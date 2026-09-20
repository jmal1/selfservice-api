package handlers

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

const myRunsLookback = time.Hour

// ListMyRuns returns the caller's in-progress playlist runs plus runs that
// completed in the last hour. GET /api/v1/runs must not collide with the
// WebSocket at /api/v1/runs/{runID}/progress/ws (registered on a different tree).
func (h *Handler) ListMyRuns(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		respondError(w, r, http.StatusUnauthorized, "unauthorized")
		return
	}

	runs, err := h.runsStore().ListRunsForUser(r.Context(), userID, time.Now().Add(-myRunsLookback))
	if err != nil {
		h.logger.Error("list my runs failed", "error", err, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "failed to list runs")
		return
	}
	if runs == nil {
		runs = []models.Run{}
	}
	respondJSON(w, http.StatusOK, runs)
}
