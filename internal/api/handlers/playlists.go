package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// --- Admin Playlist Routes ---

// AdminListPlaylists returns all playlists.
func (h *Handler) AdminListPlaylists(w http.ResponseWriter, r *http.Request) {
	playlists, err := h.db.ListPlaylists(r.Context())
	if err != nil {
		http.Error(w, "failed to list playlists", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, playlists)
}

// AdminGetPlaylist returns a single playlist with workflows.
func (h *Handler) AdminGetPlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "playlistID"))
	if err != nil {
		http.Error(w, "invalid playlist ID", http.StatusBadRequest)
		return
	}

	pl, err := h.db.GetPlaylistWithWorkflows(r.Context(), id)
	if err != nil {
		http.Error(w, "playlist not found", http.StatusNotFound)
		return
	}
	respondJSON(w, http.StatusOK, pl)
}

// AdminCreatePlaylist creates a new playlist.
func (h *Handler) AdminCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	var req struct {
		Name        string      `json:"name"`
		Slug        string      `json:"slug"`
		Description string      `json:"description"`
		WorkflowIDs []uuid.UUID `json:"workflow_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Slug == "" {
		http.Error(w, "name and slug are required", http.StatusBadRequest)
		return
	}

	pl := &models.Playlist{
		Name:        req.Name,
		Slug:        req.Slug,
		Description: req.Description,
		ScoringMode: models.ScoringModePassFail,
		CreatedBy:   userID,
		IsActive:    true,
	}

	if err := h.db.CreatePlaylist(r.Context(), pl, req.WorkflowIDs); err != nil {
		h.logger.Error("failed to create playlist", "error", err)
		http.Error(w, "failed to create playlist", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusCreated, pl)
}

// AdminUpdatePlaylist updates a playlist's metadata and workflow membership.
func (h *Handler) AdminUpdatePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "playlistID"))
	if err != nil {
		http.Error(w, "invalid playlist ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Name        *string     `json:"name"`
		Description *string     `json:"description"`
		WorkflowIDs []uuid.UUID `json:"workflow_ids"`
		IsActive    *bool       `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.UpdatePlaylist(r.Context(), id, req.Name, req.Description, req.IsActive, req.WorkflowIDs); err != nil {
		h.logger.Error("failed to update playlist", "error", err)
		http.Error(w, "failed to update playlist", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// AdminDeletePlaylist soft-deletes a playlist.
func (h *Handler) AdminDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "playlistID"))
	if err != nil {
		http.Error(w, "invalid playlist ID", http.StatusBadRequest)
		return
	}

	if err := h.db.DeactivatePlaylist(r.Context(), id); err != nil {
		http.Error(w, "failed to delete playlist", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// AdminSetTemplatePlaylists assigns playlists to a template.
func (h *Handler) AdminSetTemplatePlaylists(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template ID", http.StatusBadRequest)
		return
	}

	var req struct {
		PlaylistIDs []uuid.UUID `json:"playlist_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.SetTemplatePlaylists(r.Context(), templateID, req.PlaylistIDs); err != nil {
		h.logger.Error("failed to set template playlists", "error", err)
		http.Error(w, "failed to set playlists", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// AdminGetTemplatePlaylists returns playlist IDs assigned to a template.
func (h *Handler) AdminGetTemplatePlaylists(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template ID", http.StatusBadRequest)
		return
	}

	ids, err := h.db.GetTemplatePlaylists(r.Context(), templateID)
	if err != nil {
		h.logger.Error("failed to get template playlists", "error", err)
		http.Error(w, "failed to get playlists", http.StatusInternalServerError)
		return
	}
	if ids == nil {
		ids = []uuid.UUID{}
	}

	respondJSON(w, http.StatusOK, map[string]any{"playlist_ids": ids})
}

// AdminSetBlueprintVMPlaylists assigns playlist overrides to a blueprint VM slot.
func (h *Handler) AdminSetBlueprintVMPlaylists(w http.ResponseWriter, r *http.Request) {
	blueprintID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		http.Error(w, "invalid blueprint ID", http.StatusBadRequest)
		return
	}

	var req struct {
		VMSlot      int         `json:"vm_slot"`
		PlaylistIDs []uuid.UUID `json:"playlist_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := h.db.SetBlueprintVMPlaylists(r.Context(), blueprintID, req.VMSlot, req.PlaylistIDs); err != nil {
		h.logger.Error("failed to set blueprint VM playlists", "error", err)
		http.Error(w, "failed to set playlists", http.StatusInternalServerError)
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// AdminListRuns returns all runs across all pods (admin view).
func (h *Handler) AdminListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := h.db.ListAllRuns(r.Context())
	if err != nil {
		http.Error(w, "failed to list runs", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, runs)
}
