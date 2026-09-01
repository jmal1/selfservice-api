package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// This guard exists because the same defect was shipped independently five
// times during the image-upload/Kali-runner work: a component was built and
// unit-tested in isolation, but never actually ATTACHED in the cmd/ main that
// runs in production.
//
// It is invisible to ordinary tests by construction. Unit tests inject the
// dependency directly (a fake store, a fake metrics recorder), so they pass
// whether or not main() wires the real one. The observable symptoms only show
// up in prod, and they are all quiet ones:
//
//   - Metrics recorders that are never attached leave every Record*/Set* call
//     a nil-receiver no-op, and a recorder that is attached but never flushed
//     never reaches Pushgateway. On a dashboard an ABSENT series and a ZERO
//     series look identical, so "no cleanup failures" is indistinguishable
//     from "the engine never ran".
//   - A store that is never attached makes its whole feature answer 503
//     forever, while every test of that feature stays green.
//
// Matching on source text is crude, but it is the cheapest thing that fails
// loudly when the wiring is dropped, and the alternative (booting a full
// engine/gateway in a test) needs Postgres, NATS and vCenter.
//
// If you intentionally remove one of these, delete its row AND say why.
var requiredWiring = map[string][]struct {
	symbol string
	why    string
}{
	"cmd/api-gateway/main.go": {
		{"WithImageStore", "without it every /admin/images route answers 503 and image upload is dead in prod"},
		{"WithVCenterISOs", "without it the template wizard's ISO picker is permanently empty"},
		{"WithProvisioningAdmission", "without it PROVISIONING_ENABLED is parsed but new pod and VM requests remain admitted during maintenance"},
		{"NewProvisioningAdmissionMetrics", "without it maintenance state and rejection counters are absent from Pushgateway"},
	},
	"internal/api/routes/routes.go": {
		{"r.Use(h.ProvisioningAdmission)", "the admission gate must run after authentication but before AuditRequests can touch the database"},
	},
	"cmd/crucible-engine/main.go": {
		{"NewRunnerMetrics", "without it e.metrics stays nil and every runner metric call is a silent no-op"},
		{"WithRunnerMetrics", "the recorder must be attached to BOTH the engine and the K8s client"},
		{"RunPusher", "recorded counters never reach Pushgateway without the flush loop"},
	},
	"cmd/provision-worker/main.go": {
		{"RunPusher", "image-import metrics never reach Pushgateway without the flush loop"},
		{"ReconcileStuckImageUploads", "without it crucible_image_uploads_stuck is never refreshed, so leaked uploads are never detected; the call site moved from RunStuckUploadReconciler (deleted) to the unified leader-gated select loop"},
		{"TemplateFolder", "without it the vCenter client has no folder for source_type=iso template builds: CreateBlankVM resolves an empty path and every ISO template provision dies with `find folder \"\"` before creating anything -- the clone path hides this because it inherits the SOURCE VM's parent folder, and no ISO build had ever run"},
		{"ReconcileTemplateHealth", "without it no template health checks run, crucible_template_health_* metrics are never pushed, and a silently-rotting template is invisible until students hit it live"},
		{"ReconcileTemplateHealthIfDue", "the 12h ticker is created at process start and reset by every restart; this service deploys several times a day, so WITHOUT the leader-acquisition catch-up the ticker never fires and the feature above is dead on arrival. Note the plain ReconcileTemplateHealth row does not cover this -- it is a substring of this symbol, so it stays green even if the catch-up is deleted"},
		{"ReplaceTemplateHealthSnapshot", "without the leader-acquisition replacement, Pushgateway retains deleted templates and obsolete raw check_type series from the previous worker process whenever the due-check skips a fresh vCenter cycle"},
		{"ReconcileTemplateReplicaBuildMetrics", "without it retained replica build phase and stuck-operation gauges are never refreshed"},
	},
	"cmd/crucible-runner/main.go": {
		{"MaterializeActionLibrary", "without it the engine-generated action library is never written to disk, so every library action (http_get, port_open, ssh_exec, …) fails with exit 127 — the original defect, in which workflows appeared to run, the Job exited 0, and no action could possibly pass"},
	},
	"cmd/synthetic-api-monitor/main.go": {
		{"checks.Elevated", "without it the instructor-role checks are never registered, so the authenticated admin surface (/admin/images, /admin/vcenter/isos, wizard-state) is unmonitored — and a 503 from an unwired dependency looks identical to a healthy deploy, because every other admin check only asserts a student is refused"},
		{"SYNTHETIC_INSTRUCTOR_USER_ID", "the elevated client must be minted from a dedicated instructor row; elevating the primary synthetic user instead would turn five RBAC checks into tautologies that pass while proving nothing"},
		{"RunnerSmoke(", "without it the runner_smoke check is never registered in SYNTHETIC_RUNNER_MODE: engine dispatch through Multus macvlan DHCP to Kali image pull to action execution to callback to results persisted is completely unmonitored -- silent failures look identical to a healthy deploy"},
	},
	// Not a cmd/ main, but the same failure class: buildActionLibrary is a
	// package-level func, so deleting its only call site still compiles and
	// still passes every actionlibrary_test.go case (they call it directly).
	// The feature would just silently stop shipping.
	"internal/engine/engine.go": {
		{"ListRunnerLibraryActions", "without it no library bodies are fetched and the runner is provisioned with an empty library"},
		{"buildActionLibrary", "without it ActionLibrary is never populated on RunnerSpec and library actions revert to exit 127"},
	},
}

func TestCmdMains_WireOptionalDependencies(t *testing.T) {
	root := findRepoRoot(t)

	for relPath, required := range requiredWiring {
		t.Run(relPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}

			src := string(data)

			for _, req := range required {
				if !strings.Contains(src, req.symbol) {
					t.Errorf(
						"%s does not reference %q.\n"+
							"Why this matters: %s.\n"+
							"The feature's own unit tests inject dependencies directly, so they "+
							"will stay green while production silently does nothing.",
						relPath, req.symbol, req.why,
					)
				}
			}
		})
	}
}

