package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// Handler holds shared dependencies for all API handlers.
type Handler struct {
	db              *database.Queries
	events          *events.Client
	vc              VCenterConsole
	vcFolders       VCenterFolderEnumerator
	logger          *slog.Logger
	allowedOrigins  []string
	templatesFolder *TemplatesFolderHandler
	// healthDeps is the dependency bag for GET /admin/health. Optional
	// fields: nil pointers cause the corresponding probe to report
	// "not_configured" instead of failing.
	healthDeps HealthDeps
}

// VCenterConsole is the interface for vCenter operations needed by the
// HTTP API layer. Backed by *vcenter.Client in production. Tests
// substitute a fake to drive specific behaviors (errors, slow calls).
//
// Methods are grouped by purpose:
//   - AcquireWebMKSTicket: powers the browser console (T3 / template
//     build console).
//   - DestroyVM: powers cleanup paths that need to delete a staging or
//     student VM as a side effect of an HTTP request (e.g. template
//     delete cascading to the in-flight build VM — see AdminDeleteTemplate).
type VCenterConsole interface {
	AcquireWebMKSTicket(ctx context.Context, moref string) (*vcenter.WebMKSTicket, error)
	// GetGuestInfo returns a non-blocking snapshot of a VM's guest state
	// (name, IP, tools status, power state). The wizard polls this for
	// the staging VM during provisioning / configuring / generalizing so
	// the instructor can SSH/RDP into the build VM without waiting.
	// Errors are surfaced; an empty IPAddress is normal during boot.
	GetGuestInfo(ctx context.Context, moref string) (*vcenter.GuestInfo, error)
	// DestroyVM powers off the VM (if running) and destroys it.
	// IDEMPOTENT: returns nil if the VM was already gone in vCenter.
	DestroyVM(ctx context.Context, moref string) error
}

// NewHandler creates a new Handler.
func NewHandler(db *database.Queries, events *events.Client, vc VCenterConsole, logger *slog.Logger, allowedOrigins []string) *Handler {
	return &Handler{db: db, events: events, vc: vc, logger: logger, allowedOrigins: allowedOrigins}
}

// WithVCenterFolders enables the admin folder-enumeration endpoint by wiring
// a VCenterFolderEnumerator (typically *vcenter.Client) and the configured
// inventory path of the Templates folder.
func (h *Handler) WithVCenterFolders(vcf VCenterFolderEnumerator, templatesFolderPath string) *Handler {
	h.vcFolders = vcf
	h.templatesFolder = NewTemplatesFolderHandler(vcf, templatesFolderPath, func(ctx context.Context) ([]vcenterTemplateRow, error) {
		ts, err := h.db.ListAllTemplates(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]vcenterTemplateRow, 0, len(ts))
		for _, t := range ts {
			out = append(out, vcenterTemplateRow{
				ID:              t.ID.String(),
				Name:            t.Name,
				VCenterTemplate: t.VCenterTemplate,
			})
		}
		return out, nil
	})
	return h
}

// AdminListVCenterTemplatesFolder returns enumerated VMs in the configured
// templates folder plus per-VM Crucible registration status. Cache TTL is 5
// minutes; pass ?refresh=true to bypass. Returns 503 if vCenter is not wired.
func (h *Handler) AdminListVCenterTemplatesFolder(w http.ResponseWriter, r *http.Request) {
	if h.templatesFolder == nil {
		http.Error(w, "vCenter folder enumeration not configured", http.StatusServiceUnavailable)
		return
	}
	h.templatesFolder.ServeHTTP(w, r)
}

// invalidateTemplatesFolderCache should be called by template
// create/update/delete handlers so a newly-registered template appears in the
// admin browser immediately on the next request.
func (h *Handler) invalidateTemplatesFolderCache() {
	if h.templatesFolder != nil {
		h.templatesFolder.cache.Invalidate()
	}
}

// --- Pod Handlers ---

