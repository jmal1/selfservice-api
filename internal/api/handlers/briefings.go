package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// Briefing admin routes are registered twice: under /admin/templates/{templateID}
// and under /admin/blueprints/{blueprintID}. The URL param names the owner kind.
func briefingOwner(w http.ResponseWriter, r *http.Request) (string, uuid.UUID, bool) {
	kind, raw := models.BriefingOwnerTemplate, chi.URLParam(r, "templateID")
	if raw == "" {
		kind, raw = models.BriefingOwnerBlueprint, chi.URLParam(r, "blueprintID")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid "+kind+" id")
		return "", uuid.Nil, false
	}
	return kind, id, true
}

func briefingRevisionParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	rev, err := strconv.Atoi(chi.URLParam(r, "rev"))
	if err != nil || rev < 1 {
		respondError(w, r, http.StatusBadRequest, "invalid revision")
		return 0, false
	}
	return rev, true
}

// respondBriefingError maps store errors; it returns false when err is nil.
func (h *Handler) respondBriefingError(w http.ResponseWriter, r *http.Request, err error) bool {
	var conflict *database.BriefingConflictError
	switch {
	case err == nil:
		return false
	case errors.Is(err, database.ErrBriefingOwnerNotFound):
		respondError(w, r, http.StatusNotFound, "not found")
	case errors.Is(err, database.ErrBriefingRevisionNotFound):
		respondError(w, r, http.StatusNotFound, "revision not found")
	case errors.As(err, &conflict):
		respondJSON(w, http.StatusConflict, map[string]any{
			"error":         "briefing changed since you started editing",
			"head_revision": conflict.Head,
		})
	default:
		h.logger.Error("briefing request failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
	}
	return true
}

func (h *Handler) auditBriefing(r *http.Request, action, kind string, ownerID uuid.UUID, revision int) {
	audit.Log(r.Context(), h.db, action,
		audit.Resource(kind, ownerID),
		audit.FromRequest(r),
		audit.Detail("revision", revision),
	)
}

// AdminGetBriefing returns head and published revision numbers.
func (h *Handler) AdminGetBriefing(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	meta, err := h.db.GetBriefingMeta(r.Context(), kind, ownerID)
	if h.respondBriefingError(w, r, err) {
		return
	}
	respondJSON(w, http.StatusOK, meta)
}

// AdminListBriefingRevisions returns history, newest first, without bodies.
func (h *Handler) AdminListBriefingRevisions(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	revs, err := h.db.ListBriefingRevisions(r.Context(), kind, ownerID)
	if h.respondBriefingError(w, r, err) {
		return
	}
	respondJSON(w, http.StatusOK, revs)
}

// AdminGetBriefingRevision returns one revision with its body.
func (h *Handler) AdminGetBriefingRevision(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	rev, ok := briefingRevisionParam(w, r)
	if !ok {
		return
	}
	out, err := h.db.GetBriefingRevision(r.Context(), kind, ownerID, rev)
	if h.respondBriefingError(w, r, err) {
		return
	}
	respondJSON(w, http.StatusOK, out)
}

type saveBriefingRequest struct {
	BodyMD       string `json:"body_md"`
	Message      string `json:"message"`
	BaseRevision *int   `json:"base_revision"`
}

// AdminSaveBriefingRevision appends a revision. base_revision is required so a
// save can never silently overwrite someone else's newer revision (409).
func (h *Handler) AdminSaveBriefingRevision(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	var req saveBriefingRequest
	// JSON escaping can double a body's size, so allow headroom over the column cap.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*models.BriefingMaxBodyBytes+4096)).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.BaseRevision == nil || *req.BaseRevision < 0 {
		respondError(w, r, http.StatusBadRequest, "base_revision is required (0 for the first save)")
		return
	}
	if len(req.BodyMD) > models.BriefingMaxBodyBytes {
		respondError(w, r, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("briefing is larger than %d KiB", models.BriefingMaxBodyBytes/1024))
		return
	}
	if !utf8.ValidString(req.BodyMD) || len(req.Message) > 500 {
		respondError(w, r, http.StatusBadRequest, "invalid body or message")
		return
	}
	rev, err := h.db.SaveBriefingRevision(r.Context(), kind, ownerID,
		middleware.UserIDFromContext(r.Context()), req.BodyMD, req.Message, *req.BaseRevision)
	if h.respondBriefingError(w, r, err) {
		return
	}
	h.auditBriefing(r, "briefing.save", kind, ownerID, rev.Revision)
	rev.BodyMD = ""
	respondJSON(w, http.StatusCreated, rev)
}

// AdminRestoreBriefingRevision copies an old revision to a new head. It does
// not publish it.
func (h *Handler) AdminRestoreBriefingRevision(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	from, ok := briefingRevisionParam(w, r)
	if !ok {
		return
	}
	rev, err := h.db.RestoreBriefingRevision(r.Context(), kind, ownerID,
		middleware.UserIDFromContext(r.Context()), from)
	if h.respondBriefingError(w, r, err) {
		return
	}
	h.auditBriefing(r, "briefing.restore", kind, ownerID, rev.Revision)
	rev.BodyMD = ""
	respondJSON(w, http.StatusCreated, rev)
}

// AdminPublishBriefing sets the revision students see; publishing an older
// revision is a rollback.
func (h *Handler) AdminPublishBriefing(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	var req struct {
		Revision int `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Revision < 1 {
		respondError(w, r, http.StatusBadRequest, "revision is required")
		return
	}
	if h.respondBriefingError(w, r, h.db.PublishBriefingRevision(r.Context(), kind, ownerID, req.Revision)) {
		return
	}
	h.auditBriefing(r, "briefing.publish", kind, ownerID, req.Revision)
	h.AdminGetBriefing(w, r)
}

// AdminUnpublishBriefing hides the briefing from students.
func (h *Handler) AdminUnpublishBriefing(w http.ResponseWriter, r *http.Request) {
	kind, ownerID, ok := briefingOwner(w, r)
	if !ok {
		return
	}
	if h.respondBriefingError(w, r, h.db.UnpublishBriefing(r.Context(), kind, ownerID)) {
		return
	}
	h.auditBriefing(r, "briefing.unpublish", kind, ownerID, 0)
	h.AdminGetBriefing(w, r)
}

// GetPodBriefing returns the published briefings for a pod. Unlike GetPod,
// any instructor may read it, so instructors see exactly what students see.
// It returns 200 with an empty list when nothing is published.
func (h *Handler) GetPodBriefing(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}
	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && !middleware.HasMinRole(role, models.RoleInstructor) {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}
	sections, err := h.db.ListPodBriefingSections(r.Context(), podID)
	if err != nil {
		h.logger.Error("list pod briefing failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"sections": sections})
}
