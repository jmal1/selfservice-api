// Package scriptvalidator runs language-specific linters (shellcheck for bash)
// against user-submitted scripts and returns normalized findings.
//
// Designed for the admin workflow/action editor: results are rendered as
// inline markers in the Monaco editor and gate the "Save" button.
package scriptvalidator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Severity is the normalized severity level of a finding.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
	SeverityStyle   Severity = "style"
)

// Finding is one diagnostic from a linter, expressed in a UI-renderable form
// (1-indexed line/column, exactly like Monaco markers).
type Finding struct {
	Line      int      `json:"line"`
	Column    int      `json:"column"`
	EndLine   int      `json:"end_line"`
	EndColumn int      `json:"end_column"`
	Severity  Severity `json:"severity"`
	Code      string   `json:"code"`    // e.g. "SC2086"
	Message   string   `json:"message"`
}

// Result is what we return to the editor.
type Result struct {
	Language     string    `json:"language"`
	Findings     []Finding `json:"findings"`
	HasErrors    bool      `json:"has_errors"`
	HasWarnings  bool      `json:"has_warnings"`
	LinterStderr string    `json:"linter_stderr,omitempty"`
	DurationMs   int64     `json:"duration_ms"`
}

// Validator runs linters. Construct with NewValidator; in tests, swap out the
// commandRunner to avoid needing shellcheck installed.
type Validator struct {
	run         commandRunner
	maxScriptKB int
	timeout     time.Duration
}

// commandRunner abstracts exec.Cmd for testability.
type commandRunner func(ctx context.Context, name string, args []string, stdin string) (stdout, stderr []byte, exitCode int, err error)

// NewValidator returns a Validator that runs the real shellcheck binary.
func NewValidator() *Validator {
	return &Validator{
		run:         execRun,
		maxScriptKB: 64,
		// 3s is comfortably above worst-case shellcheck runtime for the 64KB
		// script cap on the API pod (observed ~150ms p99). Tight enough to
		// neutralize any pathological-input DoS attempt.
		timeout: 3 * time.Second,
	}
}

// WithRunner overrides the command runner (for tests). Chainable.
func (v *Validator) WithRunner(r commandRunner) *Validator {
	v.run = r
	return v
}

// WithMaxScriptKB overrides the script size limit (for tests).
func (v *Validator) WithMaxScriptKB(kb int) *Validator {
	v.maxScriptKB = kb
	return v
}

// WithTimeout overrides the per-invocation wall-clock timeout (for tests).
func (v *Validator) WithTimeout(d time.Duration) *Validator {
	v.timeout = d
	return v
}

// ErrUnsupportedLanguage is returned when the caller asks for a language we
// don't have a linter for. The HTTP handler maps this to 400.
var ErrUnsupportedLanguage = errors.New("unsupported language")

// ErrScriptTooLarge is returned when the script exceeds the configured size cap.
// The HTTP handler maps this to 413.
var ErrScriptTooLarge = errors.New("script too large")

// Validate runs the linter for the given language against script and returns
// a normalized Result. Currently supports "bash" only.
//
// Carriage returns are stripped before linting so scripts that were authored
// or stored with CRLF line endings don't produce SC1017 noise on every line.
//
// The script is wrapped in a synthetic preamble + function shell that
// expresses the runtime context Crucible action / workflow scripts execute in
// (sourced actions.sh, called as the body of a function, standard runner env
// vars, conventional LAST_* outputs). This lets shellcheck flag real bugs
// without polluting the editor with false positives for idiomatic patterns
// like `local x=$(ctx_get path)` at top level.
//
// Pass Options.InputContextNames so CTX_* vars the instructor declared on
// this action are pre-declared in the preamble. SC2154 still fires for CTX_
// names the instructor forgot to declare — that's a real bug, surfaced
// deliberately.
func (v *Validator) Validate(ctx context.Context, language, script string, opts ...Options) (*Result, error) {
	if maxBytes := v.maxScriptKB * 1024; len(script) > maxBytes {
		return nil, fmt.Errorf("%w: script is %d bytes, max %d", ErrScriptTooLarge, len(script), maxBytes)
	}

	// Normalize CRLF/CR → LF. shellcheck flags every \r as SC1017 ("Literal
	// carriage return"), which floods the editor with noise even when the
	// script is otherwise clean. Scripts run on linux runners anyway, so
	// stripping CRs matches actual runtime behaviour.
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\r", "\n")

	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}

	switch strings.ToLower(language) {
	case "bash", "sh", "shell":
		// If the script body looks like PowerShell, return an empty result
		// rather than feed it to bash shellcheck. This happens for actions
		// whose action_type is "command" but body is PowerShell — the bash
		// linter has nothing useful to say about that code and would just
		// flood the editor with noise.
		if detectShellLanguage(script) == "powershell" {
			return &Result{
				Language: "powershell",
				Findings: []Finding{},
			}, nil
		}
		return v.runShellcheck(ctx, script, o)
	case "powershell", "pwsh":
		// No PowerShell linter integrated yet — accept silently rather than
		// breaking the editor. Returning a clean result keeps the Save
		// button enabled and the marker bar empty.
		return &Result{
			Language: "powershell",
			Findings: []Finding{},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q (supported: bash, powershell)", ErrUnsupportedLanguage, language)
	}
}

