package handlers

import (
	"net/http"

	"github.com/jmal1/selfservice-api/internal/models"
)

// AdminListGuestOSCatalog (GET /admin/templates/guest-os-catalog) returns the
// curated list of guest OS options the template wizard offers for an ISO
// build. It is the single source of truth behind the wizard's "Guest OS"
// dropdown so adding an OS in models.GuestOSCatalog surfaces in the UI with no
// UI change.
//
// The list is not exhaustive and not an allowlist: the wizard also lets an
// instructor type any "<name>Guest"-shaped identifier (validated server-side by
// models.ValidGuestID), so uncommon or future OSes work without a code change.
//
// Instructor-accessible (the whole /admin block is gated to RoleInstructor).
func (h *Handler) AdminListGuestOSCatalog(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]any{
		"options": models.GuestOSCatalog,
	})
}
