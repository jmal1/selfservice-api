package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// --- Test doubles ----------------------------------------------------------

type fakeExecutor struct {
	mu        sync.Mutex
	calls     []vcenter.GuestExecRequest
	responder func(vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error)
}

func (f *fakeExecutor) RunScriptInGuest(ctx context.Context, req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	resp := f.responder
	f.mu.Unlock()
	if resp == nil {
		return &vcenter.GuestExecResult{ExitCode: 0, Stdout: "ok\n", Duration: 100 * time.Millisecond}, nil
	}
	return resp(req)
}

type fakeQueries struct {
	mu      sync.Mutex
	updates []workflowUpdate
	counts  []uuid.UUID
	failOn  string // slug to fail UpdateWorkflowResultBySlug for
}

type workflowUpdate struct {
	RunID            uuid.UUID
	Slug             string
	Status           string
	Message          string
	InstructorOutput json.RawMessage
	ActionResults    json.RawMessage
	DurationMs       *int
}

func (q *fakeQueries) UpdateWorkflowResultBySlug(ctx context.Context, runID uuid.UUID, slug, status, message string, instructorOutput, actionResults []byte, durationMs *int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.updates = append(q.updates, workflowUpdate{
		RunID: runID, Slug: slug, Status: status, Message: message,
		InstructorOutput: instructorOutput, ActionResults: actionResults, DurationMs: durationMs,
	})
	if slug == q.failOn {
		return errors.New("simulated db failure")
	}
	return nil
}

func (q *fakeQueries) UpdateRunCounts(ctx context.Context, runID uuid.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.counts = append(q.counts, runID)
	return nil
}

type fakePublisher struct {
	mu     sync.Mutex
	events []string
}

func (p *fakePublisher) publishRunEvent(podID, runID, eventType, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, eventType+":"+message)
}

// --- Helpers ---------------------------------------------------------------

func newTestRun() *models.Run {
	return &models.Run{ID: uuid.New(), PodID: uuid.New()}
}

func newTestWorkflow(slug, script string, timeoutSec int) models.Workflow {
	return models.Workflow{
		ID:             uuid.New(),
		Slug:           slug,
		Name:           slug,
		Script:         script,
		ExecutionMode:  models.ExecModeVMwareTools,
		TimeoutSeconds: timeoutSec,
	}
}

func validTarget() VMwareToolsTarget {
	return VMwareToolsTarget{
		VMMoref:  "vm-42",
		OS:       "linux",
		Username: "student",
		Password: "secret",
	}
}

// --- Tests -----------------------------------------------------------------

func TestDispatch_HappyPath_PassWorkflow(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	pub := &fakePublisher{}
	d := NewVMwareToolsDispatcher(exec, q, pub, nil)

	run := newTestRun()
	workflows := []models.Workflow{newTestWorkflow("check-uptime", "uptime", 60)}

	if err := d.Dispatch(context.Background(), run, workflows, validTarget()); err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}

	if len(q.updates) != 1 {
		t.Fatalf("expected 1 workflow update, got %d", len(q.updates))
	}
	u := q.updates[0]
	if u.Status != models.ResultStatusPass {
		t.Errorf("expected pass status, got %s", u.Status)
	}
	if u.Slug != "check-uptime" {
		t.Errorf("wrong slug: %s", u.Slug)
	}
	if len(q.counts) != 1 {
		t.Errorf("expected 1 UpdateRunCounts call, got %d", len(q.counts))
	}
	if len(pub.events) < 2 {
		t.Errorf("expected at least workflow_start + workflow_complete events, got %d: %v", len(pub.events), pub.events)
	}
}

