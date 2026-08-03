package scriptvalidator

import (
	"strings"
	"testing"
)

func codesFor(findings []Finding, code string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func TestCRU0002_FlagsToolNotInRunnerImage(t *testing.T) {
	// gobuster is a real, plausible tool an instructor would reach for, and it
	// is deliberately NOT in the runner image. Before this check it would lint
	// clean and then exit 127 mid-assessment.
	script := "gobuster dir -u http://$CTX_TARGET_IP -w /usr/share/seclists/small.txt\n"

	got := codesFor(unknownCommandFindings(script), "CRU0002")
	if len(got) != 1 {
		t.Fatalf("want exactly 1 CRU0002 finding, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Line != 1 {
		t.Errorf("Line = %d, want 1", f.Line)
	}
	if f.Severity != SeverityWarning {
		t.Errorf("Severity = %v, want warning; blocking a save on a heuristic is worse than the problem", f.Severity)
	}
	if !strings.Contains(f.Message, "gobuster") {
		t.Errorf("message does not name the offending command: %q", f.Message)
	}
	if !strings.Contains(f.Message, "127") {
		t.Errorf("message should explain the runtime symptom (exit 127): %q", f.Message)
	}
}

func TestCRU0002_AllowsInstalledTools(t *testing.T) {
	// Every one of these is in the manifest. A false positive here trains
	// instructors to ignore the linter, which is worse than no linter.
	script := `
nmap -sT -p 80,443 "$CTX_TARGET_IP"
curl -sk https://$CTX_TARGET_IP | jq .
netexec smb "$CTX_TARGET_IP" -u student -p pass
smbclient -L "$CTX_TARGET_IP"
redis-cli -h "$CTX_TARGET_IP" ping
ldapsearch -x -H "ldap://$CTX_TARGET_IP"
whatweb "http://$CTX_TARGET_IP"
`
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("false positives on installed tools: %+v", got)
	}
}

func TestCRU0002_AllowsBuiltinsAndCoreutils(t *testing.T) {
	script := `
if [ -n "$CTX_TARGET_IP" ]; then
    echo "checking"
    output=$(curl -s "http://$CTX_TARGET_IP" | grep -o 'ok' | head -1)
    printf '%s\n' "$output" | tr -d ' '
fi
for i in $(seq 1 3); do sleep 1; done
`
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("false positives on shell builtins/coreutils: %+v", got)
	}
}

func TestCRU0002_AllowsCrucibleHelpersAndLibraryDispatch(t *testing.T) {
	// run_action dispatches library actions as ARGUMENTS, so http_get must not
	// be treated as a command. This is why no LibraryActionNames option exists.
	script := `
run_action "HTTP Responds" http_get "http://$CTX_TARGET_IP"
ctx_set "web.checked" "yes"
value=$(ctx_get "web.checked")
`
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("false positives on run_action dispatch / crucible helpers: %+v", got)
	}
}

func TestCRU0002_AllowsScriptDefinedFunctions(t *testing.T) {
	script := `
check_port() {
    nc -z "$1" "$2"
}
check_port "$CTX_TARGET_IP" 80
`
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("false positives on locally-defined functions: %+v", got)
	}
}

func TestCRU0002_IgnoresCommentsAndSingleQuotedText(t *testing.T) {
	script := `
# gobuster would be nice here but is not installed
echo 'gobuster dir -u http://x'
`
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("flagged a command inside a comment or single-quoted string: %+v", got)
	}
}

func TestCRU0002_SeesCommandsAfterPipesAndOperators(t *testing.T) {
	// A missing tool tucked behind a pipe is exactly the one a quick read
	// misses, so it is the one the linter most needs to catch.
	script := "curl -s http://x | sqlmap --batch\n"

	got := codesFor(unknownCommandFindings(script), "CRU0002")
	if len(got) != 1 {
		t.Fatalf("want 1 CRU0002 for the piped command, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Message, "sqlmap") {
		t.Errorf("wrong command reported: %q", got[0].Message)
	}
}

func TestCRU0002_ResolvesWrapperPrefixes(t *testing.T) {
	// `sudo gobuster` must report gobuster, not sudo — reporting the wrapper
	// would send the instructor looking in entirely the wrong place.
	got := codesFor(unknownCommandFindings("sudo gobuster dir -u http://x\n"), "CRU0002")
	for _, f := range got {
		if strings.Contains(f.Message, "'sudo'") {
			t.Errorf("reported the wrapper instead of the wrapped command: %q", f.Message)
		}
	}
}

func TestCRU0002_IgnoresVariableAssignments(t *testing.T) {
	script := "TARGET=gobuster\nfoo_bar=1\necho \"$TARGET\"\n"
	if got := codesFor(unknownCommandFindings(script), "CRU0002"); len(got) != 0 {
		t.Errorf("flagged a variable assignment as a command: %+v", got)
	}
}
