package ci

import (
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

func runPwshCommand(t *testing.T, command string, env ...string) (string, error) {
	t.Helper()
	pwshPath, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed")
	}
	cmd := exec.Command(pwshPath, "-NoProfile", "-Command", command)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestLocalVerifyConvertWindowsPathToWslPreservesLiteralArgv(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}

	root := findRepoRoot(t)
	scriptPath := filepath.Join(root, "scripts", "local-verify.ps1")
	binDir := filepath.Join(t.TempDir(), "fake-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	capturePath := filepath.Join(binDir, "fake-wsl.capture")
	wslPath := filepath.Join(binDir, "wsl.exe")
	program := "package main\n\nimport (\n    \"fmt\"\n    \"os\"\n    \"strings\"\n)\n\nfunc main() {\n    capture := os.Getenv(\"FAKE_WSL_CAPTURE\")\n    if capture != \"\" {\n        _ = os.WriteFile(capture, []byte(strings.Join(os.Args[1:], \"\\n\")), 0o600)\n    }\n    fmt.Println(\"/mnt/c/Users/jmal1/Repo $with 'quotes' and `backtick`/demo\")\n}\n"
	if err := os.WriteFile(filepath.Join(binDir, "main.go"), []byte(program), 0o600); err != nil {
		t.Fatalf("write fake wsl source: %v", err)
	}
	build := exec.Command("go", "build", "-o", wslPath, filepath.Join(binDir, "main.go"))
	build.Dir = binDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake wsl.exe: %v (%s)", err, out)
	}

	wantWindowsPath := "C:\\Users\\jmal1\\Repo $with 'quotes' and `backtick`\\demo"
	wantLinuxPath := "/mnt/c/Users/jmal1/Repo $with 'quotes' and `backtick`/demo"
	pathSep := string(os.PathListSeparator)
	escapedWindowsPath := strings.ReplaceAll(wantWindowsPath, "'", "''")
	escapedLinuxPath := strings.ReplaceAll(wantLinuxPath, "'", "''")
	command := fmt.Sprintf("$ErrorActionPreference='Stop'; $env:PATH='%s%s' + $env:PATH; . '%s'; $actual = Convert-WindowsPathToWsl -WindowsPath '%s'; if ($actual -ne '%s') { throw \"unexpected WSL path: $actual\" }; 'OK'", binDir, pathSep, scriptPath, escapedWindowsPath, escapedLinuxPath)
	output, err := runPwshCommand(t, command, "FAKE_WSL_CAPTURE="+capturePath)
	if err != nil {
		t.Fatalf("Convert-WindowsPathToWsl failed: %v\n%s", err, output)
	}
	capture, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured WSL argv: %v", err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(capture)), "\n")
	wantArgs := []string{"-e", "wslpath", "-u", "--", wantWindowsPath}
	if strings.Join(gotArgs, "\n") != strings.Join(wantArgs, "\n") {
		t.Fatalf("WSL argv mismatch: got %q want %q", strings.Join(gotArgs, "\n"), strings.Join(wantArgs, "\n"))
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("Convert-WindowsPathToWsl did not return the converted WSL path; output: %s", output)
	}
}

