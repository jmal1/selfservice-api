package provisioning

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const testDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const testSourceSHA = "cccccccccccccccccccccccccccccccccccccccc"
const otherSourceSHA = "dddddddddddddddddddddddddddddddddddddddd"
const testUISourceSHA = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

func TestDeployScriptRollbackContainment(t *testing.T) {
	requirePOSIXShell(t)

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
			name:        "live worker status replicas ignored",
			manifest:    baselineManifest(true, "", "false"),
			helmStatus:  "deployed",
			args:        []string{"--verify-rollback-containment"},
			wantSuccess: true,
			wantOutput:  "pins every rendered workload image",
			liveStatus:  true,
		},
		{
			name:         "historical synthetic values use contained live state",
			manifest:     rollbackManifestWithHistoricalSynthetics(false),
			liveResource: rollbackManifestWithHistoricalSynthetics(true),
			helmStatus:   "deployed",
			args:         []string{"--verify-rollback-containment"},
			wantSuccess:  true,
			wantOutput:   "pins every rendered workload image",
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
			name: "content filter enabled rollback target",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"WORKER_CONTENT_FILTER_ENABLED",
				"false",
				"true",
			),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "WORKER_CONTENT_FILTER_ENABLED=false",
		},
		{
			name: "content filter feed in rollback target",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"WORKER_CONTENT_FILTER_CATEGORY_FEED_BASE_URL",
				"",
				"https://student-filter-feed.lab.jmal.io",
			),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "category feed empty",
		},
		{
			name:       "live API monitor suspended",
			manifest:   replaceSyntheticSuspend(baselineManifest(true, "", "false"), true),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "API monitor",
		},
		{
			name: "live API monitor lifecycle enabled",
			manifest: replaceEnvValue(
				baselineManifest(true, "", "false"),
				"SYNTHETIC_LIFECYCLE_ENABLED",
				"false",
				"true",
			),
			helmStatus: "deployed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "SYNTHETIC_LIFECYCLE_ENABLED=false",
		},
		{
			name:       "failed latest revision",
			manifest:   baselineManifest(true, "", "false"),
			helmStatus: "failed",
			args:       []string{"--verify-rollback-containment"},
			wantOutput: "status is failed",
		},
		{
			name:              "wrong immutable rollback revision",
			manifest:          baselineManifest(true, "", "false"),
			helmStatus:        "deployed",
			args:              []string{"--verify-rollback-containment"},
			wantOutput:        "not required immutable rollback revision 160",
			configureRevision: 159,
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
			if test.configureRevision != 0 {
				env.helmRevision = test.configureRevision
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
			if test.name == "standard deploy gates before git" {
				if body, readErr := os.ReadFile(env.gitLog); readErr == nil && len(body) > 0 {
					t.Fatalf("git ran before rollback containment passed: %s", body)
				}
			}
		})
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
			name: "API monitor lifecycle changed",
			transform: func(manifest string) string {
				return replaceEnvValue(
					manifest,
					"SYNTHETIC_LIFECYCLE_ENABLED",
					"false",
					"true",
				)
			},
			wantOutput:  "SYNTHETIC_LIFECYCLE_ENABLED=false",
			wantUpgrade: false,
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
				if serverReadErr != nil || strings.Count(string(serverDryRuns), "-o yaml") != 2 {
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
					baselineIndex := -1
					pauseIndex := -1
					if lockIndex >= 0 {
						if relative := strings.Index(string(output)[lockIndex:], "stored rollback revision"); relative >= 0 {
							baselineIndex = lockIndex + relative
						}
					}
					if baselineIndex >= 0 {
						if relative := strings.Index(string(output)[baselineIndex:], "pausing live worker provisioning claims"); relative >= 0 {
							pauseIndex = baselineIndex + relative
						}
					}
					if lockIndex < 0 || baselineIndex <= lockIndex || pauseIndex <= baselineIndex {
						t.Fatalf("claims pause did not follow lock acquisition and stored-baseline proof:\n%s", output)
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
				if serverReadErr != nil || strings.Count(string(serverDryRuns), "-o yaml") != 2 {
					t.Fatalf("server sabotage did not reach the final under-lock dry-run: err=%v log=%q", serverReadErr, serverDryRuns)
				}
			}
			if test.name == "final server object mutation" {
				serverDryRuns, serverReadErr := os.ReadFile(env.serverDryRunLog)
				if serverReadErr != nil || strings.Count(string(serverDryRuns), "-o yaml") != 2 {
					t.Fatalf("server mutation did not reach the final canonical comparison: err=%v log=%q", serverReadErr, serverDryRuns)
				}
			}
		})
	}
}

