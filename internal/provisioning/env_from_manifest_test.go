package provisioning

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// extractBashFunction pulls exactly one top-level `name() { ... }` function
// definition out of deploy.sh by counting brace depth across the raw text
// (deploy.sh's own awk one-liners only ever balance their braces, they never
// hide one in a comment or string in a way that would throw this off - see
// the manual verification this test file's helpers were built against).
// This lets these tests exercise env_from_manifest in isolation without
// running the rest of deploy.sh, which requires a fully mocked
// kubectl/helm/git environment (see newDeployScriptEnvironment elsewhere in
// this package).
func extractBashFunction(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")
	prefix := name + "() {"
	for i, line := range lines {
		if strings.TrimRight(line, "\r") != prefix {
			continue
		}
		var out strings.Builder
		depth := 0
		for j := i; j < len(lines); j++ {
			l := strings.TrimRight(lines[j], "\r")
			depth += strings.Count(l, "{") - strings.Count(l, "}")
			out.WriteString(l)
			out.WriteString("\n")
			if depth == 0 {
				return out.String()
			}
		}
		t.Fatalf("deploy.sh function %s never closes its opening brace", name)
	}
	t.Fatalf("deploy.sh does not define function %s", name)
	return ""
}

// runEnvFromManifest runs the real env_from_manifest function (extracted
// live from deploy.sh, not reimplemented here) against manifest on stdin and
// reports its stdout, exit code, and combined output.
func runEnvFromManifest(t *testing.T, variable, manifest string) (value string, exitCode int, output string) {
	t.Helper()
	requirePOSIXShell(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	writeFile(t, manifestPath, manifest)
	scriptPath := filepath.Join(dir, "run.sh")
	writeExecutable(t, scriptPath, extractBashFunction(t, "env_from_manifest")+
		"env_from_manifest \"$1\" < \"$2\"\n")
	out, err := exec.Command("bash", scriptPath, variable, manifestPath).CombinedOutput()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("failed to run env_from_manifest: %v\n%s", err, out)
		}
		code = exitErr.ExitCode()
	}
	return strings.TrimSuffix(string(out), "\n"), code, string(out)
}

// serverDefaultedWorkerFoundationEnv reproduces the exact env block shape a
// real `kubectl ... --dry-run=server -o yaml` response returns for the
// selfservice-worker container's content-filter section. corev1.EnvVar
// marshals Value with `json:"value,omitempty"`, so when
// WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL is unset (empty string), the
// server response omits its "value:" line entirely and the very next line is
// WORKER_CONTENT_FILTER_ALLOWLIST's own "- name:" line - this is the exact
// live shape that broke env_from_manifest's unconditional getline.
func serverDefaultedWorkerFoundationEnv(feedURL string) string {
	feedLine := ""
	if feedURL != "" {
		feedLine = "\n          value: \"" + feedURL + "\""
	}
	return `        env:
        - name: WORKER_CONTENT_FILTER_ENABLED
          value: "false"
        - name: WORKER_CONTENT_FILTER_SOURCE_NETWORK
          value: "10.100.0.0/16"
        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL` + feedLine + `
        - name: WORKER_CONTENT_FILTER_ALLOWLIST
`
}

// serverDefaultedSyntheticFoundationEnv is the synthetic-cronjob analogue of
// serverDefaultedWorkerFoundationEnv above.
func serverDefaultedSyntheticFoundationEnv(feedURL string) string {
	feedLine := ""
	if feedURL != "" {
		feedLine = "\n                  value: \"" + feedURL + "\""
	}
	return `            env:
                - name: SYNTHETIC_CONTENT_FILTER_EXPECTED
                  value: "false"
                - name: SYNTHETIC_CONTENT_FILTER_SOURCE_NETWORK
                  value: "10.100.0.0/16"
                - name: SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL` + feedLine + `
                - name: SYNTHETIC_CONTENT_FILTER_ALLOWLIST
`
}

