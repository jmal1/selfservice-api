package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	respondJSON(w, http.StatusOK, blueprints)
}

// GetBlueprint returns a single blueprint by ID.
func (h *Handler) GetBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		http.Error(w, "invalid blueprint id", http.StatusBadRequest)
		return
	}

	bp, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || bp == nil {
		http.Error(w, "blueprint not found", http.StatusNotFound)
		return
	}

	respondJSON(w, http.StatusOK, bp)
}

// DeployBlueprint creates a new pod from a blueprint definition.
func (h *Handler) DeployBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		http.Error(w, "invalid blueprint id", http.StatusBadRequest)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Verify blueprint access
	bp, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || bp == nil || !bp.IsActive {
		http.Error(w, "blueprint not found", http.StatusNotFound)
		return
	}

	// Check user has access (via ListBlueprintsForUser check)
	accessible, err := h.db.ListBlueprintsForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list blueprints failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "blueprint not accessible", http.StatusForbidden)
		return
	}

	// Get user for quota checks
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		http.Error(w, "user not found", http.StatusInternalServerError)
		return
	}

	// Resolve templates for all blueprint VMs and calculate total resources
	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
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
			http.Error(w, fmt.Sprintf("template %s not accessible", bv.TemplateID), http.StatusBadRequest)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if usage.ActivePods+1 > user.MaxPods {
		http.Error(w, "pod quota exceeded", http.StatusConflict)
		return
	}
	if usage.UsedVCPUs+totalVCPUs > user.MaxVCPUs {
		http.Error(w, "vCPU quota exceeded", http.StatusConflict)
		return
	}
	if usage.UsedRAMMB+totalRAM > user.MaxRAMMB {
		http.Error(w, "RAM quota exceeded", http.StatusConflict)
		return
	}

	// Create pod — reuse same transaction pattern as CreatePod
	podID := uuid.New()
	salt, err := generateSalt()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Checkout VLAN
	vlanTag, subnet, err := h.db.CheckoutVLAN(r.Context(), tx, podID, "all")
	if err != nil {
		h.logger.Error("checkout vlan failed", "error", err)
		http.Error(w, "no VLANs available", http.StatusConflict)
		return
	}

	_, err = tx.Exec(r.Context(), `UPDATE pods SET vlan_id = $1, subnet = $2 WHERE id = $3`, vlanTag, subnet, podID)
	if err != nil {
		h.logger.Error("update pod vlan failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		vmName := salt + "-" + sanitizeName(rv.DisplayName)
		vmPayloads = append(vmPayloads, vmPayload{
			PodVMID:      vmID.String(),
			TemplateName: rv.Template.VCenterTemplate,
			VMName:       vmName,
			DisplayName:  rv.DisplayName,
			VCPUs:        rv.VCPUs,
			RAMMB:        rv.RAMMB,
			DiskGB:       rv.DiskGB,
			OSType:       rv.Template.OSType,
			BootOrder:    rv.BootOrder,
		})
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.logger.Error("commit failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" || len(req.VMs) == 0 {
		http.Error(w, "name and at least one VM are required", http.StatusBadRequest)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "invalid blueprint id", http.StatusBadRequest)
		return
	}

	existing, err := h.db.GetBlueprintByID(r.Context(), bpID)
	if err != nil || existing == nil {
		http.Error(w, "blueprint not found", http.StatusNotFound)
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
		http.Error(w, "invalid request body", http.StatusBadRequest)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
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

// AdminDeleteBlueprint soft-deletes a blueprint.
func (h *Handler) AdminDeleteBlueprint(w http.ResponseWriter, r *http.Request) {
	bpID, err := uuid.Parse(chi.URLParam(r, "blueprintID"))
	if err != nil {
		http.Error(w, "invalid blueprint id", http.StatusBadRequest)
		return
	}

	if err := h.db.DeleteBlueprint(r.Context(), bpID); err != nil {
		h.logger.Error("delete blueprint failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
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
		http.Error(w, "invalid blueprint id", http.StatusBadRequest)
		return
	}

	var req struct {
		Rules []struct {
			UserID *uuid.UUID `json:"user_id,omitempty"`
			Role   *string    `json:"role,omitempty"`
		} `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	audit.Log(r.Context(), h.db, "blueprint.access",
		audit.Resource("blueprint", bpID),
		audit.IP(r.RemoteAddr),
	)

	w.WriteHeader(http.StatusNoContent)
}
