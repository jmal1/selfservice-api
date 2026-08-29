package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func parseMakefileWIKISeeds(t *testing.T) []string {
	t.Helper()
	root := findRepoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	var seeds []string
	inSeeds := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !inSeeds {
			if strings.HasPrefix(trimmed, "WIKI_SEEDS") && strings.Contains(trimmed, ":=") {
				inSeeds = true
			}
			continue
		}
		if strings.HasPrefix(trimmed, "WIKI_OUT") && strings.Contains(trimmed, ":=") {
			break
		}
		if trimmed == "" {
			continue
		}
		for _, chunk := range strings.Split(trimmed, "\\") {
			seed := strings.TrimSpace(chunk)
			if seed == "" || strings.HasPrefix(seed, "#") {
				continue
			}
			seeds = append(seeds, seed)
		}
	}
	if len(seeds) == 0 {
		t.Fatal("Makefile WIKI_SEEDS is empty")
	}
	return seeds
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

func TestLocalVerifyWIKISeedsDeriveFromMakefile(t *testing.T) {
	root := findRepoRoot(t)
	scriptText, err := os.ReadFile(filepath.Join(root, "scripts/local-verify.ps1"))
	if err != nil {
		t.Fatalf("read local-verify.ps1: %v", err)
	}
	if !strings.Contains(string(scriptText), "Get-WikiSeedPaths") {
		t.Fatal("scripts/local-verify.ps1 must derive tier 2 seeds from Makefile WIKI_SEEDS")
	}
	seeds := parseMakefileWIKISeeds(t)
	if len(seeds) == 0 {
		t.Fatal("Makefile WIKI_SEEDS should contain at least one seed")
	}
	for _, seed := range seeds {
		if !strings.Contains(string(scriptText), seed) {
			t.Fatalf("scripts/local-verify.ps1 must include the Makefile seed %q when deriving tier-2 bundle inputs", seed)
		}
	}
}

func runLocalVerifyWithEnvironment(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	pwshPath, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	root := findRepoRoot(t)
	scriptPath := filepath.Join(root, "scripts/local-verify.ps1")
	cmd := exec.Command(pwshPath, append([]string{"-NoProfile", "-File", scriptPath}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitCode = 1
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	return exitCode, string(out)
}

func TestLocalVerifyExplicitMissingToolFails(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "tier0-missing-go", args: []string{"-Tier", "0"}, env: []string{"PATH=" + t.TempDir()}},
		{name: "tier1-missing-go-and-wsl", args: []string{"-Tier", "1"}, env: []string{"PATH=" + filepath.Dir(mustPwshPath(t))}},
		{name: "tier3-missing-docker", args: []string{"-Tier", "3"}, env: []string{"PATH=" + t.TempDir()}},
		{name: "tier4-missing-ssh", args: []string{"-RemoteHelm", "-RemoteHost", "k3sv01.lab.jmal.io"}, env: []string{"PATH=" + t.TempDir()}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			code, output := runLocalVerifyWithEnvironment(t, tc.env, tc.args...)
			if code == 0 {
				t.Fatalf("explicit missing-tool tier returned success unexpectedly: %s", output)
			}
			if !strings.Contains(output, "[FAIL]") {
				t.Fatalf("explicit missing-tool tier should fail, got output: %s", output)
			}
		})
	}
}

func mustPwshPath(t *testing.T) string {
	t.Helper()
	pwshPath, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	return pwshPath
}

func TestLocalVerifyAutoDetectMissingToolSkips(t *testing.T) {
	code, output := runLocalVerifyWithEnvironment(t, []string{"PATH=" + filepath.Dir(mustPwshPath(t))}, "-Tier", "all")
	if code != 0 {
		t.Fatalf("auto-detected all tiers should be non-fatal when tools are missing: %s", output)
	}
	if !strings.Contains(output, "[SKIP]") || !strings.Contains(output, "OVERALL: PASS") {
		t.Fatalf("auto-detected missing tools should skip and still pass; got output: %s", output)
	}
}
