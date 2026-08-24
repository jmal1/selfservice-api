package provisioning

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestDeployScriptRollbackContainment(t *testing.T) {
	requirePOSIXShell(t)

	tests := []struct {
		name              string
		manifest          string
		helmStatus        string
		mismatchContainer string
		args              []string
		wantSuccess       bool
		wantOutput        string
	}{
		{
			name:        "all workload pinned success",
			manifest:    baselineManifest(true, "", "false"),
			helmStatus:  "deployed",
			args:        []string{"--verify-rollback-containment"},
			wantSuccess: true,
			wantOutput:  "pins every rendered workload image",
		},
		{
			name:       "non-worker floating image",
			manifest:   baselineManifest(true, "api-gateway", "false"),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "mutable or non-sha256 image",
		},
		{
			name:       "missing image inventory",
			manifest:   baselineManifest(false, "", "false"),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "missing image inventory",
		},
		{
			name:              "effective digest mismatch",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "deployed",
			mismatchContainer: "api-gateway",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "digest mismatch",
		},
		{
			name:       "claims enabled rollback target",
			manifest:   baselineManifest(true, "", "true"),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "renders worker provisioning claims",
		},
		{
			name:       "failed latest revision",
			manifest:   baselineManifest(true, "", "false"),
			helmStatus: "failed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "status is failed",
		},
		{
			name:       "standard deploy gates before git",
			manifest:   baselineManifest(true, "api-gateway", "false"),
			helmStatus: "deployed",
			wantOutput: "mutable or non-sha256 image",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, test.manifest, test.manifest)
			env.helmStatus = test.helmStatus
			env.mismatchContainer = test.mismatchContainer
			output, err := env.run(test.args...)
			if test.wantSuccess && err != nil {
				t.Fatalf("deploy script failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("sabotaged rollback containment unexpectedly passed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			if test.name == "standard deploy gates before git" {
				if body, readErr := os.ReadFile(env.gitLog); readErr == nil && len(body) > 0 {
					t.Fatalf("git ran before rollback containment passed: %s", body)
				}
			}
		})
	}
}

func TestDeployScriptPreparesAllWorkloadBaseline(t *testing.T) {
	requirePOSIXShell(t)

	for _, test := range []struct {
		name        string
		live        string
		wantSuccess bool
		wantOutput  string
	}{
		{
			name:        "rollout annotation drift with digest equivalence",
			live:        withAPIRolloutAnnotation(baselineManifest(true, "*", "false")),
			wantSuccess: true,
			wantOutput:  "immutable all-workload baseline complete",
		},
		{
			name:       "substantive pod template drift",
			live:       withAPISubstantiveDrift(baselineManifest(true, "*", "false")),
			wantOutput: "spec drifts from the server-defaulted safe chart",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := baselineManifest(true, "*", "false")
			env := newDeployScriptEnvironment(t, test.live, candidate)
			output, err := env.run(
				"--prepare-claims-baseline",
				"--baseline-chart-dir",
				env.chartDir,
			)
			if test.wantSuccess && err != nil {
				t.Fatalf("baseline preparation failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("sabotaged baseline preparation unexpectedly passed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			upgradeBody, readErr := os.ReadFile(env.upgradeLog)
			if test.wantSuccess {
				if readErr != nil || !strings.Contains(string(upgradeBody), "upgrade") {
					t.Fatalf("successful baseline did not invoke helm upgrade: err=%v body=%q", readErr, upgradeBody)
				}
				assertManifestImagesPinned(t, env.baselineManifest)
				if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
					t.Fatalf("successful baseline left the Helm release lock behind: %v", statErr)
				}
			} else if readErr == nil && len(upgradeBody) > 0 {
				t.Fatalf("baseline invoked helm upgrade despite substantive drift: %s", upgradeBody)
			}
		})
	}
}

func TestDeployScriptRejectsConcurrentReleaseMutation(t *testing.T) {
	requirePOSIXShell(t)
	manifest := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, manifest, manifest)
	writeFile(t, env.lockFile, "another-operator")

	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	if err == nil {
		t.Fatalf("baseline preparation ignored the existing release lock:\n%s", output)
	}
	if !strings.Contains(string(output), "Helm release lock") {
		t.Fatalf("output %q does not identify the release lock", output)
	}
	if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
		t.Fatalf("baseline invoked helm upgrade while another holder owned the lock: %s", body)
	}
}

