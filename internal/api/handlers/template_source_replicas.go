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
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type templateSourceReplicaResolver interface {
	ResolveTemplateSourceIdentity(context.Context, string) (*vcenter.TemplateSourceIdentity, error)
}

type createTemplateSourceReplicaRequest struct {
	SourceRef string `json:"source_ref"`
}

type templateSourceReplicaListResponse struct {
	ReplicaMode bool                           `json:"replica_mode"`
	Replicas    []models.TemplateSourceReplica `json:"replicas"`
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
