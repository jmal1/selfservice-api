package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type templateSourceReplicaResolver interface {
	ResolveTemplateSourceIdentity(context.Context, string) (*vcenter.TemplateSourceIdentity, error)
}

type templateReplicaBuildResolver interface {
	ResolveReplicaBuildTarget(context.Context, vcenter.ReplicaBuildTarget) (*vcenter.ReplicaBuildTarget, error)
	ValidateReplicaBuildPrivileges(context.Context, string, vcenter.ReplicaBuildTarget) error
}

type createTemplateSourceReplicaRequest struct {
	SourceRef string `json:"source_ref"`
}

type createTemplateReplicaBuildRequest struct {
	SourceReplicaID uuid.UUID                  `json:"source_replica_id"`
	IdempotencyKey  string                     `json:"idempotency_key"`
	DestinationName string                     `json:"destination_name"`
	Target          vcenter.ReplicaBuildTarget `json:"target"`
}

type templateSourceReplicaListResponse struct {
	ReplicaMode bool                           `json:"replica_mode"`
	Replicas    []models.TemplateSourceReplica `json:"replicas"`
}

func replicaBuildRequestMatches(
	build *models.TemplateReplicaBuild,
	req createTemplateReplicaBuildRequest,
) bool {
	return build != nil &&
		build.SourceReplicaID != nil &&
		*build.SourceReplicaID == req.SourceReplicaID &&
		build.IdempotencyKey == req.IdempotencyKey &&
		build.DestinationName == req.DestinationName &&
		build.ComputeResourceType == req.Target.ComputeResourceType &&
		build.ComputeResourceMoref == req.Target.ComputeResourceMoref &&
		build.ComputeResourcePath == req.Target.ComputeResourcePath &&
		build.HostMoref == req.Target.HostMoref &&
		build.HostName == req.Target.HostName &&
		build.ResourcePoolMoref == req.Target.ResourcePoolMoref &&
		build.ResourcePoolPath == req.Target.ResourcePoolPath &&
		build.DatastoreMoref == req.Target.DatastoreMoref &&
		build.DatastoreName == req.Target.DatastoreName &&
		build.FolderMoref == req.Target.FolderMoref &&
		build.FolderPath == req.Target.FolderPath
}

func (h *Handler) AdminListTemplateSourceReplicas(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	replicas, err := h.db.ListTemplateSourceReplicas(r.Context(), templateID)
	if err != nil {
		h.logger.Error("list template source replicas failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	replicaMode, err := h.db.TemplateSourceReplicaModeEnabled(r.Context(), templateID)
	if err != nil {
		h.logger.Error("read template source replica policy failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, templateSourceReplicaListResponse{
		ReplicaMode: replicaMode,
		Replicas:    replicas,
	})
}

func (h *Handler) AdminCreateTemplateSourceReplica(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	var req createTemplateSourceReplicaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	req.SourceRef = strings.TrimSpace(req.SourceRef)
	if req.SourceRef == "" {
		respondError(w, r, http.StatusBadRequest, "source_ref is required")
		return
	}
	template, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "template not found")
			return
		}
		h.logger.Error("load template for source replica failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if template == nil {
		respondError(w, r, http.StatusNotFound, "template not found")
		return
	}
	resolver, ok := h.vc.(templateSourceReplicaResolver)
	if !ok || resolver == nil {
		respondError(w, r, http.StatusServiceUnavailable, "vCenter source validation is unavailable")
		return
	}
	identity, err := resolver.ResolveTemplateSourceIdentity(r.Context(), req.SourceRef)
	if err != nil {
		h.logger.Warn("template source replica validation failed",
			"template_id", templateID,
			"source_ref", req.SourceRef,
			"error", err)
		respondError(w, r, http.StatusUnprocessableEntity, "source replica could not be validated")
		return
	}
	now := time.Now().UTC()
	replica := &models.TemplateSourceReplica{
		TemplateID:           templateID,
		SourceVMMoref:        identity.SourceVMMoref,
		ComputeResourceType:  identity.ComputeResourceType,
		ComputeResourceMoref: identity.ComputeResourceMoref,
		ComputeResourcePath:  identity.ComputeResourcePath,
		Status:               models.TemplateSourceReplicaReady,
		LastValidatedAt:      &now,
	}
	if err := h.db.CreateTemplateSourceReplica(r.Context(), replica); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, r, http.StatusConflict, "a source replica already exists for this template and compute resource")
			return
		}
		h.logger.Error("create template source replica failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusCreated, replica)
}

