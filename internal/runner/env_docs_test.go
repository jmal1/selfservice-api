package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docEnvTablePath is the instructor documentation that promises a set of
// CRUCIBLE_* variables to workflow authors.
const docEnvTablePath = "../../docs/instructor/runner-environment.md"

var backtickVar = regexp.MustCompile("`(CRUCIBLE_[A-Z0-9_]+)`")

// TestDocumentedEnvVarsAreActuallySet keeps the instructor documentation honest
// about the runner's environment contract.
//
// This exists because the doc listed six variables that the runner has never
// set: CRUCIBLE_TARGET_HOSTNAME, CRUCIBLE_POD_ID, CRUCIBLE_RUN_ID,
// CRUCIBLE_WORKFLOW_SLUG, CRUCIBLE_PLAYLIST_SLUG and CRUCIBLE_STUDENT_USERNAME,
// plus per-slot names like CRUCIBLE_TARGET_WEB_IP.
//
// The failure mode is quiet and nasty. Instructors -- and the AI authoring
// agents that are pointed at this same wiki -- write a workflow using
// "$CRUCIBLE_TARGET_HOSTNAME", the script validator's preamble declares that
// name so nothing warns, and at runtime it expands to an empty string. The
// action then does something like `ssh student@` and fails in a way that points
// nowhere near the actual cause. A student sees a red check for an assessment
// that was never capable of passing.
//
// Documentation that promises an interface IS an interface. This test compares
// the promise to BuildEnv, which is the only thing that can keep them together.
func TestDocumentedEnvVarsAreActuallySet(t *testing.T) {
	provided := providedEnvNames(t)
	documented, notProvided := parseEnvDoc(t)

	// Premise checks. If either side comes back empty the comparison below
	// would trivially pass while verifying nothing at all.
	if len(provided) == 0 {
		t.Fatal("BuildEnv returned no CRUCIBLE_* variables — this guard is vacuous")
	}
	if len(documented) == 0 {
		t.Fatalf("parsed no variables from the table in %s — the parser or the path "+
			"is wrong, which would make this guard vacuous", docEnvTablePath)
	}

	for _, name := range documented {
		if !provided[name] {
			t.Errorf("%s documents %s, but BuildEnv never sets it.\n"+
				"  A workflow using it gets an empty string at runtime, and the script\n"+
				"  validator declares the name so nothing warns the author.\n"+
				"  Either set it in BuildEnv or remove it from the table.",
				filepath.Base(docEnvTablePath), name)
		}
	}

	// The doc also explicitly names variables it says are NOT provided. If one
	// of those is later implemented, the warning becomes actively misleading.
	for _, name := range notProvided {
		if provided[name] {
			t.Errorf("%s lists %s under \"Not provided\", but BuildEnv now sets it.\n"+
				"  Move it into the table — telling authors a working variable is\n"+
				"  unavailable is as harmful as the reverse.",
				filepath.Base(docEnvTablePath), name)
		}
	}
}

// providedEnvNames returns the CRUCIBLE_* variable names BuildEnv actually sets,
// using a fully-populated config so nothing is omitted for being empty.
func providedEnvNames(t *testing.T) map[string]bool {
	t.Helper()

	cfg := &RunnerConfig{
		Target: TargetConfig{IP: "10.0.0.5", OS: "linux", Username: "student", Password: "pw"},
		Pod:    PodConfig{Subnet: "10.0.0.0/24", Index: 119},
	}

	names := map[string]bool{}
	for _, kv := range cfg.BuildEnv("/wd", "/sock", "/ctx") {
		key, _, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(key, "CRUCIBLE_") {
			names[key] = true
		}
	}
	return names
}

// parseEnvDoc extracts the CRUCIBLE_* names promised by the markdown table, and
// separately those the doc explicitly disclaims in its "Not provided" callout.
//
// Table rows and callout lines are distinguished by their line prefix ("|" vs
// ">") rather than by scanning the whole file, because both mention the same
// kind of names in backticks. Scanning indiscriminately would put every
// disclaimed variable into the "must be set" list and fail on correct code.
func parseEnvDoc(t *testing.T) (documented []string, notProvided []string) {
	t.Helper()

	raw, err := os.ReadFile(docEnvTablePath)
	if err != nil {
		t.Fatalf("read %s: %v", docEnvTablePath, err)
	}

	var inCallout bool
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, ">"):
			if strings.Contains(trimmed, "Not provided") {
				inCallout = true
			}
			if inCallout {
				for _, m := range backtickVar.FindAllStringSubmatch(trimmed, -1) {
					notProvided = append(notProvided, m[1])
				}
			}
		case strings.HasPrefix(trimmed, "|"):
			inCallout = false
			// Only the first cell names a variable; later cells hold prose.
			cells := strings.Split(trimmed, "|")
			if len(cells) < 2 {
				continue
			}
			if m := backtickVar.FindStringSubmatch(cells[1]); m != nil {
				documented = append(documented, m[1])
			}
		default:
			inCallout = false
		}
	}
	return documented, notProvided
}
