package scriptvalidator

import (
	"regexp"
	"sort"
	"strings"

	"github.com/jmal1/selfservice-crucible-runner/runnertools"
)

// cmdRe matches a token in command position: the first word of a line, or the
// first word after a shell operator that starts a new command (| && || ; etc.),
// or after `sudo`/`timeout`-style prefixes handled below.
//
// Like ctxcheck, this is deliberately not a full shell parser. The cost of a
// false positive is an instructor dismissing a warning; the cost of a false
// negative is a student staring at a red check that no change to their VM can
// ever turn green. We bias toward reporting.
var cmdRe = regexp.MustCompile(`(?:^|[;&|]|\$\(|` + "`" + `)\s*([A-Za-z_][A-Za-z0-9_.-]*)\b`)

// commandPrefixes are wrappers that take a command as their first non-flag
// argument. `sudo nmap ...` should be reported as `nmap`, not `sudo`.
var commandPrefixes = map[string]struct{}{
	"sudo": {}, "timeout": {}, "env": {}, "nohup": {}, "command": {},
	"exec": {}, "xargs": {}, "time": {},
}

// shellBuiltins and coreutils are always available: they come from bash itself
// or from the base image's essential packages, which are not enumerated in the
// runner tool manifest because they are not removable.
//
// This list only has to be complete enough to avoid noisy false positives. A
// missing entry produces a spurious warning, which is visible and easy to fix;
// it never suppresses a real one.
var alwaysAvailable = map[string]struct{}{
	// bash builtins / keywords
	"if": {}, "then": {}, "else": {}, "elif": {}, "fi": {}, "for": {}, "while": {},
	"until": {}, "do": {}, "done": {}, "case": {}, "esac": {}, "function": {},
	"return": {}, "break": {}, "continue": {}, "in": {}, "select": {}, "time": {},
	"echo": {}, "printf": {}, "read": {}, "test": {}, "eval": {}, "exit": {},
	"export": {}, "local": {}, "declare": {}, "typeset": {}, "readonly": {},
	"set": {}, "unset": {}, "shift": {}, "source": {}, "alias": {}, "trap": {},
	"cd": {}, "pwd": {}, "true": {}, "false": {}, "let": {}, "wait": {}, "jobs": {},
	"kill": {}, "type": {}, "hash": {}, "getopts": {}, "mapfile": {}, "shopt": {},
	// coreutils / essential, present in any Debian-family base
	"cat": {}, "cut": {}, "sed": {}, "awk": {}, "grep": {}, "egrep": {}, "fgrep": {},
	"head": {}, "tail": {}, "sort": {}, "uniq": {}, "wc": {}, "tr": {}, "tee": {},
	"ls": {}, "mkdir": {}, "rm": {}, "cp": {}, "mv": {}, "ln": {}, "touch": {},
	"chmod": {}, "chown": {}, "date": {}, "sleep": {}, "seq": {}, "basename": {},
	"dirname": {}, "realpath": {}, "readlink": {}, "find": {}, "xargs": {},
	"tar": {}, "gzip": {}, "gunzip": {}, "zcat": {}, "df": {}, "du": {}, "id": {},
	"whoami": {}, "hostname": {}, "uname": {}, "which": {}, "env": {}, "ps": {},
	"tempfile": {}, "mktemp": {}, "expr": {}, "sudo": {}, "nohup": {}, "exec": {},
	"md5sum": {}, "sha256sum": {}, "base64": {}, "od": {}, "xxd": {}, "strings": {},
}

// crucibleHelpers are functions the runner's action library defines. They are
// not binaries, so `command -v` would not find them, but they are always
// callable from a workflow script because actions.sh is sourced first.
//
// Keep in sync with deploy/runner/actions.sh. A missing entry here produces a
// false "not installed" warning on a perfectly valid script, which is exactly
// the kind of noise that trains instructors to ignore the linter.
var crucibleHelpers = map[string]struct{}{
	"run_action": {}, "ctx_set": {}, "ctx_get": {}, "ctx_all": {},
	"crucible_log": {}, "student_msg": {}, "fail_with": {},
	"crucible_ssh": {}, "crucible_ssh_sudo": {},
}

// unknownCommandFindings reports commands a workflow script invokes that are
// not provided by the runner image, by bash, or by the action library.
//
// WHY THIS EXISTS
// ---------------
// A missing command exits 127. The runner surfaces that at runtime as an
// "error" status with an explanatory message (see the exit-127 branch in
// actions.sh), but by then a student has already sat through a graded
// assessment whose result was decided by an authoring mistake. This check moves
// the discovery to the moment the instructor writes the action.
//
// Emitted as warnings, never errors: the manifest cannot know about tools
// installed at runtime by the script itself (`apt-get install -y foo && foo`),
// nor about commands built dynamically. Blocking a save on a heuristic would be
// worse than the problem it solves.
func unknownCommandFindings(userScript string) []Finding {
	// Functions the script defines itself are callable regardless of the image.
	localFuncs := definedFunctions(userScript)

	type ref struct {
		name string
		line int
		col  int
	}
	seen := make(map[string]struct{})
	var refs []ref

	for lineIdx, rawLine := range strings.Split(userScript, "\n") {
		line := stripQuotedAndComments(rawLine)
		matches := cmdRe.FindAllStringSubmatchIndex(line, -1)
		for _, m := range matches {
			name := line[m[2]:m[3]]

			// Skip variable assignments (FOO=bar) and appends (FOO+=bar,
			// arr+=(elem)).
			//
			// The append form matters far more than it looks. Conditional array
			// building is the idiomatic way to assemble curl/ssh arguments:
			//
			//	[[ -n "$cookies" ]] && curl_args+=(-b "/tmp/$cookies")
			//
			// and because that sits after `&&`, the array name lands in what the
			// regex sees as command position. Without this branch every action
			// written that way reports its own array as an uninstalled command —
			// three of Crucible's own shipped library actions did — which is
			// precisely the noise that trains instructors to ignore the linter.
			rest := line[m[3]:]
			if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "+=") {
				continue
			}
			// Skip wrapper prefixes; the wrapped command is matched separately
			// only when it follows an operator, so resolve it here instead.
			if _, isPrefix := commandPrefixes[name]; isPrefix {
				continue
			}
			if isKnownCommand(name, localFuncs) {
				continue
			}

			col := m[2] + 1 // 1-based, pointing at the command itself
			key := name + ":" + intToStr(lineIdx+1) + ":" + intToStr(col)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			refs = append(refs, ref{name: name, line: lineIdx + 1, col: col})
		}
	}

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
			EndColumn: r.col + len(r.name),
			Code:      "CRU0002",
			Severity:  SeverityWarning,
			Message: "'" + r.name + "' is not installed in the assessment runner. " +
				"This action will exit 127 (command not found) when it runs, and the " +
				"student will see a failed check they cannot fix. Use one of the " +
				"available tools, install it in the script first, or ask an " +
				"administrator to add it to the runner image.",
		})
	}
	return out
}