// serverDefaultedWorkerFoundationEnvWithValueFrom builds the worker
// content-filter env block for the case where
// WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL is sourced from a Secret or
// ConfigMap rather than a literal. Real corev1.EnvVar.ValueFrom renders as a
// "valueFrom:" mapping nested one indent level deeper than the "- name:"
// line, immediately following it - the exact shape env_from_manifest must
// distinguish from an omitted (empty) literal value and reject outright,
// since a secret/configMap-sourced variable is never safely "empty" just
// because it lacks a literal "value:" line.
func serverDefaultedWorkerFoundationEnvWithValueFrom(refKind string) string {
	var refBlock string
	switch refKind {
	case "secretKeyRef":
		refBlock = `          valueFrom:
            secretKeyRef:
              name: content-filter-secret
              key: feed-url
`
	case "configMapKeyRef":
		refBlock = `          valueFrom:
            configMapKeyRef:
              name: content-filter-config
              key: feed-url
`
	default:
		panic("unknown refKind: " + refKind)
	}
	return `        env:
        - name: WORKER_CONTENT_FILTER_ENABLED
          value: "false"
        - name: WORKER_CONTENT_FILTER_SOURCE_NETWORK
          value: "10.100.0.0/16"
        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
` + refBlock + `        - name: WORKER_CONTENT_FILTER_ALLOWLIST
`
}

// serverDefaultedSyntheticFoundationEnvWithValueFrom is the synthetic-cronjob
// analogue of serverDefaultedWorkerFoundationEnvWithValueFrom above.
func serverDefaultedSyntheticFoundationEnvWithValueFrom(refKind string) string {
	var refBlock string
	switch refKind {
	case "secretKeyRef":
		refBlock = `                  valueFrom:
                    secretKeyRef:
                      name: content-filter-secret
                      key: feed-url
`
	case "configMapKeyRef":
		refBlock = `                  valueFrom:
                    configMapKeyRef:
                      name: content-filter-config
                      key: feed-url
`
	default:
		panic("unknown refKind: " + refKind)
	}
	return `            env:
                - name: SYNTHETIC_CONTENT_FILTER_EXPECTED
                  value: "false"
                - name: SYNTHETIC_CONTENT_FILTER_SOURCE_NETWORK
                  value: "10.100.0.0/16"
                - name: SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
` + refBlock + `                - name: SYNTHETIC_CONTENT_FILTER_ALLOWLIST
`
}

// TestEnvFromManifestRejectsValueFrom is the load-bearing regression test for
// the second, independently-reviewed blocker: the first fix's "any
// non-'value:' line means the variable was omitted (i.e. empty)" logic also
// silently accepted a "valueFrom:" secretKeyRef/configMapKeyRef line as an
// empty value, which would let a Secret- or ConfigMap-sourced content-filter
// feed URL sail past validate_foundation_intent's "candidate must keep ...
// its category feed empty" check undetected. env_from_manifest must instead
// fail closed (non-zero exit, no printed value) whenever the line
// immediately following "- name: X" is a "valueFrom:" field belonging to
// that same entry, for both known ValueFrom source kinds and for both
// foundation vars that legitimately need to read as empty.
func TestEnvFromManifestRejectsValueFrom(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variable string
		manifest string
	}{
		{
			name:     "worker feed url secretKeyRef",
			variable: "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedWorkerFoundationEnvWithValueFrom("secretKeyRef"),
		},
		{
			name:     "worker feed url configMapKeyRef",
			variable: "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedWorkerFoundationEnvWithValueFrom("configMapKeyRef"),
		},
		{
			name:     "synthetic feed url secretKeyRef",
			variable: "SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedSyntheticFoundationEnvWithValueFrom("secretKeyRef"),
		},
		{
			name:     "synthetic feed url configMapKeyRef",
			variable: "SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedSyntheticFoundationEnvWithValueFrom("configMapKeyRef"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, code, output := runEnvFromManifest(t, tc.variable, tc.manifest)
			if code == 0 {
				t.Fatalf("env_from_manifest %s unexpectedly succeeded with value %q, want it to reject a valueFrom-sourced variable rather than treat it as empty:\n%s", tc.variable, value, output)
			}
		})
	}
}

