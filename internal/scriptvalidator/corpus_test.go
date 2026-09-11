package scriptvalidator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// corpusAction matches the JSON shape dumped from postgres into
// testdata/actions.json. Only the fields we actually use are listed.
type corpusAction struct {
	Name               string `json:"name"`
	Slug               string `json:"slug"`
	ActionType         string `json:"action_type"`
	SupportedPlatforms []string `json:"supported_platforms"`
	InputContext       []struct {
		Key string `json:"key"`
	} `json:"input_context"`
	OutputContext []struct {
		Key string `json:"key"`
	} `json:"output_context"`
	Script string `json:"script"`
}

// TestCorpus_AllProdBashActionsValidateCleanly is the regression gate that
// prevents future validator changes (rule exclusions, wrapper edits, etc.)
// from silently reintroducing the false positives that polluted the editor
// in earlier iterations.
//
// We load every action currently in production, run each one through the
// validator with its declared input/output context names, and assert that
// no errors are reported. Warnings are allowed (they're a flag for the
// instructor to review, not a hard fail).
//
// Skipped automatically if shellcheck isn't on PATH — keeps `go test ./...`
// from breaking dev environments that don't have it. CI runs in the API
// Docker image which always has shellcheck baked in (see Dockerfile).
func TestCorpus_AllProdBashActionsValidateCleanly(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skipf("shellcheck not on PATH; skipping corpus test (%v)", err)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "actions.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var actions []corpusAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}

	v := NewValidator()
	var (
		checked     int
		skipped     int
		warnedCount int
		warned      []string
	)
	for _, a := range actions {
		// Skip Windows-only actions outright — they're PowerShell, the
		// validator returns an empty result for them by design.
		if isWindowsOnly(a.SupportedPlatforms) {
			skipped++
			continue
		}
		// detectShellLanguage will skip PowerShell-shaped scripts as well,
		// so we don't need to second-guess here.
		opts := Options{
			InputContextNames:  collectKeys(a.InputContext),
			OutputContextNames: collectKeys(a.OutputContext),
		}
		res, err := v.Validate(context.Background(), "bash", a.Script, opts)
		if err != nil {
			t.Errorf("action %q: validator returned error: %v", a.Slug, err)
			continue
		}
		checked++
		if res.HasErrors {
			t.Errorf("action %q surfaced %d findings, including errors:\n  findings: %s",
				a.Slug, len(res.Findings), formatFindings(res.Findings))
		}
		if res.HasWarnings {
			warnedCount++
			warned = append(warned, a.Slug)
		}
	}
	if checked == 0 {
		t.Fatal("corpus test ran zero actions; did testdata/actions.json get truncated?")
	}
	t.Logf("corpus: checked %d bash actions, skipped %d non-bash; %d had warnings: %v",
		checked, skipped, warnedCount, warned)
}

func isWindowsOnly(platforms []string) bool {
	if len(platforms) == 0 {
		return false
	}
	for _, p := range platforms {
		if !strings.HasPrefix(strings.ToLower(p), "windows") {
			return false
		}
	}
	return true
}

func collectKeys(items []struct {
	Key string `json:"key"`
}) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Key != "" {
			out = append(out, it.Key)
		}
	}
	return out
}

func formatFindings(fs []Finding) string {
	var b strings.Builder
	for i, f := range fs {
		if i > 0 {
			b.WriteString("\n            ")
		}
		b.WriteString(string(f.Severity))
		b.WriteString(" ")
		b.WriteString(f.Code)
		b.WriteString(" @ ")
		b.WriteString(f.Message)
	}
	return b.String()
}
