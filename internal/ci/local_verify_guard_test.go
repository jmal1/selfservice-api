package ci

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var requiredLocalVerificationTiers = map[string]int{
	"ci.yaml|test|Checkout":                                                            0,
	"ci.yaml|test|Public fingerprint hygiene":                                          0,
	"ci.yaml|test|Set up Go":                                                           0,
	"ci.yaml|test|Build":                                                               0,
	"ci.yaml|test|Vet":                                                                 0,
	"ci.yaml|test|Verify wiki bundle":                                                  2,
	"ci.yaml|test|Fast fail: short Go suite":                                           0,
	"ci.yaml|test|Test":                                                                0,
	"helm-lint.yaml|lint|Install helm":                                                 4,
	"helm-lint.yaml|lint|Add chart repos":                                              4,
	"helm-lint.yaml|lint|helm dependency build":                                        4,
	"helm-lint.yaml|lint|helm lint (defaults)":                                         4,
	"helm-lint.yaml|lint|helm lint (defaults + prod overrides)":                        4,
	"helm-lint.yaml|lint|helm template (defaults + prod overrides)":                    4,
	"helm-lint.yaml|lint|assert production overlay foundation env":                     4,
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
	return runLocalVerifyScript(t, pwshPath, scriptPath, env, args...)
}

func runLocalVerifyScript(t *testing.T, pwshPath, scriptPath string, env []string, args ...string) (int, string) {
	t.Helper()
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

const fakeVerifierToolSource = `package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type invocation struct {
	Tool string   ` + "`json:\"tool\"`" + `
	Args []string ` + "`json:\"args\"`" + `
}

func main() {
	tool := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if logPath := os.Getenv("FAKE_VERIFIER_TOOL_LOG"); logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			panic(err)
		}
		if err := json.NewEncoder(f).Encode(invocation{Tool: tool, Args: os.Args[1:]}); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
	}
	switch tool {
	case "go":
		runGo()
	case "wsl":
		runWSL()
	default:
		panic("unexpected fake tool name: " + tool)
	}
}

func runGo() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "list" {
		cwd, err := os.Getwd()
		if err != nil {
			panic(err)
		}
		fmt.Println(filepath.Join(cwd, "internal", "sample"))
		return
	}
	for i := range args {
		if args[i] == "-o" && i+1 < len(args) {
			if err := os.WriteFile(args[i+1], []byte("fake linux test binary"), 0o755); err != nil {
				panic(err)
			}
			return
		}
	}
}

func runWSL() {
	args := os.Args[1:]
	if len(args) == 5 && args[0] == "-e" && args[1] == "wslpath" && args[2] == "-u" && args[3] == "--" {
		fmt.Print("/literal/" + strings.ReplaceAll(args[4], ` + "`\\`" + `, "/"))
		return
	}
	if len(args) >= 4 && args[0] == "-e" && args[1] == "bash" && args[2] == "-lc" {
		return
	}
	panic("unexpected wsl argv")
}
`

type verifierToolInvocation struct {
	Tool string   `json:"tool"`
	Args []string `json:"args"`
}

func buildFakeVerifierTools(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	binDir := t.TempDir()
	sourcePath := filepath.Join(binDir, "main.go")
	if err := os.WriteFile(sourcePath, []byte(fakeVerifierToolSource), 0o600); err != nil {
		t.Fatalf("write fake verifier tool: %v", err)
	}
	wslPath := filepath.Join(binDir, "wsl.exe")
	cmd := exec.Command("go", "build", "-o", wslPath, sourcePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake verifier tool: %v\n%s", err, out)
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName = "go.exe"
	}
	body, err := os.ReadFile(wslPath)
	if err != nil {
		t.Fatalf("read fake verifier tool: %v", err)
	}
	goPath := filepath.Join(binDir, goName)
	if err := os.WriteFile(goPath, body, 0o755); err != nil {
		t.Fatalf("write fake go tool: %v", err)
	}
	if err := os.Chmod(wslPath, 0o755); err != nil {
		t.Fatalf("chmod fake wsl.exe: %v", err)
	}
	return binDir
}

func copyLocalVerifyScript(t *testing.T, repoRoot string) string {
	t.Helper()
	source := filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read local verifier: %v", err)
	}
	if os.Getenv("LOCAL_VERIFY_SABOTAGE_STAGED_BLOB") == "1" {
		const bytePreservingRead = "$source = Get-GitIndexBlobBytes -Root $Root -RepoPath $path"
		const worktreeRead = "$source = [System.IO.File]::ReadAllBytes([System.IO.Path]::GetFullPath((Join-Path $Root $path)))"
		if strings.Count(string(body), bytePreservingRead) != 1 {
			t.Fatalf("could not uniquely sabotage staged blob byte read")
		}
		body = []byte(strings.Replace(string(body), bytePreservingRead, worktreeRead, 1))
	}
	scriptPath := filepath.Join(repoRoot, "scripts", "local-verify.ps1")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatalf("create scripts directory: %v", err)
	}
	if err := os.WriteFile(scriptPath, body, 0o600); err != nil {
		t.Fatalf("copy local verifier: %v", err)
	}
	return scriptPath
}