func TestLocalVerifyTier1UsesPositionalBashArgsForSpecialCharacters(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not installed")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}

	baseDir := filepath.Join(t.TempDir(), "repo $with 'quotes' and `backtick`")
	repoRoot := filepath.Join(baseDir, "project")
	if err := os.MkdirAll(filepath.Join(repoRoot, "internal", "provisioning"), 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/localverify\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "internal", "provisioning", "provisioning_test.go"), []byte("package provisioning\n\nfunc TestNoop(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatalf("write provisioning test: %v", err)
	}
	scriptPath := filepath.Join(repoRoot, "scripts", "local-verify.ps1")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatalf("mkdir scripts dir: %v", err)
	}
	originalScript, err := os.ReadFile(filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1"))
	if err != nil {
		t.Fatalf("read verifier script: %v", err)
	}
	if err := os.WriteFile(scriptPath, originalScript, 0o600); err != nil {
		t.Fatalf("copy verifier script: %v", err)
	}

	binDir := filepath.Join(t.TempDir(), "fake-bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	capturePath := filepath.Join(binDir, "fake-wsl.capture")
	buildFakeTool := func(name, source string) {
		t.Helper()
		srcPath := filepath.Join(binDir, name+"-stub.go")
		if err := os.WriteFile(srcPath, []byte(source), 0o600); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
		outPath := filepath.Join(binDir, name)
		if runtime.GOOS == "windows" {
			outPath += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", outPath, srcPath)
		cmd.Dir = binDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build fake %s: %v\n%s", name, err, out)
		}
	}
	fakeGoSource := `package main
import (
    "fmt"
    "os"
)
func main() {
    if len(os.Args) > 2 && os.Args[1] == "test" && os.Args[2] == "-c" {
        for i := 1; i+1 < len(os.Args); i++ {
            if os.Args[i] == "-o" {
                _ = os.WriteFile(os.Args[i+1], []byte("stub"), 0o755)
                return
            }
        }
        return
    }
    if len(os.Args) > 1 && os.Args[1] == "version" {
        fmt.Fprintln(os.Stdout, "go version devel fake")
        return
    }
}
`
	// Avoid relying on shell scripts or platform-specific file extensions; build native stubs instead.
	buildFakeTool("go", fakeGoSource)
	fakeWslSource := `package main
import (
    "fmt"
    "os"
    "strings"
)
func windowsPathToWsl(value string) string {
    value = strings.ReplaceAll(value, "\\", "/")
    value = strings.TrimSpace(value)
    if len(value) >= 2 && value[1] == ':' {
        drive := strings.ToLower(value[:1])
        remainder := strings.TrimPrefix(value[2:], "/")
        if remainder == "" {
            return "/mnt/" + drive
        }
        return "/mnt/" + drive + "/" + remainder
    }
    if strings.HasPrefix(value, "/") {
        return value
    }
    return "/" + value
}
func appendCapture(value string) {
    capture := os.Getenv("FAKE_WSL_CAPTURE")
    if capture == "" {
        return
    }
    f, err := os.OpenFile(capture, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
    if err != nil {
        return
    }
    defer f.Close()
    _, _ = f.WriteString(value)
    _, _ = f.WriteString("\n---\n")
}
func main() {
    if len(os.Args) >= 4 && os.Args[1] == "-e" && os.Args[2] == "bash" {
        appendCapture(strings.Join(os.Args[1:], "\n"))
        return
    }
    if len(os.Args) >= 6 && os.Args[1] == "-e" && os.Args[2] == "wslpath" && os.Args[3] == "-u" && os.Args[4] == "--" {
        appendCapture(strings.Join(os.Args[1:], "\n"))
        fmt.Print(windowsPathToWsl(os.Args[5]))
        return
    }
}
`
	buildFakeTool("wsl", fakeWslSource)

	windowsPathToWsl := func(value string) string {
		value = strings.ReplaceAll(value, "\\", "/")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[1] == ':' {
			drive := strings.ToLower(value[:1])
			remainder := strings.TrimPrefix(value[2:], "/")
			if remainder == "" {
				return "/mnt/" + drive
			}
			return "/mnt/" + drive + "/" + remainder
		}
		if strings.HasPrefix(value, "/") {
			return value
		}
		return "/" + value
	}
	linuxRepo := windowsPathToWsl(repoRoot)
	linuxPkgDir := windowsPathToWsl(filepath.Join(repoRoot, "internal", "provisioning"))
	linuxBinary := windowsPathToWsl(filepath.Join(repoRoot, ".tmp", "provisioning-linux.test"))
	cmd := exec.Command("pwsh", "-NoProfile", "-File", scriptPath, "-Tier", "1")
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_WSL_CAPTURE="+capturePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Tier 1 verifier failed with special-character path: %v\n%s", err, out)
	}
	capture, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured Tier 1 argv: %v", err)
	}
	blocks := strings.Split(strings.TrimSpace(string(capture)), "\n---\n")
	var bashBlock string
	for _, block := range blocks {
		trimmed := strings.TrimSpace(block)
		trimmed = strings.TrimSuffix(trimmed, "---")
		trimmed = strings.TrimSpace(trimmed)
		if strings.Contains(trimmed, "bash\n-lc") || strings.Contains(trimmed, "bash\r\n-lc") {
			bashBlock = trimmed
			break
		}
	}
	if bashBlock == "" {
		t.Fatalf("Tier 1 bash invocation was not recorded; capture was %q", string(capture))
	}
	gotArgs := strings.Split(bashBlock, "\n")
	wantArgs := []string{"-e", "bash", "-lc", "cd \"$1\" && chmod +x \"$2\" && \"$2\" -test.v", "_", linuxPkgDir, linuxBinary}
	if strings.Join(gotArgs, "\n") != strings.Join(wantArgs, "\n") {
		t.Fatalf("Tier 1 bash argv mismatch: got %q want %q (repo=%q)", strings.Join(gotArgs, "\n"), strings.Join(wantArgs, "\n"), linuxRepo)
	}
	if !strings.Contains(string(out), "[PASS] TIER 1") && !strings.Contains(string(out), "OVERALL: PASS") {
		t.Fatalf("Tier 1 verifier did not pass using special-character path: %s", out)
	}
}