func TestProvisioningAdmissionPrecedesDatabaseTouchingAudit(t *testing.T) {
	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "api", "routes", "routes.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	admission := strings.Index(src, "r.Use(h.ProvisioningAdmission)")
	audit := strings.Index(src, "r.Use(middleware.AuditRequests(db))")
	if admission < 0 || audit < 0 || admission > audit {
		t.Fatal("provisioning admission must be registered before database-touching request auditing")
	}
}

func loadWorkflow(t *testing.T, name string) map[string]any {
	t.Helper()
	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}

	var workflow map[string]any
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return workflow
}

func loadCIWorkflow(t *testing.T) map[string]any {
	t.Helper()
	return loadWorkflow(t, "ci.yaml")
}

func mustMap(t *testing.T, v any, context string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want map[string]any", context, v)
	}
	return m
}

func mustSlice(t *testing.T, v any, context string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is %T, want []any", context, v)
	}
	return s
}

func mustString(t *testing.T, v any, context string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s is %T, want string", context, v)
	}
	return s
}

func workflowJob(t *testing.T, workflow map[string]any, name string) map[string]any {
	t.Helper()
	jobs := mustMap(t, workflow["jobs"], "jobs")
	job, ok := jobs[name]
	if !ok {
		t.Fatalf("jobs.%s not found in ci.yaml", name)
	}
	return mustMap(t, job, "jobs."+name)
}

func requireWorkflowJobAbsent(t *testing.T, workflow map[string]any, name string) {
	t.Helper()
	jobs := mustMap(t, workflow["jobs"], "jobs")
	if _, ok := jobs[name]; ok {
		t.Fatalf("jobs.%s unexpectedly present in ci.yaml", name)
	}
}

func stepByName(t *testing.T, job map[string]any, name string) map[string]any {
	t.Helper()
	steps := mustSlice(t, job["steps"], "steps")
	for i, raw := range steps {
		step := mustMap(t, raw, fmt.Sprintf("step %d", i))
		if mustString(t, step["name"], fmt.Sprintf("step %d.name", i)) == name {
			return step
		}
	}
	t.Fatalf("step %q not found", name)
	return nil
}

func needsList(t *testing.T, job map[string]any, context string) []string {
	t.Helper()
	raw, ok := job["needs"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			out = append(out, mustString(t, item, fmt.Sprintf("%s[%d]", context, i)))
		}
		return out
	default:
		t.Fatalf("%s is %T, want string or []any", context, raw)
		return nil
	}
}

func TestCIWorkflowConcurrencyUsesStablePROrRunID(t *testing.T) {
	workflow := loadCIWorkflow(t)
	concurrency := mustMap(t, workflow["concurrency"], "concurrency")

	if got := mustString(t, concurrency["group"], "concurrency.group"); got != "${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}" {
		t.Fatalf("concurrency.group = %q, want PR-number/run_id fallback and no github.ref", got)
	}
	if got := mustString(t, concurrency["cancel-in-progress"], "concurrency.cancel-in-progress"); got != "${{ github.event_name == 'pull_request' }}" {
		t.Fatalf("concurrency.cancel-in-progress = %q, want PR-only cancellation", got)
	}
}

// Pull request branch filters match the target branch, so dependent PRs need
// an unrestricted trigger while image-publishing pushes stay main-only.
func TestCIWorkflowTriggersDependentPRsButPublishesOnlyFromMain(t *testing.T) {
	workflow := loadCIWorkflow(t)
	triggers := mustMap(t, workflow["on"], "on")

	pullRequest := mustMap(t, triggers["pull_request"], "on.pull_request")
	if branches, restricted := pullRequest["branches"]; restricted {
		t.Fatalf("on.pull_request.branches = %v, want no target-branch restriction so dependent PRs run", branches)
	}
	if pathsIgnore, restricted := pullRequest["paths-ignore"]; restricted {
		t.Fatalf("on.pull_request.paths-ignore = %v, want every PR to instantiate the required CI check", pathsIgnore)
	}

	push := mustMap(t, triggers["push"], "on.push")
	branches := mustSlice(t, push["branches"], "on.push.branches")
	if len(branches) != 1 || mustString(t, branches[0], "on.push.branches[0]") != "main" {
		t.Fatalf("on.push.branches = %v, want exactly [main] so feature branches cannot publish images", branches)
	}
}

func TestHelmLintWorkflowRunsOnEveryMainPushAndFiltersPRs(t *testing.T) {
	workflow := loadWorkflow(t, "helm-lint.yaml")
	triggers := mustMap(t, workflow["on"], "on")

	push := mustMap(t, triggers["push"], "on.push")
	branches := mustSlice(t, push["branches"], "on.push.branches")
	if len(branches) != 1 || mustString(t, branches[0], "on.push.branches[0]") != "main" {
		t.Fatalf("on.push.branches = %v, want exactly [main]", branches)
	}
	if paths, restricted := push["paths"]; restricted {
		t.Fatalf("on.push.paths = %v, want every main push to produce Helm Lint evidence", paths)
	}
	if pathsIgnore, restricted := push["paths-ignore"]; restricted {
		t.Fatalf("on.push.paths-ignore = %v, want every main push to produce Helm Lint evidence", pathsIgnore)
	}

	pullRequest := mustMap(t, triggers["pull_request"], "on.pull_request")
	wantPRPaths := []any{"deploy/helm/**", ".github/workflows/helm-lint.yaml"}
	if got := mustSlice(t, pullRequest["paths"], "on.pull_request.paths"); !reflect.DeepEqual(got, wantPRPaths) {
		t.Fatalf("on.pull_request.paths = %v, want %v", got, wantPRPaths)
	}
}