type deployScriptEnvironment struct {
	t                 *testing.T
	binDir            string
	chartDir          string
	liveManifest      string
	candidateManifest string
	baselineManifest  string
	upgradedMarker    string
	upgradeLog        string
	gitLog            string
	lockFile          string
	helmStatus        string
	mismatchContainer string
}

func newDeployScriptEnvironment(t *testing.T, live, candidate string) *deployScriptEnvironment {
	t.Helper()
	root := t.TempDir()
	env := &deployScriptEnvironment{
		t:                 t,
		binDir:            filepath.Join(root, "bin"),
		chartDir:          filepath.Join(root, "safe-chart"),
		liveManifest:      filepath.Join(root, "live.yaml"),
		candidateManifest: filepath.Join(root, "candidate.yaml"),
		baselineManifest:  filepath.Join(root, "baseline.yaml"),
		upgradedMarker:    filepath.Join(root, "upgraded"),
		upgradeLog:        filepath.Join(root, "upgrade.log"),
		gitLog:            filepath.Join(root, "git.log"),
		lockFile:          filepath.Join(root, "helm.lock"),
		helmStatus:        "deployed",
	}
	if err := os.MkdirAll(env.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.chartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.chartDir, "Chart.yaml"), "apiVersion: v2\nname: selfservice\nversion: 0.1.0\n")
	writeFile(t, env.liveManifest, live)
	writeFile(t, env.candidateManifest, candidate)
	env.writeCommands()
	return env
}

