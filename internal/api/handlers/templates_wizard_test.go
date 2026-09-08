package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	tid := uuid.MustParse("61684ed3-d083-4095-aefe-fd4cfcf188ae")
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
			got := buildTemplateVMName(tt.input, tid)
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

// The suffix is derived from the template ID, so repeated calls for the SAME
// template must yield the SAME name (this is what lets a retry reuse the
// existing staging VM instead of orphaning it), while different templates get
// different suffixes.
func TestBuildTemplateVMNameDeterministic(t *testing.T) {
	tid := uuid.MustParse("61684ed3-d083-4095-aefe-fd4cfcf188ae")
	first := buildTemplateVMName("collision-test", tid)
	for i := 0; i < 100; i++ {
		if got := buildTemplateVMName("collision-test", tid); got != first {
			t.Fatalf("buildTemplateVMName not stable for same template: %q vs %q on iter %d", got, first, i)
		}
	}
	if want := "tpl-collision-test-61684e"; first != want {
		t.Errorf("buildTemplateVMName = %q; want %q (first 6 hex of template ID)", first, want)
	}

	other := buildTemplateVMName("collision-test", uuid.MustParse("00000000-1111-2222-3333-444444444444"))
	if other == first {
		t.Errorf("different templates produced the same VM name %q", first)
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
	info       *vcenter.GuestInfo
	err        error
	powerErr   error
	destroyErr error
	gotCalls   []string
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
	return f.destroyErr
}

func (f *fakeVC) PowerOnVM(_ context.Context, moref string) error {
	f.gotCalls = append(f.gotCalls, "start:"+moref)
	return f.powerErr
}

func (f *fakeVC) PowerOffVM(_ context.Context, moref string) error {
	f.gotCalls = append(f.gotCalls, "stop:"+moref)
	return f.powerErr
}

func (f *fakeVC) RestartVM(_ context.Context, moref string) error {
	f.gotCalls = append(f.gotCalls, "restart:"+moref)
	return f.powerErr
}

func (f *fakeVC) ResetVM(_ context.Context, moref string) error {
	f.gotCalls = append(f.gotCalls, "reset:"+moref)
	return f.powerErr
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

// --- new tests for ISO / unattend fields ---

// TestValidateUnattendMode covers every branch of validateUnattendMode.
// Mutation tested: removing the ValidUnattendMode guard causes the "unknown"
// case to return nil instead of an error, and the test catches it.
func TestValidateUnattendMode(t *testing.T) {
	validModes := []string{
		models.UnattendModeManual,
		models.UnattendModeCloudInitCIData,
		models.UnattendModeDebianPreseed,
		models.UnattendModeWindowsAutounattend,
	}
	for _, m := range validModes {
		if err := validateUnattendMode(m); err != nil {
			t.Errorf("validateUnattendMode(%q) = %v; want nil", m, err)
		}
	}
	// empty → nil (column default handles it)
	if err := validateUnattendMode(""); err != nil {
		t.Errorf("validateUnattendMode(\"\") = %v; want nil", err)
	}
	// unknown → error (must not be silently coerced)
	unknowns := []string{"auto", "preseed", "unattend", "MANUAL", "cloudinit"}
	for _, bad := range unknowns {
		if err := validateUnattendMode(bad); err == nil {
			t.Errorf("validateUnattendMode(%q) = nil; want error", bad)
		}
	}
}

// TestNormalizeUnattendConfig covers the round-trip contract for the wire
// format. The snake_case keys below come from unattend.Spec's JSON tags
// (hostname, username, password, locale, time_zone, apt_proxy, extra_pkgs).
// Getting these wrong fails silently — fields unmarshal to zero values with
// no error — so we assert the blob is returned byte-for-byte unchanged.
//
// Mutation tested: returning `{}` instead of the original blob causes the
// "unchanged" assertion to fail.
func TestNormalizeUnattendConfig(t *testing.T) {
	// nil / empty → normalised to {}
	for _, empty := range []json.RawMessage{nil, {}, json.RawMessage("")} {
		got, err := normalizeUnattendConfig(empty)
		if err != nil {
			t.Fatalf("normalizeUnattendConfig(empty) err = %v; want nil", err)
		}
		if string(got) != "{}" {
			t.Errorf("normalizeUnattendConfig(empty) = %q; want {}", string(got))
		}
	}

	// realistic snake_case blob — must survive unchanged (round-trip test)
	realistic := `{"hostname":"kali-build","username":"student","password":"Changeme123!","locale":"en_US.UTF-8","time_zone":"America/New_York","apt_proxy":"","extra_pkgs":["nmap","wireshark"]}`
	got, err := normalizeUnattendConfig(json.RawMessage(realistic))
	if err != nil {
		t.Fatalf("normalizeUnattendConfig(realistic) err = %v; want nil", err)
	}
	if string(got) != realistic {
		t.Errorf("normalizeUnattendConfig: blob was mutated\n got:  %s\n want: %s", string(got), realistic)
	}

	// invalid JSON → error, not a panic or silent pass-through
	if _, err := normalizeUnattendConfig(json.RawMessage(`{bad json`)); err == nil {
		t.Error("normalizeUnattendConfig({bad json}) = nil error; want error")
	}

	// The separator trap (W2-9). encoding/json matches names case-insensitively
	// but does NOT ignore separators, so each of these would previously have
	// unmarshalled to the zero value with no error at all. They must now be
	// rejected while the instructor is still looking at the form.
	for _, wrong := range []string{
		`{"timeZone":"America/New_York"}`, // camelCase instead of time_zone
		`{"TimeZone":"America/New_York"}`, // PascalCase instead of time_zone
		`{"aptProxy":"http://10.10.30.20:3142"}`,
		`{"extraPkgs":["nmap"]}`,
		`{"hostnmae":"typo"}`,           // ordinary typo
		`{"mode":"debian_preseed"}`,     // unattend_mode is the source of truth
		`{"password":"x","bogus":true}`, // one good key does not excuse a bad one
	} {
		if _, err := normalizeUnattendConfig(json.RawMessage(wrong)); err == nil {
			t.Errorf("normalizeUnattendConfig(%s) = nil error; want rejection — "+
				"this key would silently unmarshal to a zero value", wrong)
		}
	}

	// A non-object is not a valid Spec either.
	if _, err := normalizeUnattendConfig(json.RawMessage(`"just a string"`)); err == nil {
		t.Error(`normalizeUnattendConfig("just a string") = nil error; want error`)
	}
}

// TestBuildProvisionPayload asserts that all four ISO-specific fields
// (disk_gb, guest_id, unattend_mode, unattend_config) are present in the
// returned map with the values from the template row.
//
// Mutation tested: removing "disk_gb" from the returned map causes the
// disk_gb assertion below to fail (key absent → zero value).
func TestBuildProvisionPayload(t *testing.T) {
	id := uuid.New()
	config := json.RawMessage(`{"hostname":"test","locale":"en_US.UTF-8"}`)
	tmpl := &models.Template{
		ID:             id,
		SourceType:     models.TemplateSourceISO,
		SourceRef:      "[NAS-BackupsAndISOS] ISOs/kali.iso",
		StagingNetwork: "PG-VM-Lab",
		DefaultVCPUs:   2,
		DefaultRAMMB:   4096,
		DefaultDiskGB:  40,
		GuestID:        "ubuntu64Guest",
		UnattendMode:   models.UnattendModeCloudInitCIData,
		UnattendConfig: config,
	}
	vmName := "tpl-kali-ab1234"
	p := buildProvisionPayload(tmpl, vmName)

	if p["disk_gb"] != 40 {
		t.Errorf("disk_gb = %v; want 40", p["disk_gb"])
	}
	if p["guest_id"] != "ubuntu64Guest" {
		t.Errorf("guest_id = %v; want ubuntu64Guest", p["guest_id"])
	}
	if p["unattend_mode"] != models.UnattendModeCloudInitCIData {
		t.Errorf("unattend_mode = %v; want %q", p["unattend_mode"], models.UnattendModeCloudInitCIData)
	}
	// unattend_config must be the exact raw message, not re-encoded
	got, ok := p["unattend_config"].(json.RawMessage)
	if !ok {
		t.Fatalf("unattend_config is %T; want json.RawMessage", p["unattend_config"])
	}
	if string(got) != string(config) {
		t.Errorf("unattend_config = %q; want %q", string(got), string(config))
	}
	// baseline fields must still be present
	if p["vm_name"] != vmName {
		t.Errorf("vm_name = %v; want %q", p["vm_name"], vmName)
	}
	if p["source_ref"] != "[NAS-BackupsAndISOS] ISOs/kali.iso" {
		t.Errorf("source_ref = %v; want datastore path", p["source_ref"])
	}
}

// TestWizardStateISOConfiguringHint exercises the hint the operator sees
// when an ISO template is parked in `configuring` waiting for the OS
// installer to finish.
//
// Mutation tested: removing the ISO/configuring branch causes both hint
// assertions to fail (empty string instead of the expected message).
func TestWizardStateISOConfiguringHint(t *testing.T) {
	h := &Handler{}

	t.Run("manual mode gets console hint", func(t *testing.T) {
		tmpl := &models.Template{
			ID:            uuid.New(),
			TemplateState: models.TemplateStateConfiguring,
			SourceType:    models.TemplateSourceISO,
			UnattendMode:  models.UnattendModeManual,
		}
		got := h.wizardState(context.Background(), tmpl)
		if got.UnattendMode != models.UnattendModeManual {
			t.Errorf("UnattendMode = %q; want manual", got.UnattendMode)
		}
		if !strings.Contains(got.ConfiguringHint, "console") {
			t.Errorf("ConfiguringHint = %q; want it to mention console for manual mode", got.ConfiguringHint)
		}
	})

	t.Run("empty mode also gets console hint (defaults to manual)", func(t *testing.T) {
		tmpl := &models.Template{
			ID:            uuid.New(),
			TemplateState: models.TemplateStateConfiguring,
			SourceType:    models.TemplateSourceISO,
			UnattendMode:  "", // not yet normalised to 'manual' in the row
		}
		got := h.wizardState(context.Background(), tmpl)
		if !strings.Contains(got.ConfiguringHint, "console") {
			t.Errorf("ConfiguringHint = %q; want console hint for empty (manual) mode", got.ConfiguringHint)
		}
	})

	t.Run("automated mode gets hands-off hint", func(t *testing.T) {
		tmpl := &models.Template{
			ID:            uuid.New(),
			TemplateState: models.TemplateStateConfiguring,
			SourceType:    models.TemplateSourceISO,
			UnattendMode:  models.UnattendModeCloudInitCIData,
		}
		got := h.wizardState(context.Background(), tmpl)
		if !strings.Contains(got.ConfiguringHint, "automated") {
			t.Errorf("ConfiguringHint = %q; want automated hint for cloudinit_cidata", got.ConfiguringHint)
		}
	})

	t.Run("non-ISO configuring has no hint", func(t *testing.T) {
		tmpl := &models.Template{
			ID:            uuid.New(),
			TemplateState: models.TemplateStateConfiguring,
			SourceType:    models.TemplateSourceCloneTemplate,
			UnattendMode:  "",
		}
		got := h.wizardState(context.Background(), tmpl)
		if got.ConfiguringHint != "" {
			t.Errorf("ConfiguringHint = %q; want empty for non-ISO template", got.ConfiguringHint)
		}
	})
}

// TestISORequiresSourceRef verifies that ISO is no longer exempt from the
// source_ref requirement. The handler is invoked with h.db == nil; if
// validation is correct it returns 400 before any DB call. If the old
// exemption is accidentally re-introduced the handler will reach the nil
// DB and panic — causing the test to fail.
//
// Mutation tested: restoring `if req.SourceType != models.TemplateSourceISO &&
// req.SourceRef == ""` causes the handler to skip the 400 and fall through
// to the DB call, panicking with nil pointer dereference.
func TestISORequiresSourceRef(t *testing.T) {
	h := &Handler{logger: noopLogger(t)} // db == nil: any DB call panics

	body := `{"name":"Kali","os_type":"linux","source_type":"iso","source_ref":""}`
	req := httptest.NewRequest("POST", "/admin/templates/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.AdminCreateTemplateDraft(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 (ISO with empty source_ref must be rejected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "datastore") {
		t.Errorf("body = %q; want a message mentioning datastore path", rec.Body.String())
	}
}

// TestUnknownUnattendModeRejected verifies that the handler rejects an
// unknown unattend_mode with 400 before touching the DB.
//
// Mutation tested: removing the validateUnattendMode call lets the unknown
// mode reach the DB (nil → panic, test failure).
func TestUnknownUnattendModeRejected(t *testing.T) {
	h := &Handler{logger: noopLogger(t)} // db == nil

	body := `{"name":"Win11","os_type":"windows","source_type":"iso","source_ref":"[DS] ISOs/win11.iso","unattend_mode":"auto"}`
	req := httptest.NewRequest("POST", "/admin/templates/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.AdminCreateTemplateDraft(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for unknown unattend_mode", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unattend_mode") {
		t.Errorf("body = %q; want error mentioning unattend_mode", rec.Body.String())
	}
}

// TestInvalidUnattendConfigRejected verifies that syntactically broken
// unattend_config JSON returns 400.
//
// Mutation tested: removing the normalizeUnattendConfig call lets the bad
// blob reach the DB (nil → panic, test failure).
func TestInvalidUnattendConfigRejected(t *testing.T) {
	h := &Handler{logger: noopLogger(t)} // db == nil

	body := `{"name":"Deb","os_type":"linux","source_type":"iso","source_ref":"[DS] ISOs/deb.iso","unattend_config":{bad}}`
	req := httptest.NewRequest("POST", "/admin/templates/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.AdminCreateTemplateDraft(rec, req)

	// json.Decoder will catch {bad} when decoding the outer struct.
	// Either 400 at decode time or 400 at normalizeUnattendConfig is correct.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400 for invalid unattend_config JSON", rec.Code)
	}
}

// TestCredentialContractPublishError_BlocksUnusableLogins guards the publish
// gate. The cases below cover both the live Linux defect and the latent
// Windows clone_no_customize hole: both shapes boot fine, but without usable
// credentials the student only discovers the breakage after the template is
// live.
func TestCredentialContractPublishError_BlocksUnusableLogins(t *testing.T) {
	tests := []struct {
		name        string
		tmpl        models.Template
		wantBlocked bool
		wantInMsg   []string
	}{
		{
			name: "linux clone_with_customize with empty default_username is blocked",
			tmpl: models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
				DefaultPassword: "pw",
			},
			wantBlocked: true,
			wantInMsg:   []string{"default_username", "student"},
		},
		{
			name: "linux clone_with_customize with non-student default_username is blocked",
			tmpl: models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "ubuntu",
				DefaultPassword: "pw",
			},
			wantBlocked: true,
			wantInMsg:   []string{"student"},
		},
		{
			name: "windows clone_no_customize with empty default_username is blocked",
			tmpl: models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "",
				DefaultPassword: "pw",
			},
			wantBlocked: true,
			wantInMsg:   []string{"default_username", models.TemplateKindCloneNoCustomize},
		},
		{
			name: "linux clone_no_customize with empty default_username is blocked",
			tmpl: models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "",
				DefaultPassword: "pw",
			},
			wantBlocked: true,
			wantInMsg:   []string{"default_username", models.TemplateKindCloneNoCustomize},
		},
		{
			name: "windows clone_no_customize with static credentials is accepted",
			tmpl: models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "Administrator",
				DefaultPassword: "pw",
			},
		},
		{
			name: "linux clone_no_customize with static credentials is accepted",
			tmpl: models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "student",
				DefaultPassword: "pw",
			},
		},
		{
			name: "windows clone_with_customize with empty default_username is accepted",
			tmpl: models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
		},
		{
			name: "manual windows template matching production shape stays accepted",
			tmpl: models.Template{
				SourceType:      models.TemplateSourceManual,
				OSType:          "windows",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, blocked := credentialContractPublishError(&tc.tmpl)

			if blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v (msg=%q)", blocked, tc.wantBlocked, msg)
			}
			if !tc.wantBlocked {
				if msg != "" {
					t.Errorf("msg = %q, want empty when not blocked", msg)
				}
				return
			}
			// A 409 body an instructor cannot act on is nearly as bad as no
			// gate at all, so assert the message actually names the field,
			// names the mode-specific reason, and carries a fix.
			for _, want := range tc.wantInMsg {
				if !strings.Contains(msg, want) {
					t.Errorf("msg = %q; want it to mention %q", msg, want)
				}
			}
			if !strings.Contains(msg, "fix:") {
				t.Errorf("msg = %q, want it to include a remediation hint", msg)
			}
		})
	}
}

