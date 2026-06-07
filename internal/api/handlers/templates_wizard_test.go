package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// These tests cover the pure-function bits of templates_wizard.go that
// don't need a database fixture. Full DB-backed handler tests live under
// the integration suite.

func TestBuildTemplateVMName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantStem string
	}{
		{"basic", "Ubuntu 24.04 LTS", "tpl-ubuntu-24-04-lts-"},
		{"empty", "", "tpl-tpl-"},
		{"only specials", "@@@!!!###", "tpl-tpl-"},
		{"mixed case + spaces", "Kali Linux 2024", "tpl-kali-linux-2024-"},
		{"truncates long names", strings.Repeat("a", 200), "tpl-" + strings.Repeat("a", 40) + "-"},
		{"strips trailing dash", "Ubuntu - ", "tpl-ubuntu-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTemplateVMName(tt.input)
			if !strings.HasPrefix(got, tt.wantStem) {
				t.Errorf("buildTemplateVMName(%q) = %q; want prefix %q",
					tt.input, got, tt.wantStem)
			}
			// Suffix is 6 hex chars: tpl- + stem + 6 chars
			suffix := strings.TrimPrefix(got, tt.wantStem)
			if len(suffix) != 6 {
				t.Errorf("buildTemplateVMName(%q) = %q; suffix %q is %d chars, want 6",
					tt.input, got, suffix, len(suffix))
			}
			// Every char must be vCenter-name-safe.
			for _, c := range got {
				ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
				if !ok {
					t.Errorf("buildTemplateVMName(%q) = %q; contains unsafe char %q",
						tt.input, got, c)
				}
			}
		})
	}
}

func TestBuildTemplateVMNameUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name := buildTemplateVMName("collision-test")
		if seen[name] {
			t.Fatalf("buildTemplateVMName produced duplicate %q on iter %d", name, i)
		}
		seen[name] = true
	}
}

func TestIfNonEmpty(t *testing.T) {
	if got := ifNonEmpty("", "msg"); got != "" {
		t.Errorf("ifNonEmpty(empty) = %q; want empty", got)
	}
	if got := ifNonEmpty("x", "msg"); got != "msg" {
		t.Errorf("ifNonEmpty(non-empty) = %q; want %q", got, "msg")
	}
}

func TestWriteStateConflict(t *testing.T) {
	h := &Handler{} // pure-function path; no deps
	tmpl := &models.Template{
		ID:            uuid.New(),
		TemplateState: models.TemplateStateDraft,
	}
	rec := httptest.NewRecorder()
	h.writeStateConflict(rec, tmpl, "test reason")

	if rec.Code != 409 {
		t.Errorf("status = %d; want 409", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q; want application/json", ct)
	}
	var body struct {
		Error             string   `json:"error"`
		Reason            string   `json:"reason"`
		CurrentState      string   `json:"current_state"`
		AllowedNextStates []string `json:"allowed_next_states"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Error != "state_conflict" {
		t.Errorf("error = %q; want state_conflict", body.Error)
	}
	if body.Reason != "test reason" {
		t.Errorf("reason = %q; want test reason", body.Reason)
	}
	if body.CurrentState != models.TemplateStateDraft {
		t.Errorf("current_state = %q; want %q", body.CurrentState, models.TemplateStateDraft)
	}
	// draft → provisioning, errored are allowed
	if len(body.AllowedNextStates) == 0 {
		t.Errorf("allowed_next_states is empty; want non-empty for draft")
	}
	var sawProvisioning bool
	for _, s := range body.AllowedNextStates {
		if s == models.TemplateStateProvisioning {
			sawProvisioning = true
		}
	}
	if !sawProvisioning {
		t.Errorf("allowed_next_states %v does not contain %q",
			body.AllowedNextStates, models.TemplateStateProvisioning)
	}
}

func TestWizardStateNil(t *testing.T) {
	h := &Handler{}
	got := h.wizardState(nil)
	if got.TemplateState != "" || got.TemplateID != uuid.Nil {
		t.Errorf("wizardState(nil) = %+v; want zero value", got)
	}
}

func TestWizardStateHappy(t *testing.T) {
	h := &Handler{}
	id := uuid.New()
	tmpl := &models.Template{
		ID:             id,
		TemplateState:  models.TemplateStateConfiguring,
		VCenterVMID:    "vm-42",
		SourceType:     models.TemplateSourceCloneTemplate,
		SourceRef:      "src-tpl-uuid",
		StagingNetwork: "LabVMs-VLAN30",
	}
	got := h.wizardState(tmpl)
	if got.TemplateID != id {
		t.Errorf("TemplateID = %v; want %v", got.TemplateID, id)
	}
	if got.TemplateState != models.TemplateStateConfiguring {
		t.Errorf("TemplateState = %q; want %q",
			got.TemplateState, models.TemplateStateConfiguring)
	}
	if got.VCenterVMID != "vm-42" {
		t.Errorf("VCenterVMID = %q; want vm-42", got.VCenterVMID)
	}
	// configuring → generalizing, errored
	var sawGen bool
	for _, s := range got.AllowedNextStates {
		if s == models.TemplateStateGeneralizing {
			sawGen = true
		}
	}
	if !sawGen {
		t.Errorf("AllowedNextStates %v does not contain %q",
			got.AllowedNextStates, models.TemplateStateGeneralizing)
	}
}
