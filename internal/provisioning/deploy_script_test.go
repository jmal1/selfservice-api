package provisioning

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testSourceSHA = "cccccccccccccccccccccccccccccccccccccccc"
const otherSourceSHA = "dddddddddddddddddddddddddddddddddddddddd"
const testUISourceSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
const testSyntheticUserID = "9f68fd44-60dc-4a59-b16f-6670c33971e5"
const testSyntheticOIDCSub = "f4ed39c8c1bcbd12aac73c64ef58a0a753014ba9b13f7d4f313c99a0511f5a0e"
const testHelmRevision = 163

func TestDeployScriptRollbackContainment(t *testing.T) {
	requirePOSIXShell(t)

	const baselineRevision = testHelmRevision

	tests := []struct {
		name              string
		manifest          string
		helmStatus        string
		helmRevision      int
		mismatchContainer string
		args              []string
		wantSuccess       bool
		wantOutput        string
		configureRevision int
		liveStatus        bool
		liveResource      string
		configure         func(*deployScriptEnvironment)
	}{
		{
			name:              "pending latest revision",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "pending-upgrade",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "status is pending-upgrade",
			configureRevision: baselineRevision,
		},
		{
			name:              "live worker status replicas ignored",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantSuccess:       true,
			wantOutput:        "pins every rendered workload image",
			liveStatus:        true,
			configureRevision: baselineRevision + 1,
		},
		{
			name:     "historical synthetic values use contained live state",
			manifest: rollbackManifestWithHistoricalSynthetics(false),
			liveResource: replaceEnvValue(
				rollbackManifestWithHistoricalSynthetics(true),
				"SYNTHETIC_LIFECYCLE_ENABLED",
				"false",
				"true",
			),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantSuccess:       true,
			wantOutput:        "pins every rendered workload image",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "non-worker floating image",
			manifest:          baselineManifest(true, "api-gateway", "false"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "mutable or non-sha256 image",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "missing image inventory",
			manifest:          baselineManifest(false, "", "false"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "missing image inventory",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "effective digest mismatch",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "deployed",
			mismatchContainer: "api-gateway",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "digest mismatch",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "claims enabled rollback target",
			manifest:          baselineManifest(true, "", "true"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "renders worker provisioning claims",
			configureRevision: baselineRevision + 1,
		},
		{
			name: "content filter enabled rollback target",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"WORKER_CONTENT_FILTER_ENABLED",
				"false",
				"true",
			),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "WORKER_CONTENT_FILTER_ENABLED=false",
			configureRevision: baselineRevision + 1,
		},
		{
			name: "content filter feed in rollback target",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
				"",
				"https://student-filter-feed.lab.jmal.io",
			),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "category feed empty",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "live API monitor suspended",
			manifest:          replaceSyntheticSuspend(baselineManifest(true, "", "false"), true),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "API monitor",
			configureRevision: baselineRevision + 1,
		},
		{
			name: "matching enabled API monitor lifecycle",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"SYNTHETIC_LIFECYCLE_ENABLED",
				"false",
				"true",
			),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantSuccess:       true,
			wantOutput:        "rollback containment verified",
			configureRevision: baselineRevision + 1,
		},
		{
			name: "live API monitor lifecycle differs from rollback target",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"SYNTHETIC_LIFECYCLE_ENABLED",
				"false",
				"true",
			),
			liveResource:      baselineManifest(true, "", "false"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "live SYNTHETIC_LIFECYCLE_ENABLED=false differs from the Helm rollback target value true",
			configureRevision: baselineRevision + 1,
		},
		{
			name:     "live API monitor lifecycle enabled outside rollback target",
			manifest: baselineManifest(true, "", "false"),
			liveResource: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"SYNTHETIC_LIFECYCLE_ENABLED",
				"false",
				"true",
			),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "live SYNTHETIC_LIFECYCLE_ENABLED=true differs from the Helm rollback target value false",
			configureRevision: baselineRevision + 1,
		},
		{
			name:              "failed latest revision",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "failed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "status is failed",
			configureRevision: baselineRevision + 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, test.manifest, test.manifest)
			env.helmStatus = test.helmStatus
			if test.configureRevision != 0 {
				env.helmRevision = test.configureRevision
			}
			if test.configure != nil {
				test.configure(env)
			}
			env.mismatchContainer = test.mismatchContainer
			if test.liveStatus {
				writeFile(t, env.liveResource, withWorkerStatus(test.manifest))
			}
			if test.liveResource != "" {
				writeFile(t, env.liveResource, test.liveResource)
			}
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
		})
	}
}

func suspendedCronJobManifest(suspend bool, image string, duplicateContainer bool) string {
	duplicate := ""
	if duplicateContainer {
		duplicate = `
          - name: synthetic-runner
            image: ` + image
	}
	return `apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-runner
spec:
  schedule: "17 * * * *"
  suspend: ` + strconv.FormatBool(suspend) + `
  jobTemplate:
    spec:
      ttlSecondsAfterFinished: 1800
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: synthetic-runner
            image: ` + image + duplicate + `
`
}

func runLiveEffectiveImageHarness(
	t *testing.T,
	deploySource string,
	manifest string,
	declaredImage string,
	expectedImage string,
	retainedJob bool,
	failJobLookup bool,
	runtimeImageID string,
) ([]byte, error) {
	t.Helper()
	requirePOSIXShell(t)

	functions := []string{
		"is_digest_image",
		"manifest_workload_inventory",
		"latest_cronjob_job",
		"cronjob_suspend_from_manifest",
		"live_effective_image",
	}
	var bodies strings.Builder
	for _, name := range functions {
		body, _, _, err := extractFunctionBody(deploySource, name)
		if err != nil {
			t.Fatal(err)
		}
		bodies.WriteString(body)
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/bin/bash
set -euo pipefail
if [ "$1 $2" = "get jobs" ]; then
  if [ "$FAKE_FAIL_JOB_LOOKUP" = true ]; then
    echo "sabotaged retained Job lookup failure" >&2
    exit 97
  fi
  if [ "$FAKE_RETAINED_JOB" = true ]; then
    printf 'selfservice-synthetic-runner-12345\n'
  fi
  exit 0
fi
if [ "$1 $2" = "get pods" ]; then
  printf '%s\n' "$FAKE_RUNTIME_IMAGE_ID"
  exit 0
fi
echo "unexpected kubectl invocation: $*" >&2
exit 90
`)
	manifestPath := filepath.Join(root, "cronjob.yaml")
	writeFile(t, manifestPath, manifest)
	harnessPath := filepath.Join(root, "harness.sh")
	writeExecutable(t, harnessPath, `#!/bin/bash
set -euo pipefail
NAMESPACE=selfservice
`+bodies.String()+`
actual="$(
  live_effective_image \
    CronJob \
    selfservice-synthetic-runner \
    containers \
    synthetic-runner \
    "$DECLARED_IMAGE" \
    "$CRONJOB_MANIFEST"
)"
if [ "$actual" != "$EXPECTED_IMAGE" ]; then
  echo "ERROR: digest mismatch for CronJob/selfservice-synthetic-runner containers/synthetic-runner: effective image $actual, pinned baseline $EXPECTED_IMAGE." >&2
  exit 1
fi
printf '%s' "$actual"
`)

	cmd := exec.Command("bash", harnessPath)
	cmd.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CRONJOB_MANIFEST="+manifestPath,
		"DECLARED_IMAGE="+declaredImage,
		"EXPECTED_IMAGE="+expectedImage,
		"FAKE_RETAINED_JOB="+strconv.FormatBool(retainedJob),
		"FAKE_FAIL_JOB_LOOKUP="+strconv.FormatBool(failJobLookup),
		"FAKE_RUNTIME_IMAGE_ID="+runtimeImageID,
	)
	return cmd.CombinedOutput()
}

func runCronJobHealthLookupHarness(t *testing.T, deploySource string, suspended bool, retainedJob bool, failJobLookup bool) ([]byte, error) {
	t.Helper()
	requirePOSIXShell(t)

	var bodies strings.Builder
	for _, name := range []string{"latest_cronjob_job", "workload_health"} {
		body, _, _, err := extractFunctionBody(deploySource, name)
		if err != nil {
			t.Fatal(err)
		}
		bodies.WriteString(body)
	}

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(binDir, "kubectl"), `#!/bin/bash
set -euo pipefail
if [ "$1 $2" = "get jobs" ]; then
  if [ "$FAKE_FAIL_JOB_LOOKUP" = true ]; then
    echo "sabotaged retained Job lookup failure" >&2
    exit 97
  fi
  if [ "$FAKE_RETAINED_JOB" = true ]; then
    printf 'selfservice-synthetic-runner-12345\n'
  fi
  exit 0
fi
if [ "$1 $2" = "get CronJob/selfservice-synthetic-runner" ]; then
  printf '%s' "$FAKE_CRONJOB_SUSPENDED"
  exit 0
fi
echo "unexpected kubectl invocation: $*" >&2
exit 90
`)
	inventoryPath := filepath.Join(root, "inventory.tsv")
	writeFile(t, inventoryPath, "CronJob\tselfservice-synthetic-runner\tcontainers\tsynthetic-runner\timage\n")
	harnessPath := filepath.Join(root, "harness.sh")
	writeExecutable(t, harnessPath, `#!/bin/bash
set -euo pipefail
NAMESPACE=selfservice
`+bodies.String()+`
workload_health "$INVENTORY"
`)

	cmd := exec.Command("bash", harnessPath)
	cmd.Env = append(
		os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"INVENTORY="+inventoryPath,
		"FAKE_CRONJOB_SUSPENDED="+strconv.FormatBool(suspended),
		"FAKE_RETAINED_JOB="+strconv.FormatBool(retainedJob),
		"FAKE_FAIL_JOB_LOOKUP="+strconv.FormatBool(failJobLookup),
	)
	return cmd.CombinedOutput()
}

func TestDeployScriptSuspendedCronJobEffectiveImageProof(t *testing.T) {
	deployBody, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(deployBody)
	imageA := "ghcr.io/jmal1/selfservice-crucible-runner@sha256:" + testDigestA
	imageB := "ghcr.io/jmal1/selfservice-crucible-runner@sha256:" + testDigestB
	runtimeImageB := "docker-pullable://" + imageB

	for _, test := range []struct {
		name               string
		manifest           string
		declaredImage      string
		expectedImage      string
		retainedJob        bool
		failJobLookup      bool
		runtimeImageID     string
		wantSuccess        bool
		wantOutputContains string
	}{
		{
			name:           "suspended without retained Job uses matching digest-pinned live spec",
			manifest:       suspendedCronJobManifest(true, imageA, false),
			declaredImage:  imageA,
			expectedImage:  imageA,
			wantSuccess:    true,
			runtimeImageID: runtimeImageB,
		},
		{
			name:               "retained Job lookup failure cannot use suspended spec fallback",
			manifest:           suspendedCronJobManifest(true, imageA, false),
			declaredImage:      imageA,
			expectedImage:      imageA,
			failJobLookup:      true,
			wantOutputContains: "failed to list retained Jobs for CronJob/selfservice-synthetic-runner",
		},
		{
			name:               "runnable without retained Job fails closed",
			manifest:           suspendedCronJobManifest(false, imageA, false),
			declaredImage:      imageA,
			expectedImage:      imageA,
			wantOutputContains: "runnable CronJob/selfservice-synthetic-runner has no retained Job",
		},
		{
			name:               "suspended floating spec image fails closed",
			manifest:           suspendedCronJobManifest(true, "ghcr.io/jmal1/selfservice-crucible-runner:latest", false),
			declaredImage:      "ghcr.io/jmal1/selfservice-crucible-runner:latest",
			expectedImage:      "ghcr.io/jmal1/selfservice-crucible-runner:latest",
			wantOutputContains: "does not declare an immutable sha256 image",
		},
		{
			name:               "suspended spec digest mismatch is rejected by desired-live comparison",
			manifest:           suspendedCronJobManifest(true, imageB, false),
			declaredImage:      imageB,
			expectedImage:      imageA,
			wantOutputContains: "digest mismatch for CronJob/selfservice-synthetic-runner",
		},
		{
			name:           "retained runnable Job still proves the observed runtime ImageID",
			manifest:       suspendedCronJobManifest(false, imageA, false),
			declaredImage:  imageA,
			expectedImage:  imageB,
			retainedJob:    true,
			runtimeImageID: runtimeImageB,
			wantSuccess:    true,
		},
		{
			name:               "suspended spec requires exactly one matching container tuple",
			manifest:           suspendedCronJobManifest(true, imageA, true),
			declaredImage:      imageA,
			expectedImage:      imageA,
			wantOutputContains: "does not contain exactly one live containers/synthetic-runner tuple",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, runErr := runLiveEffectiveImageHarness(
				t,
				source,
				test.manifest,
				test.declaredImage,
				test.expectedImage,
				test.retainedJob,
				test.failJobLookup,
				test.runtimeImageID,
			)
			if test.wantSuccess && runErr != nil {
				t.Fatalf("effective image proof failed: %v\n%s", runErr, output)
			}
			if !test.wantSuccess && runErr == nil {
				t.Fatalf("effective image proof unexpectedly succeeded:\n%s", output)
			}
			if test.wantSuccess && string(output) != test.expectedImage {
				t.Fatalf("effective image = %q, want %q", output, test.expectedImage)
			}
			if test.wantOutputContains != "" && !strings.Contains(string(output), test.wantOutputContains) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutputContains)
			}
		})
	}
}

func TestDeployScriptCronJobHealthJobLookupGuardIsLoadBearing(t *testing.T) {
	deployBody, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(deployBody)

	output, runErr := runCronJobHealthLookupHarness(t, source, true, false, false)
	if runErr != nil {
		t.Fatalf("suspended CronJob health short-circuit failed: %v\n%s", runErr, output)
	}
	sabotaged := strings.Replace(source, `  if [ "$suspended" = true ]; then`, `  if false; then`, 1)
	if sabotaged == source {
		t.Fatal("health lookup sabotage mutation had no effect")
	}
	output, runErr = runCronJobHealthLookupHarness(t, sabotaged, true, false, false)
	if runErr == nil || !strings.Contains(string(output), "runnable CronJob/selfservice-synthetic-runner has no retained Job health evidence") {
		t.Fatalf("health lookup guard did not fail for the suspended short-circuit reason: err=%v\n%s", runErr, output)
	}
}

func TestDeployScriptSuspendedCronJobFallbackGuardsAreLoadBearing(t *testing.T) {
	deployBody, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(deployBody)
	image := "ghcr.io/jmal1/selfservice-crucible-runner@sha256:" + testDigestA

	for _, test := range []struct {
		name          string
		old           string
		replacement   string
		manifest      string
		declared      string
		expected      string
		failJobLookup bool
		wantOriginal  string
	}{
		{
			name: "retained Job lookup status",
			old: `if ! job_name="$(latest_cronjob_job "$name")"; then
        echo "ERROR: failed to list retained Jobs for CronJob/$name while proving its effective image." >&2
        return 1
      fi`,
			replacement:   `job_name="$(latest_cronjob_job "$name")" || true`,
			manifest:      suspendedCronJobManifest(true, image, false),
			declared:      image,
			expected:      image,
			failJobLookup: true,
			wantOriginal:  "failed to list retained Jobs for CronJob/selfservice-synthetic-runner",
		},
		{
			name:         "suspended-only predicate",
			old:          `if [ "$suspended" != true ]; then`,
			replacement:  `if false; then`,
			manifest:     suspendedCronJobManifest(false, image, false),
			declared:     image,
			expected:     image,
			wantOriginal: "runnable CronJob/selfservice-synthetic-runner has no retained Job",
		},
		{
			name:         "immutable digest check",
			old:          `if ! is_digest_image "$spec_image"; then`,
			replacement:  `if false; then`,
			manifest:     suspendedCronJobManifest(true, "ghcr.io/jmal1/selfservice-crucible-runner:latest", false),
			declared:     "ghcr.io/jmal1/selfservice-crucible-runner:latest",
			expected:     "ghcr.io/jmal1/selfservice-crucible-runner:latest",
			wantOriginal: "does not declare an immutable sha256 image",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalOutput, originalErr := runLiveEffectiveImageHarness(
				t,
				source,
				test.manifest,
				test.declared,
				test.expected,
				false,
				test.failJobLookup,
				"",
			)
			if originalErr == nil || !strings.Contains(string(originalOutput), test.wantOriginal) {
				t.Fatalf("original guard did not fail for the expected reason: err=%v\n%s", originalErr, originalOutput)
			}
			if strings.Count(source, test.old) != 1 {
				t.Fatalf("sabotage target %q count != 1", test.old)
			}
			sabotaged := strings.Replace(source, test.old, test.replacement, 1)
			sabotagedOutput, sabotagedErr := runLiveEffectiveImageHarness(
				t,
				sabotaged,
				test.manifest,
				test.declared,
				test.expected,
				false,
				test.failJobLookup,
				"",
			)
			if sabotagedErr != nil {
				t.Fatalf("removing the %s did not expose false acceptance: %v\n%s", test.name, sabotagedErr, sabotagedOutput)
			}
			if !strings.HasSuffix(strings.TrimSpace(string(sabotagedOutput)), test.expected) {
				t.Fatalf("sabotaged proof returned %q, want false acceptance of %q", sabotagedOutput, test.expected)
			}
		})
	}
}

func TestDeployScriptRollbackContainmentAllowsSuspendedCronJobsWithoutRetainedJobs(t *testing.T) {
	requirePOSIXShell(t)

	manifest := rollbackManifestWithHistoricalSynthetics(true)
	env := newDeployScriptEnvironment(t, manifest, manifest)
	env.helmRevision = 164
	env.noRetainedJanitorJob = true
	env.noRetainedRunnerJob = true

	output, err := env.run("--verify-rollback-containment")
	if err != nil {
		t.Fatalf("rollback containment rejected suspended CronJobs without retained Jobs: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "rollback containment verified") {
		t.Fatalf("rollback containment did not reach success:\n%s", output)
	}

	runnable := rollbackManifestWithHistoricalSynthetics(false)
	env = newDeployScriptEnvironment(t, runnable, runnable)
	env.helmRevision = 164
	env.noRetainedRunnerJob = true

	output, err = env.run("--verify-rollback-containment")
	if err == nil {
		t.Fatalf("rollback containment accepted runnable CronJob without retained Job:\n%s", output)
	}
	if !strings.Contains(string(output), "runnable CronJob/selfservice-synthetic-runner has no retained Job") {
		t.Fatalf("failure did not preserve runnable CronJob evidence requirement:\n%s", output)
	}
}

func TestDeployScriptRollbackContainmentFinalRevisionFence(t *testing.T) {
	requirePOSIXShell(t)

	const baselineRevision = 163

	for _, test := range []struct {
		name          string
		finalRevision int
		finalStatus   string
		historyExit   int
		wantOutput    string
	}{
		{
			name:          "revision advances during live verification",
			finalRevision: baselineRevision + 1,
			finalStatus:   "deployed",
			wantOutput:    fmt.Sprintf("expected %d deployed, found %d deployed", baselineRevision, baselineRevision+1),
		},
		{
			name:        "status changes during live verification",
			finalStatus: "pending-upgrade",
			wantOutput:  fmt.Sprintf("expected %d deployed, found %d pending-upgrade", baselineRevision, baselineRevision),
		},
		{
			name:        "final history emits valid record then fails",
			historyExit: 7,
			wantOutput:  "could not re-read the latest Helm revision",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := baselineManifest(true, "", "false")
			env := newDeployScriptEnvironment(t, manifest, manifest)
			env.helmRevision = baselineRevision
			env.postLiveHelmRevision = test.finalRevision
			env.postLiveHelmStatus = test.finalStatus
			env.postLiveHelmHistoryExit = test.historyExit

			output, err := env.run("--verify-rollback-containment")
			if err == nil {
				t.Fatalf("rollback containment ignored concurrent Helm %s drift:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			if _, statErr := os.Stat(env.liveVerificationMark); statErr != nil {
				t.Fatalf("fake Helm drift occurred before live verification began: %v", statErr)
			}
		})
	}
}

func TestDeployScriptRollbackContainmentFinalRevisionFenceLoadBearing(t *testing.T) {
	requirePOSIXShell(t)

	const baselineRevision = 163

	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const finalFence = `  if ! require_helm_revision_still_deployed "$revision"; then
    return 1
  fi
`
	if strings.Count(string(deployBody), finalFence) != 1 {
		t.Fatal("final post-live Helm revision fence is not unique")
	}
	sabotagedBody := strings.Replace(string(deployBody), finalFence, "", 1)
	scriptDir := filepath.Dir(deployPath)
	sabotagedPath := filepath.Join(scriptDir, "deploy-sabotaged-post-live-revision-fence-test.sh")
	writeExecutable(t, sabotagedPath, sabotagedBody)
	t.Cleanup(func() { os.Remove(sabotagedPath) })

	manifest := baselineManifest(true, "", "false")
	env := newDeployScriptEnvironment(t, manifest, manifest)
	env.helmRevision = baselineRevision
	env.postLiveHelmRevision = baselineRevision + 1
	env.postLiveHelmStatus = "deployed"
	env.scriptPath = sabotagedPath

	output, runErr := env.run("--verify-rollback-containment")
	if runErr != nil {
		t.Fatalf("removing the final revision fence did not expose false acceptance: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), fmt.Sprintf("rollback containment verified: deployed revision %d", baselineRevision)) {
		t.Fatalf("removing the final revision fence did not reach false success:\n%s", output)
	}
	if _, statErr := os.Stat(env.liveVerificationMark); statErr != nil {
		t.Fatalf("fake Helm drift occurred before live verification began: %v", statErr)
	}
}

func TestDeployScriptRollbackContainmentFinalRevisionReadStatusLoadBearing(t *testing.T) {
	requirePOSIXShell(t)

	const baselineRevision = 163

	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const checkedRead = `  if ! latest_record="$(latest_helm_revision_record)"; then
    echo "ERROR: could not re-read the latest Helm revision after live rollback containment verification." >&2
    return 1
  fi
`
	const uncheckedRead = `  latest_record="$(latest_helm_revision_record)" || true
`
	if strings.Count(string(deployBody), checkedRead) != 1 {
		t.Fatal("final post-live Helm history status check is not unique")
	}
	sabotagedBody := strings.Replace(string(deployBody), checkedRead, uncheckedRead, 1)
	scriptDir := filepath.Dir(deployPath)
	sabotagedPath := filepath.Join(scriptDir, "deploy-sabotaged-post-live-history-status-test.sh")
	writeExecutable(t, sabotagedPath, sabotagedBody)
	t.Cleanup(func() { os.Remove(sabotagedPath) })

	manifest := baselineManifest(true, "", "false")
	env := newDeployScriptEnvironment(t, manifest, manifest)
	env.helmRevision = baselineRevision
	env.postLiveHelmHistoryExit = 7
	env.scriptPath = sabotagedPath

	output, runErr := env.run("--verify-rollback-containment")
	if runErr != nil {
		t.Fatalf("removing the final history status check did not expose false acceptance: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), fmt.Sprintf("rollback containment verified: deployed revision %d", baselineRevision)) {
		t.Fatalf("removing the final history status check did not reach false success:\n%s", output)
	}
}

func TestDeployScriptImmutableCandidate(t *testing.T) {
	requirePOSIXShell(t)

	live := baselineManifest(true, "", "false")
	for _, test := range []struct {
		name              string
		transform         func(string) string
		configure         func(*deployScriptEnvironment)
		wantSuccess       bool
		wantOutput        string
		wantUpgrade       bool
		expectBuiltDigest bool
	}{
		{
			name:              "exact candidate apply",
			transform:         func(manifest string) string { return baselineManifest(true, "*", "false") },
			wantSuccess:       true,
			wantOutput:        "deployed exact source",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
		{
			name:      "raw Buildx record artifact",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.uiBuildRecordRaw = true
			},
			wantSuccess:       true,
			wantOutput:        "deployed exact source",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
		{
			name: "canonicalizes Docker Hub aliases",
			transform: func(manifest string) string {
				return replaceExtraRepository(baselineManifest(true, "*", "false"), "busybox")
			},
			configure: func(env *deployScriptEnvironment) {
				qualified := replaceExtraRepository(
					baselineManifest(true, "", "false"),
					"docker.io/library/busybox",
				)
				writeFile(t, env.liveManifest, qualified)
				writeFile(t, env.liveResource, qualified)
				env.extraRepository = "docker.io/library/busybox"
			},
			wantSuccess:       true,
			wantOutput:        "deployed exact source",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
		{
			name:      "pauses temporary live claims override",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
			},
			wantSuccess:       true,
			wantOutput:        "pausing live worker provisioning claims",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
		{
			name:      "active durable job blocks upgrade",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.activeJobs = 1
			},
			wantOutput:  "durable jobs remain claimed",
			wantUpgrade: false,
		},
		{
			name:      "active Kubernetes job blocks upgrade",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.activeKubernetesJobs = 1
			},
			wantOutput:  "Kubernetes Jobs remain active",
			wantUpgrade: false,
		},
		{
			name:        "floating package tag",
			transform:   func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure:   func(env *deployScriptEnvironment) { env.packageTag = "latest" },
			wantOutput:  "full source commit",
			wantUpgrade: false,
		},
		{
			name:        "digest belongs to another commit",
			transform:   func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure:   func(env *deployScriptEnvironment) { env.packageTag = otherSourceSHA },
			wantOutput:  "full source commit",
			wantUpgrade: false,
		},
		{
			name:      "digest revision belongs to another commit",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.imageRevision = otherSourceSHA
			},
			wantOutput:  "declares revision",
			wantUpgrade: false,
		},
		{
			name:      "digest differs from workflow artifact",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.runArtifactDigest = testDigestA
			},
			wantOutput:  "differs from workflow artifact digest",
			wantUpgrade: false,
		},
		{
			name:      "digest has conflicting commit tag",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.packageAdditionalTag = otherSourceSHA
			},
			wantOutput:  "conflicting full commit tag",
			wantUpgrade: false,
		},
		{
			name:      "unverified UI commit",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.uiCommitVerified = false
			},
			wantOutput:  "UI source commit",
			wantUpgrade: false,
		},
		{
			name:      "missing UI run artifact",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.uiBuildRecordMissing = true
			},
			wantOutput:  "exactly one unexpired Buildx build record",
			wantUpgrade: false,
		},
		{
			name:      "UI artifact belongs to another commit",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.uiArtifactSHA = otherSourceSHA
			},
			wantOutput:  "UI build record is not bound",
			wantUpgrade: false,
		},
		{
			name:      "UI OCI revision belongs to another commit",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.uiImageRevision = otherSourceSHA
			},
			wantOutput:  "declares revision",
			wantUpgrade: false,
		},
		{
			name:      "external live image drifts",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.externalDriftAfterServerDryRun = true
			},
			wantOutput:  "external live image drifted",
			wantUpgrade: false,
		},
		{
			name:      "final server dry run fails",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.failFinalServerDryRun = true
			},
			wantOutput:  "server-side dry-run",
			wantUpgrade: false,
		},
		{
			name:      "final server object mutation",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				env.mutateFinalServerObject = true
			},
			wantOutput:  "server-defaulted candidate object set changed",
			wantUpgrade: false,
		},
		{
			name:      "floating upgrade-only hook",
			transform: func(manifest string) string { return baselineManifest(true, "*", "false") },
			configure: func(env *deployScriptEnvironment) {
				writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway:latest"))
			},
			wantOutput:  "Helm hook uses a mutable or wrong image",
			wantUpgrade: false,
		},
		{
			name: "claims intent changed",
			transform: func(manifest string) string {
				return replaceEnvValue(
					baselineManifest(true, "*", "false"),
					"WORKER_PROVISIONING_CLAIMS_ENABLED",
					"false",
					"true",
				)
			},
			wantOutput:  "provisioning claims",
			wantUpgrade: false,
		},
		{
			name: "content filter intent changed",
			transform: func(manifest string) string {
				return replaceEnvValue(
					baselineManifest(true, "*", "false"),
					"WORKER_CONTENT_FILTER_ENABLED",
					"false",
					"true",
				)
			},
			wantOutput:  "WORKER_CONTENT_FILTER_ENABLED=false",
			wantUpgrade: false,
		},
		{
			name: "content filter feed changed",
			transform: func(manifest string) string {
				return replaceEnvValue(
					baselineManifest(true, "*", "false"),
					"WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
					"",
					"https://student-filter-feed.lab.jmal.io",
				)
			},
			wantOutput:  "category feed empty",
			wantUpgrade: false,
		},
		{
			name: "API monitor suspension changed",
			transform: func(manifest string) string {
				return replaceSyntheticSuspend(manifest, true)
			},
			wantOutput:  "unsuspended",
			wantUpgrade: false,
		},
		{
			name: "candidate can intentionally enable API monitor lifecycle",
			transform: func(manifest string) string {
				return replaceEnvValue(
					manifest,
					"SYNTHETIC_LIFECYCLE_ENABLED",
					"false",
					"true",
				)
			},
			wantSuccess:       true,
			wantOutput:        "deployed exact source",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
		{
			name:              "candidate can restore missing synthetic producer CronJobs",
			transform:         func(manifest string) string { return rollbackManifestWithHistoricalSynthetics(false) },
			wantSuccess:       true,
			wantOutput:        "deployed exact source",
			wantUpgrade:       true,
			expectBuiltDigest: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := test.transform(live)
			env := newDeployScriptEnvironment(t, live, candidate)
			if test.configure != nil {
				test.configure(env)
			}
			output, err := env.run("--no-pull")
			if test.wantSuccess && err != nil {
				t.Fatalf("immutable candidate deploy failed: %v\n%s", err, output)
			}
			if !test.wantSuccess && err == nil {
				t.Fatalf("sabotaged immutable candidate unexpectedly passed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			upgradeBody, readErr := os.ReadFile(env.upgradeLog)
			if test.wantUpgrade {
				if readErr != nil || !strings.Contains(string(upgradeBody), "--atomic") {
					t.Fatalf("successful candidate did not use atomic Helm upgrade: err=%v body=%q", readErr, upgradeBody)
				}
				serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
				if serverReadErr != nil || countLogicalServerValidations(t, serverDryRuns) != 2 {
					t.Fatalf("candidate was not server-validated before and under the apply lock: err=%v log=%q", serverReadErr, serverDryRuns)
				}
				applied, appliedErr := os.ReadFile(env.appliedManifest)
				if appliedErr != nil {
					t.Fatalf("successful candidate did not persist applied manifest: %v", appliedErr)
				}
				assertManifestImagesPinned(t, env.appliedManifest, testDigestB, testDigestB, env.extraRepository)
				if strings.Contains(string(applied), ":latest") {
					t.Fatalf("final candidate retained a floating image:\n%s", applied)
				}
				if _, statErr := os.Stat(env.cronjobVerifyMark); statErr != nil {
					t.Fatalf("successful candidate did not prove the deployed CronJob image with a fresh Job: %v", statErr)
				}
				if test.expectBuiltDigest {
					if !strings.Contains(string(applied), "selfservice-api-gateway@sha256:"+testDigestB) {
						t.Fatalf("candidate did not use commit-bound API digest:\n%s", applied)
					}
					if !strings.Contains(string(applied), "selfservice-ui@sha256:"+testDigestB) {
						t.Fatalf("candidate did not use run-bound UI digest:\n%s", applied)
					}
				}
				if test.name == "pauses temporary live claims override" {
					if _, statErr := os.Stat(env.claimsPausedMark); statErr != nil {
						t.Fatalf("deploy did not deliberately pause the live claims override: %v", statErr)
					}
					lockIndex := strings.Index(string(output), "acquired Helm release lock")
					containmentIndex := -1
					pauseIndex := -1
					if lockIndex >= 0 {
						if relative := strings.Index(string(output)[lockIndex:], "rollback containment verified"); relative >= 0 {
							containmentIndex = lockIndex + relative
						}
					}
					if lockIndex >= 0 {
						if relative := strings.Index(string(output)[lockIndex:], "pausing live worker provisioning claims"); relative >= 0 {
							pauseIndex = lockIndex + relative
						}
					}
					if lockIndex < 0 || pauseIndex <= lockIndex || containmentIndex <= pauseIndex {
						t.Fatalf("claims pause did not precede dynamic rollback-containment proof:\n%s", output)
					}
				}
			} else if readErr == nil && len(upgradeBody) > 0 {
				t.Fatalf("sabotaged candidate invoked Helm upgrade: %s", upgradeBody)
			}
			if test.name == "external live image drifts" {
				if _, statErr := os.Stat(env.finalRollbackMark); statErr != nil {
					t.Fatalf("external drift sabotage did not reach the post-rollback recheck: %v", statErr)
				}
			}
			if test.name == "final server dry run fails" {
				serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
				if serverReadErr != nil || countLogicalServerValidations(t, serverDryRuns) != 2 {
					t.Fatalf("server sabotage did not reach the final under-lock dry-run: err=%v log=%q", serverReadErr, serverDryRuns)
				}
			}
			if test.name == "final server object mutation" {
				serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
				if serverReadErr != nil || countLogicalServerValidations(t, serverDryRuns) != 2 {
					t.Fatalf("server mutation did not reach the final canonical comparison: err=%v log=%q", serverReadErr, serverDryRuns)
				}
			}
		})
	}
}

func TestDeployScriptAtomicContainmentAndSuccessVerification(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	t.Run("successful upgrade accepts intended janitor TTL deletion", func(t *testing.T) {
		candidate := rollbackManifestWithHistoricalSynthetics(true)
		liveWithHistoricalSynthetics := baselineManifest(true, "", "false") +
			historicalSyntheticCronJobs(true)
		liveWithJanitorTTL := strings.Replace(
			liveWithHistoricalSynthetics,
			"  name: selfservice-synthetic-janitor\nspec:\n  suspend: true\n  jobTemplate:\n    spec:\n",
			"  name: selfservice-synthetic-janitor\nspec:\n  suspend: true\n  jobTemplate:\n    spec:\n      ttlSecondsAfterFinished: 1800\n",
			1,
		)
		if liveWithJanitorTTL == liveWithHistoricalSynthetics {
			t.Fatal("janitor TTL fixture was not added to the live manifest")
		}
		env := newDeployScriptEnvironment(t, liveWithJanitorTTL, candidate)
		env.retainLiveJanitorTTLBeforeUpgrade = true

		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("intended janitor TTL deletion failed post-apply comparison: %v\n%s", err, output)
		}
		if _, statErr := os.Stat(env.upgradedMarker); statErr != nil {
			t.Fatalf("TTL deletion regression did not reach Helm upgrade: %v\n%s", statErr, output)
		}
		if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
			t.Fatalf("successful TTL deletion left the release lock behind: %v\n%s", statErr, output)
		}
		ssaLog, readErr := os.ReadFile(env.janitorTTLSSALog)
		if readErr != nil {
			t.Fatalf("could not read janitor TTL SSA evidence: %v", readErr)
		}
		if !strings.Contains(string(ssaLog), "pre-upgrade-retained") ||
			!strings.Contains(string(ssaLog), "post-upgrade-omitted") {
			t.Fatalf("fake SSA did not model both sides of the production deletion race:\n%s", ssaLog)
		}
	})

	for _, test := range []struct {
		name                   string
		configure              func(*deployScriptEnvironment)
		wantOutput             string
		wantLockRetained       bool
		wantSuccessfulRevision bool
	}{
		{
			name: "contained atomic failure",
			configure: func(env *deployScriptEnvironment) {
				env.failAtomicUpgrade = true
				writeFile(t, env.liveManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.liveResource, replaceEnvValue(
					rollbackManifestWithHistoricalSynthetics(true),
					"SYNTHETIC_LIFECYCLE_ENABLED",
					"false",
					"true",
				))
				writeFile(t, env.atomicRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.containedRollbackManifest, rollbackManifestWithHistoricalSynthetics(true))
				writeFile(t, env.immutableRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
			},
			wantOutput: "atomic failure contained by the currently deployed rollback baseline",
		},
		{
			name: "pending synthetic pod destroy after rollback retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.failAtomicUpgrade = true
				env.pendingSyntheticJobs = 1
				writeFile(t, env.liveManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.liveResource, replaceEnvValue(
					rollbackManifestWithHistoricalSynthetics(true),
					"SYNTHETIC_LIFECYCLE_ENABLED",
					"false",
					"true",
				))
				writeFile(t, env.atomicRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.containedRollbackManifest, rollbackManifestWithHistoricalSynthetics(true))
				writeFile(t, env.immutableRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
			},
			wantOutput:       "nonterminal storage-mutating synthetic pod",
			wantLockRetained: true,
		},
		{
			name: "atomic rollback image drift retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.failAtomicUpgrade = true
				env.atomicRollbackMismatch = "api-gateway"
			},
			wantOutput:       "manual intervention is required",
			wantLockRetained: true,
		},
		{
			name: "successful upgrade live image mismatch retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.postUpgradeMismatch = "api-gateway"
			},
			wantOutput:             "candidate live image drifted",
			wantLockRetained:       true,
			wantSuccessfulRevision: true,
		},
		{
			name: "successful upgrade runner mismatch retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.postUpgradeMismatch = "warmer"
			},
			wantOutput:             "candidate live image drifted",
			wantLockRetained:       true,
			wantSuccessfulRevision: true,
		},
		{
			name: "successful upgrade object mismatch retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.postUpgradeObjectMutation = true
			},
			wantOutput:             "deployed Helm object set differs",
			wantLockRetained:       true,
			wantSuccessfulRevision: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			test.configure(env)
			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("sabotaged atomic outcome unexpectedly succeeded:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			upgradeBody, readErr := os.ReadFile(env.upgradeLog)
			if readErr != nil || !strings.Contains(string(upgradeBody), "--atomic") {
				t.Fatalf("sabotage did not reach atomic Helm upgrade: err=%v body=%q", readErr, upgradeBody)
			}
			_, lockErr := os.Stat(env.lockFile)
			if test.wantLockRetained && lockErr != nil {
				t.Fatalf("failed containment did not retain release lock: %v", lockErr)
			}
			if !test.wantLockRetained && !os.IsNotExist(lockErr) {
				t.Fatalf("proven atomic rollback did not release lock: %v", lockErr)
			}
			if strings.Contains(test.name, "synthetic") || test.name == "contained atomic failure" {
				if _, statErr := os.Stat(env.syntheticContainedMark); statErr != nil {
					t.Fatalf("atomic rollback did not enforce synthetic containment: %v", statErr)
				}
				if _, statErr := os.Stat(env.claimsPausedMark); statErr != nil {
					t.Fatalf("atomic rollback did not retain claims-disabled state: %v", statErr)
				}
			}
			_, upgradedErr := os.Stat(env.upgradedMarker)
			if test.wantSuccessfulRevision && upgradedErr != nil {
				t.Fatalf("success-verification sabotage did not reach deployed revision: %v", upgradedErr)
			}
		})
	}
}

func TestDeployScriptPostUpgradeActiveJobContainment(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")

	t.Run("contained API monitor is permitted", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.postUpgradeActiveKubernetesJob = "api-monitor-contained"

		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("contained API monitor caused false post-upgrade failure: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("contained API monitor did not reach successful deployment:\n%s", output)
		}
		if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
			t.Fatalf("contained API monitor fixture did not reach post-upgrade containment: %v", statErr)
		}
		if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
			t.Fatalf("successful contained API monitor deployment retained the release lock: %v", statErr)
		}
	})

	for _, test := range []struct {
		name string
		job  string
	}{
		{name: "runner remains blocking", job: "runner"},
		{name: "janitor remains blocking", job: "janitor"},
		{name: "provisioning remains blocking", job: "provisioning"},
		{name: "unknown name remains blocking", job: "unknown"},
		{name: "spoofed API monitor remains blocking", job: "api-monitor-wrong-owner"},
		{name: "lifecycle-enabled API monitor remains blocking", job: "api-monitor-lifecycle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, candidate)
			env.postUpgradeActiveKubernetesJob = test.job

			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("active %s Job unexpectedly passed post-upgrade containment:\n%s", test.job, output)
			}
			if !strings.Contains(string(output), "Kubernetes Jobs remain active") {
				t.Fatalf("active %s Job failure did not identify the job drain:\n%s", test.job, output)
			}
			if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
				t.Fatalf("active %s Job fixture did not reach post-upgrade containment: %v", test.job, statErr)
			}
			if _, statErr := os.Stat(env.lockFile); statErr != nil {
				t.Fatalf("active %s Job failure did not retain the release lock: %v", test.job, statErr)
			}
		})
	}
}

func TestDeployScriptPostUpgradeActiveJobGuardIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const guard = `  if [ -n "$active_kubernetes_jobs" ]; then
    echo "ERROR: Kubernetes Jobs remain active in namespace $NAMESPACE: $active_kubernetes_jobs." >&2
    return 1
  fi
`
	if strings.Count(string(deployBody), guard) != 1 {
		t.Fatal("post-upgrade active Kubernetes Job guard is not unique")
	}
	sabotagedBody := strings.Replace(string(deployBody), guard, "", 1)
	sabotagedPath := filepath.Join(
		filepath.Dir(deployPath),
		"deploy-sabotaged-active-job-guard-test.sh",
	)
	writeExecutable(t, sabotagedPath, sabotagedBody)
	t.Cleanup(func() { os.Remove(sabotagedPath) })

	live := baselineManifest(true, "", "false")
	env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
	env.postUpgradeActiveKubernetesJob = "provisioning"
	env.scriptPath = sabotagedPath

	output, runErr := env.run("--no-pull")
	if runErr != nil {
		t.Fatalf("removing the active Job guard did not expose false acceptance: %v\n%s", runErr, output)
	}
	if !strings.Contains(string(output), "deployed exact source") {
		t.Fatalf("removing the active Job guard did not reach false success:\n%s", output)
	}
	if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
		t.Fatalf("active Job sabotage did not reach post-upgrade containment: %v", statErr)
	}
	if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
		t.Fatalf("sabotaged active Job guard did not falsely release the lock: %v", statErr)
	}
}

func TestDeployScriptCandidateCronJobsHonorSuspensionBeforeJobCreation(t *testing.T) {
	requirePOSIXShell(t)
	live := rollbackManifestWithHistoricalSynthetics(true)
	env := newDeployScriptEnvironment(t, live, live)
	writeFile(t, env.liveResource, replaceEnvValue(
		live,
		"WORKER_PROVISIONING_CLAIMS_ENABLED",
		"false",
		"true",
	))
	env.noRetainedJanitorJob = true
	env.noRetainedRunnerJob = true

	output, err := env.run("--no-pull")
	if err != nil {
		t.Fatalf("contained candidate verification failed: %v\n%s", err, output)
	}
	if _, statErr := os.Stat(env.claimsPausedMark); statErr != nil {
		t.Fatalf("verification did not run while worker claims were paused: %v", statErr)
	}
	logBody, readErr := os.ReadFile(env.cronjobCreateLog)
	if readErr != nil {
		t.Fatalf("read CronJob create log: %v", readErr)
	}
	lines := strings.Split(strings.TrimSpace(string(logBody)), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("CronJob create log = %q, want exactly one verification Job creation", logBody)
	}
	if !strings.HasPrefix(lines[0], "selfservice-synthetic-api-monitor-deploy-verify-") {
		t.Fatalf("CronJob create log %q did not record only the API monitor verification Job", logBody)
	}
	jobBody, readErr := os.ReadFile(env.cronjobVerifyManifest)
	if readErr != nil {
		t.Fatalf("read created verification Job: %v", readErr)
	}
	var job struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
						Env  []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jobBody, &job); err != nil {
		t.Fatalf("decode created verification Job: %v", err)
	}
	var lifecycleValues []string
	for _, container := range job.Spec.Template.Spec.Containers {
		if container.Name != "synthetic-api-monitor" {
			continue
		}
		for _, envVar := range container.Env {
			if envVar.Name == "SYNTHETIC_LIFECYCLE_ENABLED" {
				lifecycleValues = append(lifecycleValues, envVar.Value)
			}
		}
	}
	if len(lifecycleValues) != 1 || lifecycleValues[0] != "false" {
		t.Fatalf("verification Job lifecycle values = %v, want exactly [false]", lifecycleValues)
	}
	if strings.Contains(string(logBody), "selfservice-synthetic-runner") || strings.Contains(string(logBody), "selfservice-synthetic-janitor") {
		t.Fatalf("suspended runner/janitor must not create verification Jobs: %q", logBody)
	}
	applied, readErr := os.ReadFile(env.appliedManifest)
	if readErr != nil {
		t.Fatalf("read applied candidate manifest: %v", readErr)
	}
	if got := manifestEnvValue(t, string(applied), "SYNTHETIC_LIFECYCLE_ENABLED"); got != "false" {
		t.Fatalf("candidate CronJob lifecycle = %q, want false", got)
	}
	if got := manifestEnvValue(t, string(applied), "PROVISIONING_ENABLED"); got != "false" {
		t.Fatalf("candidate API provisioning enabled = %q, want false", got)
	}
	if got := manifestEnvValue(t, string(applied), "SYNTHETIC_PROVISIONING_EXPECTED_ENABLED"); got != "false" {
		t.Fatalf("candidate CronJob provisioning expectation = %q, want false", got)
	}
}

func TestDeployScriptCandidateCronJobLifecycleOverrideIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	sourcePath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	const override = `                  [{name:"SYNTHETIC_LIFECYCLE_ENABLED",value:"false"}]`
	if strings.Count(string(source), override) != 1 {
		t.Fatalf("expected exactly one verification Job lifecycle override")
	}
	sabotaged := strings.Replace(
		string(source),
		override,
		`                  [{name:"SYNTHETIC_LIFECYCLE_ENABLED",value:"true"}]`,
		1,
	)
	scriptPath := filepath.Join(
		"..", "..", "deploy", "scripts",
		"deploy-sabotaged-verification-lifecycle-test.sh",
	)
	writeFile(t, scriptPath, sabotaged)
	t.Cleanup(func() { os.Remove(scriptPath) })
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}

	live := baselineManifest(true, "", "false")
	candidate := replaceEnvValue(
		baselineManifest(true, "*", "false"),
		"SYNTHETIC_LIFECYCLE_ENABLED",
		"false",
		"true",
	)
	env := newDeployScriptEnvironment(t, live, candidate)
	writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
	env.scriptPath = scriptPath
	output, runErr := env.run("--no-pull")
	if runErr == nil {
		t.Fatalf("verification Job without lifecycle containment unexpectedly passed:\n%s", output)
	}
	if !strings.Contains(string(output), "verification Job inherited SYNTHETIC_LIFECYCLE_ENABLED=true while claims were paused") {
		t.Fatalf("sabotage did not fail for the expected lifecycle reason:\n%s", output)
	}
	if _, statErr := os.Stat(env.lockFile); statErr != nil {
		t.Fatalf("sabotaged verification did not retain the release lock: %v", statErr)
	}
}

// TestDeployScriptResumesProvisioningClaimsAfterSuccessfulDeploy pins the
// symmetry of pause_live_provisioning_claims / resume_live_provisioning_claims.
//
// pause_live_provisioning_claims runs unconditionally before every guarded
// apply. Before the resume existed, nothing ever set the flag back, so a live
// worker that had been serving traffic stayed at
// WORKER_PROVISIONING_CLAIMS_ENABLED=false indefinitely after a completely
// successful deploy: it claimed no pod_create/pod_destroy jobs, and because
// cloneSchedulerEnabled() gates the L1 trust-validation reconciler on claims,
// no template_revalidate ran either, so templates.guest_credentials_verified_at
// went stale and every new clone_with_customize pod was rejected as "not
// credential-ready". This silently halted all provisioning for three days while
// every workload reported healthy, which is exactly why a green deploy is not
// sufficient evidence on its own.
//
// The restored value must come from the observed pre-deploy LIVE state, not the
// rendered chart: validate_foundation_intent and the rollback-containment gates
// all require the candidate to render claims=false, so claims are only ever
// enabled operationally on the live Deployment.
//
// The assertion is behavioral, not textual: it runs the real deploy.sh against
// the fake kubectl and inspects the resulting live claims state.
func TestDeployScriptResumesProvisioningClaimsAfterSuccessfulDeploy(t *testing.T) {
	requirePOSIXShell(t)
	candidate := baselineManifest(true, "*", "false")

	t.Run("restores claims that were live before the deploy", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), candidate)
		// Claims are enabled operationally on the live Deployment only; the
		// rendered/rollback manifests must stay claims=false, which is exactly
		// why the resume cannot read its intent from the chart.
		writeFile(t, env.liveResource, baselineManifest(true, "", "true"))

		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("successful deploy failed: %v\n%s", err, output)
		}
		if _, statErr := os.Stat(env.claimsResumedMark); statErr != nil {
			t.Fatalf("successful deploy never restored provisioning claims; the worker would claim no jobs: %v\n%s", statErr, output)
		}
		if !strings.Contains(string(output), "restoring live worker provisioning claims") {
			t.Fatalf("output %q does not report the claims restore", output)
		}
	})

	t.Run("leaves claims paused when they were already paused", func(t *testing.T) {
		// A deploy must not turn provisioning on for a cluster that was
		// deliberately quiesced before the deploy started.
		env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), candidate)

		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("successful deploy failed: %v\n%s", err, output)
		}
		if _, statErr := os.Stat(env.claimsResumedMark); !os.IsNotExist(statErr) {
			t.Fatalf("deploy enabled claims that were paused before it started (stat: %v)\n%s", statErr, output)
		}
	})
}

func TestDeployScriptPreflightFailureCannotMutate(t *testing.T) {
	requirePOSIXShell(t)
	env := newDeployScriptEnvironment(
		t,
		baselineManifest(true, "", "false"),
		baselineManifest(true, "*", "false"),
	)
	env.migrationState = "1:37:true"

	output, err := env.run("--no-pull")
	if err == nil {
		t.Fatalf("dirty migration preflight unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "database migration state is dirty: 37:true") {
		t.Fatalf("preflight failure was not actionable:\n%s", output)
	}
	assertNoPreflightMutation(t, env, output)
}

func TestDeployScriptReleasePreflightGuards(t *testing.T) {
	requirePOSIXShell(t)
	tests := []struct {
		name       string
		configure  func(*deployScriptEnvironment)
		wantOutput string
		wantPass   bool
	}{
		{name: "valid synthetic quota uses UUID despite hashed OIDC subject", wantOutput: "release preflight passed", wantPass: true},
		{
			name: "dirty migration",
			configure: func(env *deployScriptEnvironment) {
				env.migrationState = "1:37:true"
			},
			wantOutput: "database migration state is dirty",
		},
		{
			name: "migration state malformed",
			configure: func(env *deployScriptEnvironment) {
				env.migrationState = "2:38:false"
			},
			wantOutput: "invalid migration state returned by PostgreSQL",
		},
		{
			name: "migration query failure",
			configure: func(env *deployScriptEnvironment) {
				env.preflightQueryFailure = "migration"
			},
			wantOutput: "migration-state query failed",
		},
		{
			name: "pending provisioning job",
			configure: func(env *deployScriptEnvironment) {
				env.provisioningJobs = "1"
			},
			wantOutput: "provisioning jobs are nonterminal or have unknown status",
		},
		{
			name: "unknown provisioning job status",
			configure: func(env *deployScriptEnvironment) {
				env.unknownProvisioningJobStatus = true
			},
			wantOutput: "provisioning jobs are nonterminal or have unknown status",
		},
		{
			name: "provisioning job query failure",
			configure: func(env *deployScriptEnvironment) {
				env.preflightQueryFailure = "provisioning"
			},
			wantOutput: "provisioning-job preflight query failed",
		},
		{
			name: "malformed provisioning job count",
			configure: func(env *deployScriptEnvironment) {
				env.provisioningJobs = "unknown"
			},
			wantOutput: "invalid provisioning-job preflight count",
		},
		{
			name: "synthetic quota exhausted",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticQuotaState = "1:1"
			},
			wantOutput: "synthetic user has no quota for one more pod",
		},
		{
			name: "synthetic quota query failure",
			configure: func(env *deployScriptEnvironment) {
				env.preflightQueryFailure = "quota"
			},
			wantOutput: "synthetic-user quota preflight query failed",
		},
		{
			name: "synthetic user secret missing",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretExists = false
			},
			wantOutput: "failed to read the authoritative synthetic user ID",
		},
		{
			name: "synthetic user secret read failure",
			configure: func(env *deployScriptEnvironment) {
				env.failSyntheticSecretRead = true
			},
			wantOutput: "failed to read the authoritative synthetic user ID",
		},
		{
			name: "synthetic user secret key missing",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretKeyPresent = false
			},
			wantOutput: "missing required key user-id",
		},
		{
			name: "synthetic user secret value malformed base64",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretValue = "not-valid-base64%%%"
			},
			wantOutput: "key user-id is not valid base64",
		},
		{
			name: "synthetic user secret value malformed UUID",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretValue = base64.StdEncoding.EncodeToString([]byte("not-a-uuid"))
			},
			wantOutput: "key user-id is not a valid UUID",
		},
		{
			name: "synthetic user secret UUID has trailing newline",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretValue = base64.StdEncoding.EncodeToString([]byte(testSyntheticUserID + "\n"))
			},
			wantOutput: "key user-id is not a valid UUID",
		},
		{
			name: "synthetic user secret UUID has trailing NUL",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticSecretValue = base64.StdEncoding.EncodeToString(append([]byte(testSyntheticUserID), 0))
			},
			wantOutput: "key user-id is not a valid UUID",
		},
		{
			name: "synthetic user UUID is unknown",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticUserKnown = false
			},
			wantOutput: "quota query returned invalid or missing",
		},
		{
			name: "synthetic quota malformed",
			configure: func(env *deployScriptEnvironment) {
				env.syntheticQuotaState = "missing"
			},
			wantOutput: "quota query returned invalid or missing",
		},
		{
			name: "mutating synthetic active",
			configure: func(env *deployScriptEnvironment) {
				env.activeMutatingSyntheticJobs = 1
			},
			wantOutput: "scheduled mutating synthetic execution is active",
		},
		{
			name: "completed mutating synthetic passes",
			configure: func(env *deployScriptEnvironment) {
				env.completedMutatingSyntheticJob = true
			},
			wantOutput: "release preflight passed",
			wantPass:   true,
		},
		{
			name: "Kubernetes jobs malformed",
			configure: func(env *deployScriptEnvironment) {
				env.malformedJobsJSON = true
			},
			wantOutput: "Kubernetes Job data is malformed",
		},
		{
			name: "Kubernetes jobs query failure",
			configure: func(env *deployScriptEnvironment) {
				env.failJobsList = true
			},
			wantOutput: "failed to list Kubernetes Jobs",
		},
		{
			name: "candidate revision mismatch",
			configure: func(env *deployScriptEnvironment) {
				env.imageRevision = otherSourceSHA
			},
			wantOutput: "not source commit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(
				t,
				baselineManifest(true, "", "false"),
				baselineManifest(true, "*", "false"),
			)
			if test.configure != nil {
				test.configure(env)
			}
			output, err := env.run("--no-pull")
			if test.wantPass {
				if err != nil {
					t.Fatalf("preflight unexpectedly failed: %v\n%s", err, output)
				}
				if _, statErr := os.Stat(env.upgradedMarker); statErr != nil {
					t.Fatalf("passing preflight did not reach Helm upgrade: %v\n%s", statErr, output)
				}
			} else {
				if err == nil {
					t.Fatalf("failing preflight unexpectedly succeeded:\n%s", output)
				}
				assertNoPreflightMutation(t, env, output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			if strings.Contains(string(output), env.syntheticUserID) ||
				strings.Contains(string(output), env.syntheticSecretValue) {
				t.Fatalf("preflight output exposed the synthetic user Secret value:\n%s", output)
			}
		})
	}
}

func assertNoPreflightMutation(t *testing.T, env *deployScriptEnvironment, output []byte) {
	t.Helper()
	for _, marker := range []string{
		env.lockFile,
		env.claimsPausedMark,
		env.claimsResumedMark,
		env.syntheticContainedMark,
		env.candidateAppliedMark,
		env.upgradedMarker,
	} {
		if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
			t.Fatalf("preflight failure wrote mutation marker %s: %v\n%s", marker, statErr, output)
		}
	}
}

func TestDeployScriptVolatileReleasePreflightBlocksLateRegressions(t *testing.T) {
	requirePOSIXShell(t)
	tests := []struct {
		name       string
		configure  func(*deployScriptEnvironment)
		wantOutput string
	}{
		{
			name: "pending provisioning job appears after claims pause",
			configure: func(env *deployScriptEnvironment) {
				env.provisioningJobsAfterInitial = "1"
			},
			wantOutput: "provisioning jobs are nonterminal or have unknown status",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(
				t,
				baselineManifest(true, "", "false"),
				baselineManifest(true, "*", "false"),
			)
			writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
			test.configure(env)

			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("late volatile regression unexpectedly reached Helm:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			for _, marker := range []string{env.claimsPausedMark, env.claimsResumedMark, env.syntheticContainedMark} {
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Fatalf("late guard failure missed expected pause/restore marker %s: %v\n%s", marker, statErr, output)
				}
			}
			if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
				t.Fatalf("late guard failure invoked Helm upgrade: %s\n%s", body, output)
			}
			for _, marker := range []string{env.candidateAppliedMark, env.upgradedMarker, env.lockFile} {
				if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
					t.Fatalf("late guard failure left mutation marker %s: %v\n%s", marker, statErr, output)
				}
			}
		})
	}
}

func TestDeployScriptVolatileReleasePreflightIsOrderedAndLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const boundary = `require_volatile_release_preflight
echo "==> helm upgrade $RELEASE with exact digest-pinned candidate (atomic, timeout=$TIMEOUT)"`
	if strings.Count(string(source), boundary) != 1 {
		t.Fatal("volatile preflight is not directly adjacent to the final Helm upgrade announcement")
	}
	const helper = `require_volatile_release_preflight() {
  require_no_pending_provisioning_jobs
}`
	if strings.Count(string(source), helper) != 1 {
		t.Fatal("volatile preflight helper does not contain the exact two final guards")
	}

	tests := []struct {
		name      string
		guardCall string
		configure func(*deployScriptEnvironment)
	}{
		{
			name:      "provisioning recheck",
			guardCall: "  require_no_pending_provisioning_jobs\n",
			configure: func(env *deployScriptEnvironment) {
				env.provisioningJobsAfterInitial = "1"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutatedHelper := strings.Replace(helper, test.guardCall, "  :\n", 1)
			mutated := strings.Replace(string(source), helper, mutatedHelper, 1)
			scriptPath := filepath.Join(
				filepath.Dir(deployPath),
				"deploy-sabotaged-gate-a4-final-"+strings.ReplaceAll(test.name, " ", "-")+"-test.sh",
			)
			writeExecutable(t, scriptPath, mutated)
			t.Cleanup(func() { os.Remove(scriptPath) })

			env := newDeployScriptEnvironment(
				t,
				baselineManifest(true, "", "false"),
				baselineManifest(true, "*", "false"),
			)
			writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
			env.scriptPath = scriptPath
			test.configure(env)

			output, runErr := env.run("--no-pull")
			if runErr != nil {
				t.Fatalf("removing final %s did not expose the staged regression: %v\n%s", test.name, runErr, output)
			}
			if _, statErr := os.Stat(env.upgradedMarker); statErr != nil {
				t.Fatalf("removing final %s did not reach Helm mutation: %v\n%s", test.name, statErr, output)
			}
			if _, statErr := os.Stat(env.claimsResumedMark); statErr != nil {
				t.Fatalf("sabotaged successful deploy did not restore claims: %v\n%s", statErr, output)
			}
		})
	}
}

func TestDeployScriptReleasePreflightPredicatesAreLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}

	const preflightStart = "run_release_preflight() {\n"
	start := strings.Index(string(source), preflightStart)
	if start < 0 {
		t.Fatal("run_release_preflight definition is missing")
	}
	end := strings.Index(string(source)[start:], "\n}\n")
	if end < 0 {
		t.Fatal("run_release_preflight definition is unterminated")
	}
	end = start + end + len("\n}\n")
	block := string(source)[start:end]

	tests := []struct {
		name      string
		guardCall string
		configure func(*deployScriptEnvironment)
	}{
		{name: "migration", guardCall: "  require_clean_migration\n", configure: func(env *deployScriptEnvironment) { env.migrationState = "1:37:true" }},
		{name: "provisioning jobs", guardCall: "  require_no_pending_provisioning_jobs\n", configure: func(env *deployScriptEnvironment) { env.provisioningJobs = "1" }},
		{name: "synthetic quota", guardCall: "  require_synthetic_pod_quota\n", configure: func(env *deployScriptEnvironment) { env.syntheticQuotaState = "1:1" }},
		{name: "mutating synthetics", guardCall: "  require_no_active_mutating_synthetics\n", configure: func(env *deployScriptEnvironment) { env.activeMutatingSyntheticJobs = 1 }},
		{name: "candidate provenance", guardCall: "  require_candidate_image_provenance\n", configure: func(env *deployScriptEnvironment) { env.imageRevisionDriftAfter = 6 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if strings.Count(block, test.guardCall) != 1 {
				t.Fatalf("preflight guard call %q is not unique", test.guardCall)
			}
			mutatedBlock := strings.Replace(block, test.guardCall, "", 1)
			mutated := strings.Replace(string(source), block, mutatedBlock, 1)
			if test.name == "migration" {
				const underLockGuard = "require_clean_migration\nROLLBACK_BASELINE_MIGRATION="
				if strings.Count(mutated, underLockGuard) != 1 {
					t.Fatal("under-lock migration guard is not unique")
				}
				mutated = strings.Replace(mutated, underLockGuard, "VERIFIED_MIGRATION_STATE=\"$(current_migration_state)\"\nROLLBACK_BASELINE_MIGRATION=", 1)
			}
			scriptPath := filepath.Join(filepath.Dir(deployPath), "deploy-sabotaged-gate-a4-"+strings.ReplaceAll(test.name, " ", "-")+"-test.sh")
			writeExecutable(t, scriptPath, mutated)
			t.Cleanup(func() { os.Remove(scriptPath) })

			env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), baselineManifest(true, "*", "false"))
			writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
			env.scriptPath = scriptPath
			test.configure(env)
			output, _ := env.run("--no-pull")
			if _, statErr := os.Stat(env.claimsPausedMark); statErr != nil {
				t.Fatalf("removing %s guard did not expose live mutation: %v\n%s", test.name, statErr, output)
			}
		})
	}
}

func TestDeployScriptSyntheticQuotaIdentityIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}

	const authoritativeLookup = `kubectl get secret selfservice-synthetic-user -n "$NAMESPACE" \
      -o jsonpath='{.data.user-id}'`
	const uuidPredicate = `WHERE u.id = :'"'"'synthetic_user_id'"'"'::uuid`
	tests := []struct {
		name        string
		original    string
		replacement string
		wantOutput  string
	}{
		{
			name:        "authoritative secret",
			original:    authoritativeLookup,
			replacement: strings.Replace(authoritativeLookup, "selfservice-synthetic-user", "wrong-synthetic-user", 1),
			wantOutput:  "failed to read the authoritative synthetic user ID",
		},
		{
			name:        "authoritative secret key",
			original:    authoritativeLookup,
			replacement: strings.Replace(authoritativeLookup, ".data.user-id", ".data.wrong-user-id", 1),
			wantOutput:  "failed to read the authoritative synthetic user ID",
		},
		{
			name:     "UUID predicate",
			original: uuidPredicate,
			replacement: `WHERE u.oidc_sub = '"'"'synthetic-monitor-no-oidc'"'"'
                AND u.username = '"'"'synthetic'"'"'`,
			wantOutput: "quota query returned invalid or missing",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if strings.Count(string(source), test.original) != 1 {
				t.Fatalf("synthetic quota identity fragment %q is not unique", test.original)
			}
			mutated := strings.Replace(string(source), test.original, test.replacement, 1)
			scriptPath := filepath.Join(
				filepath.Dir(deployPath),
				"deploy-sabotaged-gate-a4-synthetic-identity-"+strings.ReplaceAll(test.name, " ", "-")+"-test.sh",
			)
			writeExecutable(t, scriptPath, mutated)
			t.Cleanup(func() { os.Remove(scriptPath) })

			env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), baselineManifest(true, "*", "false"))
			env.scriptPath = scriptPath
			output, runErr := env.run("--no-pull")
			if runErr == nil || !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("synthetic identity sabotage did not fail closed: %v\n%s", runErr, output)
			}
			assertNoPreflightMutation(t, env, output)
		})
	}
}

func TestDeployScriptMigrationContiguityIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const correct = "local inventory base raw name direction version expected max_version=0"
	const sabotaged = "local inventory base raw name direction version expected max_version=39"
	if strings.Count(string(source), correct) != 1 {
		t.Fatal("migration contiguity sequence initializer is not unique")
	}
	mutated := strings.Replace(string(source), correct, sabotaged, 1)
	scriptPath := filepath.Join(filepath.Dir(deployPath), "deploy-sabotaged-gate-a4-migration-gap-test.sh")
	writeExecutable(t, scriptPath, mutated)
	t.Cleanup(func() { os.Remove(scriptPath) })

	env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), baselineManifest(true, "*", "false"))
	env.scriptPath = scriptPath
	output, err := env.run("--no-pull")
	if err == nil || !strings.Contains(string(output), "migrations are not contiguous") {
		t.Fatalf("migration-gap sabotage did not fail closed: %v\n%s", err, output)
	}
	assertNoPreflightMutation(t, env, output)
}

func TestDeployScriptMigrationPairingIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	const correct = `find "$migration_dir" -maxdepth 1 -type f -name '*.sql' -printf '%f\n' |`
	const sabotaged = `{ find "$migration_dir" -maxdepth 1 -type f -name '*.sql' -printf '%f\n'; printf '000039_orphan.down.sql\n'; } |`
	if strings.Count(string(source), correct) != 1 {
		t.Fatal("migration file enumeration command is not unique")
	}
	mutated := strings.Replace(string(source), correct, sabotaged, 1)
	scriptPath := filepath.Join(filepath.Dir(deployPath), "deploy-sabotaged-gate-a4-migration-pair-test.sh")
	writeExecutable(t, scriptPath, mutated)
	t.Cleanup(func() { os.Remove(scriptPath) })

	env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), baselineManifest(true, "*", "false"))
	env.scriptPath = scriptPath
	output, err := env.run("--no-pull")
	if err == nil || !strings.Contains(string(output), "must have exactly one up and one down file") {
		t.Fatalf("orphan migration sabotage did not fail closed: %v\n%s", err, output)
	}
	assertNoPreflightMutation(t, env, output)
}

func TestDeployScriptRestoresProvisioningClaimsOnEveryPostPauseExit(t *testing.T) {
	requirePOSIXShell(t)
	candidate := baselineManifest(true, "*", "false")

	newLiveClaimsEnvironment := func(t *testing.T) *deployScriptEnvironment {
		t.Helper()
		env := newDeployScriptEnvironment(t, baselineManifest(true, "", "false"), candidate)
		writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
		return env
	}
	exitCode := func(t *testing.T, err error) int {
		t.Helper()
		if err == nil {
			return 0
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("command returned non-exit error: %v", err)
		}
		return exitErr.ExitCode()
	}
	assertRestored := func(t *testing.T, env *deployScriptEnvironment, output []byte) {
		t.Helper()
		if _, err := os.Stat(env.claimsPausedMark); err != nil {
			t.Fatalf("test never reached the real claims pause: %v\n%s", err, output)
		}
		if _, err := os.Stat(env.claimsResumedMark); err != nil {
			t.Fatalf("EXIT cleanup did not restore claims: %v\n%s", err, output)
		}
	}

	t.Run("normal success", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("deploy failed: %v\n%s", err, output)
		}
		assertRestored(t, env, output)
		if _, err := os.Stat(env.lockFile); !os.IsNotExist(err) {
			t.Fatalf("successful restore did not release the lock: %v\n%s", err, output)
		}
	})

	t.Run("early post-pause set-e failure", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.activeJobs = 1
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 1 {
			t.Fatalf("post-pause failure exit code = %d, want 1\n%s", got, output)
		}
		assertRestored(t, env, output)
	})

	t.Run("Helm failure preserves Helm exit code", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.failAtomicUpgrade = true
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 99 {
			t.Fatalf("Helm failure exit code = %d, want 99\n%s", got, output)
		}
		assertRestored(t, env, output)
		if _, err := os.Stat(env.lockFile); !os.IsNotExist(err) {
			t.Fatalf("contained Helm failure did not release the lock after restoration: %v\n%s", err, output)
		}
	})

	t.Run("post-apply comparator failure", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.postUpgradeObjectMutation = true
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 1 {
			t.Fatalf("comparator failure exit code = %d, want 1\n%s", got, output)
		}
		assertRestored(t, env, output)
		if _, err := os.Stat(env.lockFile); err != nil {
			t.Fatalf("comparator failure did not retain its containment lock: %v\n%s", err, output)
		}
	})

	t.Run("resume failure after otherwise successful deploy", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.failClaimsResume = true
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 86 {
			t.Fatalf("resume failure exit code = %d, want 86\n%s", got, output)
		}
		if _, err := os.Stat(env.claimsResumedMark); !os.IsNotExist(err) {
			t.Fatalf("failed resume wrote a success marker: %v\n%s", err, output)
		}
		if _, err := os.Stat(env.lockFile); err != nil {
			t.Fatalf("resume failure did not preserve the lock: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "original exit code: 0, resume exit code: 86") {
			t.Fatalf("resume failure did not log both statuses:\n%s", output)
		}
	})

	t.Run("resume failure preserves an existing failure", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.activeJobs = 1
		env.failClaimsResume = true
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 1 {
			t.Fatalf("existing failure was replaced by resume status: got %d want 1\n%s", got, output)
		}
		if _, err := os.Stat(env.lockFile); err != nil {
			t.Fatalf("resume failure did not preserve the lock: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "original exit code: 1, resume exit code: 86") {
			t.Fatalf("combined failure did not log both statuses:\n%s", output)
		}
	})

	t.Run("dry-run never pauses or resumes", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		output, err := env.run("--no-pull", "--dry-run")
		if err != nil {
			t.Fatalf("dry-run failed: %v\n%s", err, output)
		}
		for _, marker := range []string{env.claimsPausedMark, env.claimsResumedMark} {
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("dry-run mutated claims marker %s: %v\n%s", marker, statErr, output)
			}
		}
	})

	t.Run("pre-pause failure never resumes", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.failServerValidateDocument = "selfservice-worker"
		output, err := env.run("--no-pull")
		if err == nil {
			t.Fatalf("pre-pause sabotage unexpectedly succeeded:\n%s", output)
		}
		for _, marker := range []string{env.claimsPausedMark, env.claimsResumedMark} {
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("pre-pause failure mutated claims marker %s: %v\n%s", marker, statErr, output)
			}
		}
	})

	t.Run("signal after pause mutation but before response", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.signalDuringClaimsPause = "INT"
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 130 {
			t.Fatalf("mid-pause signal exit code = %d, want 130\n%s", got, output)
		}
		assertRestored(t, env, output)
	})

	t.Run("failure before pause mutation causes no restore mutation", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.failClaimsPauseBeforeMutation = true
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 85 {
			t.Fatalf("pre-mutation pause failure exit code = %d, want 85\n%s", got, output)
		}
		for _, marker := range []string{env.claimsPausedMark, env.claimsResumedMark} {
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Fatalf("pre-mutation pause failure wrote marker %s: %v\n%s", marker, statErr, output)
			}
		}
	})

	t.Run("signal during Helm retains containment lock", func(t *testing.T) {
		env := newLiveClaimsEnvironment(t)
		env.signalDuringHelm = "TERM"
		output, err := env.run("--no-pull")
		if got := exitCode(t, err); got != 143 {
			t.Fatalf("during-Helm signal exit code = %d, want 143\n%s", got, output)
		}
		assertRestored(t, env, output)
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("signal during Helm did not retain the unknown-state lock: %v\n%s", statErr, output)
		}
	})

	for _, signal := range []struct {
		name string
		send string
		code int
	}{
		{name: "INT", send: "INT", code: 130},
		{name: "TERM", send: "TERM", code: 143},
		{name: "HUP", send: "HUP", code: 129},
	} {
		t.Run("signal "+signal.name, func(t *testing.T) {
			env := newLiveClaimsEnvironment(t)
			env.signalAfterClaimsPause = signal.send
			output, err := env.run("--no-pull")
			if got := exitCode(t, err); got != signal.code {
				t.Fatalf("%s exit code = %d, want %d\n%s", signal.name, got, signal.code, output)
			}
			assertRestored(t, env, output)
		})
	}
}

func TestDeployScriptClaimsExitRestorationIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	sourcePath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	const restorationHook = `  if [ "$LIVE_CLAIMS_RESTORE_PENDING" = true ]; then`
	if strings.Count(string(source), restorationHook) != 1 {
		t.Fatalf("expected exactly one EXIT restoration hook %q", restorationHook)
	}
	sabotaged := strings.Replace(string(source), restorationHook, "  if false; then", 1)
	scriptPath := filepath.Join(
		"..", "..", "deploy", "scripts",
		"deploy-sabotaged-without-exit-restoration-test.sh",
	)
	writeFile(t, scriptPath, sabotaged)
	t.Cleanup(func() { os.Remove(scriptPath) })
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}

	env := newDeployScriptEnvironment(
		t,
		baselineManifest(true, "", "false"),
		baselineManifest(true, "*", "false"),
	)
	writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
	env.activeJobs = 1
	env.scriptPath = scriptPath
	output, runErr := env.run("--no-pull")
	if runErr == nil {
		t.Fatalf("sabotaged post-pause failure unexpectedly succeeded:\n%s", output)
	}
	if _, err := os.Stat(env.claimsPausedMark); err != nil {
		t.Fatalf("sabotage never reached claims pause: %v\n%s", err, output)
	}
	if _, err := os.Stat(env.claimsResumedMark); !os.IsNotExist(err) {
		t.Fatalf("sabotage unexpectedly restored claims: %v\n%s", err, output)
	}
}

func TestDeployScriptDeployedCandidateWaitsForWarmerRolloutBeforeImageVerification(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")

	t.Run("succeeds after warmer rollout", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("deploy failed despite the warmer rolling out before image verification: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("output %q does not indicate a completed deploy", output)
		}
		if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
			t.Fatalf("successful deploy left the Helm release lock behind: %v", statErr)
		}
		if _, statErr := os.Stat(env.warmerRolloutMark); statErr != nil {
			t.Fatalf("workload health never observed the warmer DaemonSet rollout: %v", statErr)
		}
		probeLog, readErr := os.ReadFile(env.warmerImageProbeLog)
		if readErr != nil {
			t.Fatalf("could not read warmer image probe log: %v", readErr)
		}
		if strings.Contains(string(probeLog), "before") {
			t.Fatalf("warmer ImageID was inspected before rollout convergence:\n%s", probeLog)
		}
		if !strings.Contains(string(probeLog), "after") {
			t.Fatalf("warmer ImageID was not observed after rollout convergence:\n%s", probeLog)
		}
	})

	t.Run("rollout failure retains lock", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.warmerRolloutFailure = true
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, err := env.run("--no-pull")
		if err == nil {
			t.Fatalf("deploy unexpectedly succeeded despite the warmer rollout failure:\n%s", output)
		}
		if !strings.Contains(string(output), "sabotaged rollout status failure for selfservice-runner-image-warmer") {
			t.Fatalf("output did not surface the warmer rollout failure:\n%s", output)
		}
		if !strings.Contains(string(output), "WARNING: preserving Helm release lock") {
			t.Fatalf("rollout failure did not preserve the release lock:\n%s", output)
		}
		if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
			t.Fatalf("rollout failure did not reach the candidate-applied boundary: %v", statErr)
		}
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("rollout failure did not retain the release lock: %v", statErr)
		}
	})

	t.Run("empty warmer image retains lock", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.warmerImageStaysEmpty = true
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, err := env.run("--no-pull")
		if err == nil {
			t.Fatalf("deploy unexpectedly succeeded despite an empty warmer ImageID after rollout:\n%s", output)
		}
		if !strings.Contains(string(output), "missing image inventory") {
			t.Fatalf("output did not report the missing warmer ImageID inventory:\n%s", output)
		}
		if !strings.Contains(string(output), "WARNING: preserving Helm release lock") {
			t.Fatalf("empty warmer ImageID did not preserve the release lock:\n%s", output)
		}
		if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
			t.Fatalf("empty warmer ImageID did not reach the candidate-applied boundary: %v", statErr)
		}
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("empty warmer ImageID did not retain the release lock: %v", statErr)
		}
		probeLog, readErr := os.ReadFile(env.warmerImageProbeLog)
		if readErr != nil {
			t.Fatalf("could not read warmer image probe log: %v", readErr)
		}
		if !strings.Contains(string(probeLog), "after-empty") {
			t.Fatalf("warmer ImageID did not remain empty after rollout:\n%s", probeLog)
		}
	})
}

func revertVerifyDeployedCandidateOrdering(t *testing.T, source string) string {
	t.Helper()
	const fixedBlock = `  workload_health "$inventory"
  workload_health_status=$?
  if [ "$workload_health_status" -ne 0 ]; then
    echo "ERROR: deployed candidate workloads are not healthy." >&2
    return 1
  fi
  if ! verify_external_candidate_images \
      "$CANDIDATE_IMAGE_MAP" \
      "$tmp_dir/live-images" \
      candidate; then
    return 1
  fi
`
	const oldBlock = `  if ! verify_external_candidate_images \
      "$CANDIDATE_IMAGE_MAP" \
      "$tmp_dir/live-images" \
      candidate; then
    return 1
  fi
  if ! workload_health "$inventory"; then
    echo "ERROR: deployed candidate workloads are not healthy." >&2
    return 1
  fi
`
	if !strings.Contains(source, fixedBlock) {
		t.Fatal("could not locate the fixed verify_deployed_candidate ordering")
	}
	return strings.Replace(source, fixedBlock, oldBlock, 1)
}

func TestDeployScriptDeployedCandidateOrderingIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	mutated := revertVerifyDeployedCandidateOrdering(t, string(originalBytes))
	scriptPath := filepath.Join(
		"..", "..", "deploy", "scripts",
		"deploy-sabotaged-deployed-candidate-ordering-test.sh",
	)
	if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(scriptPath) })

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.scriptPath = scriptPath
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, runErr := env.run("--no-pull")
	if runErr == nil {
		t.Fatalf("sabotaged deployed-candidate ordering unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "missing image inventory") {
		t.Fatalf("sabotaged ordering did not reproduce the missing ImageID failure:\n%s", output)
	}
	if _, statErr := os.Stat(env.lockFile); statErr != nil {
		t.Fatalf("sabotaged ordering did not retain the release lock: %v", statErr)
	}
	probeLog, readErr := os.ReadFile(env.warmerImageProbeLog)
	if readErr != nil {
		t.Fatalf("could not read warmer image probe log: %v", readErr)
	}
	if !strings.Contains(string(probeLog), "before") {
		t.Fatalf("sabotaged ordering never queried the warmer ImageID before rollout:\n%s", probeLog)
	}
}

func revertWorkloadHealthPropagation(t *testing.T, source string) string {
	t.Helper()
	const fixedBlock = `      Deployment|DaemonSet|StatefulSet)
        kubectl rollout status "$kind/$name" -n "$NAMESPACE" --timeout=5m || {
          echo "ERROR: $kind/$name did not stabilize during deployed candidate health verification." >&2
          return 1
        }
        ;;
`
	const oldBlock = `      Deployment|DaemonSet|StatefulSet)
        kubectl rollout status "$kind/$name" -n "$NAMESPACE" --timeout=5m
        ;;
`
	if !strings.Contains(source, fixedBlock) {
		t.Fatal("could not locate the fixed workload_health rollout propagation")
	}
	return strings.Replace(source, fixedBlock, oldBlock, 1)
}

func revertFinalDeployedCandidateHealthFence(t *testing.T, source string) string {
	t.Helper()
	const fixedBlock = `  if ! verify_external_candidate_images \
      "$CANDIDATE_IMAGE_MAP" \
      "$tmp_dir/live-images" \
      candidate; then
    return 1
  fi
  workload_health "$inventory"
  workload_health_status=$?
  if [ "$workload_health_status" -ne 0 ]; then
    echo "ERROR: deployed candidate workloads regressed after live image verification." >&2
    return 1
  fi
  if ! require_no_active_jobs; then
    return 1
  fi
`
	const oldBlock = `  if ! verify_external_candidate_images \
      "$CANDIDATE_IMAGE_MAP" \
      "$tmp_dir/live-images" \
      candidate; then
    return 1
  fi
  if ! require_no_active_jobs; then
    return 1
  fi
`
	if !strings.Contains(source, fixedBlock) {
		t.Fatal("could not locate the fixed final workload_health fence")
	}
	return strings.Replace(source, fixedBlock, oldBlock, 1)
}

func TestDeployScriptWorkloadHealthRolloutFailureIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")

	t.Run("rollout failure retains lock", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.warmerRolloutFailure = true
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, err := env.run("--no-pull")
		if err == nil {
			t.Fatalf("deploy unexpectedly succeeded despite the warmer rollout failure:\n%s", output)
		}
		if !strings.Contains(string(output), "sabotaged rollout status failure for selfservice-runner-image-warmer") {
			t.Fatalf("output did not surface the warmer rollout failure:\n%s", output)
		}
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("rollout failure did not retain the release lock: %v", statErr)
		}
		if _, statErr := os.Stat(env.cronjobVerifyMark); !os.IsNotExist(statErr) {
			t.Fatalf("external image verification should not run after a rollout failure: %v", statErr)
		}
	})

	t.Run("sabotaged explicit propagation reaches verification", func(t *testing.T) {
		mutated := revertWorkloadHealthPropagation(t, string(originalBytes))
		scriptPath := filepath.Join(
			"..", "..", "deploy", "scripts",
			"deploy-sabotaged-workload-health-propagation-test.sh",
		)
		if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(scriptPath) })

		env := newDeployScriptEnvironment(t, live, candidate)
		env.warmerRolloutFailure = true
		env.scriptPath = scriptPath
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, runErr := env.run("--no-pull")
		if runErr == nil {
			t.Fatalf("sabotaged workload_health propagation unexpectedly succeeded:\n%s", output)
		}
		if !strings.Contains(string(output), "missing image inventory") {
			t.Fatalf("sabotaged workload_health propagation did not reach external image verification:\n%s", output)
		}
		if _, statErr := os.Stat(env.cronjobVerifyMark); statErr != nil {
			t.Fatalf("sabotaged workload_health propagation did not reach external image verification: %v", statErr)
		}
		if !strings.Contains(string(output), "WARNING: preserving Helm release lock") {
			t.Fatalf("sabotaged workload_health propagation did not preserve the release lock:\n%s", output)
		}
		if _, statErr := os.Stat(env.candidateAppliedMark); statErr != nil {
			t.Fatalf("sabotaged workload_health propagation did not reach the candidate-applied boundary: %v", statErr)
		}
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("sabotaged workload_health propagation did not retain the release lock: %v", statErr)
		}
	})
}

func TestDeployScriptFinalWorkloadHealthFenceIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")

	t.Run("final health regression retains lock", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.finalWorkloadHealthRegression = true
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, err := env.run("--no-pull")
		if err == nil {
			t.Fatalf("deploy unexpectedly succeeded despite the final workload health regression:\n%s", output)
		}
		if !strings.Contains(string(output), "deployed candidate workloads regressed after live image verification") {
			t.Fatalf("output did not surface the final workload health regression:\n%s", output)
		}
		if !strings.Contains(string(output), "WARNING: preserving Helm release lock") {
			t.Fatalf("final workload health regression did not preserve the release lock:\n%s", output)
		}
		if _, statErr := os.Stat(env.cronjobVerifyMark); statErr != nil {
			t.Fatalf("final workload health regression did not reach external image verification: %v", statErr)
		}
	})

	t.Run("sabotaged final health fence succeeds", func(t *testing.T) {
		mutated := revertFinalDeployedCandidateHealthFence(t, string(originalBytes))
		scriptPath := filepath.Join(
			"..", "..", "deploy", "scripts",
			"deploy-sabotaged-final-workload-health-fence-test.sh",
		)
		if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(scriptPath) })

		env := newDeployScriptEnvironment(t, live, candidate)
		env.finalWorkloadHealthRegression = true
		env.scriptPath = scriptPath
		writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

		output, runErr := env.run("--no-pull")
		if runErr != nil {
			t.Fatalf("sabotaged final workload health fence unexpectedly failed:\n%s", output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("sabotaged final workload health fence did not complete the deploy:\n%s", output)
		}
		if _, statErr := os.Stat(env.cronjobVerifyMark); statErr != nil {
			t.Fatalf("sabotaged final workload health fence did not reach external image verification: %v", statErr)
		}
		if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
			t.Fatalf("sabotaged final workload health fence left the release lock behind: %v", statErr)
		}
	})
}

func TestDeployScriptRejectsUntrustedSource(t *testing.T) {
	requirePOSIXShell(t)

	live := baselineManifest(true, "", "false")
	for _, test := range []struct {
		name       string
		configure  func(*deployScriptEnvironment)
		wantOutput string
	}{
		{
			name:       "dirty tree",
			configure:  func(env *deployScriptEnvironment) { env.gitDirty = true },
			wantOutput: "source tree is dirty",
		},
		{
			name:       "detached head",
			configure:  func(env *deployScriptEnvironment) { env.gitBranch = "detached" },
			wantOutput: "detached HEAD",
		},
		{
			name:       "wrong branch",
			configure:  func(env *deployScriptEnvironment) { env.gitBranch = "release" },
			wantOutput: "not main",
		},
		{
			name:       "remote commit mismatch",
			configure:  func(env *deployScriptEnvironment) { env.gitRemoteSHA = otherSourceSHA },
			wantOutput: "does not match origin/main",
		},
		{
			name:       "untrusted remote",
			configure:  func(env *deployScriptEnvironment) { env.gitRemoteURL = "git@github.com:attacker/selfservice-api.git" },
			wantOutput: "not the trusted",
		},
		{
			name:       "unverified commit",
			configure:  func(env *deployScriptEnvironment) { env.commitVerified = false },
			wantOutput: "not verified by GitHub",
		},
		{
			name:       "missing required build",
			configure:  func(env *deployScriptEnvironment) { env.missingBuild = "crucible-runner" },
			wantOutput: "successful image build for crucible-runner",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			test.configure(env)
			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("untrusted source unexpectedly deployed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
				t.Fatalf("untrusted source invoked Helm upgrade: %s", body)
			}
		})
	}
}

func TestDeployScriptRequiresExplicitProvenUICandidate(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")

	for _, test := range []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{
			name:       "missing UI source SHA",
			args:       []string{"--no-pull"},
			wantOutput: "require an explicit --ui-source-sha",
		},
		{
			name:       "wrong UI source SHA",
			args:       []string{"--no-pull", "--ui-source-sha", otherSourceSHA},
			wantOutput: "UI source commit",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			output, err := env.runWithUI(false, test.args...)
			if err == nil {
				t.Fatalf("unproven UI candidate unexpectedly deployed:\n%s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("output %q does not contain %q", output, test.wantOutput)
			}
			if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
				t.Fatalf("unproven UI candidate invoked Helm upgrade: %s", body)
			}
		})
	}
}

func TestDeployScriptDryRunPrintsFinalCandidate(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
	writeFile(t, env.liveResource, baselineManifest(true, "", "true"))
	output, err := env.run("--no-pull", "--dry-run")
	if err != nil {
		t.Fatalf("candidate dry-run failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), ":latest") {
		t.Fatalf("dry-run printed the unsafe unpinned render:\n%s", output)
	}
	if !strings.Contains(string(output), "selfservice-api-gateway@sha256:"+testDigestB) ||
		!strings.Contains(string(output), "selfservice-ui@sha256:"+testDigestB) {
		t.Fatalf("dry-run did not print the final built/external digest selection:\n%s", output)
	}
	if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
		t.Fatalf("dry-run invoked Helm upgrade: %s", body)
	}
	if _, statErr := os.Stat(env.claimsPausedMark); !os.IsNotExist(statErr) {
		t.Fatalf("dry-run mutated the temporary live claims override: %v", statErr)
	}
	serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
	if serverReadErr != nil || countLogicalServerValidations(t, serverDryRuns) != 1 {
		t.Fatalf("dry-run did not server-validate exactly once: err=%v log=%q", serverReadErr, serverDryRuns)
	}
}

func TestDeployScriptPreparesAllWorkloadBaseline(t *testing.T) {
	requirePOSIXShell(t)

	for _, test := range []struct {
		name              string
		live              string
		liveHelmRelease   string
		liveHelmNamespace string
		wantSuccess       bool
		wantOutput        string
	}{
		{
			name:        "expected Helm ownership and deployment revision annotations ignored",
			live:        withAPITopLevelRevisionAnnotation(baselineManifest(true, "*", "false")),
			wantSuccess: true,
			wantOutput:  "immutable all-workload baseline complete",
		},
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
		{
			name:            "wrong Helm release name",
			live:            baselineManifest(true, "*", "false"),
			liveHelmRelease: "other-release",
			wantOutput:      `.metadata.annotations["meta.helm.sh/release-name"] must equal "selfservice"`,
		},
		{
			name:              "wrong Helm release namespace",
			live:              baselineManifest(true, "*", "false"),
			liveHelmNamespace: "other-namespace",
			wantOutput:        `.metadata.annotations["meta.helm.sh/release-namespace"] must equal "selfservice"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := baselineManifest(true, "*", "false")
			env := newDeployScriptEnvironment(t, test.live, candidate)
			if test.liveHelmRelease != "" {
				env.liveHelmRelease = test.liveHelmRelease
			}
			if test.liveHelmNamespace != "" {
				env.liveHelmNamespace = test.liveHelmNamespace
			}
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
				assertManifestImagesPinned(t, env.baselineManifest, testDigestA, testDigestA, env.extraRepository)
				if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
					t.Fatalf("successful baseline left the Helm release lock behind: %v", statErr)
				}
			} else if readErr == nil && len(upgradeBody) > 0 {
				t.Fatalf("baseline invoked helm upgrade despite substantive drift: %s", upgradeBody)
			}
		})
	}
}

func TestDeployScriptPreparesClaimsBaselineOverridesContainedFoundation(t *testing.T) {
	requirePOSIXShell(t)

	live := baselineManifest(true, "", "false")
	env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
	writeFile(t, env.currentRollbackValues, `replicaCount:
  worker: 4
provisioning:
  enabled: true
  workerClaimsEnabled: true
worker:
  contentFilter:
    enabled: true
    categoryFeedBaseURL: https://student-filter-feed.lab.jmal.io
synthetic:
  provisioningExpectedEnabled: true
  suspend: true
  janitor:
    suspend: false
  runner:
    suspend: false
  lifecycle:
    enabled: true
`)

	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	if err != nil {
		t.Fatalf("baseline preparation failed: %v\n%s", err, output)
	}

	requiredArgs := []string{
		"--set provisioning.enabled=false",
		"--set provisioning.workerClaimsEnabled=false",
		"--set worker.contentFilter.enabled=false",
		"--set-string worker.contentFilter.categoryFeedBaseURL=",
		"--set synthetic.provisioningExpectedEnabled=false",
		"--set synthetic.suspend=false",
		"--set synthetic.janitor.suspend=true",
		"--set synthetic.runner.suspend=true",
		"--set synthetic.lifecycle.enabled=false",
		"--set replicaCount.worker=1",
	}
	for _, logPath := range []string{env.templateLog, env.upgradeLog} {
		body, readErr := os.ReadFile(logPath)
		if readErr != nil {
			t.Fatalf("read %s: %v", logPath, readErr)
		}
		for _, want := range requiredArgs {
			if !strings.Contains(string(body), want) {
				t.Fatalf("%s does not contain %q:\n%s", logPath, want, body)
			}
		}
	}
	if !strings.Contains(string(output), "immutable all-workload baseline complete") {
		t.Fatalf("baseline run did not complete cleanly:\n%s", output)
	}
}

func TestDeployScriptPreparesClaimsBaselineAllowsHistoricalApiMonitorFixture(t *testing.T) {
	requirePOSIXShell(t)

	historicalApiMonitor := func(manifest string) string {
		manifest = replaceSyntheticSuspend(manifest, true)
		manifest = strings.Replace(manifest, "activeDeadlineSeconds: 300", "activeDeadlineSeconds: 900", 1)
		manifest = replaceEnvValue(manifest, "SYNTHETIC_PROVISIONING_EXPECTED_ENABLED", "false", "true")
		manifest = replaceEnvValue(manifest, "SYNTHETIC_RUNNER_EXPECTED_ENABLED", "false", "true")
		manifest = replaceEnvValue(manifest, "SYNTHETIC_LIFECYCLE_ENABLED", "false", "true")
		return manifest
	}

	live := historicalApiMonitor(baselineManifest(true, "", "false"))
	if !strings.Contains(live, "activeDeadlineSeconds: 900") ||
		!strings.Contains(live, "SYNTHETIC_PROVISIONING_EXPECTED_ENABLED") ||
		!strings.Contains(live, "SYNTHETIC_RUNNER_EXPECTED_ENABLED") ||
		!strings.Contains(live, "SYNTHETIC_LIFECYCLE_ENABLED") {
		t.Fatal("historical API monitor fixture did not pick up the intended baseline override drift")
	}

	env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	if err != nil {
		t.Fatalf("baseline preparation failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "immutable all-workload baseline complete") {
		t.Fatalf("baseline run did not complete cleanly:\n%s", output)
	}
}

func TestDeployScriptPreparesClaimsBaselineRejectsUnrelatedApiMonitorDrift(t *testing.T) {
	requirePOSIXShell(t)

	historicalApiMonitor := func(manifest string) string {
		manifest = replaceSyntheticSuspend(manifest, true)
		manifest = strings.Replace(manifest, "activeDeadlineSeconds: 300", "activeDeadlineSeconds: 900", 1)
		manifest = replaceEnvValue(manifest, "SYNTHETIC_PROVISIONING_EXPECTED_ENABLED", "false", "true")
		manifest = replaceEnvValue(manifest, "SYNTHETIC_RUNNER_EXPECTED_ENABLED", "false", "true")
		manifest = replaceEnvValue(manifest, "SYNTHETIC_LIFECYCLE_ENABLED", "false", "true")
		return manifest
	}

	live := strings.Replace(
		historicalApiMonitor(baselineManifest(true, "", "false")),
		"successfulJobsHistoryLimit: 3",
		"successfulJobsHistoryLimit: 4",
		1,
	)
	if !strings.Contains(live, "successfulJobsHistoryLimit: 4") {
		t.Fatal("unrelated API monitor fixture drift did not change the schedule-adjacent field")
	}

	env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	if err == nil {
		t.Fatalf("baseline preparation unexpectedly passed unrelated API monitor drift:\n%s", output)
	}
	if !strings.Contains(string(output), "spec drifts from the server-defaulted safe chart") {
		t.Fatalf("baseline failed for the wrong reason:\n%s", output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("baseline drift reached Helm upgrade despite failing the live-spec comparison: %s", upgradeBody)
	}
}

func TestDeployScriptHelmOwnershipNormalizationLoadBearing(t *testing.T) {
	requirePOSIXShell(t)

	scriptDir := filepath.Join("..", "..", "deploy", "scripts")
	deployPath := filepath.Join(scriptDir, "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	filterPath := filepath.Join(scriptDir, "canonicalize-workload-spec.jq")
	filterBody, err := os.ReadFile(filterPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		line string
	}{
		{
			name: "release name",
			line: `        | del(."meta.helm.sh/release-name")` + "\n",
		},
		{
			name: "release namespace",
			line: `        | del(."meta.helm.sh/release-namespace")` + "\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.Count(string(filterBody), test.line) != 1 {
				t.Fatalf("expected exactly one %s normalization line", test.name)
			}
			sabotagedFilterBody := strings.Replace(string(filterBody), test.line, "", 1)
			suffix := strings.ReplaceAll(test.name, " ", "-")
			sabotagedFilterPath := filepath.Join(
				scriptDir,
				"canonicalize-workload-spec-sabotaged-"+suffix+"-test.jq",
			)
			writeFile(t, sabotagedFilterPath, sabotagedFilterBody)
			t.Cleanup(func() { os.Remove(sabotagedFilterPath) })

			const filterAssignment = `CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/canonicalize-workload-spec.jq"`
			if strings.Count(string(deployBody), filterAssignment) != 1 {
				t.Fatal("deploy script canonicalizer assignment is not unique")
			}
			sabotagedDeployBody := strings.Replace(
				string(deployBody),
				filterAssignment,
				`CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/`+filepath.Base(sabotagedFilterPath)+`"`,
				1,
			)
			sabotagedDeployPath := filepath.Join(
				scriptDir,
				"deploy-sabotaged-"+suffix+"-normalization-test.sh",
			)
			writeExecutable(t, sabotagedDeployPath, sabotagedDeployBody)
			t.Cleanup(func() { os.Remove(sabotagedDeployPath) })

			live := withAPITopLevelRevisionAnnotation(baselineManifest(true, "*", "false"))
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			env.scriptPath = sabotagedDeployPath
			output, runErr := env.run(
				"--prepare-claims-baseline",
				"--baseline-chart-dir",
				env.chartDir,
			)
			if runErr == nil {
				t.Fatalf("baseline passed without %s normalization:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), "spec drifts from the server-defaulted safe chart") {
				t.Fatalf("baseline failed for the wrong reason without %s normalization:\n%s", test.name, output)
			}
		})
	}
}

func TestDeployScriptGeneratedAnnotationNormalizationLoadBearing(t *testing.T) {
	requirePOSIXShell(t)

	scriptDir := filepath.Join("..", "..", "deploy", "scripts")
	deployPath := filepath.Join(scriptDir, "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}
	filterPath := filepath.Join(scriptDir, "canonicalize-workload-spec.jq")
	filterBody, err := os.ReadFile(filterPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		filterLine string
		deployLine string
		mode       string
	}{
		{
			name:       "Deployment revision",
			filterLine: `            del(."deployment.kubernetes.io/revision")`,
			deployLine: `            del(.metadata.annotations."deployment.kubernetes.io/revision")`,
			mode:       "deployment-revision",
		},
		{
			name:       "DaemonSet generation",
			filterLine: `            del(."deprecated.daemonset.template.generation")`,
			deployLine: `            del(.metadata.annotations."deprecated.daemonset.template.generation")`,
			mode:       "daemonset-generation",
		},
	} {
		t.Run(test.name+" baseline", func(t *testing.T) {
			if strings.Count(string(filterBody), test.filterLine) != 1 {
				t.Fatalf("expected exactly one narrow %s baseline exception", test.name)
			}
			sabotagedFilterBody := strings.Replace(string(filterBody), test.filterLine, "            .", 1)
			suffix := strings.ToLower(strings.ReplaceAll(test.name, " ", "-"))
			sabotagedFilterPath := filepath.Join(
				scriptDir,
				"canonicalize-workload-spec-sabotaged-"+suffix+"-test.jq",
			)
			writeFile(t, sabotagedFilterPath, sabotagedFilterBody)
			t.Cleanup(func() { os.Remove(sabotagedFilterPath) })

			const filterAssignment = `CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/canonicalize-workload-spec.jq"`
			if strings.Count(string(deployBody), filterAssignment) != 1 {
				t.Fatal("deploy script canonicalizer assignment is not unique")
			}
			sabotagedDeployBody := strings.Replace(
				string(deployBody),
				filterAssignment,
				`CANONICALIZE_WORKLOAD_FILTER="$SCRIPT_DIR/`+filepath.Base(sabotagedFilterPath)+`"`,
				1,
			)
			sabotagedDeployPath := filepath.Join(
				scriptDir,
				"deploy-sabotaged-"+suffix+"-baseline-test.sh",
			)
			writeExecutable(t, sabotagedDeployPath, sabotagedDeployBody)
			t.Cleanup(func() { os.Remove(sabotagedDeployPath) })

			manifest := baselineManifest(true, "*", "false")
			env := newDeployScriptEnvironment(t, manifest, manifest)
			env.scriptPath = sabotagedDeployPath
			output, runErr := env.run(
				"--prepare-claims-baseline",
				"--baseline-chart-dir",
				env.chartDir,
			)
			if runErr == nil {
				t.Fatalf("baseline passed without the exact %s exception:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), "spec drifts from the server-defaulted safe chart") {
				t.Fatalf("baseline failed for the wrong reason without the %s exception:\n%s", test.name, output)
			}
		})

		t.Run(test.name+" post-apply", func(t *testing.T) {
			if strings.Count(string(deployBody), test.deployLine) != 1 {
				t.Fatalf("expected exactly one narrow %s post-apply exception", test.name)
			}
			sabotagedDeployBody := strings.Replace(string(deployBody), test.deployLine, "            .", 1)
			suffix := strings.ToLower(strings.ReplaceAll(test.name, " ", "-"))
			sabotagedDeployPath := filepath.Join(
				scriptDir,
				"deploy-sabotaged-"+suffix+"-post-apply-test.sh",
			)
			writeExecutable(t, sabotagedDeployPath, sabotagedDeployBody)
			t.Cleanup(func() { os.Remove(sabotagedDeployPath) })

			live := baselineManifest(true, "", "false")
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			env.postApplyAnnotationsMode = test.mode
			env.scriptPath = sabotagedDeployPath
			output, runErr := env.run("--no-pull")
			if runErr == nil {
				t.Fatalf("post-apply proof passed without the exact %s exception:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), "deployed Helm object set differs") {
				t.Fatalf("post-apply proof failed for the wrong reason without the %s exception:\n%s", test.name, output)
			}
			if _, statErr := os.Stat(env.lockFile); statErr != nil {
				t.Fatalf("post-apply mismatch did not retain the release lock: %v", statErr)
			}
		})
	}
}

func TestDeployScriptPostApplyGeneratedAnnotationHandling(t *testing.T) {
	requirePOSIXShell(t)

	live := baselineManifest(true, "", "false")
	t.Run("ignores realistic generated annotations", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
		env.postApplyAnnotationsMode = "generated"
		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("post-apply proof rejected generated annotations: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("deploy did not complete after generated annotation normalization:\n%s", output)
		}
	})

	for _, test := range []struct {
		name      string
		mode      string
		configure func(*deployScriptEnvironment)
	}{
		{name: "wrong-kind generated key", mode: "wrong-kind"},
		{name: "template-level generated key", mode: "template-level"},
		{name: "similarly named key", mode: "similarly-named"},
		{name: "unrelated annotation", mode: "unrelated"},
		{name: "wrong-kind key on DaemonSet", mode: "daemonset-wrong-kind"},
		{name: "template-level key on DaemonSet", mode: "daemonset-template-level"},
		{name: "similarly named key on DaemonSet", mode: "daemonset-similarly-named"},
		{name: "unrelated annotation on DaemonSet", mode: "daemonset-unrelated"},
		{
			name: "ordinary spec drift",
			configure: func(env *deployScriptEnvironment) {
				env.postUpgradeObjectMutation = true
			},
		},
	} {
		t.Run("preserves "+test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
			env.postApplyAnnotationsMode = test.mode
			if test.configure != nil {
				test.configure(env)
			}
			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("post-apply proof ignored %s:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), "deployed Helm object set differs") {
				t.Fatalf("post-apply proof rejected %s for the wrong reason:\n%s", test.name, output)
			}
			if _, statErr := os.Stat(env.lockFile); statErr != nil {
				t.Fatalf("post-apply mismatch did not retain the release lock: %v", statErr)
			}
		})
	}

	t.Run("broad annotation deletion hides preserved drift", func(t *testing.T) {
		deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
		deployBody, err := os.ReadFile(deployPath)
		if err != nil {
			t.Fatal(err)
		}
		const narrowBlock = `          if .kind == "Deployment" and
             (.metadata.annotations | type) == "object" and
             (.metadata.annotations | has("deployment.kubernetes.io/revision")) then
            del(.metadata.annotations."deployment.kubernetes.io/revision")
          elif .kind == "DaemonSet" and
               (.metadata.annotations | type) == "object" and
               (.metadata.annotations | has("deprecated.daemonset.template.generation")) then
            del(.metadata.annotations."deprecated.daemonset.template.generation")
          else
            .
          end
          |`
		const broadBlock = `          del(.metadata.annotations)
          |`
		if strings.Count(string(deployBody), narrowBlock) != 1 {
			t.Fatal("post-apply narrow annotation normalization block is not unique")
		}
		sabotagedBody := strings.Replace(string(deployBody), narrowBlock, broadBlock, 1)
		scriptDir := filepath.Dir(deployPath)
		sabotagedPath := filepath.Join(scriptDir, "deploy-sabotaged-broad-annotation-deletion-test.sh")
		writeExecutable(t, sabotagedPath, sabotagedBody)
		t.Cleanup(func() { os.Remove(sabotagedPath) })

		env := newDeployScriptEnvironment(t, live, baselineManifest(true, "*", "false"))
		env.postApplyAnnotationsMode = "unrelated"
		env.scriptPath = sabotagedPath
		output, runErr := env.run("--no-pull")
		if runErr != nil {
			t.Fatalf("broad deletion sabotage did not hide unrelated annotation drift: %v\n%s", runErr, output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("broad deletion sabotage did not reach false success:\n%s", output)
		}
	})
}

func TestDeployScriptPostApplyHelmOwnershipHandling(t *testing.T) {
	requirePOSIXShell(t)

	live := baselineManifest(true, "", "false")
	candidate := rollbackManifestWithHistoricalSynthetics(false)

	t.Run("expected top-level ownership on newly created CronJobs compares equal", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, live, candidate)
		env.postApplyAnnotationsMode = "helm-ownership"
		output, err := env.run("--no-pull")
		if err != nil {
			t.Fatalf("post-apply proof rejected expected Helm ownership metadata: %v\n%s", err, output)
		}
		if !strings.Contains(string(output), "deployed exact source") {
			t.Fatalf("deploy did not complete after Helm ownership normalization:\n%s", output)
		}
	})

	for _, test := range []struct {
		name                 string
		mode                 string
		wantOutput           string
		wantCanonicalFailure bool
	}{
		{
			name:                 "wrong release name fails closed",
			mode:                 "helm-wrong-release-name",
			wantOutput:           `.metadata.annotations["meta.helm.sh/release-name"] must equal "selfservice"`,
			wantCanonicalFailure: true,
		},
		{
			name:                 "wrong release namespace fails closed",
			mode:                 "helm-wrong-release-namespace",
			wantOutput:           `.metadata.annotations["meta.helm.sh/release-namespace"] must equal "selfservice"`,
			wantCanonicalFailure: true,
		},
		{
			name:                 "non-string release name fails closed",
			mode:                 "helm-nonstring-release-name",
			wantOutput:           `.metadata.annotations["meta.helm.sh/release-name"] must be a string`,
			wantCanonicalFailure: true,
		},
		{
			name:                 "non-string release namespace fails closed",
			mode:                 "helm-nonstring-release-namespace",
			wantOutput:           `.metadata.annotations["meta.helm.sh/release-namespace"] must be a string`,
			wantCanonicalFailure: true,
		},
		{
			name:                 "wrong managed-by fails closed",
			mode:                 "helm-wrong-managed-by",
			wantOutput:           `.metadata.labels["app.kubernetes.io/managed-by"] must equal "Helm"`,
			wantCanonicalFailure: true,
		},
		{
			name:                 "non-string managed-by fails closed",
			mode:                 "helm-nonstring-managed-by",
			wantOutput:           `.metadata.labels["app.kubernetes.io/managed-by"] must be a string`,
			wantCanonicalFailure: true,
		},
		{
			name:       "arbitrary top-level annotation drift remains compared",
			mode:       "helm-top-annotation-drift",
			wantOutput: "deployed Helm object set differs from the exact validated candidate",
		},
		{
			name:       "arbitrary top-level label drift remains compared",
			mode:       "helm-top-label-drift",
			wantOutput: "deployed Helm object set differs from the exact validated candidate",
		},
		{
			name:       "pod-template managed-by drift remains compared",
			mode:       "helm-template-managed-by-drift",
			wantOutput: "deployed Helm object set differs from the exact validated candidate",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newDeployScriptEnvironment(t, live, candidate)
			env.postApplyAnnotationsMode = test.mode
			output, err := env.run("--no-pull")
			if err == nil {
				t.Fatalf("post-apply proof ignored %s:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("post-apply proof rejected %s for the wrong reason:\n%s", test.name, output)
			}
			if test.wantCanonicalFailure {
				if !strings.Contains(string(output), "could not refresh the expected Helm object set against the post-upgrade cluster") {
					t.Fatalf("malformed live ownership did not fail the refreshed expected canonicalization:\n%s", output)
				}
			} else if !strings.Contains(string(output), "deployed Helm object set differs") {
				t.Fatalf("metadata drift did not fail the deployed-candidate comparison:\n%s", output)
			}
			if _, statErr := os.Stat(env.upgradedMarker); statErr != nil {
				t.Fatalf("failure occurred before the post-apply comparison: %v", statErr)
			}
			if _, statErr := os.Stat(env.lockFile); statErr != nil {
				t.Fatalf("post-apply ownership failure did not retain the release lock: %v", statErr)
			}
		})
	}
}

func TestDeployScriptPostApplyHelmOwnershipNormalizationLoadBearing(t *testing.T) {
	requirePOSIXShell(t)

	deployPath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	deployBody, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		line string
	}{
		{
			name: "release name",
			line: `          | del(.metadata.annotations."meta.helm.sh/release-name")` + "\n",
		},
		{
			name: "release namespace",
			line: `          | del(.metadata.annotations."meta.helm.sh/release-namespace")` + "\n",
		},
		{
			name: "managed-by",
			line: `          | del(.metadata.labels."app.kubernetes.io/managed-by")` + "\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.Count(string(deployBody), test.line) != 1 {
				t.Fatalf("expected exactly one narrow post-apply %s normalization", test.name)
			}
			sabotagedBody := strings.Replace(string(deployBody), test.line, "", 1)
			suffix := strings.ReplaceAll(test.name, " ", "-")
			sabotagedPath := filepath.Join(
				filepath.Dir(deployPath),
				"deploy-sabotaged-"+suffix+"-post-apply-ownership-test.sh",
			)
			writeExecutable(t, sabotagedPath, sabotagedBody)
			t.Cleanup(func() { os.Remove(sabotagedPath) })

			live := baselineManifest(true, "", "false")
			candidate := rollbackManifestWithHistoricalSynthetics(false)
			env := newDeployScriptEnvironment(t, live, candidate)
			env.postApplyAnnotationsMode = "helm-ownership"
			env.scriptPath = sabotagedPath
			output, runErr := env.run("--no-pull")
			if runErr == nil {
				t.Fatalf("revision-170-shaped post-apply proof passed without %s normalization:\n%s", test.name, output)
			}
			if !strings.Contains(string(output), "deployed Helm object set differs from the exact validated candidate") {
				t.Fatalf("sabotage failed for the wrong reason without %s normalization:\n%s", test.name, output)
			}
			if _, statErr := os.Stat(env.upgradedMarker); statErr != nil {
				t.Fatalf("sabotage did not reach the post-apply comparator: %v", statErr)
			}
			if _, statErr := os.Stat(env.lockFile); statErr != nil {
				t.Fatalf("post-apply mismatch did not retain the release lock: %v", statErr)
			}
		})
	}
}

// kubectlServerApplyDryRunFunctionName is the single bash function in
// deploy.sh that may ever invoke
// `kubectl apply --server-side --dry-run=server --force-conflicts`. This
// exact, fixed argv tuple is centralized behind one helper (see
// deploy/scripts/deploy.sh) rather than repeated independently at each of
// the three validation call sites, precisely so that requiring the
// function's entire body to be byte-for-byte identical to
// kubectlServerApplyDryRunExpectedBody actually means something: neither a
// decoy embedded in some other flag's value (for example
// --field-manager=X--dry-run=server glued next to a real, changed
// --dry-run=none), nor a second, conflicting flag of the same family
// appended onto a different, otherwise-legitimate line (for example a
// later --dry-run=none glued onto the existing -o "$output_format" line -
// which kubectl, like most flag parsers, would honor over the earlier,
// real --dry-run=server, since it honors the last occurrence of a
// repeated flag), can produce a body that still equals the one fixed,
// expected string.
const kubectlServerApplyDryRunFunctionName = "kubectl_server_apply_dry_run"

// kubectlServerApplyDryRunExpectedBody is the exact, complete, expected
// source text of kubectlServerApplyDryRunFunctionName()'s body in
// deploy.sh (as extracted by extractFunctionBody from the
// comment-stripped file) - the single, fixed, non-parameterized kubectl
// invocation this entire design depends on, including its bare
// --server-side, --dry-run=server, and --force-conflicts lines and
// nothing else.
//
// A prior implementation of this invariant checked the presence of each
// of those three flags independently, using a per-*line* scan (does some
// line begin with "--dry-run=", and is it exactly "--dry-run=server"?).
// A final independent review found that check insufficient: appending an
// additional, conflicting "--dry-run=none" onto the END of a different,
// already-legitimate line (for example "-o \"$output_format\"
// --dry-run=none") does not change which line the real --dry-run=server
// flag lives on, and the appended text does not itself begin a line with
// the "--dry-run=" prefix - so a per-line scan for that prefix never sees
// it, even though kubectl parses it as a completely independent,
// conflicting argv token that (per kubectl's own flag-parsing semantics)
// would win over the earlier one, silently turning a validation-only
// dry-run into a real, mutating, --force-conflicts server-side apply
// against live production objects.
//
// Comparing the function's *entire* body against one fixed, exact,
// hardcoded expected string sidesteps that whole class of bug: it does
// not matter which physical line an attacker chooses to add, remove,
// reorder, or append a conflicting/duplicate/neutered flag on - any
// difference whatsoever from this literal, byte-for-byte expected text
// fails the check immediately. It also happens to be far simpler to read
// and audit than a growing list of individual per-flag scans, each of
// which can only be proven correct against the specific attacks its
// author thought to consider.
//
// A further independent review found that even this exact-body check does
// not, by itself, guarantee bash actually executes the real kubectl binary
// with this literal argv: every kubectl invocation in deploy.sh, including
// this one, historically called the bare, unqualified command name
// "kubectl" - which bash resolves to a matching shell function or alias
// (defined anywhere else in the file, brace- or subshell-bodied) before
// ever doing a PATH lookup for the real binary. Such a shadow could
// silently rewrite this call's argv at runtime - for example inserting an
// unrelated value-consuming flag immediately before --dry-run=server so
// kubectl's own parser consumes that literal token as the earlier flag's
// value instead of recognizing it as dry-run - without changing this
// function's own body at all, defeating byte-equality entirely. The
// expected body below therefore now begins the kubectl invocation with
// `command kubectl`, not a bare `kubectl`: `command` forces bash to skip
// shell function and alias lookup and go straight to a PATH search for the
// real binary, so no such shadow - however it is defined - can ever
// intercept this specific call, regardless of what else the file (or
// anything it sources) defines.
const kubectlServerApplyDryRunExpectedBody = "kubectl_server_apply_dry_run() {\n" +
	"  local field_manager=$1\n" +
	"  local manifest_path=$2\n" +
	"  local output_format=$3\n" +
	"  command kubectl apply \\\n" +
	"    --server-side \\\n" +
	"    --dry-run=server \\\n" +
	"    --force-conflicts \\\n" +
	"    --field-manager=\"$field_manager\" \\\n" +
	"    -n \"$NAMESPACE\" \\\n" +
	"    -f \"$manifest_path\" \\\n" +
	"    -o \"$output_format\"\n" +
	"}\n"

// forceConflictsValidationCallers are the only bash functions allowed to
// invoke kubectlServerApplyDryRunFunctionName. Any other function -
// mutating or not - doing so is a structural violation.
var forceConflictsValidationCallers = []string{
	"validate_upgrade_hooks",
	"server_validate_candidate",
	"canonicalize_server_candidate",
}

// exactBodyLines returns every line of body with surrounding whitespace and
// a trailing "\" line-continuation removed, so a line written as
// "    kubectl_server_apply_dry_run crucible-production-deploy ... \" on
// its own line normalizes predictably regardless of indentation. Comparing
// whole normalized lines - rather than scanning for a substring anywhere in
// the body - is what makes countExactLine/countLinesWithPrefix below immune
// to unrelated text elsewhere on the same or a different line changing
// what they match.
func exactBodyLines(body string) []string {
	rawLines := strings.Split(body, "\n")
	lines := make([]string, len(rawLines))
	for i, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		trimmed = strings.TrimSuffix(trimmed, `\`)
		lines[i] = strings.TrimSpace(trimmed)
	}
	return lines
}

// countExactLine reports how many lines in body, once normalized by
// exactBodyLines, equal want exactly. Used for structural checks (how many
// times does a whole file contain the literal line "kubectl apply"?) where
// the thing being counted is not itself a value-bearing flag that could be
// appended onto a different line - see kubectlServerApplyDryRunExpectedBody
// for why the fixed --server-side/--dry-run=server/--force-conflicts flags
// themselves are no longer checked this way.
func countExactLine(body, want string) int {
	count := 0
	for _, line := range exactBodyLines(body) {
		if line == want {
			count++
		}
	}
	return count
}

// countLinesWithPrefix reports how many normalized lines in body begin with
// prefix. Used only to count call sites of kubectlServerApplyDryRunFunctionName
// (each on its own line, invoked with a leading function-name-plus-space
// prefix) - not for the fixed flags themselves, which whole-body equality
// (kubectlServerApplyDryRunExpectedBody) now checks instead.
func countLinesWithPrefix(body, prefix string) int {
	count := 0
	for _, line := range exactBodyLines(body) {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// splitDocumentFileNamePattern matches the numbered filenames
// split_manifest_documents (deploy.sh) produces for each document of a
// multi-document manifest it splits, e.g. "000001.yaml", "000002.yaml".
var splitDocumentFileNamePattern = regexp.MustCompile(`^\d{6}\.yaml$`)

// countLogicalServerValidations reports how many logical server-side
// dry-run validation passes appear in a $FAKE_SERVER_DRY_RUN_LOG-shaped log
// (one line per kubectl invocation, each line the space-joined argv).
// server_validate_candidate now issues one "-o yaml" kubectl call per
// document in the candidate manifest instead of one call for the whole
// manifest - splitting multi-document input avoids kubectl's real
// `List`-wrapping of multi-document "-o yaml" apply output (see the
// comment on server_validate_candidate in deploy.sh) - so what used to be
// exactly one logged "-o yaml" line per logical validation pass is now one
// line per document in that pass. This counts only the first document of
// each pass (a "-f" argument whose basename is exactly "000001.yaml") or an
// unsplit direct file path (used by validate_upgrade_hooks, which is never
// split), so callers can keep asserting a fixed number of logical
// validation passes regardless of how many documents a candidate manifest
// happens to render.
func countLogicalServerValidations(t *testing.T, log []byte) int {
	t.Helper()
	count := 0
	trimmed := strings.TrimRight(string(log), "\n")
	if trimmed == "" {
		return 0
	}
	for _, line := range strings.Split(trimmed, "\n") {
		fields := strings.Fields(line)
		isYAML := false
		manifestPath := ""
		for i, field := range fields {
			if field == "-o" && i+1 < len(fields) && fields[i+1] == "yaml" {
				isYAML = true
			}
			if field == "-f" && i+1 < len(fields) {
				manifestPath = fields[i+1]
			}
		}
		if !isYAML {
			continue
		}
		base := filepath.Base(manifestPath)
		if splitDocumentFileNamePattern.MatchString(base) && base != "000001.yaml" {
			continue
		}
		count++
	}
	return count
}

// extractFunctionBody returns the full source text of the named bash
// function (from its "name() {" marker through its closing "\n}\n"), and
// the byte offsets of that span within source. It fails if the function
// cannot be found, or is defined more than once.
func extractFunctionBody(source, functionName string) (body string, start, end int, err error) {
	marker := functionName + "() {"
	start = strings.Index(source, marker)
	if start < 0 {
		return "", 0, 0, fmt.Errorf("could not locate function %s() in deploy.sh", functionName)
	}
	if strings.Contains(source[start+len(marker):], marker) {
		return "", 0, 0, fmt.Errorf("function %s() is defined more than once in deploy.sh", functionName)
	}
	relativeEnd := strings.Index(source[start:], "\n}\n")
	if relativeEnd < 0 {
		return "", 0, 0, fmt.Errorf("could not locate end of function %s() in deploy.sh", functionName)
	}
	end = start + relativeEnd + len("\n}\n")
	return source[start:end], start, end, nil
}

// forceConflictsFlagLine matches an actual `--force-conflicts \` flag line
// in deploy.sh, as opposed to explanatory comment text that merely mentions
// the flag in prose. It is used only by stripForceConflictsFromFunction
// below to remove one legitimate occurrence; the invariant itself is
// enforced by checkForceConflictsInvariant, which is not line-shape
// dependent.
var forceConflictsFlagLine = regexp.MustCompile(`(?m)^[ \t]*--force-conflicts \\$`)

// stripBashLineComments removes bash "#" comments from every line so that
// explanatory prose that merely mentions "--force-conflicts" is never
// mistaken for the real flag by the whitespace-token scan below. A "#" only
// starts a comment when it begins the line (after leading whitespace) or is
// preceded by whitespace, matching how bash parses it for the plain,
// unquoted lines used around these calls.
func stripBashLineComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		for j := 0; j < len(line); j++ {
			if line[j] != '#' {
				continue
			}
			if j == 0 || line[j-1] == ' ' || line[j-1] == '\t' {
				lines[i] = line[:j]
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// forceConflictsLiteral is the exact flag text this invariant searches for
// as a raw substring, rather than via whitespace tokenization. Bash (and
// kubectl) accept this exact argument immediately adjacent to shell
// metacharacters other than whitespace - a trailing quote
// (`"--force-conflicts"`), a semicolon (`--force-conflicts;`), a pipe, a
// closing paren, etc. - so a scanner that only splits on whitespace (e.g.
// strings.Fields) can miss a leaked occurrence glued to one of those
// characters. Scanning for the literal substring instead, and only
// rejecting a match that is part of a LONGER flag/identifier, catches every
// one of those forms.
const forceConflictsLiteral = "--force-conflicts"

// isFlagNameByte reports whether b could be part of a longer flag or
// identifier name that merely contains forceConflictsLiteral as a
// substring (e.g. the "-foo" of "--force-conflicts-foo", or the "x" of
// "x--force-conflicts"), as opposed to a delimiter that could legitimately
// surround the flag itself, such as a quote, semicolon, pipe, paren, or
// whitespace.
func isFlagNameByte(b byte) bool {
	return b == '-' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// findForceConflictsOccurrences returns the start byte offset of every
// occurrence of forceConflictsLiteral in text, excluding occurrences that
// are a substring of a longer flag/identifier, and excluding occurrences
// immediately followed by '=' (an explicit value assignment, e.g.
// "--force-conflicts=false" or "--force-conflicts=true"). Every other
// neighboring character - a quote, semicolon, pipe, paren, whitespace, or
// end of input - is accepted, since all of those are valid ways for a shell
// to delimit this exact bare argument, and this check must not be evadable
// by choosing one of them instead of a plain space.
//
// The '=' exclusion matters because kubectl's --force-conflicts is a
// boolean flag that also accepts "--force-conflicts=<value>" syntax. Since
// "false" is the flag's default, "--force-conflicts=false" is functionally
// identical to omitting the flag entirely - it is not a real occurrence of
// the enabling flag, merely a substring match on it - so treating it as
// satisfying "must carry --force-conflicts exactly once" would let a
// silently-neutered mutation of one of the three known validation blocks
// pass this invariant. Deliberately excluding any "=" form, including
// "=true" (which mirrors bare --force-conflicts), keeps this check
// fail-closed: it must never be satisfied by any text this scanner cannot
// prove that kubectl treats identically to the bare flag it expects.
func findForceConflictsOccurrences(text string) []int {
	var offsets []int
	for i := 0; i+len(forceConflictsLiteral) <= len(text); i++ {
		if text[i:i+len(forceConflictsLiteral)] != forceConflictsLiteral {
			continue
		}
		if i > 0 && isFlagNameByte(text[i-1]) {
			continue
		}
		end := i + len(forceConflictsLiteral)
		if end < len(text) && (isFlagNameByte(text[end]) || text[end] == '=') {
			continue
		}
		offsets = append(offsets, i)
	}
	return offsets
}

// checkForceConflictsInvariant proves, independent of how deploy.sh happens
// to be formatted, that the fixed, non-parameterized argv tuple
// --server-side / --dry-run=server / --force-conflicts is written exactly
// once in the entire file, inside kubectlServerApplyDryRunFunctionName, and
// that every one of the three known validation-only call sites reaches it
// - and nothing else in the file (in particular no mutating command, and
// no fourth caller) can reach or reconstruct it.
//
// This check combines two independent detection strategies, because each
// closes a gap the other cannot:
//
//  1. Exact, whole-body equality (kubectlServerApplyDryRunExpectedBody)
//     against the one helper function's entire body. A prior version of
//     this invariant instead checked each fixed flag independently via a
//     per-*line* scan (is there exactly one line beginning "--dry-run=",
//     and is it exactly "--dry-run=server"?). A final independent review
//     found that insufficient: appending a second, conflicting
//     "--dry-run=none" onto the END of a different, already-legitimate
//     line (e.g. the -o "$output_format" line) does not put it on its own
//     line beginning with the "--dry-run=" prefix, so the per-line scan
//     never saw it - even though kubectl would honor that later,
//     conflicting token over the real one. Whole-body equality closes
//     this and every similar gap at once: any difference whatsoever from
//     the one fixed, expected string - an appended, reordered, removed,
//     duplicated, or neutered ("=false"/"=true") flag, anywhere in the
//     body - fails the check, regardless of which physical line it was
//     introduced on.
//  2. A literal substring scan (findForceConflictsOccurrences) over the
//     *whole*, comment-stripped file, independent of block/function
//     boundaries, to prove --force-conflicts appears nowhere else - in
//     particular not leaked, in any formatting or quoting, onto a mutating
//     command such as `kubectl patch`, `kubectl set`, `kubectl delete`, or
//     `helm upgrade`. Whole-body equality alone cannot prove this, since it
//     only inspects the one helper's own body and would not notice a
//     completely separate leak elsewhere in the file.
//
// It also independently rejects any bash function or alias definition named
// "kubectl" anywhere in the file, whether the function uses a `{ ... }`
// compound-command body or a `( ... )` subshell body - bash accepts both
// forms interchangeably as a function definition, and both are resolved in
// exactly the same way ahead of a PATH lookup. Every kubectl invocation in
// deploy.sh (including inside kubectlServerApplyDryRunFunctionName itself,
// prior to the `command kubectl` hardening below) calls the bare,
// unqualified command name "kubectl" - bash resolves an unqualified
// command name to a matching shell function or alias before ever doing a
// PATH lookup for the real binary. A `kubectl() { ... }` or
// `kubectl() ( ... )` shadow function (or `alias kubectl=...`) defined
// anywhere else in the file would silently intercept any bare `kubectl`
// call reached without `command` - letting it inject, drop, or rewrite
// argv at runtime (for example inserting an unrelated value-consuming flag
// immediately before --dry-run=server so kubectl's real flag parser
// consumes that literal token as the earlier flag's value instead of
// recognizing it as dry-run) without changing the calling function's own
// body at all. kubectlServerApplyDryRunFunctionName's own invocation is
// separately hardened against this by using `command kubectl` (checked via
// kubectlServerApplyDryRunExpectedBody above, which now requires that
// prefix); this pattern remains a second, independent, whole-file
// defense-in-depth check, since a shadow defined here could still
// intercept any other bare, unqualified `kubectl` call in the file (such as
// the pre-existing, unrelated `kubectl create --dry-run=server` call used
// elsewhere) even though it can no longer reach this specific helper.
var kubectlShadowDefinitionPattern = regexp.MustCompile(`(?m)^\s*(function\s+kubectl\s*(\(\))?\s*[{(]|kubectl\s*\(\)\s*[{(]|alias\s+kubectl=)`)

func checkForceConflictsInvariant(text string) error {
	stripped := stripBashLineComments(text)

	if loc := kubectlShadowDefinitionPattern.FindStringIndex(stripped); loc != nil {
		return fmt.Errorf("deploy.sh must never define a shell function or alias named `kubectl` anywhere in the file (whether brace- or subshell-bodied); bash resolves the bare `kubectl` command name used by any invocation not prefixed with `command` to such a shadow before ever consulting PATH for the real binary, letting it inject, drop, or rewrite argv at runtime (found near byte offset %d)", loc[0])
	}

	// Exactly one place in the whole file may invoke `command kubectl
	// apply` at all, and it must be inside
	// kubectlServerApplyDryRunFunctionName. This structurally rules out any
	// other function - mutating or not - constructing its own separate
	// --server-side/--dry-run=server/--force-conflicts invocation, or any
	// other kubectl apply entirely.
	if n := countExactLine(stripped, "command kubectl apply"); n != 1 {
		return fmt.Errorf("expected exactly one `command kubectl apply` invocation in the whole file (inside %s()), found %d", kubectlServerApplyDryRunFunctionName, n)
	}

	// A bare, unqualified `kubectl apply` (missing the `command` prefix)
	// must never appear anywhere, including inside
	// kubectlServerApplyDryRunFunctionName itself: without `command`, bash
	// would resolve the bare "kubectl" name to a shell function or alias
	// shadow - if one were ever defined anywhere in the file - before doing
	// a PATH lookup for the real binary, letting such a shadow silently
	// rewrite this call's argv at runtime without changing this function's
	// own body at all. This check ensures nobody can reintroduce that
	// unprotected form later, even if kubectlServerApplyDryRunExpectedBody
	// were (incorrectly) updated to match it.
	if n := countExactLine(stripped, "kubectl apply"); n != 0 {
		return fmt.Errorf("found %d bare `kubectl apply` invocation(s) (missing the `command` prefix) in the whole file; every kubectl apply invocation must read `command kubectl apply` so bash cannot resolve it to a shell function or alias shadow instead of the real binary", n)
	}

	body, start, end, err := extractFunctionBody(stripped, kubectlServerApplyDryRunFunctionName)
	if err != nil {
		return err
	}

	if body != kubectlServerApplyDryRunExpectedBody {
		return fmt.Errorf(
			"%s() does not exactly match the required fixed body; it must contain only the immutable --server-side/--dry-run=server/--force-conflicts invocation with no additional, reordered, duplicated, or conflicting flag anywhere in its body (for example a second --dry-run=none appended onto a different, otherwise-legitimate line)\n--- got ---\n%s--- want ---\n%s",
			kubectlServerApplyDryRunFunctionName, body, kubectlServerApplyDryRunExpectedBody,
		)
	}

	// Independently confirm --force-conflicts does not leak anywhere else
	// in the file, in any formatting or quoting, using the permissive
	// literal-substring scan (which is deliberately not restricted to
	// whole-line matches, so it also catches leakage glued inline onto an
	// existing flag's line, quoted, or placed immediately before a shell
	// separator such as `;`).
	globalOffsets := findForceConflictsOccurrences(stripped)
	if len(globalOffsets) != 1 {
		return fmt.Errorf("found %d --force-conflicts occurrence(s) in deploy.sh but exactly 1 is expected (inside %s()); --force-conflicts must never appear on any other command (e.g. a mutating kubectl patch/set/delete or helm upgrade)", len(globalOffsets), kubectlServerApplyDryRunFunctionName)
	}
	if globalOffsets[0] < start || globalOffsets[0] >= end {
		return fmt.Errorf("the single --force-conflicts occurrence in deploy.sh is not inside %s()", kubectlServerApplyDryRunFunctionName)
	}

	// Every known validation call site must reach the centralizing helper
	// exactly once, and the helper must not be reachable from anywhere
	// else in the file (in particular not from a mutating function).
	totalCalls := 0
	for _, caller := range forceConflictsValidationCallers {
		callerBody, _, _, err := extractFunctionBody(stripped, caller)
		if err != nil {
			return err
		}
		calls := countLinesWithPrefix(callerBody, kubectlServerApplyDryRunFunctionName+" ")
		if calls != 1 {
			return fmt.Errorf("%s() must call %s() exactly once (found %d call(s))", caller, kubectlServerApplyDryRunFunctionName, calls)
		}
		totalCalls += calls
	}
	if n := countLinesWithPrefix(stripped, kubectlServerApplyDryRunFunctionName+" "); n != totalCalls {
		return fmt.Errorf("found %d call(s) to %s() in deploy.sh but only %d are inside the %d known validation-only callers; it must never be reachable from any other function (e.g. a mutating one)", n, kubectlServerApplyDryRunFunctionName, totalCalls, len(forceConflictsValidationCallers))
	}
	return nil
}

// TestDeployScriptForceConflictsPairedWithServerSideDryRun is a static
// invariant: every kubectl apply invocation in deploy.sh must be exactly the
// three known --server-side --dry-run=server validation-only calls (upgrade
// hooks, the digest-pinned candidate, and per-document canonicalization),
// and every one of them must carry --force-conflicts, and nothing else in
// the file may carry it. Production objects (ingress, workload
// images/env/resources, StatefulSet volumeClaimTemplates, CronJob fields)
// are already owned by Helm (and one containment field by kubectl-set), so
// a validation-only SSA dry-run against them must be allowed to take over
// ownership or Kubernetes correctly rejects it before admission/defaulting
// validation ever runs. The real production apply remains `helm upgrade`,
// not kubectl apply, so this test proves --force-conflicts can never leak
// onto that or any other command. See checkForceConflictsInvariant for how
// leakage is detected regardless of formatting.
func TestDeployScriptForceConflictsPairedWithServerSideDryRun(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkForceConflictsInvariant(string(source)); err != nil {
		t.Fatal(err)
	}
}

// TestDeployScriptForceConflictsInvariantDetectsMutatingLeakage proves the
// static invariant above is load-bearing against leakage, not merely a
// count of backslash-continued lines: it must catch --force-conflicts
// leaking onto a real mutating kubectl command already present in deploy.sh
// (kubectl patch and kubectl set env, both of which mutate live cluster
// state), regardless of whether the leaked flag is written inline on an
// existing flag's line, as its own new line, double-quoted exactly as a
// shell would pass it, or glued immediately before a `;` statement
// terminator with no separating space. A leaked occurrence there would let
// a mutating operation silently take over field ownership it has no
// business touching, so the invariant must fail closed on it exactly as it
// would on the intended validation-only call sites, in every one of these
// shapes.
func TestDeployScriptForceConflictsInvariantDetectsMutatingLeakage(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(source)
	if err := checkForceConflictsInvariant(original); err != nil {
		t.Fatalf("precondition failed: unmodified deploy.sh must satisfy the invariant: %v", err)
	}

	tests := []struct {
		name string
		old  string
		new  string
	}{
		{
			// enforce_synthetic_rollback_containment's real, unconditional
			// `kubectl patch cronjob/... --type strategic -p '...'` mutates
			// a live CronJob. Attaching --force-conflicts inline on its
			// existing --type flag line (no new line, no distinctive
			// leading whitespace) is exactly the shape the old
			// line-anchored regex check could not see.
			name: "inline on an existing flag line of a real mutating kubectl patch",
			old:  "      --type strategic \\\n",
			new:  "      --type strategic --force-conflicts \\\n",
		},
		{
			// contain_failed_atomic_upgrade's real
			// `kubectl set env deployment/$RELEASE-worker ...` mutates a
			// live Deployment's environment. This inserts the flag as its
			// own backslash-continued line (the shape the old regex did
			// match), proving the new invariant still catches leakage even
			// in the "obvious" formatting, just onto the wrong command.
			name: "own line inside a real mutating kubectl set env",
			old:  "  if ! kubectl set env \"deployment/$RELEASE-worker\" \\\n      -n \"$NAMESPACE\" \\\n      WORKER_PROVISIONING_CLAIMS_ENABLED=false; then",
			new:  "  if ! kubectl set env \"deployment/$RELEASE-worker\" \\\n      --force-conflicts \\\n      -n \"$NAMESPACE\" \\\n      WORKER_PROVISIONING_CLAIMS_ENABLED=false; then",
		},
		{
			// Same real kubectl set env mutation, but the leaked flag is
			// double-quoted (`"--force-conflicts"`) exactly as a shell
			// would accept it - bash strips the quotes and passes the
			// identical bare argument to kubectl. A whitespace-token
			// equality check (e.g. strings.Fields plus `tok ==
			// "--force-conflicts"`) misses this because the quote
			// characters stay glued to the token; a literal substring scan
			// must not.
			name: "quoted leakage into the same real mutating kubectl set env",
			old:  "  if ! kubectl set env \"deployment/$RELEASE-worker\" \\\n      -n \"$NAMESPACE\" \\\n      WORKER_PROVISIONING_CLAIMS_ENABLED=false; then",
			new:  "  if ! kubectl set env \"deployment/$RELEASE-worker\" \\\n      \"--force-conflicts\" \\\n      -n \"$NAMESPACE\" \\\n      WORKER_PROVISIONING_CLAIMS_ENABLED=false; then",
		},
		{
			// enforce_synthetic_rollback_containment's real
			// `kubectl patch cronjob/$api_cronjob ...` mutation, with the
			// leaked flag appended immediately before the statement
			// terminator with no separating space
			// (`--force-conflicts; then`). Bash treats `;` as a command
			// separator regardless of adjacent whitespace, so this is a
			// valid, distinct argument to kubectl - but it is glued to the
			// following `;` with no space, which a whitespace-token scan
			// would fold into a single non-matching token.
			name: "semicolon-adjacent leakage into a real mutating kubectl patch",
			old:  `      -p '{"spec":{"suspend":false,"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"synthetic-api-monitor","env":[{"name":"SYNTHETIC_LIFECYCLE_ENABLED","value":"false"}]}]}}}}}}'; then`,
			new:  `      -p '{"spec":{"suspend":false,"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"synthetic-api-monitor","env":[{"name":"SYNTHETIC_LIFECYCLE_ENABLED","value":"false"}]}]}}}}}}' --force-conflicts; then`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if occurrences := strings.Count(original, test.old); occurrences != 1 {
				t.Fatalf("expected exactly one occurrence of the target text to mutate, found %d:\n%s", occurrences, test.old)
			}
			mutated := strings.Replace(original, test.old, test.new, 1)
			if mutated == original {
				t.Fatal("mutation had no effect")
			}
			err := checkForceConflictsInvariant(mutated)
			if err == nil {
				t.Fatal("invariant did not detect --force-conflicts leaking onto a real mutating kubectl command")
			}
			if !strings.Contains(err.Error(), "must never appear on any other command") {
				t.Fatalf("invariant failure did not identify leakage outside the known validation calls: %v", err)
			}
		})
	}
}

// TestDeployScriptForceConflictsInvariantDetectsRemoval proves the same
// static invariant fails closed the other direction too: removing
// --force-conflicts from the single centralizing helper that all three
// validation call sites depend on (the same mutation
// TestDeployScriptForceConflictsIsLoadBearing uses to prove the runtime
// behavior is load-bearing) must also be caught statically, without needing
// to actually execute the script.
func TestDeployScriptForceConflictsInvariantDetectsRemoval(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)

	mutated := stripForceConflictsFromFunction(t, original, kubectlServerApplyDryRunFunctionName)
	if err := checkForceConflictsInvariant(mutated); err == nil {
		t.Fatalf("invariant did not detect --force-conflicts removed from %s()", kubectlServerApplyDryRunFunctionName)
	}
}

// sabotageDryRunDowngradeWithDecoy returns a copy of the real deploy.sh with
// the centralizing helper's real --dry-run=server flag downgraded to
// --dry-run=none (which would make kubectl perform a real, mutating,
// --force-conflicts server-side apply against live production objects),
// plus a decoy "--dry-run=server" substring glued onto other, unrelated
// text - reproducing the exact structural attack the final independent
// review identified: a naive substring scan (or a test harness that joins
// argv into one string and substring-matches it) can be fooled into
// believing the safe flag is present, even though the real, effective
// --dry-run value is not "server". decoyIn selects where the decoy is
// hidden: "field-manager" (the reviewer's exact reported shape - glued onto
// the field-manager argument at the candidate call site, with no space, so
// it becomes one distinct-but-different argv token such as
// "crucible-production-deploy--dry-run=server"), "filename" (glued onto the
// manifest path argument instead), or "comment" (placed on its own comment
// line immediately next to the corrupted flag, inside the helper itself -
// proving comment-stripping means it can never rescue the invariant).
func sabotageDryRunDowngradeWithDecoy(t *testing.T, source, decoyIn string) string {
	t.Helper()
	const realFlagLine = "    --dry-run=server \\\n"
	if n := strings.Count(source, realFlagLine); n != 1 {
		t.Fatalf("expected exactly one occurrence of the real --dry-run=server flag line, found %d", n)
	}
	const callSite = `    kubectl_server_apply_dry_run crucible-production-deploy "$document" yaml >> "$output"` + "\n"
	if n := strings.Count(source, callSite); n != 1 {
		t.Fatalf("expected exactly one occurrence of the candidate call site, found %d", n)
	}

	var mutated string
	switch decoyIn {
	case "field-manager":
		mutated = strings.Replace(source, realFlagLine, "    --dry-run=none \\\n", 1)
		mutated = strings.Replace(mutated, callSite,
			"    kubectl_server_apply_dry_run crucible-production-deploy--dry-run=server \"$document\" yaml >> \"$output\"\n", 1)
	case "filename":
		mutated = strings.Replace(source, realFlagLine, "    --dry-run=none \\\n", 1)
		mutated = strings.Replace(mutated, callSite,
			"    kubectl_server_apply_dry_run crucible-production-deploy \"$document--dry-run=server\" yaml >> \"$output\"\n", 1)
	case "comment":
		mutated = strings.Replace(source, realFlagLine,
			"    --dry-run=none \\\n    # --dry-run=server intentionally preserved for compatibility\n", 1)
	default:
		t.Fatalf("unknown decoyIn %q", decoyIn)
	}
	if mutated == source {
		t.Fatal("sabotage mutation had no effect")
	}
	return mutated
}

// TestDeployScriptForceConflictsInvariantDetectsDryRunDowngradeWithDecoy is
// the final independent review's exact structural sabotage: the real,
// effective --dry-run flag value is silently downgraded from "server" to
// "none" (which would make kubectl perform a real, mutating,
// --force-conflicts server-side apply against live production objects),
// while a decoy "--dry-run=server" substring is glued onto unrelated nearby
// text so a naive substring-scan invariant (or a test harness that joins
// argv into one string and substring-matches it) is fooled into believing
// the safe flag is still present. checkForceConflictsInvariant must not be
// satisfied by any of these decoy placements, because it requires the
// centralizing helper's --dry-run= line to be its own complete, exact,
// byte-for-byte line - never a substring embedded in a different flag's
// value, a different argument's value, or a comment.
func TestDeployScriptForceConflictsInvariantDetectsDryRunDowngradeWithDecoy(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)
	if err := checkForceConflictsInvariant(original); err != nil {
		t.Fatalf("precondition failed: unmodified deploy.sh must satisfy the invariant: %v", err)
	}

	for _, decoyIn := range []string{"field-manager", "filename", "comment"} {
		t.Run(decoyIn, func(t *testing.T) {
			mutated := sabotageDryRunDowngradeWithDecoy(t, original, decoyIn)
			if err := checkForceConflictsInvariant(mutated); err == nil {
				t.Fatalf("invariant did not detect --dry-run=server downgraded to --dry-run=none with a decoy hidden in the %s", decoyIn)
			}
		})
	}
}

// TestDeployScriptDryRunDowngradeWithDecoyFailsAtRuntime is the runtime
// counterpart of the static test above: it actually executes the
// reviewer's exact sabotage (real --dry-run flag downgraded to "none", with
// a decoy "--dry-run=server" glued onto the field-manager argument) through
// the fake-kubectl test harness, and proves the harness's own exact-argv
// safety sentinel (the "FAKE KUBECTL SAFETY VIOLATION" check in the apply)
// case) fires and aborts the run, independent of the static Go-side
// invariant entirely. This proves the fix is not merely a source-text
// lint: even if a sabotaged deploy.sh somehow slipped past the static
// check, this specific decoy shape would still be caught the moment it
// actually executes, because kubectl (and this fake) parses
// --field-manager=X--dry-run=server as one distinct, non-matching argv
// token - never as a --dry-run=server flag.
func TestDeployScriptDryRunDowngradeWithDecoyFailsAtRuntime(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)
	mutated := sabotageDryRunDowngradeWithDecoy(t, original, "field-manager")

	// The sabotaged copy must live alongside the real deploy.sh (not an
	// isolated temp dir) because deploy.sh locates the Helm chart via a
	// path relative to its own script directory
	// ($SCRIPT_DIR/../helm/selfservice); only the repo's deploy/scripts/
	// directory has that chart as a real sibling.
	scriptPath := filepath.Join("..", "..", "deploy", "scripts", "deploy-sabotaged-dry-run-decoy-test.sh")
	if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(scriptPath) })

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.scriptPath = scriptPath
	// Deliberately do NOT write env.upgradeHookManifest: validate_upgrade_hooks
	// early-returns (`[ ! -s "$unpinned_hooks" ]`) when there are no rendered
	// upgrade hooks, without ever calling kubectl_server_apply_dry_run. That
	// keeps this test's first (and only) real kubectl invocation the
	// server_validate_candidate call - the exact call site the "field-manager"
	// decoy was planted on - rather than validate_upgrade_hooks's own,
	// undecorated call, which (since the corrupted --dry-run flag lives in
	// the one shared helper) would otherwise trip the safety sentinel first
	// and make this test pass for the wrong reason, without ever exercising
	// the decoy shape it claims to prove.
	output, runErr := env.run("--no-pull", "--dry-run")
	if runErr == nil {
		t.Fatalf("sabotaged dry-run-downgrade-with-decoy script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "FAKE KUBECTL SAFETY VIOLATION") {
		t.Fatalf("sabotaged run did not trip the fake kubectl's exact-argv safety sentinel:\n%s", output)
	}
	if !strings.Contains(string(output), "crucible-production-deploy--dry-run=server") {
		t.Fatalf("sabotaged run's safety violation did not originate from the decoy-bearing server_validate_candidate call site:\n%s", output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("sabotaged script invoked Helm upgrade despite failing validation: %s", upgradeBody)
	}
}

// sabotageAppendConflictingDryRun returns a copy of the real deploy.sh with
// an additional, conflicting "--dry-run=none" token introduced into
// kubectlServerApplyDryRunFunctionName's body, WITHOUT touching the real,
// correct "--dry-run=server" line at all. This is the shape a second-round
// independent review reported as unreachable by a per-line-prefix scan:
// kubectl (like most flag parsers built on pflag) honors the LAST
// occurrence of a repeated flag, so an attacker does not need to remove or
// corrupt the real flag - it is enough to append a second, later (or even
// earlier) --dry-run token elsewhere, silently turning a validation-only
// dry-run into a real, mutating, --force-conflicts server-side apply
// against live production objects. shape selects where the conflicting
// token is introduced:
//
//   - "same-line": appended directly onto the end of the existing,
//     otherwise-legitimate `-o "$output_format"` line, introducing no new
//     line at all - the exact shape the review's example
//     (`-o "$output_format" --dry-run=none`) used, and the one a
//     line-*prefix* scan (which only inspects the start of each line)
//     could never see, since that line still begins with "-o", not
//     "--dry-run=".
//   - "own-line-before": inserted as its own new backslash-continued line
//     immediately BEFORE the real "--dry-run=server" line.
//   - "own-line-after": inserted as its own new backslash-continued line
//     immediately AFTER the real "--dry-run=server" line - the classic
//     "last flag wins" shape.
func sabotageAppendConflictingDryRun(t *testing.T, source, shape string) string {
	t.Helper()
	const outputFormatLine = "    -o \"$output_format\"\n"
	if n := strings.Count(source, outputFormatLine); n != 1 {
		t.Fatalf("expected exactly one occurrence of the -o \"$output_format\" line, found %d", n)
	}
	const realFlagLine = "    --dry-run=server \\\n"
	if n := strings.Count(source, realFlagLine); n != 1 {
		t.Fatalf("expected exactly one occurrence of the real --dry-run=server flag line, found %d", n)
	}

	var mutated string
	switch shape {
	case "same-line":
		mutated = strings.Replace(source, outputFormatLine, "    -o \"$output_format\" --dry-run=none\n", 1)
	case "own-line-before":
		mutated = strings.Replace(source, realFlagLine, "    --dry-run=none \\\n"+realFlagLine, 1)
	case "own-line-after":
		mutated = strings.Replace(source, realFlagLine, realFlagLine+"    --dry-run=none \\\n", 1)
	default:
		t.Fatalf("unknown shape %q", shape)
	}
	if mutated == source {
		t.Fatal("sabotage mutation had no effect")
	}
	return mutated
}

// TestDeployScriptForceConflictsInvariantDetectsConflictingDryRunAppend is
// the second-round independent review's exact structural sabotage: the
// real, correct --dry-run=server line in kubectlServerApplyDryRunFunctionName
// is left completely untouched, but a second, conflicting --dry-run=none
// token is appended elsewhere in the same function body - on the same line
// as another legitimate flag, or as its own line before or after the real
// one. checkForceConflictsInvariant must reject every one of these shapes,
// because it now requires the helper's entire body to be byte-for-byte
// identical to kubectlServerApplyDryRunExpectedBody - and an appended,
// duplicated, or reordered token of any kind makes that impossible,
// regardless of which line it landed on.
func TestDeployScriptForceConflictsInvariantDetectsConflictingDryRunAppend(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)
	if err := checkForceConflictsInvariant(original); err != nil {
		t.Fatalf("precondition failed: unmodified deploy.sh must satisfy the invariant: %v", err)
	}

	for _, shape := range []string{"same-line", "own-line-before", "own-line-after"} {
		t.Run(shape, func(t *testing.T) {
			mutated := sabotageAppendConflictingDryRun(t, original, shape)
			if err := checkForceConflictsInvariant(mutated); err == nil {
				t.Fatalf("invariant did not detect a conflicting, appended --dry-run=none token (%s shape) alongside the untouched real --dry-run=server line", shape)
			}
		})
	}
}

// TestDeployScriptConflictingDryRunAppendFailsAtRuntime is the runtime
// counterpart of the test above, for the exact "same-line" shape the
// review reported (`-o "$output_format" --dry-run=none`): it actually
// executes the sabotaged script through the fake-kubectl test harness and
// proves the harness's exact-cardinality safety sentinel fires,
// independent of the static Go-side invariant entirely. This proves real
// bash, executing the real (sabotaged) function body, actually passes
// kubectl two conflicting --dry-run tokens - not merely that the Go static
// scan disapproves of the source text.
func TestDeployScriptConflictingDryRunAppendFailsAtRuntime(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)
	mutated := sabotageAppendConflictingDryRun(t, original, "same-line")

	// The sabotaged copy must live alongside the real deploy.sh (not an
	// isolated temp dir) because deploy.sh locates the Helm chart via a
	// path relative to its own script directory
	// ($SCRIPT_DIR/../helm/selfservice); only the repo's deploy/scripts/
	// directory has that chart as a real sibling.
	scriptPath := filepath.Join("..", "..", "deploy", "scripts", "deploy-sabotaged-dry-run-conflicting-append-test.sh")
	if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(scriptPath) })

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.scriptPath = scriptPath
	// Deliberately do NOT write env.upgradeHookManifest: validate_upgrade_hooks
	// early-returns (`[ ! -s "$unpinned_hooks" ]`) when there are no rendered
	// upgrade hooks, without ever calling kubectl_server_apply_dry_run. That
	// keeps this test's first (and only) real kubectl invocation the
	// server_validate_candidate call, so any failure is unambiguously
	// attributable to the sabotaged helper body rather than an earlier,
	// unrelated call site sharing the same corrupted body.
	output, runErr := env.run("--no-pull", "--dry-run")
	if runErr == nil {
		t.Fatalf("sabotaged conflicting-dry-run-append script unexpectedly succeeded:\n%s", output)
	}
	if !strings.Contains(string(output), "FAKE KUBECTL SAFETY VIOLATION") {
		t.Fatalf("sabotaged run did not trip the fake kubectl's exact-cardinality safety sentinel:\n%s", output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("sabotaged script invoked Helm upgrade despite failing validation: %s", upgradeBody)
	}
}

// sabotageDefineKubectlShadowFunction returns a copy of the real deploy.sh
// with a `kubectl` shell function definition inserted near the top of the
// file, before kubectlServerApplyDryRunFunctionName is defined. bodyKind
// selects between bash's two interchangeable function-body forms - "brace"
// for `kubectl() { ... }` and "subshell" for `kubectl() ( ... )` - both of
// which bash accepts as a valid function definition and resolves
// identically. Bash resolves the bare, unqualified command name "kubectl"
// - which is how every kubectl invocation in deploy.sh was written prior to
// this round's `command kubectl` hardening - to a matching shell function
// before ever doing a PATH lookup for the real binary. This shadow function
// is a structural attack the whole-body-equality check
// (kubectlServerApplyDryRunExpectedBody) cannot see by construction: it
// never touches kubectlServerApplyDryRunFunctionName's own body at all, so
// that body remains byte-for-byte identical to the expected constant, yet a
// bare (non-`command`-prefixed) kubectl call reached elsewhere in the file
// would still be intercepted and could be silently rewritten at runtime
// (for example appending a conflicting --dry-run=none) - exactly the shape
// a fourth-round independent review reported as unreachable by any check
// that only inspects the helper's own text. A sixth-round independent
// review additionally reported that the subshell-bodied form specifically
// evaded an earlier version of the detection regex that matched only the
// brace-bodied form.
func sabotageDefineKubectlShadowFunction(t *testing.T, source, bodyKind string) string {
	t.Helper()
	const anchor = "kubectl_server_apply_dry_run() {\n"
	if n := strings.Count(source, anchor); n != 1 {
		t.Fatalf("expected exactly one occurrence of the %s definition, found %d", kubectlServerApplyDryRunFunctionName, n)
	}
	var shadow string
	switch bodyKind {
	case "brace":
		shadow = "kubectl() {\n  command kubectl \"$@\" --dry-run=none\n}\n\n"
	case "subshell":
		shadow = "kubectl() (\n  command kubectl \"$@\" --dry-run=none\n)\n\n"
	default:
		t.Fatalf("unknown bodyKind %q", bodyKind)
	}
	mutated := strings.Replace(source, anchor, shadow+anchor, 1)
	if mutated == source {
		t.Fatal("sabotage mutation had no effect")
	}
	return mutated
}

// TestDeployScriptForceConflictsInvariantDetectsKubectlShadowFunction is a
// fourth-round independent review's exact structural sabotage, extended by
// a sixth-round review to also cover the subshell-bodied form:
// kubectlServerApplyDryRunFunctionName's own body is left completely
// untouched (it still passes whole-body equality), but a `kubectl` shell
// function - either brace- or subshell-bodied - is defined elsewhere in the
// file. Since bash resolves the bare "kubectl" command name to a matching
// shell function before ever consulting PATH (for any invocation not
// itself prefixed with `command`), this shadow would silently intercept
// and could rewrite any such call reached elsewhere in the file.
// checkForceConflictsInvariant must reject this independent of the
// byte-equality check, which by construction cannot see it.
func TestDeployScriptForceConflictsInvariantDetectsKubectlShadowFunction(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)
	if err := checkForceConflictsInvariant(original); err != nil {
		t.Fatalf("precondition failed: unmodified deploy.sh must satisfy the invariant: %v", err)
	}

	for _, bodyKind := range []string{"brace", "subshell"} {
		t.Run(bodyKind, func(t *testing.T) {
			mutated := sabotageDefineKubectlShadowFunction(t, original, bodyKind)
			if bodyErr := func() error {
				body, _, _, extractErr := extractFunctionBody(stripBashLineComments(mutated), kubectlServerApplyDryRunFunctionName)
				if extractErr != nil {
					return extractErr
				}
				if body != kubectlServerApplyDryRunExpectedBody {
					return fmt.Errorf("sabotage unexpectedly changed %s()'s own body", kubectlServerApplyDryRunFunctionName)
				}
				return nil
			}(); bodyErr != nil {
				t.Fatalf("precondition failed: sabotage must leave %s()'s own body untouched: %v", kubectlServerApplyDryRunFunctionName, bodyErr)
			}

			if err := checkForceConflictsInvariant(mutated); err == nil {
				t.Fatalf("invariant did not detect a %s-bodied `kubectl` shadow function defined elsewhere in the file", bodyKind)
			}
		})
	}
}

// runKubectlServerApplyDryRunHelperArgv executes body - the literal,
// verbatim source text of a single bash function definition named
// kubectlServerApplyDryRunFunctionName (either the real one extracted from
// deploy.sh via extractFunctionBody, or a sabotaged copy) - in an isolated
// bash process with a minimal recording fake `kubectl` on PATH, calls the
// function with fieldManager/manifestPath/outputFormat, and returns the
// exact argv tokens the fake kubectl received, in the order bash actually
// passed them.
//
// This is a direct, execution-based proof, independent of
// checkForceConflictsInvariant (which only reasons about source text): it
// answers "what does bash actually do with this exact function body?"
// rather than "does this text look safe?" - so it cannot be fooled by any
// gap in the Go-side static scan, and is far simpler to reason about than
// growing that scan to cover every conceivable shape by hand.
func runKubectlServerApplyDryRunHelperArgv(t *testing.T, body, fieldManager, manifestPath, outputFormat string) []string {
	t.Helper()
	requirePOSIXShell(t)

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(dir, "argv.record")

	writeExecutable(t, filepath.Join(binDir, "kubectl"),
		"#!/bin/sh\nprintf '%s\\0' \"$@\" >> \"$ARGV_RECORD\"\nexit 0\n")

	harness := "#!/bin/bash\nset -euo pipefail\nNAMESPACE=test-namespace\n" +
		body +
		"kubectl_server_apply_dry_run \"$1\" \"$2\" \"$3\"\n"
	harnessPath := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harnessPath, harness)

	cmd := exec.Command("bash", harnessPath, fieldManager, manifestPath, outputFormat)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGV_RECORD="+recordPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harness invoking %s failed: %v\n%s", kubectlServerApplyDryRunFunctionName, err, output)
	}

	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("fake kubectl did not record any argv: %v", err)
	}
	tokens := strings.Split(string(recorded), "\x00")
	if len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1] // drop the trailing empty element after the final NUL
	}
	return tokens
}

// TestKubectlServerApplyDryRunHelperArgvIsExactAndImmutable directly
// executes the real, unmodified kubectl_server_apply_dry_run() body
// extracted from deploy.sh and proves the exact argv it passes to kubectl
// is precisely the required immutable prefix
// (apply/--server-side/--dry-run=server/--force-conflicts) followed by
// only the caller-supplied field-manager/namespace/manifest/output-format
// arguments, in that exact order, with nothing else. This is the
// execution-level counterpart to kubectlServerApplyDryRunExpectedBody: the
// static check proves the source text is exactly right, and this proves
// bash's actual behavior on that text matches too.
func TestKubectlServerApplyDryRunHelperArgvIsExactAndImmutable(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := extractFunctionBody(string(originalBytes), kubectlServerApplyDryRunFunctionName)
	if err != nil {
		t.Fatal(err)
	}

	argv := runKubectlServerApplyDryRunHelperArgv(t, body, "test-field-manager", "/tmp/some-manifest.yaml", "yaml")
	want := []string{
		"apply",
		"--server-side",
		"--dry-run=server",
		"--force-conflicts",
		"--field-manager=test-field-manager",
		"-n", "test-namespace",
		"-f", "/tmp/some-manifest.yaml",
		"-o", "yaml",
	}
	if len(argv) != len(want) {
		t.Fatalf("kubectl_server_apply_dry_run invoked kubectl with argv %q (%d tokens), want exactly %q (%d tokens)", argv, len(argv), want, len(want))
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q; full argv %q, want %q", i, argv[i], want[i], argv, want)
		}
	}
}

// TestKubectlServerApplyDryRunHelperArgvRejectsConflictingDryRunAppend
// directly executes a sabotaged kubectl_server_apply_dry_run() body - the
// real --dry-run=server line left untouched, but a second, conflicting
// --dry-run=none token appended in one of the three shapes covered by
// TestDeployScriptForceConflictsInvariantDetectsConflictingDryRunAppend -
// and proves the resulting real argv actually contains both conflicting
// tokens (not merely that the Go static scan disapproves of the source
// text). kubectl itself (like most flag parsers built on pflag) honors the
// LAST occurrence of a repeated flag, so an argv containing both
// "--dry-run=server" and a later "--dry-run=none" would, against a real
// kubectl, resolve to a real, mutating, --force-conflicts server-side
// apply - exactly the shape this whole design must prevent.
func TestKubectlServerApplyDryRunHelperArgvRejectsConflictingDryRunAppend(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)

	for _, shape := range []string{"same-line", "own-line-before", "own-line-after"} {
		t.Run(shape, func(t *testing.T) {
			mutated := sabotageAppendConflictingDryRun(t, original, shape)
			body, _, _, err := extractFunctionBody(mutated, kubectlServerApplyDryRunFunctionName)
			if err != nil {
				t.Fatal(err)
			}

			argv := runKubectlServerApplyDryRunHelperArgv(t, body, "test-field-manager", "/tmp/some-manifest.yaml", "yaml")

			dryRunTokens := 0
			sawServer := false
			sawNone := false
			for _, tok := range argv {
				if tok == "--dry-run=server" || strings.HasPrefix(tok, "--dry-run=") {
					dryRunTokens++
				}
				if tok == "--dry-run=server" {
					sawServer = true
				}
				if tok == "--dry-run=none" {
					sawNone = true
				}
			}
			if dryRunTokens == 1 {
				t.Fatalf("sabotaged %s shape did not actually produce a conflicting argv (found exactly 1 --dry-run token, expected 2): %q", shape, argv)
			}
			if !sawServer || !sawNone {
				t.Fatalf("sabotaged %s shape argv did not contain both the real --dry-run=server and the conflicting --dry-run=none token: %q", shape, argv)
			}
		})
	}
}

// kubectlBraceShadowFunctionBody and kubectlSubshellShadowFunctionBody are
// the two forms of a malicious `kubectl` shadow used by the executable
// tests below: bash accepts a function body written as either a `{ ... }`
// compound command or a `( ... )` subshell, and resolves the bare name
// "kubectl" to either one identically, ahead of any PATH lookup, for any
// invocation that is not itself prefixed with `command`. Both variants, if
// bash ever actually enters them, first create a marker file at
// $SHADOW_MARKER - so a test can prove whether bash actually ran the
// shadow, as opposed to merely defining it unused - and then call through
// to the real kubectl themselves, appending a conflicting --dry-run=none,
// to demonstrate concretely what such a shadow could do to any bare,
// unqualified `kubectl` call it manages to intercept.
const kubectlBraceShadowFunctionBody = "kubectl() {\n  : > \"$SHADOW_MARKER\"\n  command kubectl \"$@\" --dry-run=none\n}\n\n"
const kubectlSubshellShadowFunctionBody = "kubectl() (\n  : > \"$SHADOW_MARKER\"\n  command kubectl \"$@\" --dry-run=none\n)\n\n"

// runBashWithKubectlShadow executes a bash harness consisting of shadowDef
// (a `kubectl` shell-function shadow definition, or "" for none) followed
// by body (the literal, verbatim source text of a single bash function
// named kubectlServerApplyDryRunFunctionName - either the real one
// extracted from deploy.sh, or a deliberately unhardened variant) followed
// by a call to it, with a recording fake `kubectl` on PATH and a marker
// file the shadow touches if bash ever actually enters it. It returns the
// exact argv tokens the fake kubectl received (in call order) and whether
// the shadow's marker file was created - i.e. whether bash actually
// resolved the bare "kubectl" name inside body to the shadow rather than to
// the real fake kubectl on PATH.
//
// This is a direct, execution-based proof of what bash's own name
// resolution does with this exact shadow definition and this exact
// function body, independent of any Go-side static text analysis: it is
// the harness a sixth-round independent review asked for so that whether
// `command kubectl` actually bypasses a shadow is settled by running bash,
// not by reading the source and reasoning about it.
func runBashWithKubectlShadow(t *testing.T, shadowDef, body, fieldManager, manifestPath, outputFormat string) (argv []string, shadowInvoked bool) {
	t.Helper()
	requirePOSIXShell(t)

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(dir, "argv.record")
	markerPath := filepath.Join(dir, "shadow.marker")

	writeExecutable(t, filepath.Join(binDir, "kubectl"),
		"#!/bin/sh\nprintf '%s\\0' \"$@\" >> \"$ARGV_RECORD\"\nexit 0\n")

	harness := "#!/bin/bash\nset -euo pipefail\nNAMESPACE=test-namespace\n" +
		shadowDef +
		body +
		"kubectl_server_apply_dry_run \"$1\" \"$2\" \"$3\"\n"
	harnessPath := filepath.Join(dir, "harness.sh")
	writeExecutable(t, harnessPath, harness)

	cmd := exec.Command("bash", harnessPath, fieldManager, manifestPath, outputFormat)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ARGV_RECORD="+recordPath,
		"SHADOW_MARKER="+markerPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("harness invoking %s (shadow defined=%t) failed: %v\n%s", kubectlServerApplyDryRunFunctionName, shadowDef != "", err, output)
	}

	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("fake kubectl did not record any argv: %v", err)
	}
	tokens := strings.Split(string(recorded), "\x00")
	if len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1] // drop the trailing empty element after the final NUL
	}

	_, statErr := os.Stat(markerPath)
	return tokens, statErr == nil
}

// TestKubectlServerApplyDryRunHelperBypassesKubectlShadowFunctions is the
// executable, direct-execution counterpart of
// TestDeployScriptForceConflictsInvariantDetectsKubectlShadowFunction: it
// proves - by actually running bash, not by reasoning about source text -
// that the real, unmodified kubectl_server_apply_dry_run() body's use of
// `command kubectl` means a `kubectl` shell-function shadow defined
// immediately before it, in either a brace- or subshell-bodied form, is
// never entered at all. Both conditions are checked: the shadow's own
// marker file must never be created (bash never resolved the bare
// "kubectl" name inside the helper to the shadow), and the fake kubectl
// actually on PATH must still receive exactly the same immutable argv as
// when no shadow is defined at all
// (TestKubectlServerApplyDryRunHelperArgvIsExactAndImmutable). This is
// exactly what a sixth-round independent review asked to be proven at the
// execution level: that the wrapper is not merely absent from the recorded
// argv, but structurally never invoked in the first place.
func TestKubectlServerApplyDryRunHelperBypassesKubectlShadowFunctions(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := extractFunctionBody(string(originalBytes), kubectlServerApplyDryRunFunctionName)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"apply",
		"--server-side",
		"--dry-run=server",
		"--force-conflicts",
		"--field-manager=test-field-manager",
		"-n", "test-namespace",
		"-f", "/tmp/some-manifest.yaml",
		"-o", "yaml",
	}

	cases := []struct {
		name      string
		shadowDef string
	}{
		{"brace-bodied", kubectlBraceShadowFunctionBody},
		{"subshell-bodied", kubectlSubshellShadowFunctionBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, shadowInvoked := runBashWithKubectlShadow(t, tc.shadowDef, body, "test-field-manager", "/tmp/some-manifest.yaml", "yaml")
			if shadowInvoked {
				t.Fatalf("a %s `kubectl` shadow function was entered despite the real helper using `command kubectl`; recorded argv %q", tc.name, argv)
			}
			if len(argv) != len(want) {
				t.Fatalf("argv %q (%d tokens) does not match expected immutable argv %q (%d tokens) with a %s shadow defined", argv, len(argv), want, len(want), tc.name)
			}
			for i := range want {
				if argv[i] != want[i] {
					t.Fatalf("argv[%d] = %q, want %q (with a %s shadow defined); full argv %q", i, argv[i], want[i], tc.name, argv)
				}
			}
		})
	}
}

// TestKubectlShadowFunctionHarnessInterceptsBareKubectlCalls is a
// counterfactual control for
// TestKubectlServerApplyDryRunHelperBypassesKubectlShadowFunctions: it
// proves the shadow-function scaffolding used there is not a no-op, by
// running the exact same shadow definitions against a deliberately
// unhardened variant of the helper body - the real body with its
// "command kubectl apply" prefix stripped back down to a bare
// "kubectl apply", i.e. what this function looked like before this round's
// fix - and confirming that in that case the shadow's marker file IS
// created (bash really did resolve the bare "kubectl" name to the shadow)
// and the shadow's own conflicting --dry-run=none IS appended to the
// recorded argv. Without this control, a bug in runBashWithKubectlShadow
// that made it silently never invoke any shadow at all would make
// TestKubectlServerApplyDryRunHelperBypassesKubectlShadowFunctions pass for
// the wrong reason.
func TestKubectlShadowFunctionHarnessInterceptsBareKubectlCalls(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := extractFunctionBody(string(originalBytes), kubectlServerApplyDryRunFunctionName)
	if err != nil {
		t.Fatal(err)
	}
	const hardenedLine = "  command kubectl apply \\\n"
	if !strings.Contains(body, hardenedLine) {
		t.Fatalf("precondition failed: real helper body does not contain the expected %q line; got:\n%s", hardenedLine, body)
	}
	bareBody := strings.Replace(body, hardenedLine, "  kubectl apply \\\n", 1)
	if bareBody == body {
		t.Fatal("precondition failed: stripping the `command ` prefix had no effect")
	}

	cases := []struct {
		name      string
		shadowDef string
	}{
		{"brace-bodied", kubectlBraceShadowFunctionBody},
		{"subshell-bodied", kubectlSubshellShadowFunctionBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, shadowInvoked := runBashWithKubectlShadow(t, tc.shadowDef, bareBody, "test-field-manager", "/tmp/some-manifest.yaml", "yaml")
			if !shadowInvoked {
				t.Fatalf("%s shadow scaffolding did not intercept a bare `kubectl apply` call - the counterfactual control is broken; recorded argv %q", tc.name, argv)
			}
			sawNone := false
			for _, tok := range argv {
				if tok == "--dry-run=none" {
					sawNone = true
				}
			}
			if !sawNone {
				t.Fatalf("%s shadow was entered but did not append its own conflicting --dry-run=none to argv %q", tc.name, argv)
			}
		})
	}
}

// TestDeployScriptServerValidationSucceedsUnderRealisticOwnershipConflicts
// proves that server_validate_candidate, canonicalize_server_candidate, and
// validate_upgrade_hooks all still succeed (and the deploy completes) when
// every existing object they dry-run validate is already owned by another
// field manager ("helm"), which is the realistic production condition this
// fix addresses.
func TestDeployScriptServerValidationSucceedsUnderRealisticOwnershipConflicts(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.simulateOwnershipConflict = true
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, err := env.run("--no-pull")
	if err != nil {
		t.Fatalf("candidate validation failed despite --force-conflicts under simulated ownership conflicts: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "deployed exact source") {
		t.Fatalf("output %q does not indicate a completed deploy", output)
	}
	if !strings.Contains(string(output), "server-side dry-run validating immutable upgrade Helm hooks") {
		t.Fatalf("run did not reach upgrade-hook server validation:\n%s", output)
	}
	upgradeBody, readErr := os.ReadFile(env.upgradeLog)
	if readErr != nil || !strings.Contains(string(upgradeBody), "--atomic") {
		t.Fatalf("successful candidate did not use atomic Helm upgrade: err=%v body=%q", readErr, upgradeBody)
	}
	serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
	if serverReadErr != nil {
		t.Fatalf("could not read server dry-run log: %v", serverReadErr)
	}
	if !strings.Contains(string(serverDryRuns), "--force-conflicts") {
		t.Fatalf("server-side dry-run invocations did not carry --force-conflicts:\n%s", serverDryRuns)
	}
}

// TestDeployScriptServerValidateCandidateHandlesListWrappedMultiDocumentOutput
// is the regression test for the production incident this change fixes: a
// real `kubectl apply --server-side --dry-run=server -f <multi-document
// manifest> -o yaml` does not return the applied objects as `---`-separated
// top-level documents - it collapses them into one `apiVersion: v1, kind:
// List` wrapper with the individual objects nested under `items:`. Because
// manifest_workload_inventory only recognizes a document's own top-level
// (unindented) `kind:` line, handing it that List wrapper silently produces
// an empty inventory and require_core_workloads then fails with "missing
// image inventory for required chart workload ...", exactly as reported
// independently against live k3s. The fake kubectl used by this whole test
// file reproduces that exact List-wrapping behavior for any `-f` "-o yaml"
// apply of a manifest with more than one YAML document (see the `apply)`
// case's `-o yaml` branch), so baselineManifest - which always has multiple
// Deployment/CronJob/DaemonSet documents - now exercises the real bug
// whenever server_validate_candidate is handed the whole manifest in one
// call. This test proves the actual fix (splitting into individual
// documents before each dry-run call, then reassembling the results) lets a
// realistic multi-document candidate validate and deploy successfully, with
// every required workload present in the resulting inventory.
func TestDeployScriptServerValidateCandidateHandlesListWrappedMultiDocumentOutput(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, err := env.run("--no-pull")
	if err != nil {
		t.Fatalf("multi-document candidate validation unexpectedly failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "missing image inventory") {
		t.Fatalf("multi-document candidate hit the List-wrapping inventory bug:\n%s", output)
	}
	upgradeBody, readErr := os.ReadFile(env.upgradeLog)
	if readErr != nil || !strings.Contains(string(upgradeBody), "--atomic") {
		t.Fatalf("successful multi-document candidate did not use atomic Helm upgrade: err=%v body=%q", readErr, upgradeBody)
	}

	serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
	if serverReadErr != nil {
		t.Fatalf("could not read server dry-run log: %v", serverReadErr)
	}
	// The split must have actually happened: prove the log recorded more
	// than one distinct per-document "-f" manifest path for "-o yaml"
	// calls (not just one call against the whole candidate manifest).
	documentPaths := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(serverDryRuns), "\n"), "\n") {
		fields := strings.Fields(line)
		isYAML := false
		manifestPath := ""
		for i, field := range fields {
			if field == "-o" && i+1 < len(fields) && fields[i+1] == "yaml" {
				isYAML = true
			}
			if field == "-f" && i+1 < len(fields) {
				manifestPath = fields[i+1]
			}
		}
		if isYAML && splitDocumentFileNamePattern.MatchString(filepath.Base(manifestPath)) {
			documentPaths[manifestPath] = true
		}
	}
	if len(documentPaths) < 2 {
		t.Fatalf("expected server_validate_candidate to issue at least 2 distinct per-document dry-run calls for the multi-document candidate, saw %d:\n%s", len(documentPaths), serverDryRuns)
	}

	assertManifestImagesPinned(t, env.appliedManifest, testDigestB, testDigestB, env.extraRepository)
	applied, appliedErr := os.ReadFile(env.appliedManifest)
	if appliedErr != nil {
		t.Fatalf("successful candidate did not persist applied manifest: %v", appliedErr)
	}
	for _, workload := range []string{
		"name: selfservice-api",
		"name: selfservice-worker",
		"name: selfservice-engine",
		"name: selfservice-ui",
	} {
		if !strings.Contains(string(applied), workload) {
			t.Fatalf("applied manifest is missing required workload %q:\n%s", workload, applied)
		}
	}
}

// revertServerValidateCandidateToSingleCall returns a copy of source with
// server_validate_candidate() rewritten to its pre-fix implementation,
// which hands kubectl the whole candidate manifest in a single "-o yaml"
// dry-run call instead of splitting it into individual documents first.
// This is the exact shape of the bug this change fixes, and is used as the
// load-bearing counterfactual: a caller relying on this function's fix must
// fail exactly the way production did without it.
func revertServerValidateCandidateToSingleCall(t *testing.T, source string) string {
	t.Helper()
	marker := "server_validate_candidate() {"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatal("could not locate server_validate_candidate() in deploy.sh")
	}
	relativeEnd := strings.Index(source[start:], "\n}\n")
	if relativeEnd < 0 {
		t.Fatal("could not locate end of server_validate_candidate() in deploy.sh")
	}
	end := start + relativeEnd + len("\n}\n")
	const oldBody = `server_validate_candidate() {
  local manifest=$1
  local expected_map=$2
  local output=$3
  local canonical_output=$4
  echo "==> server-side dry-run validating the exact digest-pinned candidate"
  kubectl_server_apply_dry_run crucible-production-deploy "$manifest" yaml > "$output"
  validate_candidate_manifest "$output" "$expected_map" "$output.inventory"
  canonicalize_server_candidate "$manifest" "$canonical_output"
}
`
	if source[start:end] == oldBody {
		t.Fatal("server_validate_candidate() already matches the pre-fix single-call form; nothing to revert")
	}
	return source[:start] + oldBody + source[end:]
}

// TestDeployScriptServerValidateCandidateMultiDocumentSplitIsLoadBearing
// proves the per-document split in server_validate_candidate is not
// incidental: reverting it to the pre-fix single whole-manifest "-o yaml"
// call reproduces the exact reported production failure ("missing image
// inventory for required chart workload ...") against a realistic
// multi-document candidate, and Helm is never invoked.
func TestDeployScriptServerValidateCandidateMultiDocumentSplitIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)

	mutated := revertServerValidateCandidateToSingleCall(t, original)
	// See TestDeployScriptForceConflictsIsLoadBearing for why the sabotaged
	// copy must live alongside the real deploy.sh rather than an isolated
	// temp dir: it locates the Helm chart via a path relative to its own
	// script directory.
	scriptPath := filepath.Join(
		"..", "..", "deploy", "scripts",
		"deploy-sabotaged-server-validate-candidate-single-call-test.sh",
	)
	if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(scriptPath) })

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.scriptPath = scriptPath
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, runErr := env.run("--no-pull")
	if runErr == nil {
		t.Fatalf("sabotaged server_validate_candidate (single whole-manifest call) unexpectedly succeeded against a multi-document candidate:\n%s", output)
	}
	if !strings.Contains(string(output), "missing image inventory for required chart workload") {
		t.Fatalf("sabotaged server_validate_candidate did not reproduce the exact reported production failure:\n%s", output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("sabotaged server_validate_candidate invoked Helm upgrade despite failing validation: %s", upgradeBody)
	}
}

// TestDeployScriptServerValidateCandidateAbortsOnPerDocumentDryRunFailure
// proves that a failure validating any single document within the
// candidate manifest aborts the whole deploy before Helm is ever invoked -
// server_validate_candidate's per-document loop is a bare (unguarded) call
// relying on `set -euo pipefail` to propagate the failure immediately, and
// this proves that propagation actually happens at runtime, not just in
// the source text.
func TestDeployScriptServerValidateCandidateAbortsOnPerDocumentDryRunFailure(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.failServerValidateDocument = "selfservice-worker"
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, runErr := env.run("--no-pull")
	if runErr == nil {
		t.Fatalf("deploy unexpectedly succeeded despite a sabotaged per-document dry-run failure:\n%s", output)
	}
	if !strings.Contains(string(output), "sabotaged per-document server-side dry-run failure for selfservice-worker") {
		t.Fatalf("output did not surface the expected per-document dry-run failure:\n%s", output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("per-document dry-run failure did not abort before Helm upgrade: %s", upgradeBody)
	}
}

// TestDeployScriptForceConflictsIsLoadBearing proves --force-conflicts is
// not decorative: removing it from the single centralizing helper that all
// three server-side dry-run validation call sites depend on causes
// deploy.sh to fail under a realistic ownership conflict against existing
// Helm-owned objects, exactly as production would without this fix.
func TestDeployScriptForceConflictsIsLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)

	mutated := stripForceConflictsFromFunction(t, original, kubectlServerApplyDryRunFunctionName)
	// The sabotaged copy must live alongside the real deploy.sh (not an
	// isolated temp dir) because deploy.sh locates the Helm chart via a
	// path relative to its own script directory
	// ($SCRIPT_DIR/../helm/selfservice); only the repo's deploy/scripts/
	// directory has that chart as a real sibling.
	scriptPath := filepath.Join(
		"..", "..", "deploy", "scripts",
		"deploy-sabotaged-"+strings.ReplaceAll(kubectlServerApplyDryRunFunctionName, "_", "-")+"-test.sh",
	)
	if err := os.WriteFile(scriptPath, []byte(mutated), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(scriptPath) })

	live := baselineManifest(true, "", "false")
	candidate := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, live, candidate)
	env.scriptPath = scriptPath
	env.simulateOwnershipConflict = true
	writeFile(t, env.upgradeHookManifest, upgradeHookManifest("ghcr.io/jmal1/selfservice-api-gateway@sha256:"+testDigestB))

	output, runErr := env.run("--no-pull", "--dry-run")
	if runErr == nil {
		t.Fatalf("sabotaged %s (missing --force-conflicts) unexpectedly succeeded under a realistic ownership conflict:\n%s", kubectlServerApplyDryRunFunctionName, output)
	}
	if !strings.Contains(string(output), "conflict") {
		t.Fatalf("sabotaged %s failure did not surface the expected ownership-conflict error:\n%s", kubectlServerApplyDryRunFunctionName, output)
	}
	if upgradeBody, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(upgradeBody) > 0 {
		t.Fatalf("sabotaged %s invoked Helm upgrade despite failing validation: %s", kubectlServerApplyDryRunFunctionName, upgradeBody)
	}
}

// TestDeployScriptForceConflictsInvariantDetectsNeuteredAssignment proves
// the invariant is not satisfied by a disguised removal: rewriting the
// centralizing helper's legitimate "--force-conflicts \" line to
// "--force-conflicts=false \" (or "=true") still contains the literal
// substring "--force-conflicts", but "false" is kubectl's default for this
// boolean flag, so the assignment form is functionally identical to
// omitting the flag - and "=true", while functionally equivalent to the
// bare flag, is not the exact form this script is required to use. Either
// mutation must be caught exactly like an outright removal, not silently
// accepted because the substring is still present somewhere in the line.
func TestDeployScriptForceConflictsInvariantDetectsNeuteredAssignment(t *testing.T) {
	originalBytes, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	original := string(originalBytes)

	for _, value := range []string{"false", "true"} {
		t.Run(kubectlServerApplyDryRunFunctionName+"_assigned_"+value, func(t *testing.T) {
			mutated := assignForceConflictsInFunction(t, original, kubectlServerApplyDryRunFunctionName, value)
			if err := checkForceConflictsInvariant(mutated); err == nil {
				t.Fatalf("invariant did not detect --force-conflicts=%s replacing the bare flag in %s()", value, kubectlServerApplyDryRunFunctionName)
			}
		})
	}
}

// assignForceConflictsInFunction returns a copy of source with the bare
// "--force-conflicts \" flag line in exactly one named bash function's body
// rewritten to "--force-conflicts=<value> \", leaving every other
// occurrence (and the rest of the script) intact. It fails the test if the
// function or the flag cannot be located, or if the rewrite has no effect.
func assignForceConflictsInFunction(t *testing.T, source, functionName, value string) string {
	t.Helper()
	marker := functionName + "() {"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("could not locate function %s() in deploy.sh", functionName)
	}
	relativeEnd := strings.Index(source[start:], "\n}\n")
	if relativeEnd < 0 {
		t.Fatalf("could not locate end of function %s() in deploy.sh", functionName)
	}
	end := start + relativeEnd + len("\n}\n")
	body := source[start:end]
	if !forceConflictsFlagLine.MatchString(body) {
		t.Fatalf("function %s() does not contain a bare --force-conflicts flag line to rewrite", functionName)
	}
	rewritten := forceConflictsFlagLine.ReplaceAllString(body, "--force-conflicts="+value+" \\")
	if rewritten == body {
		t.Fatalf("rewriting --force-conflicts=%s in %s() had no effect", value, functionName)
	}
	if forceConflictsFlagLine.MatchString(rewritten) {
		t.Fatalf("rewriting --force-conflicts=%s in %s() left a residual bare occurrence", value, functionName)
	}
	return source[:start] + rewritten + source[end:]
}

// stripForceConflictsFromFunction returns a copy of source with the
// --force-conflicts flag line removed from exactly one named bash function's
// body, leaving every other occurrence (and the rest of the script) intact.
// It fails the test if the function or the flag cannot be located, or if
// removal has no effect, so this helper can never silently no-op.
func stripForceConflictsFromFunction(t *testing.T, source, functionName string) string {
	t.Helper()
	marker := functionName + "() {"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatalf("could not locate function %s() in deploy.sh", functionName)
	}
	relativeEnd := strings.Index(source[start:], "\n}\n")
	if relativeEnd < 0 {
		t.Fatalf("could not locate end of function %s() in deploy.sh", functionName)
	}
	end := start + relativeEnd + len("\n}\n")
	body := source[start:end]
	if !forceConflictsFlagLine.MatchString(body) {
		t.Fatalf("function %s() does not contain a --force-conflicts flag line to strip", functionName)
	}
	stripped := regexp.MustCompile(`(?m)^[ \t]*--force-conflicts \\\n`).ReplaceAllString(body, "")
	if stripped == body {
		t.Fatalf("stripping --force-conflicts from %s() had no effect", functionName)
	}
	if forceConflictsFlagLine.MatchString(stripped) {
		t.Fatalf("stripping --force-conflicts from %s() left a residual flag occurrence", functionName)
	}
	return source[:start] + stripped + source[end:]
}

func TestDeployScriptsAreExecutable(t *testing.T) {
	requirePOSIXShell(t)
	for _, path := range []string{
		filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"),
		filepath.Join("..", "..", "deploy", "scripts", "pin-baseline-images.sh"),
		filepath.Join("..", "..", "deploy", "scripts", "apply-exact-candidate.sh"),
		filepath.Join("..", "..", ".github", "scripts", "compute-image-matrix.sh"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable; documented ./deploy/scripts/deploy.sh commands would fail", path)
		}
	}
}

func TestProductionPushBuildsCompleteImageMatrix(t *testing.T) {
	requirePOSIXShell(t)
	script := filepath.Join("..", "..", ".github", "scripts", "compute-image-matrix.sh")
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(workflow), `type=raw,value=${{ github.sha }}`) {
		t.Fatal("image workflow does not publish an exact full-commit tag")
	}
	if !strings.Contains(string(workflow), "actions/upload-artifact@v4") ||
		!strings.Contains(string(workflow), "image-digest-${{ matrix.component }}") ||
		!strings.Contains(string(workflow), "${{ steps.build.outputs.digest }}") {
		t.Fatal("image workflow does not persist each run-bound component digest artifact")
	}
	pushStart := strings.Index(string(workflow), "  push:")
	pullRequestStart := strings.Index(string(workflow), "  pull_request:")
	if pushStart < 0 || pullRequestStart <= pushStart {
		t.Fatal("could not locate push and pull_request workflow triggers")
	}
	if strings.Contains(string(workflow)[pushStart:pullRequestStart], "paths-ignore:") {
		t.Fatal("main pushes can still skip the complete image workflow")
	}
	all := `["api-gateway","provision-worker","crucible-engine","synthetic-api-monitor","crucible-runner"]`
	for _, test := range []struct {
		name    string
		event   string
		changes string
		want    string
	}{
		{
			name:    "partial main change still publishes all deployment images",
			event:   "push",
			changes: `["api-gateway","go-tests"]`,
			want:    all,
		},
		{
			name:    "empty main matrix still publishes all deployment images",
			event:   "push",
			changes: `[]`,
			want:    all,
		},
		{
			name:    "pull request remains selective",
			event:   "pull_request",
			changes: `["api-gateway","go-tests"]`,
			want:    `["api-gateway"]`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := exec.Command("bash", script, test.event, test.changes, all).CombinedOutput()
			if err != nil {
				t.Fatalf("matrix helper failed: %v\n%s", err, output)
			}
			if strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("matrix = %q, want %q", output, test.want)
			}
		})
	}
}

func TestDeployWorkloadCanonicalizerKubectlCompatibility(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required to test the deploy workload canonicalizer")
	}

	scriptPaths, err := filepath.Glob(filepath.Join("..", "..", "deploy", "scripts", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, scriptPath := range scriptPaths {
		script, readErr := os.ReadFile(scriptPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(script), "toJson") {
			t.Fatalf("%s still relies on kubectl's non-portable toJson template helper", scriptPath)
		}
	}
	deployScript, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deployScript), `-o json \`) {
		t.Fatal("deploy script does not request portable JSON from kubectl")
	}

	fixture := filepath.Join(t.TempDir(), "deployment.json")
	writeFile(t, fixture, `{
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "metadata": {"name": "compatibility-fixture"},
  "spec": {
    "replicas": 1,
    "template": {
      "metadata": {"labels": {"app": "fixture"}},
      "spec": {
        "containers": [{"name": "fixture", "image": "example.invalid/fixture@sha256:`+testDigestA+`"}]
      }
    }
  }
}`)

	filter := filepath.Join("..", "..", "deploy", "scripts", "canonicalize-workload-spec.jq")
	canonicalize := func(t *testing.T, filterPath, kind, body string) ([]byte, error) {
		t.Helper()
		command := exec.Command(
			jq,
			"-cS",
			"-e",
			"--arg",
			"kind",
			kind,
			"--arg",
			"release",
			"selfservice",
			"--arg",
			"namespace",
			"selfservice",
			"-f",
			filterPath,
		)
		command.Stdin = strings.NewReader(body)
		return command.CombinedOutput()
	}
	input := fixture
	if kubectl, lookErr := exec.LookPath("kubectl"); lookErr == nil {
		kubectlOutput, kubectlErr := exec.Command(
			kubectl,
			"patch",
			"--local=true",
			"-f",
			fixture,
			"--type=merge",
			"-p",
			"{}",
			"-o",
			"json",
		).CombinedOutput()
		if kubectlErr != nil {
			t.Fatalf("real kubectl could not emit portable JSON: %v\n%s", kubectlErr, kubectlOutput)
		}
		input = filepath.Join(t.TempDir(), "kubectl-output.json")
		writeFile(t, input, string(kubectlOutput))
	}

	output, err := exec.Command(
		jq,
		"-cS",
		"-e",
		"--arg",
		"kind",
		"Deployment",
		"--arg",
		"release",
		"selfservice",
		"--arg",
		"namespace",
		"selfservice",
		"-f",
		filter,
		input,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("portable JSON canonicalization failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), `"containers"`) {
		t.Fatalf("canonical output omitted the pod spec: %s", output)
	}

	deployment := func(topAnnotations, templateAnnotations, replicas string) string {
		return fmt.Sprintf(`{
  "apiVersion": "apps/v1",
  "kind": "Deployment",
  "metadata": {"name": "fixture", "annotations": %s},
  "spec": {
    "replicas": %s,
    "selector": {"matchLabels": {"app": "fixture"}},
    "template": {
      "metadata": {"labels": {"app": "fixture"}, "annotations": %s},
      "spec": {
        "containers": [{"name": "fixture", "image": "example.invalid/fixture@sha256:%s"}]
      }
    }
  }
}`, topAnnotations, replicas, templateAnnotations, testDigestA)
	}
	daemonSet := func(topAnnotations, templateAnnotations string) string {
		return fmt.Sprintf(`{
  "apiVersion": "apps/v1",
  "kind": "DaemonSet",
  "metadata": {"name": "fixture", "annotations": %s},
  "spec": {
    "selector": {"matchLabels": {"app": "fixture"}},
    "template": {
      "metadata": {"labels": {"app": "fixture"}, "annotations": %s},
      "spec": {
        "containers": [{"name": "fixture", "image": "example.invalid/fixture@sha256:%s"}]
      }
    }
  }
}`, topAnnotations, templateAnnotations, testDigestA)
	}

	t.Run("ignores expected Helm ownership and Deployment revision annotations", func(t *testing.T) {
		live := deployment(`{
		  "deployment.kubernetes.io/revision":"163",
		  "meta.helm.sh/release-name":"selfservice",
		  "meta.helm.sh/release-namespace":"selfservice"
		}`, `{}`, "1")
		desired := deployment(`{}`, `{}`, "1")
		liveOutput, liveErr := canonicalize(t, filter, "Deployment", live)
		if liveErr != nil {
			t.Fatalf("live Deployment canonicalization failed: %v\n%s", liveErr, liveOutput)
		}
		desiredOutput, desiredErr := canonicalize(t, filter, "Deployment", desired)
		if desiredErr != nil {
			t.Fatalf("desired Deployment canonicalization failed: %v\n%s", desiredErr, desiredOutput)
		}
		if string(liveOutput) != string(desiredOutput) {
			t.Fatalf("top-level revision annotation caused false drift:\nlive: %s\ndesired: %s", liveOutput, desiredOutput)
		}

		filterBody, readErr := os.ReadFile(filter)
		if readErr != nil {
			t.Fatal(readErr)
		}
		const exception = `            del(."deployment.kubernetes.io/revision")`
		if strings.Count(string(filterBody), exception) != 1 {
			t.Fatalf("expected exactly one narrow Deployment revision exception")
		}
		withoutException := strings.Replace(string(filterBody), exception, `.`, 1)
		sabotagedFilter := filepath.Join(t.TempDir(), "canonicalize-workload-spec.jq")
		writeFile(t, sabotagedFilter, withoutException)
		sabotagedLive, sabotagedLiveErr := canonicalize(t, sabotagedFilter, "Deployment", live)
		if sabotagedLiveErr != nil {
			t.Fatalf("sabotaged live canonicalization failed unexpectedly: %v\n%s", sabotagedLiveErr, sabotagedLive)
		}
		sabotagedDesired, sabotagedDesiredErr := canonicalize(t, sabotagedFilter, "Deployment", desired)
		if sabotagedDesiredErr != nil {
			t.Fatalf("sabotaged desired canonicalization failed unexpectedly: %v\n%s", sabotagedDesiredErr, sabotagedDesired)
		}
		if string(sabotagedLive) == string(sabotagedDesired) {
			t.Fatal("removing the exact revision exception did not restore the false drift")
		}
	})

	t.Run("ignores expected Helm ownership and DaemonSet generation annotations", func(t *testing.T) {
		live := daemonSet(`{
		  "deprecated.daemonset.template.generation":"7",
		  "meta.helm.sh/release-name":"selfservice",
		  "meta.helm.sh/release-namespace":"selfservice"
		}`, `{}`)
		desired := daemonSet(`{}`, `{}`)
		liveOutput, liveErr := canonicalize(t, filter, "DaemonSet", live)
		if liveErr != nil {
			t.Fatalf("live DaemonSet canonicalization failed: %v\n%s", liveErr, liveOutput)
		}
		desiredOutput, desiredErr := canonicalize(t, filter, "DaemonSet", desired)
		if desiredErr != nil {
			t.Fatalf("desired DaemonSet canonicalization failed: %v\n%s", desiredErr, desiredOutput)
		}
		if string(liveOutput) != string(desiredOutput) {
			t.Fatalf("top-level generation annotation caused false drift:\nlive: %s\ndesired: %s", liveOutput, desiredOutput)
		}

		filterBody, readErr := os.ReadFile(filter)
		if readErr != nil {
			t.Fatal(readErr)
		}
		const exception = `            del(."deprecated.daemonset.template.generation")`
		if strings.Count(string(filterBody), exception) != 1 {
			t.Fatalf("expected exactly one narrow DaemonSet generation exception")
		}
		withoutException := strings.Replace(string(filterBody), exception, `.`, 1)
		sabotagedFilter := filepath.Join(t.TempDir(), "canonicalize-workload-spec.jq")
		writeFile(t, sabotagedFilter, withoutException)
		sabotagedLive, sabotagedLiveErr := canonicalize(t, sabotagedFilter, "DaemonSet", live)
		if sabotagedLiveErr != nil {
			t.Fatalf("sabotaged live canonicalization failed unexpectedly: %v\n%s", sabotagedLiveErr, sabotagedLive)
		}
		sabotagedDesired, sabotagedDesiredErr := canonicalize(t, sabotagedFilter, "DaemonSet", desired)
		if sabotagedDesiredErr != nil {
			t.Fatalf("sabotaged desired canonicalization failed unexpectedly: %v\n%s", sabotagedDesiredErr, sabotagedDesired)
		}
		if string(sabotagedLive) == string(sabotagedDesired) {
			t.Fatal("removing the exact generation exception did not restore the false drift")
		}
	})

	for _, test := range []struct {
		name        string
		annotations string
		wantOutput  string
	}{
		{
			name:        "rejects wrong Helm release name",
			annotations: `{"meta.helm.sh/release-name":"other-release"}`,
			wantOutput:  `.metadata.annotations["meta.helm.sh/release-name"] must equal "selfservice"`,
		},
		{
			name:        "rejects wrong Helm release namespace",
			annotations: `{"meta.helm.sh/release-namespace":"other-namespace"}`,
			wantOutput:  `.metadata.annotations["meta.helm.sh/release-namespace"] must equal "selfservice"`,
		},
		{
			name:        "rejects non-string Helm release name",
			annotations: `{"meta.helm.sh/release-name":163}`,
			wantOutput:  `.metadata.annotations["meta.helm.sh/release-name"] must be a string`,
		},
		{
			name:        "rejects non-string Helm release namespace",
			annotations: `{"meta.helm.sh/release-namespace":[]}`,
			wantOutput:  `.metadata.annotations["meta.helm.sh/release-namespace"] must be a string`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, canonicalErr := canonicalize(
				t,
				filter,
				"Deployment",
				deployment(test.annotations, `{}`, "1"),
			)
			if canonicalErr == nil {
				t.Fatalf("canonicalizer accepted malformed Helm ownership: %s", output)
			}
			if !strings.Contains(string(output), test.wantOutput) {
				t.Fatalf("canonicalizer output %q does not contain %q", output, test.wantOutput)
			}
		})
	}

	for _, test := range []struct {
		name    string
		kind    string
		live    string
		desired string
	}{
		{
			name:    "preserves different top-level annotation drift",
			live:    deployment(`{"owner":"operations"}`, `{}`, "1"),
			desired: deployment(`{"owner":"platform"}`, `{}`, "1"),
		},
		{
			name:    "preserves arbitrary Helm annotation drift",
			live:    deployment(`{"meta.helm.sh/other":"live"}`, `{}`, "1"),
			desired: deployment(`{"meta.helm.sh/other":"desired"}`, `{}`, "1"),
		},
		{
			name:    "preserves similarly named Deployment revision annotation drift",
			live:    deployment(`{"deployment.kubernetes.io/revision-note":"live"}`, `{}`, "1"),
			desired: deployment(`{"deployment.kubernetes.io/revision-note":"desired"}`, `{}`, "1"),
		},
		{
			name:    "preserves pod-template revision annotation drift",
			live:    deployment(`{}`, `{"deployment.kubernetes.io/revision":"163"}`, "1"),
			desired: deployment(`{}`, `{"deployment.kubernetes.io/revision":"164"}`, "1"),
		},
		{
			name: "preserves DaemonSet generation annotation on wrong kind",
			live: deployment(
				`{"deprecated.daemonset.template.generation":"7"}`,
				`{}`,
				"1",
			),
			desired: deployment(
				`{"deprecated.daemonset.template.generation":"8"}`,
				`{}`,
				"1",
			),
		},
		{
			name:    "preserves pod-template DaemonSet generation annotation drift",
			kind:    "DaemonSet",
			live:    daemonSet(`{}`, `{"deprecated.daemonset.template.generation":"7"}`),
			desired: daemonSet(`{}`, `{"deprecated.daemonset.template.generation":"8"}`),
		},
		{
			name:    "preserves similarly named DaemonSet generation annotation drift",
			kind:    "DaemonSet",
			live:    daemonSet(`{"deprecated.daemonset.template.generation-note":"live"}`, `{}`),
			desired: daemonSet(`{"deprecated.daemonset.template.generation-note":"desired"}`, `{}`),
		},
		{
			name:    "preserves Deployment revision annotation on DaemonSet",
			kind:    "DaemonSet",
			live:    daemonSet(`{"deployment.kubernetes.io/revision":"163"}`, `{}`),
			desired: daemonSet(`{"deployment.kubernetes.io/revision":"164"}`, `{}`),
		},
		{
			name:    "preserves ordinary Deployment spec drift",
			live:    deployment(`{}`, `{}`, "1"),
			desired: deployment(`{}`, `{}`, "2"),
		},
		{
			name: "preserves revision annotation drift on other kinds",
			kind: "StatefulSet",
			live: strings.Replace(
				deployment(`{"deployment.kubernetes.io/revision":"163"}`, `{}`, "1"),
				`"kind": "Deployment"`,
				`"kind": "StatefulSet"`,
				1,
			),
			desired: strings.Replace(
				deployment(`{"deployment.kubernetes.io/revision":"164"}`, `{}`, "1"),
				`"kind": "Deployment"`,
				`"kind": "StatefulSet"`,
				1,
			),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			kind := test.kind
			if kind == "" {
				kind = "Deployment"
			}
			liveOutput, liveErr := canonicalize(t, filter, kind, test.live)
			if liveErr != nil {
				t.Fatalf("live %s canonicalization failed: %v\n%s", kind, liveErr, liveOutput)
			}
			desiredOutput, desiredErr := canonicalize(t, filter, kind, test.desired)
			if desiredErr != nil {
				t.Fatalf("desired %s canonicalization failed: %v\n%s", kind, desiredErr, desiredOutput)
			}
			if string(liveOutput) == string(desiredOutput) {
				t.Fatal("canonicalizer ignored fail-closed workload drift")
			}
		})
	}

	liveJob := `{
  "kind": "Job",
  "spec": {
    "parallelism": 1,
    "completions": 1,
    "selector": {"matchLabels": {"controller-uid": "live-generated"}},
    "template": {
      "metadata": {
        "labels": {
          "app": "fixture",
          "batch.kubernetes.io/controller-uid": "live-generated",
          "batch.kubernetes.io/job-name": "live-name",
          "controller-uid": "live-generated",
          "job-name": "live-name"
        }
      },
      "spec": {
        "restartPolicy": "Never",
        "containers": [{"name": "fixture", "image": "example.invalid/fixture@sha256:` + testDigestA + `"}]
      }
    }
  }
}`
	desiredJob := strings.NewReplacer(
		"live-generated", "dry-run-generated",
		"live-name", "temporary-name",
	).Replace(liveJob)
	var canonicalJob []byte
	for index, job := range []string{liveJob, desiredJob} {
		cmd := exec.Command(
			jq,
			"-cS",
			"-e",
			"--arg",
			"kind",
			"Job",
			"--arg",
			"release",
			"selfservice",
			"--arg",
			"namespace",
			"selfservice",
			"-f",
			filter,
		)
		cmd.Stdin = strings.NewReader(job)
		jobOutput, jobErr := cmd.CombinedOutput()
		if jobErr != nil {
			t.Fatalf("Job canonicalization failed: %v\n%s", jobErr, jobOutput)
		}
		if strings.Contains(string(jobOutput), "generated") || strings.Contains(string(jobOutput), "temporary-name") {
			t.Fatalf("Job canonicalization retained server-generated identity: %s", jobOutput)
		}
		if !strings.Contains(string(jobOutput), `"labels":{"app":"fixture"}`) ||
			!strings.Contains(string(jobOutput), `"selector":null`) {
			t.Fatalf("Job canonicalization removed durable workload fields: %s", jobOutput)
		}
		if index == 0 {
			canonicalJob = jobOutput
		} else if string(jobOutput) != string(canonicalJob) {
			t.Fatalf("live and dry-run Jobs did not canonicalize equally:\nlive: %s\ndry-run: %s", canonicalJob, jobOutput)
		}
	}

	for _, invalid := range []struct {
		kind string
		body string
	}{
		{kind: "Deployment", body: `null`},
		{kind: "Deployment", body: `{"kind":"Deployment"}`},
		{kind: "Deployment", body: `{"kind":"Deployment","spec":null}`},
		{kind: "Deployment", body: `{"kind":"CronJob","spec":{}}`},
		{kind: "Deployment", body: `{"kind":"Deployment","metadata":[],"spec":{}}`},
		{kind: "Deployment", body: `{"kind":"Deployment","metadata":{"annotations":[]},"spec":{}}`},
		{kind: "Job", body: `{"kind":"Job","spec":{"template":{"metadata":{},"spec":null}}}`},
		{kind: "Job", body: `{"kind":"Job","spec":{"template":{"metadata":{"labels":[]},"spec":{}}}}`},
	} {
		invalidPath := filepath.Join(t.TempDir(), "invalid.json")
		writeFile(t, invalidPath, invalid.body)
		if badOutput, badErr := exec.Command(
			jq,
			"-cS",
			"-e",
			"--arg",
			"kind",
			invalid.kind,
			"--arg",
			"release",
			"selfservice",
			"--arg",
			"namespace",
			"selfservice",
			"-f",
			filter,
			invalidPath,
		).CombinedOutput(); badErr == nil {
			t.Fatalf("canonicalizer accepted invalid %s resource %s: %s", invalid.kind, invalid.body, badOutput)
		}
	}
}

func TestDeployWorkerReplicaGateKubectlCompatibility(t *testing.T) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatal("kubectl is required to test the deploy worker replica gate")
	}
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Fatal("jq is required to test the deploy worker replica gate")
	}

	deployScript, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(deployScript), `live_replicas="$(worker_replicas_from_manifest "$live_worker")"`) {
		t.Fatal("deploy script still parses the server-side worker replica count from YAML")
	}

	fixture := filepath.Join(t.TempDir(), "worker-deployment.yaml")
	writeFile(t, fixture, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker-replica-fixture
spec:
  replicas: 1
  selector:
    matchLabels:
      app: worker-replica-fixture
  template:
    metadata:
      labels:
        app: worker-replica-fixture
    spec:
      containers:
      - name: worker
        image: example.invalid/worker@sha256:`+testDigestA+`
`)

	resource, err := exec.Command(
		kubectl,
		"patch",
		"--local=true",
		"-f",
		fixture,
		"--type=merge",
		"-p",
		"{}",
		"-o",
		"json",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("real kubectl could not emit the worker Deployment as JSON: %v\n%s", err, resource)
	}

	filter := filepath.Join("..", "..", "deploy", "scripts", "require-single-worker-replica.jq")
	tests := []struct {
		name        string
		transform   string
		wantSuccess bool
	}{
		{name: "integer one", transform: ".", wantSuccess: true},
		{name: "missing", transform: "del(.spec.replicas)"},
		{name: "null", transform: ".spec.replicas = null"},
		{name: "string", transform: `.spec.replicas = "1"`},
		{name: "zero", transform: ".spec.replicas = 0"},
		{name: "two", transform: ".spec.replicas = 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transform := exec.Command(jq, "-c", test.transform)
			transform.Stdin = strings.NewReader(string(resource))
			input, transformErr := transform.CombinedOutput()
			if transformErr != nil {
				t.Fatalf("could not create replica-count fixture: %v\n%s", transformErr, input)
			}

			command := exec.Command(jq, "-er", "-f", filter)
			command.Stdin = strings.NewReader(string(input))
			output, filterErr := command.CombinedOutput()
			if test.wantSuccess {
				if filterErr != nil {
					t.Fatalf("replicas=1 was rejected: %v\n%s", filterErr, output)
				}
				if strings.TrimSpace(string(output)) != "1" {
					t.Fatalf("replicas=1 produced %q, not 1", output)
				}
			} else if filterErr == nil {
				t.Fatalf("sabotaged replica count unexpectedly passed: %s", output)
			}
		})
	}
}

// renderedWorkerReplicasPreFixBody is the pre-fix implementation of
// rendered_worker_replicas_from_manifest(): it matched any "  replicas:"
// line in the rendered worker Deployment document regardless of which
// top-level YAML section ("spec:" or "status:") it fell under. A real
// server-side dry-run apply against an already-live worker Deployment
// merges in the existing "status:" block, whose "replicas:" field sits at
// the exact same two-space indent as spec.replicas, so this old parser saw
// two matches for a perfectly valid manifest and failed closed ("renders X
// workers, not exactly 1") even though spec.replicas was unambiguous. It is
// kept here, verbatim, only to prove the fix below is load-bearing.
const renderedWorkerReplicasPreFixBody = `rendered_worker_replicas_from_manifest() {
  local manifest=$1
  extract_workload_manifest "$manifest" Deployment "$RELEASE-worker" \
    | awk '
        /^  replicas:[[:space:]]*/ {
          matches++
          value = $0
          sub(/^  replicas:[[:space:]]*/, "", value)
          gsub(/^["'"'"']|["'"'"']$/, "", value)
        }
        END {
          if (matches != 1 || value == "") {
            exit 3
          }
          print value
        }
      '
}
`

// TestDeployScriptRenderedWorkerReplicasFromManifestIsSpecScoped proves that
// rendered_worker_replicas_from_manifest reads exactly spec.replicas from a
// rendered worker Deployment manifest, even when the manifest also carries a
// server-populated "status:" section (the shape produced by a real
// server-side dry-run apply against an already-live Deployment, where the
// server merges in existing status). It also proves the parser still
// rejects a manifest missing spec.replicas or with a duplicated
// spec.replicas, and it reverts the parser to its pre-fix, section-unaware
// form to prove the fix is load-bearing: without it, the exact same
// realistic spec+status manifest is wrongly rejected.
func TestDeployScriptRenderedWorkerReplicasFromManifestIsSpecScoped(t *testing.T) {
	requirePOSIXShell(t)

	source, err := os.ReadFile(filepath.Join("..", "..", "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}

	extractWorkloadManifestBody, _, _, err := extractFunctionBody(string(source), "extract_workload_manifest")
	if err != nil {
		t.Fatal(err)
	}
	currentBody, _, _, err := extractFunctionBody(string(source), "rendered_worker_replicas_from_manifest")
	if err != nil {
		t.Fatal(err)
	}
	if currentBody == renderedWorkerReplicasPreFixBody {
		t.Fatal("rendered_worker_replicas_from_manifest still matches its pre-fix, section-unaware implementation")
	}

	// A realistic server-side dry-run render of an already-live worker
	// Deployment: the server merges in a "status:" block whose "replicas:"
	// field sits at the identical two-space indent as spec.replicas.
	specAndStatus := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
  namespace: selfservice
  uid: 11111111-1111-1111-1111-111111111111
  resourceVersion: "999"
spec:
  replicas: 1
  selector:
    matchLabels:
      app: selfservice-worker
  template:
    metadata:
      labels:
        app: selfservice-worker
    spec:
      containers:
      - name: worker
        image: example.invalid/worker@sha256:` + testDigestA + `
status:
  observedGeneration: 5
  replicas: 1
  updatedReplicas: 1
  readyReplicas: 1
  availableReplicas: 1
  conditions:
  - type: Available
    status: "True"
`

	missingSpecReplicas := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
spec:
  selector:
    matchLabels:
      app: selfservice-worker
  template:
    metadata:
      labels:
        app: selfservice-worker
    spec:
      containers:
      - name: worker
        image: example.invalid/worker@sha256:` + testDigestA + `
status:
  replicas: 1
`

	duplicateSpecReplicas := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: selfservice-worker
spec:
  replicas: 1
  replicas: 2
  selector:
    matchLabels:
      app: selfservice-worker
  template:
    metadata:
      labels:
        app: selfservice-worker
    spec:
      containers:
      - name: worker
        image: example.invalid/worker@sha256:` + testDigestA + `
status:
  replicas: 1
`

	run := func(t *testing.T, replicaFnBody, manifest string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		manifestPath := filepath.Join(dir, "manifest.yaml")
		writeFile(t, manifestPath, manifest)
		// A tiny, self-contained harness: just the two real functions
		// under test (extract_workload_manifest plus whichever
		// rendered_worker_replicas_from_manifest body is being
		// exercised) and the RELEASE variable they depend on, run
		// directly against a fixture file. This avoids needing the
		// full deploy.sh run (live cluster stubs, Helm, --ui-source-sha)
		// for a bug isolated entirely to text parsing.
		harness := "#!/bin/bash\nset -euo pipefail\nRELEASE=selfservice\n\n" +
			extractWorkloadManifestBody + "\n" + replicaFnBody +
			"\nrendered_worker_replicas_from_manifest \"$1\"\n"
		harnessPath := filepath.Join(dir, "harness.sh")
		writeFile(t, harnessPath, harness)
		out, runErr := exec.Command("bash", harnessPath, manifestPath).CombinedOutput()
		return strings.TrimSpace(string(out)), runErr
	}

	t.Run("accepts spec.replicas alongside a status.replicas section", func(t *testing.T) {
		out, runErr := run(t, currentBody, specAndStatus)
		if runErr != nil {
			t.Fatalf("fixed parser rejected a realistic spec+status render: %v\n%s", runErr, out)
		}
		if out != "1" {
			t.Fatalf("fixed parser returned %q, want \"1\"", out)
		}
	})

	t.Run("rejects a manifest with no spec.replicas", func(t *testing.T) {
		if out, runErr := run(t, currentBody, missingSpecReplicas); runErr == nil {
			t.Fatalf("fixed parser accepted a manifest with no spec.replicas: %s", out)
		}
	})

	t.Run("rejects a manifest with duplicated spec.replicas", func(t *testing.T) {
		if out, runErr := run(t, currentBody, duplicateSpecReplicas); runErr == nil {
			t.Fatalf("fixed parser accepted a manifest with duplicated spec.replicas: %s", out)
		}
	})

	t.Run("pre-fix parser fails the exact production shape (load-bearing)", func(t *testing.T) {
		out, runErr := run(t, renderedWorkerReplicasPreFixBody, specAndStatus)
		if runErr == nil {
			t.Fatalf("pre-fix, section-unaware parser unexpectedly accepted a spec+status render: %s -- fix is not load-bearing", out)
		}
	})
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

func writeDeployLockFixture(
	t *testing.T,
	env *deployScriptEnvironment,
	hostname string,
	pid int,
	created time.Time,
	uid string,
	resourceVersion string,
) {
	t.Helper()
	holder := fmt.Sprintf("%s|%d|%s", hostname, pid, created.UTC().Format("2006-01-02T15:04:05Z"))
	writeDeployLockFixtureWithHolder(t, env, holder, created, uid, resourceVersion)
}

func writeLegacyDeployLockFixture(
	t *testing.T,
	env *deployScriptEnvironment,
	hostname string,
	pid int,
	created time.Time,
	uid string,
	resourceVersion string,
) {
	t.Helper()
	holder := fmt.Sprintf("%s-%d-%s", hostname, pid, created.UTC().Format("20060102T150405Z"))
	writeDeployLockFixtureWithHolder(t, env, holder, created, uid, resourceVersion)
}

func writeDeployLockFixtureWithHolder(
	t *testing.T,
	env *deployScriptEnvironment,
	holder string,
	created time.Time,
	uid string,
	resourceVersion string,
) {
	t.Helper()
	writeFile(t, env.lockFile, fmt.Sprintf(`{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "metadata": {
    "name": "selfservice-phase1-deploy-lock",
    "namespace": "selfservice",
    "creationTimestamp": %q,
    "uid": %q,
    "resourceVersion": %q,
    "annotations": {
      "crucible.jmal.io/holder": %q
    }
  }
}
`, created.UTC().Format("2006-01-02T15:04:05Z"), uid, resourceVersion, holder))
}

func TestDeployScriptStaleReleaseLockRecovery(t *testing.T) {
	requirePOSIXShell(t)
	manifest := baselineManifest(true, "*", "false")
	localHostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const deadPID = 99999999

	runPrepare := func(env *deployScriptEnvironment) ([]byte, error) {
		return env.run(
			"--prepare-claims-baseline",
			"--baseline-chart-dir",
			env.chartDir,
		)
	}
	assertNoUpgrade := func(t *testing.T, env *deployScriptEnvironment) {
		t.Helper()
		if body, readErr := os.ReadFile(env.upgradeLog); readErr == nil && len(body) > 0 {
			t.Fatalf("unsafe lock recovery reached Helm upgrade: %s", body)
		}
	}
	assertBackup := func(t *testing.T, env *deployScriptEnvironment, expected []byte) {
		t.Helper()
		entries, readErr := os.ReadDir(env.lockBackupDir)
		if readErr != nil {
			t.Fatalf("stale lock was not backed up: %v", readErr)
		}
		if len(entries) != 1 {
			t.Fatalf("stale lock backup count = %d, want 1", len(entries))
		}
		backup, readErr := os.ReadFile(filepath.Join(env.lockBackupDir, entries[0].Name()))
		if readErr != nil {
			t.Fatalf("read stale lock backup: %v", readErr)
		}
		if !bytes.Equal(bytes.TrimSpace(backup), bytes.TrimSpace(expected)) {
			t.Fatalf("stale lock backup differs from the exact observed ConfigMap:\nwant:\n%s\ngot:\n%s", expected, backup)
		}
	}

	t.Run("young same-host live holder blocks", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, os.Getpid(), now, "live-lock-uid", "41")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("young live lock was reclaimed:\n%s", output)
		}
		if !strings.Contains(string(output), "live local PID") {
			t.Fatalf("failure did not identify live same-host holder:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("young same-host dead holder backs up and reclaims", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, deadPID, now, "dead-lock-uid", "42")
		expected, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		output, runErr := runPrepare(env)
		if runErr != nil {
			t.Fatalf("dead same-host lock was not reclaimed: %v\n%s", runErr, output)
		}
		assertBackup(t, env, expected)
	})

	t.Run("young remote-host holder blocks", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, "other-deploy-host", deadPID, now, "remote-lock-uid", "43")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("young remote lock was reclaimed:\n%s", output)
		}
		if !strings.Contains(string(output), "remote liveness is ambiguous") {
			t.Fatalf("failure did not identify ambiguous remote holder:\n%s", output)
		}
		if !strings.Contains(string(output), "21600-second lease") {
			t.Fatalf("default six-hour TTL is not load-bearing in remote-host fencing:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("expired same-host live holder blocks", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, os.Getpid(), now.Add(-7*time.Hour), "expired-lock-uid", "44")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("expired same-host live lock was reclaimed:\n%s", output)
		}
		if !strings.Contains(string(output), "regardless of lease age") {
			t.Fatalf("failure did not document the live-PID safety override:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("expired remote-host holder backs up and reclaims", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, "other-deploy-host", deadPID, now.Add(-7*time.Hour), "expired-remote-lock-uid", "44a")
		expected, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		output, runErr := runPrepare(env)
		if runErr != nil {
			t.Fatalf("expired remote lock was not reclaimed: %v\n%s", runErr, output)
		}
		if !strings.Contains(string(output), "lease expired") {
			t.Fatalf("recovery did not document remote lease expiry:\n%s", output)
		}
		assertBackup(t, env, expected)
	})

	t.Run("legacy young same-host live holder blocks", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeLegacyDeployLockFixture(t, env, localHostname, os.Getpid(), now, "legacy-live-lock-uid", "44b")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("legacy live lock was reclaimed:\n%s", output)
		}
		if !strings.Contains(string(output), "live local PID") {
			t.Fatalf("failure did not identify legacy live same-host holder:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("legacy young same-host dead holder backs up and reclaims", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeLegacyDeployLockFixture(t, env, localHostname, deadPID, now, "legacy-dead-lock-uid", "44c")
		expected, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		output, runErr := runPrepare(env)
		if runErr != nil {
			t.Fatalf("legacy dead same-host lock was not reclaimed: %v\n%s", runErr, output)
		}
		assertBackup(t, env, expected)
	})

	t.Run("legacy expired same-host live holder blocks", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeLegacyDeployLockFixture(t, env, localHostname, os.Getpid(), now.Add(-7*time.Hour), "legacy-expired-live-lock-uid", "44d")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("legacy expired same-host live lock was reclaimed:\n%s", output)
		}
		if !strings.Contains(string(output), "regardless of lease age") {
			t.Fatalf("failure did not document the legacy live-PID safety override:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("legacy expired remote-host holder backs up and reclaims", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeLegacyDeployLockFixture(t, env, "legacy-remote-deploy-host", deadPID, now.Add(-7*time.Hour), "legacy-expired-remote-lock-uid", "44e")
		expected, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		output, runErr := runPrepare(env)
		if runErr != nil {
			t.Fatalf("legacy expired remote lock was not reclaimed: %v\n%s", runErr, output)
		}
		assertBackup(t, env, expected)
	})

	for _, malformedHolder := range []struct {
		name   string
		holder string
	}{
		{name: "legacy missing PID", holder: "legacyhost-20260101T000000Z"},
		{name: "legacy zero PID", holder: "legacyhost-0-20260101T000000Z"},
		{name: "legacy timestamp separators", holder: "legacyhost-99999999-2026-01-01T00:00:00Z"},
		{name: "legacy impossible timestamp", holder: "legacyhost-99999999-20269999T999999Z"},
	} {
		t.Run("malformed "+malformedHolder.name+" blocks", func(t *testing.T) {
			env := newDeployScriptEnvironment(t, manifest, manifest)
			writeDeployLockFixtureWithHolder(t, env, malformedHolder.holder, now, "legacy-malformed-uid", "44f")
			output, runErr := runPrepare(env)
			if runErr == nil {
				t.Fatalf("malformed legacy lock was reclaimed:\n%s", output)
			}
			if !strings.Contains(string(output), "malformed") {
				t.Fatalf("malformed legacy lock failed for wrong reason:\n%s", output)
			}
			assertNoUpgrade(t, env)
		})
	}

	for _, malformed := range []struct {
		name            string
		hostname        string
		pid             int
		created         time.Time
		uid             string
		resourceVersion string
		mutate          func(string) string
	}{
		{name: "holder", hostname: "bad|host", pid: deadPID, created: now, uid: "uid", resourceVersion: "45"},
		{
			name:            "timestamp",
			hostname:        localHostname,
			pid:             deadPID,
			created:         now,
			uid:             "uid",
			resourceVersion: "46",
			mutate: func(body string) string {
				return strings.Replace(body, now.Format("2006-01-02T15:04:05Z"), "not-a-time", 1)
			},
		},
		{
			name:            "holder timestamp",
			hostname:        localHostname,
			pid:             deadPID,
			created:         now,
			uid:             "uid",
			resourceVersion: "46a",
			mutate: func(body string) string {
				index := strings.LastIndex(body, now.Format("2006-01-02T15:04:05Z"))
				if index < 0 {
					t.Fatal("holder timestamp fixture missing")
				}
				return body[:index] + "2026-99-99T99:99:99Z" + body[index+len(now.Format("2006-01-02T15:04:05Z")):]
			},
		},
		{name: "uid", hostname: localHostname, pid: deadPID, created: now, uid: "", resourceVersion: "47"},
		{name: "resourceVersion", hostname: localHostname, pid: deadPID, created: now, uid: "uid", resourceVersion: ""},
	} {
		t.Run("malformed "+malformed.name+" blocks", func(t *testing.T) {
			env := newDeployScriptEnvironment(t, manifest, manifest)
			writeDeployLockFixture(t, env, malformed.hostname, malformed.pid, malformed.created, malformed.uid, malformed.resourceVersion)
			if malformed.mutate != nil {
				body, readErr := os.ReadFile(env.lockFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				writeFile(t, env.lockFile, malformed.mutate(string(body)))
			}
			output, runErr := runPrepare(env)
			if runErr == nil {
				t.Fatalf("malformed lock was reclaimed:\n%s", output)
			}
			if !strings.Contains(string(output), "malformed") {
				t.Fatalf("malformed lock failed for wrong reason:\n%s", output)
			}
			assertNoUpgrade(t, env)
		})
	}

	t.Run("backup failure blocks deletion", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, deadPID, now, "backup-lock-uid", "48")
		writeFile(t, env.lockBackupDir, "not a directory")
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("backup failure did not block stale-lock deletion:\n%s", output)
		}
		if _, statErr := os.Stat(env.lockFile); statErr != nil {
			t.Fatalf("backup failure deleted the lock: %v\n%s", statErr, output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("UID resourceVersion race blocks deletion", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, deadPID, now, "raced-lock-uid", "49")
		expected, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		env.lockDeleteRaceMode = "both"
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("lock replacement race was deleted:\n%s", output)
		}
		body, readErr := os.ReadFile(env.lockFile)
		if readErr != nil {
			t.Fatalf("replacement lock was deleted: %v\n%s", readErr, output)
		}
		if !strings.Contains(string(body), "replacement-lock-uid") {
			t.Fatalf("race did not preserve the replacement lock:\n%s\n%s", body, output)
		}
		assertBackup(t, env, expected)
		assertNoUpgrade(t, env)
	})

	t.Run("TTL below guarded runtime is rejected", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		env.lockTTLSeconds = "21599"
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("undersized lock TTL was accepted:\n%s", output)
		}
		if !strings.Contains(string(output), "21600 through 604800") {
			t.Fatalf("undersized TTL failed for the wrong reason:\n%s", output)
		}
		assertNoUpgrade(t, env)
	})

	t.Run("oversized TTL cannot overflow into stale classification", func(t *testing.T) {
		env := newDeployScriptEnvironment(t, manifest, manifest)
		writeDeployLockFixture(t, env, localHostname, os.Getpid(), now, "overflow-lock-uid", "50")
		env.lockTTLSeconds = "999999999999999999999999999999999999"
		output, runErr := runPrepare(env)
		if runErr == nil {
			t.Fatalf("oversized lock TTL was accepted:\n%s", output)
		}
		if !strings.Contains(string(output), "21600 through 604800") {
			t.Fatalf("oversized TTL failed for the wrong reason:\n%s", output)
		}
		body, readErr := os.ReadFile(env.lockFile)
		if readErr != nil || !strings.Contains(string(body), "overflow-lock-uid") {
			t.Fatalf("oversized TTL deleted or replaced the live lock: err=%v body=%s\n%s", readErr, body, output)
		}
		assertNoUpgrade(t, env)
	})
}

func TestDeployScriptStaleLockGuardsAreLoadBearing(t *testing.T) {
	requirePOSIXShell(t)
	sourcePath := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	manifest := baselineManifest(true, "*", "false")
	localHostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	runPrepare := func(env *deployScriptEnvironment) ([]byte, error) {
		return env.run(
			"--prepare-claims-baseline",
			"--baseline-chart-dir",
			env.chartDir,
		)
	}
	runSabotage := func(t *testing.T, old, replacement string, configure func(*deployScriptEnvironment)) {
		t.Helper()
		if strings.Count(source, old) != 1 {
			t.Fatalf("sabotage target %q count != 1", old)
		}
		scriptPath := filepath.Join(
			"..", "..", "deploy", "scripts",
			"deploy-sabotaged-lock-"+strings.ReplaceAll(t.Name(), "/", "-")+".sh",
		)
		writeFile(t, scriptPath, strings.Replace(source, old, replacement, 1))
		t.Cleanup(func() { os.Remove(scriptPath) })
		if err := os.Chmod(scriptPath, 0o755); err != nil {
			t.Fatal(err)
		}
		env := newDeployScriptEnvironment(t, manifest, manifest)
		env.scriptPath = scriptPath
		configure(env)
		output, runErr := runPrepare(env)
		if runErr != nil {
			t.Fatalf("sabotaged guard did not permit the unsafe mutation, so the load-bearing test is not exercising it: %v\n%s", runErr, output)
		}
	}

	t.Run("same-host liveness", func(t *testing.T) {
		runSabotage(
			t,
			`    if [ -d "/proc/$HELM_RELEASE_LOCK_OBSERVED_PID" ] ||
       kill -0 "$HELM_RELEASE_LOCK_OBSERVED_PID" 2>/dev/null; then`,
			"    if false; then",
			func(env *deployScriptEnvironment) {
				writeDeployLockFixture(t, env, localHostname, os.Getpid(), time.Now(), "live-sabotage-uid", "51")
			},
		)
	})

	t.Run("lease TTL", func(t *testing.T) {
		runSabotage(
			t,
			`  else
    if [ "$lock_age" -lt "$HELM_RELEASE_LOCK_TTL_SECONDS" ]; then`,
			`  else
    if false; then`,
			func(env *deployScriptEnvironment) {
				writeDeployLockFixture(t, env, "remote-deploy-host", 99999999, time.Now(), "ttl-sabotage-uid", "52")
			},
		)
	})

	t.Run("all UID resourceVersion preconditions", func(t *testing.T) {
		runSabotage(
			t,
			`        preconditions: {
          uid: $uid,
          resourceVersion: $resource_version
        }`,
			"        preconditions: {}",
			func(env *deployScriptEnvironment) {
				writeDeployLockFixture(t, env, localHostname, 99999999, time.Now(), "precondition-sabotage-uid", "53")
				env.lockDeleteRaceMode = "both"
			},
		)
	})

	t.Run("UID precondition", func(t *testing.T) {
		runSabotage(
			t,
			`          uid: $uid,
`,
			"",
			func(env *deployScriptEnvironment) {
				writeDeployLockFixture(t, env, localHostname, 99999999, time.Now(), "uid-sabotage", "54")
				env.lockDeleteRaceMode = "uid"
			},
		)
	})

	t.Run("resourceVersion precondition", func(t *testing.T) {
		runSabotage(
			t,
			`          uid: $uid,
          resourceVersion: $resource_version`,
			"          uid: $uid",
			func(env *deployScriptEnvironment) {
				writeDeployLockFixture(t, env, localHostname, 99999999, time.Now(), "rv-sabotage", "55")
				env.lockDeleteRaceMode = "resourceVersion"
			},
		)
	})
}

func TestDeployScriptInterruptedLockCreationIsReconciled(t *testing.T) {
	requirePOSIXShell(t)
	manifest := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, manifest, manifest)
	env.signalDuringLockCreate = "HUP"
	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 129 {
		t.Fatalf("interrupted lock creation exit = %v, want 129\n%s", err, output)
	}
	if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted lock creation left an orphaned owned lock: %v\n%s", statErr, output)
	}
}

func TestDeployScriptAmbiguousLockCreationIsReconciled(t *testing.T) {
	requirePOSIXShell(t)
	manifest := baselineManifest(true, "*", "false")
	env := newDeployScriptEnvironment(t, manifest, manifest)
	env.failLockCreateAfterMutation = true
	output, err := env.run(
		"--prepare-claims-baseline",
		"--baseline-chart-dir",
		env.chartDir,
	)
	if err != nil {
		t.Fatalf("server-created lock with lost response was not adopted: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "acquired Helm release lock") {
		t.Fatalf("ambiguous creation was not reconciled as owned:\n%s", output)
	}
	if _, statErr := os.Stat(env.lockFile); !os.IsNotExist(statErr) {
		t.Fatalf("reconciled lock was not released after success: %v\n%s", statErr, output)
	}
}

type deployScriptEnvironment struct {
	t                         *testing.T
	binDir                    string
	chartDir                  string
	liveManifest              string
	liveResource              string
	candidateManifest         string
	upgradeHookManifest       string
	baselineManifest          string
	appliedManifest           string
	upgradedMarker            string
	upgradeLog                string
	templateLog               string
	gitLog                    string
	lockFile                  string
	lockBackupDir             string
	provisioningQueryCount    string
	lockDeleteRaceMark        string
	serverDryRunLog           string
	serverDryRunMark          string
	janitorTTLSSALog          string
	finalRollbackMark         string
	liveVerificationMark      string
	extraImageCount           string
	atomicFailedMark          string
	candidateAppliedMark      string
	syntheticContainedMark    string
	atomicRollbackManifest    string
	containedRollbackManifest string
	immutableRollbackManifest string
	immutableRollbackHooks    string
	currentRollbackHooks      string
	immutableRollbackValues   string
	currentRollbackValues     string
	cronjobVerifyMark         string
	cronjobVerifyManifest     string
	cronjobCreateLog          string
	claimsPausedMark          string
	claimsResumedMark         string
	warmerRolloutMark         string
	warmerImageProbeLog       string
	helmStatus                string
	helmRevision              int
	immutableRevision         int
	immutableBaselineRevision int
	postLiveHelmRevision      int
	postLiveHelmStatus        string
	postLiveHelmHistoryExit   int
	helmDescription           string
	immutableRevisionMissing  bool
	immutableHelmStatus       string
	immutableChartVersion     string
	currentChartVersion       string
	mismatchContainer         string
	packageTag                string
	packageAdditionalTag      string
	packageDigest             string
	runArtifactDigest         string
	imageRevision             string
	imageRevisionProbeCount   string
	imageRevisionDriftAfter   int
	uiBuildRecord             string
	uiSourceSHA               string
	uiArtifactSHA             string
	uiArtifactDigest          string
	uiImageRevision           string
	uiCommitVerified          bool
	uiBuildRecordMissing      bool
	uiBuildRecordRaw          bool
	extraRepository           string
	gitBranch                 string
	gitRemoteSHA              string
	gitRemoteURL              string
	commitVerified            bool
	missingBuild              string
	gitDirty                  bool
	// Model Helm ownership metadata present live but absent from server-side dry-run objects.
	liveHelmRelease   string
	liveHelmNamespace string

	failFinalServerDryRun             bool
	externalDriftAfterServerDryRun    bool
	mutateFinalServerObject           bool
	warmerRolloutFailure              bool
	warmerImageStaysEmpty             bool
	finalWorkloadHealthRegression     bool
	activeJobs                        int
	activeKubernetesJobs              int
	postUpgradeActiveKubernetesJob    string
	activeMutatingSyntheticJobs       int
	completedMutatingSyntheticJob     bool
	noRetainedJanitorJob              bool
	noRetainedRunnerJob               bool
	pendingSyntheticJobs              int
	migrationState                    string
	provisioningJobs                  string
	provisioningJobsAfterInitial      string
	unknownProvisioningJobStatus      bool
	syntheticQuotaState               string
	syntheticUserID                   string
	syntheticOIDCSub                  string
	syntheticSecretValue              string
	syntheticSecretExists             bool
	syntheticSecretKeyPresent         bool
	failSyntheticSecretRead           bool
	syntheticUserKnown                bool
	malformedJobsJSON                 bool
	preflightQueryFailure             string
	failJobsList                      bool
	failAtomicUpgrade                 bool
	atomicRollbackMismatch            string
	postUpgradeMismatch               string
	postUpgradeObjectMutation         bool
	retainLiveJanitorTTLBeforeUpgrade bool
	postApplyAnnotationsMode          string
	failClaimsResume                  bool
	lockDeleteRaceMode                string
	lockTTLSeconds                    string
	signalAfterClaimsPause            string
	signalDuringClaimsPause           string
	failClaimsPauseBeforeMutation     bool
	signalDuringHelm                  string
	signalDuringLockCreate            string
	failLockCreateAfterMutation       bool
	cronjobVerificationShape          string

	// simulateOwnershipConflict makes the fake kubectl reject any
	// `--server-side --dry-run=server` apply that lacks `--force-conflicts`
	// with a realistic SSA ownership-conflict error, mimicking existing
	// objects already owned by Helm/kubectl-set in production.
	simulateOwnershipConflict bool
	// failServerValidateDocument, when set to a workload's metadata name
	// (e.g. "selfservice-worker"), makes the fake kubectl fail exactly the
	// "-o yaml" server-side dry-run apply for that one candidate document,
	// simulating a real per-document admission/defaulting failure.
	failServerValidateDocument string
	// scriptPath overrides the deploy.sh path invoked by run/runWithUI.
	// Empty means the real, unmodified repo script.
	scriptPath string
}

func newDeployScriptEnvironment(t *testing.T, live, candidate string) *deployScriptEnvironment {
	t.Helper()
	root := t.TempDir()
	baselineRevision := testHelmRevision
	env := &deployScriptEnvironment{
		t:                         t,
		binDir:                    filepath.Join(root, "bin"),
		chartDir:                  filepath.Join(root, "safe-chart"),
		liveManifest:              filepath.Join(root, "live.yaml"),
		liveResource:              filepath.Join(root, "live-resource.yaml"),
		candidateManifest:         filepath.Join(root, "candidate.yaml"),
		upgradeHookManifest:       filepath.Join(root, "upgrade-hook.yaml"),
		baselineManifest:          filepath.Join(root, "baseline.yaml"),
		appliedManifest:           filepath.Join(root, "applied.yaml"),
		upgradedMarker:            filepath.Join(root, "upgraded"),
		upgradeLog:                filepath.Join(root, "upgrade.log"),
		templateLog:               filepath.Join(root, "template.log"),
		gitLog:                    filepath.Join(root, "git.log"),
		lockFile:                  filepath.Join(root, "helm.lock"),
		lockBackupDir:             filepath.Join(root, "lock-backups"),
		provisioningQueryCount:    filepath.Join(root, "provisioning-query-count"),
		lockDeleteRaceMark:        filepath.Join(root, "lock-delete-race.marker"),
		serverDryRunLog:           filepath.Join(root, "server-dry-run.log"),
		serverDryRunMark:          filepath.Join(root, "server-dry-run.marker"),
		janitorTTLSSALog:          filepath.Join(root, "janitor-ttl-ssa.log"),
		finalRollbackMark:         filepath.Join(root, "final-rollback.marker"),
		liveVerificationMark:      filepath.Join(root, "live-verification.marker"),
		extraImageCount:           filepath.Join(root, "extra-image-count"),
		atomicFailedMark:          filepath.Join(root, "atomic-failed"),
		candidateAppliedMark:      filepath.Join(root, "candidate-applied"),
		syntheticContainedMark:    filepath.Join(root, "synthetic-contained"),
		atomicRollbackManifest:    filepath.Join(root, "atomic-rollback.yaml"),
		containedRollbackManifest: filepath.Join(root, "atomic-rollback-contained.yaml"),
		immutableRollbackManifest: filepath.Join(root, "immutable-rollback.yaml"),
		immutableRollbackHooks:    filepath.Join(root, "immutable-rollback-hooks.yaml"),
		currentRollbackHooks:      filepath.Join(root, "current-rollback-hooks.yaml"),
		immutableRollbackValues:   filepath.Join(root, "immutable-rollback-values.json"),
		currentRollbackValues:     filepath.Join(root, "current-rollback-values.json"),
		cronjobVerifyMark:         filepath.Join(root, "cronjob-verified"),
		cronjobVerifyManifest:     filepath.Join(root, "cronjob-verification-job.json"),
		cronjobCreateLog:          filepath.Join(root, "cronjob-create.log"),
		claimsPausedMark:          filepath.Join(root, "claims-paused.marker"),
		claimsResumedMark:         filepath.Join(root, "claims-resumed.marker"),
		warmerRolloutMark:         filepath.Join(root, "warmer-rollout.marker"),
		warmerImageProbeLog:       filepath.Join(root, "warmer-image-probe.log"),
		helmStatus:                "deployed",
		helmRevision:              baselineRevision,
		immutableRevision:         baselineRevision,
		immutableBaselineRevision: baselineRevision,
		helmDescription:           "Upgrade complete",
		immutableHelmStatus:       "superseded",
		immutableChartVersion:     "0.1.0",
		currentChartVersion:       "0.1.0",
		packageTag:                testSourceSHA,
		packageDigest:             testDigestB,
		runArtifactDigest:         testDigestB,
		imageRevision:             testSourceSHA,
		imageRevisionProbeCount:   filepath.Join(root, "image-revision-probe-count"),
		uiBuildRecord:             filepath.Join(root, "ui-build-record.dockerbuild"),
		uiSourceSHA:               testUISourceSHA,
		uiArtifactSHA:             testUISourceSHA,
		uiArtifactDigest:          testDigestB,
		uiImageRevision:           testUISourceSHA,
		uiCommitVerified:          true,
		extraRepository:           "registry.example/extra",
		gitBranch:                 "main",
		gitRemoteSHA:              testSourceSHA,
		gitRemoteURL:              "git@github.com:jmal1/selfservice-api.git",
		commitVerified:            true,
		migrationState:            "1:38:false",
		provisioningJobs:          "0",
		syntheticQuotaState:       "1:0",
		syntheticUserID:           testSyntheticUserID,
		syntheticOIDCSub:          testSyntheticOIDCSub,
		syntheticSecretValue:      base64.StdEncoding.EncodeToString([]byte(testSyntheticUserID)),
		syntheticSecretExists:     true,
		syntheticSecretKeyPresent: true,
		syntheticUserKnown:        true,
		liveHelmRelease:           "selfservice",
		liveHelmNamespace:         "selfservice",
	}
	if err := os.MkdirAll(env.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(env.chartDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.chartDir, "Chart.yaml"), "apiVersion: v2\nname: selfservice\nversion: 0.1.0\n")
	writeFile(t, env.liveManifest, live)
	writeFile(t, env.liveResource, live)
	writeFile(t, env.candidateManifest, candidate)
	writeFile(t, env.upgradeHookManifest, "")
	writeFile(t, env.atomicRollbackManifest, live)
	writeFile(t, env.containedRollbackManifest, live)
	writeFile(t, env.immutableRollbackManifest, live)
	writeFile(t, env.immutableRollbackHooks, "")
	writeFile(t, env.currentRollbackHooks, "")
	writeFile(t, env.immutableRollbackValues, `{"provisioning":{"workerClaimsEnabled":false}}`+"\n")
	writeFile(t, env.currentRollbackValues, `{"provisioning":{"workerClaimsEnabled":false}}`+"\n")
	env.writeCommands()
	return env
}

func (e *deployScriptEnvironment) writeCommands() {
	e.t.Helper()
	writeExecutable(e.t, filepath.Join(e.binDir, "helm"), `#!/bin/bash
set -euo pipefail

latest_revision() {
  local revision=$FAKE_HELM_REVISION
  if [ -f "$FAKE_LIVE_VERIFICATION_MARKER" ] &&
     [ "$FAKE_POST_LIVE_HELM_REVISION" != 0 ]; then
    printf '%s' "$FAKE_POST_LIVE_HELM_REVISION"
    return
  fi
  if [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
    revision=$((FAKE_HELM_REVISION + 2))
  elif [ -f "$FAKE_UPGRADED_MARKER" ]; then
    revision=$((FAKE_HELM_REVISION + 1))
  fi
  printf '%s' "$revision"
}

requested_revision() {
  local previous=
  for argument in "$@"; do
    if [ "$previous" = --revision ]; then
      printf '%s' "$argument"
      return
    fi
    previous=$argument
  done
  latest_revision
}

case "$1 $2" in
  "history selfservice")
    revision=$(latest_revision)
    status=$FAKE_HELM_STATUS
    if [ -f "$FAKE_LIVE_VERIFICATION_MARKER" ] &&
       [ -n "$FAKE_POST_LIVE_HELM_STATUS" ]; then
      status=$FAKE_POST_LIVE_HELM_STATUS
    fi
    printf '%s\n' '- app_version: test' "  description: $FAKE_HELM_DESCRIPTION" "  revision: $revision" "  status: $status"
    if [ -f "$FAKE_LIVE_VERIFICATION_MARKER" ] &&
       [ "$FAKE_POST_LIVE_HELM_HISTORY_EXIT" != 0 ]; then
      exit "$FAKE_POST_LIVE_HELM_HISTORY_EXIT"
    fi
    ;;
  "get manifest")
    revision=$(requested_revision "$@")
    latest=$(latest_revision)
    if [ "$revision" = "$FAKE_IMMUTABLE_BASELINE_REVISION" ] && [ "$latest" != "$FAKE_IMMUTABLE_BASELINE_REVISION" ]; then
      [ "$FAKE_IMMUTABLE_REVISION_MISSING" != true ] || exit 1
      cat "$FAKE_IMMUTABLE_ROLLBACK_MANIFEST"
    elif [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
      cat "$FAKE_ATOMIC_ROLLBACK_MANIFEST"
    elif [ -f "$FAKE_UPGRADED_MARKER" ]; then
      cat "$FAKE_BASELINE_MANIFEST"
    else
      cat "$FAKE_LIVE_MANIFEST"
    fi
    ;;
  "get hooks")
    revision=$(requested_revision "$@")
    latest=$(latest_revision)
    if [ "$revision" = "$FAKE_IMMUTABLE_BASELINE_REVISION" ] && [ "$latest" != "$FAKE_IMMUTABLE_BASELINE_REVISION" ]; then
      [ "$FAKE_IMMUTABLE_REVISION_MISSING" != true ] || exit 1
      cat "$FAKE_IMMUTABLE_ROLLBACK_HOOKS"
    else
      cat "$FAKE_CURRENT_ROLLBACK_HOOKS"
    fi
    ;;
  "get values")
    if [[ "$*" == *"-o json"* ]] && [[ "$*" != *"--all"* ]]; then
      echo "complete rollback equivalence values require --all" >&2
      exit 93
    fi
    revision=$(requested_revision "$@")
    latest=$(latest_revision)
    if [ "$revision" = "$FAKE_IMMUTABLE_BASELINE_REVISION" ] && [ "$latest" != "$FAKE_IMMUTABLE_BASELINE_REVISION" ]; then
      [ "$FAKE_IMMUTABLE_REVISION_MISSING" != true ] || exit 1
      cat "$FAKE_IMMUTABLE_ROLLBACK_VALUES"
    else
      cat "$FAKE_CURRENT_ROLLBACK_VALUES"
    fi
    ;;
  "get metadata")
    revision=$(requested_revision "$@")
    latest=$(latest_revision)
    if [ "$revision" = "$FAKE_IMMUTABLE_BASELINE_REVISION" ] && [ "$latest" != "$FAKE_IMMUTABLE_BASELINE_REVISION" ]; then
      [ "$FAKE_IMMUTABLE_REVISION_MISSING" != true ] || exit 1
      chart_version=$FAKE_IMMUTABLE_CHART_VERSION
      status=$FAKE_IMMUTABLE_HELM_STATUS
    else
      chart_version=$FAKE_CURRENT_CHART_VERSION
      status=$FAKE_HELM_STATUS
    fi
    jq -cn \
      --arg name selfservice \
      --arg chart selfservice \
      --arg version "$chart_version" \
      --arg appVersion test \
      --arg namespace selfservice \
      --argjson revision "$revision" \
      --arg status "$status" \
      '{name:$name,chart:$chart,version:$version,appVersion:$appVersion,namespace:$namespace,revision:$revision,status:$status,deployedAt:"2026-08-27T00:00:00Z"}'
    ;;
  "dependency build"|"dep build")
    ;;
  "template selfservice")
    original_args="$*"
    printf '%s\n' "$original_args" >> "$FAKE_TEMPLATE_LOG"
    cat "$FAKE_CANDIDATE_MANIFEST"
    if [[ "$*" == *"--is-upgrade"* ]] && [ -s "$FAKE_UPGRADE_HOOK_MANIFEST" ]; then
      printf '%s\n' '---'
      cat "$FAKE_UPGRADE_HOOK_MANIFEST"
    fi
    ;;
  "upgrade selfservice")
    post_renderer=
    original_args="$*"
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--post-renderer" ]; then
        post_renderer=$2
        break
      fi
      shift
    done
    [ -n "$post_renderer" ]
    if [ -n "${EXPECTED_CANDIDATE_MANIFEST:-}" ]; then
      "$post_renderer" < "$FAKE_CANDIDATE_MANIFEST" > "$FAKE_APPLIED_MANIFEST"
    else
      "$post_renderer" < "$FAKE_CANDIDATE_MANIFEST" > "$FAKE_BASELINE_MANIFEST"
    fi
    printf '%s\n' "$original_args" >> "$FAKE_UPGRADE_LOG"
    if [ -n "${EXPECTED_CANDIDATE_MANIFEST:-}" ] &&
       [ -n "$FAKE_SIGNAL_DURING_HELM" ]; then
      kill -s "$FAKE_SIGNAL_DURING_HELM" "$PPID"
      sleep 1
    fi
    if [ -n "${EXPECTED_CANDIDATE_MANIFEST:-}" ] && [ "$FAKE_FAIL_ATOMIC_UPGRADE" = true ]; then
      rm -f "$FAKE_SYNTHETIC_CONTAINED_MARKER"
      : > "$FAKE_ATOMIC_FAILED_MARKER"
      exit 99
    fi
    if [ -n "${EXPECTED_CANDIDATE_MANIFEST:-}" ]; then
      cp "$FAKE_APPLIED_MANIFEST" "$FAKE_BASELINE_MANIFEST"
      if [ "$FAKE_POST_UPGRADE_OBJECT_MUTATION" = true ]; then
        sed '/- name: api-gateway/a\        command: ["/post-upgrade-drift"]' \
          "$FAKE_BASELINE_MANIFEST" > "$FAKE_BASELINE_MANIFEST.mutated"
        mv "$FAKE_BASELINE_MANIFEST.mutated" "$FAKE_BASELINE_MANIFEST"
      fi
      : > "$FAKE_CANDIDATE_APPLIED_MARKER"
    fi
    : > "$FAKE_UPGRADED_MARKER"
    ;;
  "list -n")
    printf 'selfservice 131 deployed\n'
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
  if [ -f "$FAKE_CLAIMS_RESUMED_MARKER" ]; then
    printf '%s' "$FAKE_LIVE_RESOURCE_MANIFEST"
  elif [ -f "$FAKE_UPGRADED_MARKER" ]; then
    printf '%s' "$FAKE_BASELINE_MANIFEST"
  elif [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
    if [ -f "$FAKE_SYNTHETIC_CONTAINED_MARKER" ]; then
      printf '%s' "$FAKE_CONTAINED_ROLLBACK_MANIFEST"
    else
      printf '%s' "$FAKE_ATOMIC_ROLLBACK_MANIFEST"
    fi
  elif [ -f "$FAKE_CLAIMS_PAUSED_MARKER" ]; then
    printf '%s.paused' "$FAKE_LIVE_RESOURCE_MANIFEST"
  else
    printf '%s' "$FAKE_LIVE_RESOURCE_MANIFEST"
  fi
}

# count_logical_yaml_calls reports how many logical server-side dry-run
# validation passes have appeared in $FAKE_SERVER_DRY_RUN_LOG so far,
# counting "-o yaml" invocations. server_validate_candidate now issues one
# "-o yaml" kubectl call per document in the candidate manifest (splitting
# multi-document input avoids kubectl's real List-wrapping of multi-doc
# "-o yaml" apply output - see the comment on server_validate_candidate in
# deploy.sh), so one logical validation pass appears in the log as one call
# per split document, numbered 000001.yaml, 000002.yaml, .... This counts
# only the first document of each pass (a manifest path whose basename is
# exactly "000001.yaml") or an unsplit direct file path (used by
# validate_upgrade_hooks, which is never split), so the result reports the
# same logical-pass count regardless of how many documents the manifest
# being validated happens to contain.
count_logical_yaml_calls() {
  [ -f "$FAKE_SERVER_DRY_RUN_LOG" ] || { echo 0; return; }
  awk '
    {
      is_yaml = 0
      manifest = ""
      for (i = 1; i <= NF; i++) {
        if ($i == "-o" && $(i + 1) == "yaml") { is_yaml = 1 }
        if ($i == "-f") { manifest = $(i + 1) }
      }
      if (!is_yaml) { next }
      n = split(manifest, parts, "/")
      base = parts[n]
      if (base ~ /^[0-9]{6}\.yaml$/ && base != "000001.yaml") { next }
      count++
    }
    END { print count + 0 }
  ' "$FAKE_SERVER_DRY_RUN_LOG"
}

extract_resource() {
  local manifest=$1
  local wanted=${2#*/}
  local wanted_kind=${2%%/*}
  case "$wanted_kind" in
    cronjob) wanted_kind=CronJob ;;
    daemonset) wanted_kind=DaemonSet ;;
    deployment) wanted_kind=Deployment ;;
    statefulset) wanted_kind=StatefulSet ;;
  esac
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

json_resource() {
  local manifest=$1
  local resource=$2
  local source=${3:-desired}
  local resource_file canonical kind replicas revision daemonset_generation
  resource_file=$(mktemp)
  extract_resource "$manifest" "$resource" > "$resource_file"
  canonical=$(canonical_resource "$resource_file")
  if [ "$FAKE_RETAIN_LIVE_JANITOR_TTL_BEFORE_UPGRADE" = true ] &&
     [ "$resource" = "CronJob/selfservice-synthetic-janitor" ]; then
    if [ "$source" = desired ]; then
      canonical="$canonical
      ttlSecondsAfterFinished: 1800"
      echo pre-upgrade-retained >> "$FAKE_JANITOR_TTL_SSA_LOG"
    elif [ "$source" = post-upgrade-expected ] || [ "$source" = post-apply ]; then
      echo post-upgrade-omitted >> "$FAKE_JANITOR_TTL_SSA_LOG"
    fi
  fi
  kind=${resource%%/*}
  revision=$(awk '
    /^metadata:[[:space:]]*$/ { metadata = 1; next }
    metadata && /^  annotations:[[:space:]]*$/ { annotations = 1; next }
    metadata && annotations && /^    deployment\.kubernetes\.io\/revision:[[:space:]]*/ {
      value = $0
      sub(/^    deployment\.kubernetes\.io\/revision:[[:space:]]*/, "", value)
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      print value
      exit
    }
    metadata && /^[^[:space:]]/ { exit }
  ' "$resource_file")
  if [ "$source" = live ] && [ "$kind" = Deployment ]; then
    revision=$FAKE_LIVE_DEPLOYMENT_REVISION
  fi
  daemonset_generation=
  if [ "$source" = live ] && [ "$kind" = DaemonSet ]; then
    daemonset_generation=$FAKE_LIVE_DAEMONSET_GENERATION
  fi
  server_yaml_count=$(count_logical_yaml_calls)
  if [ "$FAKE_MUTATE_FINAL_SERVER_OBJECT" = true ] &&
     [ "$server_yaml_count" -ge 2 ] &&
     [ "$resource" = "Deployment/selfservice-api" ]; then
    canonical="$canonical
serverInjectedMutation: true"
  fi
  replicas=$(awk '
    /^spec:[[:space:]]*$/ { spec = 1; next }
    spec && /^  replicas:[[:space:]]*/ {
      value = $0
      sub(/^  replicas:[[:space:]]*/, "", value)
      print value
      exit
    }
  ' "$resource_file")
  if [ -n "$replicas" ]; then
    jq -cn \
      --arg kind "$kind" \
      --arg canonical "$canonical" \
      --arg revision "$revision" \
      --arg daemonset_generation "$daemonset_generation" \
      --arg source "$source" \
      --arg post_apply_mode "$FAKE_POST_APPLY_ANNOTATIONS_MODE" \
      --arg resource "$resource" \
      --arg release "$FAKE_LIVE_HELM_RELEASE" \
      --arg namespace "$FAKE_LIVE_HELM_NAMESPACE" \
      --argjson replicas "$replicas" \
      '{
        apiVersion:"fixture/v1",
        kind:$kind,
        metadata:{
          annotations:(
            if $source == "live" and $kind != "Job" then
              {
                "meta.helm.sh/release-name":$release,
                "meta.helm.sh/release-namespace":$namespace
              } + (
                if $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision":$revision}
                elif $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation":$daemonset_generation}
                else
                  {}
                end
              )
            elif $source == "post-apply" or
                 ($source == "post-upgrade-expected" and
                  (($post_apply_mode | startswith("helm-wrong-")) or
                   ($post_apply_mode | startswith("helm-nonstring-")))) then
              (
                if ($post_apply_mode == "generated" or $post_apply_mode == "deployment-revision") and
                   $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision":"164"}
                elif ($post_apply_mode == "generated" or $post_apply_mode == "daemonset-generation") and
                     $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation":"8"}
                elif $post_apply_mode == "unrelated" and $kind == "Deployment" then
                  {"operations.example/owner":"live"}
                elif $post_apply_mode == "similarly-named" and $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision-note":"live"}
                elif $post_apply_mode == "wrong-kind" and $kind == "Deployment" then
                  {"deprecated.daemonset.template.generation":"8"}
                elif $post_apply_mode == "daemonset-unrelated" and $kind == "DaemonSet" then
                  {"operations.example/owner":"live"}
                elif $post_apply_mode == "daemonset-similarly-named" and $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation-note":"live"}
                elif $post_apply_mode == "daemonset-wrong-kind" and $kind == "DaemonSet" then
                  {"deployment.kubernetes.io/revision":"164"}
                else
                  {}
                end
              ) + (
                if ($post_apply_mode | startswith("helm-")) and
                   ($resource == "CronJob/selfservice-synthetic-janitor" or
                    $resource == "CronJob/selfservice-synthetic-runner") then
                  {
                    "meta.helm.sh/release-name":(
                      if $post_apply_mode == "helm-nonstring-release-name" then
                        170
                      elif $post_apply_mode == "helm-wrong-release-name" then
                        "other-release"
                      else
                        "selfservice"
                      end
                    ),
                    "meta.helm.sh/release-namespace":(
                      if $post_apply_mode == "helm-nonstring-release-namespace" then
                        []
                      elif $post_apply_mode == "helm-wrong-release-namespace" then
                        "other-namespace"
                      else
                        "selfservice"
                      end
                    )
                  } + (
                    if $post_apply_mode == "helm-top-annotation-drift" then
                      {"operations.example/owner":"live"}
                    else
                      {}
                    end
                  )
                else
                  {}
                end
              )
              | if length == 0 then null else . end
            else
              null
            end
          ),
          labels:(
            if ($source == "post-apply" or
                ($source == "post-upgrade-expected" and
                 (($post_apply_mode | startswith("helm-wrong-")) or
                  ($post_apply_mode | startswith("helm-nonstring-"))))) and
               ($post_apply_mode | startswith("helm-")) and
               ($resource == "CronJob/selfservice-synthetic-janitor" or
                $resource == "CronJob/selfservice-synthetic-runner") then
              {
                "app.kubernetes.io/managed-by":(
                  if $post_apply_mode == "helm-nonstring-managed-by" then
                    170
                  elif $post_apply_mode == "helm-wrong-managed-by" then
                    "Other"
                  else
                    "Helm"
                  end
                )
              } + (
                if $post_apply_mode == "helm-top-label-drift" then
                  {"operations.example/owner":"live"}
                else
                  {}
                end
              )
            else
              null
            end
          )
        },
        spec:(
          {fixtureCanonical:$canonical,replicas:$replicas} +
          if $source == "post-apply" and $post_apply_mode == "template-level" and
             $kind == "Deployment" then
            {template:{metadata:{annotations:{"deployment.kubernetes.io/revision":"164"}}}}
          elif $source == "post-apply" and $post_apply_mode == "daemonset-template-level" and
               $kind == "DaemonSet" then
            {template:{metadata:{annotations:{"deprecated.daemonset.template.generation":"8"}}}}
          elif $source == "post-apply" and
               $post_apply_mode == "helm-template-managed-by-drift" and
               ($resource == "CronJob/selfservice-synthetic-janitor" or
                $resource == "CronJob/selfservice-synthetic-runner") then
            {jobTemplate:{spec:{template:{metadata:{labels:{"app.kubernetes.io/managed-by":"Other"}}}}}}
          else
            {}
          end
        )
      }'
  else
    jq -cn \
      --arg kind "$kind" \
      --arg canonical "$canonical" \
      --arg revision "$revision" \
      --arg daemonset_generation "$daemonset_generation" \
      --arg source "$source" \
      --arg post_apply_mode "$FAKE_POST_APPLY_ANNOTATIONS_MODE" \
      --arg resource "$resource" \
      --arg release "$FAKE_LIVE_HELM_RELEASE" \
      --arg namespace "$FAKE_LIVE_HELM_NAMESPACE" \
      '{
        apiVersion:"fixture/v1",
        kind:$kind,
        metadata:{
          annotations:(
            if $source == "live" and $kind != "Job" then
              {
                "meta.helm.sh/release-name":$release,
                "meta.helm.sh/release-namespace":$namespace
              } + (
                if $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision":$revision}
                elif $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation":$daemonset_generation}
                else
                  {}
                end
              )
            elif $source == "post-apply" or
                 ($source == "post-upgrade-expected" and
                  (($post_apply_mode | startswith("helm-wrong-")) or
                   ($post_apply_mode | startswith("helm-nonstring-")))) then
              (
                if ($post_apply_mode == "generated" or $post_apply_mode == "deployment-revision") and
                   $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision":"164"}
                elif ($post_apply_mode == "generated" or $post_apply_mode == "daemonset-generation") and
                     $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation":"8"}
                elif $post_apply_mode == "unrelated" and $kind == "Deployment" then
                  {"operations.example/owner":"live"}
                elif $post_apply_mode == "similarly-named" and $kind == "Deployment" then
                  {"deployment.kubernetes.io/revision-note":"live"}
                elif $post_apply_mode == "wrong-kind" and $kind == "Deployment" then
                  {"deprecated.daemonset.template.generation":"8"}
                elif $post_apply_mode == "daemonset-unrelated" and $kind == "DaemonSet" then
                  {"operations.example/owner":"live"}
                elif $post_apply_mode == "daemonset-similarly-named" and $kind == "DaemonSet" then
                  {"deprecated.daemonset.template.generation-note":"live"}
                elif $post_apply_mode == "daemonset-wrong-kind" and $kind == "DaemonSet" then
                  {"deployment.kubernetes.io/revision":"164"}
                else
                  {}
                end
              ) + (
                if ($post_apply_mode | startswith("helm-")) and
                   ($resource == "CronJob/selfservice-synthetic-janitor" or
                    $resource == "CronJob/selfservice-synthetic-runner") then
                  {
                    "meta.helm.sh/release-name":(
                      if $post_apply_mode == "helm-nonstring-release-name" then
                        170
                      elif $post_apply_mode == "helm-wrong-release-name" then
                        "other-release"
                      else
                        "selfservice"
                      end
                    ),
                    "meta.helm.sh/release-namespace":(
                      if $post_apply_mode == "helm-nonstring-release-namespace" then
                        []
                      elif $post_apply_mode == "helm-wrong-release-namespace" then
                        "other-namespace"
                      else
                        "selfservice"
                      end
                    )
                  } + (
                    if $post_apply_mode == "helm-top-annotation-drift" then
                      {"operations.example/owner":"live"}
                    else
                      {}
                    end
                  )
                else
                  {}
                end
              )
              | if length == 0 then null else . end
            else
              null
            end
          ),
          labels:(
            if ($source == "post-apply" or
                ($source == "post-upgrade-expected" and
                 (($post_apply_mode | startswith("helm-wrong-")) or
                  ($post_apply_mode | startswith("helm-nonstring-"))))) and
               ($post_apply_mode | startswith("helm-")) and
               ($resource == "CronJob/selfservice-synthetic-janitor" or
                $resource == "CronJob/selfservice-synthetic-runner") then
              {
                "app.kubernetes.io/managed-by":(
                  if $post_apply_mode == "helm-nonstring-managed-by" then
                    170
                  elif $post_apply_mode == "helm-wrong-managed-by" then
                    "Other"
                  else
                    "Helm"
                  end
                )
              } + (
                if $post_apply_mode == "helm-top-label-drift" then
                  {"operations.example/owner":"live"}
                else
                  {}
                end
              )
            else
              null
            end
          )
        },
        spec:(
          {fixtureCanonical:$canonical} +
          if $source == "post-apply" and $post_apply_mode == "template-level" and
             $kind == "Deployment" then
            {template:{metadata:{annotations:{"deployment.kubernetes.io/revision":"164"}}}}
          elif $source == "post-apply" and $post_apply_mode == "daemonset-template-level" and
               $kind == "DaemonSet" then
            {template:{metadata:{annotations:{"deprecated.daemonset.template.generation":"8"}}}}
          elif $source == "post-apply" and
               $post_apply_mode == "helm-template-managed-by-drift" and
               ($resource == "CronJob/selfservice-synthetic-janitor" or
                $resource == "CronJob/selfservice-synthetic-runner") then
            {jobTemplate:{spec:{template:{metadata:{labels:{"app.kubernetes.io/managed-by":"Other"}}}}}}
          else
            {}
          end
        )
      }'
  fi
  rm -f "$resource_file"
}

image_for_container() {
  local container=$1
  local repository
  case "$container" in
    warmer) repository=ghcr.io/jmal1/selfservice-crucible-runner ;;
    api-gateway) repository=ghcr.io/jmal1/selfservice-api-gateway ;;
    provision-worker) repository=ghcr.io/jmal1/selfservice-provision-worker ;;
    engine) repository=ghcr.io/jmal1/selfservice-crucible-engine ;;
    ui) repository=ghcr.io/jmal1/selfservice-ui ;;
    synthetic-api-monitor) repository=ghcr.io/jmal1/selfservice-synthetic-api-monitor ;;
    synthetic-janitor) repository=ghcr.io/jmal1/selfservice-provision-worker ;;
    synthetic-runner) repository=ghcr.io/jmal1/selfservice-crucible-runner ;;
    extra) repository=$FAKE_EXTRA_REPOSITORY ;;
    *) echo "unknown fake container $container" >&2; exit 91 ;;
  esac
  local digest=$FAKE_DIGEST_A
  if [ "$container" = warmer ] &&
     [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
    if [ -f "$FAKE_WARMER_ROLLOUT_MARKER" ]; then
      if [ "$FAKE_WARMER_IMAGE_STAYS_EMPTY" = true ]; then
        if [ -n "$FAKE_WARMER_IMAGE_PROBE_LOG" ]; then
          printf 'after-empty\n' >> "$FAKE_WARMER_IMAGE_PROBE_LOG"
        fi
        printf ''
        return 0
      fi
      if [ -n "$FAKE_WARMER_IMAGE_PROBE_LOG" ]; then
        printf 'after\n' >> "$FAKE_WARMER_IMAGE_PROBE_LOG"
      fi
      digest=$FAKE_DIGEST_B
    else
      if [ -n "$FAKE_WARMER_IMAGE_PROBE_LOG" ]; then
        printf 'before\n' >> "$FAKE_WARMER_IMAGE_PROBE_LOG"
      fi
      printf ''
      return 0
    fi
  fi
  if [ "$container" = synthetic-api-monitor ] &&
     [ -f "$FAKE_CRONJOB_VERIFY_MARKER" ]; then
    digest=$FAKE_DIGEST_B
  fi
  [ "$container" = "$FAKE_MISMATCH_CONTAINER" ] && digest=$FAKE_DIGEST_B
  if [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
    case "$container" in
      warmer|api-gateway|provision-worker|engine|ui|synthetic-janitor|synthetic-runner)
        digest=$FAKE_DIGEST_B
        ;;
    esac
    [ "$container" != "$FAKE_POST_UPGRADE_MISMATCH" ] || digest=$FAKE_DIGEST_A
  elif [ -f "$FAKE_ATOMIC_FAILED_MARKER" ] &&
       [ "$container" = "$FAKE_ATOMIC_ROLLBACK_MISMATCH" ]; then
    digest=$FAKE_DIGEST_B
  fi
  if [ "$container" = extra ]; then
    count=0
    [ ! -f "$FAKE_EXTRA_IMAGE_COUNT" ] || count=$(cat "$FAKE_EXTRA_IMAGE_COUNT")
    count=$((count + 1))
    printf '%s' "$count" > "$FAKE_EXTRA_IMAGE_COUNT"
    if [ "$FAKE_EXTERNAL_DRIFT_AFTER_SERVER_DRY_RUN" = true ] &&
       [ "$count" -ge 4 ]; then
      digest=$FAKE_DIGEST_B
      : > "$FAKE_FINAL_ROLLBACK_MARKER"
    fi
  fi
  printf 'docker-pullable://%s@sha256:%s\n' "$repository" "$digest"
}

case "$1" in
  get)
    if [[ "$2" == configmap/* ]]; then
      if [ ! -f "$FAKE_LOCK_FILE" ]; then
        exit 1
      fi
      cat "$FAKE_LOCK_FILE"
    elif [ "$2" = secret ]; then
      if [ "${3:-}" != selfservice-synthetic-user ] ||
         [[ "$*" != *"jsonpath={.data.user-id}"* ]]; then
        echo "unexpected synthetic user Secret lookup" >&2
        exit 96
      fi
      if [ "$FAKE_FAIL_SYNTHETIC_SECRET_READ" = true ]; then
        echo "sabotaged synthetic user Secret read failure" >&2
        exit 97
      fi
      if [ "$FAKE_SYNTHETIC_SECRET_EXISTS" != true ]; then
        echo "Error from server (NotFound): secrets \"selfservice-synthetic-user\" not found" >&2
        exit 1
      fi
      if [ "$FAKE_SYNTHETIC_SECRET_KEY_PRESENT" = true ]; then
        printf '%s' "$FAKE_SYNTHETIC_SECRET_VALUE"
      fi
    elif [[ "$*" == *"app.kubernetes.io/name=postgresql,app.kubernetes.io/instance=selfservice"* ]]; then
      printf 'selfservice-postgresql-0'
    elif [ "$2" = "jobs" ]; then
      if [[ "$*" == *"-o json"* ]]; then
        if [ "$FAKE_FAIL_JOBS_LIST" = true ]; then
          echo "sabotaged jobs list failure" >&2
          exit 97
        fi
        if [ "$FAKE_MALFORMED_JOBS_JSON" = true ]; then
          printf '{"items":"not-an-array"}\n'
        else
          post_upgrade=
          if [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
            post_upgrade=$FAKE_POST_UPGRADE_ACTIVE_KUBERNETES_JOB
          fi
          jq -cn \
            --argjson active "$FAKE_ACTIVE_KUBERNETES_JOBS" \
            --argjson synthetic "$FAKE_ACTIVE_MUTATING_SYNTHETIC_JOBS" \
            --argjson completed "$FAKE_COMPLETED_MUTATING_SYNTHETIC_JOB" \
            --arg post_upgrade "$post_upgrade" \
            '{
              items: (
                (if $active > 0 then
                   [{metadata:{name:"active-fixture"},status:{active:$active}}]
                 else [] end) +
                (if $post_upgrade == "api-monitor-contained" or
                    $post_upgrade == "api-monitor-lifecycle" then
                   [{
                     metadata:{
                       name:"selfservice-synthetic-api-monitor-29802528",
                       ownerReferences:[{
                         apiVersion:"batch/v1",
                         kind:"CronJob",
                         name:"selfservice-synthetic-api-monitor",
                         controller:true
                       }]
                     },
                     spec:{template:{spec:{containers:[{
                       name:"synthetic-api-monitor",
                       env:[{
                         name:"SYNTHETIC_LIFECYCLE_ENABLED",
                         value:(if $post_upgrade == "api-monitor-contained" then "false" else "true" end)
                       }]
                     }]}}},
                     status:{active:1}
                   }]
                 elif $post_upgrade == "api-monitor-wrong-owner" then
                   [{
                     metadata:{
                       name:"selfservice-synthetic-api-monitor-29802528",
                       ownerReferences:[{
                         apiVersion:"apps/v1",
                         kind:"Deployment",
                         name:"selfservice-worker",
                         controller:true
                       }]
                     },
                     spec:{template:{spec:{containers:[{
                       name:"synthetic-api-monitor",
                       env:[{name:"SYNTHETIC_LIFECYCLE_ENABLED",value:"false"}]
                     }]}}},
                     status:{active:1}
                   }]
                 elif $post_upgrade == "runner" or $post_upgrade == "janitor" then
                   [{
                     metadata:{
                       name:("selfservice-synthetic-" + $post_upgrade + "-active"),
                       ownerReferences:[{
                         apiVersion:"batch/v1",
                         kind:"CronJob",
                         name:("selfservice-synthetic-" + $post_upgrade),
                         controller:true
                       }]
                     },
                     spec:{template:{spec:{containers:[{name:("synthetic-" + $post_upgrade)}]}}},
                     status:{active:1}
                   }]
                 elif $post_upgrade == "provisioning" then
                   [{
                     metadata:{name:"selfservice-image-import-active"},
                     spec:{template:{spec:{containers:[{name:"provision-worker"}]}}},
                     status:{active:1}
                   }]
                 elif $post_upgrade == "unknown" then
                   [{
                     metadata:{
                       name:"unexpected-active",
                       ownerReferences:[{
                         apiVersion:"batch/v1",
                         kind:"CronJob",
                         name:"selfservice-synthetic-api-monitor",
                         controller:true
                       }]
                     },
                     spec:{template:{spec:{containers:[{
                       name:"synthetic-api-monitor",
                       env:[{name:"SYNTHETIC_LIFECYCLE_ENABLED",value:"false"}]
                     }]}}},
                     status:{active:1}
                   }]
                 elif $post_upgrade == "" then
                   []
                 else
                   error("unknown post-upgrade active Job fixture")
                 end) +
                (if $synthetic > 0 then
                   [{
                     metadata:{
                       name:"synthetic-runner-active",
                       ownerReferences:[{kind:"CronJob",name:"selfservice-synthetic-runner"}]
                     },
                     status:{active:$synthetic}
                   }]
                 else [] end) +
                (if $completed then
                   [{
                     metadata:{
                       name:"synthetic-runner-complete",
                       ownerReferences:[{kind:"CronJob",name:"selfservice-synthetic-runner"}]
                     },
                     status:{conditions:[{type:"Complete",status:"True"}]}
                   }]
                 else [] end)
              )
            }'
        fi
      else
        if [[ "$*" == *'eq .name "selfservice-synthetic-janitor"'* ]] &&
         [ "$FAKE_NO_RETAINED_JANITOR_JOB" = true ]; then
        :
        elif [[ "$*" == *'eq .name "selfservice-synthetic-runner"'* ]] &&
           [ "$FAKE_NO_RETAINED_RUNNER_JOB" = true ]; then
        :
        else
        printf 'selfservice-synthetic-api-monitor-1\n'
        fi
      fi
    elif [[ "$2" == job/* ]]; then
      printf '1 0 0'
    elif [ "$2" = "pods" ] && [[ "$*" == *".imageID"* ]]; then
      for container in warmer api-gateway provision-worker engine ui synthetic-api-monitor synthetic-janitor synthetic-runner extra; do
        if [[ "$*" == *"@.name==\"$container\""* ]]; then
          image_for_container "$container"
          exit 0
        fi
      done
      echo "missing fake image status for: $*" >&2
      exit 92
    elif [ "$2" = "pods" ]; then
      printf 'NAME READY STATUS\n'
    elif [[ "$2" == */* ]]; then
      if [[ "$*" == *"-o yaml"* ]]; then
        resource_file=$(mktemp)
        extract_resource "$(current_manifest)" "$2" > "$resource_file"
        cat "$resource_file"
        rm -f "$resource_file"
      elif [[ "$*" == *"-o jsonpath={.spec.suspend}"* ]]; then
        resource_file=$(mktemp)
        extract_resource "$(current_manifest)" "$2" > "$resource_file"
        suspend=$(awk '/^  suspend:[[:space:]]*/ { print $2; found = 1; exit } END { if (!found) exit 3 }' "$resource_file")
        if [[ "$*" == *"SYNTHETIC_LIFECYCLE_ENABLED"* ]]; then
          lifecycle=$(awk '
            /- name: SYNTHETIC_LIFECYCLE_ENABLED/ { wanted = 1; next }
            wanted && /value:/ {
              value = $0
              sub(/^[[:space:]]*value:[[:space:]]*"?/, "", value)
              sub(/"?[[:space:]]*$/, "", value)
              print value
              found = 1
              exit
            }
            END { if (!found) exit 3 }
          ' "$resource_file")
          printf '%s\t%s' "$suspend" "$lifecycle"
        else
          printf '%s' "$suspend"
        fi
        rm -f "$resource_file"
      elif [[ "$*" == *"-o json"* ]]; then
        json_resource "$(current_manifest)" "$2" live
      else
        resource_file=$(mktemp)
        extract_resource "$(current_manifest)" "$2" > "$resource_file"
        printf '%s\n' "$2"
        rm -f "$resource_file"
      fi
    else
      echo "unexpected kubectl get invocation: $*" >&2
      exit 90
    fi
    ;;
  rollout)
    if [[ "$*" == *"deployment/selfservice-worker"* ]] &&
       [ -n "$FAKE_SIGNAL_AFTER_CLAIMS_PAUSE" ] &&
       [ -f "$FAKE_CLAIMS_PAUSED_MARKER" ] &&
       [ ! -f "$FAKE_CLAIMS_RESUMED_MARKER" ]; then
      kill -s "$FAKE_SIGNAL_AFTER_CLAIMS_PAUSE" "$PPID"
      sleep 1
    fi
    case "$*" in
      *"DaemonSet/selfservice-runner-image-warmer"*|*"daemonset/selfservice-runner-image-warmer"*)
       if [ "$FAKE_WARMER_ROLLOUT_FAILURE" = true ] && [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
         echo "sabotaged rollout status failure for selfservice-runner-image-warmer" >&2
         exit 94
       fi
       if [ "$FAKE_FINAL_WORKLOAD_HEALTH_REGRESSION" = true ] &&
          [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ] &&
          [ -f "$FAKE_CRONJOB_VERIFY_MARKER" ]; then
         echo "sabotaged workload health regression after external verification for selfservice-runner-image-warmer" >&2
         exit 95
       fi
       if [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
         : > "$FAKE_WARMER_ROLLOUT_MARKER"
       fi
       ;;
    esac
    if [ "$FAKE_POST_LIVE_HELM_REVISION" != 0 ] ||
       [ -n "$FAKE_POST_LIVE_HELM_STATUS" ] ||
       [ "$FAKE_POST_LIVE_HELM_HISTORY_EXIT" != 0 ]; then
      : > "$FAKE_LIVE_VERIFICATION_MARKER"
    fi
    ;;
  exec)
    if [[ "$*" == *"gate_a4_provisioning_preflight"* ]]; then
      [ "$FAKE_PREFLIGHT_QUERY_FAILURE" != provisioning ] || exit 97
      query_count=0
      [ ! -f "$FAKE_PROVISIONING_QUERY_COUNT" ] || query_count=$(cat "$FAKE_PROVISIONING_QUERY_COUNT")
      query_count=$((query_count + 1))
      printf '%s' "$query_count" > "$FAKE_PROVISIONING_QUERY_COUNT"
      if [ "$FAKE_UNKNOWN_PROVISIONING_JOB_STATUS" = true ]; then
        if [[ "$*" == *"status NOT IN ('completed', 'failed')"* ]]; then
          printf '1\n'
        else
          printf '0\n'
        fi
      else
        jobs=$FAKE_PROVISIONING_JOBS
        if [ "$query_count" -gt 1 ] && [ -n "$FAKE_PROVISIONING_JOBS_AFTER_INITIAL" ]; then
          jobs=$FAKE_PROVISIONING_JOBS_AFTER_INITIAL
        fi
        printf '%s\n' "$jobs"
      fi
    elif [[ "$*" == *"gate_a4_synthetic_quota_preflight"* ]]; then
      [ "$FAKE_PREFLIGHT_QUERY_FAILURE" != quota ] || exit 97
      IFS= read -r synthetic_user_id || exit 96
      if [[ "$*" != *"WHERE u.id = :'synthetic_user_id'::uuid"* ]] ||
         [[ "$*" != *'synthetic_user_id=$synthetic_user_id'* ]] ||
         [[ "$*" != *'printf "%s\n" "SELECT '* ]] ||
         [[ "$*" != *"-At"* ]] ||
         [[ "$*" == *"-Atc"* ]] ||
         [[ "$*" == *"u.oidc_sub"* ]] ||
         [[ "$*" == *"u.username"* ]] ||
         [ "$synthetic_user_id" != "$FAKE_SYNTHETIC_USER_ID" ] ||
         [ "$FAKE_SYNTHETIC_USER_KNOWN" != true ]; then
        exit 0
      fi
      [ "$FAKE_SYNTHETIC_OIDC_SUB" != synthetic-monitor-no-oidc ] || exit 96
      printf '%s\n' "$FAKE_SYNTHETIC_QUOTA_STATE"
    elif [[ "$*" == *"pod_name"* ]]; then
      [[ "$*" == *"pod_create"* && "$*" == *"pod_destroy"* ]] || {
        echo "synthetic durable drain omitted create or destroy jobs" >&2
        exit 96
      }
      [[ "$*" == *"LEFT JOIN pods"* && "$*" == *"p.name"* && "$*" == *"pod_id"* ]] || {
        echo "synthetic destroy drain did not resolve pod_id to the authoritative pod name" >&2
        exit 96
      }
      if [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
        printf '%s\n' "$FAKE_PENDING_SYNTHETIC_JOBS"
      else
        printf '0\n'
      fi
    elif [[ "$*" == *"FROM jobs"* ]]; then
      printf '%s\n' "$FAKE_ACTIVE_JOBS"
    else
      [ "$FAKE_PREFLIGHT_QUERY_FAILURE" != migration ] || exit 97
      printf '%s\n' "$FAKE_MIGRATION_STATE"
    fi
    ;;
  create)
    if [ "${2:-}" = job ] && [[ "$*" == *"--from=cronjob/"* ]] &&
       [[ "$*" == *"--dry-run=client"* ]] && [[ "$*" == *"-o json"* ]]; then
      source_cronjob=
      for argument in "$@"; do
        case "$argument" in
          --from=cronjob/*) source_cronjob=${argument#--from=cronjob/} ;;
        esac
      done
      case "$source_cronjob" in
        selfservice-synthetic-api-monitor) target_container=synthetic-api-monitor ;;
        selfservice-synthetic-janitor) target_container=synthetic-janitor ;;
        selfservice-synthetic-runner) target_container=synthetic-runner ;;
        *)
          echo "unknown fake source CronJob: $source_cronjob" >&2
          exit 96
          ;;
      esac
      resource_file=$(mktemp)
      extract_resource "$(current_manifest)" "CronJob/$source_cronjob" > "$resource_file"
      lifecycle=
      if [ "$target_container" = synthetic-api-monitor ]; then
        lifecycle=$(awk '
          /- name: SYNTHETIC_LIFECYCLE_ENABLED/ { wanted = 1; next }
          wanted && /value:/ {
            value = $0
            sub(/^[[:space:]]*value:[[:space:]]*"?/, "", value)
            sub(/"?[[:space:]]*$/, "", value)
            print value
            found = 1
            exit
          }
          END { if (!found) exit 3 }
        ' "$resource_file")
      fi
      image=$(awk -v target="$target_container" '
        $0 ~ "- name: " target "$" { wanted = 1; next }
        wanted && /image:/ {
          value = $0
          sub(/^[[:space:]]*image:[[:space:]]*"?/, "", value)
          sub(/"?[[:space:]]*$/, "", value)
          print value
          found = 1
          exit
        }
        END { if (!found) exit 3 }
      ' "$resource_file")
      rm -f "$resource_file"
      case "$FAKE_CRONJOB_VERIFICATION_SHAPE" in
        "")
          jq -cn \
            --arg name "$3" \
            --arg image "$image" \
            --arg target "$target_container" \
            --arg lifecycle "$lifecycle" \
            '{
              apiVersion:"batch/v1",
              kind:"Job",
              metadata:{name:$name},
              spec:{template:{spec:{
                restartPolicy:"Never",
                containers:[{
                  name:$target,
                  image:$image
                } + (
                  if $target == "synthetic-api-monitor" then
                    {env:[
                      {name:"SYNTHETIC_CONTENT_FILTER_EXPECTED",value:"false"},
                      {name:"SYNTHETIC_LIFECYCLE_ENABLED",value:$lifecycle}
                    ]}
                  else
                    {}
                  end
                )]
              }}}
            }'
          ;;
        missing-target)
          jq -cn \
            --arg name "$3" \
            --arg image "$image" \
            '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name},spec:{template:{spec:{containers:[{name:"wrong-container",image:$image}]}}}}'
          ;;
        missing-lifecycle)
          jq -cn \
            --arg name "$3" \
            --arg image "$image" \
            '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name},spec:{template:{spec:{containers:[{name:"synthetic-api-monitor",image:$image}]}}}}'
          ;;
        malformed-containers)
          jq -cn \
            --arg name "$3" \
            '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name},spec:{template:{spec:{containers:"not-an-array"}}}}'
          ;;
        *)
          echo "unknown fake verification Job shape: $FAKE_CRONJOB_VERIFICATION_SHAPE" >&2
          exit 96
          ;;
      esac
    elif [ "${2:-}" = "-f" ] && [ "${3:-}" != "-" ]; then
      manifest=$3
      if [ -f "$FAKE_CLAIMS_PAUSED_MARKER" ] &&
         jq -e '.spec.template.spec.containers[]? | select(.name == "synthetic-api-monitor")' \
           "$manifest" >/dev/null; then
        lifecycle=$(jq -er '
          [
            .spec.template.spec.containers[]? |
            select(.name == "synthetic-api-monitor") |
            .env[]? |
            select(.name == "SYNTHETIC_LIFECYCLE_ENABLED") |
            .value
          ] |
          if length == 1 then .[0] else error("expected one lifecycle value") end
        ' "$manifest")
        if [ "$lifecycle" != false ]; then
          echo "verification Job inherited SYNTHETIC_LIFECYCLE_ENABLED=$lifecycle while claims were paused" >&2
          exit 98
        fi
      fi
      if [ -n "$FAKE_CRONJOB_CREATE_LOG" ]; then
        printf '%s\n' "$(jq -r '.metadata.name' "$manifest")" >> "$FAKE_CRONJOB_CREATE_LOG"
      fi
      cp "$manifest" "$FAKE_CRONJOB_VERIFY_MANIFEST"
      : > "$FAKE_CRONJOB_VERIFY_MARKER"
      printf 'job.batch/%s created\n' "$(jq -r '.metadata.name' "$manifest")"
    elif [[ "$*" == *"--dry-run=server"* ]]; then
      manifest=
      while [ "$#" -gt 0 ]; do
        if [ "$1" = "-f" ]; then
          manifest=$2
          break
        fi
        shift
      done
      [ -n "$manifest" ]
      resource=$(awk '
        /^kind:[[:space:]]*/ {
          kind = $0
          sub(/^kind:[[:space:]]*/, "", kind)
        }
        /^metadata:[[:space:]]*$/ { metadata = 1 }
        metadata && /^  name:[[:space:]]*/ {
          name = $0
          sub(/^  name:[[:space:]]*/, "", name)
          print kind "/" name
          exit
        }
      ' "$manifest")
      [ -n "$resource" ]
      json_resource "$manifest" "$resource"
    else
      [ ! -f "$FAKE_LOCK_FILE" ] || exit 1
      body=$(cat)
      holder=$(printf '%s\n' "$body" | sed -n 's/.*crucible.jmal.io\/holder: "\(.*\)"/\1/p')
      [ -n "$holder" ]
      jq -cn \
        --arg holder "$holder" \
        --arg created_at "${holder##*|}" \
        '{
          apiVersion:"v1",
          kind:"ConfigMap",
          metadata:{
            name:"selfservice-phase1-deploy-lock",
            namespace:"selfservice",
            creationTimestamp:$created_at,
            uid:"11111111-1111-1111-1111-111111111111",
            resourceVersion:"100",
            annotations:{"crucible.jmal.io/holder":$holder}
          }
        }' > "$FAKE_LOCK_FILE"
      if [ -n "$FAKE_SIGNAL_DURING_LOCK_CREATE" ]; then
        kill -s "$FAKE_SIGNAL_DURING_LOCK_CREATE" "$PPID"
        sleep 1
      fi
      if [ "$FAKE_FAIL_LOCK_CREATE_AFTER_MUTATION" = true ]; then
        echo "sabotaged lost lock creation response" >&2
        exit 88
      fi
    fi
    ;;
  wait)
    ;;
  apply)
    printf '%s\n' "$*" >> "$FAKE_SERVER_DRY_RUN_LOG"
    # Parse the exact argv tokens actual kubectl would receive - never "$*"
    # substring matching, which a decoy value glued onto another flag (for
    # example --field-manager=X--dry-run=server, while the real --dry-run
    # value is actually "none") could satisfy even though the real flag is
    # unsafe. Every comparison below is against one exact, whole argv
    # element, exactly as bash itself would have split it.
    #
    # Counts, not one-shot booleans: a second independent review reported
    # that a prior version of this parser only ever *set* has_dry_run_server
    # to true and never reset it, so a conflicting, appended --dry-run=none
    # token elsewhere in the same argv (which real kubectl - like most
    # pflag-based CLIs - resolves by honoring the LAST occurrence of a
    # repeated flag) was invisible to it: the earlier, real --dry-run=server
    # token had already flipped the flag true, and nothing here noticed a
    # second, later, conflicting token. Counting every occurrence of each
    # flag family, and separately counting how many of those occurrences are
    # in the one exact, safe form, catches any duplicate or conflicting
    # variant regardless of where in the argv it appears or which one came
    # first.
    server_side_count=0
    server_side_ok_count=0
    dry_run_count=0
    dry_run_server_count=0
    force_conflicts_count=0
    force_conflicts_ok_count=0
    output_format=
    manifest=
    prev=
    for arg in "$@"; do
      case "$prev" in
        -f) manifest=$arg ;;
        -o) output_format=$arg ;;
      esac
      case "$arg" in
        --server-side)
          server_side_count=$((server_side_count + 1))
          server_side_ok_count=$((server_side_ok_count + 1))
          ;;
        --server-side=*)
          server_side_count=$((server_side_count + 1))
          ;;
        --dry-run=server)
          dry_run_count=$((dry_run_count + 1))
          dry_run_server_count=$((dry_run_server_count + 1))
          ;;
        --dry-run | --dry-run=*)
          dry_run_count=$((dry_run_count + 1))
          ;;
        --force-conflicts)
          force_conflicts_count=$((force_conflicts_count + 1))
          force_conflicts_ok_count=$((force_conflicts_ok_count + 1))
          ;;
        --force-conflicts=*)
          force_conflicts_count=$((force_conflicts_count + 1))
          ;;
      esac
      prev=$arg
    done
    has_force_conflicts=false
    [ "$force_conflicts_count" -eq 0 ] || has_force_conflicts=true
    # Whether --server-side and --dry-run=server are each present exactly
    # once, in exactly their one safe form - independent of whether
    # --force-conflicts is present at all, since the ownership-conflict
    # simulation below deliberately exercises the no-force-conflicts case.
    server_side_and_dry_run_exact=false
    if [ "$server_side_count" -eq 1 ] && [ "$server_side_ok_count" -eq 1 ] &&
       [ "$dry_run_count" -eq 1 ] && [ "$dry_run_server_count" -eq 1 ]; then
      server_side_and_dry_run_exact=true
    fi
    is_exact_safe_tuple=false
    if [ "$server_side_and_dry_run_exact" = true ] &&
       [ "$force_conflicts_count" -eq 1 ] && [ "$force_conflicts_ok_count" -eq 1 ]; then
      is_exact_safe_tuple=true
    fi
    # Fake-kubectl safety sentinel, not merely a test-only assertion: any
    # invocation that carries --force-conflicts at all, unless the argv is
    # EXACTLY the one safe tuple - one --server-side, one --dry-run=server
    # and no other --dry-run variant, one bare --force-conflicts and no
    # other force-conflicts variant - has exactly the shape of a real,
    # mutating, force-conflicts server-side apply against production (or is
    # one step away from becoming one via a duplicate/conflicting token) -
    # the one thing this whole change must never do. Fail loudly and
    # distinctly here (rather than silently proceeding as if it were the
    # intended validation-only call) so a sabotaged deploy.sh is caught the
    # moment it actually runs, independent of any static source-text check.
    if [ "$has_force_conflicts" = true ] && [ "$is_exact_safe_tuple" != true ]; then
      echo "FAKE KUBECTL SAFETY VIOLATION: --force-conflicts present without the exact required tuple of exactly one --server-side, one --dry-run=server, and one --force-conflicts (server_side=$server_side_count/$server_side_ok_count dry_run=$dry_run_count/$dry_run_server_count force_conflicts=$force_conflicts_count/$force_conflicts_ok_count); this is exactly the shape of a real, mutating apply in production: $*" >&2
      exit 87
    fi
    dry_run_count_log=$(count_logical_yaml_calls)
    if [ "$FAKE_FAIL_FINAL_SERVER_DRY_RUN" = true ] &&
       [ "$output_format" = yaml ] &&
       [ "$dry_run_count_log" -ge 2 ]; then
      echo "sabotaged final server-side dry-run failure" >&2
      exit 95
    fi
    if [ -n "${FAKE_FAIL_SERVER_VALIDATE_DOCUMENT:-}" ] &&
       [ "$output_format" = yaml ] &&
       grep -q "^  name: $FAKE_FAIL_SERVER_VALIDATE_DOCUMENT\$" "$manifest"; then
      echo "sabotaged per-document server-side dry-run failure for $FAKE_FAIL_SERVER_VALIDATE_DOCUMENT" >&2
      exit 93
    fi
    if [ "$FAKE_SIMULATE_OWNERSHIP_CONFLICT" = true ] &&
       [ "$server_side_and_dry_run_exact" = true ] &&
       [ "$has_force_conflicts" != true ]; then
      echo "error: Apply failed with 1 conflict: conflict with \"helm\" using apps/v1: .spec.replicas" >&2
      echo "Please review the fields above--they currently have other managers. Here" >&2
      echo "are the ways you can resolve this warning:" >&2
      echo "* If you intend to manage all of these fields, please re-run the apply" >&2
      echo "  command with the --force-conflicts flag." >&2
      exit 1
    fi
    [ -n "$manifest" ]
    if [ "$output_format" = json ]; then
      resource=$(awk '
        /^kind:[[:space:]]*/ {
          kind = $0
          sub(/^kind:[[:space:]]*/, "", kind)
        }
        /^metadata:[[:space:]]*$/ { metadata = 1 }
        metadata && /^  name:[[:space:]]*/ {
          name = $0
          sub(/^  name:[[:space:]]*/, "", name)
          print kind "/" name
          exit
        }
      ' "$manifest")
      [ -n "$resource" ]
      source=desired
      if [ -f "$FAKE_UPGRADED_MARKER" ]; then
        case "$manifest" in
          *expected.server.canonical.documents/*) source=post-upgrade-expected ;;
          *) source=post-apply ;;
        esac
      fi
      json_resource "$manifest" "$resource" "$source"
    else
      # Real kubectl does not return "---"-separated top-level documents
      # for a multi-document "-f" "-o yaml" apply: it collapses every
      # applied object into one "apiVersion: v1, kind: List" wrapper with
      # the individual objects nested (indented) under "items:". This is
      # the exact production bug server_validate_candidate's per-document
      # splitting works around (see its comment in deploy.sh) - reproduce
      # it here so a regression (e.g. reverting to one whole-manifest call)
      # is caught by tests, not just discovered in production. A
      # single-document "-f" input (the shape every current caller now
      # uses) is unaffected and still echoed as-is.
      doc_count=$(awk '
        /^---[[:space:]]*$/ { if (doc != "") { c++ }; doc = ""; next }
        { doc = doc $0 }
        END { if (doc != "") { c++ }; print c + 0 }
      ' "$manifest")
      if [ "$doc_count" -gt 1 ]; then
        printf 'apiVersion: v1\nkind: List\nmetadata:\n  resourceVersion: ""\nitems:\n'
        sed -e '/^---[[:space:]]*$/d' -e 's/^/  /' "$manifest"
      else
        cat "$manifest"
      fi
      : > "$FAKE_SERVER_DRY_RUN_MARKER"
    fi
    ;;
  delete)
    if [[ "$*" == *"--raw="* ]]; then
      delete_options=
      previous=
      for argument in "$@"; do
        if [ "$previous" = "-f" ]; then
          delete_options=$argument
          break
        fi
        previous=$argument
      done
      [ -n "$delete_options" ]
      expected_uid=$(jq -r '.preconditions.uid // empty' "$delete_options")
      expected_resource_version=$(jq -r '.preconditions.resourceVersion // empty' "$delete_options")
      if [ -n "$FAKE_LOCK_DELETE_RACE_MODE" ] &&
         [ ! -f "$FAKE_LOCK_DELETE_RACE_MARKER" ]; then
        case "$FAKE_LOCK_DELETE_RACE_MODE" in
          both)
            mutation='.metadata.uid = "replacement-lock-uid" | .metadata.resourceVersion = "999"'
            ;;
          uid)
            mutation='.metadata.uid = "replacement-lock-uid"'
            ;;
          resourceVersion)
            mutation='.metadata.resourceVersion = "999"'
            ;;
          *)
            echo "unexpected lock race mode $FAKE_LOCK_DELETE_RACE_MODE" >&2
            exit 98
            ;;
        esac
        jq "$mutation" "$FAKE_LOCK_FILE" > "$FAKE_LOCK_FILE.replacement"
        mv "$FAKE_LOCK_FILE.replacement" "$FAKE_LOCK_FILE"
        : > "$FAKE_LOCK_DELETE_RACE_MARKER"
      fi
      if [ "$(jq -r '.apiVersion' "$delete_options")" != "v1" ] ||
         [ "$(jq -r '.kind' "$delete_options")" != "DeleteOptions" ]; then
        echo "invalid raw DeleteOptions envelope" >&2
        exit 1
      fi
      expected_uri="/api/v1/namespaces/selfservice/configmaps/selfservice-phase1-deploy-lock"
      actual_uri=
      for argument in "$@"; do
        case "$argument" in
          --raw=*) actual_uri=${argument#--raw=} ;;
        esac
      done
      if [ "$actual_uri" != "$expected_uri" ]; then
        echo "invalid raw lock deletion URI $actual_uri" >&2
        exit 1
      fi
      current_uid=$(jq -er '.metadata.uid' "$FAKE_LOCK_FILE")
      current_resource_version=$(jq -er '.metadata.resourceVersion' "$FAKE_LOCK_FILE")
      if { [ -n "$expected_uid" ] && [ "$expected_uid" != "$current_uid" ]; } ||
         { [ -n "$expected_resource_version" ] &&
           [ "$expected_resource_version" != "$current_resource_version" ]; }; then
        echo "Conflict: lock precondition failed" >&2
        exit 1
      fi
      rm -f "$FAKE_LOCK_FILE"
    else
      rm -f "$FAKE_LOCK_FILE"
    fi
    ;;
  patch)
    : > "$FAKE_SYNTHETIC_CONTAINED_MARKER"
    if [ "$2" = "cronjob/selfservice-synthetic-api-monitor" ] &&
       [[ "$*" == *"SYNTHETIC_LIFECYCLE_ENABLED"* ]]; then
      manifest=$(current_manifest)
      sed '/- name: SYNTHETIC_LIFECYCLE_ENABLED/{n;s/value: "true"/value: "false"/;}' \
        "$manifest" > "$manifest.patched"
      mv "$manifest.patched" "$manifest"
    fi
    ;;
  set)
    [ -f "$FAKE_LOCK_FILE" ] || {
      echo "claims pause attempted without the release lock" >&2
      exit 97
    }
    # Honor the requested value rather than assuming a pause: deploy.sh both
    # pauses (=false) before the guarded apply and resumes (=true) after the
    # candidate verifies, and a fake that always pauses would make a broken
    # resume look successful.
    claims_value=
    for arg in "$@"; do
      case "$arg" in
        WORKER_PROVISIONING_CLAIMS_ENABLED=*)
          claims_value="${arg#WORKER_PROVISIONING_CLAIMS_ENABLED=}"
          ;;
      esac
    done
    case "$claims_value" in
      false)
        if [ "$FAKE_FAIL_CLAIMS_PAUSE_BEFORE_MUTATION" = true ]; then
          echo "sabotaged claims pause failure before mutation" >&2
          exit 85
        fi
        sed '/- name: WORKER_PROVISIONING_CLAIMS_ENABLED/{n;s/value: "true"/value: "false"/;}' \
          "$FAKE_LIVE_RESOURCE_MANIFEST" > "$FAKE_LIVE_RESOURCE_MANIFEST.paused"
        : > "$FAKE_CLAIMS_PAUSED_MARKER"
        if [ -n "$FAKE_SIGNAL_DURING_CLAIMS_PAUSE" ]; then
          kill -s "$FAKE_SIGNAL_DURING_CLAIMS_PAUSE" "$PPID"
          sleep 1
        fi
        ;;
      true)
        if [ "$FAKE_FAIL_CLAIMS_RESUME" = true ]; then
          echo "sabotaged claims resume failure" >&2
          exit 86
        fi
        sed '/- name: WORKER_PROVISIONING_CLAIMS_ENABLED/{n;s/value: "false"/value: "true"/;}' \
          "$FAKE_LIVE_RESOURCE_MANIFEST" > "$FAKE_LIVE_RESOURCE_MANIFEST.resumed"
        mv "$FAKE_LIVE_RESOURCE_MANIFEST.resumed" "$FAKE_LIVE_RESOURCE_MANIFEST"
        : > "$FAKE_CLAIMS_RESUMED_MARKER"
        ;;
      *)
        echo "unexpected claims value: $*" >&2
        exit 96
        ;;
    esac
    ;;
  *)
    echo "unexpected kubectl invocation: $*" >&2
    exit 90
    ;;
esac
`)

	writeExecutable(e.t, filepath.Join(e.binDir, "git"), `#!/bin/bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GIT_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-C" ]; then
    shift 2
    continue
  fi
  break
done
case "${1:-}" in
  status)
    [ "${FAKE_GIT_DIRTY:-false}" != true ] || printf ' M deploy/scripts/deploy.sh\n'
    ;;
  symbolic-ref)
    [ "$FAKE_GIT_BRANCH" != detached ] || exit 1
    printf '%s\n' "$FAKE_GIT_BRANCH"
    ;;
  rev-parse)
    if [ "${2:-}" = HEAD ]; then
      printf '%s\n' "$FAKE_SOURCE_SHA"
    else
      printf '%s\n' "$FAKE_GIT_REMOTE_SHA"
    fi
    ;;
  remote)
    printf '%s\n' "$FAKE_GIT_REMOTE_URL"
    ;;
  pull)
    ;;
  *)
    echo "unexpected git invocation: $*" >&2
    exit 91
    ;;
esac
`)

	writeExecutable(e.t, filepath.Join(e.binDir, "gh"), `#!/bin/bash
set -euo pipefail
case "$*" in
  "auth token")
    printf 'github-token\n'
    ;;
  "run download "*)
    output_dir=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "--dir" ]; then
        output_dir=$2
        break
      fi
      shift
    done
    [ -n "$output_dir" ]
    for component in api-gateway provision-worker crucible-engine synthetic-api-monitor crucible-runner; do
      artifact_dir="$output_dir/image-digest-$component"
      mkdir -p "$artifact_dir"
      printf '%s\t%s\tsha256:%s\t%s\n' \
        "$component" \
        "ghcr.io/jmal1/selfservice-$component" \
        "$FAKE_RUN_ARTIFACT_DIGEST" \
        "$FAKE_SOURCE_SHA" \
        > "$artifact_dir/image-digest.tsv"
    done
    ;;
  *"repos/jmal1/selfservice-api/commits/"*)
    jq -cn \
      --arg sha "$FAKE_SOURCE_SHA" \
      --argjson verified "$FAKE_COMMIT_VERIFIED" \
      '{sha:$sha,commit:{verification:{verified:$verified}}}'
    ;;
  *"repos/jmal1/selfservice-ui/commits/"*)
    jq -cn \
      --arg sha "$FAKE_UI_SOURCE_SHA" \
      --argjson verified "$FAKE_UI_COMMIT_VERIFIED" \
      '{sha:$sha,commit:{verification:{verified:$verified}}}'
    ;;
  *"repos/jmal1/selfservice-ui/actions/workflows/ci.yaml/runs"*)
    if [[ "$*" == *"head_sha=$FAKE_UI_SOURCE_SHA"* ]]; then
      jq -cn \
       --arg sha "$FAKE_UI_SOURCE_SHA" \
       '{workflow_runs:[{id:9002,run_number:51,run_attempt:1,head_sha:$sha,head_branch:"master",event:"push",status:"completed",conclusion:"success"}]}'
    else
      printf '{"workflow_runs":[]}\n'
    fi
    ;;
  *"repos/jmal1/selfservice-ui/actions/runs/9002/jobs"*)
    printf '{"jobs":[{"name":"test","status":"completed","conclusion":"success"},{"name":"build","status":"completed","conclusion":"success"}]}\n'
    ;;
  *"repos/jmal1/selfservice-ui/actions/runs/9002/artifacts"*)
    if [ "$FAKE_UI_BUILD_RECORD_MISSING" = true ]; then
      printf '{"artifacts":[]}\n'
    else
      printf '{"artifacts":[{"id":9102,"name":"jmal1~selfservice-ui~fixture.dockerbuild","expired":false}]}\n'
    fi
    ;;
  *"repos/jmal1/selfservice-ui/actions/artifacts/9102/zip"*)
    cat "$FAKE_UI_BUILD_RECORD"
    ;;
  *"actions/workflows/ci.yaml/runs"*)
    jq -cn \
      --arg sha "$FAKE_SOURCE_SHA" \
      '{workflow_runs:[{id:9001,run_number:42,head_sha:$sha,head_branch:"main",event:"push",status:"completed",conclusion:"success"}]}'
    ;;
  *"actions/runs/9001/jobs"*)
    jq -cn --arg missing "$FAKE_MISSING_BUILD" '{
      jobs: (
        ["api-gateway","provision-worker","crucible-engine","synthetic-api-monitor","crucible-runner"]
        | map(select(. != $missing) | {name:("build (" + . + ")"),status:"completed",conclusion:"success"})
      )
    }'
    ;;
  *"/packages/container/"*)
    tags=$FAKE_PACKAGE_TAG
    [ -z "$FAKE_PACKAGE_ADDITIONAL_TAG" ] || tags="$tags,$FAKE_PACKAGE_ADDITIONAL_TAG"
    printf 'sha256:%s\t%s\n' "$FAKE_PACKAGE_DIGEST" "$tags"
    ;;
  *)
    echo "unexpected gh invocation: $*" >&2
    exit 96
    ;;
esac
`)

	writeExecutable(e.t, filepath.Join(e.binDir, "curl"), `#!/bin/bash
set -euo pipefail
case "$*" in
  *"ghcr.io/token"*)
    printf '{"token":"registry-token"}\n'
    ;;
  *"/blobs/"*)
    revision=$FAKE_IMAGE_REVISION
    [[ "$*" != *"/jmal1/selfservice-ui/"* ]] || revision=$FAKE_UI_IMAGE_REVISION
    count=0
    [ ! -f "$FAKE_IMAGE_REVISION_PROBE_COUNT" ] || count=$(cat "$FAKE_IMAGE_REVISION_PROBE_COUNT")
    count=$((count + 1))
    printf '%s' "$count" > "$FAKE_IMAGE_REVISION_PROBE_COUNT"
    if [ "$FAKE_IMAGE_REVISION_DRIFT_AFTER" -gt 0 ] &&
       [ "$count" -gt "$FAKE_IMAGE_REVISION_DRIFT_AFTER" ]; then
      revision=$FAKE_OTHER_SOURCE_SHA
    fi
    jq -cn --arg revision "$revision" \
      '{config:{Labels:{"org.opencontainers.image.revision":$revision}}}'
    ;;
  *"/manifests/"*)
    jq -cn --arg digest "sha256:$FAKE_CONFIG_DIGEST" \
      '{mediaType:"application/vnd.oci.image.manifest.v1+json",config:{digest:$digest}}'
    ;;
  *)
    echo "unexpected curl invocation: $*" >&2
    exit 98
    ;;
esac
`)
}

func (e *deployScriptEnvironment) writeUIBuildRecord() {
	e.t.Helper()
	files := map[string]string{
		"index.json": `{
  "schemaVersion": 2,
  "manifests": [{"digest": "sha256:` + testDigestA + `"}]
}`,
		"blobs/sha256/" + testDigestA: `{
  "schemaVersion": 2,
  "config": {"digest": "sha256:` + testDigestB + `"}
}`,
		"blobs/sha256/" + testDigestB: `{
  "FrontendAttrs": {
    "attest:provenance": "type=provenance,builder-id=https://github.com/jmal1/selfservice-ui/actions/runs/9002/attempts/1,mode=min",
    "label:org.opencontainers.image.revision": "` + e.uiArtifactSHA + `",
    "label:org.opencontainers.image.source": "https://github.com/jmal1/selfservice-ui"
  },
  "Exporters": [{
    "Type": "image",
    "Attrs": {"name": "ghcr.io/jmal1/selfservice-ui:master,ghcr.io/jmal1/selfservice-ui:` + e.uiArtifactSHA[:7] + `"}
  }],
  "Result": {
    "ResultDeprecated": {"digest": "sha256:` + e.uiArtifactDigest + `"},
    "Results": {"0": {"digest": "sha256:` + e.uiArtifactDigest + `"}}
  }
}`,
	}
	var record bytes.Buffer
	gzipWriter := gzip.NewWriter(&record)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range []string{
		"index.json",
		"blobs/sha256/" + testDigestA,
		"blobs/sha256/" + testDigestB,
	} {
		body := []byte(files[name])
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			e.t.Fatal(err)
		}
		if _, err := tarWriter.Write(body); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		e.t.Fatal(err)
	}
	if e.uiBuildRecordRaw {
		if err := os.WriteFile(e.uiBuildRecord, record.Bytes(), 0o644); err != nil {
			e.t.Fatal(err)
		}
		return
	}
	var artifact bytes.Buffer
	zipWriter := zip.NewWriter(&artifact)
	entry, err := zipWriter.Create("jmal1~selfservice-ui~fixture.dockerbuild")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := entry.Write(record.Bytes()); err != nil {
		e.t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.uiBuildRecord, artifact.Bytes(), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *deployScriptEnvironment) run(args ...string) ([]byte, error) {
	return e.runWithUI(true, args...)
}

func (e *deployScriptEnvironment) runWithUI(includeUI bool, args ...string) ([]byte, error) {
	e.t.Helper()
	e.writeUIBuildRecord()
	if includeUI {
		args = append(args, "--ui-source-sha", e.uiSourceSHA)
	}
	script := e.scriptPath
	if script == "" {
		script = filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(
		os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG=/dev/null",
		"FAKE_LIVE_MANIFEST="+e.liveManifest,
		"FAKE_LIVE_RESOURCE_MANIFEST="+e.liveResource,
		// Keep the fake live Deployment revision in sync with the active test fixture,
		// not the stale historical 163 baseline. Otherwise unrelated tests trip the
		// immutable rollback-equivalence gate simply because the synthetic live state
		// is still pegged to the old baseline number.
		"FAKE_LIVE_DEPLOYMENT_REVISION="+strconv.Itoa(e.helmRevision),
		"FAKE_LIVE_DAEMONSET_GENERATION=7",
		"FAKE_LIVE_HELM_RELEASE="+e.liveHelmRelease,
		"FAKE_LIVE_HELM_NAMESPACE="+e.liveHelmNamespace,
		"FAKE_CANDIDATE_MANIFEST="+e.candidateManifest,
		"FAKE_UPGRADE_HOOK_MANIFEST="+e.upgradeHookManifest,
		"FAKE_BASELINE_MANIFEST="+e.baselineManifest,
		"FAKE_APPLIED_MANIFEST="+e.appliedManifest,
		"FAKE_UPGRADED_MARKER="+e.upgradedMarker,
		"FAKE_UPGRADE_LOG="+e.upgradeLog,
		"FAKE_TEMPLATE_LOG="+e.templateLog,
		"FAKE_GIT_LOG="+e.gitLog,
		"FAKE_LOCK_FILE="+e.lockFile,
		"FAKE_LOCK_BACKUP_DIR="+e.lockBackupDir,
		"FAKE_LOCK_DELETE_RACE_MODE="+e.lockDeleteRaceMode,
		"FAKE_LOCK_DELETE_RACE_MARKER="+e.lockDeleteRaceMark,
		"FAKE_SIGNAL_DURING_LOCK_CREATE="+e.signalDuringLockCreate,
		"FAKE_FAIL_LOCK_CREATE_AFTER_MUTATION="+strconv.FormatBool(e.failLockCreateAfterMutation),
		"FAKE_SERVER_DRY_RUN_LOG="+e.serverDryRunLog,
		"FAKE_SERVER_DRY_RUN_MARKER="+e.serverDryRunMark,
		"FAKE_FINAL_ROLLBACK_MARKER="+e.finalRollbackMark,
		"FAKE_LIVE_VERIFICATION_MARKER="+e.liveVerificationMark,
		"FAKE_EXTRA_IMAGE_COUNT="+e.extraImageCount,
		"FAKE_ATOMIC_FAILED_MARKER="+e.atomicFailedMark,
		"FAKE_CANDIDATE_APPLIED_MARKER="+e.candidateAppliedMark,
		"FAKE_SYNTHETIC_CONTAINED_MARKER="+e.syntheticContainedMark,
		"FAKE_ATOMIC_ROLLBACK_MANIFEST="+e.atomicRollbackManifest,
		"FAKE_CONTAINED_ROLLBACK_MANIFEST="+e.containedRollbackManifest,
		"FAKE_IMMUTABLE_ROLLBACK_MANIFEST="+e.immutableRollbackManifest,
		"FAKE_IMMUTABLE_ROLLBACK_HOOKS="+e.immutableRollbackHooks,
		"FAKE_CURRENT_ROLLBACK_HOOKS="+e.currentRollbackHooks,
		"FAKE_IMMUTABLE_ROLLBACK_VALUES="+e.immutableRollbackValues,
		"FAKE_CURRENT_ROLLBACK_VALUES="+e.currentRollbackValues,
		"FAKE_CRONJOB_VERIFY_MARKER="+e.cronjobVerifyMark,
		"FAKE_CRONJOB_VERIFY_MANIFEST="+e.cronjobVerifyManifest,
		"FAKE_CRONJOB_CREATE_LOG="+e.cronjobCreateLog,
		"FAKE_CRONJOB_VERIFICATION_SHAPE="+e.cronjobVerificationShape,
		"FAKE_CLAIMS_PAUSED_MARKER="+e.claimsPausedMark,
		"FAKE_CLAIMS_RESUMED_MARKER="+e.claimsResumedMark,
		"FAKE_FAIL_CLAIMS_RESUME="+strconv.FormatBool(e.failClaimsResume),
		"FAKE_FAIL_CLAIMS_PAUSE_BEFORE_MUTATION="+strconv.FormatBool(e.failClaimsPauseBeforeMutation),
		"FAKE_SIGNAL_AFTER_CLAIMS_PAUSE="+e.signalAfterClaimsPause,
		"FAKE_SIGNAL_DURING_CLAIMS_PAUSE="+e.signalDuringClaimsPause,
		"FAKE_SIGNAL_DURING_HELM="+e.signalDuringHelm,
		"FAKE_WARMER_ROLLOUT_MARKER="+e.warmerRolloutMark,
		"FAKE_WARMER_IMAGE_PROBE_LOG="+e.warmerImageProbeLog,
		"FAKE_WARMER_ROLLOUT_FAILURE="+strconv.FormatBool(e.warmerRolloutFailure),
		"FAKE_WARMER_IMAGE_STAYS_EMPTY="+strconv.FormatBool(e.warmerImageStaysEmpty),
		"FAKE_FINAL_WORKLOAD_HEALTH_REGRESSION="+strconv.FormatBool(e.finalWorkloadHealthRegression),
		"FAKE_MIGRATION_STATE="+e.migrationState,
		"FAKE_PROVISIONING_JOBS="+e.provisioningJobs,
		"FAKE_PROVISIONING_JOBS_AFTER_INITIAL="+e.provisioningJobsAfterInitial,
		"FAKE_PROVISIONING_QUERY_COUNT="+e.provisioningQueryCount,
		"FAKE_UNKNOWN_PROVISIONING_JOB_STATUS="+strconv.FormatBool(e.unknownProvisioningJobStatus),
		"FAKE_SYNTHETIC_QUOTA_STATE="+e.syntheticQuotaState,
		"FAKE_SYNTHETIC_USER_ID="+e.syntheticUserID,
		"FAKE_SYNTHETIC_OIDC_SUB="+e.syntheticOIDCSub,
		"FAKE_SYNTHETIC_SECRET_VALUE="+e.syntheticSecretValue,
		"FAKE_SYNTHETIC_SECRET_EXISTS="+strconv.FormatBool(e.syntheticSecretExists),
		"FAKE_SYNTHETIC_SECRET_KEY_PRESENT="+strconv.FormatBool(e.syntheticSecretKeyPresent),
		"FAKE_FAIL_SYNTHETIC_SECRET_READ="+strconv.FormatBool(e.failSyntheticSecretRead),
		"FAKE_SYNTHETIC_USER_KNOWN="+strconv.FormatBool(e.syntheticUserKnown),
		"FAKE_ACTIVE_MUTATING_SYNTHETIC_JOBS="+strconv.Itoa(e.activeMutatingSyntheticJobs),
		"FAKE_COMPLETED_MUTATING_SYNTHETIC_JOB="+strconv.FormatBool(e.completedMutatingSyntheticJob),
		"FAKE_NO_RETAINED_JANITOR_JOB="+strconv.FormatBool(e.noRetainedJanitorJob),
		"FAKE_NO_RETAINED_RUNNER_JOB="+strconv.FormatBool(e.noRetainedRunnerJob),
		"FAKE_MALFORMED_JOBS_JSON="+strconv.FormatBool(e.malformedJobsJSON),
		"FAKE_PREFLIGHT_QUERY_FAILURE="+e.preflightQueryFailure,
		"FAKE_FAIL_JOBS_LIST="+strconv.FormatBool(e.failJobsList),
		"FAKE_IMAGE_REVISION_PROBE_COUNT="+e.imageRevisionProbeCount,
		"FAKE_IMAGE_REVISION_DRIFT_AFTER="+strconv.Itoa(e.imageRevisionDriftAfter),
		"FAKE_OTHER_SOURCE_SHA="+otherSourceSHA,
		"FAKE_HELM_STATUS="+e.helmStatus,
		"FAKE_HELM_REVISION="+strconv.Itoa(e.helmRevision),
		"FAKE_IMMUTABLE_REVISION="+strconv.Itoa(e.immutableRevision),
		"FAKE_IMMUTABLE_BASELINE_REVISION="+strconv.Itoa(e.immutableBaselineRevision),
		"FAKE_POST_LIVE_HELM_REVISION="+strconv.Itoa(e.postLiveHelmRevision),
		"FAKE_POST_LIVE_HELM_STATUS="+e.postLiveHelmStatus,
		"FAKE_POST_LIVE_HELM_HISTORY_EXIT="+strconv.Itoa(e.postLiveHelmHistoryExit),
		"FAKE_HELM_DESCRIPTION="+e.helmDescription,
		"FAKE_IMMUTABLE_REVISION_MISSING="+strconv.FormatBool(e.immutableRevisionMissing),
		"FAKE_IMMUTABLE_HELM_STATUS="+e.immutableHelmStatus,
		"FAKE_IMMUTABLE_CHART_VERSION="+e.immutableChartVersion,
		"FAKE_CURRENT_CHART_VERSION="+e.currentChartVersion,
		"FAKE_MISMATCH_CONTAINER="+e.mismatchContainer,
		"FAKE_EXTERNAL_DRIFT_AFTER_SERVER_DRY_RUN="+strconv.FormatBool(e.externalDriftAfterServerDryRun),
		"FAKE_FAIL_FINAL_SERVER_DRY_RUN="+strconv.FormatBool(e.failFinalServerDryRun),
		"FAKE_MUTATE_FINAL_SERVER_OBJECT="+strconv.FormatBool(e.mutateFinalServerObject),
		"FAKE_SIMULATE_OWNERSHIP_CONFLICT="+strconv.FormatBool(e.simulateOwnershipConflict),
		"FAKE_FAIL_SERVER_VALIDATE_DOCUMENT="+e.failServerValidateDocument,
		"FAKE_DIGEST_A="+testDigestA,
		"FAKE_DIGEST_B="+testDigestB,
		"FAKE_SOURCE_SHA="+testSourceSHA,
		"FAKE_PACKAGE_TAG="+e.packageTag,
		"FAKE_PACKAGE_ADDITIONAL_TAG="+e.packageAdditionalTag,
		"FAKE_PACKAGE_DIGEST="+e.packageDigest,
		"FAKE_RUN_ARTIFACT_DIGEST="+e.runArtifactDigest,
		"FAKE_IMAGE_REVISION="+e.imageRevision,
		"FAKE_UI_BUILD_RECORD="+e.uiBuildRecord,
		"FAKE_UI_BUILD_RECORD_MISSING="+strconv.FormatBool(e.uiBuildRecordMissing),
		"FAKE_UI_SOURCE_SHA="+e.uiSourceSHA,
		"FAKE_UI_IMAGE_REVISION="+e.uiImageRevision,
		"FAKE_UI_COMMIT_VERIFIED="+strconv.FormatBool(e.uiCommitVerified),
		"FAKE_CONFIG_DIGEST="+testDigestA,
		"FAKE_EXTRA_REPOSITORY="+e.extraRepository,
		"FAKE_ACTIVE_JOBS="+strconv.Itoa(e.activeJobs),
		"FAKE_ACTIVE_KUBERNETES_JOBS="+strconv.Itoa(e.activeKubernetesJobs),
		"FAKE_POST_UPGRADE_ACTIVE_KUBERNETES_JOB="+e.postUpgradeActiveKubernetesJob,
		"FAKE_PENDING_SYNTHETIC_JOBS="+strconv.Itoa(e.pendingSyntheticJobs),
		"FAKE_FAIL_ATOMIC_UPGRADE="+strconv.FormatBool(e.failAtomicUpgrade),
		"FAKE_ATOMIC_ROLLBACK_MISMATCH="+e.atomicRollbackMismatch,
		"FAKE_POST_UPGRADE_MISMATCH="+e.postUpgradeMismatch,
		"FAKE_POST_UPGRADE_OBJECT_MUTATION="+strconv.FormatBool(e.postUpgradeObjectMutation),
		"FAKE_RETAIN_LIVE_JANITOR_TTL_BEFORE_UPGRADE="+strconv.FormatBool(e.retainLiveJanitorTTLBeforeUpgrade),
		"FAKE_JANITOR_TTL_SSA_LOG="+e.janitorTTLSSALog,
		"FAKE_POST_APPLY_ANNOTATIONS_MODE="+e.postApplyAnnotationsMode,
		"FAKE_GIT_BRANCH="+e.gitBranch,
		"FAKE_GIT_REMOTE_SHA="+e.gitRemoteSHA,
		"FAKE_GIT_REMOTE_URL="+e.gitRemoteURL,
		"FAKE_GIT_DIRTY="+strconv.FormatBool(e.gitDirty),
		"FAKE_COMMIT_VERIFIED="+strconv.FormatBool(e.commitVerified),
		"FAKE_MISSING_BUILD="+e.missingBuild,
		"HELM_RELEASE_LOCK_BACKUP_DIR="+e.lockBackupDir,
		"HELM_RELEASE_LOCK_TTL_SECONDS="+e.lockTTLSeconds,
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
		engineImage = "        image: " + image("engine", "ghcr.io/jmal1/selfservice-crucible-engine") + "\n"
	}
	runnerImage := image("warmer", "ghcr.io/jmal1/selfservice-crucible-runner")
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
        image: ` + image("api-gateway", "ghcr.io/jmal1/selfservice-api-gateway") + `
        env:
        - name: PROVISIONING_ENABLED
          value: "false"
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
        image: ` + image("provision-worker", "ghcr.io/jmal1/selfservice-provision-worker") + `
        env:
        - name: WORKER_PROVISIONING_CLAIMS_ENABLED
          value: "` + claims + `"
        - name: WORKER_CONTENT_FILTER_ENABLED
          value: "false"
        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
          value: ""
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
        image: ` + image("ui", "ghcr.io/jmal1/selfservice-ui") + `
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-api-monitor
spec:
  schedule: "*/10 * * * *"
  suspend: false
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
            image: ` + image("synthetic-api-monitor", "ghcr.io/jmal1/selfservice-synthetic-api-monitor") + `
            env:
            - name: SYNTHETIC_CONTENT_FILTER_EXPECTED
              value: "false"
            - name: SYNTHETIC_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
              value: ""
            - name: SYNTHETIC_PROVISIONING_EXPECTED_ENABLED
              value: "false"
            - name: SYNTHETIC_RUNNER_EXPECTED_ENABLED
              value: "false"
            - name: SYNTHETIC_LIFECYCLE_ENABLED
              value: "false"
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

func rollbackManifestWithHistoricalSynthetics(contained bool) string {
	manifest := baselineManifest(true, "", "false")
	lifecycle := "true"
	if contained {
		lifecycle = "false"
	}
	if !contained {
		manifest = replaceEnvValue(manifest, "SYNTHETIC_LIFECYCLE_ENABLED", "false", lifecycle)
	}
	return manifest + historicalSyntheticCronJobs(contained)
}

func historicalSyntheticCronJobs(suspend bool) string {
	return `---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-janitor
spec:
  suspend: ` + strconv.FormatBool(suspend) + `
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: synthetic-janitor
            image: ghcr.io/jmal1/selfservice-provision-worker@sha256:` + testDigestA + `
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-runner
spec:
  suspend: ` + strconv.FormatBool(suspend) + `
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
          - name: synthetic-runner
            image: ghcr.io/jmal1/selfservice-crucible-runner@sha256:` + testDigestA + `
`
}

func upgradeHookManifest(image string) string {
	return `apiVersion: batch/v1
kind: Job
metadata:
  name: selfservice-upgrade-hook
  annotations:
    helm.sh/hook: pre-upgrade
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: upgrade-hook
        image: ` + image + `
`
}

func replaceEnvValue(manifest, name, oldValue, newValue string) string {
	nameMarker := "- name: " + name
	nameIndex := strings.Index(manifest, nameMarker)
	if nameIndex < 0 {
		panic("environment sabotage fixture did not match " + name)
	}
	valueMarker := `value: "` + oldValue + `"`
	valueOffset := strings.Index(manifest[nameIndex+len(nameMarker):], valueMarker)
	if valueOffset < 0 {
		panic("environment sabotage fixture did not match " + name)
	}
	valueIndex := nameIndex + len(nameMarker) + valueOffset
	return manifest[:valueIndex] + `value: "` + newValue + `"` + manifest[valueIndex+len(valueMarker):]
}

func manifestEnvValue(t *testing.T, manifest, name string) string {
	t.Helper()
	nameMarker := "- name: " + name
	nameIndex := strings.Index(manifest, nameMarker)
	if nameIndex < 0 {
		t.Fatalf("manifest does not contain environment variable %s", name)
	}
	valuePattern := regexp.MustCompile(`(?m)^[[:space:]]*value:[[:space:]]*"?([^"\r\n]*)"?[[:space:]]*$`)
	match := valuePattern.FindStringSubmatch(manifest[nameIndex+len(nameMarker):])
	if len(match) != 2 {
		t.Fatalf("manifest environment variable %s has no scalar value", name)
	}
	return strings.TrimSpace(match[1])
}

func withWorkerStatus(manifest string) string {
	needle := `        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
          value: ""
---`
	replacement := `        - name: WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL
          value: ""
status:
  replicas: 1
  readyReplicas: 1
---`
	result := strings.Replace(manifest, needle, replacement, 1)
	if result == manifest {
		panic("worker status fixture did not match")
	}
	return result
}

func replaceSyntheticSuspend(manifest string, suspended bool) string {
	replacement := "  suspend: false"
	if suspended {
		replacement = "  suspend: true"
	}
	result := strings.Replace(manifest, "  suspend: false", replacement, 1)
	if result == manifest && suspended {
		panic("synthetic suspend fixture did not match")
	}
	return result
}

func replaceExtraRepository(manifest, repository string) string {
	result := strings.Replace(manifest, "registry.example/extra", repository, 1)
	if result == manifest {
		panic("extra repository fixture did not match")
	}
	return result
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

func withAPITopLevelRevisionAnnotation(manifest string) string {
	const before = `metadata:
  name: selfservice-api
spec:`
	const after = `metadata:
  name: selfservice-api
  annotations:
    deployment.kubernetes.io/revision: "163"
spec:`
	result := strings.Replace(manifest, before, after, 1)
	if result == manifest {
		panic("API top-level revision annotation fixture did not match the manifest")
	}
	return result
}

func withAPISubstantiveDrift(manifest string) string {
	const before = `        image: ghcr.io/jmal1/selfservice-api-gateway:latest
`
	const after = `        image: ghcr.io/jmal1/selfservice-api-gateway:latest
        env:
        - name: UNSAFE_LIVE_ONLY_DRIFT
          value: "true"
`
	result := strings.Replace(manifest, before, after, 1)
	if result == manifest {
		panic("API substantive-drift fixture did not match the manifest")
	}
	return result
}

func assertManifestImagesPinned(t *testing.T, path, builtDigest, uiDigest, extraRepository string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, repository := range []string{
		"ghcr.io/jmal1/selfservice-api-gateway",
		"ghcr.io/jmal1/selfservice-provision-worker",
		"ghcr.io/jmal1/selfservice-crucible-engine",
		"ghcr.io/jmal1/selfservice-synthetic-api-monitor",
		"ghcr.io/jmal1/selfservice-crucible-runner",
	} {
		if !strings.Contains(text, repository+"@sha256:"+builtDigest) {
			t.Fatalf("manifest does not pin built image %s to %s: %s", repository, builtDigest, text)
		}
	}
	if !strings.Contains(text, "ghcr.io/jmal1/selfservice-ui@sha256:"+uiDigest) {
		t.Fatalf("manifest does not select expected UI digest %s: %s", uiDigest, text)
	}
	if !strings.Contains(text, extraRepository+"@sha256:"+testDigestA) {
		t.Fatalf("manifest does not preserve external image %s: %s", extraRepository, text)
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
