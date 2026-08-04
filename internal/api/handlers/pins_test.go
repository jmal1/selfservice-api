package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	setErr        error
	reorderPins   map[uuid.UUID]database.PinState
	reorderBy     uuid.UUID
	setTemplateID uuid.UUID
	setPinned     bool
	setPinOrder   int
	setPinnedBy   uuid.UUID
}

func (f *fakeTemplatePinStore) ReorderTemplates(ctx context.Context, pins map[uuid.UUID]database.PinState, pinnedBy uuid.UUID) error {
	f.reorderPins = make(map[uuid.UUID]database.PinState, len(pins))
	for id, state := range pins {
		f.reorderPins[id] = state
	}
	f.reorderBy = pinnedBy
	return f.reorderErr
}

func (f *fakeTemplatePinStore) SetTemplatePin(ctx context.Context, templateID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error {
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
	if store.reorderBy != userID {
		t.Fatalf("reorder pinnedBy = %s, want %s", store.reorderBy, userID)
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

func TestPinningSourceAssertions(t *testing.T) {
	queryBytes, err := os.ReadFile("..\\..\\database\\queries.go")
	if err != nil {
		t.Fatal(err)
	}
	routeBytes, err := os.ReadFile("..\\routes\\routes.go")
	if err != nil {
		t.Fatal(err)
	}

	querySrc := string(queryBytes)
	routeSrc := string(routeBytes)
	mustContain := func(src, needle string) {
		t.Helper()
		if !strings.Contains(src, needle) {
			t.Fatalf("missing %q", needle)
		}
	}

	mustContain(querySrc, `var ErrTemplateNotFound = errors.New("template not found")`)
	mustContain(querySrc, `var ErrBlueprintNotFound = errors.New("blueprint not found")`)
	mustContain(querySrc, `pinned, pin_order, pinned_at, pinned_by`)
	mustContain(querySrc, `CASE WHEN $1 AND NOT pinned THEN now()`)
	mustContain(querySrc, `CASE WHEN $1 AND NOT pinned THEN $3`)
	mustContain(querySrc, `func (q *Queries) ReorderTemplates(ctx context.Context, pins map[uuid.UUID]PinState, pinnedBy uuid.UUID) error`)
	mustContain(querySrc, `func (q *Queries) ReorderBlueprints(ctx context.Context, pins map[uuid.UUID]PinState, pinnedBy uuid.UUID) error`)
	mustContain(querySrc, `func (q *Queries) SetTemplatePin(ctx context.Context, templateID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error`)
	mustContain(querySrc, `func (q *Queries) SetBlueprintPin(ctx context.Context, blueprintID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error`)
	mustContain(routeSrc, `r.Post("/{id}/pin", h.AdminSetTemplatePin)`)
	mustContain(routeSrc, `r.Delete("/{id}/pin", h.AdminUnpinTemplate)`)
	mustContain(routeSrc, `r.Post("/blueprints/{id}/pin", h.AdminSetBlueprintPin)`)
	mustContain(routeSrc, `r.Delete("/blueprints/{id}/pin", h.AdminUnpinBlueprint)`)
}