func TestDispatch_FailWorkflow_RecordsFailStatus(t *testing.T) {
	exec := &fakeExecutor{
		responder: func(req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
			return &vcenter.GuestExecResult{
				ExitCode: 1,
				Stdout:   "",
				Stderr:   "service not running\n",
				Duration: 200 * time.Millisecond,
			}, nil
		},
	}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)

	run := newTestRun()
	wfs := []models.Workflow{newTestWorkflow("nginx-up", "systemctl is-active nginx", 60)}
	if err := d.Dispatch(context.Background(), run, wfs, validTarget()); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}

	if got := q.updates[0].Status; got != models.ResultStatusFail {
		t.Errorf("expected fail, got %s", got)
	}
	if got := q.updates[0].Message; got != "service not running" {
		t.Errorf("expected stderr last line as message, got %q", got)
	}
}

func TestDispatch_TimedOut_RecordsTimeoutStatus(t *testing.T) {
	exec := &fakeExecutor{
		responder: func(req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
			return &vcenter.GuestExecResult{
				ExitCode: -1,
				TimedOut: true,
				Duration: 5 * time.Minute,
			}, nil
		},
	}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	if err := d.Dispatch(context.Background(), run, []models.Workflow{newTestWorkflow("slow", "sleep 9999", 1)}, validTarget()); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if got := q.updates[0].Status; got != models.ResultStatusTimeout {
		t.Errorf("expected timeout, got %s", got)
	}
}

func TestDispatch_GuestExecError_RecordsErrorStatus(t *testing.T) {
	exec := &fakeExecutor{
		responder: func(req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
			return nil, errors.New("vm not powered on")
		},
	}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	if err := d.Dispatch(context.Background(), run, []models.Workflow{newTestWorkflow("noop", "true", 60)}, validTarget()); err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if got := q.updates[0].Status; got != models.ResultStatusError {
		t.Errorf("expected error, got %s", got)
	}
	if got := q.updates[0].Message; got == "" {
		t.Error("expected non-empty error message")
	}
}

func TestDispatch_InvalidTarget_MarksAllWorkflowsAsError(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	wfs := []models.Workflow{
		newTestWorkflow("a", "true", 60),
		newTestWorkflow("b", "true", 60),
	}
	// Missing VMMoref → validation fails
	bad := VMwareToolsTarget{OS: "linux", Username: "x", Password: "y"}
	if err := d.Dispatch(context.Background(), run, wfs, bad); err == nil {
		t.Fatal("expected error from Dispatch with invalid target")
	}
	if len(q.updates) != 2 {
		t.Errorf("expected 2 error rows, got %d", len(q.updates))
	}
	for _, u := range q.updates {
		if u.Status != models.ResultStatusError {
			t.Errorf("expected error status for %s, got %s", u.Slug, u.Status)
		}
	}
	// Executor must NOT have been called
	if len(exec.calls) != 0 {
		t.Errorf("expected 0 executor calls, got %d", len(exec.calls))
	}
}

func TestDispatch_ContextCancelled_RemainingWorkflowsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan struct{})
	exec := &fakeExecutor{
		responder: func(req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
			// Signal first workflow ran, then cancel ctx before second.
			close(first)
			return &vcenter.GuestExecResult{ExitCode: 0, Stdout: "ok"}, nil
		},
	}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	wfs := []models.Workflow{
		newTestWorkflow("first", "true", 60),
		newTestWorkflow("second", "true", 60),
		newTestWorkflow("third", "true", 60),
	}
	// Cancel right after first executes
	go func() {
		<-first
		cancel()
	}()
	err := d.Dispatch(ctx, run, wfs, validTarget())
	if err == nil {
		t.Error("expected ctx.Err() back from Dispatch")
	}
	// first should be pass, second+third should be cancelled
	statusBySlug := map[string]string{}
	for _, u := range q.updates {
		statusBySlug[u.Slug] = u.Status
	}
	if statusBySlug["first"] != models.ResultStatusPass {
		t.Errorf("first should be pass, got %s", statusBySlug["first"])
	}
	if statusBySlug["second"] != models.ResultStatusCancelled {
		t.Errorf("second should be cancelled, got %s", statusBySlug["second"])
	}
	if statusBySlug["third"] != models.ResultStatusCancelled {
		t.Errorf("third should be cancelled, got %s", statusBySlug["third"])
	}
}

