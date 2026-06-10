package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
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
	got := h.wizardState(context.Background(), nil)
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
	got := h.wizardState(context.Background(), tmpl)
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

// fakeVC is a tiny in-memory VCenterConsole used by wizardState tests
// that exercise the Phase H build-VM access fields. It records calls so
// tests can assert wizardState() didn't query vCenter outside the
// build-time states (cost: a wasted SOAP round-trip per poll otherwise).
type fakeVC struct {
	info     *vcenter.GuestInfo
	err      error
	gotCalls []string
}

func (f *fakeVC) AcquireWebMKSTicket(_ context.Context, moref string) (*vcenter.WebMKSTicket, error) {
	f.gotCalls = append(f.gotCalls, "ticket:"+moref)
	return nil, errors.New("not used in these tests")
}

func (f *fakeVC) GetGuestInfo(_ context.Context, moref string) (*vcenter.GuestInfo, error) {
	f.gotCalls = append(f.gotCalls, "info:"+moref)
	return f.info, f.err
}

func (f *fakeVC) DestroyVM(_ context.Context, moref string) error {
	f.gotCalls = append(f.gotCalls, "destroy:"+moref)
	return nil
}

func TestWizardStatePopulatesBuildVMFields(t *testing.T) {
	vc := &fakeVC{
		info: &vcenter.GuestInfo{
			Name:         "tpl-windows11-ab12cd",
			IPAddress:    "10.10.30.42",
			ToolsRunning: true,
			PoweredOn:    true,
		},
	}
	h := &Handler{vc: vc, logger: noopLogger(t)}
	tmpl := &models.Template{
		ID:              uuid.New(),
		TemplateState:   models.TemplateStateConfiguring,
		VCenterVMID:     "vm-9001",
		OSType:          "windows",
		Kind:            models.TemplateKindCloneWithCustomize,
		DefaultUsername: "Student",
		DefaultPassword: "Changeme123!",
	}
	got := h.wizardState(context.Background(), tmpl)
	if got.BuildVMName != "tpl-windows11-ab12cd" {
		t.Errorf("BuildVMName = %q; want tpl-windows11-ab12cd", got.BuildVMName)
	}
	if got.BuildVMIP != "10.10.30.42" {
		t.Errorf("BuildVMIP = %q; want 10.10.30.42", got.BuildVMIP)
	}
	if !got.BuildVMTools {
		t.Errorf("BuildVMTools = false; want true")
	}
	if !got.BuildVMPowerOn {
		t.Errorf("BuildVMPowerOn = false; want true")
	}
	if got.OSType != "windows" {
		t.Errorf("OSType = %q; want windows", got.OSType)
	}
	if got.TemplateKind != models.TemplateKindCloneWithCustomize {
		t.Errorf("TemplateKind = %q; want %q",
			got.TemplateKind, models.TemplateKindCloneWithCustomize)
	}
	if got.DefaultUsername != "Student" {
		t.Errorf("DefaultUsername = %q; want Student", got.DefaultUsername)
	}
	if got.DefaultPassword != "Changeme123!" {
		t.Errorf("DefaultPassword = %q; want Changeme123!", got.DefaultPassword)
	}
	if len(vc.gotCalls) != 1 || vc.gotCalls[0] != "info:vm-9001" {
		t.Errorf("expected one info:vm-9001 call; got %v", vc.gotCalls)
	}
}

func TestWizardStateSkipsVCenterOutsideBuildStates(t *testing.T) {
	// The point of isBuildState() is to avoid hammering vCenter on every
	// 5-s wizard poll for templates that aren't actively being built. We
	// assert here that draft + ready + active never trigger a GetGuestInfo.
	for _, state := range []string{
		models.TemplateStateDraft,
		models.TemplateStateReady,
		models.TemplateStateActive,
	} {
		t.Run(state, func(t *testing.T) {
			vc := &fakeVC{}
			h := &Handler{vc: vc, logger: noopLogger(t)}
			tmpl := &models.Template{
				ID:            uuid.New(),
				TemplateState: state,
				VCenterVMID:   "vm-should-not-be-queried",
			}
			_ = h.wizardState(context.Background(), tmpl)
			if len(vc.gotCalls) != 0 {
				t.Errorf("vCenter was called in state %q: %v (want zero calls)",
					state, vc.gotCalls)
			}
		})
	}
}

func TestWizardStateSurvivesVCenterError(t *testing.T) {
	// Transient vCenter failures must not break the wizard response;
	// the UI just won't see live IP/tools fields until the next poll.
	vc := &fakeVC{err: errors.New("simulated vcenter outage")}
	h := &Handler{vc: vc, logger: noopLogger(t)}
	tmpl := &models.Template{
		ID:              uuid.New(),
		TemplateState:   models.TemplateStateConfiguring,
		VCenterVMID:     "vm-broken",
		DefaultUsername: "ubuntu",
	}
	got := h.wizardState(context.Background(), tmpl)
	if got.BuildVMIP != "" || got.BuildVMName != "" {
		t.Errorf("live VM fields should be empty on vCenter error; got name=%q ip=%q",
			got.BuildVMName, got.BuildVMIP)
	}
	// The non-vCenter fields (from the template row) still come through.
	if got.DefaultUsername != "ubuntu" {
		t.Errorf("DefaultUsername should survive vCenter error; got %q",
			got.DefaultUsername)
	}
}

func noopLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
