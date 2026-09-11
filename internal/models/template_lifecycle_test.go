package models

import (
	"encoding/json"
	"testing"
)

// TestAllTemplateStatesContainsEveryStateConstant guards against the AllTemplateStates
// slice falling out of sync with the TemplateState* constants. New states must be added
// to BOTH the const block and the slice; otherwise the worker/lifecycle layer may admit
// transitions the database CHECK constraint then rejects.
func TestAllTemplateStatesContainsEveryStateConstant(t *testing.T) {
	want := map[string]bool{
		TemplateStateDraft:        true,
		TemplateStateProvisioning: true,
		TemplateStateConfiguring:  true,
		TemplateStateGeneralizing: true,
		TemplateStateReady:        true,
		TemplateStateVerifying:    true,
		TemplateStateActive:       true,
		TemplateStateError:        true,
	}
	if len(AllTemplateStates) != len(want) {
		t.Fatalf("AllTemplateStates len=%d, want %d (constant block drifted from slice)", len(AllTemplateStates), len(want))
	}
	seen := make(map[string]bool, len(AllTemplateStates))
	for _, s := range AllTemplateStates {
		if seen[s] {
			t.Errorf("AllTemplateStates has duplicate %q", s)
		}
		seen[s] = true
		if !want[s] {
			t.Errorf("AllTemplateStates contains unknown state %q", s)
		}
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("AllTemplateStates missing state %q", k)
		}
	}
}

// TestTemplateStateConstantsAreStable ensures the string values of the lifecycle states
// match the CHECK constraint in migration 000018. Changing any of these requires a
// follow-up migration that updates the constraint AND backfills existing rows.
func TestTemplateStateConstantsAreStable(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{TemplateStateDraft, "draft"},
		{TemplateStateProvisioning, "provisioning"},
		{TemplateStateConfiguring, "configuring"},
		{TemplateStateGeneralizing, "generalizing"},
		{TemplateStateReady, "ready"},
		{TemplateStateVerifying, "verifying"},
		{TemplateStateActive, "active"},
		{TemplateStateError, "error"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("template state constant drifted: got %q want %q (migration 000018 CHECK will reject)", c.got, c.want)
		}
	}
}

// TestTemplateSourceConstantsAreStable ensures source_type values match the CHECK in
// migration 000018. The worker reads these to branch on provisioning strategy.
func TestTemplateSourceConstantsAreStable(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{TemplateSourceManual, "manual"},
		{TemplateSourceCloneTemplate, "clone_template"},
		{TemplateSourceCloneVCenter, "clone_vcenter"},
		{TemplateSourceISO, "iso"},
		{TemplateSourceOVF, "ovf"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("template source constant drifted: got %q want %q (migration 000018 CHECK will reject)", c.got, c.want)
		}
	}
}