// TestEnvFromManifestValueFromSabotage proves TestEnvFromManifestRejectsValueFrom
// is load-bearing: it runs the *previously shipped* fix (the one that treats
// any non-"value:" line as an omitted-empty value, without checking whether
// that line structurally starts the next env item or exits the env
// list/block/document) against a valueFrom fixture and asserts it wrongly
// reports the variable as empty. This is exactly the bypass the independent
// review flagged.
func TestEnvFromManifestValueFromSabotage(t *testing.T) {
	requirePOSIXShell(t)
	const previousEnvFromManifest = `env_from_manifest() {
  local variable=$1
  awk -v variable="$variable" '
    $0 ~ "^[[:space:]]*- name:[[:space:]]*" variable "[[:space:]]*$" {
      matches++
      if ((getline next_line) <= 0) {
        exit 2
      }
      if (next_line ~ /^[[:space:]]*value:[[:space:]]*/) {
        value = next_line
        sub(/^[[:space:]]*value:[[:space:]]*/, "", value)
        gsub(/^["'"'"']|["'"'"']$/, "", value)
      } else {
        value = ""
        if (next_line ~ "^[[:space:]]*- name:[[:space:]]*" variable "[[:space:]]*$") {
          matches++
        }
      }
    }
    END {
      if (matches != 1) {
        exit 3
      }
      print value
    }
  '
}
`
	manifest := serverDefaultedWorkerFoundationEnvWithValueFrom("secretKeyRef")
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	writeFile(t, manifestPath, manifest)
	scriptPath := filepath.Join(dir, "run.sh")
	writeExecutable(t, scriptPath, previousEnvFromManifest+
		"env_from_manifest WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL < \"$1\"\n")
	out, err := exec.Command("bash", scriptPath, manifestPath).CombinedOutput()
	if err != nil {
		t.Fatalf("previous env_from_manifest unexpectedly errored (fixture no longer reproduces the bypass): %v\n%s", err, out)
	}
	got := strings.TrimSuffix(string(out), "\n")
	if got != "" {
		t.Fatalf("previous env_from_manifest returned %q, want it to wrongly report empty for a valueFrom-sourced variable (proving this fixture reproduces the bypass)", got)
	}

	// The current (hardened) implementation must reject this instead of
	// silently treating the secret-sourced variable as empty.
	value, code, output := runEnvFromManifest(t, "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL", manifest)
	if code == 0 {
		t.Fatalf("hardened env_from_manifest unexpectedly succeeded with value %q, want rejection:\n%s", value, output)
	}
}

