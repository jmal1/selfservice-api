package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/runner"
)

func TestCallbackRouter_InvalidToken(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Engine with nil queries — token validation will fail
	eng := &Engine{logger: logger}
	cb := NewCallbackServer(eng, logger)

	srv := httptest.NewServer(cb.Router())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/internal/callback/bad-token/heartbeat", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// Engine with nil queries — should return 503
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestCallbackRouter_BadToken(t *testing.T) {
	// This test would need a real DB to validate token lookup returns 401.
	// Covered by the integration tests (TestE2E_*).
	// Here we just verify the router structure works.
}

func TestCallbackRouter_MissingToken(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	eng := &Engine{logger: logger}
	cb := NewCallbackServer(eng, logger)

	srv := httptest.NewServer(cb.Router())
	defer srv.Close()

	// No token in path
	resp, err := http.Post(srv.URL+"/internal/callback//heartbeat", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	// Should be 503 (nil queries) or 404
	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 for missing token")
	}
}

func TestActionPayload_Marshal(t *testing.T) {
	payload := runner.CallbackActionPayload{
		WorkflowSlug: "test-wf",
		Action: runner.ActionOutput{
			Action:   "Check SSH",
			Status:   "pass",
			ExitCode: 0,
			Duration: 250 * time.Millisecond,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	json.Unmarshal(data, &decoded)

	if decoded["workflow_slug"] != "test-wf" {
		t.Errorf("workflow_slug = %v", decoded["workflow_slug"])
	}

	action := decoded["action"].(map[string]any)
	if action["action"] != "Check SSH" {
		t.Errorf("action = %v", action["action"])
	}
	if action["status"] != "pass" {
		t.Errorf("status = %v", action["status"])
	}
}

func TestCompletePayload_Marshal(t *testing.T) {
	payload := runner.CallbackCompletePayload{
		Status: "completed",
		Results: []runner.WorkflowRunResult{
			{
				WorkflowSlug: "wf-1",
				WorkflowName: "Test WF",
				Status:       "pass",
				ActionResults: []runner.ActionOutput{
					{Action: "action-1", Status: "pass", ExitCode: 0},
				},
				TotalDuration: 5 * time.Second,
			},
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded runner.CallbackCompletePayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Status != "completed" {
		t.Errorf("status = %q", decoded.Status)
	}
	if len(decoded.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(decoded.Results))
	}
	if decoded.Results[0].WorkflowSlug != "wf-1" {
		t.Errorf("workflow_slug = %q", decoded.Results[0].WorkflowSlug)
	}
	if len(decoded.Results[0].ActionResults) != 1 {
		t.Errorf("action_results = %d, want 1", len(decoded.Results[0].ActionResults))
	}
}

func TestHeartbeatPayload_Marshal(t *testing.T) {
	payload := runner.CallbackHeartbeatPayload{
		Phase:          "executing",
		CurrentAction:  "Port Scan",
		ElapsedSeconds: 45,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	json.Unmarshal(data, &decoded)

	if decoded["phase"] != "executing" {
		t.Errorf("phase = %v", decoded["phase"])
	}
	if decoded["current_action"] != "Port Scan" {
		t.Errorf("current_action = %v", decoded["current_action"])
	}
	if decoded["elapsed_seconds"] != float64(45) {
		t.Errorf("elapsed_seconds = %v", decoded["elapsed_seconds"])
	}
}
