package scriptvalidator

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// crucibleHelpers claims to be "kept in sync with actions.sh" in the
// selfservice-crucible-runner module. The load-bearing direction is
// actions.sh -> map. A stale entry in the map is harmless noise; a missing one
// produces a wrong warning on correct code.
func TestCrucibleHelpers_CoversEveryPublicFunctionInActionsSh(t *testing.T) {
	mod, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/jmal1/selfservice-crucible-runner").Output()
	if err != nil {
		t.Fatalf("locate selfservice-crucible-runner module: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(mod)), "actions.sh"))
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
