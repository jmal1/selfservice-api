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

// TestLinuxContractPublishError_BlocksLiveUbuntuDefect guards the publish
// gate. The shapes below are not hypothetical: the live "Ubuntu 24.04 Server"
// template is active in production with an empty default_username, which is
// exactly case 1. Without this gate that template boots green through the
// smoke test and then rejects every password the UI shows a student.
func TestLinuxContractPublishError_BlocksLiveUbuntuDefect(t *testing.T) {
	tests := []struct {
		name        string
		tmpl        models.Template
		wantBlocked bool
		wantInMsg   string
	}{
		{
			name:        "linux with empty default_username is blocked",
			tmpl:        models.Template{OSType: "linux", DefaultUsername: "", DefaultPassword: "pw"},
			wantBlocked: true,
			wantInMsg:   "default_username",
		},
		{
			name:        "linux with non-student default_username is blocked",
			tmpl:        models.Template{OSType: "linux", DefaultUsername: "ubuntu", DefaultPassword: "pw"},
			wantBlocked: true,
			wantInMsg:   "student",
		},
		{
			name:        "compliant linux template publishes",
			tmpl:        models.Template{OSType: "linux", DefaultUsername: "student", DefaultPassword: "pw"},
			wantBlocked: false,
		},
		{
			name:        "windows is never subject to the linux contract",
			tmpl:        models.Template{OSType: "windows", DefaultUsername: ""},
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, blocked := linuxContractPublishError(&tc.tmpl)

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
			// gate at all, so assert the message actually names the field
			// and carries a fix rather than merely being non-empty.
			if !strings.Contains(msg, tc.wantInMsg) {
				t.Errorf("msg = %q, want it to mention %q", msg, tc.wantInMsg)
			}
			if !strings.Contains(msg, "fix:") {
				t.Errorf("msg = %q, want it to include a remediation hint", msg)
			}
		})
	}
}