func TestDeployScriptAtomicContainmentAndSuccessVerification(t *testing.T) {
	requirePOSIXShell(t)
	live := baselineManifest(true, "", "false")
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
				writeFile(t, env.liveResource, rollbackManifestWithHistoricalSynthetics(true))
				writeFile(t, env.atomicRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.containedRollbackManifest, rollbackManifestWithHistoricalSynthetics(true))
			},
			wantOutput: "revision-160 immutable image baseline via deployed revision 162",
		},
		{
			name: "pending synthetic pod destroy after rollback retains lock",
			configure: func(env *deployScriptEnvironment) {
				env.failAtomicUpgrade = true
				env.pendingSyntheticJobs = 1
				writeFile(t, env.liveManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.liveResource, rollbackManifestWithHistoricalSynthetics(true))
				writeFile(t, env.atomicRollbackManifest, rollbackManifestWithHistoricalSynthetics(false))
				writeFile(t, env.containedRollbackManifest, rollbackManifestWithHistoricalSynthetics(true))
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
	if serverReadErr != nil || strings.Count(string(serverDryRuns), "-o yaml") != 1 {
		t.Fatalf("dry-run did not server-validate exactly once: err=%v log=%q", serverReadErr, serverDryRuns)
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
	gitLog                    string
	lockFile                  string
	serverDryRunLog           string
	serverDryRunMark          string
	finalRollbackMark         string
	extraImageCount           string
	atomicFailedMark          string
	candidateAppliedMark      string
	syntheticContainedMark    string
	atomicRollbackManifest    string
	containedRollbackManifest string
	cronjobVerifyMark         string
	claimsPausedMark          string
	helmStatus                string
	helmRevision              int
	mismatchContainer         string
	packageTag                string
	packageAdditionalTag      string
	packageDigest             string
	runArtifactDigest         string
	imageRevision             string
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

	failFinalServerDryRun          bool
	externalDriftAfterServerDryRun bool
	mutateFinalServerObject        bool
	activeJobs                     int
	activeKubernetesJobs           int
	pendingSyntheticJobs           int
	failAtomicUpgrade              bool
	atomicRollbackMismatch         string
	postUpgradeMismatch            string
	postUpgradeObjectMutation      bool
}

func newDeployScriptEnvironment(t *testing.T, live, candidate string) *deployScriptEnvironment {
	t.Helper()
	root := t.TempDir()
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
		gitLog:                    filepath.Join(root, "git.log"),
		lockFile:                  filepath.Join(root, "helm.lock"),
		serverDryRunLog:           filepath.Join(root, "server-dry-run.log"),
		serverDryRunMark:          filepath.Join(root, "server-dry-run.marker"),
		finalRollbackMark:         filepath.Join(root, "final-rollback.marker"),
		extraImageCount:           filepath.Join(root, "extra-image-count"),
		atomicFailedMark:          filepath.Join(root, "atomic-failed"),
		candidateAppliedMark:      filepath.Join(root, "candidate-applied"),
		syntheticContainedMark:    filepath.Join(root, "synthetic-contained"),
		atomicRollbackManifest:    filepath.Join(root, "atomic-rollback.yaml"),
		containedRollbackManifest: filepath.Join(root, "atomic-rollback-contained.yaml"),
		cronjobVerifyMark:         filepath.Join(root, "cronjob-verified"),
		claimsPausedMark:          filepath.Join(root, "claims-paused.marker"),
		helmStatus:                "deployed",
		helmRevision:              160,
		packageTag:                testSourceSHA,
		packageDigest:             testDigestB,
		runArtifactDigest:         testDigestB,
		imageRevision:             testSourceSHA,
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
	env.writeCommands()
	return env
}

func (e *deployScriptEnvironment) writeCommands() {
	e.t.Helper()
	writeExecutable(e.t, filepath.Join(e.binDir, "helm"), `#!/bin/bash
set -euo pipefail
case "$1 $2" in
  "history selfservice")
    revision=$FAKE_HELM_REVISION
    if [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
      revision=$((FAKE_HELM_REVISION + 2))
    elif [ -f "$FAKE_UPGRADED_MARKER" ]; then
      revision=$((FAKE_HELM_REVISION + 1))
    fi
    printf '%s\n' '- app_version: test' "  revision: $revision" "  status: $FAKE_HELM_STATUS"
    ;;
  "get manifest")
    if [ -f "$FAKE_ATOMIC_FAILED_MARKER" ]; then
      cat "$FAKE_ATOMIC_ROLLBACK_MANIFEST"
    elif [ -f "$FAKE_UPGRADED_MARKER" ]; then
      cat "$FAKE_BASELINE_MANIFEST"
    else
      cat "$FAKE_LIVE_MANIFEST"
    fi
    ;;
  "get values")
    printf '{}\n'
    ;;
  "dependency build"|"dep build")
    ;;
  "template selfservice")
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
  if [ -f "$FAKE_UPGRADED_MARKER" ]; then
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
  local resource_file canonical kind replicas
  resource_file=$(mktemp)
  extract_resource "$manifest" "$resource" > "$resource_file"
  canonical=$(canonical_resource "$resource_file")
  kind=${resource%%/*}
  server_yaml_count=$(grep -c -- '-o yaml' "$FAKE_SERVER_DRY_RUN_LOG" || true)
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
    jq -cn --arg kind "$kind" --arg canonical "$canonical" --argjson replicas "$replicas" \
      '{apiVersion:"fixture/v1",kind:$kind,spec:{fixtureCanonical:$canonical,replicas:$replicas}}'
  else
    jq -cn --arg kind "$kind" --arg canonical "$canonical" \
      '{apiVersion:"fixture/v1",kind:$kind,spec:{fixtureCanonical:$canonical}}'
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
  if [ "$container" = synthetic-api-monitor ] &&
     [ -f "$FAKE_CRONJOB_VERIFY_MARKER" ]; then
    digest=$FAKE_DIGEST_B
  fi
  [ "$container" = "$FAKE_MISMATCH_CONTAINER" ] && digest=$FAKE_DIGEST_B
  if [ -f "$FAKE_CANDIDATE_APPLIED_MARKER" ]; then
    case "$container" in
      warmer|api-gateway|provision-worker|engine|ui)
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
      if [[ "$*" == *"-o yaml"* ]]; then
        printf 'holder: %s\n' "$(cat "$FAKE_LOCK_FILE")"
      else
        cat "$FAKE_LOCK_FILE"
      fi
    elif [[ "$*" == *"app.kubernetes.io/name=postgresql,app.kubernetes.io/instance=selfservice"* ]]; then
      printf 'selfservice-postgresql-0'
    elif [ "$2" = "jobs" ]; then
      if [[ "$*" == *"-o json"* ]]; then
        jq -cn --argjson active "$FAKE_ACTIVE_KUBERNETES_JOBS" \
          '{items:(if $active > 0 then [{metadata:{name:"active-fixture"},status:{active:$active}}] else [] end)}'
      else
        printf 'selfservice-synthetic-api-monitor-1\n'
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
        json_resource "$(current_manifest)" "$2"
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
    ;;
  exec)
    if [[ "$*" == *"pod_name"* ]]; then
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
      printf '35:false\n'
    fi
    ;;
  create)
    if [ "${2:-}" = job ] && [[ "$*" == *"--from=cronjob/"* ]]; then
      : > "$FAKE_CRONJOB_VERIFY_MARKER"
      printf 'job.batch/%s created\n' "$3"
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
      printf '%s' "$holder" > "$FAKE_LOCK_FILE"
    fi
    ;;
  wait)
    ;;
  apply)
    printf '%s\n' "$*" >> "$FAKE_SERVER_DRY_RUN_LOG"
    dry_run_count=$(grep -c -- '-o yaml' "$FAKE_SERVER_DRY_RUN_LOG" || true)
    if [ "$FAKE_FAIL_FINAL_SERVER_DRY_RUN" = true ] &&
       [[ "$*" == *"-o yaml"* ]] &&
       [ "$dry_run_count" -ge 2 ]; then
      echo "sabotaged final server-side dry-run failure" >&2
      exit 95
    fi
    manifest=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "-f" ]; then
        manifest=$2
        break
      fi
      shift
    done
    [ -n "$manifest" ]
    if [[ "$*" == *"-o json"* ]]; then
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
      cat "$manifest"
      : > "$FAKE_SERVER_DRY_RUN_MARKER"
    fi
    ;;
  delete)
    rm -f "$FAKE_LOCK_FILE"
    ;;
  patch)
    : > "$FAKE_SYNTHETIC_CONTAINED_MARKER"
    ;;
  set)
    [ -f "$FAKE_LOCK_FILE" ] || {
      echo "claims pause attempted without the release lock" >&2
      exit 97
    }
    sed '/- name: WORKER_PROVISIONING_CLAIMS_ENABLED/{n;s/value: "true"/value: "false"/;}' \
      "$FAKE_LIVE_RESOURCE_MANIFEST" > "$FAKE_LIVE_RESOURCE_MANIFEST.paused"
    : > "$FAKE_CLAIMS_PAUSED_MARKER"
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
	script := filepath.Join("..", "..", "deploy", "scripts", "deploy.sh")
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(
		os.Environ(),
		"PATH="+e.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG=/dev/null",
		"FAKE_LIVE_MANIFEST="+e.liveManifest,
		"FAKE_LIVE_RESOURCE_MANIFEST="+e.liveResource,
		"FAKE_CANDIDATE_MANIFEST="+e.candidateManifest,
		"FAKE_UPGRADE_HOOK_MANIFEST="+e.upgradeHookManifest,
		"FAKE_BASELINE_MANIFEST="+e.baselineManifest,
		"FAKE_APPLIED_MANIFEST="+e.appliedManifest,
		"FAKE_UPGRADED_MARKER="+e.upgradedMarker,
		"FAKE_UPGRADE_LOG="+e.upgradeLog,
		"FAKE_GIT_LOG="+e.gitLog,
		"FAKE_LOCK_FILE="+e.lockFile,
		"FAKE_SERVER_DRY_RUN_LOG="+e.serverDryRunLog,
		"FAKE_SERVER_DRY_RUN_MARKER="+e.serverDryRunMark,
		"FAKE_FINAL_ROLLBACK_MARKER="+e.finalRollbackMark,
		"FAKE_EXTRA_IMAGE_COUNT="+e.extraImageCount,
		"FAKE_ATOMIC_FAILED_MARKER="+e.atomicFailedMark,
		"FAKE_CANDIDATE_APPLIED_MARKER="+e.candidateAppliedMark,
		"FAKE_SYNTHETIC_CONTAINED_MARKER="+e.syntheticContainedMark,
		"FAKE_ATOMIC_ROLLBACK_MANIFEST="+e.atomicRollbackManifest,
		"FAKE_CONTAINED_ROLLBACK_MANIFEST="+e.containedRollbackManifest,
		"FAKE_CRONJOB_VERIFY_MARKER="+e.cronjobVerifyMark,
		"FAKE_CLAIMS_PAUSED_MARKER="+e.claimsPausedMark,
		"FAKE_HELM_STATUS="+e.helmStatus,
		"FAKE_HELM_REVISION="+strconv.Itoa(e.helmRevision),
		"FAKE_MISMATCH_CONTAINER="+e.mismatchContainer,
		"FAKE_EXTERNAL_DRIFT_AFTER_SERVER_DRY_RUN="+strconv.FormatBool(e.externalDriftAfterServerDryRun),
		"FAKE_FAIL_FINAL_SERVER_DRY_RUN="+strconv.FormatBool(e.failFinalServerDryRun),
		"FAKE_MUTATE_FINAL_SERVER_OBJECT="+strconv.FormatBool(e.mutateFinalServerObject),
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
		"FAKE_PENDING_SYNTHETIC_JOBS="+strconv.Itoa(e.pendingSyntheticJobs),
		"FAKE_FAIL_ATOMIC_UPGRADE="+strconv.FormatBool(e.failAtomicUpgrade),
		"FAKE_ATOMIC_ROLLBACK_MISMATCH="+e.atomicRollbackMismatch,
		"FAKE_POST_UPGRADE_MISMATCH="+e.postUpgradeMismatch,
		"FAKE_POST_UPGRADE_OBJECT_MUTATION="+strconv.FormatBool(e.postUpgradeObjectMutation),
		"FAKE_GIT_BRANCH="+e.gitBranch,
		"FAKE_GIT_REMOTE_SHA="+e.gitRemoteSHA,
		"FAKE_GIT_REMOTE_URL="+e.gitRemoteURL,
		"FAKE_GIT_DIRTY="+strconv.FormatBool(e.gitDirty),
		"FAKE_COMMIT_VERIFIED="+strconv.FormatBool(e.commitVerified),
		"FAKE_MISSING_BUILD="+e.missingBuild,
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
	janitorSuspend := "false"
	runnerSuspend := "false"
	if contained {
		lifecycle = "false"
		janitorSuspend = "true"
		runnerSuspend = "true"
	}
	if !contained {
		manifest = replaceEnvValue(manifest, "SYNTHETIC_LIFECYCLE_ENABLED", "false", lifecycle)
	}
	return manifest + `---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: selfservice-synthetic-janitor
spec:
  suspend: ` + janitorSuspend + `
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
  suspend: ` + runnerSuspend + `
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
