package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// RunnerSmokeConfig controls the runner_smoke check's behaviour.
// Defaults are chosen to cover the full Kali-runner path with room for a
// slow vCenter clone, Multus interface attachment, DHCP lease, Kali image
// pull, and action execution, all within an hourly CronJob cadence.
type RunnerSmokeConfig struct {
	// TemplateName MUST match a row in templates.name (NOT vcenter_template).
	// Empty disables the check entirely; the runner won't register it.
	TemplateName string

	// PlaylistID is the UUID of the playlist to run. The playlist-list
	// endpoint (/api/v1/admin/playlists) requires RoleInstructor, so the
	// student synthetic user cannot resolve a playlist by name at runtime.
	// Supply the UUID directly from SYNTHETIC_RUNNER_PLAYLIST_ID.
	// Empty disables the check entirely.
	PlaylistID string

	// ReadyTimeout is the upper bound for waiting on PodStatusActive.
	// 8 minutes accommodates a cold vCenter clone + NetBird onboarding.
	ReadyTimeout time.Duration

	// RunTimeout is the upper bound for waiting on a terminal run state.
	// 10 minutes covers Kali image pull (cold node) + workflow execution
	// + callback latency. Increase if workflows are unusually long.
	RunTimeout time.Duration

	// DestroyTimeout is the upper bound for waiting on PodStatusDestroyed.
	DestroyTimeout time.Duration

	// PreCleanMaxAge controls which orphan synthetic pods are pre-destroyed.
	// 5 minutes is shorter than a normal run so in-flight pods from a
	// concurrent run are not trampled when CronJob overlap happens.
	PreCleanMaxAge time.Duration

	// Logger receives per-step audit lines. Every observable action emits a
	// structured line so an operator paging on this check can reconstruct
	// what happened without re-running it. Defaults to slog.Default().
	Logger *slog.Logger
}

// AttemptTimeout returns the outer Runner context needed for one fully
// configured runner_smoke attempt.
func (cfg RunnerSmokeConfig) AttemptTimeout() time.Duration {
	return synthetic.CheckAttemptTimeout(cfg.ReadyTimeout, cfg.RunTimeout, cfg.DestroyTimeout)
}

// DefaultRunnerSmokeConfig returns production-tuned defaults.
//
// The Helm chart pins the same defaults into the dedicated runner CronJob.
// Eight minutes covers cold clone and network attachment, while ten minutes
// covers a cold Kali image pull and workflow execution. With two attempts and
// a 30-second backoff, the Runner's maximum authorized cycle is:
//
//	2 × (ReadyTimeout + RunTimeout + DestroyTimeout + per-attempt overhead) + Backoff
//	= 2 × (480 s + 600 s + 90 s + 60 s) + 30 s = 2490 s = 41.5 min
//
// That fits beneath the chart's 45-minute active deadline and hourly schedule.
func DefaultRunnerSmokeConfig(templateName, playlistID string) RunnerSmokeConfig {
	return RunnerSmokeConfig{
		TemplateName:   templateName,
		PlaylistID:     playlistID,
		ReadyTimeout:   8 * time.Minute,
		RunTimeout:     10 * time.Minute,
		DestroyTimeout: 90 * time.Second,
		PreCleanMaxAge: 5 * time.Minute,
	}
}

// RunnerSmoke returns a Check that exercises the full Kali-runner assessment
// path: resolve template → pre-clean orphans → create pod → poll active →
// POST testing/run → poll terminal → assert completed with results → destroy.
//
// This is the continuous guard for Epic D: engine dispatch → Multus NAD →
// macvlan DHCP → Kali image pull → action execution → callback → results
// persisted → terminal run state. No other check covers this path.
//
// The check name is stable: runner_smoke. Alert rules and dashboards
// reference this name directly.
func RunnerSmoke(cfg RunnerSmokeConfig) synthetic.Check {
	return synthetic.CheckFunc{
		NameVal:  "runner_smoke",
		TitleVal: "Kali Runner Executes a Workflow",
		DescriptionVal: "Creates a pod and runs an assessment playlist through the Kali runner " +
			"then verifies a terminal run state with at least one workflow result. " +
			"Guards engine dispatch through Multus macvlan DHCP to Kali image pull " +
			"to action execution to callback to results persisted.",
		SeverityVal: synthetic.SeverityCritical,
		RunFn: func(ctx context.Context, c *synthetic.Client) (int, error) {
			return runRunnerSmoke(ctx, c, cfg)
		},
	}
}