// TestUnattendCredentials_RecoversTheGeneratedBuildAccount guards the fallback
// that makes generalize usable for ISO-built templates.
//
// An unattended install generates its own guest credentials and records them in
// unattend_config; they are never copied to default_password (that column is
// the per-clone student credential). Before this fallback existed, POST
// /generalize returned 400 "guest_username and guest_password are required" for
// a template the platform had built itself, and the only way to proceed was to
// read the password back out of the database by hand.
func TestUnattendCredentials_RecoversTheGeneratedBuildAccount(t *testing.T) {
	tmpl := &models.Template{
		UnattendConfig: []byte(`{"hostname":"tpl","username":"student","password":"s3cr3t","time_zone":"America/New_York"}`),
	}
	u, p := unattendCredentials(tmpl)
	if u != "student" || p != "s3cr3t" {
		t.Fatalf("unattendCredentials = (%q, %q), want (student, s3cr3t); "+
			"generalize will reject an ISO template whose credentials only the platform knows", u, p)
	}
}

// ---- Tests for V3 credential inheritance and resolved-credentials (added 2026-08) ----

// Test 1 & 2: inheritSourceTemplateCredentials (pure function — no DB needed).

// TestInheritSourceTemplateCredentials_InheritsWhenOmitted verifies that a
// clone_template draft with no credentials copies them from the source row.
func TestInheritSourceTemplateCredentials_InheritsWhenOmitted(t *testing.T) {
	req := &CreateTemplateDraftRequest{} // no username or password
	src := &models.Template{DefaultUsername: "student", DefaultPassword: "s0urceP@ss"}
	inheritSourceTemplateCredentials(req, src)
	if req.DefaultUsername != "student" {
		t.Errorf("DefaultUsername = %q; want %q", req.DefaultUsername, "student")
	}
	if req.DefaultPassword != "s0urceP@ss" {
		t.Errorf("DefaultPassword = %q; want inherited value", req.DefaultPassword)
	}
}