func TestCIWorkflowRequiredPRCheckAggregatesConditionalJobs(t *testing.T) {
	workflow := loadCIWorkflow(t)
	required := workflowJob(t, workflow, "ci-required")

	if got := mustString(t, required["if"], "jobs.ci-required.if"); got != "always() && github.event_name == 'pull_request'" {
		t.Fatalf("jobs.ci-required.if = %q, want an always-evaluated PR-only gate", got)
	}
	if got := needsList(t, required, "jobs.ci-required.needs"); !reflect.DeepEqual(got, []string{"changes", "build-pr", "test"}) {
		t.Fatalf("jobs.ci-required.needs = %v, want [changes build-pr test]", got)
	}
	if value, ok := required["continue-on-error"]; ok {
		t.Fatalf("jobs.ci-required.continue-on-error = %v, want failures to remain blocking", value)
	}

	steps := mustSlice(t, required["steps"], "jobs.ci-required.steps")
	if len(steps) != 1 {
		t.Fatalf("jobs.ci-required.steps has %d entries, want one aggregation step", len(steps))
	}
	step := mustMap(t, steps[0], "jobs.ci-required.steps[0]")
	if got := mustString(t, step["name"], "jobs.ci-required.steps[0].name"); got != "Require successful PR CI" {
		t.Fatalf("jobs.ci-required.steps[0].name = %q, want %q", got, "Require successful PR CI")
	}
	if value, ok := step["continue-on-error"]; ok {
		t.Fatalf("jobs.ci-required.steps[0].continue-on-error = %v, want failures to remain blocking", value)
	}
	wantEnv := map[string]any{
		"CHANGES_RESULT":  "${{ needs.changes.result }}",
		"BUILD_PR_RESULT": "${{ needs.build-pr.result }}",
		"TEST_RESULT":     "${{ needs.test.result }}",
	}
	if got := mustMap(t, step["env"], "jobs.ci-required.steps[0].env"); !reflect.DeepEqual(got, wantEnv) {
		t.Fatalf("jobs.ci-required.steps[0].env = %#v, want %#v", got, wantEnv)
	}
	const wantRun = `set -euo pipefail
test "$CHANGES_RESULT" = "success"
for result in "$BUILD_PR_RESULT" "$TEST_RESULT"; do
  case "$result" in
    success|skipped) ;;
    *) echo "required PR job failed or was cancelled: $result" >&2; exit 1 ;;
  esac
done
`
	if got := mustString(t, step["run"], "jobs.ci-required.steps[0].run"); got != wantRun {
		t.Fatalf("jobs.ci-required.steps[0].run = %q, want strict success/skipped aggregation", got)
	}
}

func TestCIWorkflowBuildJobsSplitPRAndPush(t *testing.T) {
	workflow := loadCIWorkflow(t)
	buildPR := workflowJob(t, workflow, "build-pr")
	build := workflowJob(t, workflow, "build")
	requireWorkflowJobAbsent(t, workflow, "build-push")

	if got := needsList(t, buildPR, "jobs.build-pr.needs"); !reflect.DeepEqual(got, []string{"changes"}) {
		t.Fatalf("build-pr needs = %v, want [changes]", got)
	}
	if got := needsList(t, build, "jobs.build.needs"); !reflect.DeepEqual(got, []string{"changes", "test"}) {
		t.Fatalf("build needs = %v, want [changes test]", got)
	}

	if got := mustString(t, buildPR["if"], "jobs.build-pr.if"); !strings.Contains(got, "github.event_name == 'pull_request'") ||
		!strings.Contains(got, "needs.changes.outputs.components != '[]'") ||
		strings.Contains(got, "needs.test.result") ||
		strings.Contains(got, "github.ref") {
		t.Fatalf("build-pr if = %q, want PR-only matrix build without test dependency", got)
	}
	if got := mustString(t, build["if"], "jobs.build.if"); !strings.Contains(got, "github.event_name == 'push'") ||
		!strings.HasPrefix(strings.TrimSpace(got), "always() &&") ||
		!strings.Contains(got, "needs.changes.outputs.components != '[]'") ||
		!strings.Contains(got, "needs.test.result == 'success' || needs.test.result == 'skipped'") ||
		strings.Contains(got, "github.ref") {
		t.Fatalf("build if = %q, want push-only build gated by test success/skipped", got)
	}

	if !reflect.DeepEqual(buildPR["strategy"], build["strategy"]) {
		t.Fatalf("build-pr and build strategy differ:\nPR:   %#v\nPush: %#v", buildPR["strategy"], build["strategy"])
	}
	if !reflect.DeepEqual(buildPR["steps"], build["steps"]) {
		t.Fatalf("build-pr and build steps differ")
	}

	steps := mustSlice(t, buildPR["steps"], "jobs.build-pr.steps")
	login := stepByName(t, buildPR, "Log in to GHCR")
	if got := mustString(t, login["if"], "Log in to GHCR.if"); got != "github.event_name == 'push'" {
		t.Fatalf("Log in to GHCR if = %q, want push-only login", got)
	}
	buildStep := stepByName(t, buildPR, "Build and push")
	with := mustMap(t, buildStep["with"], "Build and push.with")
	if got := mustString(t, with["push"], "Build and push.with.push"); got != "${{ github.event_name == 'push' }}" {
		t.Fatalf("Build and push push = %q, want push-only build", got)
	}
	record := stepByName(t, buildPR, "Record immutable deployment digest")
	if got := mustString(t, record["if"], "Record immutable deployment digest.if"); got != "github.event_name == 'push'" {
		t.Fatalf("Record immutable deployment digest if = %q, want push-only digest capture", got)
	}
	upload := stepByName(t, buildPR, "Upload immutable deployment digest")
	if got := mustString(t, upload["if"], "Upload immutable deployment digest.if"); got != "github.event_name == 'push'" {
		t.Fatalf("Upload immutable deployment digest if = %q, want push-only artifact upload", got)
	}

	if len(steps) == 0 {
		t.Fatal("build-pr has no steps")
	}
}

