package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

const sharedAssessmentUnavailable = "Assessments on a Single VM are not available yet."

type createSingleVMRequest struct {
	TemplateID uuid.UUID `json:"template_id"`
	Name       string    `json:"name"`
}

// ListSingleVMs returns the caller's non-destroyed Single VMs.
func (h *Handler) ListSingleVMs(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	pods, err := h.db.ListPodsByOwner(r.Context(), userID)
	if err != nil {
		h.logger.Error("list single vms failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]models.Pod, 0)
	for _, pod := range pods {
		if pod.NetworkMode == models.NetworkModeShared {
			out = append(out, pod)
		}
	}
	respondJSON(w, http.StatusOK, out)
}

// CreateSingleVM places one VM on a stripe that already exists.
func (h *Handler) CreateSingleVM(w http.ResponseWriter, r *http.Request) {
	if h.rejectProvisioning(w, r, provisioningRouteSingleVM) {
		return
	}
	var req createSingleVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.TemplateID == uuid.Nil {
		respondError(w, r, http.StatusBadRequest, "name and template_id are required")
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		respondError(w, r, http.StatusInternalServerError, "user not found")
		return
	}
	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var found *models.Template
	for i := range templates {
		if templates[i].ID == req.TemplateID {
			found = &templates[i]
			break
		}
	}
	if found == nil || found.TemplateState != models.TemplateStateActive || found.IsInternal {
		respondError(w, r, http.StatusBadRequest, "template not found or not accessible")
		return
	}
	if role == models.RoleStudent && found.Visibility == "instructor_only" {
		respondError(w, r, http.StatusForbidden, "template not found or not accessible")
		return
	}
	if err := validateProvisioningRAM(found.DefaultRAMMB); err != nil {
		respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := ValidateQuotas(usage, user, 0, found.DefaultVCPUs, found.DefaultRAMMB); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
			return
		}
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	expiresAt, err := models.NewExpiresAt(role, time.Now())
	if err != nil {
		respondError(w, r, http.StatusForbidden, "unsupported user role")
		return
	}
	saltBytes := make([]byte, 3)
	if _, err := rand.Read(saltBytes); err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	salt := hex.EncodeToString(saltBytes)

	ctx := r.Context()
	tx, err := h.db.Pool().Begin(ctx)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx)

	owned, err := h.db.CountSharedPodsTx(ctx, tx, userID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if owned >= user.MaxSingleVMs {
		respondError(w, r, http.StatusConflict, "Single VM limit reached")
		return
	}
	networkID, stripe, err := h.db.PickSharedStripe(ctx, tx)
	if err != nil {
		if errors.Is(err, database.ErrSharedNetworksNotReady) {
			respondError(w, r, http.StatusConflict, "Single VM networks are not ready")
			return
		}
		if errors.Is(err, database.ErrSharedNetworksFull) {
			respondError(w, r, http.StatusConflict, "Single VM capacity is full")
			return
		}
		h.logger.Error("pick shared stripe failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	podID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO pods (
			id, owner_id, name, salt, vlan_id, subnet, status, expires_at,
			network_mode, shared_network_id, allow_vm_additions
		) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, 'shared', $8, false)
	`, podID, userID, req.Name, salt, stripe.VLANTag, stripe.CIDR, expiresAt, networkID); err != nil {
		h.logger.Error("create single vm pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var vmID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		RETURNING id
	`, podID, found.ID, found.Name, found.DefaultVCPUs, found.DefaultRAMMB, found.DefaultDiskGB).Scan(&vmID); err != nil {
		h.logger.Error("create single vm failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	kind := found.Kind
	if kind == "" {
		kind = models.TemplateKindCloneWithCustomize
	}
	payload, err := json.Marshal(map[string]any{
		"pod_id":   podID,
		"pod_name": req.Name,
		"user_id":  userID.String(),
		"vms": []map[string]any{{
			"pod_vm_id":     vmID,
			"template_name": found.VCenterRef(),
			"vm_name":       salt + "-" + sanitizeName(found.Name),
			"vcpus":         found.DefaultVCPUs,
			"ram_mb":        found.DefaultRAMMB,
			"disk_gb":       found.DefaultDiskGB,
			"os_type":       found.OSType,
			"kind":          kind,
			"assign_ip":     found.AssignIP,
		}},
	})
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	job, err := h.db.CreatePodCreateJobTx(ctx, tx, podID, payload)
	if err != nil {
		h.logger.Error("queue single vm failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to queue provisioning job")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if h.jobEvents != nil {
		if err := h.jobEvents.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
		}
	}
	audit.Log(r.Context(), h.db, "single_vm.create",
		audit.Resource("job", job.ID),
		audit.FromRequest(r),
		audit.Detail("pod_id", podID.String()),
	)
	respondJSON(w, http.StatusAccepted, map[string]any{
		"pod_id": podID,
		"job_id": job.ID,
		"vlan":   stripe.VLANTag,
	})
}

// AdminProvisionSharedNetworks inserts the nine stripe rows and enqueues the
// one job that builds them. It does not build a network itself.
func (h *Handler) AdminProvisionSharedNetworks(w http.ResponseWriter, r *http.Request) {
	if err := h.db.EnsureSharedNetworks(r.Context()); err != nil {
		h.logger.Error("ensure shared networks failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	job, err := h.db.CreateJob(r.Context(), models.JobTypeSharedNetworkProvision, []byte(`{}`))
	if err != nil {
		h.logger.Error("queue shared network provision failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to queue provisioning job")
		return
	}
	if h.jobEvents != nil {
		if err := h.jobEvents.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
		}
	}
	respondJSON(w, http.StatusAccepted, map[string]any{"job_id": job.ID})
}

func rejectSharedAssessment(w http.ResponseWriter, r *http.Request, pod *models.Pod) bool {
	if pod == nil || pod.NetworkMode != models.NetworkModeShared {
		return false
	}
	respondError(w, r, http.StatusConflict, sharedAssessmentUnavailable)
	return true
}