func readVerifierToolInvocations(t *testing.T, logPath string) []verifierToolInvocation {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open fake verifier tool log: %v", err)
	}
	defer f.Close()
	var invocations []verifierToolInvocation
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var invocation verifierToolInvocation
		if err := json.Unmarshal(scanner.Bytes(), &invocation); err != nil {
			t.Fatalf("decode fake verifier tool invocation: %v", err)
		}
		invocations = append(invocations, invocation)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fake verifier tool log: %v", err)
	}
	return invocations
}

func TestLocalVerifyTier1UsesLiteralWSLArgv(t *testing.T) {
	pwshPath := mustPwshPath(t)
	fakeBin := buildFakeVerifierTools(t)
	repoRoot := filepath.Join(t.TempDir(), "repo space's $dollar `tick` [brackets]")
	scriptPath := copyLocalVerifyScript(t, repoRoot)
	logPath := filepath.Join(t.TempDir(), "wsl-invocations.jsonl")
	env := []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_VERIFIER_TOOL_LOG=" + logPath,
	}
	code, output := runLocalVerifyScript(t, pwshPath, scriptPath, env, "-Tier", "1")
	if code != 0 {
		t.Fatalf("Tier 1 failed for literal hostile path: %s", output)
	}

	var wslInvocations []verifierToolInvocation
	for _, invocation := range readVerifierToolInvocations(t, logPath) {
		if invocation.Tool == "wsl" {
			wslInvocations = append(wslInvocations, invocation)
		}
	}
	if len(wslInvocations) != 3 {
		t.Fatalf("got %d wsl.exe invocations, want 3: %#v", len(wslInvocations), wslInvocations)
	}
	pkgPath := filepath.Join(repoRoot, "internal", "provisioning")
	binaryPath := filepath.Join(repoRoot, ".tmp", "provisioning-linux.test")
	wantConversions := [][]string{
		{"-e", "wslpath", "-u", "--", pkgPath},
		{"-e", "wslpath", "-u", "--", binaryPath},
	}
	for i, want := range wantConversions {
		if fmt.Sprint(wslInvocations[i].Args) != fmt.Sprint(want) {
			t.Fatalf("wslpath invocation %d = %#v, want %#v", i, wslInvocations[i].Args, want)
		}
	}
	converted := func(path string) string {
		return "/literal/" + strings.ReplaceAll(path, `\`, "/")
	}
	wantBash := []string{
		"-e", "bash", "-lc",
		`cd "$1" && chmod +x "$2" && "$2" -test.v`,
		"_", converted(pkgPath), converted(binaryPath),
	}
	if fmt.Sprint(wslInvocations[2].Args) != fmt.Sprint(wantBash) {
		t.Fatalf("bash invocation = %#v, want positional argv %#v", wslInvocations[2].Args, wantBash)
	}
}

func initTier0Fixture(t *testing.T) (string, string) {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "tier0-repo")
	copyLocalVerifyScript(t, repoRoot)
	goPath := filepath.Join(repoRoot, "internal", "sample", "sample [café].go")
	if err := os.MkdirAll(filepath.Dir(goPath), 0o755); err != nil {
		t.Fatalf("create Go package: %v", err)
	}
	formatted := []byte("package sample\n\nfunc Example() {}\n")
	if err := os.WriteFile(goPath, formatted, 0o600); err != nil {
		t.Fatalf("write formatted Go file: %v", err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--quiet")
	runGit("config", "core.autocrlf", "false")
	runGit("config", "user.name", "Verifier Test")
	runGit("config", "user.email", "verifier@example.invalid")
	runGit("add", ".")
	runGit("commit", "--quiet", "-m", "fixture")
	return repoRoot, goPath
}

func tier0FixtureEnvironment(t *testing.T) []string {
	t.Helper()
	fakeBin := buildFakeVerifierTools(t)
	return []string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func TestLocalVerifyTier0IgnoresCRLFOnlyChanges(t *testing.T) {
	pwshPath := mustPwshPath(t)
	repoRoot, goPath := initTier0Fixture(t)
	if err := os.WriteFile(goPath, []byte("package sample\r\n\r\nfunc Example() {}\r\n"), 0o600); err != nil {
		t.Fatalf("write CRLF Go file: %v", err)
	}
	code, output := runLocalVerifyScript(t, pwshPath, filepath.Join(repoRoot, "scripts", "local-verify.ps1"), tier0FixtureEnvironment(t), "-Tier", "0")
	if code != 0 {
		t.Fatalf("Tier 0 rejected a CRLF-only worktree change: %s", output)
	}
}

func TestLocalVerifyTier0RejectsSubstantiveGofmtDifferences(t *testing.T) {
	testCases := []string{"changed", "staged", "untracked"}
	for _, testCase := range testCases {
		t.Run(testCase, func(t *testing.T) {
			pwshPath := mustPwshPath(t)
			repoRoot, goPath := initTier0Fixture(t)
			badPath := goPath
			bad := []byte("package sample\n\nfunc Bad( ){ }\n")
			switch testCase {
			case "changed":
				if err := os.WriteFile(goPath, bad, 0o600); err != nil {
					t.Fatalf("write changed Go file: %v", err)
				}
			case "staged":
				if err := os.WriteFile(goPath, bad, 0o600); err != nil {
					t.Fatalf("write staged Go file: %v", err)
				}
				cmd := exec.Command("git", "add", filepath.ToSlash(strings.TrimPrefix(goPath, repoRoot+string(os.PathSeparator))))
				cmd.Dir = repoRoot
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("stage malformed Go file: %v\n%s", err, out)
				}
				if err := os.WriteFile(goPath, []byte("package sample\n\nfunc Example() {}\n"), 0o600); err != nil {
					t.Fatalf("restore formatted worktree file: %v", err)
				}
			case "untracked":
				badPath = filepath.Join(filepath.Dir(goPath), "untracked.go")
				if err := os.WriteFile(badPath, bad, 0o600); err != nil {
					t.Fatalf("write untracked Go file: %v", err)
				}
			}
			code, output := runLocalVerifyScript(t, pwshPath, filepath.Join(repoRoot, "scripts", "local-verify.ps1"), tier0FixtureEnvironment(t), "-Tier", "0")
			if code == 0 {
				t.Fatalf("Tier 0 accepted substantive %s gofmt drift in %s: %s", testCase, badPath, output)
			}
			if !strings.Contains(output, filepath.Base(badPath)) {
				t.Fatalf("Tier 0 did not identify %s gofmt drift path: %s", testCase, output)
			}
		})
	}
}

func TestLocalVerifyTier0PreservesStagedBlobEOF(t *testing.T) {
	pwshPath := mustPwshPath(t)
	repoRoot, goPath := initTier0Fixture(t)
	stagedWithoutEOF := []byte("package sample\n\nfunc Example() {}")
	if err := os.WriteFile(goPath, stagedWithoutEOF, 0o600); err != nil {
		t.Fatalf("write staged Go file without EOF newline: %v", err)
	}
	cmd := exec.Command("git", "add", filepath.ToSlash(strings.TrimPrefix(goPath, repoRoot+string(os.PathSeparator))))
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage Go file without EOF newline: %v\n%s", err, out)
	}
	if err := os.WriteFile(goPath, []byte("package sample\n\nfunc Example() {}\n"), 0o600); err != nil {
		t.Fatalf("restore formatted worktree file: %v", err)
	}
	code, output := runLocalVerifyScript(t, pwshPath, filepath.Join(repoRoot, "scripts", "local-verify.ps1"), tier0FixtureEnvironment(t), "-Tier", "0")
	if code == 0 {
		t.Fatalf("Tier 0 accepted a staged blob whose only defect was its missing final newline: %s", output)
	}
	if !strings.Contains(output, filepath.Base(goPath)) {
		t.Fatalf("Tier 0 did not attribute the staged EOF-only defect to %s: %s", filepath.Base(goPath), output)
	}
}

func TestLocalVerifyTier0StagedBlobEOFGuardIsLoadBearing(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestLocalVerifyTier0PreservesStagedBlobEOF$", "-test.v")
	cmd.Env = append(os.Environ(), "LOCAL_VERIFY_SABOTAGE_STAGED_BLOB=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("staged EOF regression test passed after sabotaging the byte-preserving index read:\n%s", out)
	}
	if !strings.Contains(string(out), "only defect was its missing final newline") {
		t.Fatalf("staged EOF regression failed for an unrelated reason after sabotage:\n%s", out)
	}
}

func TestLocalVerifyTier2MismatchCollectionsAreStableArrays(t *testing.T) {
	pwshPath := mustPwshPath(t)
	scriptBody, err := os.ReadFile(filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1"))
	if err != nil {
		t.Fatalf("read local verifier: %v", err)
	}
	source := string(scriptBody)
	start := strings.Index(source, "function Get-FileHashes")
	end := strings.Index(source, "function Get-SelectedTiers")
	if start < 0 || end <= start {
		t.Fatal("could not isolate production Tier 2 comparison helpers")
	}
	harness := source[start:end] + `
$root = [System.IO.Path]::GetFullPath($env:TIER2_ARRAY_FIXTURE)
foreach ($case in @("zero", "one", "multiple")) {
    $actual = Join-Path $root ($case + "-actual")
    $expected = Join-Path $root ($case + "-expected")
    $result = Compare-BundleDirectories -Actual $actual -Expected $expected
    if ($result -isnot [System.Array]) {
        throw "$case mismatch result is $($result.GetType().FullName), want a stable array"
    }
    $want = switch ($case) { "zero" { 0 } "one" { 1 } "multiple" { 2 } }
    if ($result.Count -ne $want) {
        throw "$case mismatch count is $($result.Count), want $want"
    }
}
`
	fixture := t.TempDir()
	for _, name := range []string{"zero", "one", "multiple"} {
		for _, side := range []string{"actual", "expected"} {
			if err := os.MkdirAll(filepath.Join(fixture, name+"-"+side), 0o755); err != nil {
				t.Fatalf("create Tier 2 fixture: %v", err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(fixture, "zero-actual", "same"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "zero-expected", "same"), []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "one-actual", "changed"), []byte("actual"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "one-expected", "changed"), []byte("expected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "multiple-actual", "actual-only"), []byte("actual"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "multiple-expected", "expected-only"), []byte("expected"), 0o600); err != nil {
		t.Fatal(err)
	}
	harnessPath := filepath.Join(t.TempDir(), "tier2-array-harness.ps1")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o600); err != nil {
		t.Fatalf("write Tier 2 harness: %v", err)
	}
	cmd := exec.Command(pwshPath, "-NoProfile", "-File", harnessPath)
	cmd.Env = append(os.Environ(), "TIER2_ARRAY_FIXTURE="+fixture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("production Tier 2 helper did not preserve array cardinality: %v\n%s", err, out)
	}
}

func TestLocalVerifyToolContractDoesNotRequireJQOrKubectl(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1"))
	if err != nil {
		t.Fatalf("read local verifier: %v", err)
	}
	for _, dependency := range []string{`Get-ToolCommand -ToolName "jq"`, `Get-ToolCommand -ToolName "kubectl"`} {
		if strings.Contains(string(body), dependency) {
			t.Fatalf("local verifier unexpectedly requires optional dependency %s", dependency)
		}
	}
}

func TestLocalVerifyExplicitMissingToolFails(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{name: "tier0-missing-go", args: []string{"-Tier", "0"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go"}, want: "Go toolchain is required"},
		{name: "tier0-missing-gofmt", args: []string{"-Tier", "0"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=gofmt"}, want: "gofmt is required"},
		{name: "tier1-missing-go-and-wsl", args: []string{"-Tier", "1"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go,wsl"}, want: "Go toolchain and WSL"},
		{name: "tier3-missing-docker", args: []string{"-Tier", "3"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=docker"}, want: "Docker is required"},
		{name: "tier4-missing-ssh", args: []string{"-Tier", "4", "-RemoteHost", "k3s.example.test"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=ssh"}, want: "SSH client is required"},
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
			if !strings.Contains(output, tc.want) {
				t.Fatalf("explicit missing-tool tier did not report %q: %s", tc.want, output)
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
	code, output := runLocalVerifyWithEnvironment(t, []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go,gofmt,wsl,docker,ssh"}, "-Tier", "all")
	if code != 0 {
		t.Fatalf("auto-detected all tiers should be non-fatal when tools are missing: %s", output)
	}
	if !strings.Contains(output, "[SKIP]") || !strings.Contains(output, "OVERALL: PASS") {
		t.Fatalf("auto-detected missing tools should skip and still pass; got output: %s", output)
	}
}
