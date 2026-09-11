package scriptvalidator

import (
	"strings"
	"testing"
)

func TestSanitizeNames_FiltersInjectionAttempts(t *testing.T) {
	in := []string{
		"PATH; rm -rf /",
		"foo$bar",
		"name with spaces",
		"`backticks`",
		"$(cmd)",
		"valid_name",
		"123leading_digit",
		"",
	}
	got := sanitizeNames(in)
	want := []string{"VALID_NAME"}
	if !equalStringSlice(got, want) {
		t.Errorf("sanitizeNames(%v) = %v, want %v", in, got, want)
	}
}

func TestSanitizeNames_UpperCasesDedupesSorts(t *testing.T) {
	got := sanitizeNames([]string{"foo", "FOO", "bar", "Foo", "baz"})
	want := []string{"BAR", "BAZ", "FOO"}
	if !equalStringSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSanitizeNames_EmptyInputReturnsNil(t *testing.T) {
	if got := sanitizeNames(nil); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := sanitizeNames([]string{}); got != nil {
		t.Errorf("expected nil for empty slice, got %v", got)
	}
}

// TestBuildPreamble_LineCountStable pins the preamble line offset so a future
// edit that adds a line without updating the test catches the silent drift
// before findings start landing in the wrong place.
func TestBuildPreamble_LineCountStable(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantOff int
	}{
		{
			name:    "default",
			opts:    Options{},
			wantOff: 17,
		},
		{
			name:    "with two input context names",
			opts:    Options{InputContextNames: []string{"path", "value"}},
			wantOff: 20, // +3: comment header + 2 declarations
		},
		{
			name:    "with one input + one output context",
			opts:    Options{InputContextNames: []string{"path"}, OutputContextNames: []string{"result"}},
			wantOff: 21, // +4: 2 comment headers + 2 declarations
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, got := buildPreamble(tc.opts)
			if got != tc.wantOff {
				text, _ := buildPreamble(tc.opts)
				t.Errorf("line offset = %d, want %d\npreamble:\n%s", got, tc.wantOff, text)
			}
		})
	}
}

func TestBuildPreamble_IncludesCTXDeclarations(t *testing.T) {
	text, _ := buildPreamble(Options{InputContextNames: []string{"path", "value"}})
	if !strings.Contains(text, `: "${CTX_PATH:=}"`) {
		t.Errorf("expected CTX_PATH declaration in preamble:\n%s", text)
	}
	if !strings.Contains(text, `: "${CTX_VALUE:=}"`) {
		t.Errorf("expected CTX_VALUE declaration in preamble:\n%s", text)
	}
}

func TestBuildPreamble_AlwaysDeclaresStandardEnv(t *testing.T) {
	text, _ := buildPreamble(Options{})
	for _, want := range []string{
		"CRUCIBLE_TARGET_IP", "CRUCIBLE_TARGET_USER", "CRUCIBLE_TARGET_PORT",
		"CRUCIBLE_RUN_ID", "CRUCIBLE_ACTION_LABEL", "CRUCIBLE_SOCKET",
		"CRUCIBLE_WORKDIR", "ACTION_TIMEOUT",
		"LAST_ERROR", "LAST_STUDENT_MSG", "LAST_STUDENT_MESSAGE",
		"ctx_get", "ctx_set", "ctx_has", "run_action",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preamble missing %q:\n%s", want, text)
		}
	}
}

func TestWrapForValidation_IndentsNonEmptyUserLines(t *testing.T) {
	w := wrapForValidation("echo hi\n\nls -la\n", Options{})
	if !strings.Contains(w.text, "\techo hi") {
		t.Errorf("expected tab-indented user line, got:\n%s", w.text)
	}
	if !strings.Contains(w.text, "\tls -la") {
		t.Errorf("expected tab-indented user line, got:\n%s", w.text)
	}
	// Empty line preserved without indent.
	if strings.Contains(w.text, "\t\n") {
		t.Errorf("empty line should not be indented:\n%s", w.text)
	}
}

func TestWrapForValidation_TerminatesUnterminatedScript(t *testing.T) {
	w := wrapForValidation("echo hi", Options{})
	// User script had no trailing newline; wrapper added one and then the
	// closing brace must be on a new line for the function syntax to parse.
	if !strings.Contains(w.text, "\techo hi\n}\n") {
		t.Errorf("expected newline before closing brace:\n%s", w.text)
	}
}

func TestRemapFindings_DropsPreambleFindings(t *testing.T) {
	w := wrappedScript{lineOffset: 17, userLines: 3}
	in := []Finding{{Line: 5, EndLine: 5}}
	got := remapFindings(in, w)
	if len(got) != 0 {
		t.Errorf("expected preamble finding dropped, got %+v", got)
	}
}

func TestRemapFindings_DropsEpilogueFindings(t *testing.T) {
	w := wrappedScript{lineOffset: 17, userLines: 3}
	// userLines is 3, so absolute lines 18..20 = user lines 1..3. Line 30
	// is well into the epilogue.
	in := []Finding{{Line: 30, EndLine: 30}}
	got := remapFindings(in, w)
	if len(got) != 0 {
		t.Errorf("expected epilogue finding dropped, got %+v", got)
	}
}

func TestRemapFindings_ShiftsColumnsForTabIndent(t *testing.T) {
	w := wrappedScript{lineOffset: 17, userLines: 5}
	in := []Finding{{Line: 19, EndLine: 19, Column: 5, EndColumn: 9}}
	got := remapFindings(in, w)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	if got[0].Line != 2 || got[0].Column != 4 || got[0].EndColumn != 8 {
		t.Errorf("remap wrong: %+v (want Line=2 Col=4 EndCol=8)", got[0])
	}
}

func TestRemapFindings_Column1StaysAt1(t *testing.T) {
	// Column 1 in the wrapped script could be the leading tab itself; we
	// don't shift below 1 because column 0 would be meaningless to the user.
	w := wrappedScript{lineOffset: 17, userLines: 5}
	in := []Finding{{Line: 19, EndLine: 19, Column: 1, EndColumn: 1}}
	got := remapFindings(in, w)
	if len(got) != 1 || got[0].Column != 1 || got[0].EndColumn != 1 {
		t.Errorf("column 1 should stay 1, got %+v", got)
	}
}

func TestRemapFindings_EmptyInputReturnsEmpty(t *testing.T) {
	w := wrappedScript{lineOffset: 17, userLines: 5}
	if got := remapFindings(nil, w); len(got) != 0 {
		t.Errorf("expected empty result, got %+v", got)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
