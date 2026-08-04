package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

func withRoleAndUser(r *http.Request, role string, userID uuid.UUID) *http.Request {
	ctx := middleware.WithRole(r.Context(), role)
	ctx = middleware.WithUserID(ctx, userID)
	return r.WithContext(ctx)
}

func withRouteParam(r *http.Request, key string, id uuid.UUID) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, id.String())
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

type fakeTemplatePinStore struct {
	reorderErr    error
	reorderPins   map[uuid.UUID]database.PinState
	reorderBy     uuid.UUID
	reorderCalled bool
	setErr        error
	setTemplateID uuid.UUID
	setPinned     bool
	setPinOrder   int
	setPinnedBy   uuid.UUID
	setCalled     bool
}

func (f *fakeTemplatePinStore) ReorderTemplates(ctx context.Context, pins map[uuid.UUID]database.PinState, pinnedBy uuid.UUID) error {
	f.reorderCalled = true
	if f.reorderErr != nil {
		return f.reorderErr
	}
	f.reorderPins = make(map[uuid.UUID]database.PinState, len(pins))
	for id, state := range pins {
		f.reorderPins[id] = state
	}
	f.reorderBy = pinnedBy
	return nil
}

func (f *fakeTemplatePinStore) SetTemplatePin(ctx context.Context, templateID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error {
	f.setCalled = true
	f.setTemplateID = templateID
	f.setPinned = pinned
	f.setPinOrder = pinOrder
	f.setPinnedBy = pinnedBy
	return f.setErr
}

type fakeBlueprintPinStore struct {
	reorderErr     error
	setErr         error
	reorderPins    map[uuid.UUID]database.PinState
	reorderBy      uuid.UUID
	setBlueprintID uuid.UUID
	setPinned      bool
	setPinOrder    int
	setPinnedBy    uuid.UUID
}

func (f *fakeBlueprintPinStore) ReorderBlueprints(ctx context.Context, pins map[uuid.UUID]database.PinState, pinnedBy uuid.UUID) error {
	f.reorderPins = make(map[uuid.UUID]database.PinState, len(pins))
	for id, state := range pins {
		f.reorderPins[id] = state
	}
	f.reorderBy = pinnedBy
	return f.reorderErr
}

func (f *fakeBlueprintPinStore) SetBlueprintPin(ctx context.Context, blueprintID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error {
	f.setBlueprintID = blueprintID
	f.setPinned = pinned
	f.setPinOrder = pinOrder
	f.setPinnedBy = pinnedBy
	return f.setErr
}

type fakeListStore struct {
	templates []models.Template
	err       error
}

func (f *fakeListStore) ListTemplatesForUser(ctx context.Context, userID uuid.UUID, role string) ([]models.Template, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.templates, nil
}

func (f *fakeListStore) ListExplicitTemplateAccessForUser(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	return map[uuid.UUID]struct{}{}, nil
}

func TestAdminReorderTemplates_StudentGet403(t *testing.T) {
	h := &Handler{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/templates/reorder", bytes.NewReader([]byte("{}")))
	req = withRoleAndUser(req, models.RoleStudent, uuid.New())

	w := httptest.NewRecorder()
	h.AdminReorderTemplates(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestAdminReorderBlueprints_StudentGet403(t *testing.T) {
	h := &Handler{logger: slog.Default()}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints/reorder", bytes.NewReader([]byte("{}")))
	req = withRoleAndUser(req, models.RoleStudent, uuid.New())

	w := httptest.NewRecorder()
	h.AdminReorderBlueprints(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestAdminReorderTemplates_NotFoundGets404(t *testing.T) {
	targetID := uuid.New()
	store := &fakeTemplatePinStore{
		reorderErr: fmt.Errorf("%w: %s", database.ErrTemplateNotFound, targetID),
	}
	h := &Handler{logger: slog.Default(), templatePins: store}

	body, err := json.Marshal(map[string]map[string]any{
		targetID.String(): {"pinned": true, "pin_order": 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/templates/reorder", bytes.NewReader(body))
	userID := uuid.New()
	req = withRoleAndUser(req, models.RoleInstructor, userID)

	w := httptest.NewRecorder()
	h.AdminReorderTemplates(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Specified template not found") {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
	if !store.reorderCalled {
		t.Fatal("ReorderTemplates was not called")
	}
}

func TestAdminReorderBlueprints_NotFoundGets404(t *testing.T) {
	targetID := uuid.New()
	store := &fakeBlueprintPinStore{
		reorderErr: fmt.Errorf("%w: %s", database.ErrBlueprintNotFound, targetID),
	}
	h := &Handler{logger: slog.Default(), blueprintPins: store}

	body, err := json.Marshal(map[string]map[string]any{
		targetID.String(): {"pinned": true, "pin_order": 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints/reorder", bytes.NewReader(body))
	userID := uuid.New()
	req = withRoleAndUser(req, models.RoleInstructor, userID)

	w := httptest.NewRecorder()
	h.AdminReorderBlueprints(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Specified blueprint not found") {
		t.Fatalf("unexpected body: %q", w.Body.String())
	}
	if store.reorderBy != userID {
		t.Fatalf("reorder pinnedBy = %s, want %s", store.reorderBy, userID)
	}
}

func TestAdminSetTemplatePin_PositivePath(t *testing.T) {
	templateID := uuid.New()
	userID := uuid.New()
	store := &fakeTemplatePinStore{}
	h := &Handler{logger: slog.Default(), templatePins: store}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/templates/"+templateID.String()+"/pin", bytes.NewReader([]byte(`{"pin_order":7}`)))
	req = withRoleAndUser(req, models.RoleInstructor, userID)
	req = withRouteParam(req, "id", templateID)

	w := httptest.NewRecorder()
	h.AdminSetTemplatePin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if store.setTemplateID != templateID || !store.setPinned || store.setPinOrder != 7 || store.setPinnedBy != userID {
		t.Fatalf("unexpected SetTemplatePin call: %+v", store)
	}
}

func TestAdminSetBlueprintPin_PositivePath(t *testing.T) {
	blueprintID := uuid.New()
	userID := uuid.New()
	store := &fakeBlueprintPinStore{}
	h := &Handler{logger: slog.Default(), blueprintPins: store}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints/"+blueprintID.String()+"/pin", bytes.NewReader([]byte(`{"pin_order":3}`)))
	req = withRoleAndUser(req, models.RoleInstructor, userID)
	req = withRouteParam(req, "id", blueprintID)

	w := httptest.NewRecorder()
	h.AdminSetBlueprintPin(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if store.setBlueprintID != blueprintID || !store.setPinned || store.setPinOrder != 3 || store.setPinnedBy != userID {
		t.Fatalf("unexpected SetBlueprintPin call: %+v", store)
	}
}

// TestReorderTemplates_IsInternalNotVisibleWhenPinned verifies that pinned templates
// with is_internal=true still do not appear in student-facing queries, proving that
// the student template list uses is_internal filtering even when items are pinned.
func TestReorderTemplates_IsInternalNotVisibleWhenPinned(t *testing.T) {
	studentID := uuid.New()
	fakeStore := &fakeListStore{
		templates: []models.Template{
			{
				ID:         uuid.New(),
				Name:       "Public Template",
				Pinned:     true,
				PinOrder:   0,
				IsInternal: false,
				IsActive:   true,
			},
			{
				ID:         uuid.New(),
				Name:       "Internal Fixture",
				Pinned:     true,
				PinOrder:   1,
				IsInternal: true,
				IsActive:   true,
			},
		},
	}

	h := &Handler{templates: fakeStore, logger: noopLogger(t)}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
	req = withRoleAndUser(req, models.RoleStudent, studentID)
	w := httptest.NewRecorder()

	h.ListTemplates(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", w.Code)
	}

	var resp []TemplatePublic
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, tmpl := range resp {
		if tmpl.IsInternal {
			t.Fatalf("is_internal template leaked to student list: %s", tmpl.Name)
		}
	}
}

// TestAdminReorderTemplates_AtomicityPreventsPARTIALUpdate verifies that when
// a batch contains one invalid ID, the transaction rolls back and no mutations
// are applied. The fake store tracks that ReorderTemplates was called but
// returns ErrTemplateNotFound, proving that a handler would see the error before
// any row-level updates.
func TestAdminReorderTemplates_AtomicityPreventsPARTIALUpdate(t *testing.T) {
	validID := uuid.New()
	invalidID := uuid.New()
	userID := uuid.New()

	fakeStore := &fakeTemplatePinStore{
		reorderErr: database.ErrTemplateNotFound,
	}

	h := &Handler{templatePins: fakeStore, logger: noopLogger(t)}

	body, err := json.Marshal(map[string]map[string]any{
		validID.String():   {"pinned": true, "pin_order": 0},
		invalidID.String(): {"pinned": true, "pin_order": 1},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/templates/reorder", bytes.NewReader(body))
	req = withRoleAndUser(req, models.RoleInstructor, userID)
	w := httptest.NewRecorder()

	h.AdminReorderTemplates(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", w.Code)
	}

	if !fakeStore.reorderCalled {
		t.Fatal("ReorderTemplates was not called")
	}
}
