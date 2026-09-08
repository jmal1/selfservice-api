package scriptvalidator

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// crucibleHelpers claims to be "kept in sync with deploy/runner/actions.sh" by
// comment alone, and that invariant has already broken once: adding
// crucible_ssh to actions.sh without registering it here made every action that
// called it warn "not installed in the runner image" — the precise false
// positive the map exists to prevent, and the kind that trains instructors to
// ignore the linter.
//
// The load-bearing direction is actions.sh -> map. A stale entry in the map is
// harmless noise; a missing one produces a wrong warning on correct code.
func TestCrucibleHelpers_CoversEveryPublicFunctionInActionsSh(t *testing.T) {
	src, err := os.ReadFile("../../deploy/runner/actions.sh")
	if err != nil {
		t.Fatalf("read actions.sh: %v", err)
	}

	defRe := regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*\(\)\s*\{`)
	matches := defRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found no function definitions in actions.sh; the pattern is broken, " +
			"so this guard would pass no matter what actions.sh defines")
	}

	var checked int
	for _, m := range matches {
		name := m[1]
		// A leading underscore marks a private helper that workflow and action
		// bodies are not expected to call.
		if strings.HasPrefix(name, "_") {
			continue
		}
		checked++
		if _, ok := crucibleHelpers[name]; !ok {
			t.Errorf("actions.sh defines %q but crucibleHelpers does not list it; "+
				"every script calling it will be warned that it is not installed", name)
		}
	}
	if checked == 0 {
		t.Fatal("every function in actions.sh looked private; the filter is wrong")
	}
	t.Logf("verified %d public actions.sh helpers are registered", checked)
}
