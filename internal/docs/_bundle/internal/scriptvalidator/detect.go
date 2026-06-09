package scriptvalidator

import (
	"regexp"
	"strings"
)

// detectShellLanguage returns the best guess at what shell language a script
// was written in, by sniffing the first non-blank lines for obvious dialect
// markers. We only need to distinguish bash/sh from PowerShell — anything we
// don't recognise as PowerShell, we treat as bash so existing behaviour is
// preserved.
//
// We sniff because the admin form has `action_type` but not a `language`
// column, and several of our prod actions ship PowerShell under
// action_type="command". Running bash shellcheck against PowerShell produces
// noise that's actively unhelpful, so we skip cleanly instead.
//
// "powershell" -> looks like Windows PowerShell / pwsh
// "bash"       -> looks like bash/sh (or we can't tell — default)
func detectShellLanguage(script string) string {
	// Shebang trumps everything.
	if strings.HasPrefix(script, "#!") {
		first, _, _ := strings.Cut(script, "\n")
		lower := strings.ToLower(first)
		if strings.Contains(lower, "pwsh") || strings.Contains(lower, "powershell") {
			return "powershell"
		}
		if strings.Contains(lower, "bash") || strings.Contains(lower, "/sh") || strings.Contains(lower, "/zsh") {
			return "bash"
		}
	}
	// Heuristic: PowerShell uses $Var = "…", Get-/Set-/Test- verbs, param(),
	// and { } blocks with parens. Bash uses local x=…, $1/$@, [[ … ]].
	// We need a low false-positive rate so we only flag if multiple PS-shaped
	// tokens appear together within the first few lines.
	preview := firstNLines(script, 25)
	psHits := 0
	if powershellAssignment.MatchString(preview) {
		psHits++
	}
	if powershellCmdlet.MatchString(preview) {
		psHits++
	}
	if powershellParam.MatchString(preview) {
		psHits++
	}
	if powershellSwitch.MatchString(preview) {
		psHits++
	}
	// Two or more PowerShell-flavoured constructs in the first 25 lines is
	// enough to call it. One alone (e.g. a `$Var = "x"` assignment) is too
	// common in bash heredocs / quoted blocks to act on.
	if psHits >= 2 {
		return "powershell"
	}
	return "bash"
}

var (
	// $PascalCase assignment with an `=` — typical PowerShell variable form.
	// Bash doesn't put spaces around `=` in assignments, so the spaces here
	// help discriminate.
	powershellAssignment = regexp.MustCompile(`(?m)^\s*\$[A-Za-z_][A-Za-z0-9_]*\s*=\s*`)
	// Get-Foo / Set-Bar / Test-Baz — PowerShell cmdlet naming convention.
	powershellCmdlet = regexp.MustCompile(`(?m)^\s*(Get|Set|Test|New|Remove|Add|Start|Stop|Restart|Invoke|Write|Read|Format|Out|Select|Where|Foreach|ConvertTo|ConvertFrom|Import|Export|Enable|Disable|Find|Resolve|Show|Hide|Lock|Unlock|Send|Receive|Move|Copy|Rename|Clear|Update)-[A-Z][A-Za-z]+`)
	// param( … ) block at the top.
	powershellParam = regexp.MustCompile(`(?m)^\s*param\s*\(`)
	// `switch ($x) { … }` — switch-on-parentheses is PS syntax.
	powershellSwitch = regexp.MustCompile(`(?m)^\s*switch\s*\(`)
)

func firstNLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