// TestInheritSourceTemplateCredentials_ExplicitWins verifies that explicitly
// supplied credentials are never overwritten by the source template's values.
func TestInheritSourceTemplateCredentials_ExplicitWins(t *testing.T) {
	req := &CreateTemplateDraftRequest{DefaultUsername: "custom", DefaultPassword: "custom-pw"}
	src := &models.Template{DefaultUsername: "student", DefaultPassword: "source-pw"}
	inheritSourceTemplateCredentials(req, src)
	if req.DefaultUsername != "custom" {
		t.Errorf("DefaultUsername = %q; explicit value was overwritten by source template", req.DefaultUsername)
	}
	if req.DefaultPassword != "custom-pw" {
		t.Errorf("DefaultPassword = %q; explicit value was overwritten by source template", req.DefaultPassword)
	}
}

// Test 3: clone_template with empty username after inheritance is rejected 400.
//
// With db == nil the handler panics if it reaches any DB call, so the guard
// MUST fire — and return a 400 with a clear message — before hitting the DB.
// The test exercises the path where source_ref is not a parseable UUID so
// the inheritance lookup is skipped; the guard must still fire.
func TestCloneTemplateDraftRequiresUsername(t *testing.T) {
	h := &Handler{logger: noopLogger(t)} // db == nil: any DB call panics

	body := `{"name":"Ubuntu Copy","os_type":"linux","source_type":"clone_template","source_ref":"not-a-uuid"}`
	req := httptest.NewRequest("POST", "/admin/templates/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.AdminCreateTemplateDraft(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for clone_template with empty default_username", rec.Code)
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "default_username") {
		t.Errorf("body = %q; want it to name default_username", bodyStr)
	}
}