// TestTemplateJSONWireShapeIncludesLifecycleFields locks in the JSON contract so the
// SvelteKit UI keeps working when we add more fields. CreatedBy is *uuid.UUID; when nil
// it should be omitted from the wire payload (omitempty).
func TestTemplateJSONWireShapeIncludesLifecycleFields(t *testing.T) {
	tmpl := Template{
		Name:           "wire-shape",
		TemplateState:  TemplateStateDraft,
		VCenterVMID:    "vm-1234",
		SourceType:     TemplateSourceCloneTemplate,
		SourceRef:      "00000000-0000-0000-0000-000000000001",
		StagingNetwork: "LabVMs-VLAN30",
		SkipGeneralize: true,
	}
	b, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	got := string(b)
	for _, want := range []string{
		`"template_state":"draft"`,
		`"vcenter_vm_id":"vm-1234"`,
		`"source_type":"clone_template"`,
		`"source_ref":"00000000-0000-0000-0000-000000000001"`,
		`"staging_network":"LabVMs-VLAN30"`,
		`"skip_generalize":true`,
	} {
		if !contains(got, want) {
			t.Errorf("template JSON missing %s in: %s", want, got)
		}
	}
	// CreatedBy is nil-valued with omitempty; should NOT appear when unset.
	if contains(got, `"created_by"`) {
		t.Errorf("nil CreatedBy should be omitted, got: %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestValidateSkipGeneralize(t *testing.T) {
	if err := ValidateSkipGeneralize(TemplateSourceISO, true); err == nil {
		t.Fatal("skip_generalize=true on iso must be rejected")
	}
	if err := ValidateSkipGeneralize(TemplateSourceCloneTemplate, true); err == nil {
		t.Fatal("skip_generalize=true on clone_template must be rejected")
	}
	if err := ValidateSkipGeneralize(TemplateSourceManual, true); err == nil {
		t.Fatal("skip_generalize=true on manual must be rejected")
	}
	if err := ValidateSkipGeneralize(TemplateSourceOVF, true); err != nil {
		t.Fatalf("skip_generalize=true on ovf should be allowed: %v", err)
	}
}

// TestResolveWizardTemplateKind covers the kind the wizard assigns to a
// draft. clone_no_customize is the only kind that works for an appliance OVA
// with no cloud-init, and it must stay scoped to already-prepared sources —
// the same set ValidateSkipGeneralize allows.
func TestResolveWizardTemplateKind(t *testing.T) {
	cases := []struct {
		sourceType string
		kind       string
		want       string
		wantErr    bool
	}{
		// Omitted kind keeps the historical behavior for every source type.
		{TemplateSourceOVF, "", TemplateKindCloneWithCustomize, false},
		{TemplateSourceISO, "", TemplateKindCloneWithCustomize, false},
		{TemplateSourceCloneTemplate, "", TemplateKindCloneWithCustomize, false},
		{TemplateSourceCloneVCenter, "", TemplateKindCloneWithCustomize, false},

		// Explicit clone_with_customize is always fine.
		{TemplateSourceISO, TemplateKindCloneWithCustomize, TemplateKindCloneWithCustomize, false},

		// clone_no_customize: allowed only for already-prepared sources.
		{TemplateSourceOVF, TemplateKindCloneNoCustomize, TemplateKindCloneNoCustomize, false},
		{TemplateSourceCloneVCenter, TemplateKindCloneNoCustomize, TemplateKindCloneNoCustomize, false},
		{TemplateSourceISO, TemplateKindCloneNoCustomize, "", true},
		{TemplateSourceCloneTemplate, TemplateKindCloneNoCustomize, "", true},

		// Not a wizard flow, and not a kind at all.
		{TemplateSourceOVF, TemplateKindRegisteredExistingVM, "", true},
		{TemplateSourceOVF, "clone", "", true},
		{TemplateSourceOVF, "CLONE_NO_CUSTOMIZE", "", true},
	}
	for _, c := range cases {
		got, err := ResolveWizardTemplateKind(c.sourceType, c.kind)
		if c.wantErr {
			if err == nil {
				t.Errorf("ResolveWizardTemplateKind(%q, %q) = %q, nil; want an error",
					c.sourceType, c.kind, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveWizardTemplateKind(%q, %q) unexpected error: %v",
				c.sourceType, c.kind, err)
			continue
		}
		if got != c.want {
			t.Errorf("ResolveWizardTemplateKind(%q, %q) = %q; want %q",
				c.sourceType, c.kind, got, c.want)
		}
	}

	// The allowed set must track ValidateSkipGeneralize rather than drifting
	// from it: both answer "is this source already prepared?".
	for _, st := range []string{
		TemplateSourceOVF, TemplateSourceCloneVCenter,
		TemplateSourceISO, TemplateSourceCloneTemplate,
	} {
		_, kindErr := ResolveWizardTemplateKind(st, TemplateKindCloneNoCustomize)
		skipErr := ValidateSkipGeneralize(st, true)
		if (kindErr == nil) != (skipErr == nil) {
			t.Errorf("source_type %q: clone_no_customize allowed=%v but skip_generalize allowed=%v; "+
				"these must describe the same already-prepared source set",
				st, kindErr == nil, skipErr == nil)
		}
	}
	if err := ValidateSkipGeneralize(TemplateSourceCloneVCenter, true); err != nil {
		t.Fatalf("skip_generalize=true on clone_vcenter should be allowed: %v", err)
	}
	if err := ValidateSkipGeneralize(TemplateSourceISO, false); err != nil {
		t.Fatalf("skip_generalize=false is always allowed: %v", err)
	}
}

func TestValidWizardSourceType(t *testing.T) {
	want := map[string]bool{
		TemplateSourceCloneTemplate: true,
		TemplateSourceCloneVCenter:  true,
		TemplateSourceISO:           true,
		TemplateSourceOVF:           true,
		TemplateSourceManual:        false,
		"bogus":                     false,
		"":                          false,
	}
	for s, ok := range want {
		if got := ValidWizardSourceType(s); got != ok {
			t.Errorf("ValidWizardSourceType(%q) = %v; want %v", s, got, ok)
		}
	}
}