func (h *Handler) AdminDeleteTemplateSourceReplica(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	replicaID, err := uuid.Parse(chi.URLParam(r, "replicaID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid source replica id")
		return
	}
	deleted, err := h.db.DeleteTemplateSourceReplica(r.Context(), templateID, replicaID)
	if err != nil {
		if errors.Is(err, database.ErrTemplateReplicaBuildConflict) {
			respondError(w, r, http.StatusConflict,
				"source replica is owned by an active or retained replica build")
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			respondError(w, r, http.StatusConflict, "source replica is referenced by an existing VM placement")
			return
		}
		h.logger.Error("delete template source replica failed",
			"template_id", templateID,
			"replica_id", replicaID,
			"error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if !deleted {
		respondError(w, r, http.StatusNotFound, "source replica not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) AdminCreateTemplateReplicaBuild(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	var req createTemplateReplicaBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.DestinationName = strings.TrimSpace(req.DestinationName)
	if req.SourceReplicaID == uuid.Nil || req.IdempotencyKey == "" ||
		len(req.IdempotencyKey) > 128 || req.DestinationName == "" ||
		len(req.DestinationName) > 80 {
		respondError(w, r, http.StatusBadRequest,
			"source_replica_id, idempotency_key (max 128), and destination_name (max 80) are required")
		return
	}
	existing, err := h.db.GetTemplateReplicaBuildByIdempotencyKey(
		r.Context(),
		templateID,
		req.IdempotencyKey,
	)
	if err != nil {
		h.logger.Error("load idempotent replica build failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if existing != nil {
		if !replicaBuildRequestMatches(existing, req) {
			respondError(w, r, http.StatusConflict, "replica build conflicts with an existing operation")
			return
		}
		h.auditLog(r.Context(), "template.replica_build.create",
			audit.Resource("template", templateID),
			audit.Detail("build_id", existing.ID),
			audit.Detail("source_replica_id", existing.SourceReplicaID),
			audit.Detail("compute_resource", existing.ComputeResourceMoref),
			audit.Detail("host_moref", existing.HostMoref),
			audit.Detail("idempotency_replay", true),
			audit.IP(r.RemoteAddr),
		)
		respondJSON(w, http.StatusOK, existing)
		return
	}
	anchor, err := h.db.GetTemplateSourceReplica(r.Context(), templateID, req.SourceReplicaID)
	if err != nil {
		h.logger.Error("load replica build source anchor failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if anchor == nil {
		respondError(w, r, http.StatusNotFound, "source replica anchor not found")
		return
	}
	if anchor.Status != models.TemplateSourceReplicaReady {
		respondError(w, r, http.StatusConflict, "source replica anchor is not ready")
		return
	}
	resolver, ok := h.vc.(templateReplicaBuildResolver)
	if !ok || resolver == nil {
		respondError(w, r, http.StatusServiceUnavailable, "vCenter replica build validation is unavailable")
		return
	}
	target, err := resolver.ResolveReplicaBuildTarget(r.Context(), req.Target)
	if err != nil {
		h.logger.Warn("replica build target validation failed",
			"template_id", templateID,
			"source_replica_id", req.SourceReplicaID,
			"error", err)
		respondError(w, r, http.StatusUnprocessableEntity, "replica build target could not be validated")
		return
	}
	if err := resolver.ValidateReplicaBuildPrivileges(r.Context(), anchor.SourceVMMoref, *target); err != nil {
		h.logger.Warn("replica build privilege validation failed",
			"template_id", templateID,
			"source_replica_id", req.SourceReplicaID,
			"error", err)
		respondError(w, r, http.StatusUnprocessableEntity,
			"vCenter service account lacks required replica build privileges")
		return
	}
	build := &models.TemplateReplicaBuild{
		TemplateID:           templateID,
		SourceReplicaID:      &anchor.ID,
		IdempotencyKey:       req.IdempotencyKey,
		OperationID:          uuid.NewString(),
		CanaryOperationID:    uuid.NewString(),
		SourceVMMoref:        anchor.SourceVMMoref,
		SourceSnapshotName:   "base-image",
		DestinationName:      req.DestinationName,
		ComputeResourceType:  target.ComputeResourceType,
		ComputeResourceMoref: target.ComputeResourceMoref,
		ComputeResourcePath:  target.ComputeResourcePath,
		HostMoref:            target.HostMoref,
		HostName:             target.HostName,
		ResourcePoolMoref:    target.ResourcePoolMoref,
		ResourcePoolPath:     target.ResourcePoolPath,
		DatastoreMoref:       target.DatastoreMoref,
		DatastoreName:        target.DatastoreName,
		FolderMoref:          target.FolderMoref,
		FolderPath:           target.FolderPath,
		ProvisionDatastore:   target.ProvisionDatastore,
		Status:               models.TemplateReplicaBuildPending,
		Phase:                models.TemplateReplicaBuildPhasePending,
	}
	build, created, err := h.db.CreateTemplateReplicaBuild(
		r.Context(),
		build,
		middleware.UserIDFromContext(r.Context()),
	)
	if err != nil {
		if errors.Is(err, database.ErrTemplateReplicaBuildConflict) {
			respondError(w, r, http.StatusConflict, "replica build conflicts with an existing operation")
			return
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, r, http.StatusConflict, "a replica build already exists for this template and compute resource")
			return
		}
		h.logger.Error("create template replica build failed", "template_id", templateID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.auditLog(r.Context(), "template.replica_build.create",
		audit.Resource("template", templateID),
		audit.Detail("build_id", build.ID),
		audit.Detail("source_replica_id", build.SourceReplicaID),
		audit.Detail("compute_resource", build.ComputeResourceMoref),
		audit.Detail("host_moref", build.HostMoref),
		audit.Detail("idempotency_replay", !created),
		audit.IP(r.RemoteAddr),
	)
	status := http.StatusAccepted
	if !created {
		status = http.StatusOK
	}
	respondJSON(w, status, build)
}

func (h *Handler) AdminGetTemplateReplicaBuild(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	buildID, err := uuid.Parse(chi.URLParam(r, "buildID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid replica build id")
		return
	}
	build, err := h.db.GetTemplateReplicaBuild(r.Context(), templateID, buildID)
	if err != nil {
		h.logger.Error("get template replica build failed", "build_id", buildID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if build == nil {
		respondError(w, r, http.StatusNotFound, "replica build not found")
		return
	}
	respondJSON(w, http.StatusOK, build)
}

func (h *Handler) restartTemplateReplicaBuild(w http.ResponseWriter, r *http.Request, cleanupOnly bool) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	buildID, err := uuid.Parse(chi.URLParam(r, "buildID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid replica build id")
		return
	}
	build, err := h.db.RestartTemplateReplicaBuild(r.Context(), templateID, buildID, cleanupOnly)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "replica build not found")
			return
		}
		if errors.Is(err, database.ErrTemplateReplicaBuildConflict) {
			respondError(w, r, http.StatusConflict, "replica build is not recoverable through this operation")
			return
		}
		h.logger.Error("restart template replica build failed", "build_id", buildID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	action := "retry"
	if cleanupOnly {
		action = "cleanup"
	}
	h.auditLog(r.Context(), "template.replica_build."+action,
		audit.Resource("template", templateID),
		audit.Detail("build_id", buildID),
		audit.IP(r.RemoteAddr),
	)
	respondJSON(w, http.StatusAccepted, build)
}

func (h *Handler) AdminRetryTemplateReplicaBuild(w http.ResponseWriter, r *http.Request) {
	h.restartTemplateReplicaBuild(w, r, false)
}

func (h *Handler) AdminCleanupTemplateReplicaBuild(w http.ResponseWriter, r *http.Request) {
	h.restartTemplateReplicaBuild(w, r, true)
}
