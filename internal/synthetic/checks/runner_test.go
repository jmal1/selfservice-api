package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

// runnerSmokeFakeAPI is a stateful httptest server that pretends to be the
// Crucible API for runner_smoke tests. It models a tiny state machine for
// both the pod (pending → active → destroying → destroyed) and the run
// (running → configurable terminal state).
type runnerSmokeFakeAPI struct {
	mu sync.Mutex

	templates []map[string]string

	// Pod state machine; keyed by pod ID.
	// fakePod is defined in lifecycle_test.go (same package).
	pods map[string]*fakePod

	// Run state machine; keyed by run ID.
	runs map[string]*runnerFakeRun

	// Pod behaviour controls.
	createActiveAfter int // GET calls on a pod before it flips to "active"

	// Run behaviour controls.
	createRunHTTPStatus int    // HTTP status for POST /testing/run (default 202)
	runTerminalStatus   string // run status once terminal (default "completed")
	runTerminalAfter    int    // GET calls before run becomes terminal (default 1)
	runResultCount      int    // number of results to include in terminal response (default 1)

	// runResultStatuses, when non-nil, overrides runResultCount and emits one
	// workflow result per entry with that entry's status. This is what lets a
	// test model the failure the check actually exists to catch: a run that
	// finishes cleanly while the assessments inside it failed.
	runResultStatuses []string

	// Observed values — inspected by tests.
	createPodCalls     int
	deleteCalls        int
	createRunCalls     int
	postedPlaylistID   string
	lastCreatedPodName string
}

type runnerFakeRun struct {
	ID       string
	getCalls int
}

func newRunnerSmokeFakeAPI() *runnerSmokeFakeAPI {
	return &runnerSmokeFakeAPI{
		templates:           []map[string]string{{"id": "tmpl-runner-1", "name": "synthetic-noop"}},
		pods:                map[string]*fakePod{},
		runs:                map[string]*runnerFakeRun{},
		createActiveAfter:   1,
		createRunHTTPStatus: http.StatusAccepted,
		runTerminalStatus:   "completed",
		runTerminalAfter:    1,
		runResultCount:      1,
	}
}

