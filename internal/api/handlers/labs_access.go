package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

const isolatedLabsDeniedMessage = "Isolated labs are not enabled for this account."

// WithLabsRequireGrant records whether students need users.labs_enabled
// before isolated lab routes succeed. The chart default is false, so a
// merged API does not lock students out until a deploy turns it on.
func (h *Handler) WithLabsRequireGrant(enabled bool) *Handler {
	h.labsRequireGrant = enabled
	return h
}

func isolatedLabsDeniedFor(role string, labsEnabled, requireGrant bool) bool {
	if !requireGrant {
		return false
	}
	switch role {
	case models.RoleInstructor, models.RoleAdmin:
		return false
	default:
		return !labsEnabled
	}
}

func (h *Handler) rejectIsolatedLabs(w http.ResponseWriter, r *http.Request, user *models.User) bool {
	if user == nil || !isolatedLabsDeniedFor(user.Role, user.LabsEnabled, h.labsRequireGrant) {
		return false
	}
	respondError(w, r, http.StatusForbidden, isolatedLabsDeniedMessage)
	return true
}

func singleVMOnlyNames(templates []*models.Template) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, t := range templates {
		if t == nil || !t.SingleVMOnly {
			continue
		}
		if _, ok := seen[t.Name]; ok {
			continue
		}
		seen[t.Name] = struct{}{}
		names = append(names, t.Name)
	}
	return names
}

func templateIDs(vms []models.BlueprintVM) []uuid.UUID {
	ids := make([]uuid.UUID, len(vms))
	for i := range vms {
		ids[i] = vms[i].TemplateID
	}
	return ids
}

func singleVMOnlyMessage(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	return "template " + strings.Join(quoted, ", ") + " can only be provisioned as a Single VM"
}

func (h *Handler) rejectSingleVMOnly(w http.ResponseWriter, r *http.Request, templates []*models.Template) bool {
	names := singleVMOnlyNames(templates)
	if len(names) == 0 {
		return false
	}
	respondError(w, r, http.StatusConflict, singleVMOnlyMessage(names))
	return true
}

func blueprintSingleVMOnlyMessage(templateName string, blueprints []string) string {
	return fmt.Sprintf("template %q is used by blueprints: %s", templateName, strings.Join(blueprints, ", "))
}

// accessPatchError reports why a student-access patch must be refused.
// A zero status means the patch is allowed.
func accessPatchError(role string, maxSingleVMs int) (int, string) {
	switch role {
	case models.RoleInstructor, models.RoleAdmin:
		return http.StatusConflict, "instructors and admins already have isolated labs"
	}
	if maxSingleVMs < 0 || maxSingleVMs > 3 {
		return http.StatusBadRequest, "max_single_vms must be between 0 and 3"
	}
	return 0, ""
}
