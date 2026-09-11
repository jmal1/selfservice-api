package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// TestAdminGeneralizeTemplate_SkipGeneralizeDoesNotRequireCredentials proves
// the enqueue path does not demand GuestOps credentials when the draft
// persisted skip_generalize=true.
func TestAdminGeneralizeTemplate_SkipGeneralizeDoesNotRequireCredentials(t *testing.T) {
	tmpl := &models.Template{
		Name:           "ova-skip",
		TemplateState:  models.TemplateStateConfiguring,
		SourceType:     models.TemplateSourceOVF,
		SourceRef:      "vm-123",
		VCenterVMID:    "vm-staging",
		OSType:         models.OSTypeLinux,
		SkipGeneralize: true,
	}
	db := &fakeProvDB{tmpl: tmpl, afterUpdateState: models.TemplateStateGeneralizing}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/generalize", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminGeneralizeTemplate(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 (body=%s)", w.Code, w.Body.String())
	}
	if db.createJobCalls != 1 {
		t.Fatalf("CreateJob calls = %d; want 1", db.createJobCalls)
	}
	if db.createJobType != models.JobTypeTemplateGeneralize {
		t.Errorf("job type = %q; want %q", db.createJobType, models.JobTypeTemplateGeneralize)
	}
	if db.updateFrom != models.TemplateStateConfiguring || db.updateTo != models.TemplateStateGeneralizing {
		t.Errorf("transition = %s→%s; want configuring→generalizing", db.updateFrom, db.updateTo)
	}
}

func TestAdminGeneralizeTemplate_SkipFalseStillRequiresCredentials(t *testing.T) {
	tmpl := &models.Template{
		Name:          "needs-creds",
		TemplateState: models.TemplateStateConfiguring,
		SourceType:    models.TemplateSourceCloneVCenter,
		SourceRef:     "vm-123",
		VCenterVMID:   "vm-staging",
		OSType:        models.OSTypeLinux,
	}
	db := &fakeProvDB{tmpl: tmpl}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/generalize", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminGeneralizeTemplate(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 when skip_generalize=false and creds missing", w.Code)
	}
	if db.createJobCalls != 0 {
		t.Error("CreateJob must not run when credentials are missing")
	}
}

// TestAdminPublishTemplate_SkipGeneralizeStillRequiresVerify is the publish
// gate: skip_generalize does not add a ready→active edge. Mutation tested:
// changing the enqueue destination from verifying to active fails this test.
func TestAdminPublishTemplate_SkipGeneralizeStillRequiresVerify(t *testing.T) {
	tmpl := &models.Template{
		Name:           "ova-ready",
		TemplateState:  models.TemplateStateReady,
		SourceType:     models.TemplateSourceOVF,
		SourceRef:      "vm-123",
		VCenterVMID:    "vm-staging",
		OSType:         models.OSTypeLinux,
		Kind:           models.TemplateKindCloneWithCustomize,
		DefaultUsername: "student",
		DefaultPassword: "pw",
		SkipGeneralize: true,
	}
	db := &fakeProvDB{tmpl: tmpl, afterUpdateState: models.TemplateStateVerifying}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/publish", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminPublishTemplate(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 (body=%s)", w.Code, w.Body.String())
	}
	if db.updateFrom != models.TemplateStateReady || db.updateTo != models.TemplateStateVerifying {
		t.Fatalf("transition = %s→%s; want ready→verifying (no ready→active)", db.updateFrom, db.updateTo)
	}
	if db.createJobType != models.JobTypeTemplateVerify {
		t.Errorf("job type = %q; want %q", db.createJobType, models.JobTypeTemplateVerify)
	}
	if db.updateTo == models.TemplateStateActive {
		t.Fatal("publish must not move directly to active")
	}

	var body map[string]any
	if err := json.NewDecoder(strings.NewReader(w.Body.String())).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