func TestDispatch_LanguageOverride_RespectsGuestInterpreter(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	wf := newTestWorkflow("ps", "Get-Service", 60)
	ps := "powershell"
	wf.GuestInterpreter = &ps
	tgt := validTarget()
	tgt.OS = "linux" // override should still win
	if err := d.Dispatch(context.Background(), run, []models.Workflow{wf}, tgt); err != nil {
		t.Fatal(err)
	}
	if exec.calls[0].Language != "powershell" {
		t.Errorf("expected language=powershell from override, got %s", exec.calls[0].Language)
	}
}

func TestDispatch_WindowsOS_DefaultsToPowershell(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	tgt := validTarget()
	tgt.OS = "windows"
	if err := d.Dispatch(context.Background(), newTestRun(), []models.Workflow{newTestWorkflow("a", "Get-Date", 60)}, tgt); err != nil {
		t.Fatal(err)
	}
	if exec.calls[0].Language != "powershell" {
		t.Errorf("windows OS should default to powershell, got %s", exec.calls[0].Language)
	}
}

func TestDispatch_DefaultTimeout_AppliedWhenZero(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	wf := newTestWorkflow("a", "true", 0)
	if err := d.Dispatch(context.Background(), newTestRun(), []models.Workflow{wf}, validTarget()); err != nil {
		t.Fatal(err)
	}
	if exec.calls[0].Timeout != 5*time.Minute {
		t.Errorf("expected 5m default, got %s", exec.calls[0].Timeout)
	}
}

func TestDispatch_RequestFieldsPlumbedThrough(t *testing.T) {
	exec := &fakeExecutor{}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	run := newTestRun()
	wf := newTestWorkflow("plumbing", "echo hi", 30)
	tgt := validTarget()
	if err := d.Dispatch(context.Background(), run, []models.Workflow{wf}, tgt); err != nil {
		t.Fatal(err)
	}
	got := exec.calls[0]
	if got.VMMoref != tgt.VMMoref {
		t.Errorf("VMMoref not plumbed: %s vs %s", got.VMMoref, tgt.VMMoref)
	}
	if got.GuestUser != tgt.Username || got.GuestPassword != tgt.Password {
		t.Error("credentials not plumbed")
	}
	if got.Script != "echo hi" {
		t.Errorf("script not plumbed: %q", got.Script)
	}
	if got.RunID != run.ID.String() {
		t.Errorf("RunID not plumbed")
	}
	if got.ActionSlug != "plumbing" {
		t.Errorf("ActionSlug not plumbed: %s", got.ActionSlug)
	}
	if got.Timeout != 30*time.Second {
		t.Errorf("Timeout not plumbed: %s", got.Timeout)
	}
}

func TestDispatch_ActionContextIncludesStdoutStderr(t *testing.T) {
	exec := &fakeExecutor{
		responder: func(req vcenter.GuestExecRequest) (*vcenter.GuestExecResult, error) {
			return &vcenter.GuestExecResult{
				ExitCode:     0,
				Stdout:       "line1\nline2\n",
				Stderr:       "warn1\n",
				Duration:     100 * time.Millisecond,
				TruncatedOut: true,
			}, nil
		},
	}
	q := &fakeQueries{}
	d := NewVMwareToolsDispatcher(exec, q, nil, nil)
	if err := d.Dispatch(context.Background(), newTestRun(), []models.Workflow{newTestWorkflow("a", "true", 60)}, validTarget()); err != nil {
		t.Fatal(err)
	}
	var results []map[string]any
	if err := json.Unmarshal(q.updates[0].ActionResults, &results); err != nil {
		t.Fatalf("unmarshal action results: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 action, got %d", len(results))
	}
	ctxRaw, ok := results[0]["context"]
	if !ok {
		t.Fatal("expected context field on action")
	}
	ctx, _ := ctxRaw.(map[string]any)
	if ctx["stdout"] != "line1\nline2\n" {
		t.Errorf("stdout not preserved: %v", ctx["stdout"])
	}
	if ctx["stderr"] != "warn1\n" {
		t.Errorf("stderr not preserved: %v", ctx["stderr"])
	}
	if ctx["truncated"] != true {
		t.Errorf("truncated flag not preserved")
	}
}

