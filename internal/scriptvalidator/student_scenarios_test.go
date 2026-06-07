package scriptvalidator

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// requireShellcheck skips the test when shellcheck isn't installed locally —
// these scenarios depend on the real linter, not the fake runner.
func requireShellcheck(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skipf("shellcheck not on PATH; skipping scenario test (%v)", err)
	}
}

// TestStudentScenarios codifies the validator's contract from a student's
// perspective: things that are idiomatic Crucible action code MUST validate
// cleanly, and things that are real bugs MUST surface. If any of these
// assertions break, it means the editor is either too noisy (false positives
// hurt students) or too quiet (real bugs slip through to runtime).
//
// Skipped when shellcheck isn't available locally — every CI build and the
// API Docker image have it baked in.
func TestStudentScenarios(t *testing.T) {
	requireShellcheck(t)

	v := NewValidator()

	t.Run("happy_path_action_validates_clean", func(t *testing.T) {
		// Canonical action body: declared CTX_* inputs, local vars, ctx_set
		// outputs, LAST_ERROR + LAST_STUDENT_MSG, return code.
		script := `local cmd="${CTX_CMD:-}" expect="${CTX_EXPECT:-}"
local output
output=$(eval "$cmd" 2>&1) || {
    LAST_ERROR="Command failed: $cmd"
    LAST_STUDENT_MSG="Run: $cmd"
    return 1
}
ctx_set "command_output" "$output"
if [[ "$output" != *"$expect"* ]]; then
    LAST_ERROR="Output did not contain $expect"
    return 1
fi
return 0
`
		res, err := v.Validate(context.Background(), "bash", script,
			Options{
				InputContextNames:  []string{"cmd", "expect"},
				OutputContextNames: []string{"command_output"},
			})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if res.HasErrors {
			t.Errorf("happy-path action should validate clean; got findings: %s", dumpFindings(res.Findings))
		}
	})

	t.Run("undeclared_ctx_var_is_surfaced", func(t *testing.T) {
		// Instructor referenced CTX_TYPO but only declared CTX_PATH; the
		// wrapper does NOT pre-declare CTX_TYPO so SC2154 should fire.
		script := `if [[ -f "$CTX_TYPO" ]]; then
    echo "exists"
fi
`
		res, err := v.Validate(context.Background(), "bash", script,
			Options{InputContextNames: []string{"path"}})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if !hasCode(res.Findings, "SC2154") {
			t.Errorf("expected SC2154 (referenced but not assigned) for CTX_TYPO; got: %s",
				dumpFindings(res.Findings))
		}
	})

	t.Run("unquoted_variable_is_warned", func(t *testing.T) {
		// Real bug — splits on whitespace, globs on *.
		script := `local file="${CTX_FILE:-}"
if [ -f $file ]; then
    cat $file
fi
`
		res, err := v.Validate(context.Background(), "bash", script,
			Options{InputContextNames: []string{"file"}})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if !hasCode(res.Findings, "SC2086") {
			t.Errorf("expected SC2086 (double quote) for unquoted $file; got: %s",
				dumpFindings(res.Findings))
		}
	})

	t.Run("missing_then_is_surfaced", func(t *testing.T) {
		// Syntax error — if without then.
		script := `if [ -f /etc/hosts ]
    echo "yes"
fi
`
		res, err := v.Validate(context.Background(), "bash", script)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if !res.HasErrors {
			t.Errorf("expected an error for missing 'then'; got: %s", dumpFindings(res.Findings))
		}
	})

	t.Run("powershell_body_returns_clean_skip", func(t *testing.T) {
		// Several actions in the prod corpus are PowerShell. Sending them
		// through the bash linter would produce noise; detectShellLanguage
		// catches them and we return a clean result.
		script := `param([string]$Service)
$svc = Get-Service -Name $Service
if ($svc.Status -ne "Running") {
    Write-Output "stopped"
    exit 1
}
`
		res, err := v.Validate(context.Background(), "bash", script)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if len(res.Findings) != 0 {
			t.Errorf("PowerShell body should skip cleanly; got findings: %s",
				dumpFindings(res.Findings))
		}
		if res.Language != "powershell" {
			t.Errorf("expected Result.Language=powershell after detection, got %q", res.Language)
		}
	})

	t.Run("typical_action_with_local_is_not_flagged_for_sc2168", func(t *testing.T) {
		// Top-level `local` is a real-world bash idiom inside Crucible
		// actions because the runner wraps them in a function. SC2168
		// would flag this if we weren't wrapping — the smart wrapper
		// suppresses the false positive without disabling the rule.
		script := `local x=1
local y=2
echo $((x + y))
`
		res, err := v.Validate(context.Background(), "bash", script)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if hasCode(res.Findings, "SC2168") {
			t.Errorf("SC2168 should not fire for top-level local in actions; got: %s",
				dumpFindings(res.Findings))
		}
	})

	t.Run("LAST_ERROR_assignment_is_not_flagged_unused", func(t *testing.T) {
		// LAST_ERROR is read by the wrapper after the function returns;
		// SC2034 would flag it as unused if the wrapper didn't reference it.
		script := `LAST_ERROR="something broke"
return 1
`
		res, err := v.Validate(context.Background(), "bash", script)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if hasCode(res.Findings, "SC2034") {
			t.Errorf("SC2034 should not fire for LAST_ERROR (referenced by wrapper); got: %s",
				dumpFindings(res.Findings))
		}
	})

	t.Run("unused_local_still_flagged_for_sc2034", func(t *testing.T) {
		// We don't want to over-suppress SC2034 — a genuinely unused local
		// is still a real bug.
		script := `local thisIsUnused="never read"
echo "hi"
`
		res, err := v.Validate(context.Background(), "bash", script)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if !hasCode(res.Findings, "SC2034") {
			t.Errorf("expected SC2034 for genuinely-unused local; got: %s",
				dumpFindings(res.Findings))
		}
	})
}

func hasCode(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

func dumpFindings(fs []Finding) string {
	var b strings.Builder
	for i, f := range fs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.Code)
		b.WriteString("(")
		b.WriteString(string(f.Severity))
		b.WriteString(")@L")
		b.WriteByte(byte('0' + f.Line/10))
		b.WriteByte(byte('0' + f.Line%10))
		b.WriteString(": ")
		b.WriteString(f.Message)
	}
	return b.String()
}
