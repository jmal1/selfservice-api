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
