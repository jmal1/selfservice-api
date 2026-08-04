package handlers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

// setCtxUser is a test helper to inject a user into request context.
func setCtxUser(r *http.Request, user *models.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), contextKey("user"), user))
}

// TestAdminReorderTemplates_StudentGet403 verifies that students cannot
// mutate the pin state of templates.
func TestAdminReorderTemplates_StudentGet403(t *testing.T) {
	h := &Handler{
		db:     nil, // Not needed for auth gate
		logger: slog.Default(),
	}

	// Set up a student (role=1)
	student := &models.User{
		ID:   uuid.New(),
		Role: models.RoleStudent,
	}

	// Prepare a request with empty body (we just care about the auth gate)
	body := bytes.NewReader([]byte("{}"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/templates/reorder", body)
	req = setCtxUser(req, student)

	w := httptest.NewRecorder()
	h.AdminReorderTemplates(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("student reorder: got %d, want 403", w.Code)
	}
}

// TestAdminReorderBlueprints_StudentGet403 verifies that students cannot
// mutate the pin state of blueprints.
func TestAdminReorderBlueprints_StudentGet403(t *testing.T) {
	h := &Handler{
		db:     nil,
		logger: slog.Default(),
	}

	student := &models.User{
		ID:   uuid.New(),
		Role: models.RoleStudent,
	}

	body := bytes.NewReader([]byte("{}"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints/reorder", body)
	req = setCtxUser(req, student)

	w := httptest.NewRecorder()
	h.AdminReorderBlueprints(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("student reorder blueprints: got %d, want 403", w.Code)
	}
}

// TestListTemplatesSortOrder verifies that templates are returned in the correct
// order: pinned first (by pin_order), then unpinned (by name).
// This is a unit test that demonstrates the expected behavior.
func TestListTemplatesSortOrder(t *testing.T) {
	tests := []struct {
		name      string
		templates []models.Template
		want      []string
	}{
		{
			name: "all_unpinned_sorted_by_name",
			templates: []models.Template{
				{ID: uuid.New(), Name: "Zebra"},
				{ID: uuid.New(), Name: "Alpha"},
				{ID: uuid.New(), Name: "Beta"},
			},
			want: []string{"Alpha", "Beta", "Zebra"},
		},
		{
			name: "pinned_first_then_unpinned",
			templates: []models.Template{
				{ID: uuid.New(), Name: "Zebra", Pinned: false},
				{ID: uuid.New(), Name: "Pinned-Alpha", Pinned: true, PinOrder: 1},
				{ID: uuid.New(), Name: "Alpha", Pinned: false},
				{ID: uuid.New(), Name: "Pinned-Beta", Pinned: true, PinOrder: 0},
			},
			want: []string{"Pinned-Beta", "Pinned-Alpha", "Alpha", "Zebra"},
		},
		{
			name: "pinned_same_order_sorted_by_pinned_at_desc",
			templates: []models.Template{
				{ID: uuid.New(), Name: "First", Pinned: true, PinOrder: 0, PinnedAt: timePtr(time.Unix(100, 0))},
				{ID: uuid.New(), Name: "Second", Pinned: true, PinOrder: 0, PinnedAt: timePtr(time.Unix(200, 0))},
				{ID: uuid.New(), Name: "Third", Pinned: true, PinOrder: 1},
			},
			want: []string{"Second", "First", "Third"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Sort templates according to the specification
			sorted := sortTemplatesByPin(tc.templates)

			// Verify order
			for i, want := range tc.want {
				if i >= len(sorted) {
					t.Errorf("sorted has fewer items than expected (got %d, want %d)", len(sorted), len(tc.want))
					return
				}
				if sorted[i].Name != want {
					t.Errorf("position %d: got %q, want %q", i, sorted[i].Name, want)
				}
			}
		})
	}
}

// sortTemplatesByPin mirrors the database sort logic for testing.
func sortTemplatesByPin(templates []models.Template) []models.Template {
	sorted := make([]models.Template, len(templates))
	copy(sorted, templates)

	// Bubble sort implementation that correctly implements the multi-level sort
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if shouldSwap(sorted[i], sorted[j]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	return sorted
}

// shouldSwap returns true if template a should come after template b in the sort order.
// Sort order: pinned DESC (true first), then pin_order ASC, then pinned_at DESC (NULL last), then name ASC.
func shouldSwap(a, b models.Template) bool {
	// Pinned items first
	if a.Pinned && !b.Pinned {
		return false // a is pinned, b is not → don't swap (a goes first)
	}
	if !a.Pinned && b.Pinned {
		return true // a is not pinned, b is → swap (b should go first)
	}

	// Both pinned or both unpinned: compare by pin_order if both are pinned
	if a.Pinned && b.Pinned {
		if a.PinOrder != b.PinOrder {
			return a.PinOrder > b.PinOrder // Lower pin_order goes first (ASC)
		}
		// Same pin_order: compare by pinned_at (DESC, NULL last)
		cmp := compareTime(a.PinnedAt, b.PinnedAt)
		if cmp != 0 {
			return cmp > 0 // compareTime returns -1 if a should go before b in DESC order
		}
	}

	// Unpinned or same pin state and pin_order: compare by name (ASC)
	return a.Name > b.Name
}

// compareTime compares two time pointers (DESC for pinned_at, NULL LAST).
func compareTime(a, b *time.Time) int {
	if a == nil && b == nil {
		return 0
	}
	if a == nil {
		return 1 // NULL sorts last (descending)
	}
	if b == nil {
		return -1
	}
	// Both non-nil: DESC order
	if a.After(*b) {
		return -1
	}
	if a.Before(*b) {
		return 1
	}
	return 0
}

// timePtr returns a pointer to a time.Time.
func timePtr(t time.Time) *time.Time {
	return &t
}
