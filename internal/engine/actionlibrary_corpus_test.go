package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/runner"
	"github.com/jmal1/selfservice-api/internal/runnertools"
	"github.com/jmal1/selfservice-api/internal/scriptvalidator"
)

// TestBuildActionLibrary_ProductionCorpusParses runs the generator over a
// snapshot of the real library actions from production and asserts the result
// is valid bash that defines every expected function.
//
// Synthetic fixtures cannot prove this. The real bodies use `local`, `return`,
// `[[ ]]`, `case`, heredocs, `while` argument parsers and embedded quoting, and
// the whole library is sourced as ONE file — so a single body that does not
// parse takes out every action in the run, not just its own. This is the test
// that would have caught the original defect class before it reached a student.
//
// Refresh the snapshot with:
//
//	select json_agg(json_build_object('slug',slug,'name',name,
//	    'description',coalesce(description,''),'script',script) order by slug)
//	from actions
//	where is_library=true and slug is not null and btrim(script) <> ''
//	  and not (supported_platforms @> '["windows"]'::jsonb);
func TestBuildActionLibrary_ProductionCorpusParses(t *testing.T) {
	requireBash(t)

	raw, err := os.ReadFile("testdata/library_actions.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []LibraryAction
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(corpus) < 10 {
		t.Fatalf("corpus looks truncated: only %d actions", len(corpus))
	}

	lib, err := buildActionLibrary(corpus)
	if err != nil {
		t.Fatalf("buildActionLibrary over production corpus: %v", err)
	}
	if err := runner.ValidateBashSyntax(lib); err != nil {
		t.Fatalf("production action library is not valid bash: %v\n---\n%s", err, lib)
	}

	// Every action must actually be callable, not merely present as text.
	var checks strings.Builder
	checks.WriteString(lib)
	checks.WriteString("\nmissing=0\n")
	for _, a := range corpus {
		fn, err := libraryFuncName(a.Slug)
		if err != nil {
			t.Fatalf("slug %q from production does not yield a legal function name: %v", a.Slug, err)
		}
		checks.WriteString("declare -F " + fn + " >/dev/null || { echo \"MISSING " + fn + "\"; missing=1; }\n")
	}
	checks.WriteString("[ \"$missing\" = 0 ] && echo ALL_RESOLVED\n")

	out := runBash(t, checks.String())
	if !strings.Contains(out, "ALL_RESOLVED") {
		t.Fatalf("not every library action is callable after sourcing:\n%s", out)
	}
}

// TestBuildActionLibrary_CorpusToolsAreInstalled asserts every command invoked
// by a shipped library action actually exists in the runner image.
//
// This is the gap that the authoring-time validator structurally cannot close.
// `POST /scripts/validate` only sees a script an instructor deliberately submits
// to it, and it only ever emits CRU0002 as a *warning* — so an action that was
// saved before the check existed, or saved by someone who dismissed the warning,
// ships with a missing dependency and nothing notices. There is no sweep over
// what is already in the database.
//
// The consequence is specific and bad: library actions are concatenated into one
// library.sh and sourced into every run, and a missing binary surfaces as a bare
// exit 127 during a graded assessment. The student sees a failed check that no
// change to their VM can turn green, and the instructor sees a result that looks
// like a student mistake. That is a grading-correctness bug, not a lint.
//
// So for OUR OWN shipped actions the finding is promoted from warning to build
// failure. Instructor-authored scripts keep the advisory treatment, because for
// those the manifest legitimately cannot know about a tool the script installs
// itself (`apt-get install -y foo && foo`).
//
// Refresh testdata/library_actions.json with the SQL in
// TestBuildActionLibrary_ProductionCorpusParses above.
//
// Coverage as verified against production on 2026-08-03: the snapshot matched
// the live database exactly (18 actions, identical slugs), and there were ZERO
// non-library actions carrying a script — so this guard covers every action
// script the runner can execute, not merely a sample.
//
// The residual risk is snapshot drift: a library action added directly in the
// database without refreshing this file is not checked here. That is a real gap,
// but a bounded one — library actions are Crucible's own shipped content, not
// instructor input — and it is preferable to state it plainly than to build
// speculative machinery around it. If library actions ever start being authored
// through the API at any volume, this needs to become a runtime check instead.
func TestBuildActionLibrary_CorpusToolsAreInstalled(t *testing.T) {
	raw, err := os.ReadFile("testdata/library_actions.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []LibraryAction
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if len(corpus) < 10 {
		t.Fatalf("corpus looks truncated: only %d actions", len(corpus))
	}

	var scanned int
	for _, a := range corpus {
		if strings.TrimSpace(a.Script) == "" {
			continue
		}
		scanned++
		missing := scriptvalidator.MissingCommands(a.Script)
		if len(missing) == 0 {
			continue
		}
		t.Errorf("library action %q (%s) invokes %v, which the runner image does not provide.\n"+
			"Every one of these exits 127 mid-assessment, and the student sees a failed check "+
			"they cannot fix.\n"+
			"Fix by adding the providing package to internal/runnertools/tools.txt (and rebuilding "+
			"the runner image), or by rewriting the action to use a tool that is installed.\n"+
			"Installed commands: %v",
			a.Slug, a.Name, missing, runnertools.Commands())
	}

	// A corpus that silently stopped being scanned would make this test pass
	// forever while proving nothing — the same hollow-guard failure mode that
	// has already bitten this repo.
	if scanned == 0 {
		t.Fatal("scanned 0 action scripts; the corpus parse is broken, so this guard " +
			"would pass no matter what the actions call")
	}
	t.Logf("verified runner tool dependencies for %d library actions", scanned)
}

// TestBuildActionLibrary_CorpusHasNoPowerShell is a canary on the SQL filter.
// PowerShell bodies are excluded by supported_platforms in
// ListRunnerLibraryActions; if one ever slips into the corpus, sourcing fails
// and every action in the run dies.
func TestBuildActionLibrary_CorpusHasNoPowerShell(t *testing.T) {
	raw, err := os.ReadFile("testdata/library_actions.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []LibraryAction
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	for _, a := range corpus {
		if strings.HasPrefix(a.Slug, "win-") {
			t.Errorf("windows action %q is in the bash library corpus; supported_platforms filter is not working", a.Slug)
		}
		for _, marker := range []string{"Get-", "Set-", "Test-Path", "Out-String", "$PSVersionTable"} {
			if strings.Contains(a.Script, marker) {
				t.Errorf("action %q contains PowerShell marker %q; it would break sourcing for every action", a.Slug, marker)
			}
		}
	}
}
