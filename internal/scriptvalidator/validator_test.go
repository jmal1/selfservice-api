package scriptvalidator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeRunner returns canned stdout/stderr/exitCode without invoking a real
// subprocess, so tests run without shellcheck installed.
func fakeRunner(stdout, stderr string, exitCode int, err error) commandRunner {
	return func(ctx context.Context, name string, args []string, stdin string) ([]byte, []byte, int, error) {
		return []byte(stdout), []byte(stderr), exitCode, err
	}
}

const fixtureShellcheckJSON = `{
  "comments": [
    {
      "file": "-",
      "line": 3,
      "endLine": 3,
      "column": 5,
      "endColumn": 7,
      "level": "warning",
      "code": 2086,
      "message": "Double quote to prevent globbing and word splitting."
    },
    {
      "file": "-",
      "line": 7,
      "endLine": 7,
      "column": 1,
      "endColumn": 4,
      "level": "error",
      "code": 1009,
      "message": "The mentioned syntax error was in this if expression."
    },
    {
      "file": "-",
      "line": 12,
      "endLine": 12,
      "column": 9,
      "endColumn": 10,
      "level": "info",
      "code": 2155,
      "message": "Declare and assign separately to avoid masking return values."
    }
  ]
}`

// fixtureShellcheckJSON returns canned shellcheck JSON whose line numbers are
// computed relative to the synthetic preamble the validator wraps user
// scripts in. Tests that compare line numbers must use these helpers so they
// stay correct as the preamble grows.
func fixtureLinesAfterWrap(userLines ...int) []int {
	_, off := buildPreamble(Options{})
	out := make([]int, len(userLines))
	for i, l := range userLines {
		out[i] = l + off
	}
	return out
}

func fixtureShellcheckJSONFor(lines []int) string {
	if len(lines) != 3 {
		panic("fixtureShellcheckJSONFor expects exactly 3 line numbers")
	}
	return fmt.Sprintf(`{
  "comments": [
    {"file":"-","line":%d,"endLine":%d,"column":5,"endColumn":7,"level":"warning","code":2086,"message":"Double quote to prevent globbing and word splitting."},
    {"file":"-","line":%d,"endLine":%d,"column":1,"endColumn":4,"level":"error","code":1009,"message":"The mentioned syntax error was in this if expression."},
    {"file":"-","line":%d,"endLine":%d,"column":9,"endColumn":10,"level":"info","code":2155,"message":"Declare and assign separately to avoid masking return values."}
  ]
}`, lines[0], lines[0], lines[1], lines[1], lines[2], lines[2])
}

// shellcheckOnly filters out Crucible's own static checks (CRU*) so tests that
// exercise shellcheck-output parsing and line remapping assert on what they
// actually mean.
//
// This is NOT a relaxed assertion: these tests drive a FAKE shellcheck runner
// and verify that its output is parsed and remapped correctly. Validate() also
// runs independent CRU0001/CRU0002 passes over the same script, and the
// synthetic "line1/line2/..." fixtures legitimately trip CRU0002 because
// "line1" really is not a command. Counting those toward a shellcheck-parsing
// assertion would be measuring the wrong thing.
func shellcheckOnly(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if !strings.HasPrefix(f.Code, "CRU") {
			out = append(out, f)
		}
	}
	return out
}

func TestValidate_Bash_ParsesShellcheckOutput(t *testing.T) {
	// User script is 5 lines; pick finding lines that fall inside that range.
	userScript := "line1\nline2\nline3\nline4\nline5\n"
	wrapLines := fixtureLinesAfterWrap(3, 4, 5)
	v := NewValidator().WithRunner(fakeRunner(fixtureShellcheckJSONFor(wrapLines), "", 1, nil))

	res, err := v.Validate(context.Background(), "bash", userScript)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Language != "bash" {
		t.Errorf("expected language=bash, got %q", res.Language)
	}
	sc := shellcheckOnly(res.Findings)
	if len(sc) != 3 {
		t.Fatalf("expected 3 shellcheck findings, got %d: %+v", len(sc), sc)
	}
	if !res.HasErrors {
		t.Error("expected HasErrors=true (we have an SC1009 error)")
	}
	if !res.HasWarnings {
		t.Error("expected HasWarnings=true (we have an SC2086 warning)")
	}
	got := sc[0]
	// Column 5 in wrapped script — we tab-indent user lines by 1 — so user-
	// space column is 4.
	if got.Code != "SC2086" || got.Severity != SeverityWarning || got.Line != 3 || got.Column != 4 {
		t.Errorf("finding[0] wrong: %+v", got)
	}
	if sc[1].Code != "SC1009" || sc[1].Severity != SeverityError || sc[1].Line != 4 {
		t.Errorf("finding[1] wrong: %+v", sc[1])
	}
	if sc[2].Severity != SeverityInfo || sc[2].Line != 5 {
		t.Errorf("finding[2] wrong: %+v", sc[2])
	}
}

