package scriptvalidator

import "testing"

func TestUndeclaredCTXFindings_FlagsTypo(t *testing.T) {
	script := `local path="${CTX_FILE:-}"
if [ -f "$CTX_FIEL" ]; then
    echo "exists"
fi
`
	fs := undeclaredCTXFindings(script, Options{InputContextNames: []string{"file"}})
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(fs), fs)
	}
	got := fs[0]
	if got.Code != "CRU0001" {
		t.Errorf("expected code CRU0001, got %q", got.Code)
	}
	if got.Severity != SeverityWarning {
		t.Errorf("expected warning severity, got %q", got.Severity)
	}
	if got.Line != 2 {
		t.Errorf("expected line 2, got %d", got.Line)
	}
}

func TestUndeclaredCTXFindings_DeclaredInputAccepted(t *testing.T) {
	script := `echo "$CTX_FILE"
echo "${CTX_PATH:-/tmp}"
`
	fs := undeclaredCTXFindings(script, Options{
		InputContextNames: []string{"file", "path"},
	})
	if len(fs) != 0 {
		t.Errorf("expected 0 findings, got: %+v", fs)
	}
}

func TestUndeclaredCTXFindings_DeclaredOutputAccepted(t *testing.T) {
	script := `local result
result=$(some_cmd)
CTX_RESULT="$result"
echo "$CTX_RESULT"
`
	fs := undeclaredCTXFindings(script, Options{
		OutputContextNames: []string{"result"},
	})
	if len(fs) != 0 {
		t.Errorf("expected 0 findings, got: %+v", fs)
	}
}

func TestUndeclaredCTXFindings_AllExpansionFormsCaught(t *testing.T) {
	script := `echo "$CTX_BARE"
echo "${CTX_BRACED}"
echo "${CTX_DEFAULT:-fallback}"
echo "${CTX_LENGTH}"
echo "${#CTX_LEN_OP}"
`
	fs := undeclaredCTXFindings(script, Options{})
	// All five forms should produce a finding for distinct names.
	if len(fs) != 5 {
		t.Errorf("expected 5 findings (one per CTX form), got %d:\n  %+v", len(fs), fs)
	}
	names := map[string]bool{}
	for _, f := range fs {
		// extract name from message
		if !contains(f.Message, "CTX_") {
			t.Errorf("finding message missing CTX_ name: %q", f.Message)
		}
		names[f.Message] = true
	}
	if len(names) != 5 {
		t.Errorf("expected 5 unique findings, got %d unique: %v", len(names), names)
	}
}

func TestUndeclaredCTXFindings_DedupesIdenticalRefs(t *testing.T) {
	// Same name, same position should not be duplicated. Different positions
	// (different lines) should each surface.
	script := `echo "$CTX_TYPO"
echo "$CTX_TYPO"
`
	fs := undeclaredCTXFindings(script, Options{})
	if len(fs) != 2 {
		t.Errorf("expected 2 findings (two lines), got %d: %+v", len(fs), fs)
	}
}

func TestUndeclaredCTXFindings_EmptyScriptReturnsNothing(t *testing.T) {
	fs := undeclaredCTXFindings("", Options{})
	if len(fs) != 0 {
		t.Errorf("empty script should produce no findings, got: %+v", fs)
	}
}

func TestUndeclaredCTXFindings_NoCTXRefsReturnsNothing(t *testing.T) {
	script := `local x=1
echo "$x"
echo "$HOME"
echo "$CRUCIBLE_TARGET_IP"
`
	fs := undeclaredCTXFindings(script, Options{})
	if len(fs) != 0 {
		t.Errorf("script with no CTX_ refs should produce no findings, got: %+v", fs)
	}
}

func TestUndeclaredCTXFindings_DeterministicOrder(t *testing.T) {
	// Two findings on the same line should sort by column.
	script := `echo "$CTX_ZED $CTX_AYE"
`
	fs := undeclaredCTXFindings(script, Options{})
	if len(fs) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(fs))
	}
	if fs[0].Column >= fs[1].Column {
		t.Errorf("findings not sorted by column: %d then %d", fs[0].Column, fs[1].Column)
	}
}

func TestUndeclaredCTXFindings_NameNormalization(t *testing.T) {
	// Declared names are upper-cased before comparison, so the user can
	// declare them with any case on the form.
	script := `echo "$CTX_HOSTNAME"
`
	fs := undeclaredCTXFindings(script, Options{
		InputContextNames: []string{"hostname"},
	})
	if len(fs) != 0 {
		t.Errorf("lowercase declaration should match ALL_CAPS use, got: %+v", fs)
	}
}

// contains is a tiny helper because std lib's `strings.Contains` would
// require an import we'd otherwise not need in this test file.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