// ListPods returns the user's pods (admin: all pods with owners).
func (h *Handler) ListPods(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	var pods []models.Pod
	var err error
	if role == models.RoleAdmin {
		pods, err = h.db.ListAllPods(r.Context())
	} else {
		pods, err = h.db.ListPodsByOwner(r.Context(), userID)
	}
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

// CreatePod creates the pod + VMs in the DB, then queues a provisioning job.
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
	type resolvedVM struct {
		req      models.VMRequest
		template models.Template
		vcpus    int
		ramMB    int
		diskGB   int
	}
	var resolved []resolvedVM
	var totalVCPUs, totalRAM int

	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	for _, vm := range req.VMs {
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
		ramMB := found.DefaultRAMMB
		if vm.RAMMB != nil {
			ramMB = *vm.RAMMB
		}
		diskGB := found.DefaultDiskGB
		if vm.DiskGB != nil {
			diskGB = *vm.DiskGB
		}
		totalVCPUs += vcpus
		totalRAM += ramMB
		resolved = append(resolved, resolvedVM{req: vm, template: *found, vcpus: vcpus, ramMB: ramMB, diskGB: diskGB})
	}

	if err := ValidateQuotas(usage, user, 1, totalVCPUs, totalRAM); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			http.Error(w, qe.Error(), http.StatusConflict)
		} else {
			h.logger.Error("quota validation failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
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

	// Generate a short random salt for VM naming
	saltBytes := make([]byte, 3)
	if _, err := rand.Read(saltBytes); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	salt := hex.EncodeToString(saltBytes)

	ctx := r.Context()

	// Begin transaction: create pod, checkout VLAN, create pod_vms
	tx, err := h.db.Pool().Begin(ctx)
	if err != nil {
		h.logger.Error("begin tx failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	podID := uuid.New()

	// Insert pod record first (FK target for vlan_pool.pod_id)
	var podCreatedAt, podUpdatedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO pods (id, owner_id, name, salt, vlan_id, subnet, status, expires_at)
		VALUES ($1, $2, $3, $4, 0, '', 'pending', $5)
		RETURNING created_at, updated_at
	`, podID, userID, req.Name, salt, expiresAt).Scan(
		&podCreatedAt, &podUpdatedAt,
	)
	if err != nil {
		h.logger.Error("create pod failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Checkout VLAN (now the pod exists, so FK is satisfied)
	vlanTag, subnet, err := h.db.CheckoutVLAN(ctx, tx, podID, "all")
	if err != nil {
		h.logger.Error("checkout VLAN failed", "error", err)
		http.Error(w, "no available VLAN slots", http.StatusConflict)
		return
	}

	// Update pod with allocated VLAN info
	_, err = tx.Exec(ctx, `
		UPDATE pods SET vlan_id = $1, subnet = $2 WHERE id = $3
	`, vlanTag, subnet, podID)
	if err != nil {
		h.logger.Error("create pod failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Insert pod_vm records and build worker VM specs
	type workerVMSpec struct {
		PodVMID      uuid.UUID `json:"pod_vm_id"`
		TemplateName string    `json:"template_name"`
		VMName       string    `json:"vm_name"`
		VCPUs        int32     `json:"vcpus"`
		RAMMB        int64     `json:"ram_mb"`
		DiskGB       int       `json:"disk_gb"`
		OSType       string    `json:"os_type"`
		// Kind mirrors templates.kind so the provisioner can branch between
		// clone_with_customize / clone_no_customize / registered_existing_vm
		// without re-reading the template row.
		Kind string `json:"kind"`
		// AssignIP mirrors templates.assign_ip. When false, the provisioner
		// attaches the NIC but skips WaitForIP and leaves pod_vms.ip_address
		// NULL — the guest is expected to manage its own networking.
		AssignIP bool `json:"assign_ip"`
	}
	var vmSpecs []workerVMSpec

	for _, rv := range resolved {
		var vmID uuid.UUID
		var vmCreatedAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')
			RETURNING id, created_at
		`, podID, rv.template.ID, rv.req.DisplayName, rv.vcpus, rv.ramMB, rv.diskGB).Scan(&vmID, &vmCreatedAt)
		if err != nil {
			h.logger.Error("create pod_vm failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		vmName := salt + "-" + sanitizeName(rv.req.DisplayName)
		kind := rv.template.Kind
		if kind == "" {
			kind = models.TemplateKindCloneWithCustomize
		}
		vmSpecs = append(vmSpecs, workerVMSpec{
			PodVMID:      vmID,
			TemplateName: rv.template.VCenterRef(),
			VMName:       vmName,
			VCPUs:        int32(rv.vcpus),
			RAMMB:        int64(rv.ramMB),
			DiskGB:       rv.diskGB,
			OSType:       rv.template.OSType,
			Kind:         kind,
			AssignIP:     rv.template.AssignIP,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("commit tx failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Create job payload matching worker's CreatePodPayload struct
	type jobPayload struct {
		PodID   uuid.UUID      `json:"pod_id"`
		PodName string         `json:"pod_name"`
		VMs     []workerVMSpec `json:"vms"`
		UserID  string         `json:"user_id"`
	}
	payload, _ := json.Marshal(jobPayload{
		PodID:   podID,
		PodName: req.Name,
		VMs:     vmSpecs,
		UserID:  userID.String(),
	})

	// Insert job
	job, err := h.db.CreateJob(ctx, models.JobTypePodCreate, payload)
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
	audit.Log(r.Context(), h.db, "pod.create",
		audit.Resource("job", job.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("pod_id", podID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"pod_id": podID,
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

	payload, _ := json.Marshal(map[string]string{"pod_id": podID.String(), "pod_name": pod.Name, "user_id": userID.String()})
	job, err := h.db.CreateJob(r.Context(), models.JobTypePodDestroy, payload)
	if err != nil {
		h.logger.Error("create destroy job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "pod.delete",
		audit.Resource("pod", podID),
		audit.IP(r.RemoteAddr),
		audit.Detail("pod_name", pod.Name),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// ExtendPod allows a user to extend their pod's expiration (attestation).
func (h *Handler) ExtendPod(w http.ResponseWriter, r *http.Request) {
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

	if pod.Status != models.PodStatusActive {
		http.Error(w, "pod must be active to extend", http.StatusConflict)
		return
	}

	// Calculate new expiry based on role (+7d student, +30d instructor/admin)
	var extension time.Duration
	switch role {
	case models.RoleInstructor, models.RoleAdmin:
		extension = 30 * 24 * time.Hour
	default:
		extension = 7 * 24 * time.Hour
	}
	newExpiry := time.Now().Add(extension)

	// Record attestation
	attestation := &models.PodAttestation{
		PodID:             podID,
		UserID:            userID,
		PreviousExpiresAt: pod.ExpiresAt,
		NewExpiresAt:      newExpiry,
	}
	if err := h.db.CreatePodAttestation(r.Context(), attestation); err != nil {
		h.logger.Error("create attestation failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Update pod expiry
	if err := h.db.UpdatePodExpiry(r.Context(), podID, newExpiry); err != nil {
		h.logger.Error("update pod expiry failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Log(r.Context(), h.db, "pod.extend",
		audit.Resource("pod", podID),
		audit.IP(r.RemoteAddr),
		audit.Detail("previous_expires_at", fmt.Sprintf("%v", pod.ExpiresAt)),
		audit.Detail("new_expires_at", newExpiry.Format(time.RFC3339)),
	)

	respondJSON(w, http.StatusOK, map[string]any{
		"pod_id":           podID,
		"expires_at":       newExpiry.Format(time.RFC3339),
		"extended_by_days": int(extension.Hours() / 24),
	})
}

// AdminExtendPod allows an admin to extend any pod's expiration.
func (h *Handler) AdminExtendPod(w http.ResponseWriter, r *http.Request) {
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

	if pod.Status != models.PodStatusActive {
		http.Error(w, "pod must be active to extend", http.StatusConflict)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	newExpiry := time.Now().Add(30 * 24 * time.Hour)

	attestation := &models.PodAttestation{
		PodID:             podID,
		UserID:            userID,
		PreviousExpiresAt: pod.ExpiresAt,
		NewExpiresAt:      newExpiry,
	}
	if err := h.db.CreatePodAttestation(r.Context(), attestation); err != nil {
		h.logger.Error("create attestation failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.db.UpdatePodExpiry(r.Context(), podID, newExpiry); err != nil {
		h.logger.Error("update pod expiry failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Log(r.Context(), h.db, "pod.admin_extend",
		audit.Resource("pod", podID),
		audit.IP(r.RemoteAddr),
		audit.Detail("new_expires_at", newExpiry.Format(time.RFC3339)),
	)

	respondJSON(w, http.StatusOK, map[string]any{
		"pod_id":           podID,
		"expires_at":       newExpiry.Format(time.RFC3339),
		"extended_by_days": 30,
	})
}

// DeleteVM queues a single VM destruction job.
func (h *Handler) DeleteVM(w http.ResponseWriter, r *http.Request) {
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
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Verify VM belongs to this pod
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}
	if vm.Status == models.VMStatusDeleted {
		http.Error(w, "vm is already deleted", http.StatusConflict)
		return
	}

	vmName := vm.DisplayName
	if vm.VCenterVMName != nil {
		vmName = *vm.VCenterVMName
	}
	payload, _ := json.Marshal(map[string]string{
		"pod_id":    podID.String(),
		"pod_vm_id": vmID.String(),
		"pod_name":  pod.Name,
		"vm_name":   vmName,
		"user_id":   userID.String(),
	})
	job, err := h.db.CreateJob(r.Context(), models.JobTypeVMDestroy, payload)
	if err != nil {
		h.logger.Error("create vm destroy job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.delete",
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// AddVM queues a job to add a VM to an existing pod.
func (h *Handler) AddVM(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		http.Error(w, "invalid pod id", http.StatusBadRequest)
		return
	}

	var req models.AddVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.DisplayName == "" {
		http.Error(w, "display_name is required", http.StatusBadRequest)
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

	if pod.Status != models.PodStatusActive {
		http.Error(w, "pod must be active to add VMs", http.StatusConflict)
		return
	}

	if !pod.AllowVMAdditions && role != models.RoleAdmin {
		http.Error(w, "this environment does not allow adding VMs", http.StatusForbidden)
		return
	}

	// Resolve template
	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var found *models.Template
	for _, t := range templates {
		if t.ID == req.TemplateID {
			found = &t
			break
		}
	}
	if found == nil {
		http.Error(w, "template not found or not accessible", http.StatusBadRequest)
		return
	}

	// Check quotas
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusInternalServerError)
		return
	}
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	vcpus := found.DefaultVCPUs
	if req.VCPUs != nil {
		vcpus = *req.VCPUs
	}
	ram := found.DefaultRAMMB
	if req.RAMMB != nil {
		ram = *req.RAMMB
	}
	diskGB := found.DefaultDiskGB
	if req.DiskGB != nil {
		diskGB = *req.DiskGB
	}

	if err := ValidateQuotas(usage, user, 0, vcpus, ram); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			http.Error(w, qe.Error(), http.StatusConflict)
		} else {
			h.logger.Error("quota validation failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	// Create pod_vm record
	vm := &models.PodVM{
		PodID:       podID,
		TemplateID:  req.TemplateID,
		DisplayName: req.DisplayName,
		VCPUs:       vcpus,
		RAMMB:       ram,
		DiskGB:      diskGB,
		Status:      models.VMStatusPending,
	}
	if err := h.db.CreatePodVM(r.Context(), vm); err != nil {
		h.logger.Error("create pod vm failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Queue vm_add job
	payload, _ := json.Marshal(map[string]string{
		"pod_id":        podID.String(),
		"pod_vm_id":     vm.ID.String(),
		"template_name": found.VCenterRef(),
		"vm_name":       pod.Salt + "-" + sanitizeName(req.DisplayName),
		"display_name":  req.DisplayName,
		"user_id":       userID.String(),
	})
	job, err := h.db.CreateJob(r.Context(), models.JobTypeVMAdd, payload)
	if err != nil {
		h.logger.Error("create vm add job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.add",
		audit.Resource("vm", vm.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"vm_id":  vm.ID,
		"status": "pending",
	})
}

// VMPowerAction handles start/stop/restart for a VM within a pod.
func (h *Handler) VMPowerAction(w http.ResponseWriter, r *http.Request) {
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

	// Determine action from the last path segment
	action := path.Base(r.URL.Path)
	var jobType string
	switch action {
	case "start":
		jobType = models.JobTypeVMStart
	case "stop":
		jobType = models.JobTypeVMStop
	case "restart":
		jobType = models.JobTypeVMRestart
	case "reset":
		jobType = models.JobTypeVMReset
	default:
		http.Error(w, "unknown power action", http.StatusBadRequest)
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

	// Verify VM belongs to this pod
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		http.Error(w, "vm not found in this pod", http.StatusNotFound)
		return
	}
	if vm.Status == models.VMStatusDeleted {
		http.Error(w, "vm is deleted", http.StatusConflict)
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":    podID.String(),
		"pod_vm_id": vmID.String(),
		"user_id":   userID.String(),
		"vm_name":   vm.DisplayName,
		"pod_name":  pod.Name,
	})
	job, err := h.db.CreateJob(r.Context(), jobType, payload)
	if err != nil {
		h.logger.Error("create vm power job failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm."+action,
		audit.Resource("vm", vmID),
		audit.IP(r.RemoteAddr),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// ListTemplates returns templates accessible to the current user.
// Internal templates (is_internal=true, e.g. synthetic-noop) are
// filtered out here so they don't appear in the user-facing picker
// at /pods/new. Two escape hatches keep system flows working:
//
//  1. Admins should use /admin/templates (ListAllTemplates) which
//     shows everything regardless of is_internal.
//  2. Users with an EXPLICIT per-user-id template_access grant still
//     see the template even if is_internal=true. This is how the
//     synthetic monitor user keeps access to synthetic-noop so the
//     UI E2E lifecycle check (which walks /pods/new) continues to
//     find the template card. Role-based grants alone do not bypass
//     the filter — only an explicit user_id row in template_access.
//
// The server-side pod-create / vm-add / blueprint handlers continue
// to use ListTemplatesForUser unfiltered so internal template IDs
// resolve correctly.
func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list templates failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	explicit, err := h.db.ListExplicitTemplateAccessForUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("list explicit template access failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	visible := templates[:0]
	for _, t := range templates {
		if t.IsInternal {
			if _, ok := explicit[t.ID]; !ok {
				continue
			}
		}
		visible = append(visible, t)
	}
	respondJSON(w, http.StatusOK, visible)
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

// ListMyJobs returns the current user's recent jobs.
func (h *Handler) ListMyJobs(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	jobs, err := h.db.ListJobsByUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("list user jobs failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []models.Job{}
	}
	respondJSON(w, http.StatusOK, jobs)
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

	h.invalidateTemplatesFolderCache()
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

// AdminUpdateTemplate partially updates a template.
func (h *Handler) AdminUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}

	var req models.UpdateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	tmpl, err := h.db.UpdateTemplate(r.Context(), templateID, req)
	if err != nil {
		if errors.Is(err, database.ErrTemplateStale) {
			http.Error(w, "template was modified by another user; refresh and try again", http.StatusConflict)
			return
		}
		h.logger.Error("update template failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tmpl == nil {
		http.Error(w, "template not found", http.StatusNotFound)
		return
	}

	h.invalidateTemplatesFolderCache()
	respondJSON(w, http.StatusOK, tmpl)
}

// AdminDeleteTemplate deletes a template and (if it has a staging or
// post-build VM in vCenter) destroys that VM first so we don't orphan
// it. Refuses with 409 when a worker is actively building the VM
// (states `provisioning` / `generalizing`) — the operator must wait
// for that job to settle (or cancel it) before deleting, otherwise
// the destroy here races the worker's clone/sysprep task.
//
// Order matters: destroy in vCenter FIRST, then DELETE the row. If
// the vCenter call fails we keep the row so the operator can retry —
// otherwise we'd leak the moref forever. If the row delete fails
// after a successful destroy, the next retry sees `DestroyVM` return
// nil (it's idempotent) and proceeds straight to the row delete.
func (h *Handler) AdminDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}

	tmpl, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil {
		h.logger.Error("load template for delete failed", "error", err, "template_id", templateID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if tmpl == nil {
		// Treat as already-deleted — idempotent.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if refuse, reason := templateDeleteStateRefusal(tmpl.TemplateState); refuse {
		h.writeStateConflict(w, tmpl, reason)
		return
	}

	if tmpl.VCenterVMID != "" {
		if h.vc == nil {
			// Production always wires vc; this branch protects test/dev
			// configs from silently orphaning VMs.
			h.logger.Warn("template delete: vCenter client not configured, leaving VM in place",
				"template_id", tmpl.ID, "moref", tmpl.VCenterVMID)
		} else {
			if err := h.vc.DestroyVM(r.Context(), tmpl.VCenterVMID); err != nil {
				h.logger.Error("template delete: destroy staging VM failed",
					"template_id", tmpl.ID, "moref", tmpl.VCenterVMID, "error", err)
				audit.Log(r.Context(), h.db, "template.delete_failed",
					audit.Resource("template", tmpl.ID),
					audit.IP(r.RemoteAddr),
					audit.Detail("moref", tmpl.VCenterVMID),
					audit.Detail("error", err.Error()),
				)
				// 502: an upstream system (vCenter) failed. The DB row
				// is preserved so the operator can retry.
				http.Error(w, "failed to destroy staging VM in vCenter: "+err.Error(),
					http.StatusBadGateway)
				return
			}
			h.logger.Info("template delete: destroyed staging VM",
				"template_id", tmpl.ID, "moref", tmpl.VCenterVMID)
		}
	}

	if err := h.db.DeleteTemplate(r.Context(), templateID); err != nil {
		h.logger.Error("delete template failed", "error", err, "template_id", templateID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Log(r.Context(), h.db, "template.delete",
		audit.Resource("template", tmpl.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("name", tmpl.Name),
		audit.Detail("state", tmpl.TemplateState),
		audit.Detail("moref", tmpl.VCenterVMID),
	)
	h.invalidateTemplatesFolderCache()
	w.WriteHeader(http.StatusNoContent)
}

// templateDeleteStateRefusal reports whether AdminDeleteTemplate should
// refuse the request because a worker job is currently mid-flight on
// the staging VM. Returning (false, "") means proceed.
//
// We refuse for `provisioning` (worker is cloning right now) and
// `generalizing` (worker is running sysprep / cloud-init). For all
// other states the VM is at rest — `configuring` means the operator
// is interacting with it directly via the build console, but no
// worker job is running, so it's safe to destroy. `ready` / `active`
// have a converted-to-template VM. `error` has whatever state the
// worker left behind, and the whole point of delete-with-cleanup is
// to recover from those. `draft` has no VM.
func templateDeleteStateRefusal(state string) (bool, string) {
	switch state {
	case models.TemplateStateProvisioning:
		return true, "cannot delete while provisioning; wait for the job to settle or cancel first"
	case models.TemplateStateGeneralizing:
		return true, "cannot delete while generalizing; wait for the job to settle or cancel first"
	}
	return false, ""
}

// AdminListTemplateDependents returns VMs that depend on a template's base disk.
func (h *Handler) AdminListTemplateDependents(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		http.Error(w, "invalid template id", http.StatusBadRequest)
		return
	}

	templateName, vms, err := h.db.ListTemplateDependents(r.Context(), templateID)
	if err != nil {
		h.logger.Error("list template dependents failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if vms == nil {
		vms = []models.TemplateDependentVM{}
	}

	resp := models.TemplateDependentsResponse{
		TemplateID:   templateID.String(),
		TemplateName: templateName,
		ActiveVMs:    len(vms),
		VMs:          vms,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// AdminListJobs returns all jobs.
func (h *Handler) AdminListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.db.ListAllJobs(r.Context())
	if err != nil {
		h.logger.Error("admin list jobs failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jobs = []models.Job{}
	}
	respondJSON(w, http.StatusOK, jobs)
}

// AdminListAuditLog returns audit log entries.
func (h *Handler) AdminListAuditLog(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListAuditLog(r.Context())
	if err != nil {
		h.logger.Error("admin list audit log failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []models.AuditLog{}
	}
	respondJSON(w, http.StatusOK, entries)
}

// --- VLAN Pool Admin Handlers ---

// AdminListVLANPool returns all VLAN pool entries.
func (h *Handler) AdminListVLANPool(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListVLANPool(r.Context())
	if err != nil {
		h.logger.Error("admin list vlan pool failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []models.VLANPoolEntry{}
	}
	respondJSON(w, http.StatusOK, entries)
}

// AdminAddVLAN adds a new VLAN to the pool.
func (h *Handler) AdminAddVLAN(w http.ResponseWriter, r *http.Request) {
	var req models.AddVLANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.VLANTag < 1 || req.VLANTag > 4094 || req.Subnet == "" || req.HostScope == "" {
		http.Error(w, "vlan_tag (1-4094), subnet, and host_scope are required", http.StatusBadRequest)
		return
	}

	entry, err := h.db.AddVLAN(r.Context(), req)
	if err != nil {
		h.logger.Error("add VLAN failed", "error", err)
		http.Error(w, "failed to add VLAN (may already exist)", http.StatusConflict)
		return
	}

	respondJSON(w, http.StatusCreated, entry)
}

// AdminUpdateVLAN updates a VLAN pool entry's scope.
func (h *Handler) AdminUpdateVLAN(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "vlanID")
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid vlan id", http.StatusBadRequest)
		return
	}

	var req models.UpdateVLANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	entry, err := h.db.UpdateVLAN(r.Context(), id, req)
	if err != nil {
		h.logger.Error("update VLAN failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if entry == nil {
		http.Error(w, "VLAN not found", http.StatusNotFound)
		return
	}

	respondJSON(w, http.StatusOK, entry)
}

// AdminRemoveVLAN removes a VLAN from the pool (only if unallocated).
func (h *Handler) AdminRemoveVLAN(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "vlanID")
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid vlan id", http.StatusBadRequest)
		return
	}

	if err := h.db.RemoveVLAN(r.Context(), id); err != nil {
		h.logger.Error("remove VLAN failed", "error", err)
		http.Error(w, err.Error(), http.StatusConflict)
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

// sanitizeName converts a display name to a DNS-safe slug.
func sanitizeName(name string) string {
	result := make([]byte, 0, len(name))
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
		} else if c == ' ' || c == '_' || c == '.' {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	// Trim trailing dash
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// --- Paginated Audit Log ---

// AdminSearchAuditLog returns filtered, paginated audit log entries.
func (h *Handler) AdminSearchAuditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := database.AuditLogFilter{
		Action:       q.Get("action"),
		ResourceType: q.Get("resource_type"),
	}

	if p := q.Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &filter.Page)
	}
	if pp := q.Get("per_page"); pp != "" {
		fmt.Sscanf(pp, "%d", &filter.PerPage)
	}
	if uid := q.Get("user_id"); uid != "" {
		if id, err := uuid.Parse(uid); err == nil {
			filter.UserID = &id
		}
	}
	if rid := q.Get("resource_id"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.ResourceID = &id
		}
	}
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			filter.Since = &t
		}
	}
	if u := q.Get("until"); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			filter.Until = &t
		}
	}

	page, err := h.db.ListAuditLogPaginated(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin search audit log failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if page.Entries == nil {
		page.Entries = []models.AuditLog{}
	}
	respondJSON(w, http.StatusOK, page)
}

// --- Session Admin ---

// AdminListSessions returns all active user sessions.
func (h *Handler) AdminListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.db.ListActiveSessions(r.Context())
	if err != nil {
		h.logger.Error("admin list sessions failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []database.ActiveSession{}
	}
	respondJSON(w, http.StatusOK, sessions)
}