// TestEnvFromManifestOmittedValueListEnd covers the structural shapes that
// legitimately signal an omitted (empty) value once the consumed line is not
// itself a "value:" scalar and is not deeper-indented than "- name: X"
// (which would mean it belongs to X's own entry, e.g. "valueFrom:"): the env
// list ending because the container has no further env entries and its next
// field follows at the same indent as the list items (real k8s-rendered
// YAML aligns a block sequence's "- " markers with their parent mapping
// key, so a later sibling field of the *container* sits at that same indent
// - see the "- name: provision-worker" / "env:" / "- name: ..." nesting in
// serverDefaultedWorkerFoundationEnv above, which mirrors the actual chart
// and the rest of this package's fixtures), the env list ending via a
// shallower dedent (e.g. exiting a nested list entirely), and the env list
// ending because the current YAML document ends and another begins ("---").
func TestEnvFromManifestOmittedValueListEnd(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{
			// This is the shape that actually occurs in real dry-run output
			// whenever the omitted-value variable happens to be the last env
			// entry in the container: the container's next field (here
			// "resources:") is rendered at the *same* indent as "env:" and
			// its "- name:" items, not indented deeper.
			name: "list ends via a same-indent sibling container field",
			manifest: `        env:
        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
        resources: {}
`,
		},
		{
			name: "list ends via a dedent to a shallower field",
			manifest: `          env:
          - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
        resources: {}
`,
		},
		{
			name: "list ends via a document separator",
			manifest: `        env:
        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
---
apiVersion: v1
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, code, output := runEnvFromManifest(t, "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL", tc.manifest)
			if code != 0 {
				t.Fatalf("env_from_manifest exited %d:\n%s", code, output)
			}
			if value != "" {
				t.Fatalf("env_from_manifest = %q, want empty", value)
			}
		})
	}
}

// TestEnvFromManifestOmittedValueSerialization is the load-bearing regression
// test for the exact 2026-08-25 dry-run failure: a real server-side dry-run
// response drops an EnvVar's "value:" line entirely when Value is the empty
// string (corev1.EnvVar's `omitempty`), so "- name: X" can be immediately
// followed by the *next* entry's own "- name:" line instead of X's value.
// This covers all four foundation vars validate_foundation_intent reads via
// env_from_manifest whose value can legitimately be empty.
func TestEnvFromManifestOmittedValueSerialization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variable string
		manifest string
		want     string
	}{
		{
			name:     "worker content filter enabled scalar unaffected by sibling omission",
			variable: "WORKER_CONTENT_FILTER_ENABLED",
			manifest: serverDefaultedWorkerFoundationEnv(""),
			want:     "false",
		},
		{
			name:     "worker category feed omitted value reads empty not next name",
			variable: "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedWorkerFoundationEnv(""),
			want:     "",
		},
		{
			name:     "worker category feed present value still parses",
			variable: "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedWorkerFoundationEnv("https://student-filter-feed.example.test"),
			want:     "https://student-filter-feed.example.test",
		},
		{
			name:     "synthetic content filter expected scalar unaffected by sibling omission",
			variable: "SYNTHETIC_CONTENT_FILTER_EXPECTED",
			manifest: serverDefaultedSyntheticFoundationEnv(""),
			want:     "false",
		},
		{
			name:     "synthetic category feed omitted value reads empty not next name",
			variable: "SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedSyntheticFoundationEnv(""),
			want:     "",
		},
		{
			name:     "synthetic category feed present value still parses",
			variable: "SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
			manifest: serverDefaultedSyntheticFoundationEnv("https://student-filter-feed.example.test"),
			want:     "https://student-filter-feed.example.test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, code, output := runEnvFromManifest(t, tc.variable, tc.manifest)
			if code != 0 {
				t.Fatalf("env_from_manifest %s exited %d:\n%s", tc.variable, code, output)
			}
			if value != tc.want {
				t.Fatalf("env_from_manifest %s = %q, want %q", tc.variable, value, tc.want)
			}
		})
	}
}

// TestEnvFromManifestScalarAndBooleanValues checks ordinary true/false/scalar
// extraction (including quote-stripping) keeps working unchanged.
func TestEnvFromManifestScalarAndBooleanValues(t *testing.T) {
	manifest := `        env:
        - name: WORKER_PROVISIONING_CLAIMS_ENABLED
          value: "true"
        - name: RUNNER_IMAGE
          value: "ghcr.io/jmal1/selfservice-crucible-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
        - name: SYNTHETIC_LIFECYCLE_ENABLED
          value: "false"
        - name: SYNTHETIC_RUNNER_EXPECTED_ENABLED
          value: "true"
`
	for _, tc := range []struct {
		variable string
		want     string
	}{
		{"WORKER_PROVISIONING_CLAIMS_ENABLED", "true"},
		{"RUNNER_IMAGE", "ghcr.io/jmal1/selfservice-crucible-runner@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{"SYNTHETIC_LIFECYCLE_ENABLED", "false"},
		{"SYNTHETIC_RUNNER_EXPECTED_ENABLED", "true"},
	} {
		value, code, output := runEnvFromManifest(t, tc.variable, manifest)
		if code != 0 {
			t.Fatalf("env_from_manifest %s exited %d:\n%s", tc.variable, code, output)
		}
		if value != tc.want {
			t.Fatalf("env_from_manifest %s = %q, want %q", tc.variable, value, tc.want)
		}
	}
}

// TestEnvFromManifestRejectsMissingAndDuplicateMatches proves the fix did not
// weaken the existing zero/duplicate-match rejection, including the tricky
// case of two immediately-adjacent omitted-value entries for the same name
// (which the fix's re-test of the consumed line must still catch as a
// duplicate rather than silently swallowing one of them).
func TestEnvFromManifestRejectsMissingAndDuplicateMatches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variable string
		manifest string
	}{
		{
			name:     "missing variable",
			variable: "NOT_PRESENT",
			manifest: "        - name: WORKER_CONTENT_FILTER_ENABLED\n          value: \"false\"\n",
		},
		{
			name:     "duplicate variable with values",
			variable: "WORKER_CONTENT_FILTER_ENABLED",
			manifest: "        - name: WORKER_CONTENT_FILTER_ENABLED\n          value: \"false\"\n" +
				"        - name: WORKER_CONTENT_FILTER_ENABLED\n          value: \"true\"\n",
		},
		{
			name:     "immediately adjacent duplicate with omitted values",
			variable: "DUPVAR",
			manifest: "        - name: DUPVAR\n        - name: DUPVAR\n        - name: OTHER\n          value: \"x\"\n",
		},
		{
			name:     "name found but nothing follows in the manifest",
			variable: "TRAILING",
			manifest: "        - name: TRAILING\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, code, output := runEnvFromManifest(t, tc.variable, tc.manifest)
			if code == 0 {
				t.Fatalf("env_from_manifest %s unexpectedly succeeded with value %q:\n%s", tc.variable, value, output)
			}
		})
	}
}

// TestEnvFromManifestOmittedValueSabotage is the explicit sabotage proof: it
// runs the pre-fix env_from_manifest implementation (reproduced verbatim
// from before this change, not re-derived) against the exact fixture from
// TestEnvFromManifestOmittedValueSerialization and asserts it returns the
// wrong, non-empty value that caused
// "candidate must keep WORKER_CONTENT_FILTER_ENABLED=false and its category
// feed empty" to fire on a clean production candidate. This proves the new
// regression test is load-bearing: it fails against the old parser and
// passes only because of the fix.
func TestEnvFromManifestOmittedValueSabotage(t *testing.T) {
	requirePOSIXShell(t)
	const oldEnvFromManifest = `env_from_manifest() {
  local variable=$1
  awk -v variable="$variable" '
    $0 ~ "^[[:space:]]*- name:[[:space:]]*" variable "[[:space:]]*$" {
      matches++
      if (getline <= 0) {
        exit 2
      }
      value = $0
      sub(/^[[:space:]]*value:[[:space:]]*/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
    }
    END {
      if (matches != 1) {
        exit 3
      }
      print value
    }
  '
}
`
	manifest := serverDefaultedWorkerFoundationEnv("")
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	writeFile(t, manifestPath, manifest)
	scriptPath := filepath.Join(dir, "run.sh")
	writeExecutable(t, scriptPath, oldEnvFromManifest+
		"env_from_manifest WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL < \"$1\"\n")
	out, err := exec.Command("bash", scriptPath, manifestPath).CombinedOutput()
	if err != nil {
		t.Fatalf("old env_from_manifest unexpectedly errored (fixture no longer reproduces the bug): %v\n%s", err, out)
	}
	got := strings.TrimSuffix(string(out), "\n")
	if got == "" {
		t.Fatal("old env_from_manifest unexpectedly returned empty; the omitted-value fixture no longer reproduces the historical bug")
	}
	if !strings.Contains(got, "WORKER_CONTENT_FILTER_ALLOWLIST") {
		t.Fatalf("old env_from_manifest returned %q, want it to misparse the next \"- name:\" line as the value", got)
	}

	// The current (fixed) implementation must not reproduce that misparse.
	fixed, code, fixedOutput := runEnvFromManifest(t, "WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL", manifest)
	if code != 0 {
		t.Fatalf("fixed env_from_manifest exited %d:\n%s", code, fixedOutput)
	}
	if fixed != "" {
		t.Fatalf("fixed env_from_manifest = %q, want empty", fixed)
	}
}