// --- Pure function tests ---------------------------------------------------

func TestClassifyGuestResult_NilReturnsError(t *testing.T) {
	s, m := classifyGuestResult(nil)
	if s != models.ResultStatusError || m == "" {
		t.Errorf("nil result should be error+message, got %s/%q", s, m)
	}
}

func TestClassifyGuestResult_TimeoutBeatsExitCode(t *testing.T) {
	r := &vcenter.GuestExecResult{ExitCode: 0, TimedOut: true, Duration: 10 * time.Second}
	s, _ := classifyGuestResult(r)
	if s != models.ResultStatusTimeout {
		t.Errorf("expected timeout, got %s", s)
	}
}

func TestClassifyGuestResult_PassUsesStdoutLastLine(t *testing.T) {
	r := &vcenter.GuestExecResult{ExitCode: 0, Stdout: "starting\nall good\n"}
	_, m := classifyGuestResult(r)
	if m != "all good" {
		t.Errorf("expected 'all good', got %q", m)
	}
}

func TestClassifyGuestResult_FailPrefersStderr(t *testing.T) {
	r := &vcenter.GuestExecResult{ExitCode: 2, Stdout: "trying\n", Stderr: "permission denied\n"}
	_, m := classifyGuestResult(r)
	if m != "permission denied" {
		t.Errorf("expected stderr last line, got %q", m)
	}
}

func TestClassifyGuestResult_FailFallsBackToExitCode(t *testing.T) {
	r := &vcenter.GuestExecResult{ExitCode: 7}
	_, m := classifyGuestResult(r)
	if m == "" {
		t.Error("expected non-empty message even without stdout/stderr")
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"\n\n":             "",
		"only":             "only",
		"a\nb\nc\n":        "c",
		"a\nb\n\n\n":       "b",
		"  leading\n":      "leading",
		"x\r\ny\r\n":       "y",
		"a\nb\nc with text":"c with text",
	}
	for in, want := range cases {
		if got := lastNonEmptyLine(in); got != want {
			t.Errorf("lastNonEmptyLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLanguageForOS(t *testing.T) {
	ps := "pwsh"
	cases := []struct {
		os       string
		override *string
		want     string
	}{
		{"linux", nil, "bash"},
		{"", nil, "bash"},
		{"windows", nil, "powershell"},
		{"linux", &ps, "pwsh"},
		{"windows", &ps, "pwsh"},
	}
	for _, c := range cases {
		if got := languageForOS(c.os, c.override); got != c.want {
			t.Errorf("languageForOS(%q, %v) = %s, want %s", c.os, c.override, got, c.want)
		}
	}
}

func TestValidateTarget(t *testing.T) {
	good := validTarget()
	if err := validateTarget(good); err != nil {
		t.Errorf("good target failed: %v", err)
	}
	for _, mutate := range []func(*VMwareToolsTarget){
		func(t *VMwareToolsTarget) { t.VMMoref = "" },
		func(t *VMwareToolsTarget) { t.Username = "" },
		func(t *VMwareToolsTarget) { t.Password = "" },
	} {
		bad := good
		mutate(&bad)
		if err := validateTarget(bad); err == nil {
			t.Errorf("expected error for mutated target %+v", bad)
		}
	}
}

func TestWorkflowTimeout(t *testing.T) {
	if workflowTimeout(0) != 5*time.Minute {
		t.Error("zero should default to 5m")
	}
	if workflowTimeout(-1) != 5*time.Minute {
		t.Error("negative should default to 5m")
	}
	if workflowTimeout(60) != time.Minute {
		t.Error("60 should be 1m")
	}
}