func TestCIWorkflowPushBuildRemainsEligibleWhenTestsSkip(t *testing.T) {
	workflow := loadCIWorkflow(t)
	build := workflowJob(t, workflow, "build")

	got := mustString(t, build["if"], "jobs.build.if")
	if !strings.HasPrefix(strings.TrimSpace(got), "always() &&") {
		t.Fatalf("build if = %q, want always() fallback so skipped tests do not block push builds", got)
	}
	if !strings.Contains(got, "needs.changes.outputs.components != '[]'") {
		t.Fatalf("build if = %q, want nonempty matrix gate", got)
	}
	if !strings.Contains(got, "needs.test.result == 'success' || needs.test.result == 'skipped'") {
		t.Fatalf("build if = %q, want skipped tests accepted but failures rejected", got)
	}
}

func TestCIWorkflowBuildJobMatchesDeployProof(t *testing.T) {
	workflow := loadCIWorkflow(t)
	workflowJob(t, workflow, "build")
	requireWorkflowJobAbsent(t, workflow, "build-push")

	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "deploy", "scripts", "deploy.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `--arg job "build ($component)"`) {
		t.Fatal(`deploy.sh no longer proves successful image builds with job names "build ($component)"`)
	}
	if !strings.Contains(string(body), `--arg release "Build release images ($component)"`) {
		t.Fatal(`deploy.sh no longer accepts CI matrix job names "Build release images ($component)"`)
	}
}

func TestCIWorkflowTestJobStillRunsFullCoverage(t *testing.T) {
	workflow := loadCIWorkflow(t)
	testJob := workflowJob(t, workflow, "test")

	if got := needsList(t, testJob, "jobs.test.needs"); !reflect.DeepEqual(got, []string{"changes"}) {
		t.Fatalf("test needs = %v, want [changes]", got)
	}
	if got := mustString(t, testJob["if"], "jobs.test.if"); !strings.Contains(got, "needs.changes.outputs.run_tests == 'true'") {
		t.Fatalf("test if = %q, want run_tests gating", got)
	}

	steps := mustSlice(t, testJob["steps"], "jobs.test.steps")
	var names []string
	for i, raw := range steps {
		step := mustMap(t, raw, fmt.Sprintf("jobs.test.steps[%d]", i))
		name := mustString(t, step["name"], fmt.Sprintf("jobs.test.steps[%d].name", i))
		names = append(names, name)
	}
	wantNames := []string{"Checkout", "Set up Go", "Build", "Vet", "Verify wiki bundle", "Fast fail: short Go suite", "Test"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("test step names = %v, want %v", names, wantNames)
	}

	expectRuns := map[string]string{
		"Build":                    "go build ./...",
		"Vet":                      "go vet ./...",
		"Verify wiki bundle":       "make verify-wiki",
		"Fast fail: short Go suite": "go test ./... -short -count=1",
		"Test":                     "go test ./... -v -race",
	}
	for _, raw := range steps {
		step := mustMap(t, raw, "jobs.test.steps")
		name := mustString(t, step["name"], "jobs.test.steps.name")
		if run, ok := step["run"]; ok {
			if want, ok := expectRuns[name]; ok {
				if got := mustString(t, run, "jobs.test.steps."+name+".run"); got != want {
					t.Fatalf("%s run = %q, want %q", name, got, want)
				}
			}
		}
	}
}

// TestCreatePod_ActiveTransitionIsGuarded pins the compare-and-swap on the create
// job's final status write.
//
// Pod create and pod destroy are independent worker jobs and can overlap. vCenter
// routinely takes minutes to report a VM's IP, and a destroy issued during that wait
// deletes the VMs and marks the pod destroyed. With an unconditional UPDATE the slow
// create won purely by finishing last and re-marked the pod "active" -- with no VM
// behind it.
//
// Observed in production on 2026-08-03: pod d0a994c6 was destroyed at 13:13:30, and
// its still-running create job set it back to "active" at 13:15:06.
//
// The resulting ghost pod is silent by construction. The API, the UI and quota
// accounting all trust pods.status, so the pod reads as healthy, consumes its owner's
// quota indefinitely, and no reconciler reaps it -- every component believes the
// column. That makes this strictly worse than a create that fails outright.
//
// This is a source-text guard for the same reason as the wiring table above: there is
// no Postgres test harness in this repo, so the CAS itself cannot be exercised in a
// unit test. Reverting the call to the unconditional variant would compile, pass every
// other test, and silently restore the bug.
func TestCreatePod_ActiveTransitionIsGuarded(t *testing.T) {
	root := findRepoRoot(t)
	relPath := "internal/provisioner/create.go"

	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}

	src := string(data)

	const guarded = `UpdatePodStatusFrom(ctx, pod.ID, []string{"provisioning"}, "active", "")`
	if !strings.Contains(src, guarded) {
		t.Errorf(
			"%s does not perform its active transition via %s.\n"+
				"Without the compare-and-swap, a create that finishes after a concurrent "+
				"destroy resurrects the pod as active with no VM behind it.",
			relPath, guarded,
		)
	}

	// "UpdatePodStatusFrom(" does not contain "UpdatePodStatus(", so this matches only
	// the unconditional variant.
	const unguarded = `UpdatePodStatus(ctx, pod.ID, "active"`
	if strings.Contains(src, unguarded) {
		t.Errorf(
			"%s writes the active status unconditionally via %s.\n"+
				"That is the ghost-pod bug: destroy is the terminal intent and must win, "+
				"so the transition to active must be conditional on the pod still being "+
				"in \"provisioning\".",
			relPath, unguarded,
		)
	}
}

func TestProvisioningJobEntryTransitionsAreGuarded(t *testing.T) {
	root := findRepoRoot(t)
	files := map[string][]string{
		"internal/provisioner/create.go": {
			"UpdatePodStatusFrom(",
			"UpdatePodVMStatusFrom(",
			"AdoptPodVMClone(",
			"models.PodStatusPending",
			"models.PodStatusProvisioning",
			"stale pod create job skipped",
			"stopPodCreateIfStale",
			"BeginPodCreateCleanup(",
			"rb.Rollback(cleanupCtx)",
			"failPodCreateForStaleVM",
		},
		"internal/provisioner/vm_ops.go": {
			"UpdatePodVMStatusFrom(",
			"AdoptPodVMClone(",
			"stale vm_add job skipped",
			"VCenterVMID string",
			"CleanupOnly bool",
			"stageVMCloneCleanup",
		},
	}

	for relPath, fragments := range files {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(body), fragment) {
				t.Errorf("%s is missing %q; a stale provisioning job could overwrite terminal cleanup state", relPath, fragment)
			}
		}

	}
}