func (e *deployScriptEnvironment) writeCommands() {
	e.t.Helper()
	writeExecutable(e.t, filepath.Join(e.binDir, "helm"), `#!/bin/bash
set -euo pipefail
case "$1 $2" in
  "history selfservice")
    revision=130
    [ -f "$FAKE_UPGRADED_MARKER" ] && revision=131
    printf '%s\n' '- app_version: test' "  revision: $revision" "  status: $FAKE_HELM_STATUS"
    ;;
  "get manifest")
    if [ -f "$FAKE_UPGRADED_MARKER" ]; then
      cat "$FAKE_BASELINE_MANIFEST"
    else
      cat "$FAKE_LIVE_MANIFEST"
    fi
    ;;
  "get values")
    printf '{}\n'
    ;;
  "dependency build")
    ;;
  "template selfservice")
    cat "$FAKE_CANDIDATE_MANIFEST"
    ;;
  "upgrade selfservice")
    post_renderer=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--post-renderer" ]; then
        post_renderer=$2
        break
      fi
      shift
    done
    [ -n "$post_renderer" ]
    "$post_renderer" < "$FAKE_CANDIDATE_MANIFEST" > "$FAKE_BASELINE_MANIFEST"
    printf 'upgrade\n' >> "$FAKE_UPGRADE_LOG"
    : > "$FAKE_UPGRADED_MARKER"
    ;;
  *)
    echo "unexpected helm invocation: $*" >&2
    exit 90
    ;;
esac
`)

	writeExecutable(e.t, filepath.Join(e.binDir, "kubectl"), `#!/bin/bash
set -euo pipefail

current_manifest() {
  if [ -f "$FAKE_UPGRADED_MARKER" ]; then
    printf '%s' "$FAKE_BASELINE_MANIFEST"
  else
    printf '%s' "$FAKE_LIVE_MANIFEST"
  fi
}

extract_resource() {
  local manifest=$1
  local wanted=${2#*/}
  local wanted_kind=${2%%/*}
  awk -v wanted_kind="$wanted_kind" -v wanted="$wanted" '
    function flush() {
      if (kind == wanted_kind && name == wanted) {
        printf "%s", doc
        matches++
      }
    }
    function reset() {
      doc = ""
      kind = ""
      name = ""
      metadata = 0
    }
    BEGIN { reset() }
    /^---[[:space:]]*$/ { flush(); reset() }
    { doc = doc $0 ORS }
    /^kind:[[:space:]]*/ {
      kind = $0
      sub(/^kind:[[:space:]]*/, "", kind)
    }
    /^metadata:[[:space:]]*$/ { metadata = 1 }
    metadata && /^  name:[[:space:]]*/ {
      name = $0
      sub(/^  name:[[:space:]]*/, "", name)
      metadata = 0
    }
    END {
      flush()
      if (matches != 1) exit 3
    }
  ' "$manifest"
}

canonical_resource() {
  awk '
    /^spec:[[:space:]]*$/ { spec = 1 }
    spec {
      line = $0
      if (line ~ /^[[:space:]]*(-[[:space:]]*)?image:[[:space:]]*"/) {
        sub(/image:[[:space:]]*"/, "image: ", line)
        sub(/"[[:space:]]*$/, "", line)
      }
      print line
    }
  ' "$1"
}

image_for_container() {
  local container=$1
  local repository
  case "$container" in
    warmer) repository=registry.example/runner ;;
    api-gateway) repository=registry.example/api ;;
    provision-worker) repository=registry.example/worker ;;
    engine) repository=registry.example/engine ;;
    ui) repository=registry.example/ui ;;
    synthetic-api-monitor) repository=registry.example/synthetic ;;
    extra) repository=registry.example/extra ;;
    *) echo "unknown fake container $container" >&2; exit 91 ;;
  esac
  local digest=$FAKE_DIGEST_A
  [ "$container" = "$FAKE_MISMATCH_CONTAINER" ] && digest=$FAKE_DIGEST_B
  printf 'docker-pullable://%s@sha256:%s\n' "$repository" "$digest"
}

case "$1" in
  get)
    if [[ "$2" == configmap/* ]]; then
      if [ ! -f "$FAKE_LOCK_FILE" ]; then
        exit 1
      fi
      if [[ "$*" == *"-o yaml"* ]]; then
        printf 'holder: %s\n' "$(cat "$FAKE_LOCK_FILE")"
      else
        cat "$FAKE_LOCK_FILE"
      fi
    elif [[ "$*" == *"app.kubernetes.io/name=postgresql,app.kubernetes.io/instance=selfservice"* ]]; then
      printf 'selfservice-postgresql-0'
    elif [ "$2" = "jobs" ]; then
      printf 'selfservice-synthetic-api-monitor-1\n'
    elif [[ "$2" == job/* ]]; then
      printf '1 0 0'
    elif [ "$2" = "pods" ] && [[ "$*" == *".imageID"* ]]; then
      for container in warmer api-gateway provision-worker engine ui synthetic-api-monitor extra; do
        if [[ "$*" == *"@.name==\"$container\""* ]]; then
          image_for_container "$container"
          exit 0
        fi
      done
      echo "missing fake image status for: $*" >&2
      exit 92
    elif [[ "$2" == */* ]]; then
      resource_file=$(mktemp)
      extract_resource "$(current_manifest)" "$2" > "$resource_file"
      if [[ "$*" == *"-o yaml"* ]]; then
        cat "$resource_file"
      elif [[ "$*" == *"-o go-template="* ]]; then
        canonical_resource "$resource_file"
      else
        echo "unexpected kubectl resource output: $*" >&2
        rm -f "$resource_file"
        exit 94
      fi
      rm -f "$resource_file"
    else
      echo "unexpected kubectl get invocation: $*" >&2
      exit 90
    fi
    ;;
  rollout)
    ;;
  exec)
    printf '35:false\n'
    ;;
  create)
    if [[ "$*" == *"--dry-run=server"* ]]; then
      manifest=
      while [ "$#" -gt 0 ]; do
        if [ "$1" = "-f" ]; then
          manifest=$2
          break
        fi
        shift
      done
      [ -n "$manifest" ]
      canonical_resource "$manifest"
    else
      [ ! -f "$FAKE_LOCK_FILE" ] || exit 1
      body=$(cat)
      holder=$(printf '%s\n' "$body" | sed -n 's/.*crucible.jmal.io\/holder: "\(.*\)"/\1/p')
      [ -n "$holder" ]
      printf '%s' "$holder" > "$FAKE_LOCK_FILE"
    fi
    ;;
  delete)
    rm -f "$FAKE_LOCK_FILE"
    ;;
  set)
    ;;
  *)
    echo "unexpected kubectl invocation: $*" >&2
    exit 90
    ;;
esac
`)

	writeExecutable(e.t, filepath.Join(e.binDir, "git"), `#!/bin/bash
printf '%s\n' "$*" >> "$FAKE_GIT_LOG"
exit 91
`)
}