// MissingCommands returns the sorted, de-duplicated set of commands a script
// invokes that will not resolve at runtime in the assessment runner.
//
// This is the same analysis that powers the CRU0002 authoring-time warning,
// exposed so callers can enforce it where a warning is not enough.
//
// The authoring-time path is deliberately advisory: it lints scripts an
// instructor is still writing, where the manifest genuinely cannot know about a
// tool the script installs itself, and blocking a save on a heuristic would be
// worse than the problem it solves. That trade does NOT hold for Crucible's own
// shipped library actions. Those are ours, they are sourced into every single
// run as one file, and a missing tool in one of them is a graded assessment that
// no student can pass. For that corpus the finding is a build failure, not a hint.
func MissingCommands(script string) []string {
	var out []string
	seen := make(map[string]struct{})
	for _, f := range unknownCommandFindings(script) {
		// Finding.Message embeds the name; recover it from the column span so
		// there is one extraction path rather than two that can disagree.
		lines := strings.Split(script, "\n")
		if f.Line-1 < 0 || f.Line-1 >= len(lines) {
			continue
		}
		line := lines[f.Line-1]
		if f.Column-1 < 0 || f.EndColumn-1 > len(line) || f.Column > f.EndColumn {
			continue
		}
		name := line[f.Column-1 : f.EndColumn-1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// isKnownCommand reports whether name will resolve at runtime.
//
// Library actions are deliberately NOT consulted here. They are dispatched as
// `run_action "Name" http_get ...`, so the action name appears as an *argument*
// to run_action rather than in command position, and cmdRe never matches it.
// Threading a LibraryActionNames option through for a case that cannot occur
// would add an option nothing sets — the dead-wiring pattern this codebase has
// already been bitten by seven times.
func isKnownCommand(name string, localFuncs map[string]struct{}) bool {
	if _, ok := alwaysAvailable[name]; ok {
		return true
	}
	if _, ok := crucibleHelpers[name]; ok {
		return true
	}
	if _, ok := localFuncs[name]; ok {
		return true
	}
	return runnertools.HasCommand(name)
}

// funcDefRe matches `foo()` and `function foo` definitions.
var funcDefRe = regexp.MustCompile(`(?m)^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)`)

func definedFunctions(script string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, m := range funcDefRe.FindAllStringSubmatch(script, -1) {
		out[m[1]] = struct{}{}
	}
	return out
}

// stripQuotedAndComments blanks out quoted spans and trailing comments so
// words inside them are not mistaken for commands.
//
// Single quotes suppress all expansion, so nothing inside them can ever be a
// command and they are always blanked.
//
// A double-quoted span is blanked only when it contains no `$(` and no
// backtick, because those are real command substitutions and this check exists
// to see them. Keeping the rest of the span visible was producing a specific
// false positive that mattered: a student-facing message such as
// "(Apache: ServerTokens Prod; nginx: server_tokens off)" contains a `;`, and
// cmdRe treats the next word as a new command — so a correct action was
// reported as depending on nginx being installed in the runner image.
func stripQuotedAndComments(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				// Unterminated: the rest of the line is quoted.
				b.WriteString(strings.Repeat(" ", len(line)-i))
				return b.String()
			}
			b.WriteString(strings.Repeat(" ", end+2))
			i += end + 1
		case c == '"':
			end := findUnescaped(line[i+1:], '"')
			if end < 0 {
				// Unterminated. A multi-line double-quoted string is legal
				// bash, so blanking the remainder is the conservative choice
				// only if it holds no substitution.
				rest := line[i+1:]
				if containsSubstitution(rest) {
					b.WriteString(rest)
					return b.String()
				}
				b.WriteString(strings.Repeat(" ", len(line)-i))
				return b.String()
			}
			span := line[i+1 : i+1+end]
			if containsSubstitution(span) {
				b.WriteByte(' ')
				b.WriteString(span)
				b.WriteByte(' ')
			} else {
				b.WriteString(strings.Repeat(" ", end+2))
			}
			i += end + 1
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			// Rest of the line is a comment.
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func containsSubstitution(s string) bool {
	return strings.Contains(s, "$(") || strings.Contains(s, "`")
}

// findUnescaped returns the index of the first unescaped occurrence of want.
func findUnescaped(s string, want byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == want {
			return i
		}
	}
	return -1
}