// excludedShellcheckCodes are the only rules we suppress globally. Both
// concern the linter's inability to resolve files it cannot see from stdin,
// not the actual quality of the user's code.
//
//	SC1090  "Can't follow non-constant source."
//	SC1091  "Not following: file not specified as input."
//
// Every other false positive Crucible scripts used to hit (SC1017, SC2034
// for LAST_*, SC2168 for top-level `local`, SC2154 for CTX_* / CRUCIBLE_*)
// is now handled by wrapForValidation building a preamble + function shell
// that expresses the real runtime context. That keeps the linter honest:
// SC2034 still flags a typo'd output variable, SC2168 still flags `local`
// in a script that genuinely has no enclosing function, SC2154 still flags
// a CTX_ name the instructor forgot to declare.
var excludedShellcheckCodes = []string{"SC1090", "SC1091"}

// runShellcheck wraps the script for context, invokes shellcheck with JSON
// output, parses the result, and maps findings back to the user's coordinate
// space before returning.
func (v *Validator) runShellcheck(ctx context.Context, script string, opts Options) (*Result, error) {
	cctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	wrapped := wrapForValidation(script, opts)

	start := time.Now()
	stdout, stderr, exitCode, err := v.run(cctx, "shellcheck", []string{
		"--shell=bash",
		"--format=json1",
		"--severity=style", // surface everything; the UI filters
		"--exclude=" + strings.Join(excludedShellcheckCodes, ","),
		"-", // read script from stdin
	}, wrapped.text)
	duration := time.Since(start)

	// shellcheck exits 1 when there are findings — that's not an error for us.
	// It exits >1 (or err != nil) only on real failure (binary missing, bad
	// flags, timeout, etc.).
	if err != nil && exitCode == 0 {
		// runner-level failure (binary missing, ctx deadline, etc.)
		return nil, fmt.Errorf("shellcheck invocation failed: %w (stderr: %s)", err, truncate(string(stderr), 512))
	}
	if exitCode > 1 {
		return nil, fmt.Errorf("shellcheck exited %d: %s", exitCode, truncate(string(stderr), 512))
	}

	findings, err := parseShellcheckJSON(stdout)
	if err != nil {
		return nil, fmt.Errorf("parse shellcheck output: %w", err)
	}
	findings = remapFindings(findings, wrapped)

	// CRU0001 — undeclared $CTX_<NAME> references. shellcheck's SC2154
	// deliberately ignores ALL_CAPS variables, so typos in CTX_ refs slip
	// past the linter entirely. We compensate with a focused static pass
	// that runs against the ORIGINAL user script (not the wrapped version)
	// so the line/column numbers map directly to what the user sees.
	findings = append(findings, undeclaredCTXFindings(script, opts)...)

	// CRU0002 — commands the runner image does not provide. shellcheck has no
	// notion of which binaries exist on the target, so `gobuster ...` lints
	// perfectly clean and then exits 127 during a graded assessment, where the
	// student sees a red check that nothing they do can turn green. Same
	// coordinate space as CRU0001: run against the ORIGINAL user script.
	findings = append(findings, unknownCommandFindings(script)...)

	res := &Result{
		Language:     "bash",
		Findings:     findings,
		DurationMs:   duration.Milliseconds(),
		LinterStderr: truncate(string(stderr), 512),
	}
	for _, f := range findings {
		switch f.Severity {
		case SeverityError:
			res.HasErrors = true
		case SeverityWarning:
			res.HasWarnings = true
		}
	}
	return res, nil
}

// shellcheckJSON is the schema shellcheck emits with --format=json1.
type shellcheckJSON struct {
	Comments []struct {
		File      string `json:"file"`
		Line      int    `json:"line"`
		EndLine   int    `json:"endLine"`
		Column    int    `json:"column"`
		EndColumn int    `json:"endColumn"`
		Level     string `json:"level"` // error, warning, info, style
		Code      int    `json:"code"`
		Message   string `json:"message"`
	} `json:"comments"`
}

func parseShellcheckJSON(raw []byte) ([]Finding, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil, nil
	}
	var doc shellcheckJSON
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("invalid shellcheck JSON: %w (got %s)", err, truncate(string(raw), 256))
	}
	findings := make([]Finding, 0, len(doc.Comments))
	for _, c := range doc.Comments {
		endLine := c.EndLine
		if endLine == 0 {
			endLine = c.Line
		}
		endCol := c.EndColumn
		if endCol == 0 {
			endCol = c.Column + 1
		}
		sev := normalizeSeverity(c.Level)
		findings = append(findings, Finding{
			Line:      c.Line,
			Column:    c.Column,
			EndLine:   endLine,
			EndColumn: endCol,
			Severity:  sev,
			Code:      fmt.Sprintf("SC%d", c.Code),
			Message:   c.Message,
		})
	}
	return findings, nil
}

func normalizeSeverity(level string) Severity {
	switch strings.ToLower(level) {
	case "error":
		return SeverityError
	case "warning":
		return SeverityWarning
	case "info":
		return SeverityInfo
	case "style":
		return SeverityStyle
	default:
		return SeverityInfo
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// execRun is the default commandRunner — invokes a real subprocess.
func execRun(ctx context.Context, name string, args []string, stdin string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	stdoutPipe, _ := cmd.StdoutPipe()
	stderrPipe, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return nil, nil, 0, err
	}
	stdout, _ := io.ReadAll(stdoutPipe)
	stderr, _ := io.ReadAll(stderrPipe)
	werr := cmd.Wait()

	exitCode := 0
	if exitErr, ok := werr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
		// Exit codes from the linter itself are not invocation errors.
		werr = nil
	}
	return stdout, stderr, exitCode, werr
}
