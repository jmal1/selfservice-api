package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// generateSalt creates a short random hex string for VM naming.
func generateSalt() (string, error) {
	saltBytes := make([]byte, 3)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(saltBytes), nil
}

// --- Student Blueprint Endpoints ---

// ListBlueprints returns blueprints accessible to the current user.
func (h *Handler) ListBlueprints(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	blueprints, err := h.db.ListBlueprintsForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list blueprints failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, blueprints)
}

// GetBlueprint returns a single blueprint by ID.
func (h *Handler) GetBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	bp, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || bp == nil {
		respondError(w, r, http.StatusNotFound, "blueprint not found")
		return
	}

	respondJSON(w, http.StatusOK, bp)
}

// DeployBlueprint creates a new pod from a blueprint definition.
func (h *Handler) DeployBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		respondError(w, r, http.StatusBadRequest, "name is required")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify blueprint access
	bp, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || bp == nil || !bp.IsActive {
		respondError(w, r, http.StatusNotFound, "blueprint not found")
		return
	}

	// Check user has access (via ListBlueprintsForUser check)
	accessible, err := h.db.ListBlueprintsForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list blueprints failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	found := false
	for _, b := range accessible {
		if b.ID == bpID {
			found = true
			break
		}
	}
	if !found {
		respondError(w, r, http.StatusForbidden, "blueprint not accessible")
		return
	}

	// Get user for quota checks
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		respondError(w, r, http.StatusInternalServerError, "user not found")
		return
	}

	// Resolve templates for all blueprint VMs and calculate total resources
	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	templateMap := make(map[uuid.UUID]*models.Template)
	for i := range templates {
		templateMap[templates[i].ID] = &templates[i]
	}

	var totalVCPUs, totalRAM int
	type resolvedVM struct {
		DisplayName string
		TemplateID  uuid.UUID
		Template    *models.Template
		VCPUs       int
		RAMMB       int
		DiskGB      int
		BootOrder   int
	}
	var resolved []resolvedVM

	for _, bv := range bp.VMs {
		tmpl, ok := templateMap[bv.TemplateID]
		if !ok {
			respondError(w, r, http.StatusBadRequest, fmt.Sprintf("template %s not accessible", bv.TemplateID))
			return
		}

		// Defense in depth: ensure students cannot use instructor_only templates
		if role == models.RoleStudent && tmpl.Visibility == "instructor_only" {
			respondError(w, r, http.StatusForbidden, fmt.Sprintf("template %s not accessible", bv.TemplateID))
			return
		}

		vcpus := tmpl.DefaultVCPUs
		if bv.VCPUs != nil {
			vcpus = *bv.VCPUs
		}
		ram := tmpl.DefaultRAMMB
		if bv.RAMMB != nil {
			ram = *bv.RAMMB
		}
		disk := tmpl.DefaultDiskGB
		if bv.DiskGB != nil {
			disk = *bv.DiskGB
		}

		for q := 0; q < bv.Quantity; q++ {
			displayName := bv.DisplayName
			if bv.Quantity > 1 {
				displayName = fmt.Sprintf("%s-%d", bv.DisplayName, q+1)
			}
			resolved = append(resolved, resolvedVM{
				DisplayName: displayName,
				TemplateID:  bv.TemplateID,
				Template:    tmpl,
				VCPUs:       vcpus,
				RAMMB:       ram,
				DiskGB:      disk,
				BootOrder:   bv.BootOrder,
			})
			totalVCPUs += vcpus
			totalRAM += ram
		}
	}

	// Check quotas
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := ValidateQuotas(usage, user, 1, totalVCPUs, totalRAM); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
		} else {
			h.logger.Error("quota validation failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Create pod — reuse same transaction pattern as CreatePod
	podID := uuid.New()
	salt, err := generateSalt()
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Set expiration
	var expiresAt *time.Time
	switch role {
	case models.RoleStudent:
		t := time.Now().Add(7 * 24 * time.Hour)
		expiresAt = &t
	case models.RoleInstructor:
		t := time.Now().Add(30 * 24 * time.Hour)
		expiresAt = &t
	}

	tx, err := h.db.Pool().Begin(r.Context())
	if err != nil {
		h.logger.Error("begin tx failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(r.Context())

	// Insert pod with blueprint reference
	_, err = tx.Exec(r.Context(), `
		INSERT INTO pods (id, owner_id, name, salt, vlan_id, subnet, status, expires_at, blueprint_id, allow_vm_additions)
		VALUES ($1, $2, $3, $4, 0, '', 'pending', $5, $6, $7)
	`, podID, userID, req.Name, salt, expiresAt, bpID, bp.AllowVMAdditions)
	if err != nil {
		h.logger.Error("insert pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Checkout VLAN
	vlanTag, subnet, err := h.db.CheckoutVLAN(r.Context(), tx, podID, "all")
	if err != nil {
		h.logger.Error("checkout vlan failed", "error", err)
		respondError(w, r, http.StatusConflict, "no VLANs available")
		return
	}

	_, err = tx.Exec(r.Context(), `UPDATE pods SET vlan_id = $1, subnet = $2 WHERE id = $3`, vlanTag, subnet, podID)
	if err != nil {
		h.logger.Error("update pod vlan failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Create pod_vms
	type vmPayload struct {
		PodVMID      string `json:"pod_vm_id"`
		TemplateName string `json:"template_name"`
		VMName       string `json:"vm_name"`
		DisplayName  string `json:"display_name"`
		VCPUs        int    `json:"vcpus"`
		RAMMB        int    `json:"ram_mb"`
		DiskGB       int    `json:"disk_gb"`
		OSType       string `json:"os_type"`
		BootOrder    int    `json:"boot_order"`
		Kind         string `json:"kind"`
		AssignIP     bool   `json:"assign_ip"`
	}
	var vmPayloads []vmPayload

	for _, rv := range resolved {
		vmID := uuid.New()
		_, err = tx.Exec(r.Context(), `
			INSERT INTO pod_vms (id, pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status, boot_order)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8)
		`, vmID, podID, rv.TemplateID, rv.DisplayName, rv.VCPUs, rv.RAMMB, rv.DiskGB, rv.BootOrder)
		if err != nil {
			h.logger.Error("insert pod_vm failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
			return
		}

		vmName := salt + "-" + sanitizeName(rv.DisplayName)
		kind := rv.Template.Kind
		if kind == "" {
			kind = models.TemplateKindCloneWithCustomize
		}
		vmPayloads = append(vmPayloads, vmPayload{
			PodVMID:      vmID.String(),
			TemplateName: rv.Template.VCenterRef(),
			VMName:       vmName,
			DisplayName:  rv.DisplayName,
			VCPUs:        rv.VCPUs,
			RAMMB:        rv.RAMMB,
			DiskGB:       rv.DiskGB,
			OSType:       rv.Template.OSType,
			BootOrder:    rv.BootOrder,
			Kind:         kind,
			AssignIP:     rv.Template.AssignIP,
		})
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.logger.Error("commit failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Queue provisioning job
	payload, _ := json.Marshal(map[string]any{
		"pod_id":   podID.String(),
		"pod_name": req.Name,
		"user_id":  userID.String(),
		"vms":      vmPayloads,
	})
	job, err := h.db.CreateJob(r.Context(), models.JobTypePodCreate, payload)
	if err != nil {
		h.logger.Error("create job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job event", "error", err)
	}

	audit.Log(r.Context(), h.db, "blueprint.deploy",
		audit.Resource("pod", podID),
		audit.IP(r.RemoteAddr),
		audit.Detail("blueprint_id", bpID.String()),
		audit.Detail("blueprint_name", bp.Name),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"pod_id": podID,
		"status": "pending",
	})
}

// --- Admin Blueprint Endpoints ---

// AdminListBlueprints returns all blueprints (admin view).
func (h *Handler) AdminListBlueprints(w http.ResponseWriter, r *http.Request) {
	blueprints, err := h.db.ListAllBlueprints(r.Context())
	if err != nil {
		h.logger.Error("list all blueprints failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, blueprints)
}

// AdminCreateBlueprint creates a new blueprint.
func (h *Handler) AdminCreateBlueprint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		AllowVMAdditions bool   `json:"allow_vm_additions"`
		VMs              []struct {
			TemplateID  uuid.UUID `json:"template_id"`
			DisplayName string    `json:"display_name"`
			VCPUs       *int      `json:"vcpus,omitempty"`
			RAMMB       *int      `json:"ram_mb,omitempty"`
			DiskGB      *int      `json:"disk_gb,omitempty"`
			BootOrder   int       `json:"boot_order"`
			Quantity    int       `json:"quantity"`
		} `json:"vms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || len(req.VMs) == 0 {
		respondError(w, r, http.StatusBadRequest, "name and at least one VM are required")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())

	bp := &models.Blueprint{
		Name:             req.Name,
		Description:      req.Description,
		CreatedBy:        userID,
		AllowVMAdditions: req.AllowVMAdditions,
	}

	for _, v := range req.VMs {
		qty := v.Quantity
		if qty < 1 {
			qty = 1
		}
		bp.VMs = append(bp.VMs, models.BlueprintVM{
			TemplateID:  v.TemplateID,
			DisplayName: v.DisplayName,
			VCPUs:       v.VCPUs,
			RAMMB:       v.RAMMB,
			DiskGB:      v.DiskGB,
			BootOrder:   v.BootOrder,
			Quantity:    qty,
		})
	}

	if err := h.db.CreateBlueprint(r.Context(), bp); err != nil {
		h.logger.Error("create blueprint failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "blueprint.create",
		audit.Resource("blueprint", bp.ID),
		audit.IP(r.RemoteAddr),
		audit.Detail("name", bp.Name),
	)

	respondJSON(w, http.StatusCreated, bp)
}

// AdminUpdateBlueprint updates a blueprint.
func (h *Handler) AdminUpdateBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	var req struct {
		Name             string `json:"name"`
		Description      string `json:"description"`
		AllowVMAdditions bool   `json:"allow_vm_additions"`
		VMs              []struct {
			TemplateID  uuid.UUID `json:"template_id"`
			DisplayName string    `json:"display_name"`
			VCPUs       *int      `json:"vcpus,omitempty"`
			RAMMB       *int      `json:"ram_mb,omitempty"`
			DiskGB      *int      `json:"disk_gb,omitempty"`
			BootOrder   int       `json:"boot_order"`
			Quantity    int       `json:"quantity"`
		} `json:"vms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	// Same invariant the create path enforces: a blueprint must always keep a
	// name and at least one VM. Without this an update could persist an empty
	// blueprint, which the API then serializes with the `vms` key omitted
	// (json:"vms,omitempty") and crashes the admin UI's blueprints page.
	if req.Name == "" || len(req.VMs) == 0 {
		respondError(w, r, http.StatusBadRequest, "name and at least one VM are required")
		return
	}

	existing, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || existing == nil {
		respondError(w, r, http.StatusNotFound, "blueprint not found")
		return
	}

	bp := &models.Blueprint{
		ID:               bpID,
		Name:             req.Name,
		Description:      req.Description,
		AllowVMAdditions: req.AllowVMAdditions,
	}

	for _, v := range req.VMs {
		qty := v.Quantity
		if qty < 1 {
			qty = 1
		}
		bp.VMs = append(bp.VMs, models.BlueprintVM{
			BlueprintID: bpID,
			TemplateID:  v.TemplateID,
			DisplayName: v.DisplayName,
			VCPUs:       v.VCPUs,
			RAMMB:       v.RAMMB,
			DiskGB:      v.DiskGB,
			BootOrder:   v.BootOrder,
			Quantity:    qty,
		})
	}

	if err := h.db.UpdateBlueprint(r.Context(), bp); err != nil {
		h.logger.Error("update blueprint failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "blueprint.update",
		audit.Resource("blueprint", bpID),
		audit.IP(r.RemoteAddr),
	)

	// Re-fetch with VMs
	updated, _ := h.db.GetBlueprintByID(r.Context(), bpID)
	respondJSON(w, http.StatusOK, updated)
}

// --- Blueprint Pinning (migration 000030) ---

// AdminReorderBlueprints updates the pin state and ordering for blueprints.
// Instructor/admin only. Request body is a map of blueprint IDs to {pinned, pin_order}.
// All updates are atomic; either all succeed or none do. Returns 403 if role < instructor.
func (h *Handler) AdminReorderBlueprints(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	var reqBody map[string]struct {
		Pinned   bool `json:"pinned"`
		PinOrder int  `json:"pin_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	// Convert string keys to UUIDs
	req := make(map[uuid.UUID]database.PinState)
	for k, v := range reqBody {
		id, err := uuid.Parse(k)
		if err != nil {
			respondError(w, r, http.StatusBadRequest, "invalid blueprint id: "+k)
			return
		}
		req[id] = database.PinState{Pinned: v.Pinned, PinOrder: v.PinOrder}
	}

	userID := middleware.UserIDFromContext(r.Context())
	err := h.blueprintPinStore().ReorderBlueprints(r.Context(), req, userID)
	if err != nil {
		if errors.Is(err, database.ErrBlueprintNotFound) {
			h.logger.Warn("reorder blueprints target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified blueprint not found")
			return
		}
		h.logger.Error("reorder blueprints failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "blueprints.reorder",
		audit.IP(r.RemoteAddr),
		audit.Detail("count", fmt.Sprintf("%d", len(req))),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminSetBlueprintPin pins a single blueprint at the requested position.
func (h *Handler) AdminSetBlueprintPin(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	blueprintID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	var req struct {
		PinOrder int `json:"pin_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	if err := h.blueprintPinStore().SetBlueprintPin(r.Context(), blueprintID, true, req.PinOrder, userID); err != nil {
		if errors.Is(err, database.ErrBlueprintNotFound) {
			h.logger.Warn("set blueprint pin target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified blueprint not found")
			return
		}
		h.logger.Error("set blueprint pin failed", "error", err, "blueprint_id", blueprintID, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "blueprint.pin",
		audit.Resource("blueprint", blueprintID),
		audit.IP(r.RemoteAddr),
		audit.Detail("pin_order", fmt.Sprintf("%d", req.PinOrder)),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminUnpinBlueprint clears the pinned state for a single blueprint.
func (h *Handler) AdminUnpinBlueprint(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	blueprintID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	if err := h.blueprintPinStore().SetBlueprintPin(r.Context(), blueprintID, false, 0, userID); err != nil {
		if errors.Is(err, database.ErrBlueprintNotFound) {
			h.logger.Warn("unpin blueprint target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified blueprint not found")
			return
		}
		h.logger.Error("unpin blueprint failed", "error", err, "blueprint_id", blueprintID, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "blueprint.unpin",
		audit.Resource("blueprint", blueprintID),
		audit.IP(r.RemoteAddr),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminDeleteBlueprint soft-deletes a blueprint.
func (h *Handler) AdminDeleteBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	if err := h.db.DeleteBlueprint(r.Context(), bpID); err != nil {
		h.logger.Error("delete blueprint failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "blueprint.delete",
		audit.Resource("blueprint", bpID),
		audit.IP(r.RemoteAddr),
	)

	w.WriteHeader(http.StatusNoContent)
}

// AdminSetBlueprintAccess replaces access rules for a blueprint.
func (h *Handler) AdminSetBlueprintAccess(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid blueprint id")
		return
	}

	var req struct {
		Rules []struct {
			UserID *uuid.UUID `json:"user_id,omitempty"`
			Role   *string    `json:"role,omitempty"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	var rules []models.BlueprintAccess
	for _, rule := range req.Rules {
		rules = append(rules, models.BlueprintAccess{
			BlueprintID: bpID,
			UserID:      rule.UserID,
			Role:        rule.Role,
		})
	}

	if err := h.db.SetBlueprintAccess(r.Context(), bpID, rules); err != nil {
		h.logger.Error("set blueprint access failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "blueprint.access",
		audit.Resource("blueprint", bpID),
		audit.IP(r.RemoteAddr),
	)

	w.WriteHeader(http.StatusNoContent)
}
