package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/jmal1/selfservice-api/internal/database"
)

// AdminListTemplateHealth returns the health state for all student-visible
// templates. Each row reports health_status, consecutive_failures, last_structural
// and last_deep check timestamps, and the last error message if any.
//
// Auth: RoleInstructor+ (enforced at the route level in routes.go — this
// handler should never be reached by a student caller). The
// template_health_status_rbac synthetic check asserts a 403 for student-role
// callers continuously to guard this invariant.
//
// Templates that have never been checked yet appear in the response with
// health_status="unknown" and nil timestamps, so a newly-published template
// is visible immediately rather than silently absent.
//
// The response is always 200 — a partial or empty list is not an error.
// Callers must not treat "no rows" as "all healthy"; it means the checker
// has not run yet or there are no active templates.
func (h *Handler) AdminListTemplateHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	states, err := h.db.ListTemplateHealthStates(ctx)
	if err != nil {
		h.logger.Error("list template health states failed", "error", err)
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Always encode an array (never JSON null).
	if states == nil {
		states = []database.TemplateHealthState{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(states)
}
