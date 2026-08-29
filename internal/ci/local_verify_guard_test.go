package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

var requiredLocalVerificationTiers = map[string]int{
	"ci.yaml|test|Checkout":                                                            0,
	"ci.yaml|test|Set up Go":                                                           0,
	"ci.yaml|test|Build":                                                               0,
	"ci.yaml|test|Vet":                                                                 0,
	"ci.yaml|test|Verify wiki bundle":                                                  2,
	"ci.yaml|test|Test":                                                                0,
	"helm-lint.yaml|lint|Install helm":                                                 4,
	"helm-lint.yaml|lint|Add chart repos":                                              4,
	"helm-lint.yaml|lint|helm dependency build":                                        4,
	"helm-lint.yaml|lint|helm lint (defaults)":                                         4,
	"helm-lint.yaml|lint|helm lint (defaults + prod overrides)":                        4,
	"helm-lint.yaml|lint|helm template (defaults + prod overrides)":                    4,
	"helm-lint.yaml|lint|assert SYNTHETIC_LIFECYCLE_ENABLED=true in prod render":       4,
	"helm-lint.yaml|lint|helm lint and template (approved full-fleet overlay)":         4,
	"helm-lint.yaml|lint|assert SYNTHETIC_LIFECYCLE_ENABLED=true in full-fleet render": 4,
}

func localCoverageFromWorkflows(t *testing.T) map[string]int {
	t.Helper()
	root := findRepoRoot(t)
	coverage := map[string]int{}
	for _, rel := range []string{".github/workflows/ci.yaml", ".github/workflows/helm-lint.yaml"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var workflow map[string]any
		if err := yaml.Unmarshal(data, &workflow); err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		jobs, ok := workflow["jobs"].(map[string]any)
		if !ok {
			t.Fatalf("jobs missing from %s", rel)
		}
		for jobName, rawJob := range jobs {
			if jobName != "test" && jobName != "lint" {
				continue
			}
			job, ok := rawJob.(map[string]any)
			if !ok {
				t.Fatalf("jobs.%s is %T, want map[string]any", jobName, rawJob)
			}
			steps, ok := job["steps"].([]any)
			if !ok {
				t.Fatalf("jobs.%s.steps is %T, want []any", jobName, job["steps"])
			}
			for _, rawStep := range steps {
				step, ok := rawStep.(map[string]any)
				if !ok {
					t.Fatalf("jobs.%s.steps entry is %T, want map[string]any", jobName, rawStep)
				}
				name, ok := step["name"].(string)
				if !ok || name == "" {
					continue
				}
				coverage[fmt.Sprintf("%s|%s|%s", filepath.Base(rel), jobName, name)] = 0
			}
		}
	}
	return coverage
}

func isLocalVerificationCoverageComplete(actual map[string]int, expected map[string]int) bool {
	if len(actual) != len(expected) {
		return false
	}
	for key := range expected {
		if _, ok := actual[key]; !ok {
			return false
		}
	}
	return true
}

func TestLocalVerificationCoverageMatchesRequiredCI(t *testing.T) {
	actual := localCoverageFromWorkflows(t)
	for key, tier := range requiredLocalVerificationTiers {
		if tier < 0 || tier > 4 {
			t.Fatalf("%q maps to unsupported local tier %d; valid tiers are 0-4", key, tier)
		}
		if _, ok := actual[key]; !ok {
			t.Fatalf("required local verification entry %q is missing from current CI checks; add the tier mapping in internal/ci/local_verify_guard_test.go", key)
		}
	}
	for key := range actual {
		if _, ok := requiredLocalVerificationTiers[key]; !ok {
			t.Fatalf("CI check %q is present locally but not mapped to a local tier; add it to requiredLocalVerificationTiers", key)
		}
	}
	if !isLocalVerificationCoverageComplete(actual, requiredLocalVerificationTiers) {
		t.Fatal("local verification coverage is incomplete or drifted from the current required workflow checks")
	}
}

func TestLocalVerificationCoverageRejectsSabotage(t *testing.T) {
	actual := localCoverageFromWorkflows(t)
	sabotaged := make(map[string]int, len(actual))
	for key, tier := range actual {
		sabotaged[key] = tier
	}
	delete(sabotaged, "ci.yaml|test|Verify wiki bundle")
	if isLocalVerificationCoverageComplete(sabotaged, requiredLocalVerificationTiers) {
		t.Fatal("sabotage proof failed: removing a required check from the coverage map still passed")
	}
}