func (f *runnerSmokeFakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		// GET /api/v1/templates
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/templates":
			_ = json.NewEncoder(w).Encode(f.templates)

		// GET /api/v1/pods  (list — for pre-clean)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
			out := []map[string]any{}
			for _, p := range f.pods {
				if p.Status == "destroyed" {
					continue
				}
				out = append(out, map[string]any{
					"id":         p.ID,
					"name":       p.Name,
					"status":     p.Status,
					"created_at": p.CreatedAt,
				})
			}
			_ = json.NewEncoder(w).Encode(out)

		// POST /api/v1/pods
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/pods":
			f.createPodCalls++
			var req struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.lastCreatedPodName = req.Name
			id := fmt.Sprintf("pod-%d", len(f.pods)+1)
			f.pods[id] = &fakePod{
				ID:        id,
				Name:      req.Name,
				Status:    "pending",
				CreatedAt: time.Now(),
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pod_id": id,
				"status": "pending",
			})

		// POST /api/v1/pods/{podID}/testing/run
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/testing/run"):
			f.createRunCalls++
			podID := strings.TrimPrefix(
				strings.TrimSuffix(r.URL.Path, "/testing/run"),
				"/api/v1/pods/",
			)
			if _, ok := f.pods[podID]; !ok {
				http.NotFound(w, r)
				return
			}
			// Decode the playlist_id so tests can assert it was sent correctly.
			var req struct {
				PlaylistID string `json:"playlist_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.postedPlaylistID = req.PlaylistID

			if f.createRunHTTPStatus != http.StatusAccepted {
				http.Error(w, "create run failed", f.createRunHTTPStatus)
				return
			}
			runID := fmt.Sprintf("run-%d", len(f.runs)+1)
			f.runs[runID] = &runnerFakeRun{ID: runID}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run_id":  runID,
				"status":  "pending",
				"message": "Assessment run queued.",
			})

		// GET /api/v1/pods/{podID}/testing/runs/{runID}
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/testing/runs/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/pods/"), "/")
			// parts: [podID, "testing", "runs", runID]
			if len(parts) != 4 {
				http.NotFound(w, r)
				return
			}
			runID := parts[3]
			run, ok := f.runs[runID]
			if !ok {
				http.NotFound(w, r)
				return
			}
			run.getCalls++

			status := "running"
			if run.getCalls > f.runTerminalAfter {
				status = f.runTerminalStatus
			}

			var results []map[string]any
			if status == "completed" || status == "failed" {
				statuses := f.runResultStatuses
				if statuses == nil {
					statuses = make([]string, f.runResultCount)
					for i := range statuses {
						statuses[i] = "pass"
					}
				}
				for i, st := range statuses {
					results = append(results, map[string]any{
						"id":              fmt.Sprintf("result-%d", i+1),
						"workflow_id":     fmt.Sprintf("wf-%d", i+1),
						"execution_order": i,
						"status":          st,
						// Mirrors production: GetTestingRun strips action_results
						// and instructor_output for the student-role synthetic
						// user, so student_message is the only diagnostic text
						// the check can see.
						"student_message": "",
					})
				}
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":      run.ID,
				"status":  status,
				"results": results,
			})

		// GET /api/v1/pods/{podID}  (single pod — for polling)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/pods/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/pods/")
			p, ok := f.pods[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			p.getCalls++
			if p.Status == "pending" && p.getCalls > f.createActiveAfter {
				p.Status = "active"
			}
			if p.deleteAt != nil && p.Status != "destroyed" {
				p.Status = "destroyed"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     p.ID,
				"name":   p.Name,
				"status": p.Status,
			})

		// DELETE /api/v1/pods/{podID}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/pods/"):
			f.deleteCalls++
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/pods/")
			p, ok := f.pods[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			now := time.Now()
			p.deleteAt = &now
			p.Status = "destroying"
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})

		default:
			http.NotFound(w, r)
		}
	})
}

// cfg returns a RunnerSmokeConfig suitable for tests: short timeouts so tests
// finish quickly, seeded with the fake template name and a placeholder
// playlist UUID.
func runnerSmokeTestCfg(playlistID string) RunnerSmokeConfig {
	if playlistID == "" {
		playlistID = "00000000-0000-0000-0000-000000000001"
	}
	return RunnerSmokeConfig{
		TemplateName:   "synthetic-noop",
		PlaylistID:     playlistID,
		ReadyTimeout:   5 * time.Second,
		RunTimeout:     3 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 1 * time.Minute,
	}
}

// ---- happy path ----------------------------------------------------------

func TestRunnerSmoke_HappyPath(t *testing.T) {
	const wantPlaylistID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	fake := newRunnerSmokeFakeAPI()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	cfg := runnerSmokeTestCfg(wantPlaylistID)
	chk := RunnerSmoke(cfg)
	status, err := chk.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err != nil {
		t.Fatalf("happy path should succeed: status=%d err=%v", status, err)
	}
	if status != http.StatusOK {
		t.Errorf("status=%d, want 200", status)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.createPodCalls != 1 {
		t.Errorf("createPodCalls=%d, want 1", fake.createPodCalls)
	}
	// At minimum: the explicit lifecycle DELETE + the deferred safety DELETE.
	if fake.deleteCalls < 1 {
		t.Errorf("deleteCalls=%d, want >=1 (explicit + deferred)", fake.deleteCalls)
	}
	if fake.createRunCalls != 1 {
		t.Errorf("createRunCalls=%d, want 1", fake.createRunCalls)
	}
	if fake.postedPlaylistID != wantPlaylistID {
		t.Errorf("postedPlaylistID=%q, want %q", fake.postedPlaylistID, wantPlaylistID)
	}
}

// ---- pod name prefix guard -----------------------------------------------

// TestRunnerSmoke_PodNamePrefix asserts the created pod name starts with
// SyntheticPodNamePrefix. This is load-bearing: the janitor CronJob sweeps
// only pods with this prefix; a name mismatch means leaked pods accumulate
// silently and exhaust the synthetic user's quota.
func TestRunnerSmoke_PodNamePrefix(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	if _, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, "")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if !strings.HasPrefix(fake.lastCreatedPodName, SyntheticPodNamePrefix) {
		t.Errorf("pod name %q must start with SyntheticPodNamePrefix=%q",
			fake.lastCreatedPodName, SyntheticPodNamePrefix)
	}
}

// ---- failed terminal run state guard -------------------------------------

// TestRunnerSmoke_RunFailedState guards the core assertion: a run that reaches
// a "failed" terminal state must cause the check to return an error. A runner
// that reports failure is useless if the check silently accepts it as success.
func TestRunnerSmoke_RunFailedState(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "failed"
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a run that terminates as 'failed' MUST fail the check — that is the entire point")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("error %q must name the terminal status", err)
	}
}

// ---- zero results guard --------------------------------------------------

// TestRunnerSmoke_ZeroResults guards the "silent no-op" failure mode: a run
// that reports "completed" but produced zero workflow results means the runner
// executed but skipped every action. This is indistinguishable from success
// if you only assert the status, so we assert result count too.
func TestRunnerSmoke_ZeroResults(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "completed"
	fake.runResultCount = 0 // no results
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a completed run with zero results MUST fail — 'no results' is the exact failure mode this check exists to catch")
	}
	if !strings.Contains(err.Error(), "zero results") {
		t.Errorf("error %q must mention zero results so an on-call can distinguish this from a status failure", err)
	}
}

// ---- workflow-result status guard ----------------------------------------

// TestRunnerSmoke_FailingWorkflowResultFailsTheCheck is the guard for the
// hollow-check failure mode found in production on 2026-08-03.
//
// The engine marks a run "completed" when the runner reports back, NOT when the
// assessments succeeded — finalizeRun in engine.go says so explicitly ("we don't
// need to decide pass-vs-fail at the run level"). So every failure this check
// exists to detect — the Multus NAD missing, no CNI dhcp lease on net1, the
// target unreachable on the pod VLAN, nmap absent from the Kali image — arrives
// as a run with status "completed" carrying results whose status is "fail".
//
// Asserting only the run status and result COUNT therefore reported those as
// GREEN. That is strictly worse than having no check at all, because the board
// actively asserts a broken path is healthy.
func TestRunnerSmoke_FailingWorkflowResultFailsTheCheck(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "completed" // the run itself finished fine
	fake.runResultStatuses = []string{"fail"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a run that COMPLETED with a failing workflow result MUST fail the check. " +
			"'completed' means the runner reported back, not that the assessment passed — if this " +
			"passes, runner_smoke stays green while Multus, the DHCP lease, or nmap are broken")
	}
	if !strings.Contains(err.Error(), "did not pass") {
		t.Errorf("error %q must say the results did not pass, so an on-call can tell this apart "+
			"from a run-level status failure or a zero-results failure", err)
	}
	if !strings.Contains(err.Error(), "wf-1") {
		t.Errorf("error %q must name the failing workflow — a count alone forces the on-call "+
			"to go digging in the database", err)
	}
}

// TestRunnerSmoke_SkippedWorkflowResultFailsTheCheck covers the watchdog path.
// When the runner pod dies mid-run, remaining workflows are recorded "skipped"
// and the run still closes out as "completed". A check that only asks "did it
// finish?" cannot see that at all.
func TestRunnerSmoke_SkippedWorkflowResultFailsTheCheck(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "completed"
	fake.runResultStatuses = []string{"pass", "skipped"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a 'skipped' workflow result MUST fail the check — it means the runner died mid-run " +
			"and the remaining assessments never executed")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("error %q must report how many of how many results failed, so a single flaky "+
			"workflow reads differently from a total runner outage", err)
	}
}

// TestRunnerSmoke_EmptyResultStatusFailsTheCheck pins the default. A result row
// carrying no status at all is a contract violation; treating an unrecognised or
// absent value as a pass is exactly how a check rots into decoration.
func TestRunnerSmoke_EmptyResultStatusFailsTheCheck(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "completed"
	fake.runResultStatuses = []string{""}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a workflow result with an EMPTY status MUST fail the check — anything that is not " +
			"explicitly 'pass' has not been proven to work")
	}
	if !strings.Contains(err.Error(), "<empty>") {
		t.Errorf("error %q must render the empty status visibly rather than as a blank gap", err)
	}
}

// TestRunnerSmoke_OnlyPassIsAccepted pins the accepted set in both directions so
// widening it later is a deliberate, visible edit rather than a quiet drift.
func TestRunnerSmoke_OnlyPassIsAccepted(t *testing.T) {
	if successResultStatus != "pass" {
		t.Fatalf("successResultStatus = %q, want \"pass\" (models.ResultStatusPass)", successResultStatus)
	}
	// Every other status the API can emit must be rejected. Sourced from
	// models.ResultStatus* in internal/models/workflow_models.go.
	for _, st := range []string{"pending", "running", "fail", "error", "timeout", "skipped", "cancelled"} {
		t.Run(st, func(t *testing.T) {
			fake := newRunnerSmokeFakeAPI()
			fake.runTerminalStatus = "completed"
			fake.runResultStatuses = []string{st}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
			if err == nil {
				t.Fatalf("workflow result status %q MUST fail runner_smoke; only %q proves the "+
					"Kali runner path actually worked", st, successResultStatus)
			}
		})
	}
}

// ---- run timeout guard ---------------------------------------------------

// TestRunnerSmoke_RunTimeout guards the timeout path: a run that never reaches
// a terminal state within RunTimeout must surface as a timeout error that
// includes the last-seen status, so an on-call knows where the run was stuck.
func TestRunnerSmoke_RunTimeout(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalAfter = 99999 // never become terminal
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	cfg := runnerSmokeTestCfg("")
	cfg.RunTimeout = 300 * time.Millisecond
	_, err := RunnerSmoke(cfg).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected run-timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q must mention timeout", err)
	}
	// The last-seen status must appear so an on-call knows where the run stalled.
	if !strings.Contains(err.Error(), "running") {
		t.Errorf("error %q must include the last-seen run status", err)
	}
}

// ---- pod destroyed on failure (quota-exhaustion guard) -------------------

// TestRunnerSmoke_PodDestroyedOnRunFailure is the load-bearing guard for the
// defer. A run that ends in a non-successful state must not leak the pod:
// leaked synthetic pods exhaust the synthetic user's quota and then every
// subsequent pod_lifecycle and runner_smoke run fails with 409.
func TestRunnerSmoke_PodDestroyedOnRunFailure(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalStatus = "failed"
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected failure from failed run status")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.deleteCalls < 1 {
		t.Errorf("deleteCalls=%d — a failed run must still DELETE the pod; leaked pods exhaust the quota and break pod_lifecycle with 409", fake.deleteCalls)
	}
}

// TestRunnerSmoke_PodDestroyedOnRunTimeout covers the defer for the timeout
// failure path specifically, since the deferred destroy fires from a different
// code path than the explicit destroy at the end of a happy run.
func TestRunnerSmoke_PodDestroyedOnRunTimeout(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.runTerminalAfter = 99999
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	cfg := runnerSmokeTestCfg("")
	cfg.RunTimeout = 300 * time.Millisecond
	_, err := RunnerSmoke(cfg).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("expected timeout error")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if fake.deleteCalls < 1 {
		t.Errorf("deleteCalls=%d — a timed-out run must still DELETE the pod", fake.deleteCalls)
	}
}

// ---- 409 on create run ---------------------------------------------------

// TestRunnerSmoke_409OnCreateRun guards that a 409 from POST /testing/run is
// surfaced as a clear error, not swallowed. A 409 means another assessment is
// already running on the pod — if we silently ignored it we would think the
// check passed when no new run was dispatched at all.
func TestRunnerSmoke_409OnCreateRun(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.createRunHTTPStatus = http.StatusConflict
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a 409 from POST /testing/run must fail the check")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("error %q must include the status code so the on-call knows it was a conflict", err)
	}
	// Must distinguish "already running" from a generic runner failure so the
	// on-call can tell the difference between a misconfigured check cadence and
	// a broken runner without reading source code.
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q must mention 'already running' to distinguish a conflict from a runner failure", err)
	}
}

// ---- 429 (rate limit) on create run -------------------------------------

// TestRunnerSmoke_429OnCreateRun guards that a 429 from POST /testing/run is
// surfaced as a distinct error explaining rate-limiting. A 429 means the check
// CronJob is too chatty or reusing a long-lived pod — it is NOT a runner
// failure and must not be reported as one.
func TestRunnerSmoke_429OnCreateRun(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	fake.createRunHTTPStatus = http.StatusTooManyRequests
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	_, err := RunnerSmoke(runnerSmokeTestCfg("")).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("a 429 from POST /testing/run must fail the check")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error %q must include the status code", err)
	}
	// Must distinguish "rate limited" from a runner failure; conflating them
	// sends someone hunting a phantom runner outage when the check is the problem.
	if !strings.Contains(strings.ToLower(err.Error()), "rate") {
		t.Errorf("error %q must mention rate limiting to distinguish it from a runner failure", err)
	}
}

// ---- empty playlist ID guard ---------------------------------------------

// TestRunnerSmoke_EmptyPlaylistID guards that a missing SYNTHETIC_RUNNER_PLAYLIST_ID
// fails fast (before creating any pod) with a message naming the env var.
// This prevents a confusing API 400 or 404 from being the first symptom.
func TestRunnerSmoke_EmptyPlaylistID(t *testing.T) {
	fake := newRunnerSmokeFakeAPI()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	cfg := RunnerSmokeConfig{
		TemplateName:   "synthetic-noop",
		PlaylistID:     "", // deliberately empty
		ReadyTimeout:   2 * time.Second,
		RunTimeout:     2 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: 1 * time.Minute,
	}
	_, err := RunnerSmoke(cfg).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("empty PlaylistID must fail immediately")
	}
	if !strings.Contains(err.Error(), "SYNTHETIC_RUNNER_PLAYLIST_ID") {
		t.Errorf("error %q must name the env var so an operator can fix it without reading source", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createPodCalls != 0 {
		t.Errorf("createPodCalls=%d: must fail before creating any pod (no wasted quota)", fake.createPodCalls)
	}
}

// ---- metadata guard ------------------------------------------------------

// TestRunnerSmoke_Metadata pins the check's stable name (referenced by alert
// rules and dashboards), its severity, and the no-comma requirement for
// Prometheus exposition labels.
func TestRunnerSmoke_Metadata(t *testing.T) {
	chk := RunnerSmoke(DefaultRunnerSmokeConfig("synthetic-noop", "00000000-0000-0000-0000-000000000001"))

	if chk.Name() != "runner_smoke" {
		t.Errorf("Name()=%q, want runner_smoke (alert rules and dashboards reference this name)", chk.Name())
	}
	if chk.Severity() != synthetic.SeverityCritical {
		t.Errorf("Severity()=%q, want critical", chk.Severity())
	}
	if strings.TrimSpace(chk.Title()) == "" {
		t.Error("Title() is empty")
	}
	if len(chk.Title()) > 60 {
		t.Errorf("Title()=%q is too long (>60 chars; keep it pill-sized for the dashboard)", chk.Title())
	}
	if strings.TrimSpace(chk.Description()) == "" {
		t.Error("Description() is empty")
	}
	// Commas, double-quotes, and newlines inside a Prometheus exposition label
	// value corrupt the whole push batch. Enforce the same rule here that
	// TestElevated_HasFriendlyMetadata enforces for elevated checks.
	for _, bad := range []string{",", `"`, "\n"} {
		if strings.Contains(chk.Title()+chk.Description(), bad) {
			t.Errorf("metadata contains %q which corrupts Prometheus exposition labels", bad)
		}
	}
}