// Test 4: the new clone_template guard must NOT fire for iso or clone_vcenter.
//
// iso templates get credentials from unattend_config later in the flow;
// clone_vcenter authors set theirs during the configuring phase. Rejecting
// them here would break both paths. With db == nil the handler panics once
// it reaches a real DB call — a panic means the guard correctly did not fire,
// which is what we want.
func TestCloneTemplateDraftGuardOnlyAppliesToCloneTemplate(t *testing.T) {
	for _, tc := range []struct {
		sourceType string
		sourceRef  string
	}{
		{models.TemplateSourceISO, "[NAS] ISOs/kali.iso"},
		{models.TemplateSourceCloneVCenter, "vm-1234"},
		{models.TemplateSourceOVF, "vm-123"},
	} {
		t.Run(tc.sourceType, func(t *testing.T) {
			h := &Handler{logger: noopLogger(t)} // nil DB

			body := fmt.Sprintf(
				`{"name":"Test","os_type":"linux","source_type":%q,"source_ref":%q}`,
				tc.sourceType, tc.sourceRef,
			)
			r := httptest.NewRequest("POST", "/admin/templates/draft", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			func() {
				defer func() { recover() }() // absorb nil-DB panic
				h.AdminCreateTemplateDraft(rec, r)
			}()

			// The guard must NOT have emitted our clone_template rejection.
			if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "clone_template") {
				t.Errorf("source_type %q incorrectly triggered the clone_template username guard", tc.sourceType)
			}
		})
	}
}

