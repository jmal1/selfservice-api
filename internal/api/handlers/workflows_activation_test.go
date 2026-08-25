package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/workflowvalidation"
)

type workflowActivationStub struct {
	called bool
	id     uuid.UUID
	err    error
}

func (s *workflowActivationStub) ActivateWorkflow(_ context.Context, id uuid.UUID) error {
	s.called = true
	s.id = id
	return s.err
}

func activateWorkflowRequest(id uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/admin/workflows/"+id.String()+"/activate", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("workflowID", id.String())
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func TestAdminActivateWorkflowUsesValidatedActivationBoundary(t *testing.T) {
	id := uuid.New()
	store := &workflowActivationStub{}
	h := &Handler{
		workflowActivationDB: store,
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()

	h.AdminActivateWorkflow(w, activateWorkflowRequest(id))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !store.called || store.id != id {
		t.Fatalf("validated activation boundary was not called for %s", id)
	}
}

func TestAdminActivateWorkflowRejectsMalformedRunAction(t *testing.T) {
	store := &workflowActivationStub{err: &workflowvalidation.RunActionError{
		Line:    3,
		Message: `run_action uses library slug "demo-http-service-reachable" as the callable; use generated function "demo_http_service_reachable"`,
	}}
	h := &Handler{
		workflowActivationDB: store,
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()

	h.AdminActivateWorkflow(w, activateWorkflowRequest(uuid.New()))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body["error"], "line 3") ||
		!strings.Contains(body["error"], "demo_http_service_reachable") {
		t.Fatalf("response is not instructor-actionable: %q", body["error"])
	}
}

func TestAdminActivateWorkflowPreservesStatusConflict(t *testing.T) {
	store := &workflowActivationStub{err: database.ErrWorkflowNotApproved}
	h := &Handler{
		workflowActivationDB: store,
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()

	h.AdminActivateWorkflow(w, activateWorkflowRequest(uuid.New()))

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestAdminActivateWorkflowDoesNotMaskDatabaseFailureAsStatusConflict(t *testing.T) {
	store := &workflowActivationStub{err: errors.New("database unavailable")}
	h := &Handler{
		workflowActivationDB: store,
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	w := httptest.NewRecorder()

	h.AdminActivateWorkflow(w, activateWorkflowRequest(uuid.New()))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
}