func (e *deployScriptEnvironment) run(args ...string) ([]byte, error) {
	e.t.Helper()
	script := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(
		os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG=/dev/null",
		"FAKE_LIVE_MANIFEST="+e.liveManifest,
		"FAKE_CANDIDATE_MANIFEST="+e.candidateManifest,
		"FAKE_BASELINE_MANIFEST="+e.baselineManifest,
		"FAKE_UPGRADED_MARKER="+e.upgradedMarker,
		"FAKE_UPGRADE_LOG="+e.upgradeLog,
		"FAKE_GIT_LOG="+e.gitLog,
		"FAKE_LOCK_FILE="+e.lockFile,
		"FAKE_HELM_STATUS="+e.helmStatus,
		"FAKE_MISMATCH_CONTAINER="+e.mismatchContainer,
		"FAKE_DIGEST_A="+testDigestA,
		"FAKE_DIGEST_B="+testDigestB,
	)
	return cmd.CombinedOutput()
}

func baselineManifest(includeEngineImage bool, floatingContainer, claims string) string {
	image := func(container, repository string) string {
		if floatingContainer == "*" || container == floatingContainer {
			return repository + ":latest"
		}
		return repository + "@sha256:" + testDigestA
	}
	engineImage := ""
	if includeEngineImage {
		engineImage = "        image: " + image("engine", "registry.example/engine") + "\n"
	}
	runnerImage := image("warmer", "registry.example/runner")
	return `---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-api
spec:
  replicas: 1
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
      serviceAccountName: selfservice-api
      containers:
      - name: api-gateway
        image: ` + image("api-gateway", "registry.example/api") + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
spec:
  replicas: 1
  selector:
    matchLabels:
      app: worker
  template:
    metadata:
      labels:
        app: worker
    spec:
      serviceAccountName: selfservice-worker
      containers:
      - name: provision-worker
        image: ` + image("provision-worker", "registry.example/worker") + `
        env:
        - name: WORKER_PROVISIONING_CLAIMS_ENABLED
          value: "` + claims + `"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-engine
spec:
  replicas: 1
  selector:
    matchLabels:
      app: engine
  template:
    metadata:
      labels:
        app: engine
    spec:
      serviceAccountName: selfservice-engine
      containers:
      - name: engine
` + engineImage + `        env:
        - name: RUNNER_IMAGE
          value: "` + runnerImage + `"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-ui
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ui
  template:
    metadata:
      labels:
        app: ui
    spec:
      containers:
      - name: ui
        image: ` + image("ui", "registry.example/ui") + `
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-api-monitor
spec:
  schedule: "*/10 * * * *"
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            app: synthetic
        spec:
          restartPolicy: Never
          containers:
          - name: synthetic-api-monitor
            image: ` + image("synthetic-api-monitor", "registry.example/synthetic") + `
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: selfservice-runner-image-warmer
spec:
  selector:
    matchLabels:
      app: runner
  template:
    metadata:
      labels:
        app: runner
    spec:
      containers:
      - name: warmer
        image: ` + runnerImage + `
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-extra
spec:
  replicas: 1
  selector:
    matchLabels:
      app: extra
  template:
    metadata:
      labels:
        app: extra
    spec:
      containers:
      - image: ` + image("extra", "registry.example/extra") + `
        name: extra
`
}

func withAPIRolloutAnnotation(manifest string) string {
	const before = `    metadata:
      labels:
        app: api
    spec:`
	const after = `    metadata:
      labels:
        app: api
      annotations:
        kubectl.kubernetes.io/restartedAt: "2026-08-23T00:00:00Z"
    spec:`
	return strings.Replace(manifest, before, after, 1)
}

func withAPISubstantiveDrift(manifest string) string {
	const before = `        image: registry.example/api:latest
`
	const after = `        image: registry.example/api:latest
        env:
        - name: UNSAFE_LIVE_ONLY_DRIFT
          value: "true"
`
	return strings.Replace(manifest, before, after, 1)
}

func assertManifestImagesPinned(t *testing.T, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, repository := range []string{
		"registry.example/api",
		"registry.example/worker",
		"registry.example/engine",
		"registry.example/ui",
		"registry.example/synthetic",
		"registry.example/runner",
		"registry.example/extra",
	} {
		if !strings.Contains(text, repository+"@sha256:"+testDigestA) {
			t.Fatalf("baseline does not pin %s: %s", repository, text)
		}
	}
	if strings.Contains(text, ":latest") {
		t.Fatalf("baseline still contains a floating image: %s", text)
	}
}

func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("deploy script semantics require a POSIX shell")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
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
