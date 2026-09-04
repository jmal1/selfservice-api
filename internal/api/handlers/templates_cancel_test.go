package handlers

// templates_cancel_test.go — unit tests for AdminCancelTemplate.
//
// Cancel must destroy the staging VM before forgetting the moref and
// transitioning to draft. A cancel that only flips state orphans inventory
// forever until a background reconciler notices.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

type fakeCancelDB struct {
	tmpl          *models.Template
	setMorefCalls []string
	updateFrom    string
	updateTo      string
	updateCalled  bool
	setMorefErr   error
	updateErr     error
}

func (f *fakeCancelDB) GetTemplateByID(_ context.Context, _ uuid.UUID) (*models.Template, error) {
	if f.tmpl == nil {
		return nil, nil
	}
	cp := *f.tmpl
	return &cp, nil
}

func (f *fakeCancelDB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, from, to string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updateCalled = true
	f.updateFrom = from
	f.updateTo = to
	if f.tmpl != nil {
		f.tmpl.TemplateState = to
	}
	return nil
}

func (f *fakeCancelDB) CreateJob(_ context.Context, _ string, _ []byte) (*models.Job, error) {
	return &models.Job{ID: uuid.New()}, nil
}

func (f *fakeCancelDB) SetTemplateVCenterVM(_ context.Context, _ uuid.UUID, vcenterVMID string) error {
	if f.setMorefErr != nil {
		return f.setMorefErr
	}
	f.setMorefCalls = append(f.setMorefCalls, vcenterVMID)
	if f.tmpl != nil {
		f.tmpl.VCenterVMID = vcenterVMID
	}
	return nil
}

func doCancelRequest(t *testing.T, h *Handler, templateID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/cancel", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()
	h.AdminCancelTemplate(w, req)
	return w
}

func TestAdminCancelTemplate_DestroysStagingVMAndClearsMoref(t *testing.T) {
	tmpl := &models.Template{
		ID:            uuid.New(),
		Name:          "cancel-me",
		TemplateState: models.TemplateStateConfiguring,
		VCenterVMID:   "vm-9001",
	}
	db := &fakeCancelDB{tmpl: tmpl}
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: db, logger: noopLogger(t)}

	w := doCancelRequest(t, h, tmpl.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 1 || vc.gotCalls[0] != "destroy:vm-9001" {
		t.Fatalf("vc calls = %v; want [destroy:vm-9001]", vc.gotCalls)
	}
	if len(db.setMorefCalls) != 1 || db.setMorefCalls[0] != "" {
		t.Fatalf("SetTemplateVCenterVM calls = %v; want [\"\"]", db.setMorefCalls)
	}
	if !db.updateCalled || db.updateFrom != models.TemplateStateConfiguring || db.updateTo != models.TemplateStateDraft {
		t.Fatalf("lifecycle update = called=%v %s→%s; want configuring→draft",
			db.updateCalled, db.updateFrom, db.updateTo)
	}

	var body struct {
		State   WizardStateResponse `json:"state"`
		Warning string              `json:"warning"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Warning != "" {
		t.Errorf("warning = %q; want empty after successful destroy", body.Warning)
	}
	if body.State.TemplateState != models.TemplateStateDraft {
		t.Errorf("state = %q; want draft", body.State.TemplateState)
	}
	if body.State.VCenterVMID != "" {
		t.Errorf("VCenterVMID = %q; want empty after clear", body.State.VCenterVMID)
	}
}

func TestAdminCancelTemplate_DestroyFailureKeepsState(t *testing.T) {
	tmpl := &models.Template{
		ID:            uuid.New(),
		Name:          "cancel-fail",
		TemplateState: models.TemplateStateReady,
		VCenterVMID:   "vm-bad",
	}
	db := &fakeCancelDB{tmpl: tmpl}
	vc := &fakeVC{destroyErr: errors.New("vcenter down")}
	h := &Handler{vc: vc, provDB: db, logger: noopLogger(t)}

	w := doCancelRequest(t, h, tmpl.ID)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 (body: %s)", w.Code, w.Body.String())
	}
	if db.updateCalled {
		t.Fatal("lifecycle must not advance when DestroyVM fails")
	}
	if len(db.setMorefCalls) != 0 {
		t.Fatalf("moref must not be cleared when DestroyVM fails; got %v", db.setMorefCalls)
	}
	if tmpl.TemplateState != models.TemplateStateReady {
		t.Errorf("template state mutated to %q; want ready", tmpl.TemplateState)
	}
}

func TestAdminCancelTemplate_NoStagingVMStillDrafts(t *testing.T) {
	tmpl := &models.Template{
		ID:            uuid.New(),
		Name:          "cancel-empty",
		TemplateState: models.TemplateStateConfiguring,
	}
	db := &fakeCancelDB{tmpl: tmpl}
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: db, logger: noopLogger(t)}

	w := doCancelRequest(t, h, tmpl.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 0 {
		t.Fatalf("vc calls = %v; want none when no staging VM", vc.gotCalls)
	}
	if !db.updateCalled {
		t.Fatal("expected draft transition")
	}
}