// Run statuses as they appear on the wire.
//
// These are deliberately literals here rather than imports of
// models.RunStatus*: this check is a black-box prober, and its job includes
// noticing if the API ever stops reporting the values its clients expect.
// Binding it to the internal constants would make it follow a breaking rename
// silently, which is precisely the regression a synthetic exists to catch.
//
// TestRunnerSmoke_TerminalStatusesMatchTheAPIContract pins them to the model
// constants so a deliberate change fails the build here rather than turning
// this check into a ten-minute timeout with a confusing message.
const successRunStatus = "completed"

// nonTerminalRunStatuses is the set of statuses a run can still MOVE ON from.
// Anything else is treated as terminal.
//
// WHY A DENYLIST AND NOT AN ALLOWLIST OF TERMINAL STATUSES:
//
// This started as an allowlist, and an allowlist has a failure mode that is
// specific to a poller: a status the list does not know about is not reported,
// it is WAITED ON. The check burns the full 10-minute RunTimeout and then
// reports "timed out waiting for terminal run status" — which reads like the
// Kali runner hung, when in fact the run finished promptly and the prober
// simply did not recognise the word. That sends the on-call reader to the
// runner, the node, and the image pull, none of which are the problem.
//
// The in-progress states are the small, stable, well-understood end of this
// contract; the finished states are the end that grows. Prod today enforces:
//
//	runs_status_check CHECK (status = ANY (ARRAY['pending','provisioning',
//	    'running','completed','failed','cancelled','timeout']))
//
// so inverting the test means any status added by a future migration is
// handled correctly on the day it ships, and is reported with its actual name.
//
// This also removes a previously-listed "error" status, which was unreachable:
// it is not in the CHECK constraint above (verified against prod) and has no
// models.RunStatus* constant, so the database would reject it. The comment
// justifying it claimed the engine sets it on infra failures; engine.go
// actually uses RunStatusFailed for exactly those cases.
//
// Still deliberately literals rather than models.RunStatus* imports: this is a
// black-box prober, and following an internal rename silently is precisely the
// regression it exists to catch.
var nonTerminalRunStatuses = []string{"pending", "provisioning", "running"}

func isTerminalRunStatus(s string) bool {
	// An empty status means the response did not carry one. Treat that as
	// non-terminal so a malformed or partial payload results in continued
	// polling rather than being mistaken for a finished run and asserted
	// against as though it had failed.
	if s == "" {
		return false
	}
	for _, t := range nonTerminalRunStatuses {
		if s == t {
			return false
		}
	}
	return true
}

// successResultStatus is the only workflow-result status this check accepts.
//
// Mirrors models.ResultStatusPass. Kept as a local constant rather than an
// import for the same reason as successRunStatus above: this package stays free
// of a models dependency, and a deliberate rename fails the contract test in
// runner_contract_test.go instead of silently changing what "healthy" means.
const successResultStatus = "pass"

// runnerWorkflowResult is the subset of a workflow result this check asserts on.
//
// ActionResults and InstructorOutput are deliberately absent. The synthetic user
// holds the student role, and GetTestingRun (handlers/testing.go) strips both
// fields for non-instructor callers, so they arrive as null no matter what the
// runner produced. The workflow-level Status is therefore the deepest signal
// this check can see — which is precisely why it has to be asserted.
type runnerWorkflowResult struct {
	WorkflowID     string `json:"workflow_id"`
	ExecutionOrder int    `json:"execution_order"`
	Status         string `json:"status"`
	StudentMessage string `json:"student_message"`
}

// runnerRunResponse is the subset of the run JSON we decode during polling.
// We need the run status and the per-workflow results; the rest is ignored.
type runnerRunResponse struct {
	Status  string                 `json:"status"`
	Results []runnerWorkflowResult `json:"results"`
}