// Test 5: the resolved-credentials handler response MUST NOT contain the raw
// password value, even when the template row holds one.
//
// The response is built through respondJSON (the same path the handler uses)
// into an httptest.ResponseRecorder so we assert the actual wire bytes, not
// just the struct layout.
//
// Positive control: a leakyResponse struct that does include a "password"
// field is marshalled the same way; the test verifies that bytes.Contains
// would catch the sentinel — proving the detection cannot silently pass when
// the leak is real.
func TestResolvedCredentials_HandlerResponseDoesNotLeakPassword(t *testing.T) {
	const sentinel = "SUPERSECRET-DO-NOT-LEAK"

	// Resolve credentials from a template that carries the sentinel password.
	tmpl := &models.Template{
		DefaultUsername: "student",
		DefaultPassword: sentinel,
	}
	resolvedUser, resolvedPass, source := resolveGuestCredentials(tmpl, "", "")
	if resolvedUser == "" || resolvedPass == "" {
		t.Fatalf("resolveGuestCredentials did not resolve; user=%q src=%q", resolvedUser, source)
	}

	// --- real response path ---
	rec := httptest.NewRecorder()
	respondJSON(rec, http.StatusOK, ResolvedCredentials{
		Username:    resolvedUser,
		HasPassword: resolvedPass != "",
		Source:      source,
	})
	body := rec.Body.Bytes()

	// (a) The raw password must not appear in the response bytes.
	if bytes.Contains(body, []byte(sentinel)) {
		t.Errorf("handler response leaks the raw password:\n%s", body)
	}
	// (b) No value-bearing "password" key (excluding "has_password").
	// The JSON encoder writes "has_password" not "password" as a standalone key;
	// assert neither `"password":"` nor `,"password":` appears.
	if bytes.Contains(body, []byte(`"password":"`)) {
		t.Errorf(`handler response contains value-bearing "password" key:\n%s`, body)
	}
	// (c) has_password must be true so the UI knows a password is available.
	if !bytes.Contains(body, []byte(`"has_password":true`)) {
		t.Errorf("has_password is not true in response: %s", body)
	}

	// --- positive control: prove detection catches an actual leak ---
	// Marshal a response that DOES include the raw password; the assertions
	// above must trigger for that shape. This confirms that if someone
	// inadvertently adds a Password field to ResolvedCredentials the tests
	// will fail.
	type leakyResponse struct {
		Username    string `json:"username"`
		HasPassword bool   `json:"has_password"`
		Source      string `json:"source"`
		Password    string `json:"password"` // this field must NOT exist on ResolvedCredentials
	}
	leakyRec := httptest.NewRecorder()
	respondJSON(leakyRec, http.StatusOK, leakyResponse{
		Username:    resolvedUser,
		HasPassword: true,
		Source:      source,
		Password:    sentinel,
	})
	leakyBody := leakyRec.Body.Bytes()

	if !bytes.Contains(leakyBody, []byte(sentinel)) {
		t.Fatal("positive control: sentinel not found in leakyResponse — bytes.Contains is broken or sentinel changed")
	}
	if !bytes.Contains(leakyBody, []byte(`"password":"`)) {
		t.Fatal(`positive control: value-bearing "password" key not found in leakyResponse — detection pattern is wrong`)
	}
}

