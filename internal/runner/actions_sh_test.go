package runner_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// These tests execute deploy/runner/actions.sh for real, rather than asserting
// on its text. That distinction matters here: the defect they guard against was
// invisible to every form of inspection short of running it.
//
// Background. Library actions live in the database as bash function bodies, and
// the engine now injects them into the runner as a sourceable file. That alone
// is NOT enough to make them callable, because run_action dispatched every
// action through
//
//	output=$(timeout "$timeout_sec" "$@" 2>&1)
//
// and `timeout` is an external binary that execve()s its argument. A shell
// function is not an executable file, so every library action died with
//
//	timeout: failed to run command 'http_get': No such file or directory
//
// and exit code 127 — reported to the student as a failed check, as though they
// had misconfigured something.

func actionsShSource(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "deploy", "runner", "actions.sh")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read actions.sh: %v", err)
	}
	return string(b)
}

// runActionsShScript materialises actions.sh and a fake action library inside a
// bash-native temp dir and runs body against them.
//
// The file contents are embedded in the script via heredoc rather than passed
// as a path, because a Windows path (Z:\Repos\…) is not openable by git-bash and
// the whole test would fail for reasons unrelated to what it checks.
func runActionsShScript(t *testing.T, library, body string) string {
	t.Helper()
	return runActionsShScriptVars(t, library, body, nil)
}

