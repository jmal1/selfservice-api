package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnixSocket_ActionEvents(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	contextPath := filepath.Join(dir, "context.json")

	// Initialize context file
	os.WriteFile(contextPath, []byte("{}"), 0644)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient("http://localhost:9999", "tok", logger)
	sidecar := NewSidecar(socketPath, contextPath, callback, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan ActionOutput, 10)

	// Start listener in background
	go func() {
		sidecar.Listen(ctx, "test-wf", resultCh)
	}()

	// Wait for socket to be ready
	time.Sleep(100 * time.Millisecond)

	// Send action_start event
	sendEvent(t, socketPath, ActionEvent{
		Event:  "action_start",
		Action: "Test Action",
	})

	time.Sleep(50 * time.Millisecond)
	if got := sidecar.CurrentAction(); got != "Test Action" {
		t.Errorf("CurrentAction() = %q, want %q", got, "Test Action")
	}

	// Send action_end event
	sendEvent(t, socketPath, ActionEvent{
		Event:      "action_end",
		Action:     "Test Action",
		Status:     "pass",
		ExitCode:   0,
		DurationMs: 250,
	})

	// Read result
	select {
	case result := <-resultCh:
		if result.Action != "Test Action" {
			t.Errorf("Action = %q, want %q", result.Action, "Test Action")
		}
		if result.Status != "pass" {
			t.Errorf("Status = %q, want %q", result.Status, "pass")
		}
		if result.ExitCode != 0 {
			t.Errorf("ExitCode = %d, want 0", result.ExitCode)
		}
		if result.Duration.Duration() != 250*time.Millisecond {
			t.Errorf("Duration = %v, want %v", result.Duration, 250*time.Millisecond)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for action result")
	}

	// Verify current action cleared
	if got := sidecar.CurrentAction(); got != "" {
		t.Errorf("CurrentAction() after end = %q, want empty", got)
	}

	cancel()
	sidecar.Close()
}

func TestUnixSocket_ActionEndPropagatesStudentMessage(t *testing.T) {
	// run_action puts the student-facing message on the action_end event
	// precisely so the executor does not have to scrape it out of the workflow's
	// stdout — the socket and the stdout pipe are independent channels and the
	// event is sent first, so the scrape loses the race. If the sidecar drops
	// the field on the way through, that whole fix is inert and every failing
	// library action reports a bare red X with no reason.
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	contextPath := filepath.Join(dir, "context.json")
	os.WriteFile(contextPath, []byte("{}"), 0644)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient("http://localhost:9999", "tok", logger)
	sidecar := NewSidecar(socketPath, contextPath, callback, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan ActionOutput, 10)
	go func() { sidecar.Listen(ctx, "test-wf", resultCh) }()
	time.Sleep(100 * time.Millisecond)

	const msg = `Web server returned 000 instead of 200 for http://10.100.19.10:80`
	sendEvent(t, socketPath, ActionEvent{
		Event:      "action_end",
		Action:     "HTTP Responds",
		Status:     "fail",
		ExitCode:   1,
		DurationMs: 28,
		Message:    msg,
	})

	select {
	case result := <-resultCh:
		if result.Message != msg {
			t.Errorf("Message = %q, want %q — the student sees a failed check with no explanation",
				result.Message, msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for action result")
	}

	cancel()
	sidecar.Close()
}

func TestContextSnapshot(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "test.sock")
	contextPath := filepath.Join(dir, "context.json")

	// Write context with data
	ctx_data := map[string]string{
		"check_ssh.status": "0",
		"check_ssh.body":   "SSH running",
	}
	data, _ := json.Marshal(ctx_data)
	os.WriteFile(contextPath, data, 0644)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	callback := NewCallbackClient("http://localhost:9999", "tok", logger)
	sidecar := NewSidecar(socketPath, contextPath, callback, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resultCh := make(chan ActionOutput, 10)
	go func() {
		sidecar.Listen(ctx, "test-wf", resultCh)
	}()
	time.Sleep(100 * time.Millisecond)

	// Send action_end to trigger snapshot
	sendEvent(t, socketPath, ActionEvent{
		Event:      "action_end",
		Action:     "Check SSH",
		Status:     "pass",
		ExitCode:   0,
		DurationMs: 100,
	})

	select {
	case result := <-resultCh:
		if result.Context == nil {
			t.Fatal("expected non-nil context snapshot")
		}
		var snapshot map[string]string
		if err := json.Unmarshal(result.Context, &snapshot); err != nil {
			t.Fatalf("unmarshal context: %v", err)
		}
		if snapshot["check_ssh.status"] != "0" {
			t.Errorf("context status = %q, want %q", snapshot["check_ssh.status"], "0")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for action result")
	}

	// Verify BuildContextEnv
	envVars := sidecar.BuildContextEnv()
	envMap := make(map[string]string)
	for _, e := range envVars {
		parts := splitFirst(e, "=")
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if envMap["CTX_CHECK_SSH_STATUS"] != "0" {
		t.Errorf("CTX_CHECK_SSH_STATUS = %q, want %q", envMap["CTX_CHECK_SSH_STATUS"], "0")
	}
	if envMap["CTX_CHECK_SSH_BODY"] != "SSH running" {
		t.Errorf("CTX_CHECK_SSH_BODY = %q, want %q", envMap["CTX_CHECK_SSH_BODY"], "SSH running")
	}

	cancel()
	sidecar.Close()
}

func TestSanitizeForEnv(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"clean", "hello world", "hello world"},
		{"nul bytes", "hello\x00world", "helloworld"},
		{"newlines", "line1\nline2\r\nline3", "line1 line2  line3"},
		{"long string", strings.Repeat("a", 5000), strings.Repeat("a", 4096)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeForEnv(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeForEnv(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// sendEvent connects to the Unix socket and sends a JSON event.
func sendEvent(t *testing.T, socketPath string, event ActionEvent) {
	t.Helper()

	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		t.Fatalf("connect to socket: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(event); err != nil {
		t.Fatalf("send event: %v", err)
	}
}
