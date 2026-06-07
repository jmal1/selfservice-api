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
		timeout:     8 * time.Second,
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
func (v *Validator) Validate(ctx context.Context, language, script string) (*Result, error) {
	if maxBytes := v.maxScriptKB * 1024; len(script) > maxBytes {
		return nil, fmt.Errorf("%w: script is %d bytes, max %d", ErrScriptTooLarge, len(script), maxBytes)
	}

	// Normalize CRLF/CR → LF. shellcheck flags every \r as SC1017 ("Literal
	// carriage return"), which floods the editor with noise even when the
	// script is otherwise clean. Scripts run on linux runners anyway, so
	// stripping CRs matches actual runtime behaviour.
	script = strings.ReplaceAll(script, "\r\n", "\n")
	script = strings.ReplaceAll(script, "\r", "\n")

	switch strings.ToLower(language) {
	case "bash", "sh", "shell":
		return v.runShellcheck(ctx, script)
	default:
		return nil, fmt.Errorf("%w: %q (supported: bash)", ErrUnsupportedLanguage, language)
	}
}

// excludedShellcheckCodes are rules that produce false positives for Crucible
// action / workflow scripts and so are filtered out at lint time.
//
//	SC1090, SC1091  Scripts always begin with `source /opt/crucible/lib/actions.sh`
//	                which the validator cannot resolve from stdin.
//	SC1017          Carriage-return noise (we already strip CRs in Validate,
//	                but exclude defensively in case any survive).
//	SC2168          Action bodies are commonly executed as the body of a
//	                wrapper function (via `run_action`) or sourced into a
//	                larger script, so top-level `local x=$(ctx_get …)` is
//	                idiomatic and not a real bug.
var excludedShellcheckCodes = []string{"SC1017", "SC1090", "SC1091", "SC2168"}

// runShellcheck invokes shellcheck with JSON output, parses it, and normalizes
// the findings into our Result schema.
func (v *Validator) runShellcheck(ctx context.Context, script string) (*Result, error) {
	cctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	start := time.Now()
	stdout, stderr, exitCode, err := v.run(cctx, "shellcheck", []string{
		"--shell=bash",
		"--format=json1",
		"--severity=style", // surface everything; the UI filters
		"--exclude=" + strings.Join(excludedShellcheckCodes, ","),
		"-", // read script from stdin
	}, script)
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