// runActionsShScriptVars is runActionsShScript with extra shell variables
// defined for the body.
//
// Values are injected base64-encoded and decoded inside bash rather than passed
// through the process environment. Two reasons: `bash` on a developer machine
// may be WSL, which does not inherit the Windows environment at all — so an
// env-passed value silently arrives empty and the test asserts nothing, which is
// exactly how this helper failed the first time — and base64 is pure ASCII, so
// the test's own quoting can never be mistaken for the escaping under test. The
// `; printf X` / `%X` pair preserves trailing newlines, which command
// substitution would otherwise strip.
func runActionsShScriptVars(t *testing.T, library, body string, vars map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	var decls strings.Builder
	for k, v := range vars {
		enc := base64.StdEncoding.EncodeToString([]byte(v))
		fmt.Fprintf(&decls, "%s=$(printf '%%s' '%s' | base64 -d; printf X); %s=${%s%%X}\n", k, enc, k, k)
	}

	script := "set -uo pipefail\n" +
		"tmpdir=$(mktemp -d)\n" +
		"trap 'rm -rf \"$tmpdir\"' EXIT\n" +
		"cat > \"$tmpdir/actions.sh\" <<'CRUCIBLE_ACTIONS_EOF'\n" + actionsShSource(t) + "\nCRUCIBLE_ACTIONS_EOF\n" +
		"cat > \"$tmpdir/library.sh\" <<'CRUCIBLE_LIBRARY_EOF'\n" + library + "\nCRUCIBLE_LIBRARY_EOF\n" +
		"export CRUCIBLE_ACTIONS_LIB=\"$tmpdir/actions.sh\"\n" +
		"export CRUCIBLE_ACTION_LIBRARY=\"$tmpdir/library.sh\"\n" +
		"export CRUCIBLE_CONTEXT=\"$tmpdir/context.json\"\n" +
		"export CRUCIBLE_SOCKET=\"$tmpdir/no-such.sock\"\n" +
		"source \"$CRUCIBLE_ACTIONS_LIB\"\n" +
		decls.String() +
		body

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

const demoLibrary = `demo_action() {
    local value=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --value) value="$2"; shift 2;;
            *) shift;;
        esac
    done
    if [ "$value" = "good" ]; then
        echo "DEMO_OK"
        return 0
    fi
    LAST_ERROR="value was '$value'"
    LAST_STUDENT_MSG="Expected good, got '$value'."
    return 1
}`

func TestActionsSh_RunActionDispatchesShellFunctions(t *testing.T) {
	// The core fix: a library action is a shell function and must be callable.
	out := runActionsShScript(t, demoLibrary,
		"if run_action \"Demo\" demo_action --value good; then echo RC_ZERO; else echo RC_NONZERO; fi\n")

	if strings.Contains(out, "No such file or directory") {
		t.Fatalf("run_action still cannot dispatch a shell function (the exit-127 defect):\n%s", out)
	}
	if !strings.Contains(out, "DEMO_OK") {
		t.Errorf("action body did not execute; output was:\n%s", out)
	}
	if !strings.Contains(out, "RC_ZERO") {
		t.Errorf("a passing library action should return 0, got:\n%s", out)
	}
}

func TestActionsSh_RunActionPropagatesFunctionFailure(t *testing.T) {
	// A failing action must fail — a dispatch fix that swallowed non-zero would
	// turn every check into a pass, which is far worse than exit 127.
	out := runActionsShScript(t, demoLibrary,
		"if run_action \"Demo\" demo_action --value bad; then echo RC_ZERO; else echo RC_NONZERO; fi\n")

	if !strings.Contains(out, "RC_NONZERO") {
		t.Errorf("a failing library action must propagate non-zero, got:\n%s", out)
	}
}

func TestActionsSh_StudentMessageEscapesTheSubshell(t *testing.T) {
	// Library bodies report problems by ASSIGNING LAST_STUDENT_MSG/LAST_ERROR,
	// but run_action harvests the student message by grepping stdout for
	// "STUDENT_MSG:". The body runs in a command-substitution subshell, so a
	// bare assignment can never reach the caller — every library action would
	// fail with a correct exit code and no explanation. run_action therefore
	// emits both variables before exiting the subshell.
	out := runActionsShScript(t, demoLibrary,
		"run_action \"Demo\" demo_action --value bad || true\n")

	if !strings.Contains(out, "STUDENT_MSG:Expected good") {
		t.Errorf("LAST_STUDENT_MSG did not escape the subshell in the form run_action greps for; "+
			"students would see a failed check with no reason. Output:\n%s", out)
	}
	if !strings.Contains(out, "ERROR:value was 'bad'") {
		t.Errorf("LAST_ERROR did not escape the subshell, so instructor diagnostics are lost. Output:\n%s", out)
	}
}

func TestActionsSh_PassingActionEmitsNoStudentMessage(t *testing.T) {
	// The emit must be conditional. Unconditionally echoing an empty
	// STUDENT_MSG: line would attach a blank message to every passing check.
	out := runActionsShScript(t, demoLibrary,
		"run_action \"Demo\" demo_action --value good || true\n")

	if strings.Contains(out, "STUDENT_MSG:") {
		t.Errorf("a passing action must not emit a student message, got:\n%s", out)
	}
	if strings.Contains(out, "ERROR:") {
		t.Errorf("a passing action must not emit an error, got:\n%s", out)
	}
}

func TestActionsSh_RunActionStillDispatchesExternalCommands(t *testing.T) {
	// The function branch must not regress plain binaries, which is how every
	// existing non-library action is invoked.
	out := runActionsShScript(t, demoLibrary,
		"if run_action \"Echo\" echo hello-from-binary; then echo RC_ZERO; else echo RC_NONZERO; fi\n")

	if !strings.Contains(out, "hello-from-binary") {
		t.Errorf("external command dispatch regressed, output was:\n%s", out)
	}
	if !strings.Contains(out, "RC_ZERO") {
		t.Errorf("external command should return 0, got:\n%s", out)
	}
}

func TestActionsSh_ActionBodyFailureDoesNotAbortViaSetE(t *testing.T) {
	// Action bodies detect their own failures and set LAST_STUDENT_MSG before
	// returning. Under the inherited `set -e` the first non-zero command inside
	// the body aborts it immediately, so the student gets a bare non-zero exit
	// instead of an actionable message. run_action's subshell therefore runs
	// with `set +e`; this proves it.
	lib := `probing_action() {
    /bin/false
    echo "REACHED_AFTER_FAILING_COMMAND"
    LAST_STUDENT_MSG="checked and failed"
    return 1
}`
	out := runActionsShScript(t, lib,
		"if run_action \"Probe\" probing_action; then echo RC_ZERO; else echo RC_NONZERO; fi\n")

	if !strings.Contains(out, "REACHED_AFTER_FAILING_COMMAND") {
		t.Errorf("body aborted at the first failing command; it cannot report a useful message. Output:\n%s", out)
	}
	if !strings.Contains(out, "RC_NONZERO") {
		t.Errorf("body's own `return 1` must still surface as a failure, got:\n%s", out)
	}
}

// sidecarEvents runs body with _sidecar_send replaced by a capture shim and
// returns the decoded event payloads.
//
// The shim is installed *after* actions.sh is sourced, which works because bash
// resolves function calls at call time. Capturing the real payload is the point:
// these tests are about the bytes that reach the sidecar, and asserting on
// anything less would not have caught a payload that the sidecar cannot decode.
func sidecarEvents(t *testing.T, library, body string) []runner.ActionEvent {
	t.Helper()
	out := runActionsShScript(t, library,
		"_sidecar_send() { echo \"SIDECAR_EVENT:$1\"; }\n"+body)

	var events []runner.ActionEvent
	for _, line := range strings.Split(out, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "SIDECAR_EVENT:")
		if !ok {
			continue
		}
		var ev runner.ActionEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("sidecar payload is not decodable JSON — the sidecar would drop this\n"+
				"event entirely and the action would never be reported.\npayload: %s\nerror: %v\nfull output:\n%s",
				raw, err, out)
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatalf("no sidecar events captured; output was:\n%s", out)
	}
	return events
}

func actionEndEvent(t *testing.T, events []runner.ActionEvent) runner.ActionEvent {
	t.Helper()
	for _, ev := range events {
		if ev.Event == "action_end" {
			return ev
		}
	}
	t.Fatalf("no action_end event among %d events", len(events))
	return runner.ActionEvent{}
}

