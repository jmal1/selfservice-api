package handlers

// templates_power_test.go — unit tests for AdminTemplatePowerAction
// (POST /admin/templates/:id/power).
//
// The staging VM is a raw vCenter moref (tmpl.vcenter_vm_id), not a pod VM,
// so the handler talks to the vCenter client directly. These tests drive the
// handler with a fake provisionDB (no live pgxpool) and the shared fakeVC so
// we can assert the action→vCenter-call mapping, the 409 no-staging-VM gate,
// and the 400 unknown-action gate without any infrastructure.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// fakePowerDB is a minimal provisionDB that always returns the same template.
// Unlike fakeProvDB it does not mutate the returned state across calls, so the
// handler's second (fresh-state) load is deterministic.
type fakePowerDB struct {
	tmpl *models.Template
}

func (f *fakePowerDB) GetTemplateByID(_ context.Context, _ uuid.UUID) (*models.Template, error) {
	if f.tmpl == nil {
		return nil, nil
	}
	cp := *f.tmpl
	return &cp, nil
}

func (f *fakePowerDB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, _, _ string) error {
	return nil
}

func (f *fakePowerDB) CreateJob(_ context.Context, _ string, _ []byte) (*models.Job, error) {
	return &models.Job{ID: uuid.New()}, nil
}

func (f *fakePowerDB) SetTemplateVCenterVM(_ context.Context, _ uuid.UUID, vcenterVMID string) error {
	if f.tmpl != nil {
		f.tmpl.VCenterVMID = vcenterVMID
	}
	return nil
}

// powerTemplate builds a ready-state template with a staging VM moref. `ready`
// is deliberately not a build state, so wizardState() won't issue a GetGuestInfo
// call and pollute the fakeVC call log we assert on.
func powerTemplate(moref string) *models.Template {
	return &models.Template{
		ID:            uuid.New(),
		Name:          "pwr-template",
		TemplateState: models.TemplateStateReady,
		VCenterVMID:   moref,
	}
}

func doPowerRequest(t *testing.T, h *Handler, templateID uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/power", rdr)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()
	h.AdminTemplatePowerAction(w, req)
	return w
}

func TestAdminTemplatePowerAction_MapsActionsToVCenter(t *testing.T) {
	cases := []struct {
		action   string
		wantCall string
	}{
		{"start", "start:vm-42"},
		{"stop", "stop:vm-42"},
		{"restart", "restart:vm-42"},
		{"reset", "reset:vm-42"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			tmpl := powerTemplate("vm-42")
			vc := &fakeVC{}
			h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

			w := doPowerRequest(t, h, tmpl.ID, `{"action":"`+tc.action+`"}`)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 for action %q (body: %s)", w.Code, tc.action, w.Body.String())
			}
			if len(vc.gotCalls) != 1 || vc.gotCalls[0] != tc.wantCall {
				t.Errorf("vc calls = %v; want exactly [%q]", vc.gotCalls, tc.wantCall)
			}
		})
	}
}

func TestAdminTemplatePowerAction_UppercaseActionAccepted(t *testing.T) {
	tmpl := powerTemplate("vm-42")
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

	w := doPowerRequest(t, h, tmpl.ID, `{"action":"START"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 for normalized action (body: %s)", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 1 || vc.gotCalls[0] != "start:vm-42" {
		t.Errorf("vc calls = %v; want [start:vm-42]", vc.gotCalls)
	}
}

func TestAdminTemplatePowerAction_NoStagingVM409(t *testing.T) {
	tmpl := powerTemplate("") // empty moref → no staging VM yet
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

	w := doPowerRequest(t, h, tmpl.ID, `{"action":"start"}`)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 when template has no staging VM", w.Code)
	}
	if len(vc.gotCalls) != 0 {
		t.Errorf("vCenter must not be called when there is no staging VM; got %v", vc.gotCalls)
	}
}

func TestAdminTemplatePowerAction_UnknownAction400(t *testing.T) {
	tmpl := powerTemplate("vm-42")
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

	for _, bad := range []string{`{"action":"suspend"}`, `{"action":""}`, `{}`, ``} {
		t.Run(bad, func(t *testing.T) {
			w := doPowerRequest(t, h, tmpl.ID, bad)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 for body %q", w.Code, bad)
			}
			if len(vc.gotCalls) != 0 {
				t.Errorf("vCenter must not be called for an invalid action; got %v", vc.gotCalls)
			}
		})
	}
}

func TestAdminTemplatePowerAction_NotConfigured503(t *testing.T) {
	tmpl := powerTemplate("vm-42")
	h := &Handler{vc: nil, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

	w := doPowerRequest(t, h, tmpl.ID, `{"action":"start"}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 when vCenter is not configured", w.Code)
	}
}

func TestAdminTemplatePowerAction_VCenterErrorIs502(t *testing.T) {
	tmpl := powerTemplate("vm-42")
	vc := &fakeVC{powerErr: errors.New("boom")}
	h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: tmpl}, logger: noopLogger(t)}

	w := doPowerRequest(t, h, tmpl.ID, `{"action":"stop"}`)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 when the vCenter call fails", w.Code)
	}
	if len(vc.gotCalls) != 1 || vc.gotCalls[0] != "stop:vm-42" {
		t.Errorf("vc calls = %v; want [stop:vm-42]", vc.gotCalls)
	}
}

func TestAdminTemplatePowerAction_TemplateNotFound404(t *testing.T) {
	vc := &fakeVC{}
	h := &Handler{vc: vc, provDB: &fakePowerDB{tmpl: nil}, logger: noopLogger(t)}

	w := doPowerRequest(t, h, uuid.New(), `{"action":"start"}`)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 when the template does not exist", w.Code)
	}
	if len(vc.gotCalls) != 0 {
		t.Errorf("vCenter must not be called when the template is missing; got %v", vc.gotCalls)
	}
}
