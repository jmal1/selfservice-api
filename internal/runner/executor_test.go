package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
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