// Test 6: resolveGuestCredentials returns the correct source for each rung.
func TestResolveGuestCredentials_SourcePerRung(t *testing.T) {
	tests := []struct {
		name       string
		tmpl       *models.Template
		reqUser    string
		reqPass    string
		wantUser   string
		wantSource string
	}{
		{
			name:       "request overrides all",
			tmpl:       &models.Template{DefaultUsername: "student", DefaultPassword: "tmplpass"},
			reqUser:    "admin",
			reqPass:    "adminpass",
			wantUser:   "admin",
			wantSource: "request",
		},
		{
			name:       "template row",
			tmpl:       &models.Template{DefaultUsername: "student", DefaultPassword: "tmplpass"},
			wantUser:   "student",
			wantSource: "template",
		},
		{
			name: "unattend_config when template row is empty",
			tmpl: &models.Template{
				UnattendConfig: []byte(`{"username":"iso-user","password":"isopass","time_zone":"UTC"}`),
			},
			wantUser:   "iso-user",
			wantSource: "unattend_config",
		},
		{
			name:       "none when nothing resolves",
			tmpl:       &models.Template{},
			wantUser:   "",
			wantSource: "none",
		},
		{
			name: "partial template row falls through to unattend_config",
			tmpl: &models.Template{
				DefaultUsername: "student", // password missing
				UnattendConfig:  []byte(`{"username":"iso-user","password":"isopass","time_zone":"UTC"}`),
			},
			wantUser:   "student",
			wantSource: "unattend_config", // unattend was needed to complete the pair
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUser, _, gotSource := resolveGuestCredentials(tt.tmpl, tt.reqUser, tt.reqPass)
			if gotUser != tt.wantUser {
				t.Errorf("username = %q; want %q", gotUser, tt.wantUser)
			}
			if gotSource != tt.wantSource {
				t.Errorf("source = %q; want %q", gotSource, tt.wantSource)
			}
		})
	}
}

