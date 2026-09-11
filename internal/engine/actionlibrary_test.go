package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-crucible-runner/runner"
)

// realisticBody mirrors the shape of an actual library action body: it uses
// `local`, `return 1`, and sets LAST_ERROR / LAST_STUDENT_MSG. All three are
// only legal inside a function, which is why the bodies must be wrapped rather
// than concatenated.
const realisticBody = `local url="$1"
local expect="${2:-200}"
local code
code=$(curl -s -o /dev/null -w '%{http_code}' "$url") || true
if [ "$code" != "$expect" ]; then
    LAST_ERROR="expected $expect, got $code"
    LAST_STUDENT_MSG="The web server did not respond with $expect."
    return 1
fi
return 0`

func TestLibraryFuncName_MapsHyphensToUnderscores(t *testing.T) {
	cases := map[string]string{
		"http-get":         "http_get",
		"port-open":        "port_open",
		"ssh-exec":         "ssh_exec",
		"wait-for":         "wait_for",
		"ufw-rule-exists":  "ufw_rule_exists",
		"commandcheck":     "commandcheck",
		"nmap-service":     "nmap_service",
		"smb-share-access": "smb_share_access",
	}
	for slug, want := range cases {
		got, err := libraryFuncName(slug)
		if err != nil {
			t.Fatalf("libraryFuncName(%q): unexpected error %v", slug, err)
		}
		if got != want {
			t.Errorf("libraryFuncName(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestLibraryFuncName_RejectsIllegalIdentifiers(t *testing.T) {
	// A slug that cannot become a legal shell identifier must be a loud error.
	// Emitting it anyway would produce a file that fails to source, which takes
	// out every action in the run rather than just this one.
	for _, slug := range []string{"", "9lives", "has space", "has.dot", "has/slash", "UPPER", "has$dollar"} {
		if got, err := libraryFuncName(slug); err == nil {
			t.Errorf("libraryFuncName(%q) = %q, want error", slug, got)
		}
	}
}

func TestBuildActionLibrary_WrapsBodiesAsCallableFunctions(t *testing.T) {
	lib, err := buildActionLibrary([]LibraryAction{
		{Slug: "http-get", Name: "HTTP GET", Description: "Fetch a URL", Script: realisticBody},
	})
	if err != nil {
		t.Fatalf("buildActionLibrary: %v", err)
	}
	if !strings.Contains(lib, "http_get() {") {
		t.Fatalf("generated library does not define http_get():\n%s", lib)
	}
	if !strings.Contains(lib, "LAST_STUDENT_MSG=") {
		t.Errorf("action body was not carried into the function:\n%s", lib)
	}
	// The whole reason this feature exists: `run_action ... http_get` must
	// resolve. Prove it does, by sourcing the library and calling it.
	requireBash(t)
	script := lib + "\nif declare -F http_get >/dev/null; then echo RESOLVED; else echo MISSING; fi\n"
	out := runBash(t, script)
	if !strings.Contains(out, "RESOLVED") {
		t.Fatalf("http_get is not callable after sourcing the library, got: %q", out)
	}
}

func TestBuildActionLibrary_OutputIsValidBash(t *testing.T) {
	requireBash(t)
	lib, err := buildActionLibrary([]LibraryAction{
		{Slug: "http-get", Name: "HTTP GET", Script: realisticBody},
		{Slug: "port-open", Name: "Port Open", Description: "Multi\nline\ndescription", Script: "local p=\"$1\"\nnc -z \"$TARGET_IP\" \"$p\"\nreturn $?"},
		{Slug: "wait-for", Name: "Wait For", Script: "sleep \"${1:-1}\"\nreturn 0"},
	})
	if err != nil {
		t.Fatalf("buildActionLibrary: %v", err)
	}
	if err := runner.ValidateBashSyntax(lib); err != nil {
		t.Fatalf("generated library is not valid bash: %v\n---\n%s", err, lib)
	}
}

func TestBuildActionLibrary_RejectsCollidingFunctionNames(t *testing.T) {
	// `http-get` and `http_get` are distinct slugs that map to the same shell
	// function. Silently letting the second win would make a workflow call the
	// wrong action body -- a wrong-answer bug in student assessment, not a crash.
	_, err := buildActionLibrary([]LibraryAction{
		{Slug: "http-get", Name: "A", Script: "return 0"},
		{Slug: "http_get", Name: "B", Script: "return 1"},
	})
	if err == nil {
		t.Fatal("expected a collision error for http-get vs http_get, got nil")
	}
	if !strings.Contains(err.Error(), "http_get") {
		t.Errorf("collision error should name the offending function, got: %v", err)
	}
}

func TestBuildActionLibrary_SkipsEmptyBodies(t *testing.T) {
	// `name() { }` is a bash syntax error, and because the library is sourced as
	// one file, a single empty body would break every other action too.
	requireBash(t)
	lib, err := buildActionLibrary([]LibraryAction{
		{Slug: "empty-one", Name: "Empty", Script: "   \n\t\n"},
		{Slug: "good-one", Name: "Good", Script: "return 0"},
	})
	if err != nil {
		t.Fatalf("buildActionLibrary: %v", err)
	}
	if strings.Contains(lib, "empty_one()") {
		t.Errorf("empty body should be skipped, but empty_one() was emitted:\n%s", lib)
	}
	if !strings.Contains(lib, "good_one()") {
		t.Errorf("good_one() should still be emitted:\n%s", lib)
	}
	if err := runner.ValidateBashSyntax(lib); err != nil {
		t.Fatalf("library with a skipped empty body is not valid bash: %v", err)
	}
}

func TestBuildActionLibrary_IsDeterministic(t *testing.T) {
	// Non-deterministic output would churn the runner Secret on every run and
	// make diffs unreviewable.
	a := []LibraryAction{
		{Slug: "zeta", Script: "return 0"},
		{Slug: "alpha", Script: "return 0"},
		{Slug: "mid", Script: "return 0"},
	}
	b := []LibraryAction{a[1], a[2], a[0]}

	libA, err := buildActionLibrary(a)
	if err != nil {
		t.Fatalf("buildActionLibrary(a): %v", err)
	}
	libB, err := buildActionLibrary(b)
	if err != nil {
		t.Fatalf("buildActionLibrary(b): %v", err)
	}
	if libA != libB {
		t.Errorf("output depends on input order:\n--- a ---\n%s\n--- b ---\n%s", libA, libB)
	}
	if strings.Index(libA, "alpha()") > strings.Index(libA, "zeta()") {
		t.Error("functions should be emitted in slug order")
	}
}

func TestBuildActionLibrary_EmptyInputYieldsEmptyOutput(t *testing.T) {
	// The runner skips materialisation entirely on an empty string, so a
	// header-only file would be a pointless write plus a misleading artefact.
	lib, err := buildActionLibrary(nil)
	if err != nil {
		t.Fatalf("buildActionLibrary(nil): %v", err)
	}
	if lib != "" {
		t.Errorf("expected empty output for no actions, got %q", lib)
	}
}

// --- contract with selfservice-crucible-runner actions.sh ----------------

func actionsShPath(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/jmal1/selfservice-crucible-runner").Output()
	if err != nil {
		t.Fatalf("locate selfservice-crucible-runner module: %v", err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "actions.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("cannot find actions.sh at %s: %v", p, err)
	}
	return p
}

func TestActionsSh_SourcesGeneratedLibraryPath(t *testing.T) {
	// If these two ever disagree, library actions silently stop resolving and
	// every workflow that uses one fails with exit 127 -- the exact defect this
	// feature fixes.
	//
	// The assertion deliberately looks for an executable `source` statement
	// rather than merely for the path appearing somewhere in the file: the
	// explanatory comment block above the guard also names the path, so a
	// contains-check would keep passing after someone deleted the actual source
	// line. That weaker version was written first and verified NOT to catch the
	// bug, which is why this one parses lines.
	body, err := os.ReadFile(actionsShPath(t))
	if err != nil {
		t.Fatalf("read actions.sh: %v", err)
	}

	found := false
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.Contains(trimmed, runner.ActionLibraryPath) {
			continue
		}
		if strings.HasPrefix(trimmed, "source ") || strings.HasPrefix(trimmed, ". ") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("actions.sh has no executable `source %s` statement; library actions would fail with exit 127", runner.ActionLibraryPath)
	}

	if actionLibraryPath != runner.ActionLibraryPath {
		t.Fatalf("engine path %q != runner path %q", actionLibraryPath, runner.ActionLibraryPath)
	}
}

func TestActionsSh_SourceGuardSurvivesSetE(t *testing.T) {
	// actions.sh runs under `set -euo pipefail` and is itself SOURCED by every
	// workflow script. That topology is what makes the guard style matter, and
	// it is subtler than it first looks:
	//
	//   * A bare `[ -f x ] && source x` in the MIDDLE of a script does not trip
	//     set -e, because a failing command in an AND-OR list is exempt unless
	//     it is the final one.
	//   * But as the LAST statement of a sourced file it sets that file's exit
	//     status to 1, so `source actions.sh` returns 1 in the caller -- where
	//     set -e is now active -- and the workflow dies before running a single
	//     action, with no output pointing at the cause.
	//
	// So the test must reproduce the real shape (sourced file, guard last), not
	// just run the line standalone. An `if` block with no `else` returns 0 and
	// is immune.
	requireBash(t)

	const safe = `
set -euo pipefail
tmp=$(mktemp)
cat > "$tmp" <<'INNER'
set -euo pipefail
if [ -f /nonexistent/library.sh ]; then
    source /nonexistent/library.sh
fi
INNER
source "$tmp"
echo SURVIVED
rm -f "$tmp"
`
	if out := runBash(t, safe); !strings.Contains(out, "SURVIVED") {
		t.Fatalf("if-guard did not survive set -e with a missing library: %q", out)
	}

	const unsafe = `
set -euo pipefail
tmp=$(mktemp)
cat > "$tmp" <<'INNER'
set -euo pipefail
[ -f /nonexistent/library.sh ] && source /nonexistent/library.sh
INNER
source "$tmp"
echo SURVIVED
rm -f "$tmp"
`
	if out := runBash(t, unsafe); strings.Contains(out, "SURVIVED") {
		t.Fatal("expected the trailing `&& source` form to abort the caller under set -e; " +
			"if this passes, the rationale documented in actions.sh no longer holds and the guard style should be re-justified")
	}

	// And the real file must use the safe form.
	body, err := os.ReadFile(actionsShPath(t))
	if err != nil {
		t.Fatalf("read actions.sh: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "&&") && strings.Contains(trimmed, "source") {
			t.Errorf("actions.sh uses the set -e-unsafe `&& source` form: %q", trimmed)
		}
	}
}

func TestRunnerLibraryQuery_ExcludesWindowsActions(t *testing.T) {
	// Windows library actions are PowerShell. Because the library is sourced as
	// one bash file, including even one of them makes `source` fail and every
	// action in the run dies -- not just the Windows one. The exclusion is in
	// SQL rather than a language heuristic, so guard the SQL.
	src, err := os.ReadFile("queries.go")
	if err != nil {
		t.Fatalf("read queries.go: %v", err)
	}
	s := string(src)
	idx := strings.Index(s, "func (q *Queries) ListRunnerLibraryActions")
	if idx < 0 {
		t.Fatal("ListRunnerLibraryActions not found in queries.go")
	}
	fn := s[idx:]
	if end := strings.Index(fn[1:], "\nfunc "); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, `supported_platforms @> '["windows"]'`) {
		t.Error("ListRunnerLibraryActions must exclude windows actions via supported_platforms; a PowerShell body breaks sourcing for every action in the run")
	}
	if !strings.Contains(fn, "is_library = true") {
		t.Error("ListRunnerLibraryActions must select only library actions")
	}
}

// --- helpers ---------------------------------------------------------------

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-semantics test")
	}
}

func runBash(t *testing.T, script string) string {
	t.Helper()
	// Fed on stdin, not as a file argument: git-bash on Windows cannot open a
	// native temp path, which would make every shell-semantics assertion here
	// fail for reasons unrelated to what is being tested.
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, _ := cmd.CombinedOutput()
	return string(out)
}