func TestLocalVerifyCompareBundleDirectoriesHandlesZeroOneAndMultipleMismatches(t *testing.T) {
	root := findRepoRoot(t)
	scriptPath := filepath.Join(root, "scripts", "local-verify.ps1")
	cases := []struct {
		name     string
		actual   map[string]string
		expected map[string]string
		want     int
	}{
		{name: "zero", actual: map[string]string{"alpha.txt": "one"}, expected: map[string]string{"alpha.txt": "one"}, want: 0},
		{name: "one", actual: map[string]string{"alpha.txt": "one"}, expected: map[string]string{"alpha.txt": "two"}, want: 1},
		{name: "multiple", actual: map[string]string{"alpha.txt": "one", "beta.txt": "two"}, expected: map[string]string{"alpha.txt": "three", "beta.txt": "four"}, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseDir := t.TempDir()
			actualDir := filepath.Join(baseDir, "actual")
			expectedDir := filepath.Join(baseDir, "expected")
			if err := os.MkdirAll(actualDir, 0o755); err != nil {
				t.Fatalf("mkdir actual: %v", err)
			}
			if err := os.MkdirAll(expectedDir, 0o755); err != nil {
				t.Fatalf("mkdir expected: %v", err)
			}
			for rel, content := range tc.actual {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(actualDir, rel)), 0o755); err != nil {
					t.Fatalf("mkdir actual parent for %s: %v", rel, err)
				}
				if err := os.WriteFile(filepath.Join(actualDir, rel), []byte(content), 0o600); err != nil {
					t.Fatalf("write actual %s: %v", rel, err)
				}
			}
			for rel, content := range tc.expected {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(expectedDir, rel)), 0o755); err != nil {
					t.Fatalf("mkdir expected parent for %s: %v", rel, err)
				}
				if err := os.WriteFile(filepath.Join(expectedDir, rel), []byte(content), 0o600); err != nil {
					t.Fatalf("write expected %s: %v", rel, err)
				}
			}
			command := fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $diffs = Compare-BundleDirectories -Actual '%s' -Expected '%s'; if ($diffs.Count -ne %d) { throw ('unexpected mismatch count: ' + ($diffs -join '; ')) }; 'OK'", scriptPath, actualDir, expectedDir, tc.want)
			output, err := runPwshCommand(t, command)
			if err != nil {
				t.Fatalf("Compare-BundleDirectories failed for %s: %v\n%s", tc.name, err, output)
			}
			if !strings.Contains(output, "OK") {
				t.Fatalf("Compare-BundleDirectories did not report the expected count for %s; output: %s", tc.name, output)
			}
		})
	}
}