// --- Guest OS ID validation on the ISO draft path (regression: the wizard
// shipped no way to set guest_id, so ISO templates were created with
// guest_id="" and every provision failed with vCenter's opaque
// "guest ID required" fault). These 400s are returned before any DB access,
// so a Handler{} with nil deps is sufficient. ---

func postDraft(t *testing.T, body CreateTemplateDraftRequest) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/templates/draft", bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	h.AdminCreateTemplateDraft(rec, req)
	return rec
}

func TestAdminCreateTemplateDraft_ISORequiresGuestID(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:       "Mint 22",
		OSType:     models.OSTypeLinux,
		SourceType: models.TemplateSourceISO,
		SourceRef:  "[NAS-BackupsAndISOS] ISOs/mint.iso",
		// GuestID deliberately omitted.
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "guest_id is required") {
		t.Errorf("body = %q; want a 'guest_id is required' message", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "guest-os-catalog") {
		t.Errorf("body = %q; want a pointer to the guest-os-catalog endpoint", rec.Body.String())
	}
}

func TestAdminCreateTemplateDraft_ISORejectsJunkGuestID(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:       "Mint 22",
		OSType:     models.OSTypeLinux,
		SourceType: models.TemplateSourceISO,
		SourceRef:  "[NAS-BackupsAndISOS] ISOs/mint.iso",
		GuestID:    "not a real guest",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not a valid vSphere guest OS identifier") {
		t.Errorf("body = %q; want an invalid-guest-id message", rec.Body.String())
	}
}