func TestCleanupOnlyPodCreateCannotReachForwardProvisioning(t *testing.T) {
	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "provisioner", "create.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	createStart := strings.Index(src, "func (p *Provisioner) CreatePod(")
	if createStart < 0 {
		t.Fatal("CreatePod function not found")
	}
	cleanupBranch := strings.Index(src[createStart:], "if payload.CleanupOnly {")
	forwardStart := strings.Index(src[createStart:], "// Get pod from DB")
	if cleanupBranch < 0 || forwardStart < 0 || cleanupBranch > forwardStart {
		t.Fatal("cleanup-only dispatch must return before the forward pod-create path")
	}
	branch := src[createStart+cleanupBranch : createStart+forwardStart]
	if !strings.Contains(branch, "return p.runPodCreateCleanup(ctx, job, payload, rb)") {
		t.Fatal("cleanup-only dispatch must return directly into compensation")
	}

	cleanupStart := strings.Index(src, "func (p *Provisioner) runPodCreateCleanup(")
	if cleanupStart < 0 {
		t.Fatal("runPodCreateCleanup function not found")
	}
	cleanupEnd := strings.Index(src[cleanupStart+1:], "\nfunc ")
	if cleanupEnd < 0 {
		t.Fatal("could not isolate runPodCreateCleanup")
	}
	cleanupBody := src[cleanupStart : cleanupStart+1+cleanupEnd]
	for _, forbidden := range []string{
		"CreateVLAN(",
		"CreateDHCPSubnet(",
		"AddDHCPInterface(",
		"CreatePortGroupOnAllHosts(",
		"CloneVM(",
		"PowerOnVM(",
		"CreateVMSnapshot(",
	} {
		if strings.Contains(cleanupBody, forbidden) {
			t.Errorf("cleanup-only execution contains forward provisioning call %q", forbidden)
		}
	}
	if got := strings.Count(src, "rb.Rollback("); got != 1 {
		t.Fatalf("pod-create rollback has %d call sites, want one guarded compensation path", got)
	}
	commonCleanupStart := strings.Index(src, "func (p *Provisioner) cleanupPodCreateResources(")
	if commonCleanupStart < 0 {
		t.Fatal("cleanupPodCreateResources function not found")
	}
	commonCleanupEnd := strings.Index(src[commonCleanupStart+1:], "\nfunc ")
	if commonCleanupEnd < 0 {
		t.Fatal("could not isolate cleanupPodCreateResources")
	}
	commonCleanupBody := src[commonCleanupStart : commonCleanupStart+1+commonCleanupEnd]
	marker := strings.Index(commonCleanupBody, "BeginPodCreateCleanup(")
	rollback := strings.Index(commonCleanupBody, "rb.Rollback(cleanupCtx)")
	if marker < 0 || rollback < 0 || marker > rollback {
		t.Fatal("cleanup-only state must be committed before rollback starts")
	}
	cleanupSucceeded := strings.Index(commonCleanupBody, "if len(cleanupErrs) > 0 {")
	releaseCapacity := strings.Index(commonCleanupBody, "p.releaseVMPlacementCapacity(")
	markVMTerminal := strings.Index(commonCleanupBody, "p.db.UpdatePodVMStatusFrom(")
	if cleanupSucceeded < rollback ||
		releaseCapacity < cleanupSucceeded ||
		markVMTerminal < releaseCapacity {
		t.Fatal("pod-create compensation must finish exact cleanup and release capacity before terminal VM states")
	}

	failCleanupStart := strings.Index(src, "func (p *Provisioner) failPodCreateWithCleanup(")
	if failCleanupStart < 0 {
		t.Fatal("failPodCreateWithCleanup function not found")
	}
	failCleanupEnd := strings.Index(src[failCleanupStart+1:], "\nfunc ")
	if failCleanupEnd < 0 {
		t.Fatal("could not isolate failPodCreateWithCleanup")
	}
	failCleanupBody := src[failCleanupStart : failCleanupStart+1+failCleanupEnd]
	if !strings.Contains(failCleanupBody, "\n\t\ttrue,\n") {
		t.Fatal("forward pod-create failure must atomically relinquish provisioning before cleanup")
	}
	if !strings.Contains(cleanupBody, "podCreateCleanupOwnsStagedClone(payload)") {
		t.Fatal("cleanup-only retry may transition provisioning only with an exact staged clone target")
	}

	queryBody, err := os.ReadFile(filepath.Join(root, "internal", "database", "queries.go"))
	if err != nil {
		t.Fatal(err)
	}
	querySrc := string(queryBody)
	beginStart := strings.Index(querySrc, "func (q *Queries) BeginPodCreateCleanup(")
	retryStart := strings.Index(querySrc, "const retryJobSQL")
	if beginStart < 0 || retryStart < 0 || beginStart > retryStart {
		t.Fatal("BeginPodCreateCleanup transaction not found")
	}
	beginBody := querySrc[beginStart:retryStart]
	for _, required := range []string{
		"SELECT status FROM pods WHERE id = $1 FOR UPDATE",
		"status == models.PodStatusProvisioning && allowProvisioningTransition",
		"podCreateCleanupStatusSafe(status)",
		"markPodCreateCleanupOnlySQL",
		"tx.Commit(ctx)",
	} {
		if !strings.Contains(beginBody, required) {
			t.Errorf("atomic cleanup transition is missing %q", required)
		}
	}
}

func TestVMCloneCompensationUsesOnlyDurableExactTargets(t *testing.T) {
	root := findRepoRoot(t)
	read := func(relPath string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	createSrc := read("internal/provisioner/create.go")
	vmOpsSrc := read("internal/provisioner/vm_ops.go")
	querySrc := read("internal/database/queries.go")
	cloneOperationSrc := read("internal/provisioner/clone_operation.go")
	cloneDBSrc := read("internal/database/clone_operations.go")

	if strings.Contains(vmOpsSrc, "ResolveVMByName(") {
		t.Fatal("stale-clone compensation must never resolve a mutable VM name")
	}
	for relPath, src := range map[string]string{
		"internal/provisioner/create.go": createSrc,
		"internal/provisioner/vm_ops.go": vmOpsSrc,
	} {
		clone := strings.Index(src, "executeDurableVMClone(")
		if clone < 0 {
			t.Errorf("%s has no durable clone operation call", relPath)
			continue
		}
		adopt := strings.Index(src[clone:], "p.db.AdoptPodVMClone(")
		disarm := strings.Index(src[clone:], "p.db.DisarmVMCloneCleanup(")
		if adopt < 0 || disarm < 0 || adopt > disarm {
			t.Errorf("%s must adopt then disarm the durable clone", relPath)
		}
		if relPath == "internal/provisioner/create.go" {
			record := strings.Index(src[clone:], "rb.Record(")
			if record < 0 || record > adopt {
				t.Errorf("%s must persist rollback before adopting and disarming the clone", relPath)
			}
		}
	}
	executeStart := strings.Index(cloneOperationSrc, "func executeDurableVMClone(")
	if executeStart < 0 {
		t.Fatal("durable clone execution function is missing")
	}
	executeSrc := cloneOperationSrc[executeStart:]
	loadOperation := strings.Index(executeSrc, "store.GetVMCloneOperation(")
	freshOperation := strings.Index(executeSrc, "if op == nil {")
	resolvePlacement := strings.Index(executeSrc, "client.ResolveClonePlacement(")
	prepare := strings.Index(executeSrc, "store.PrepareVMCloneOperation(")
	submit := strings.Index(executeSrc, "client.StartCloneVMOperation(")
	persistTask := strings.Index(executeSrc, "store.PersistVMCloneTask(")
	waitTask := strings.Index(executeSrc, "client.WaitCloneVMTask(")
	stage := strings.Index(executeSrc, "store.StageVMCloneCleanup(")
	configure := strings.Index(executeSrc, "client.ConfigureClonedVM(")
	if loadOperation < 0 || freshOperation < loadOperation || resolvePlacement < freshOperation ||
		prepare < resolvePlacement || submit < prepare || persistTask < submit || waitTask < persistTask ||
		stage < waitTask || configure < stage {
		t.Fatal("durable clone order must be load existing, resolve/prepare only if new, submit, persist task, wait, stage exact MoRef, configure")
	}
	if strings.Contains(createSrc, "cleanupStaleVMClone(") ||
		strings.Contains(vmOpsSrc, "cleanupStaleVMClone(") {
		t.Fatal("legacy stale-clone cleanup bypasses the durable exact-target lifecycle")
	}
	if got := strings.Count(createSrc, "failPodCreateForStaleVM("); got != 7 {
		t.Fatalf("pod_create stale-clone compensation sites = %d, want 6 calls plus helper", got)
	}
	if got := strings.Count(vmOpsSrc, "failVMAddWithCleanup("); got != 15 {
		t.Fatalf("vm_add stale-clone compensation sites = %d, want 14 calls plus helper", got)
	}

	addStart := strings.Index(vmOpsSrc, "func (p *Provisioner) AddVM(")
	if addStart < 0 {
		t.Fatal("AddVM function not found")
	}
	cleanupBranch := strings.Index(vmOpsSrc[addStart:], "if payload.CleanupOnly {")
	forwardStart := strings.Index(vmOpsSrc[addStart:], "pod, err := p.db.GetPodByID")
	if cleanupBranch < 0 || forwardStart < 0 || cleanupBranch > forwardStart {
		t.Fatal("vm_add cleanup-only dispatch must precede all forward provisioning")
	}
	runCleanupStart := strings.Index(vmOpsSrc, "func (p *Provisioner) runVMAddCleanup(")
	if runCleanupStart < 0 {
		t.Fatal("runVMAddCleanup function not found")
	}
	runCleanupEnd := strings.Index(vmOpsSrc[runCleanupStart+1:], "\nfunc ")
	if runCleanupEnd < 0 {
		t.Fatal("could not isolate runVMAddCleanup")
	}
	runCleanupBody := vmOpsSrc[runCleanupStart : runCleanupStart+1+runCleanupEnd]
	for _, forbidden := range []string{
		"CloneVM(",
		"executeDurableVMClone(",
		"StartCloneVMOperation(",
		"ConfigureClonedVM(",
		"PowerOnVM(",
		"CreateVMSnapshot(",
		"ResolveVMByName(",
	} {
		if strings.Contains(runCleanupBody, forbidden) {
			t.Errorf("vm_add cleanup-only execution contains forward/unsafe call %q", forbidden)
		}
	}
	if !strings.Contains(runCleanupBody, "p.finalizeVMAddCompensation(") {
		t.Fatal("vm_add cleanup does not finalize capacity and VM state after exact cleanup")
	}
	finalizeStart := strings.Index(vmOpsSrc, "func (p *Provisioner) finalizeVMAddCompensation(")
	if finalizeStart < 0 {
		t.Fatal("vm_add compensation finalizer not found")
	}
	finalizeEnd := strings.Index(vmOpsSrc[finalizeStart+1:], "\nfunc ")
	if finalizeEnd < 0 {
		t.Fatal("could not isolate vm_add compensation finalizer")
	}
	finalizeBody := vmOpsSrc[finalizeStart : finalizeStart+1+finalizeEnd]
	releaseCapacity := strings.Index(finalizeBody, "p.releaseVMPlacementCapacity(")
	markError := strings.Index(finalizeBody, "p.db.UpdatePodVMStatusFrom(")
	if releaseCapacity < 0 || markError < releaseCapacity ||
		!strings.Contains(finalizeBody, "models.VMStatusError") {
		t.Fatal("vm_add compensation must release capacity before exposing terminal VM state")
	}

	for _, required := range []string{
		"func (q *Queries) GetVMCloneOperation(",
		"func (q *Queries) StageVMCloneCleanup(",
		"'{cleanup_target}'",
		"func (q *Queries) AdoptPodVMClone(",
		"(vcenter_vm_id IS NULL OR vcenter_vm_id = $1)",
		"func (q *Queries) DisarmVMCloneCleanup(",
		"payload - 'cleanup_target' - 'cleanup_only'",
		"func (q *Queries) CompleteVMCloneCleanup(",
		"vcenter_vm_id = $2\n\t\t    OR (vcenter_vm_id IS NULL AND status IN ('pending', 'cloning', 'configuring'))",
		"func (q *Queries) MarkJobCompensationCompleted(",
		"if !updated && !alreadyScheduled {\n\t\treturn fmt.Errorf(\"%w: job %s is not owned by %s for retry scheduling\"",
		"return fmt.Errorf(\"%w: job %s is not owned by %s for status %s\"",
	} {
		if !strings.Contains(querySrc+cloneDBSrc, required) {
			t.Errorf("database compensation lifecycle is missing %q", required)
		}
	}
	if strings.Contains(querySrc, "func (q *Queries) SetPodVMVCenterReference(") {
		t.Fatal("legacy clone-reference persistence can bypass the staged cleanup transaction")
	}
	for _, required := range []string{
		"return true, newPodCreateCompensatedError(stage)",
		"return newPodCreateCompensatedError(\"initial pod lookup\")",
		"return &compensationRetryError{\n\t\t\terr: fmt.Errorf(\"prepare pod_create cleanup",
		"p.finalizeVMAddCompensation(ctx, job, podVMID, true)",
		"return p.completeVMAddWithoutClone(",
		"return newVMAddCompensatedError(reason)",
	} {
		if !strings.Contains(createSrc+vmOpsSrc, required) {
			t.Errorf("compensation terminal/retry semantics are missing %q", required)
		}
	}
}

func TestCreatePodDoesNotUseUnguardedVMStatusWrites(t *testing.T) {
	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "provisioner", "create.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "UpdatePodVMStatus(ctx") {
		t.Fatal("pod create contains an unconditional VM status write that can resurrect deleted state")
	}
}

func TestJobRecoveryUsesOwnedHeartbeatLeases(t *testing.T) {
	root := findRepoRoot(t)
	read := func(relPath string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	mainSrc := read("cmd/provision-worker/main.go")
	querySrc := read("internal/database/queries.go")
	createSrc := read("internal/provisioner/create.go")
	cloneOperationSrc := read("internal/provisioner/clone_operation.go")
	cloneDBSrc := read("internal/database/clone_operations.go")
	vcenterInventorySrc := read("internal/vcenter/inventory_lookup.go")
	rollbackSrc := read("internal/rollback/engine.go")
	templateJobsSrc := read("internal/provisioner/template_jobs.go")

	for _, required := range []string{
		`workerID := workerHost + "-" + uuid.NewString()`,
		"claimID := newJobClaimID(workerID)",
		`return workerID + ":" + uuid.NewString()`,
		"queries.RecoverStaleJobs(ctx, provisioner.JobLeaseDuration)",
		"time.NewTicker(provisioner.JobLeaseRecoveryInterval)",
		"shutdownDeadline := time.Now().Add(2 * time.Minute)",
		"jobRuns.StopAndWait(time.Until(shutdownDeadline))",
		"l1ValidationScheduler.WaitTimeout(time.Until(shutdownDeadline))",
		"closeBeforeDeadline(pool, time.Until(shutdownDeadline))",
	} {
		if !strings.Contains(mainSrc, required) {
			t.Errorf("worker lease wiring is missing %q", required)
		}
	}
	for _, required := range []string{
		"AND claimed_by = $4",
		"$2 = 'completed'",
		"COALESCE(payload->>'cleanup_only', 'false') = 'true'",
		"type <> 'template_replica_build'",
		"AND NOT (",
		"func (q *Queries) RenewJobLease(",
		"AND claimed_by = $5",
		"claimed_at < now() - ($1 * interval '1 second')",
		"claimed_by = $3",
		"claimed_by = $4 AND status IN ('claimed', 'in_progress')",
		`fields["destroyed_cleanup_targets"]`,
		"'pod_id', $2::jsonb->>'pod_id'",
		"'pod_vm_id', $2::jsonb->>'pod_vm_id'",
		"'vcenter_vm_id', $2::jsonb->>'vcenter_vm_id'",
		"status = 'pending'",
		"ErrVMCloneAlreadyDestroyed",
		"func (q *Queries) UpdateJobRollbackSteps(",
		"SET rollback_steps = $2, claimed_at = now()",
		"cannot update rollback steps",
		"func (q *Queries) PrepareVMCloneOperation(",
		"type IN ('pod_create', 'vm_add', 'template_verify', 'template_revalidate')",
		"func (q *Queries) ArmVMCloneOperation(",
		"func (q *Queries) AbandonUnsubmittedVMCloneOperation(",
		"SELECT NOT (payload ? 'clone_operation')",
		"'{clone_operation,phase}'",
		"'{cleanup_only}'",
		"func (q *Queries) PersistVMCloneTask(",
		"func (q *Queries) FinishStandaloneVMCloneCleanup(",
		"func (q *Queries) AdoptJobRollbackStep(",
		"rollback_steps = $2",
		"status = 'pending'",
	} {
		if !strings.Contains(querySrc+cloneDBSrc, required) {
			t.Errorf("database lease ownership is missing %q", required)
		}
	}
	for _, required := range []string{
		"context.WithCancelCause(ctx)",
		"maintainJobLease(",
		"persistCleanupRetry(ctx, db, job, workerID, cleanupTarget)",
	} {
		if !strings.Contains(createSrc, required) {
			t.Errorf("job execution lease is missing %q", required)
		}
	}
	if strings.Contains(mainSrc, "RecoverStaleJobs(ctx)") {
		t.Fatal("worker still contains broad owner-unaware stale-job recovery")
	}
	if strings.Contains(mainSrc, "defer pool.Close()") {
		t.Fatal("database pool close can bypass the bounded worker shutdown deadline")
	}

	vcenterSrc := read("internal/vcenter/client.go")
	for _, required := range []string{
		"context.WithTimeout(ctx, cloneTaskOperationalTimeout)",
		"context.WithTimeout(overallCtx, cloneTaskWaitSlice)",
		"task.WaitForResult(waitCtx, nil)",
		"return task.Reference().Value, nil",
		"task = object.NewTask(c.client.Client, ref)",
		"already exists without immutable ownership proof",
	} {
		if !strings.Contains(vcenterSrc, required) {
			t.Errorf("clone task handoff is missing %q", required)
		}
	}
	if strings.Contains(vcenterSrc, `c.withRetry(ctx, "clone VM"`) {
		t.Fatal("CloneVM can retry the whole clone after a remote task was issued")
	}
	if strings.Contains(vcenterSrc, "func (c *Client) CloneVM(") ||
		strings.Contains(vcenterSrc, "func (c *Client) StartCloneVM(") {
		t.Fatal("vCenter still exposes a clone path that bypasses durable operation arming")
	}
	if strings.Contains(templateJobsSrc, "p.vc.CloneVM(") ||
		!strings.Contains(templateJobsSrc, "executeDurableVMClone(") ||
		!strings.Contains(templateJobsSrc, "FinishStandaloneVMCloneCleanup(") {
		t.Fatal("template smoke clones bypass durable operation identity or exact cleanup")
	}
	if !strings.Contains(templateJobsSrc, `smokeName := fmt.Sprintf("smoke-%s-%s"`) {
		t.Fatal("smoke clone target name is not stable across retries")
	}
	verifyStart := strings.Index(templateJobsSrc, "func (p *Provisioner) VerifyTemplate(")
	if verifyStart < 0 {
		t.Fatal("VerifyTemplate not found")
	}
	verifyBody := templateJobsSrc[verifyStart:]
	cleanupGate := strings.Index(verifyBody, "if jobPayloadCleanupOnly(job)")
	templateLookup := strings.Index(verifyBody, "p.db.GetTemplateByID(")
	stateGate := strings.Index(verifyBody, "if tmpl.TemplateState != models.TemplateStateVerifying")
	if cleanupGate < 0 || templateLookup < 0 || stateGate < 0 ||
		cleanupGate > templateLookup || cleanupGate > stateGate {
		t.Fatal("template_verify lookup or lifecycle state gate blocks cleanup-only clone recovery")
	}
	revalidateStart := strings.Index(templateJobsSrc, "func (p *Provisioner) RevalidateL1Template(")
	if revalidateStart < 0 {
		t.Fatal("RevalidateL1Template not found")
	}
	revalidateBody := templateJobsSrc[revalidateStart:]
	if cleanup := strings.Index(revalidateBody, "if jobPayloadCleanupOnly(job)"); cleanup < 0 ||
		cleanup > strings.Index(revalidateBody, "return revalidateL1TemplateJob(") {
		t.Fatal("template_revalidate mutable lookups block cleanup-only clone recovery")
	}
	arm := strings.Index(vcenterSrc, "if err := arm(ctx); err != nil")
	remoteSubmit := strings.Index(vcenterSrc, "template.Clone(ctx, folder, params.VMName, cloneSpec)")
	if arm < 0 || remoteSubmit < 0 || arm > remoteSubmit {
		t.Fatal("durable cleanup intent must be armed immediately before CloneVM_Task submission")
	}
	for _, required := range []string{
		"CloneOperationIDKey",
		"CloneOperationSourceKey",
		"CloneOperationPodVMKey",
		"client.StartCloneVMOperation(",
		"store.PersistVMCloneTask(",
		"client.WaitCloneVMTask(",
		"client.FindVMByCloneOperation(",
		"store.StageVMCloneCleanup(",
		"client.ConfigureClonedVM(",
		"cloneSubmissionReconcileDeadline",
	} {
		if !strings.Contains(vcenterSrc+cloneOperationSrc, required) {
			t.Errorf("durable clone recovery is missing %q", required)
		}
	}
	if !strings.Contains(vcenterInventorySrc, "isManagedObjectNotFound(err)") ||
		!strings.Contains(vcenterInventorySrc, "continue") {
		t.Fatal("clone reconciliation no longer tolerates per-child inventory deletion races")
	}
	if !strings.Contains(rollbackSrc, "handoff.HandoffRollbackStep(") {
		t.Fatal("rollback persistence failure no longer hands its exact receipt to the successor")
	}
	if !strings.Contains(rollbackSrc, "context.WithTimeout(context.WithoutCancel(ctx), rollbackFenceTimeout)") ||
		!strings.Contains(rollbackSrc, "e.persister.SaveRollbackSteps(fenceCtx, e.jobID, remaining)") {
		t.Fatal("rollback destructive undo is missing its active ownership fence")
	}
	if strings.Contains(rollbackSrc, "undoErr = undoFn(") {
		t.Fatal("rollback Record can still destructively undo after ambiguous persistence")
	}
	if !strings.Contains(rollbackSrc, "checkpoint rollback %s after undo") {
		t.Fatal("successful rollback steps are not checkpointed before continuing")
	}
	vmOpsSrc := read("internal/provisioner/vm_ops.go")
	if strings.Contains(querySrc, "func (q *Queries) StageVMCloneDestructionHandoff(") ||
		strings.Contains(querySrc, "func (q *Queries) RecordDestroyedVMCloneHandoff(") ||
		strings.Contains(vmOpsSrc, "destroyer.DestroyVM(") {
		t.Fatal("a lease-lost clone owner can still destroy behind its successor")
	}
}
