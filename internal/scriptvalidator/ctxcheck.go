package scriptvalidator

import (
	"regexp"
	"sort"
	"strings"
)

// ctxRefRe matches `$CTX_FOO` and `${CTX_FOO...}` (default expansions,
// length, substring, etc.). The capture group is the name *after* the
// `CTX_` prefix so it can be compared against the declared
// InputContextNames + OutputContextNames lists (which are stored without
// the `CTX_` prefix everywhere else in the validator).
//
// We deliberately don't try to be a full shell parser here — false positives
// inside heredocs or single-quoted strings are acceptable because the worst
// outcome is the instructor seeing a "CTX_X is not declared" warning on a
// string literal they don't intend to expand, which is easy to dismiss. The
// alternative — letting silent typos hit production unnoticed — is worse.
var ctxRefRe = regexp.MustCompile(`\$(?:\{[#!]?)?CTX_([A-Za-z_][A-Za-z0-9_]*)`)

// undeclaredCTXFindings parses the user script (not the wrapped version) for
// references to $CTX_<NAME> where <NAME> is not in the declared
// InputContextNames or OutputContextNames lists. Each such reference is
// surfaced as a CRU0001 warning so the editor highlights it. This compensates
// for shellcheck SC2154's deliberate blind spot around ALL_CAPS variables —
// shellcheck assumes ALL_CAPS names come from the environment and refuses to
// fire SC2154 on them, which means typo'd $CTX_TYPO refs would otherwise slip
// through both layers of validation.
//
// Findings are emitted as warnings (not errors) so instructors are alerted
// but not blocked from saving — there are edge cases (dynamic context
// injection, runtime-set vars) where the static check is wrong and the
// instructor knows better.
func undeclaredCTXFindings(userScript string, opts Options) []Finding {
	declared := make(map[string]struct{})
	for _, n := range sanitizeNames(opts.InputContextNames) {
		declared[n] = struct{}{}
	}
	for _, n := range sanitizeNames(opts.OutputContextNames) {
		declared[n] = struct{}{}
	}

	// We also accept any CTX_* the runner itself sets on every action
	// (e.g. CTX_TARGET_IP-style aliases the runner exposes), if any get
	// added later. Keep this list explicit so it doesn't drift silently.
	for _, n := range runnerProvidedCTXVars {
		declared[n] = struct{}{}
	}

	type ref struct {
		name string
		line int
		col  int
	}
	seen := make(map[string]struct{}) // dedupe per (name,line,col)
	var refs []ref

	for lineIdx, line := range strings.Split(userScript, "\n") {
		matches := ctxRefRe.FindAllStringSubmatchIndex(line, -1)
		for _, m := range matches {
			// m[0]..m[1] is the full match; m[2]..m[3] is the name capture.
			name := strings.ToUpper(line[m[2]:m[3]])
			if _, ok := declared[name]; ok {
				continue
			}
			col := m[0] + 1 // shellcheck/Monaco coordinates are 1-based
			key := name + ":" + intToStr(lineIdx+1) + ":" + intToStr(col)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, ref{name: name, line: lineIdx + 1, col: col})
		}
	}

	// Sort for deterministic output (matters for tests).
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].line != refs[j].line {
			return refs[i].line < refs[j].line
		}
		return refs[i].col < refs[j].col
	})

	out := make([]Finding, 0, len(refs))
	for _, r := range refs {
		out = append(out, Finding{
			Line:      r.line,
			EndLine:   r.line,
			Column:    r.col,
			EndColumn: r.col + len("CTX_") + len(r.name) + 1, // include leading $
			Code:      "CRU0001",
			Severity:  SeverityWarning,
			Message: "CTX_" + r.name + " is referenced but not declared in this " +
				"action's Input Context or Output Context. Either add it on the " +
				"action form or fix the typo. (shellcheck's SC2154 deliberately " +
				"ignores ALL_CAPS variables, so this check supplements it.)",
		})
	}
	return out
}

// runnerProvidedCTXVars lists CTX_* names the runner sets unconditionally
// for every action invocation. Keep this list empty unless the runner
// actually does this — false negatives here mean students see no warning
// for a real typo. As of 2026-06-07 the runner does NOT auto-populate any
// CTX_* names; every CTX_* must come from the declared input_context.
var runnerProvidedCTXVars = []string{}

// intToStr avoids pulling in strconv for the one place we need it; keeps
// this file self-contained.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
