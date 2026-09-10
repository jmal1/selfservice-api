package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

func doGeneralizeRequest(t *testing.T, h *Handler, templateID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/generalize", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()
	h.AdminGeneralizeTemplate(w, req)
	return w
}

func TestGeneralizeAdmission(t *testing.T) {
	id := uuid.New()

	t.Run("configuring with moref is allowed", func(t *testing.T) {
		from, reason, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateConfiguring,
			VCenterVMID:   "vm-1",
		})
		if blocked {
			t.Fatalf("blocked with reason %q", reason)
		}
		if from != models.TemplateStateConfiguring {
			t.Errorf("from = %q; want configuring", from)
		}
	})

	t.Run("error with moref is allowed (re-run Generalize)", func(t *testing.T) {
		from, reason, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateError,
			VCenterVMID:   "vm-27858",
		})
		if blocked {
			t.Fatalf("blocked with reason %q; a leftover staging VM must be re-generalizable", reason)
		}
		if from != models.TemplateStateError {
			t.Errorf("from = %q; want error", from)
		}
	})

	t.Run("error without moref is refused", func(t *testing.T) {
		_, reason, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateError,
		})
		if !blocked {
			t.Fatal("error without a staging VM must not skip provision via generalize")
		}
		if !strings.Contains(reason, "retry") {
			t.Errorf("reason must point at /retry; got %q", reason)
		}
	})

	t.Run("configuring without moref is refused", func(t *testing.T) {
		_, reason, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateConfiguring,
		})
		if !blocked {
			t.Fatal("configuring without a staging VM must not enqueue generalize")
		}
		if !strings.Contains(reason, "vCenter VM") {
			t.Errorf("reason = %q; want it to name the missing VM", reason)
		}
	})

	t.Run("draft is refused", func(t *testing.T) {
		_, _, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateDraft,
			VCenterVMID:   "vm-1",
		})
		if !blocked {
			t.Fatal("draft must not generalize")
		}
	})

	t.Run("ready is refused (publish is the next step, not generalize)", func(t *testing.T) {
		_, _, blocked := generalizeAdmission(&models.Template{
			ID:            id,
			TemplateState: models.TemplateStateReady,
			VCenterVMID:   "vm-1",
		})
		if !blocked {
			t.Fatal("ready must not re-enter generalize through this handler")
		}
	})
}

// TestAdminGeneralizeTemplate_FromErrorWithStagingVM is the keep-the-VM
// recovery path: a failed Linux generalize can leave template_state=error
// with a powered-on leftover moref. Retry refuses that moref (PR #267);
// Cancel would destroy the guest. Generalize must enqueue from error.
func TestAdminGeneralizeTemplate_FromErrorWithStagingVM(t *testing.T) {
	tmpl := &models.Template{
		Name:            "linux-mint-22-3-mate",
		TemplateState:   models.TemplateStateError,
		SourceType:      models.TemplateSourceISO,
		VCenterVMID:     "vm-27858",
		OSType:          models.OSTypeLinux,
		DefaultUsername: "student",
		DefaultPassword: "Changeme123!",
	}
	db := &fakeProvDB{tmpl: tmpl, afterUpdateState: models.TemplateStateGeneralizing}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	w := doGeneralizeRequest(t, h, templateID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 (body=%s)", w.Code, w.Body.String())
	}
	if db.createJobCalls != 1 {
		t.Fatalf("CreateJob calls = %d; want 1", db.createJobCalls)
	}
	if db.createJobType != models.JobTypeTemplateGeneralize {
		t.Errorf("job type = %q; want %q", db.createJobType, models.JobTypeTemplateGeneralize)
	}
	if db.updateFrom != models.TemplateStateError || db.updateTo != models.TemplateStateGeneralizing {
		t.Errorf("transition = %s→%s; want error→generalizing", db.updateFrom, db.updateTo)
	}

	var payload map[string]any
	if err := json.Unmarshal(db.createJobPayload, &payload); err != nil {
		t.Fatalf("job payload: %v", err)
	}
	if payload["vm_moref"] != "vm-27858" {
		t.Errorf("vm_moref = %v; want vm-27858 (must reuse the leftover staging VM)", payload["vm_moref"])
	}
}

func TestAdminGeneralizeTemplate_FromErrorWithoutStagingVM(t *testing.T) {
	tmpl := &models.Template{
		Name:            "provision-never-cloned",
		TemplateState:   models.TemplateStateError,
		SourceType:      models.TemplateSourceISO,
		OSType:          models.OSTypeLinux,
		DefaultUsername: "student",
		DefaultPassword: "Changeme123!",
	}
	db := &fakeProvDB{tmpl: tmpl}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	w := doGeneralizeRequest(t, h, templateID)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", w.Code, w.Body.String())
	}
	if db.createJobCalls != 0 {
		t.Fatal("CreateJob must not run when error has no staging VM")
	}
	if db.updateCalled {
		t.Fatal("lifecycle must not advance when generalize is refused")
	}
	if !strings.Contains(w.Body.String(), "retry") {
		t.Errorf("conflict must point at /retry; got %s", w.Body.String())
	}
}

func TestAdminGeneralizeTemplate_FromDraftStillConflict(t *testing.T) {
	tmpl := &models.Template{
		Name:            "still-draft",
		TemplateState:   models.TemplateStateDraft,
		VCenterVMID:     "vm-1",
		OSType:          models.OSTypeLinux,
		DefaultUsername: "student",
		DefaultPassword: "Changeme123!",
	}
	db := &fakeProvDB{tmpl: tmpl}
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	w := doGeneralizeRequest(t, h, templateID)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 (body=%s)", w.Code, w.Body.String())
	}
	if db.createJobCalls != 0 {
		t.Fatal("CreateJob must not run from draft")
	}
}