func TestValidate_Bash_EmptyOutputIsNoFindings(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner(`{"comments":[]}`, "", 0, nil))
	res, err := v.Validate(context.Background(), "bash", "echo hello\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(res.Findings))
	}
	if res.HasErrors || res.HasWarnings {
		t.Error("expected no errors/warnings on clean script")
	}
}

func TestValidate_Bash_TotallyEmptyStdoutIsNoFindings(t *testing.T) {
	// Some shellcheck versions print nothing at all when there are no findings.
	v := NewValidator().WithRunner(fakeRunner("", "", 0, nil))
	res, err := v.Validate(context.Background(), "bash", "echo hello\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(res.Findings))
	}
}

func TestValidate_UnsupportedLanguage(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner("", "", 0, nil))
	_, err := v.Validate(context.Background(), "python", "print('hi')")
	if !errors.Is(err, ErrUnsupportedLanguage) {
		t.Fatalf("expected ErrUnsupportedLanguage for python, got %v", err)
	}
}

func TestValidate_PowerShellAcceptedButSkipped(t *testing.T) {
	// PowerShell is recognised but we don't have a linter for it yet —
	// callers should get a clean empty Result rather than an error so the
	// Save button stays enabled and the editor isn't blocked.
	v := NewValidator().WithRunner(fakeRunner("", "", 0, nil))
	res, err := v.Validate(context.Background(), "powershell", "Write-Host hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || len(res.Findings) != 0 || res.Language != "powershell" {
		t.Errorf("expected clean powershell result, got %+v", res)
	}
}

func TestValidate_LanguageAliases(t *testing.T) {
	for _, lang := range []string{"bash", "sh", "shell", "BASH", "Shell"} {
		v := NewValidator().WithRunner(fakeRunner(`{"comments":[]}`, "", 0, nil))
		if _, err := v.Validate(context.Background(), lang, "echo hi"); err != nil {
			t.Errorf("language %q should be supported: %v", lang, err)
		}
	}
}

func TestValidate_ScriptTooLarge(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner("", "", 0, nil)).WithMaxScriptKB(1)
	huge := strings.Repeat("a", 1024*2)
	_, err := v.Validate(context.Background(), "bash", huge)
	if !errors.Is(err, ErrScriptTooLarge) {
		t.Fatalf("expected ErrScriptTooLarge, got %v", err)
	}
}

func TestValidate_InvocationFailureBubblesUp(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner("", "shellcheck: command not found", 0, errors.New("exec: \"shellcheck\": executable file not found in $PATH")))
	_, err := v.Validate(context.Background(), "bash", "echo hi")
	if err == nil {
		t.Fatal("expected error when invocation fails")
	}
	if !strings.Contains(err.Error(), "shellcheck invocation failed") {
		t.Errorf("error should mention invocation failure: %v", err)
	}
}

func TestValidate_ShellcheckCrash(t *testing.T) {
	// Exit code >1 = shellcheck itself crashed, not just findings.
	v := NewValidator().WithRunner(fakeRunner("", "panic: something bad", 2, nil))
	_, err := v.Validate(context.Background(), "bash", "echo hi")
	if err == nil {
		t.Fatal("expected error when shellcheck exits >1")
	}
}

func TestValidate_MalformedJSON(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner("not json at all", "", 1, nil))
	_, err := v.Validate(context.Background(), "bash", "echo hi")
	if err == nil {
		t.Fatal("expected error on malformed shellcheck JSON")
	}
	if !strings.Contains(err.Error(), "parse shellcheck output") {
		t.Errorf("expected parse error, got: %v", err)
	}
}

