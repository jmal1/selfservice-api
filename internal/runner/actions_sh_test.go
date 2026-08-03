package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
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
