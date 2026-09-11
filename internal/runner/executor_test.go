package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWorkflowExecution_SimplePass(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{
				Slug:           "simple-pass",
				Name:           "Simple Pass",
				Script:         "#!/bin/bash\nexit 0",
				TimeoutSeconds: 10,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if result.Status != "pass" {
		t.Errorf("Status = %q, want %q", result.Status, "pass")
	}
	if result.WorkflowSlug != "simple-pass" {
		t.Errorf("WorkflowSlug = %q, want %q", result.WorkflowSlug, "simple-pass")
	}
}

func TestWorkflowExecution_SimpleFail(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{
				Slug:           "simple-fail",
				Name:           "Simple Fail",
				Script:         "#!/bin/bash\nexit 1",
				TimeoutSeconds: 10,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if result.Status != "fail" {
		t.Errorf("Status = %q, want %q", result.Status, "fail")
	}
}

func TestWorkflowExecution_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{
				Slug:           "timeout-test",
				Name:           "Timeout Test",
				Script:         "#!/bin/bash\nsleep 30",
				TimeoutSeconds: 1,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if result.Status != "timeout" {
		t.Errorf("Status = %q, want %q", result.Status, "timeout")
	}
}

func TestWorkflowExecution_Setup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{
				Slug:           "with-setup",
				Name:           "With Setup",
				Script:         "#!/bin/bash\nexit 0",
				Setup:          "#!/bin/bash\necho 'setup complete'",
				TimeoutSeconds: 10,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if result.Status != "pass" {
		t.Errorf("Status = %q, want %q", result.Status, "pass")
	}
	if result.SetupOutput == nil {
		t.Fatal("expected non-nil SetupOutput")
	}
	if result.SetupOutput.Status != "pass" {
		t.Errorf("SetupOutput.Status = %q, want %q", result.SetupOutput.Status, "pass")
	}
}

func TestWorkflowExecution_SetupFailAborts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{
				Slug:           "setup-fail",
				Name:           "Setup Fail",
				Script:         "#!/bin/bash\nexit 0",
				Setup:          "#!/bin/bash\nexit 1",
				TimeoutSeconds: 10,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if result.Status != "error" {
		t.Errorf("Status = %q, want %q", result.Status, "error")
	}
	if result.Message != "Setup script failed" {
		t.Errorf("Message = %q, want %q", result.Message, "Setup script failed")
	}
}

func TestWorkflowExecution_RunAll(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{Slug: "wf-1", Name: "WF 1", Script: "#!/bin/bash\nexit 0", TimeoutSeconds: 10},
			{Slug: "wf-2", Name: "WF 2", Script: "#!/bin/bash\nexit 0", TimeoutSeconds: 10},
			{Slug: "wf-3", Name: "WF 3", Script: "#!/bin/bash\nexit 0", TimeoutSeconds: 10},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	executor := NewExecutor(cfg, callback, logger)
	results, err := executor.RunAll(context.Background())
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	for i, r := range results {
		if r.Status != "pass" {
			t.Errorf("result[%d].Status = %q, want pass", i, r.Status)
		}
	}
}

func TestWorkflowExecution_CancelledContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows: []WorkflowDef{
			{Slug: "wf-1", Name: "WF 1", Script: "#!/bin/bash\nexit 0", TimeoutSeconds: 10},
			{Slug: "wf-2", Name: "WF 2", Script: "#!/bin/bash\nexit 0", TimeoutSeconds: 10},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	// Cancel context immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	executor := NewExecutor(cfg, callback, logger)
	results, _ := executor.RunAll(ctx)

	// Both should be skipped
	for i, r := range results {
		if r.Status != "skipped" {
			t.Errorf("result[%d].Status = %q, want skipped", i, r.Status)
		}
	}
}

func TestExtractStudentMessage(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{"no message", "just output\nnothing special", ""},
		{"single message", "output\nSTUDENT_MSG: Check your SSH config\nmore output", "Check your SSH config"},
		{"multiple messages", "STUDENT_MSG: First\nSTUDENT_MSG: Second", "Second"},
		{"empty message", "STUDENT_MSG:", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractStudentMessage(tt.stdout)
			if got != tt.want {
				t.Errorf("extractStudentMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWorkflowExecution_LateActionStillFailsTheWorkflow proves that an action
// result delivered after the workflow's bash process has already exited is
// still applied to the workflow's status, message and reported results.
//
// This is not a rare edge case. run_action writes its action_end event to the
// sidecar socket and the script then ends, so for the LAST action of every
// workflow the `resultCh` and `done` cases of the executor's select are ready
// at essentially the same instant — and Go chooses uniformly at random between
// ready cases. Before the fix the drain path only appended to ActionResults,
// so a failing final check had roughly a coin-flip chance of being recorded
// while its workflow was still reported as a pass. That is a silent grading
// error biased towards leniency, which is the hardest direction to notice.
//
// The event is delivered from the test rather than from the script so the
// timing is deterministic: the script signals via a sentinel file immediately
// before exiting, and the sender then waits well past bash's death, which
// makes the drain the only path the event can take.
func TestWorkflowExecution_LateActionStillFailsTheWorkflow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — requires bash")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sentinel := filepath.Join(t.TempDir(), "script-exited")

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient(server.URL, "tok", logger)

	cfg := &RunnerConfig{
		CallbackURL:   server.URL,
		CallbackToken: "tok",
		RunID:         "run-late",
		Workflows: []WorkflowDef{
			{
				Slug:           "late-action",
				Name:           "Late Action",
				Script:         "#!/bin/bash\ntouch " + sentinel + "\nexit 0",
				TimeoutSeconds: 20,
			},
		},
		Target: TargetConfig{IP: "10.0.0.1"},
	}

	sendErr := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(sentinel); err == nil {
				break
			}
			if time.Now().After(deadline) {
				sendErr <- fmt.Errorf("script never created sentinel %s", sentinel)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		// bash exits microseconds after touching the sentinel; this margin
		// guarantees the executor has already taken the `done` case.
		time.Sleep(300 * time.Millisecond)

		conn, err := net.DialTimeout("unix", DefaultSocketPath, 2*time.Second)
		if err != nil {
			sendErr <- fmt.Errorf("dial sidecar after script exit: %w", err)
			return
		}
		defer conn.Close()
		sendErr <- json.NewEncoder(conn).Encode(ActionEvent{
			Event:      "action_end",
			Action:     "Final Check",
			Status:     "fail",
			Message:    "the last check failed",
			ExitCode:   1,
			DurationMs: 12,
		})
	}()

	executor := NewExecutor(cfg, callback, logger)
	result := executor.RunWorkflow(context.Background(), cfg.Workflows[0])

	if err := <-sendErr; err != nil {
		t.Fatalf("could not deliver the late action event: %v\n"+
			"The sidecar listener must stay open for the duration of the drain; "+
			"cancelling it before draining makes late events unroutable.", err)
	}

	if len(result.ActionResults) != 1 {
		t.Fatalf("ActionResults = %d, want 1 — the drained action was dropped entirely",
			len(result.ActionResults))
	}
	if result.Status != "fail" {
		t.Errorf("Status = %q, want %q.\n"+
			"An action delivered during the post-exit drain must still fail its workflow. "+
			"The last action of every workflow routinely arrives on this path, so dropping "+
			"its status silently marks a failing final check as a pass.",
			result.Status, "fail")
	}
	if result.Message != "the last check failed" {
		t.Errorf("Message = %q, want %q — the drained action's student message must "+
			"reach the workflow result, otherwise the student sees a red check with no "+
			"explanation.", result.Message, "the last check failed")
	}
}