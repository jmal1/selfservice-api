package actionlibrary

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	valid := []string{"http-get", "ufw-rule-exists", "port-open", "wait-for", "a", "win-service-running", "nmap2-service"}
	for _, slug := range valid {
		if err := ValidateSlug(slug); err != nil {
			t.Errorf("ValidateSlug(%q) rejected a legal slug: %v", slug, err)
		}
	}

	invalid := []string{
		"",             // empty
		"HTTP-Get",     // uppercase
		"http_get",     // underscore would collide with http-get's callable
		"-http-get",    // leading hyphen
		"http-get-",    // trailing hyphen
		"http--get",    // doubled hyphen
		"2fast",        // leading digit yields an illegal identifier start
		"http get",     // space
		"http.get",     // dot
		"rm -rf /",     // anything shell-shaped
		"http-get\n:x", // newline injection into the generated file
	}
	for _, slug := range invalid {
		if err := ValidateSlug(slug); err == nil {
			t.Errorf("ValidateSlug(%q) accepted an illegal slug", slug)
		}
	}
}

// Every slug ValidateSlug accepts must yield a callable, and no two distinct
// accepted slugs may collide. That injectivity is what lets the create handler
// rely on the database's slug uniqueness instead of a separate collision scan.
func TestValidateSlug_AcceptedSlugsMapInjectivelyToCallables(t *testing.T) {
	slugs := []string{"http-get", "httpget", "http-get-2", "h", "a-b-c", "ab-c", "a-bc"}
	seen := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		if err := ValidateSlug(slug); err != nil {
			t.Fatalf("fixture slug %q should be legal: %v", slug, err)
		}
		callable, err := CallableName(slug)
		if err != nil {
			t.Fatalf("CallableName(%q) failed on a slug ValidateSlug accepted: %v", slug, err)
		}
		if prev, dup := seen[callable]; dup {
			t.Errorf("slugs %q and %q both map to callable %q", prev, slug, callable)
		}
		seen[callable] = slug
	}
}

func TestValidateBody_AcceptsFunctionOnlyConstructs(t *testing.T) {
	// `local` and `return` are illegal at the top level of a script and legal
	// only inside a function. Rejecting them would reject every real action.
	body := `local name=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --name) name="$2"; shift 2;;
        *) shift;;
    esac
done
if [ -z "$name" ]; then
    LAST_ERROR="--name is required"
    return 1
fi
return 0`
	if err := ValidateBody("service_running", body); err != nil {
		t.Fatalf("ValidateBody rejected a valid action body: %v", err)
	}
}

func TestValidateBody_RejectsUnbalancedSyntax(t *testing.T) {
	cases := map[string]string{
		"missing fi":         "if [ -z \"$x\" ]; then\n    return 1\n",
		"missing done":       "while true; do\n    return 0\n",
		"stray brace":        "}\nreturn 0",
		"unclosed quote":     "echo \"unterminated\nreturn 0",
		"unclosed substitut": "x=$(echo hi\nreturn 0",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateBody("some_action", body); err == nil {
				t.Fatal("ValidateBody accepted a body that cannot be sourced")
			}
		})
	}
}

// A closing brace in the body would end the generated function early and leave
// the rest of the library at top level -- valid bash, catastrophic semantics.
// The parser catches it because the trailing `}` we append is then unmatched.
func TestValidateBody_RejectsBodyThatClosesItsOwnFunction(t *testing.T) {
	if err := ValidateBody("evil", "return 0\n}\necho pwned"); err == nil {
		t.Fatal("ValidateBody accepted a body that closes its own function")
	}
}

func TestValidateBody_ReportsAuthorLineNumbers(t *testing.T) {
	// Line 1 is fine; the unterminated `if` is reported at the end of input,
	// which must not be attributed to the synthetic wrapper line.
	err := ValidateBody("some_action", "echo ok\nif [ 1 ]; then")
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), "line 0") || strings.Contains(err.Error(), "line -1") {
		t.Errorf("line number was not rebased onto the author's body: %v", err)
	}
}

// The shipped library is the real contract. If a body in the corpus stops
// passing, either the corpus captured something unsourceable or this validator
// became stricter than the engine -- both are bugs worth failing on.
func TestValidateBody_AcceptsEveryShippedAction(t *testing.T) {
	raw, err := os.ReadFile("../scriptvalidator/testdata/actions.json")
	if err != nil {
		t.Skipf("action corpus unavailable: %v", err)
	}
	var actions []struct {
		Slug               string          `json:"slug"`
		Script             string          `json:"script"`
		SupportedPlatforms json.RawMessage `json:"supported_platforms"`
	}
	if err := json.Unmarshal(raw, &actions); err != nil {
		t.Fatalf("decode action corpus: %v", err)
	}
	if len(actions) == 0 {
		t.Fatal("action corpus is empty")
	}
	for _, a := range actions {
		if err := ValidateSlug(a.Slug); err != nil {
			t.Errorf("shipped slug %q would now be rejected: %v", a.Slug, err)
			continue
		}
		// Windows bodies are PowerShell and are excluded from the bash library.
		if strings.Contains(string(a.SupportedPlatforms), `"windows"`) {
			continue
		}
		callable, err := CallableName(a.Slug)
		if err != nil {
			t.Errorf("CallableName(%q): %v", a.Slug, err)
			continue
		}
		if err := ValidateBody(callable, a.Script); err != nil {
			t.Errorf("shipped action %q would now be rejected: %v", a.Slug, err)
		}
	}
}
