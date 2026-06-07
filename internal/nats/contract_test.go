// Package events_test contains contract tests for the NATS event payloads
// crossed between the API gateway, the workflow engine, the runner pods, and
// any subscribers (UI WebSocket bridge, synthetic monitor, etc.). The goal of
// this file is regression-protection: if you change the shape of any wire
// payload here, the test fails and you must either update the test (and any
// downstream consumers) intentionally, or revert the change.
//
// Adding a new event type:
//  1. Add a contract entry below describing the SUBJECT, the producer, the
//     consumer(s), and the canonical-JSON example.
//  2. Run `go test ./internal/nats/...` to lock the schema in.
//
// Removing or renaming a field is a BREAKING CHANGE — bump the engine and the
// UI in lockstep and don't merge a half-deployed change.
package events_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	events "github.com/jmal1/selfservice-api/internal/nats"
)

// canonicalJSON returns the bytes of the event re-encoded with sorted keys, so
// schema comparisons are order-independent.
func canonicalJSON(t *testing.T, v interface{}) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var into map[string]interface{}
	if err := json.Unmarshal(raw, &into); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(into))
	for k := range into {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += ","
		}
		val, _ := json.Marshal(into[k])
		out += `"` + k + `":` + string(val)
	}
	return out + "}"
}

// fieldNames extracts JSON field names from a struct via reflection, treating
// `json:"-"` and unexported fields as absent.
func fieldNames(t *testing.T, v interface{}) []string {
	t.Helper()
	typ := reflect.TypeOf(v)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("fieldNames: not a struct: %v", typ.Kind())
	}
	names := []string{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := tag
		if idx := indexOf(tag, ','); idx >= 0 {
			name = tag[:idx]
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func indexOf(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// TestContract_EventStruct_ApprovedFields freezes the wire shape of the
// generic events.Event struct used by *every* publisher in the codebase.
// Adding or renaming a field here means coordinating a deploy across the API
// gateway, engine, worker, runner, synthetic monitor, AND the UI WebSocket
// consumer. Treat any test failure as a deployment-coupling alert.
func TestContract_EventStruct_ApprovedFields(t *testing.T) {
	approved := []string{
		"job_id",
		"message",
		"pod_id",
		"status",
		"step",
		"total_steps",
		"type",
		"vm_id",
	}
	got := fieldNames(t, events.Event{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf(`events.Event wire shape changed.
got:      %v
approved: %v

If this is intentional, update the approved list AND coordinate a deploy:
  * API gateway (every PublishJobX caller)
  * Engine (publishRunEvent in internal/engine/engine.go)
  * UI WebSocket consumer (selfservice-ui/src/lib/api/runProgress.ts)
  * Synthetic monitor (cmd/synthetic-api-monitor/)
`, got, approved)
	}
}

// TestContract_SubjectConstants freezes the NATS subject taxonomy. Renaming
// any subject here is a coordinated rename across publishers + subscribers in
// at least two services; the test exists so nobody changes a string literal
// "and figures out the consumer later."
func TestContract_SubjectConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"job created", events.SubjectJobCreated, "jobs.created"},
		{"job status template", events.SubjectJobStatus, "jobs.%s.status"},
		{"job log template", events.SubjectJobLog, "jobs.%s.log"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("subject %s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestContract_RunProgressEvent_JSONShape locks the exact JSON serialization
// of a run progress event (the kind the engine emits and the W4a WebSocket
// handler forwards to the UI). This is the contract the SvelteKit live-events
// card depends on. If you need to add a field, append it AFTER all existing
// fields, give it the `omitempty` tag, and update this test.
func TestContract_RunProgressEvent_JSONShape(t *testing.T) {
	evt := events.Event{
		Type:       "run.action_complete",
		JobID:      "00000000-0000-0000-0000-000000000001",
		PodID:      "00000000-0000-0000-0000-00000000abcd",
		Status:     "action_complete",
		Message:    "checks/port-22-open: pass",
		Step:       3,
		TotalSteps: 7,
	}
	got := canonicalJSON(t, evt)
	want := `{"job_id":"00000000-0000-0000-0000-000000000001","message":"checks/port-22-open: pass","pod_id":"00000000-0000-0000-0000-00000000abcd","status":"action_complete","step":3,"total_steps":7,"type":"run.action_complete"}`
	if got != want {
		t.Errorf("run progress event JSON shape changed.\n got:  %s\n want: %s", got, want)
	}
}

// TestContract_RunProgressEvent_OmitsZeroOptionals — VMID, Step, TotalSteps
// must NOT appear when zero. The UI relies on `step in event` to decide
// whether to render progress; if we emit `"step":0` always, the UI mistakes
// the engine startup event for action 0/N.
func TestContract_RunProgressEvent_OmitsZeroOptionals(t *testing.T) {
	evt := events.Event{
		Type:    "run.provisioning",
		JobID:   "run-1",
		PodID:   "pod-1",
		Status:  "provisioning",
		Message: "Run claimed, provisioning runner",
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"vm_id"`, `"step"`, `"total_steps"`} {
		if contains(string(raw), banned) {
			t.Errorf("zero-value field %s leaked into wire payload: %s", banned, raw)
		}
	}
}

// TestContract_KnownRunStatuses freezes the set of `status` values the engine
// is allowed to put on a run.* event. The UI uses these to render colored
// badges; the synthetic monitor uses them to decide whether a run is done.
// Adding a new status means updating BOTH consumers.
func TestContract_KnownRunStatuses(t *testing.T) {
	knownStatuses := map[string]string{
		// Lifecycle (non-terminal)
		"provisioning":      "Run claimed by the engine, runner being provisioned",
		"running":           "Runner started; workflows executing",
		"action_complete":   "A single action inside a workflow finished",
		"workflow_complete": "All actions in a workflow finished",
		// Terminal
		"completed": "Run finished successfully (no workflow failures)",
		"failed":    "Run finished with at least one workflow failure or engine error",
		"cancelled": "Run was cancelled (admin action or context cancellation)",
		"timeout":   "Run exceeded the configured wall-clock budget",
		// Special
		"workflow_start": "About to dispatch a workflow (currently vmware_tools only)",
	}

	terminal := map[string]bool{
		"completed": true,
		"failed":    true,
		"cancelled": true,
		"timeout":   true,
	}

	for status, desc := range knownStatuses {
		if status == "" {
			t.Errorf("empty status with description %q is not allowed", desc)
		}
	}

	// Terminal set is part of the W4a websocket close-on-terminal contract.
	for s := range terminal {
		if _, ok := knownStatuses[s]; !ok {
			t.Errorf("terminal status %q not in known set", s)
		}
	}
	if len(terminal) != 4 {
		t.Errorf("terminal status set size = %d; want 4 (completed/failed/cancelled/timeout). Update internal/api/handlers/run_progress_ws.go and the UI if this is a deliberate change.", len(terminal))
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
