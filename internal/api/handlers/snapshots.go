package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// ListVMSnapshots returns all snapshots for a VM.
func (h *Handler) ListVMSnapshots(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}

	snaps, err := h.db.ListVMSnapshots(r.Context(), vmID)
	if err != nil {
		h.logger.Error("list vm snapshots failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if snaps == nil {
		snaps = []models.VMSnapshot{}
	}
	respondJSON(w, http.StatusOK, snaps)
}

// CreateVMSnapshot enqueues a job to create a new snapshot for a VM.
func (h *Handler) CreateVMSnapshot(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	count, err := h.db.CountUserSnapshots(r.Context(), vmID)
	if err != nil {
		h.logger.Error("count user snapshots failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if count >= models.MaxUserSnapshots {
		http.Error(w, "snapshot limit reached (max 2)", http.StatusConflict)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":      podID.String(),
		"pod_vm_id":   vmID.String(),
		"name":        body.Name,
		"description": body.Description,
		"user_id":     userID.String(),
		"vm_name":     vm.DisplayName,
		"pod_name":    pod.Name,
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, models.JobTypeVMSnapshot, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			http.Error(w, "pod is not available for VM operations", http.StatusConflict)
			return
		}
		h.logger.Error("create snapshot job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.snapshot.create",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// RevertToInitial enqueues a job to revert a VM to its initial snapshot.
func (h *Handler) RevertToInitial(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}

	if vm.Status != models.VMStatusStopped {
		http.Error(w, "vm must be powered off", http.StatusConflict)
		return
	}

	snaps, err := h.db.ListVMSnapshots(r.Context(), vmID)
	if err != nil {
		h.logger.Error("list vm snapshots failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var initialSnap *models.VMSnapshot
	for i := range snaps {
		if snaps[i].IsInitial {
			initialSnap = &snaps[i]
			break
		}
	}
	if initialSnap == nil {
		http.Error(w, "no initial snapshot available", http.StatusNotFound)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":        podID.String(),
		"pod_vm_id":     vmID.String(),
		"snapshot_id":   initialSnap.ID.String(),
		"user_id":       userID.String(),
		"vm_name":       vm.DisplayName,
		"pod_name":      pod.Name,
		"snapshot_name": initialSnap.Name,
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, models.JobTypeVMRevert, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			http.Error(w, "pod is not available for VM operations", http.StatusConflict)
			return
		}
		h.logger.Error("create revert-initial job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.snapshot.revert-initial",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// RevertToSnapshot enqueues a job to revert a VM to a specific snapshot.
func (h *Handler) RevertToSnapshot(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}
	snapID, err := uuid.Parse(chi.URLParam(r, "snapID"))
	if err != nil {
		http.Error(w, "invalid snapshot id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}

	if vm.Status != models.VMStatusStopped {
		http.Error(w, "vm must be powered off", http.StatusConflict)
		return
	}

	snap, err := h.db.GetVMSnapshot(r.Context(), snapID)
	if err != nil || snap == nil {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	if snap.PodVMID != vmID {
		http.Error(w, "snapshot does not belong to this vm", http.StatusNotFound)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":        podID.String(),
		"pod_vm_id":     vmID.String(),
		"snapshot_id":   snapID.String(),
		"user_id":       userID.String(),
		"vm_name":       vm.DisplayName,
		"pod_name":      pod.Name,
		"snapshot_name": snap.Name,
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, models.JobTypeVMRevert, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			http.Error(w, "pod is not available for VM operations", http.StatusConflict)
			return
		}
		h.logger.Error("create revert job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.snapshot.revert",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// DeleteVMSnapshot enqueues a job to delete a specific (non-initial) snapshot.
func (h *Handler) DeleteVMSnapshot(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		http.Error(w, "invalid vm id", http.StatusBadRequest)
		return
	}
	snapID, err := uuid.Parse(chi.URLParam(r, "snapID"))
	if err != nil {
		http.Error(w, "invalid snapshot id", http.StatusBadRequest)
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}

	snap, err := h.db.GetVMSnapshot(r.Context(), snapID)
	if err != nil || snap == nil {
		http.Error(w, "snapshot not found", http.StatusNotFound)
		return
	}
	if snap.PodVMID != vmID {
		http.Error(w, "snapshot does not belong to this vm", http.StatusNotFound)
		return
	}
	if snap.IsInitial {
		http.Error(w, "cannot delete initial snapshot", http.StatusForbidden)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":        podID.String(),
		"pod_vm_id":     vmID.String(),
		"snapshot_id":   snapID.String(),
		"user_id":       userID.String(),
		"vm_name":       vm.DisplayName,
		"pod_name":      pod.Name,
		"snapshot_name": snap.Name,
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, models.JobTypeVMSnapshotDelete, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			http.Error(w, "pod is not available for VM operations", http.StatusConflict)
			return
		}
		h.logger.Error("create snapshot delete job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.snapshot.delete",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}
