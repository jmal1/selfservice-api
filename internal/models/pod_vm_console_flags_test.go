package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPodVM_JSONIncludesConsoleHelperFlags proves the student GetPod payload
// can carry assign_ip and skip_generalize on each VM without a nested template
// object — the console helper registry keys off these wire fields.
func TestPodVM_JSONIncludesConsoleHelperFlags(t *testing.T) {
	vm := PodVM{
		ID:             uuid.New(),
		PodID:          uuid.New(),
		TemplateID:     uuid.New(),
		DisplayName:    "legacy-ova",
		Status:         VMStatusRunning,
		TemplateKind:   TemplateKindCloneNoCustomize,
		OSType:         "linux",
		AssignIP:       false,
		SkipGeneralize: true,
	}

	raw, err := json.Marshal(vm)
	if err != nil {
		t.Fatalf("marshal PodVM: %v", err)
	}
	body := string(raw)

	for _, want := range []string{
		`"assign_ip":false`,
		`"skip_generalize":true`,
		`"template_kind":"clone_no_customize"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PodVM JSON missing %s; got %s", want, body)
		}
	}

	var decoded PodVM
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal PodVM: %v", err)
	}
	if decoded.AssignIP {
		t.Errorf("AssignIP = true, want false")
	}
	if !decoded.SkipGeneralize {
		t.Errorf("SkipGeneralize = false, want true")
	}
}