func TestValidate_DefaultsEndLineAndColumn(t *testing.T) {
	// Some shellcheck findings omit endLine/endColumn; we fill them in.
	// Place the finding inside the user-script range after wrapping so it
	// survives remapFindings.
	userLines := fixtureLinesAfterWrap(2)
	fixture := fmt.Sprintf(`{"comments":[{"file":"-","line":%d,"column":2,"level":"warning","code":2086,"message":"x"}]}`, userLines[0])
	v := NewValidator().WithRunner(fakeRunner(fixture, "", 1, nil))
	res, err := v.Validate(context.Background(), "bash", "line1\nline2\nline3\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sc := shellcheckOnly(res.Findings)
	if len(sc) != 1 {
		t.Fatalf("expected 1 shellcheck finding, got %d: %+v", len(sc), sc)
	}
	f := sc[0]
	if f.Line != 2 {
		t.Errorf("Line remapping wrong: got %d, want 2", f.Line)
	}
	if f.EndLine != 2 {
		t.Errorf("EndLine default wrong: %d", f.EndLine)
	}
	// Column 2 in wrapped script - 1 (tab indent) = column 1 in user space.
	// EndColumn was column+1 = 3 in wrapped, -1 = 2 in user space.
	if f.Column != 1 || f.EndColumn != 2 {
		t.Errorf("column remap wrong: col=%d endCol=%d", f.Column, f.EndColumn)
	}
}

func TestParseShellcheckJSON_UnknownSeverityFallsBackToInfo(t *testing.T) {
	raw := []byte(`{"comments":[{"file":"-","line":1,"column":1,"level":"unknown","code":9999,"message":"x"}]}`)
	got, err := parseShellcheckJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Severity != SeverityInfo {
		t.Errorf("expected info fallback, got %+v", got)
	}
}

// captureStdinRunner records the stdin shellcheck receives so we can assert
// the validator stripped CRs before invoking the linter.
func captureStdinRunner(captured *string) commandRunner {
	return func(ctx context.Context, name string, args []string, stdin string) ([]byte, []byte, int, error) {
		*captured = stdin
		return []byte(`{"comments":[]}`), nil, 0, nil
	}
}

func TestValidate_StripsCarriageReturns(t *testing.T) {
	var stdin string
	v := NewValidator().WithRunner(captureStdinRunner(&stdin))

	in := "echo hi\r\nls -la\r\n"
	if _, err := v.Validate(context.Background(), "bash", in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsRune(stdin, '\r') {
		t.Errorf("validator forwarded \\r to shellcheck: %q", stdin)
	}
	if !strings.Contains(stdin, "echo hi\n") || !strings.Contains(stdin, "ls -la\n") {
		t.Errorf("expected normalized user body in stdin, got: %q", stdin)
	}
}

func TestValidate_StripsBareCarriageReturns(t *testing.T) {
	var stdin string
	v := NewValidator().WithRunner(captureStdinRunner(&stdin))

	if _, err := v.Validate(context.Background(), "bash", "a\rb\rc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.ContainsRune(stdin, '\r') {
		t.Errorf("bare \\r not normalized: %q", stdin)
	}
	if !strings.Contains(stdin, "a\n") || !strings.Contains(stdin, "b\n") || !strings.Contains(stdin, "c") {
		t.Errorf("expected each user line in stdin, got: %q", stdin)
	}
}

// captureArgsRunner records the argv shellcheck was invoked with so we can
// assert the exclusion list is wired through.
func captureArgsRunner(captured *[]string) commandRunner {
	return func(ctx context.Context, name string, args []string, stdin string) ([]byte, []byte, int, error) {
		*captured = args
		return []byte(`{"comments":[]}`), nil, 0, nil
	}
}

func TestValidate_PassesExclusionList(t *testing.T) {
	var args []string
	v := NewValidator().WithRunner(captureArgsRunner(&args))

	if _, err := v.Validate(context.Background(), "bash", "echo hi"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var excludeArg string
	for _, a := range args {
		if strings.HasPrefix(a, "--exclude=") {
			excludeArg = a
			break
		}
	}
	if excludeArg == "" {
		t.Fatalf("validator did not pass --exclude flag; args=%v", args)
	}
	// Each rule from the package-level list must be present so the docstring
	// stays in sync with the actual lint config.
	for _, code := range excludedShellcheckCodes {
		if !strings.Contains(excludeArg, code) {
			t.Errorf("exclude flag %q missing code %s", excludeArg, code)
		}
	}
}

// TestValidate_TimeoutCancelsRunner ensures the per-invocation timeout cuts
// off a runaway shellcheck process. The fake runner blocks on the context
// being cancelled; if the timeout is wired correctly the call returns an
// error within ~50ms instead of hanging.
func TestValidate_TimeoutCancelsRunner(t *testing.T) {
	blockingRunner := func(ctx context.Context, name string, args []string, stdin string) ([]byte, []byte, int, error) {
		<-ctx.Done()
		return nil, nil, 0, ctx.Err()
	}
	v := NewValidator().WithRunner(blockingRunner).WithTimeout(50 * time.Millisecond)

	start := time.Now()
	_, err := v.Validate(context.Background(), "bash", "echo hi")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Should fire well before any plausible shellcheck runtime.
	if elapsed > 500*time.Millisecond {
		t.Errorf("validator did not cancel promptly; elapsed=%s", elapsed)
	}
}

// TestValidate_WiresCRU0002 asserts the unknown-command check is reachable
// through Validate, not merely implemented.
//
// The original CRU0002 tests all called unknownCommandFindings directly, so
// deleting its call site in Validate left every one of them green -- the exact
// dead-wiring failure class documented in internal/ci/wiring_test.go. Verified
// by removing the call from Validate and watching this test fail.
func TestValidate_WiresCRU0002(t *testing.T) {
	v := NewValidator().WithRunner(fakeRunner(`{"comments":[]}`, "", 0, nil))

	res, err := v.Validate(context.Background(), "bash", "sqlmap --url http://x\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found *Finding
	for i := range res.Findings {
		if res.Findings[i].Code == "CRU0002" {
			found = &res.Findings[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Validate did not emit CRU0002 for a command absent from the runner image; "+
			"the check is implemented but not wired into Validate, so instructors get no "+
			"authoring-time warning and the action fails at runtime with exit 127. "+
			"got findings: %+v", res.Findings)
	}
	if !strings.Contains(found.Message, "sqlmap") {
		t.Errorf("CRU0002 message should name the missing command, got %q", found.Message)
	}
	if found.Severity != SeverityWarning {
		t.Errorf("CRU0002 should be a warning (the manifest cannot know about "+
			"self-installed tools), got %q", found.Severity)
	}
}
