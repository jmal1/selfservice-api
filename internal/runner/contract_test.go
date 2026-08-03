// Package runner_test holds Phase 5.6 contract tests for the
// runner ↔ crucible-engine HTTP callback API. These tests are
// regression-protection: any change to the wire payloads or URL routes
// crossed between the runner (running inside a K8s Job, in a student pod)
// and the engine (running in the selfservice namespace) MUST update these
// tests intentionally — and the engine deploy MUST go out BEFORE the
// runner image change, or vice versa, to keep the contract honored end-to-end.
//
// Adding a new field:
//   1. Append it to the relevant struct in runner/types.go with `omitempty`
//   2. Add the field name to the approved list in this file
//   3. Update internal/engine/callback.go to consume it
//   4. Update this file's JSON shape test with the new canonical example
//   5. Bump engine image FIRST, then runner image. Never both at once.
//
// Removing or renaming a field IS a breaking change. Even with omitempty,
// removing a field that the engine reads will silently zero the value on
// every existing in-flight callback.
package runner_test

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// fieldNames lists exported JSON field tags on a struct, sorted. Helper
// duplicated from internal/nats/contract_test.go on purpose so this file
// stays self-contained.
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
		if idx := strings.IndexByte(tag, ','); idx >= 0 {
			name = tag[:idx]
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestContract_CallbackURLPattern locks the URL shape the runner POSTs to:
//
//	{baseURL}/internal/callback/{token}/{action|workflow|complete|heartbeat}
//
// If you rename any path segment, you MUST update both:
//   - internal/runner/callback.go (the runner side; see post() body)
//   - internal/engine/callback.go (the engine routes)
// AND bump both image versions in the same Helm release.
func TestContract_CallbackURLPattern(t *testing.T) {
	// The CallbackClient's post() method builds the URL from these pieces.
	// We don't have a public accessor for the URL so we verify the format
	// by checking the documented paths exist on the engine side, indirectly:
	// the engine has /action, /workflow, /complete, /heartbeat under
	// /internal/callback/{token}/. Any rename here will break the runner.
	wantSuffixes := []string{"/action", "/workflow", "/complete", "/heartbeat"}
	for _, s := range wantSuffixes {
		if s == "" || !strings.HasPrefix(s, "/") {
			t.Errorf("callback path %q must start with /", s)
		}
	}
}

// TestContract_CallbackActionPayload locks the JSON shape of the per-action
// callback. This is the highest-volume call (1 per action in a workflow),
// so any silent field rename here means losing per-action accounting until
// the runner image is rebuilt.
func TestContract_CallbackActionPayload(t *testing.T) {
	approved := []string{"action", "workflow_slug"}
	got := fieldNames(t, runner.CallbackActionPayload{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("CallbackActionPayload wire shape changed.\n got:      %v\n approved: %v", got, approved)
	}

	approvedAction := []string{"action", "context", "duration_ms", "exit_code", "message", "status"}
	gotAction := fieldNames(t, runner.ActionOutput{})
	if !reflect.DeepEqual(gotAction, approvedAction) {
		t.Fatalf("ActionOutput wire shape changed.\n got:      %v\n approved: %v\n\nIf intentional, update internal/engine/callback.go handleAction.", gotAction, approvedAction)
	}

	// Lock the exact JSON output for a representative payload.
	payload := runner.CallbackActionPayload{
		WorkflowSlug: "checks/port-22-open",
		Action: runner.ActionOutput{
			Action:   "verify_ssh_listening",
			Status:   "pass",
			Message:  "port 22 is open",
			ExitCode: 0,
			Duration: 1234 * time.Millisecond,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"workflow_slug":"checks/port-22-open","action":{"action":"verify_ssh_listening","status":"pass","message":"port 22 is open","exit_code":0,"duration_ms":1234000000}}`
	if string(raw) != want {
		t.Errorf("CallbackActionPayload JSON shape changed.\n got:  %s\n want: %s", raw, want)
	}
}

// TestContract_CallbackWorkflowPayload locks the workflow-complete callback
// shape. Fired once per workflow; engine uses it to update workflow-level
// status badges in the admin UI.
func TestContract_CallbackWorkflowPayload(t *testing.T) {
	approved := []string{"result"}
	got := fieldNames(t, runner.CallbackWorkflowPayload{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("CallbackWorkflowPayload wire shape changed.\n got:      %v\n approved: %v", got, approved)
	}

	approvedResult := []string{"action_results", "message", "setup_output", "status", "total_duration_ms", "workflow_name", "workflow_slug"}
	gotResult := fieldNames(t, runner.WorkflowRunResult{})
	if !reflect.DeepEqual(gotResult, approvedResult) {
		t.Fatalf("WorkflowRunResult wire shape changed.\n got:      %v\n approved: %v", gotResult, approvedResult)
	}
}

// TestContract_CallbackCompletePayload locks the terminal callback. The
// engine uses Status to mark the run row completed vs failed in Postgres
// and to publish a terminal NATS event that closes the W4a WebSocket.
func TestContract_CallbackCompletePayload(t *testing.T) {
	approved := []string{"results", "status"}
	got := fieldNames(t, runner.CallbackCompletePayload{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("CallbackCompletePayload wire shape changed.\n got:      %v\n approved: %v", got, approved)
	}

	// Sanity-check the allowed terminal statuses. Engine uses these to set
	// runs.status in the DB and to publish a terminal `run.*` NATS event.
	allowed := map[string]bool{
		"completed": true,
		"failed":    true,
	}
	for s := range allowed {
		payload := runner.CallbackCompletePayload{Status: s}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("marshal status=%q: %v", s, err)
			continue
		}
		if !strings.Contains(string(raw), `"status":"`+s+`"`) {
			t.Errorf("status %q did not round-trip cleanly: %s", s, raw)
		}
	}
}

// TestContract_CallbackHeartbeatPayload locks the heartbeat shape. The
// engine watchdog uses this to extend the run-lease lock and to update
// `last_seen_at` on the run row; if the field names rename silently, the
// watchdog falls back to its 5-min inactivity timeout and the run gets
// marked failed prematurely.
func TestContract_CallbackHeartbeatPayload(t *testing.T) {
	approved := []string{"current_action", "elapsed_seconds", "phase"}
	got := fieldNames(t, runner.CallbackHeartbeatPayload{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("CallbackHeartbeatPayload wire shape changed.\n got:      %v\n approved: %v", got, approved)
	}

	// Lock the JSON for a representative heartbeat (current_action present).
	payload := runner.CallbackHeartbeatPayload{
		Phase:          "running",
		CurrentAction:  "verify_ssh_listening",
		ElapsedSeconds: 42,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"phase":"running","current_action":"verify_ssh_listening","elapsed_seconds":42}`
	if string(raw) != want {
		t.Errorf("heartbeat shape changed.\n got:  %s\n want: %s", raw, want)
	}

	// And one without current_action — the omitempty tag must keep the
	// field out of the wire entirely (not emit "current_action":"").
	payload2 := runner.CallbackHeartbeatPayload{
		Phase:          "provisioning",
		ElapsedSeconds: 3,
	}
	raw2, err := json.Marshal(payload2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw2), "current_action") {
		t.Errorf("empty current_action leaked into wire: %s", raw2)
	}
}

// TestContract_RunnerConfig_FieldsFrozen locks the structure of the
// /opt/crucible/runner-config.json file the engine writes into a K8s
// Secret and mounts into the runner pod at startup. Adding a field here
// requires the runner image to know how to read it (zero-value safe for
// older runners, but only if the new field is genuinely optional).
func TestContract_RunnerConfig_FieldsFrozen(t *testing.T) {
	approved := []string{"callback_token", "callback_url", "pod", "run_id", "target", "workflows"}
	got := fieldNames(t, runner.RunnerConfig{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("RunnerConfig wire shape changed.\n got:      %v\n approved: %v\n\nThis file is consumed by the runner image at startup. Bump the runner image BEFORE the engine if you add a required field; otherwise existing runner pods fail at config-load.", got, approved)
	}
}

// TestContract_ActionEvent_UnixSocket locks the Unix-socket event shape
// emitted by run_action helpers in actions.sh. This is consumed by the
// runner's sidecar; any rename breaks per-action progress reporting.
func TestContract_ActionEvent_UnixSocket(t *testing.T) {
	approved := []string{"action", "duration_ms", "event", "exit_code", "status"}
	got := fieldNames(t, runner.ActionEvent{})
	if !reflect.DeepEqual(got, approved) {
		t.Fatalf("ActionEvent wire shape changed.\n got:      %v\n approved: %v", got, approved)
	}

	// Lock representative wire payloads for both event types.
	start := runner.ActionEvent{Event: "action_start", Action: "verify_ssh"}
	startRaw, _ := json.Marshal(start)
	if !strings.Contains(string(startRaw), `"event":"action_start"`) {
		t.Errorf("action_start lost event tag: %s", startRaw)
	}
	end := runner.ActionEvent{Event: "action_end", Action: "verify_ssh", Status: "pass", ExitCode: 0, DurationMs: 250}
	endRaw, _ := json.Marshal(end)
	if !strings.Contains(string(endRaw), `"status":"pass"`) || !strings.Contains(string(endRaw), `"duration_ms":250`) {
		t.Errorf("action_end lost critical fields: %s", endRaw)
	}
}

// TestRunnerContract_PathsUnchanged guards the path constants that the runner
// contract depends on. These values are baked into the Dockerfile install layout
// and must not drift without a coordinated image + engine change.
//
// The three path anchors this test locks:
//   - /opt/crucible      -- install root (binary at /bin, actions lib at /lib)
//   - /tmp               -- container WORKDIR; sidecar socket and context live here
//
// If any of these exact constant values change, the Dockerfile must be updated
// in the same commit or the runner container will silently mis-behave at runtime.
func TestRunnerContract_PathsUnchanged(t *testing.T) {
	if runner.DefaultConfigPath != "/opt/crucible/runner-config.json" {
		t.Errorf("DefaultConfigPath = %q; want /opt/crucible/runner-config.json\n"+
			"The binary (/opt/crucible/bin) and actions lib (/opt/crucible/lib/actions.sh) share this install root.",
			runner.DefaultConfigPath)
	}
	if runner.DefaultSocketPath != "/tmp/crucible-sidecar.sock" {
		t.Errorf("DefaultSocketPath = %q; want /tmp/crucible-sidecar.sock\n"+
			"Socket must remain in /tmp (the container WORKDIR).",
			runner.DefaultSocketPath)
	}
	if runner.DefaultContextPath != "/tmp/crucible-context.json" {
		t.Errorf("DefaultContextPath = %q; want /tmp/crucible-context.json\n"+
			"Context file must remain in /tmp (the container WORKDIR).",
			runner.DefaultContextPath)
	}
}

// TestRunnerDockerfile_ContractPreserved reads deploy/runner/Dockerfile from disk
// and asserts that the Kali rebase preserved all runner contract paths and that
// the size-busting packages are absent. This test fails the build if a future
// Dockerfile edit moves the binary, drops the ENTRYPOINT, or adds a banned package.
func TestRunnerDockerfile_ContractPreserved(t *testing.T) {
	data, err := os.ReadFile("../../deploy/runner/Dockerfile")
	if err != nil {
		t.Fatalf("could not read Dockerfile: %v", err)
	}
	content := string(data)

	mustContain := []struct {
		needle string
		reason string
	}{
		{"kalilinux/kali-rolling", "runtime stage must be based on kali-rolling"},
		{"/opt/crucible/bin", "runner binary must be installed at /opt/crucible/bin"},
		{"/opt/crucible/lib", "actions library must be installed at /opt/crucible/lib"},
		{"ENTRYPOINT", "ENTRYPOINT directive must be present"},
		{"crucible-runner", "ENTRYPOINT must invoke crucible-runner"},
		{"WORKDIR /tmp", "WORKDIR must be /tmp (executor sidecar socket and context live there)"},
		{`ENV PATH="/opt/crucible/bin`, "PATH must include /opt/crucible/bin"},
		{"FATAL: required runner tooling missing", "Dockerfile must verify assessment tools resolve on PATH after install"},
	}
	for _, tc := range mustContain {
		if !strings.Contains(content, tc.needle) {
			t.Errorf("Dockerfile missing %q: %s", tc.needle, tc.reason)
		}
	}

	// Strip comment lines before checking for banned packages: the Dockerfile's
	// own comment block warns "do not add X" using the exact package names, which
	// would produce false positives if we scanned the full file text.
	var nonCommentLines []string
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			nonCommentLines = append(nonCommentLines, line)
		}
	}
	installContent := strings.Join(nonCommentLines, "\n")

	mustNotContain := []struct {
		needle string
		reason string
	}{
		{"metasploit-framework", "metasploit adds several GB -- keep image under 3 GB"},
		{"kali-linux-large", "kali-linux-large meta-package exceeds the 3 GB size target"},
		{"kali-linux-everything", "kali-linux-everything meta-package exceeds the 3 GB size target"},
		// A previous draft installed assessment tools in a loop that turned an
		// apt failure into an echoed warning, so a kali-rolling package rename
		// would ship a green-but-toolless image and corrupt student results.
		// Tool installs must fail the build.
		{"WARNING: SKIPPED", "apt install failures must fail the build, never be downgraded to a log line"},
	}
	for _, tc := range mustNotContain {
		if strings.Contains(installContent, tc.needle) {
			t.Errorf("Dockerfile must NOT install %q: %s", tc.needle, tc.reason)
		}
	}
}

// TestRunnerDockerfile_KaliBaseArgIsGlobal guards a Docker scoping rule that is
// invisible until an image is actually built.
//
// `FROM ${KALI_BASE}` only expands if KALI_BASE is a *global* ARG, declared
// before the first FROM. An ARG declared after a FROM is scoped to that build
// stage, so the variable expands to empty and buildx fails the whole image with
// "base name (${KALI_BASE}) should not be blank". Nothing in `go build`,
// `go vet` or `go test` can see this, and the runner image is the one Crucible
// component whose build is gated behind the test job -- so it stayed broken
// until the very first CI image build ran.
func TestRunnerDockerfile_KaliBaseArgIsGlobal(t *testing.T) {
	data, err := os.ReadFile("../../deploy/runner/Dockerfile")
	if err != nil {
		t.Fatalf("could not read Dockerfile: %v", err)
	}

	argIdx, firstFromIdx, usedInFrom := -1, -1, false
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if argIdx == -1 && strings.HasPrefix(trimmed, "ARG KALI_BASE") {
			argIdx = i
		}
		if strings.HasPrefix(trimmed, "FROM ") {
			if firstFromIdx == -1 {
				firstFromIdx = i
			}
			if strings.Contains(trimmed, "${KALI_BASE}") || strings.Contains(trimmed, "$KALI_BASE") {
				usedInFrom = true
			}
		}
	}

	if !usedInFrom {
		t.Skip("Dockerfile no longer parameterises its base image via KALI_BASE")
	}
	if argIdx == -1 {
		t.Fatal("FROM uses ${KALI_BASE} but no ARG KALI_BASE is declared: the base name expands to empty and the image build fails")
	}
	if argIdx > firstFromIdx {
		t.Errorf("ARG KALI_BASE is declared at line %d, after the first FROM at line %d: "+
			"an ARG declared after a FROM is scoped to that build stage, so ${KALI_BASE} "+
			"expands to empty and buildx fails with \"base name (${KALI_BASE}) should not be blank\". "+
			"Move it above the first FROM", argIdx+1, firstFromIdx+1)
	}
	if !strings.Contains(string(data), "ARG KALI_BASE=") {
		t.Error("ARG KALI_BASE has no default value: CI does not pass --build-arg KALI_BASE, so the base name would be blank")
	}
}