func TestLocalVerifyGetGoFormattingTargetsOnlyIncludesChangedFiles(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	for _, cmd := range [][]string{
		{"git", "init"},
		{"git", "config", "user.name", "Local Verifier"},
		{"git", "config", "user.email", "local-verifier@example.com"},
	} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/localverify\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	for _, cmd := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}

	scriptPath := filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1")
	command := fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $targets = Get-GoFormattingTargets -Root '%s'; if ($targets.Count -ne 0) { throw ('expected unmodified repo to have zero formatting targets, got: ' + ($targets -join ', ')) }; 'OK'", scriptPath, repoRoot)
	output, err := runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-GoFormattingTargets unexpectedly saw unmodified files: %v\n%s", err, output)
	}

	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\n\nfunc main(){println(\"demo\")}\n"), 0o600); err != nil {
		t.Fatalf("rewrite main.go to intentionally unformatted state: %v", err)
	}
	command = fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $targets = Get-GoFormattingTargets -Root '%s'; if ($targets.Count -ne 1) { throw ('expected one changed Go file, got: ' + ($targets -join ', ')) }; if ($targets[0] -ne 'main.go') { throw ('expected main.go to be the changed file, got: ' + ($targets -join ', ')) }; 'OK'", scriptPath, repoRoot)
	output, err = runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-GoFormattingTargets failed to pick up intentionally unformatted Go changes: %v\n%s", err, output)
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("Get-GoFormattingTargets did not report the changed file as expected; output: %s", output)
	}
}

func TestLocalVerifyGoFormattingNormalizesCRLFAndRejectsMalformedCRLF(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	for _, cmd := range [][]string{
		{"git", "init"},
		{"git", "config", "user.name", "Local Verifier"},
		{"git", "config", "user.email", "local-verifier@example.com"},
	} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/localverify\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	formatted := "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(formatted), 0o600); err != nil {
		t.Fatalf("write main.go before commit: %v", err)
	}
	for _, cmd := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(strings.ReplaceAll(formatted, "\n", "\r\n")), 0o600); err != nil {
		t.Fatalf("rewrite main.go with CRLF: %v", err)
	}

	scriptPath := filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1")
	command := fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $targets = Get-GoFormattingTargets -Root '%s'; if ($targets.Count -ne 1) { throw ('expected one changed Go file, got: ' + ($targets -join ', ')) }; $problems = Get-GoFormattingProblems -Root '%s' -Targets $targets; if ($problems.Count -ne 0) { throw ('expected CRLF-formatted Go file to pass, got: ' + ($problems -join '; ')) }; 'OK'", scriptPath, repoRoot, repoRoot)
	output, err := runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-GoFormattingProblems incorrectly rejected a correctly gofmt'd CRLF file: %v\n%s", err, output)
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("CRLF-formatted Go file did not pass formatting check as expected; output: %s", output)
	}

	bad := "package main\n\nfunc main(){println(\"bad\")}\n"
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(strings.ReplaceAll(bad, "\n", "\r\n")), 0o600); err != nil {
		t.Fatalf("rewrite main.go to intentionally malformed CRLF state: %v", err)
	}
	command = fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $targets = Get-GoFormattingTargets -Root '%s'; $problems = Get-GoFormattingProblems -Root '%s' -Targets $targets; if ($problems.Count -lt 1) { throw 'expected malformed CRLF Go file to fail formatting checks' }; 'OK'", scriptPath, repoRoot, repoRoot)
	output, err = runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-GoFormattingProblems failed to reject an intentionally malformed CRLF file: %v\n%s", err, output)
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("Malformed CRLF Go file did not fail formatting detection as expected; output: %s", output)
	}
}