func TestAdminListGuestOSCatalog(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/templates/guest-os-catalog", nil)
	rec := httptest.NewRecorder()
	h.AdminListGuestOSCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	var body struct {
		Options []models.GuestOSOption `json:"options"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Options) == 0 {
		t.Fatal("options is empty; want the full guest OS catalog")
	}
	// Spot-check that the Mint/Ubuntu entry a student needs is present.
	var sawUbuntu bool
	for _, o := range body.Options {
		if o.GuestID == "ubuntu64Guest" {
			sawUbuntu = true
		}
	}
	if !sawUbuntu {
		t.Error("catalog response missing ubuntu64Guest")
	}
}

// TestAdminCreateTemplateDraft_RejectsSkipGeneralizeOnISO is the load-bearing
// guard for the unsafe combo. Mutation tested: removing the
// ValidateSkipGeneralize call lets this request fall through to auth (401)
// instead of 400 naming skip_generalize + iso.
func TestAdminCreateTemplateDraft_RejectsSkipGeneralizeOnISO(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:           "Fresh ISO",
		OSType:         models.OSTypeLinux,
		SourceType:     models.TemplateSourceISO,
		SourceRef:      "[NAS] ISOs/ubuntu.iso",
		GuestID:        "ubuntu64Guest",
		SkipGeneralize: true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (skip_generalize on iso is unsafe)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "skip_generalize") {
		t.Errorf("body = %q; want it to name skip_generalize", body)
	}
	if !strings.Contains(body, "iso") {
		t.Errorf("body = %q; want it to name source_type=iso", body)
	}
}

func TestAdminCreateTemplateDraft_RejectsSkipGeneralizeOnCloneTemplate(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:           "Copy",
		OSType:         models.OSTypeLinux,
		SourceType:     models.TemplateSourceCloneTemplate,
		SourceRef:      uuid.NewString(),
		DefaultUsername: "student",
		SkipGeneralize: true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "skip_generalize") {
		t.Errorf("body = %q; want skip_generalize", rec.Body.String())
	}
}

func TestAdminCreateTemplateDraft_AcceptsOVF(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:       "Imported OVA",
		OSType:     models.OSTypeLinux,
		SourceType: models.TemplateSourceOVF,
		SourceRef:  "vm-123",
	})
	// Nil handler has no auth context; ovf must pass source-type validation
	// and reach the auth required 401 (not 400 "unknown source_type").
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("ovf was rejected as invalid source_type: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 after ovf validation (no auth on Handler{})", rec.Code)
	}
}

func TestAdminCreateTemplateDraft_AllowsSkipGeneralizeOnOVF(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:           "Imported OVA",
		OSType:         models.OSTypeLinux,
		SourceType:     models.TemplateSourceOVF,
		SourceRef:      "vm-123",
		SkipGeneralize: true,
	})
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "skip_generalize") {
		t.Fatalf("skip_generalize on ovf was rejected: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 after accepting ovf+skip_generalize", rec.Code)
	}
}

// TestAdminCreateTemplateDraft_OVFAcceptsCloneNoCustomize proves the wizard
// can now author the only kind that works for an appliance OVA. Before this,
// every draft was hard-coded clone_with_customize, so a published OVA
// template produced pods that waited for VMware Tools to accept a generated
// `student` credential no appliance ever creates, then failed into
// compensation.
func TestAdminCreateTemplateDraft_OVFAcceptsCloneNoCustomize(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:            "VyOS Appliance",
		OSType:          models.OSTypeLinux,
		SourceType:      models.TemplateSourceOVF,
		SourceRef:       "vm-123",
		SkipGeneralize:  true,
		Kind:            models.TemplateKindCloneNoCustomize,
		DefaultUsername: "vyos",
		DefaultPassword: "vyos",
	})
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("ovf + clone_no_customize was rejected: %s", rec.Body.String())
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 after passing validation (no auth on Handler{})", rec.Code)
	}
}

// TestAdminCreateTemplateDraft_CloneNoCustomizeRequiresCredentials covers the
// hole this kind opens: it bypasses the generated-credential acceptance gate,
// so static credentials are the only way into a pod cloned from it and the
// gate that would normally catch empty ones is the very thing being skipped.
func TestAdminCreateTemplateDraft_CloneNoCustomizeRequiresCredentials(t *testing.T) {
	for _, tc := range []struct {
		name     string
		username string
		password string
	}{
		{"both empty", "", ""},
		{"password missing", "vyos", ""},
		{"username missing", "", "vyos"},
		{"whitespace only", "  ", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postDraft(t, CreateTemplateDraftRequest{
				Name:            "VyOS Appliance",
				OSType:          models.OSTypeLinux,
				SourceType:      models.TemplateSourceOVF,
				SourceRef:       "vm-123",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: tc.username,
				DefaultPassword: tc.password,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (clone_no_customize with no usable credentials)", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "default_username") || !strings.Contains(body, "default_password") {
				t.Errorf("body = %q; want it to name both credential fields", body)
			}
		})
	}
}

// TestAdminCreateTemplateDraft_RejectsCloneNoCustomizeOnBuiltSources keeps the
// new kind scoped to already-prepared sources. A fresh ISO install or a
// re-customized clone has no prepared credentials to fall back on.
func TestAdminCreateTemplateDraft_RejectsCloneNoCustomizeOnBuiltSources(t *testing.T) {
	for _, sourceType := range []string{models.TemplateSourceISO, models.TemplateSourceCloneTemplate} {
		t.Run(sourceType, func(t *testing.T) {
			rec := postDraft(t, CreateTemplateDraftRequest{
				Name:            "Nope",
				OSType:          models.OSTypeLinux,
				SourceType:      sourceType,
				SourceRef:       "[NAS] ISOs/ubuntu.iso",
				GuestID:         "ubuntu64Guest",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "student",
				DefaultPassword: "pw",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 for clone_no_customize on %s", rec.Code, sourceType)
			}
			if !strings.Contains(rec.Body.String(), models.TemplateKindCloneNoCustomize) {
				t.Errorf("body = %q; want it to name the rejected kind", rec.Body.String())
			}
		})
	}
}

// TestAdminCreateTemplateDraft_OmittedKindStaysCustomize pins the default so
// adding the kind field cannot change behavior for any existing caller — the
// UI does not send one.
func TestAdminCreateTemplateDraft_OmittedKindStaysCustomize(t *testing.T) {
	resolved, err := models.ResolveWizardTemplateKind(models.TemplateSourceOVF, "")
	if err != nil {
		t.Fatalf("omitted kind must be accepted: %v", err)
	}
	if resolved != models.TemplateKindCloneWithCustomize {
		t.Fatalf("default kind = %q; want %q", resolved, models.TemplateKindCloneWithCustomize)
	}
}

func TestAdminCreateTemplateDraft_RejectsUnknownSourceType(t *testing.T) {
	rec := postDraft(t, CreateTemplateDraftRequest{
		Name:       "X",
		OSType:     models.OSTypeLinux,
		SourceType: "manual",
		SourceRef:  "vm-1",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 for source_type=manual on the wizard", rec.Code)
	}
}