func TestActionsSh_ActionEndCarriesStudentMessage(t *testing.T) {
	// The student message must travel ON the event, not be left for the executor
	// to scrape out of the workflow's stdout.
	//
	// run_action necessarily sends the event before it echoes the action's
	// output, and the socket and the stdout pipe are independent channels, so a
	// stdout-only harvest races the Go reader draining the pipe. In production
	// that race is routinely lost: the observed symptom was a failing action
	// whose message field was empty even though "STUDENT_MSG:Web server returned
	// 000 instead of 200" was sitting in the captured body.
	ev := actionEndEvent(t, sidecarEvents(t, demoLibrary,
		"run_action \"Demo\" demo_action --value bad || true\n"))

	if ev.Status != "fail" {
		t.Errorf("status = %q, want fail", ev.Status)
	}
	if ev.Message != "Expected good, got 'bad'." {
		t.Errorf("action_end carried message %q; the student sees a failed check with no reason", ev.Message)
	}
}

func TestActionsSh_ActionEndOmitsMessageOnPass(t *testing.T) {
	// A passing check must not carry a stale or blank explanation.
	ev := actionEndEvent(t, sidecarEvents(t, demoLibrary,
		"run_action \"Demo\" demo_action --value good || true\n"))

	if ev.Status != "pass" {
		t.Errorf("status = %q, want pass", ev.Status)
	}
	if ev.Message != "" {
		t.Errorf("passing action carried message %q, want empty", ev.Message)
	}
}

func TestActionsSh_EventPayloadSurvivesJSONMetacharacters(t *testing.T) {
	// Action names come from instructor-authored workflow scripts and student
	// messages are derived from command output, so both can contain quotes and
	// backslashes. Splicing them into a JSON template unescaped produces a
	// payload the sidecar cannot decode — and a decode failure is silent: the
	// action's result simply never arrives, so the run reports fewer actions
	// than it actually executed.
	lib := `nasty_action() {
    LAST_STUDENT_MSG='Expected "200" but got C:\path\to\nothing	(tab)'
    return 1
}`
	ev := actionEndEvent(t, sidecarEvents(t, lib,
		"run_action 'Check \"quoted\" C:\\path' nasty_action || true\n"))

	if ev.Action != `Check "quoted" C:\path` {
		t.Errorf("action name mangled in transit: %q", ev.Action)
	}
	if !strings.Contains(ev.Message, `Expected "200"`) || !strings.Contains(ev.Message, `C:\path\to`) {
		t.Errorf("student message mangled in transit: %q", ev.Message)
	}
}

func TestJSONEscape_ProducesValidJSON(t *testing.T) {
	cases := map[string]string{
		"plain":                  "all good",
		"double quote":           `he said "no"`,
		"backslash":              `C:\Users\student`,
		"trailing backslash":     `trailing\`,
		"escaped looking":        `\"not really escaped\"`,
		"newline":                "line one\nline two",
		"carriage return":        "line one\rline two",
		"tab":                    "col1\tcol2",
		"everything":             "a\"b\\c\nd\te\rf",
		"empty":                  "",
		"json injection attempt": `x","event":"action_start","injected":"`,
	}

	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			// Feed the input through the environment so the test's own quoting
			// cannot be mistaken for the escaping under test.
			out := runActionsShScriptVars(t, "",
				"printf 'ESCAPED:{\"v\":\"%s\"}\\n' \"$(_json_escape \"$CRUCIBLE_TEST_INPUT\")\"\n",
				map[string]string{"CRUCIBLE_TEST_INPUT": in})

			var raw string
			for _, line := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ESCAPED:"); ok {
					raw = v
				}
			}
			if raw == "" {
				t.Fatalf("no escaped output captured:\n%s", out)
			}

			var decoded struct {
				V string `json:"v"`
			}
			if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
				t.Fatalf("_json_escape produced a value that breaks the JSON literal.\n"+
					"input:  %q\npayload: %s\nerror: %v", in, raw, err)
			}
			// Newline/CR/tab survive as themselves through JSON; the escaping is
			// lossless, not merely valid.
			if decoded.V != in {
				t.Errorf("round-trip changed the value.\ninput:  %q\noutput: %q", in, decoded.V)
			}
		})
	}
}

func TestActionsSh_MissingLibraryDoesNotBreakSourcing(t *testing.T) {
	// The library is optional. If sourcing actions.sh fails when it is absent,
	// every workflow dies before running anything.
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := "set -uo pipefail\n" +
		"tmpdir=$(mktemp -d)\n" +
		"trap 'rm -rf \"$tmpdir\"' EXIT\n" +
		"cat > \"$tmpdir/actions.sh\" <<'CRUCIBLE_ACTIONS_EOF'\n" + actionsShSource(t) + "\nCRUCIBLE_ACTIONS_EOF\n" +
		"export CRUCIBLE_ACTIONS_LIB=\"$tmpdir/actions.sh\"\n" +
		"export CRUCIBLE_ACTION_LIBRARY=\"$tmpdir/definitely-absent.sh\"\n" +
		"export CRUCIBLE_CONTEXT=\"$tmpdir/context.json\"\n" +
		"export CRUCIBLE_SOCKET=\"$tmpdir/no-such.sock\"\n" +
		"source \"$CRUCIBLE_ACTIONS_LIB\"\n" +
		"echo SOURCED_OK\n"

	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "SOURCED_OK") {
		t.Fatalf("sourcing actions.sh failed with no library present:\n%s", out)
	}
}
