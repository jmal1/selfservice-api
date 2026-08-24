package provisioning

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDeployScriptRollbackContainmentPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy script semantics require a POSIX shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	script := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	for _, test := range []struct {
		name           string
		renderedClaims string
		liveClaims     string
		helmStatus     string
		args           []string
		wantSuccess    bool
		wantOutput     string
	}{
		{
			name:           "verified rollback target",
			renderedClaims: "false",
			liveClaims:     "false",
			helmStatus:     "deployed",
			args:           []string{"--verify-rollback-containment"},
			wantSuccess:    true,
			wantOutput:     "rollback containment verified",
		},
		{
			name:           "live override cannot substitute for helm baseline",
			renderedClaims: "true",
			liveClaims:     "false",
			helmStatus:     "deployed",
			args:           []string{"--verify-rollback-containment"},
			wantOutput:     "rollback target",
		},
		{
			name:           "helm baseline cannot substitute for live rollout",
			renderedClaims: "false",
			liveClaims:     "true",
			helmStatus:     "deployed",
			args:           []string{"--verify-rollback-containment"},
			wantOutput:     "live worker deployment",
		},
		{
			name:           "mutable image cannot be a rollback baseline",
			renderedClaims: "false",
			liveClaims:     "false",
			helmStatus:     "deployed",
			args:           []string{"--verify-rollback-containment"},
			wantOutput:     "immutable sha256 digest",
		},
		{
			name:           "failed latest revision cannot be rollback baseline",
			renderedClaims: "false",
			liveClaims:     "false",
			helmStatus:     "failed",
			args:           []string{"--verify-rollback-containment"},
			wantOutput:     "status is failed",
		},
		{
			name:           "standard deploy is gated before pull and upgrade",
			renderedClaims: "true",
			liveClaims:     "false",
			helmStatus:     "deployed",
			wantOutput:     "rollback target",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			binDir := t.TempDir()
			helmPath := filepath.Join(binDir, "helm")
			kubectlPath := filepath.Join(binDir, "kubectl")
			gitPath := filepath.Join(binDir, "git")
			renderedImage := "ghcr.io/jmal1/selfservice-provision-worker@sha256:" + strings.Repeat("a", 64)
			if test.name == "mutable image cannot be a rollback baseline" {
				renderedImage = "ghcr.io/jmal1/selfservice-provision-worker:latest"
			}
			writeExecutable(t, helmPath, `#!/bin/bash
set -euo pipefail
if [ "$1" = "history" ]; then
  cat <<EOF
- app_version: test
  revision: 128
  status: `+test.helmStatus+`
EOF
  exit 0
fi
if [ "$1 $2" = "get manifest" ]; then
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
spec:
  template:
    spec:
      containers:
        - name: provision-worker
          image: "`+renderedImage+`"
          env:
            - name: WORKER_PROVISIONING_CLAIMS_ENABLED
              value: "`+test.renderedClaims+`"
EOF
  exit 0
fi
echo "unexpected helm invocation: $*" >&2
exit 90
`)
			writeExecutable(t, kubectlPath, `#!/bin/bash
set -euo pipefail
case "$*" in
  *WORKER_PROVISIONING_CLAIMS_ENABLED*) printf '%s' '`+test.liveClaims+`' ;;
  *'.containers[?(@.name=="provision-worker")].image'*) printf '%s' '`+renderedImage+`' ;;
  *) echo "unexpected kubectl invocation: $*" >&2; exit 90 ;;
esac
`)
			writeExecutable(t, gitPath, `#!/bin/bash
echo "git invoked before rollback containment" >&2
exit 91
`)

			cmd := exec.Command("bash", append([]string{script}, test.args...)...)
			cmd.Env = append(
				os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"KUBECONFIG=/dev/null",
			)
			output, err := cmd.CombinedOutput()
			if test.wantSuccess && err != nil {
				t.Fatalf("preflight failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("sabotaged containment unexpectedly passed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
		})
	}
}

func TestDeployScriptClaimsBaselineRejectsWorkerDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("deploy script semantics require a POSIX shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	baselineChart := filepath.Join(tempDir, "safe-chart")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(baselineChart, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baselineChart, "Chart.yaml"), []byte("apiVersion: v2\nname: selfservice\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	liveManifest := filepath.Join(tempDir, "live.yaml")
	candidateManifest := filepath.Join(tempDir, "candidate.yaml")
	upgradeLog := filepath.Join(tempDir, "upgrade.log")
	writeFile(t, liveManifest, workerManifest("safe:sha256-old", "true"))
	candidate := strings.Replace(
		workerManifest("unsafe:sha256-new", "false"),
		"spec:\n  template:",
		"spec:\n  replicas: 2\n  template:",
		1,
	)
	writeFile(t, candidateManifest, candidate)

	writeExecutable(t, filepath.Join(binDir, "helm"), `#!/bin/bash
set -euo pipefail
case "$1 $2" in
  "history selfservice") printf '%s\n' '- app_version: test' '  revision: 128' '  status: deployed'; exit 0 ;;
  "dependency build") exit 0 ;;
  "get values") printf '{}\n'; exit 0 ;;
  "get manifest") cat "$FAKE_LIVE_MANIFEST"; exit 0 ;;
  "template selfservice") cat "$FAKE_CANDIDATE_MANIFEST"; exit 0 ;;
  "upgrade selfservice") printf 'upgrade\n' >> "$FAKE_UPGRADE_LOG"; exit 0 ;;
esac
echo "unexpected helm invocation: $*" >&2
exit 90
`)
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/bin/bash
set -euo pipefail
case "$*" in
  *WORKER_PROVISIONING_CLAIMS_ENABLED*) printf 'false' ;;
  *containerStatuses*) printf 'docker-pullable://ghcr.io/jmal1/selfservice-provision-worker@sha256:`+strings.Repeat("a", 64)+`' ;;
  *) echo "unexpected kubectl invocation: $*" >&2; exit 90 ;;
esac
`)

	script := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	cmd := exec.Command(
		"bash",
		script,
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		baselineChart,
	)
	cmd.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG=/dev/null",
		"FAKE_LIVE_MANIFEST="+liveManifest,
		"FAKE_CANDIDATE_MANIFEST="+candidateManifest,
		"FAKE_UPGRADE_LOG="+upgradeLog,
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("worker image drift unexpectedly passed baseline render assertion:\n%s", output)
	}
	if !strings.Contains(string(output), "beyond disabling claims") {
		t.Fatalf("output %q does not identify worker drift", output)
	}
	if body, readErr := os.ReadFile(upgradeLog); readErr == nil && len(body) > 0 {
		t.Fatalf("baseline invoked helm upgrade despite worker drift: %s", body)
	}
}

func workerManifest(image, claims string) string {
	return `---
# Source: selfservice/templates/worker-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
spec:
  template:
    spec:
      containers:
        - name: provision-worker
          image: "` + image + `"
          env:
            - name: WORKER_PROVISIONING_CLAIMS_ENABLED
              value: "` + claims + `"
`
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
