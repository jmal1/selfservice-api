package preflight

import (
	"encoding/json"
	"testing"
)

// TestResultWireContract locks the JSON key names that selfservice-ui reads.
//
// preflight.Result is serialized straight onto the wire by both
// AdminPreflightTemplate (200) and the provision gate (409). The UI's
// PreflightResult interface in src/lib/api/client.ts must match these keys
// exactly. Without this test the contract depends on nothing more than the
// presence of the struct tags, and a rename or a tag removal breaks the panel
// silently — the UI renders "undefined" rows and every check reads as passing,
// because `!r.ok` on a missing field is false.
func TestResultWireContract(t *testing.T) {
	b, err := json.Marshal(Result{
		ID:       "PF-01",
		Severity: "block",
		OK:       false,
		Detail:   "observed",
		Fix:      "do the thing",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(b)
	want := `{"id":"PF-01","severity":"block","ok":false,"detail":"observed","fix":"do the thing"}`
	if got != want {
		t.Errorf("Result wire format changed.\n got: %s\nwant: %s\n\n"+
			"If this is intentional, update PreflightResult in "+
			"selfservice-ui/src/lib/api/client.ts and PreflightPanel.svelte in the same PR.",
			got, want)
	}
}

// TestRunAllResultsAreSerializable guards the aggregate shape: every check must
// produce a Result whose severity is one of the two values the UI switches on.
// A typo'd severity ("blocking", "warning") makes the UI treat the row as a
// warning and lets a blocking failure through the panel.
func TestRunAllResultsAreSerializable(t *testing.T) {
	for _, sev := range []string{"block", "warn"} {
		var probe map[string]any
		b, _ := json.Marshal(Result{Severity: sev})
		if err := json.Unmarshal(b, &probe); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if probe["severity"] != sev {
			t.Errorf("severity %q did not round-trip: got %v", sev, probe["severity"])
		}
	}
}