func TestLocalVerifyGoFormattingRejectsStagedSabotage(t *testing.T) {
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	for _, cmd := range [][]string{
		{"git", "init"},
		{"git", "config", "user.name", "Local Verifier"},
		{"git", "config", "user.email", "local-verifier@example.com"},
	} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.com/localverify\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	formatted := "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(formatted), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	for _, cmd := range [][]string{{"git", "add", "."}, {"git", "commit", "-m", "init"}} {
		performed := exec.Command(cmd[0], cmd[1:]...)
		performed.Dir = repoRoot
		if out, err := performed.CombinedOutput(); err != nil {
			t.Fatalf("run %v: %v\n%s", cmd, err, out)
		}
	}

	malformed := "package main\n\nfunc main(){println(\"staged bad\")}"
	blobCmd := exec.Command("git", "hash-object", "-w", "--stdin")
	blobCmd.Dir = repoRoot
	blobCmd.Stdin = strings.NewReader(malformed)
	blobOut, err := blobCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash malformed staged blob: %v\n%s", err, blobOut)
	}
	blobHash := strings.TrimSpace(string(blobOut))
	updateCmd := exec.Command("git", "update-index", "--add", "--cacheinfo", "100644,"+blobHash+",main.go")
	updateCmd.Dir = repoRoot
	if out, err := updateCmd.CombinedOutput(); err != nil {
		t.Fatalf("stage malformed blob in index: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(formatted), 0o600); err != nil {
		t.Fatalf("restore formatted working tree while leaving malformed staged index content: %v", err)
	}

	scriptPath := filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1")
	command := fmt.Sprintf("$ErrorActionPreference='Stop'; . '%s'; $targets = Get-GoFormattingTargets -Root '%s'; $problems = Get-GoFormattingProblems -Root '%s' -Targets $targets; if ($problems.Count -lt 1) { throw 'expected staged malformed content to fail formatting checks even when the working tree is clean' }; 'OK'", scriptPath, repoRoot, repoRoot)
	output, err := runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-GoFormattingProblems did not reject staged malformed content when the worktree was formatted: %v\n%s", err, output)
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("staged malformed Go file did not trigger formatting failure; output: %s", output)
	}
}

func TestLocalVerifyOptionalDependencyNamesAreIgnored(t *testing.T) {
	scriptPath := filepath.Join(findRepoRoot(t), "scripts", "local-verify.ps1")
	command := fmt.Sprintf("$ErrorActionPreference='Stop'; $env:LOCAL_VERIFY_FORCE_MISSING_TOOLS='jq,kubectl'; . '%s'; $go = Get-ToolCommand -ToolName 'go'; if ($null -eq $go) { throw 'go should remain available to the verifier even when optional tooling is forced missing' }; $jq = Get-ToolCommand -ToolName 'jq'; if ($null -ne $jq) { throw 'jq was unexpectedly recognized as a required verifier dependency' }; $kubectl = Get-ToolCommand -ToolName 'kubectl'; if ($null -ne $kubectl) { throw 'kubectl was unexpectedly recognized as a required verifier dependency' }; 'OK'", scriptPath)
	output, err := runPwshCommand(t, command)
	if err != nil {
		t.Fatalf("Get-ToolCommand should ignore optional dependency names like jq and kubectl: %v\n%s", err, output)
	}
	if !strings.Contains(output, "OK") {
		t.Fatalf("optional dependency check did not pass; output: %s", output)
	}
}

func TestLocalVerifyExplicitMissingToolFails(t *testing.T) {
	testCases := []struct {
		name string
		args []string
		env  []string
	}{
		{name: "tier0-missing-go", args: []string{"-Tier", "0"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go"}},
		{name: "tier0-missing-gofmt", args: []string{"-Tier", "0"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=gofmt"}},
		{name: "tier1-missing-go-and-wsl", args: []string{"-Tier", "1"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go,wsl"}},
		{name: "tier3-missing-docker", args: []string{"-Tier", "3"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=docker"}},
		{name: "tier4-missing-ssh", args: []string{"-Tier", "4", "-RemoteHelm", "-RemoteHost", "k3sv01.lab.jmal.io"}, env: []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=ssh"}},
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
	code, output := runLocalVerifyWithEnvironment(t, []string{"LOCAL_VERIFY_FORCE_MISSING_TOOLS=go,gofmt,wsl,docker,ssh"}, "-Tier", "all")
	if code != 0 {
		t.Fatalf("auto-detected all tiers should be non-fatal when tools are missing: %s", output)
	}
	if !strings.Contains(output, "[SKIP]") || !strings.Contains(output, "OVERALL: PASS") {
		t.Fatalf("auto-detected missing tools should skip and still pass; got output: %s", output)
	}
}
