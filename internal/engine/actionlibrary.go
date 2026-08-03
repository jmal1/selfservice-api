package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// LibraryAction is one reusable action body from the `actions` table with
// is_library = true. Only the fields needed to render a shell function are
// carried, so the engine does not depend on the full admin-facing model.
type LibraryAction struct {
	Slug        string
	Name        string
	Description string
	Script      string
}

// actionLibraryPath is where the runner materialises the generated library and
// where deploy/runner/actions.sh sources it from. The two MUST agree; the
// contract is asserted by TestActionLibraryPath_MatchesActionsSh.
const actionLibraryPath = "/opt/crucible/lib/library.sh"

// actionLibraryHeader marks the file as generated. It is also the sentinel the
// runner uses to confirm it wrote what it thinks it wrote.
const actionLibraryHeader = "# Crucible action library — GENERATED PER RUN, DO NOT EDIT"

var libraryFuncNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// libraryFuncName converts a library action slug into the shell function name a
// workflow author calls.
//
// Slugs are kebab-case (`http-get`) but shell function names are conventionally
// snake_case, and every workflow script and every AI-authoring example in the
// wiki already writes `run_action "..." http_get ...`. Hyphens are technically
// legal in bash function names but cannot be called without quoting in many
// contexts and make shellcheck complain, so the underscore form is the contract.
func libraryFuncName(slug string) (string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(slug), "-", "_")
	if !libraryFuncNamePattern.MatchString(name) {
		return "", fmt.Errorf("library action slug %q does not yield a legal shell function name (got %q)", slug, name)
	}
	return name, nil
}

// buildActionLibrary renders library actions into a sourceable bash file of
// function definitions.
//
// Why this exists at all: library action bodies live in the database and were
// never delivered to the runner. runner.WorkflowDef carried only the workflow's
// own script, and deploy/runner/actions.sh defines just run_action/ctx_set/
// ctx_get. So a workflow calling `run_action "HTTP Responds" http_get ...` --
// which is exactly what the seeded workflows and the instructor docs tell
// authors to write -- died with exit 127, "No such file or directory". Every
// one of the 24 library actions was unreachable at runtime; the failure was
// invisible because no runner result had ever reached the database.
//
// The bodies are written as bash function *bodies*: they use `local`, `return 1`
// and set LAST_ERROR/LAST_STUDENT_MSG, which are only meaningful inside a
// function. Wrapping each in `name() { ... }` is therefore the intended shape,
// not a workaround.
//
// Windows actions are excluded by the caller via supported_platforms rather than
// by sniffing the script, because their bodies are PowerShell and would be a
// bash syntax error -- one bad body makes `source` fail and takes out every
// action in the run, not just its own.
func buildActionLibrary(actions []LibraryAction) (string, error) {
	// Deterministic output: the same inputs must produce byte-identical files so
	// the Secret does not churn and diffs stay reviewable.
	sorted := make([]LibraryAction, len(actions))
	copy(sorted, actions)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })

	var b strings.Builder
	b.WriteString(actionLibraryHeader + "\n")
	b.WriteString("#\n")
	b.WriteString("# Sourced by /opt/crucible/lib/actions.sh. Each function is the body of a\n")
	b.WriteString("# library action from the Crucible action library, addressed by its slug with\n")
	b.WriteString("# hyphens replaced by underscores.\n\n")

	seen := make(map[string]string, len(sorted))
	count := 0
	for _, a := range sorted {
		if strings.TrimSpace(a.Script) == "" {
			// An empty body would render as `name() { }`, which is a bash syntax
			// error and would break sourcing for every other action too.
			continue
		}
		fn, err := libraryFuncName(a.Slug)
		if err != nil {
			return "", err
		}
		if prev, dup := seen[fn]; dup {
			return "", fmt.Errorf("library actions %q and %q both map to shell function %q", prev, a.Slug, fn)
		}
		seen[fn] = a.Slug

		if a.Name != "" {
			b.WriteString(fmt.Sprintf("# %s\n", a.Name))
		}
		if d := strings.TrimSpace(a.Description); d != "" {
			b.WriteString(fmt.Sprintf("# %s\n", strings.ReplaceAll(d, "\n", "\n# ")))
		}
		b.WriteString(fn + "() {\n")
		for _, line := range strings.Split(strings.TrimRight(a.Script, "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("    " + line + "\n")
		}
		b.WriteString("}\n\n")
		count++
	}

	if count == 0 {
		// Nothing to inject. Return empty rather than a header-only file so the
		// runner can skip materialising it entirely.
		return "", nil
	}
	return b.String(), nil
}