// runRunnerSmoke is the actual implementation, factored out for testability.
// All HTTP status returns reflect the LAST status seen so failures surface
// the offending response code in the metric.
func runRunnerSmoke(ctx context.Context, c *synthetic.Client, cfg RunnerSmokeConfig) (int, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("check", "runner_smoke",
		"template", cfg.TemplateName,
		"playlist_id", cfg.PlaylistID,
	)

	attemptCtx, workCtx, cancelAttempt := boundedRunnerAttemptContexts(ctx, cfg)
	defer cancelAttempt()
	ctx = workCtx

	// Fail fast, before any network call, on config the operator must fix.
	//
	// main.go deliberately registers this check even when these are unset
	// (an unregistered check's series ceases to exist and cannot match
	// `1 - crucible_synthetic_check_success > 0`, so it would be invisible
	// rather than red). That makes a clear, self-describing failure here
	// part of the contract: this error text is the ONLY thing telling an
	// operator which env var to set.
	if strings.TrimSpace(cfg.TemplateName) == "" {
		return 0, fmt.Errorf("template name is empty (set SYNTHETIC_RUNNER_TEMPLATE)")
	}
	if strings.TrimSpace(cfg.PlaylistID) == "" {
		return 0, fmt.Errorf("playlist ID is empty (set SYNTHETIC_RUNNER_PLAYLIST_ID)")
	}

	// 1. Resolve template name → UUID via the live /templates endpoint.
	// resolveTemplateID is defined in lifecycle.go; reused here rather than
	// duplicated to keep the two checks' behaviour consistent.
	log.Info("runner_smoke: resolving template")
	tmplID, status, err := resolveTemplateID(ctx, c, cfg.TemplateName)
	if err != nil {
		log.Error("runner_smoke: resolve template failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("resolve template %q: %w", cfg.TemplateName, err)
	}
	log.Info("runner_smoke: template resolved", "template_id", tmplID)

	// 2. Pre-clean: destroy orphan synthetic pods so they don't accumulate
	// when a previous run was killed mid-flight.
	//
	// A pre-clean failure aborts the check rather than being swallowed. If
	// orphans cannot be reaped they will exhaust the synthetic user's pod
	// quota within a few cycles, and the resulting failure surfaces as an
	// opaque quota error at pod creation. Failing here instead names the
	// actual cause while it is still cheap to fix.
	log.Info("runner_smoke: pre-cleaning orphans", "max_age", cfg.PreCleanMaxAge)
	if status, err = preCleanOrphans(ctx, c, cfg.PreCleanMaxAge, log); err != nil {
		log.Error("runner_smoke: pre-clean failed; aborting before pod creation",
			"http_status", status, "error", err.Error())
		return status, fmt.Errorf("pre-clean orphan synthetic pods: %w", err)
	}

	// 3. Create the pod.
	log.Info("runner_smoke: creating pod", "template_id", tmplID)
	podID, status, err := createSyntheticPod(ctx, c, tmplID)
	if err != nil {
		log.Error("runner_smoke: create pod failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("create pod: %w", err)
	}
	log.Info("runner_smoke: pod created", "pod_id", podID, "http_status", status)
	log = log.With("pod_id", podID)

	// 4. Always attempt to destroy the pod, even on later failures, to keep
	// the lab tidy and the synthetic user's pod quota free. The deferred
	// destroy issues DELETE only (no polling) — if the explicit destroy at
	// step 9 already succeeded, destroyPod treats the resulting 404 as
	// success, so double-firing is harmless. The deferred error is only
	// logged, never returned, so it cannot mask the primary failure. The work
	// context expires first, reserving the shared overhead for this request.
	defer func() {
		log.Info("runner_smoke: deferred cleanup destroy")
		s, err := destroyPod(attemptCtx, c, podID)
		if err != nil {
			log.Warn("runner_smoke: deferred destroy failed (best effort)", "http_status", s, "error", err.Error())
		} else {
			log.Info("runner_smoke: deferred destroy issued", "http_status", s)
		}
	}()

	// 5. Poll for active.
	log.Info("runner_smoke: polling for active", "timeout", cfg.ReadyTimeout)
	if status, err = waitForPodStatus(ctx, c, podID, []string{"active"}, cfg.ReadyTimeout, 15*time.Second); err != nil {
		log.Error("runner_smoke: wait for active failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("wait for active: %w", err)
	}
	log.Info("runner_smoke: pod reached active")

	// 6. POST /testing/run to trigger the assessment.
	log.Info("runner_smoke: triggering assessment run", "playlist_id", cfg.PlaylistID)
	runID, status, err := createTestingRun(ctx, c, podID, cfg.PlaylistID)
	if err != nil {
		log.Error("runner_smoke: create testing run failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("create testing run: %w", err)
	}
	log.Info("runner_smoke: testing run created", "run_id", runID)
	log = log.With("run_id", runID)

	// 7. Poll GET /testing/runs/{runID} until a terminal status is reached
	// or RunTimeout expires.
	log.Info("runner_smoke: polling for terminal run status", "timeout", cfg.RunTimeout)
	run, status, err := waitForRunTerminal(ctx, c, podID, runID, cfg.RunTimeout, 15*time.Second)
	if err != nil {
		log.Error("runner_smoke: wait for terminal run failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("wait for terminal run: %w", err)
	}
	log.Info("runner_smoke: run reached terminal state", "run_status", run.Status, "result_count", len(run.Results))

	// 8. Assert the run reached a successful terminal state AND produced at
	// least one result. A runner that silently produces nothing is the exact
	// failure mode this check exists to catch: "completed with no results"
	// is indistinguishable from success if you only assert the status, but
	// it means every action the runner was supposed to execute was skipped.
	if run.Status != successRunStatus {
		return status, fmt.Errorf("run terminated with non-successful status %q (want %q)", run.Status, successRunStatus)
	}
	if len(run.Results) == 0 {
		return status, fmt.Errorf(
			"run completed with zero results: the runner executed but produced no workflow outcomes " +
				"(possible silent action failure or missing playlist assignment on the template)")
	}

	// 8b. Assert every workflow result actually PASSED.
	//
	// This is the assertion that gives the check its meaning, and it is NOT
	// implied by run.Status. The engine marks a run "completed" when the runner
	// reported back, not when the assessments succeeded — finalizeRun in
	// engine.go says so outright: "we don't need to decide pass-vs-fail at the
	// run level". So a run in which Multus never attached the NAD, the CNI dhcp
	// lease never arrived on net1, or nmap was missing from the Kali image comes
	// back "completed" with one result whose status is "fail". Asserting only
	// the run status would report that as GREEN — a check that stays healthy
	// while the exact path it exists to guard is broken.
	//
	// The playlist is version-controlled (deploy/sql/synthetic-runner-smoke-playlist.sql)
	// precisely so it can serve as this check's assertion set: every workflow in
	// it is expected to pass on a healthy lab. Anything else is a real failure,
	// including "skipped", which is what the watchdog writes when the runner dies
	// mid-run — the very scenario a "did it finish?" assertion cannot see.
	var failed []string
	for i, res := range run.Results {
		if res.Status == successResultStatus {
			continue
		}
		label := res.WorkflowID
		if label == "" {
			label = fmt.Sprintf("result[%d]", i)
		}
		detail := res.Status
		if detail == "" {
			// A result row carrying no status is a contract violation, not a
			// pass. Name it explicitly so it is never mistaken for one.
			detail = "<empty>"
		}
		if msg := strings.TrimSpace(res.StudentMessage); msg != "" {
			detail += ": " + snippet([]byte(msg))
		}
		failed = append(failed, label+"="+detail)
	}
	if len(failed) > 0 {
		return status, fmt.Errorf(
			"run completed but %d of %d workflow results did not pass (%s): the Kali runner path is broken. "+
				"Check the pod-VLAN NetworkAttachmentDefinition, the CNI dhcp lease on the runner's net1 "+
				"interface, and the tooling in the runner image",
			len(failed), len(run.Results), strings.Join(failed, ", "))
	}
	log.Info("runner_smoke: all workflow results passed", "result_count", len(run.Results))

	// 9. Explicit destroy + poll so the check confirms the destroy path
	// works too. The deferred destroy at step 4 also fires afterward, but
	// treats the 404 from an already-destroyed pod as success.
	log.Info("runner_smoke: deleting pod")
	if status, err = destroyPod(ctx, c, podID); err != nil {
		log.Error("runner_smoke: delete pod failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("delete pod: %w", err)
	}
	log.Info("runner_smoke: polling for destroyed", "timeout", cfg.DestroyTimeout)
	if status, err = waitForPodStatus(ctx, c, podID, []string{"destroyed"}, cfg.DestroyTimeout, 5*time.Second); err != nil {
		log.Error("runner_smoke: wait for destroyed failed", "http_status", status, "error", err.Error())
		return status, fmt.Errorf("wait for destroyed: %w", err)
	}
	log.Info("runner_smoke: pod destroyed; check passed")

	return http.StatusOK, nil
}

func boundedRunnerAttemptContexts(
	parent context.Context,
	cfg RunnerSmokeConfig,
) (context.Context, context.Context, context.CancelFunc) {
	attemptDeadline := time.Now().Add(cfg.AttemptTimeout())
	attemptCtx, cancelAttempt := context.WithDeadline(parent, attemptDeadline)
	workCtx, cancelWork := context.WithDeadline(
		attemptCtx, attemptDeadline.Add(-synthetic.CheckCleanupReserve),
	)
	return attemptCtx, workCtx, func() {
		cancelWork()
		cancelAttempt()
	}
}

// createTestingRun POSTs to /api/v1/pods/{podID}/testing/run with the given
// playlist UUID. Expects 202 with a run_id in the response body.
//
// Note: workflow_ids is explicitly rejected by the API with 400; only
// playlist_id is supported. See handlers/testing.go CreateTestingRun.
func createTestingRun(ctx context.Context, c *synthetic.Client, podID, playlistID string) (string, int, error) {
	payload, _ := json.Marshal(map[string]any{
		"playlist_id": playlistID,
	})
	resp, err := c.Do(ctx, http.MethodPost, "/api/v1/pods/"+podID+"/testing/run", strings.NewReader(string(payload)))
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusAccepted:
		// happy path — fall through to body parsing
	case http.StatusConflict:
		// 409 means a prior run never reached a terminal state on this pod.
		// This is a configuration/cleanup problem ("check is chatty" or
		// "previous run leaked"), NOT a runner failure. Distinguish it
		// explicitly so an on-call doesn't hunt a phantom runner outage.
		return "", resp.StatusCode, fmt.Errorf(
			"POST /pods/%s/testing/run returned 409: an assessment is already running on this pod "+
				"(a previous run may not have reached a terminal state; check the run dashboard or wait for it to complete)",
			podID)
	case http.StatusTooManyRequests:
		// 429 means the per-(podID,userID) rate limit fired: max 3 runs/hour.
		// This is a check-cadence problem, not a runner problem. Report it
		// with enough context that an on-call can distinguish "runner broken"
		// from "check is too chatty". The Retry-After header is 3600 s.
		retryAfter := resp.Header.Get("Retry-After")
		return "", resp.StatusCode, fmt.Errorf(
			"POST /pods/%s/testing/run returned 429 rate limited (max 3 runs/hour per pod; Retry-After: %s): "+
				"this means the check CronJob is firing too frequently or reusing a pod — it is NOT a runner failure",
			podID, retryAfter)
	default:
		return "", resp.StatusCode, fmt.Errorf("POST /pods/%s/testing/run returned %d: %s",
			podID, resp.StatusCode, snippet(body))
	}
	var parsed struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", resp.StatusCode, fmt.Errorf("testing/run response not JSON: %w", err)
	}
	if parsed.RunID == "" {
		return "", resp.StatusCode, fmt.Errorf("testing/run response missing run_id: %s", snippet(body))
	}
	return parsed.RunID, resp.StatusCode, nil
}

// waitForRunTerminal polls GET /api/v1/pods/{podID}/testing/runs/{runID}
// until the run reaches a terminal status (completed / failed / cancelled /
// timeout), the overall RunTimeout expires, or an unrecoverable error occurs.
// The returned runnerRunResponse includes the final status and results slice
// so the caller can assert both in one place.
func waitForRunTerminal(
	ctx context.Context,
	c *synthetic.Client,
	podID, runID string,
	timeout, interval time.Duration,
) (runnerRunResponse, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	// Cap interval so short timeouts (tests, fast-fail configs) still get
	// multiple poll attempts.
	if interval > timeout/3 && timeout/3 > 0 {
		interval = timeout / 3
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	path := "/api/v1/pods/" + podID + "/testing/runs/" + runID
	var lastStatus int
	var lastRunStatus string
	for {
		resp, err := c.Do(ctx, http.MethodGet, path, nil)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return runnerRunResponse{}, lastStatus,
					fmt.Errorf("timed out waiting for terminal run status (last seen %q) after %s",
						lastRunStatus, timeout)
			}
			return runnerRunResponse{}, lastStatus, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		lastStatus = resp.StatusCode

		if resp.StatusCode != http.StatusOK {
			return runnerRunResponse{}, resp.StatusCode,
				fmt.Errorf("GET %s returned %d: %s", path, resp.StatusCode, snippet(body))
		}

		var run runnerRunResponse
		if err := json.Unmarshal(body, &run); err != nil {
			return runnerRunResponse{}, resp.StatusCode,
				fmt.Errorf("run body not JSON: %w", err)
		}
		lastRunStatus = run.Status

		if isTerminalRunStatus(run.Status) {
			return run, resp.StatusCode, nil
		}

		if time.Now().After(deadline) {
			return runnerRunResponse{}, lastStatus,
				fmt.Errorf("timed out waiting for terminal run status (last seen %q) after %s",
					lastRunStatus, timeout)
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return runnerRunResponse{}, lastStatus,
					fmt.Errorf("timed out waiting for terminal run status (last seen %q) after %s",
						lastRunStatus, timeout)
			}
			return runnerRunResponse{}, lastStatus, ctx.Err()
		case <-time.After(interval):
		}
	}
}
