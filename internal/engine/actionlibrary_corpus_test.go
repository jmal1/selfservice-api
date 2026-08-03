package engine

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/runner"
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
