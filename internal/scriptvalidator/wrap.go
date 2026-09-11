package scriptvalidator

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Options influences how the user script is wrapped before being handed to
// shellcheck. The wrapper exists to express the runtime context Crucible
// action and workflow scripts actually execute in — sourced under
// /opt/crucible/lib/actions.sh, run as the body of a function called by a
// wrapper, with `LAST_ERROR` / `LAST_STUDENT_MSG` consumed externally — so
// the linter doesn't flag idiomatic action code as broken.
type Options struct {
	// InputContextNames are the basenames of context variables declared by
	// the instructor on this action (the right-hand column in the Action
	// editor's "Input Context" table). Each becomes a CTX_<NAME> shell
	// variable that the runner injects; declaring them in the preamble
	// suppresses SC2154 ("referenced but not assigned") for these specific
	// vars without disabling the rule globally.
	InputContextNames []string

	// OutputContextNames are the basenames of context variables this action
	// is expected to set via ctx_set. Declaring them keeps SC2034 honest:
	// other unused locals still get flagged.
	OutputContextNames []string
}

// preambleHeader / preambleAPI / preambleVars are split out so the test
// suite can assert on their exact contents and so future additions don't
// silently shift line offsets.
//
// IMPORTANT: edits here change the LineOffset returned by buildPreamble. The
// validator depends on that count to map shellcheck findings back to the
// user's coordinate space. The TestPreamble_LineCount test pins this number
// down so accidental regressions get caught.
const preambleHeader = `#!/usr/bin/env bash
# shellcheck shell=bash
# === Crucible validator preamble (not part of the script that runs) ===
`

const preambleAPI = `# Runtime API provided by /opt/crucible/lib/actions.sh
ctx_get()    { echo ""; }
ctx_set()    { :; }
ctx_has()    { return 0; }
run_action() { :; }
`

const preambleStandardEnv = `# Standard environment exposed by the runner
: "${CRUCIBLE_TARGET_IP:=}" "${CRUCIBLE_TARGET_USER:=}" "${CRUCIBLE_TARGET_PORT:=}"
: "${CRUCIBLE_RUN_ID:=}" "${CRUCIBLE_ACTION_LABEL:=}" "${CRUCIBLE_SOCKET:=}"
: "${CRUCIBLE_WORKDIR:=}" "${ACTION_TIMEOUT:=}"
`

const preambleOutputs = `# Convention-output variables read by the wrapper after the body returns
LAST_ERROR=""
LAST_STUDENT_MSG=""
LAST_STUDENT_MESSAGE=""
`

const preambleFnOpen = `_crucible_action_body() {
`

const preambleFnClose = `}

# Reference outputs + arg passthrough so SC2034 sees them used and SC2120
# doesn't fire on the function call below.
_crucible_action_body "$@"
: "$LAST_ERROR" "$LAST_STUDENT_MSG" "$LAST_STUDENT_MESSAGE"
`

// validCTXName matches the same identifier shape postgres / the runner accept
// for context variable names so a malicious or malformed input_context list
// can't inject arbitrary shell code into the preamble.
var validCTXName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// buildPreamble assembles the synthetic preamble that comes before the user
// script in the wrapped output. Returns the preamble text and the number of
// lines it occupies (i.e. how many lines to subtract from each finding's
// .Line / .EndLine to map back to the user's coordinate space).
//
// The preamble's structure is deterministic so the line offset is stable and
// testable: header + api + standard-env + per-input-context declarations +
// outputs + function-open.
func buildPreamble(opts Options) (string, int) {
	var b strings.Builder
	b.WriteString(preambleHeader)
	b.WriteString(preambleAPI)
	b.WriteString(preambleStandardEnv)

	if names := sanitizeNames(opts.InputContextNames); len(names) > 0 {
		b.WriteString("# Input context declared on this action\n")
		for _, n := range names {
			fmt.Fprintf(&b, ": \"${CTX_%s:=}\"\n", n)
		}
	}

	if names := sanitizeNames(opts.OutputContextNames); len(names) > 0 {
		b.WriteString("# Output context this action is expected to set via ctx_set\n")
		for _, n := range names {
			fmt.Fprintf(&b, ": \"${CTX_%s:=}\"\n", n)
		}
	}

	b.WriteString(preambleOutputs)
	b.WriteString(preambleFnOpen)
	return b.String(), strings.Count(b.String(), "\n")
}

// sanitizeNames upper-cases, dedupes, sorts, and filters input names to a
// safe identifier shape. Anything that doesn't match validCTXName is
// dropped silently — the validator's job isn't to police input_context
// definitions, just to avoid shell injection through the preamble.
func sanitizeNames(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		n := strings.ToUpper(strings.TrimSpace(raw))
		if !validCTXName.MatchString(n) {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// wrappedScript holds everything runShellcheck needs to map findings back to
// user coordinates after shellcheck runs against the wrapped text.
type wrappedScript struct {
	text       string // the full wrapped script handed to shellcheck
	lineOffset int    // subtract this from each finding.Line / EndLine
	userLines  int    // findings with mapped line > userLines were synthetic
}

// wrapForValidation builds the full wrapped script: preamble + user body
// (indented inside the synthetic function) + epilogue. Returns a wrappedScript
// with the offsets needed to translate findings back into user coordinates.
func wrapForValidation(userScript string, opts Options) wrappedScript {
	preamble, preambleLines := buildPreamble(opts)

	// Indent the user body by a tab so it visually sits inside the function.
	// shellcheck doesn't care about indentation, but a tab keeps column
	// numbers in error messages reasonable when humans look at the wrapped
	// text. Lines that were originally empty stay empty so we don't shift
	// columns visually.
	bodyLines := strings.Split(userScript, "\n")
	for i, line := range bodyLines {
		if line == "" {
			continue
		}
		bodyLines[i] = "\t" + line
	}
	body := strings.Join(bodyLines, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}

	full := preamble + body + preambleFnClose
	return wrappedScript{
		text:       full,
		lineOffset: preambleLines,
		userLines:  strings.Count(userScript, "\n") + 1,
	}
}

// remapFindings translates findings from wrapped-script coordinates back to
// user-script coordinates and drops any finding that points at the synthetic
// preamble or epilogue (those aren't user-actionable).
//
// findings that straddle the user/preamble boundary (e.g. an unclosed quote
// that shellcheck reports at the closing `}` line) get dropped as well; the
// alternative would be misleading markers pointing at content the user can't
// see.
func remapFindings(in []Finding, w wrappedScript) []Finding {
	if len(in) == 0 {
		return in
	}
	out := in[:0]
	for _, f := range in {
		f.Line -= w.lineOffset
		f.EndLine -= w.lineOffset
		// Drop anything outside the user's lines. Note `f.Line >= 1` filters
		// preamble findings; `f.Line <= w.userLines` filters epilogue
		// findings. Column stays as shellcheck reported (the tab indent we
		// added shifts columns by 1, which we compensate for below).
		if f.Line < 1 || f.Line > w.userLines {
			continue
		}
		// We indented every non-empty body line by one tab. Subtract that
		// shift so columns line up with what the user sees in Monaco.
		if f.Column > 1 {
			f.Column--
		}
		if f.EndColumn > 1 {
			f.EndColumn--
		}
		out = append(out, f)
	}
	return out
}
