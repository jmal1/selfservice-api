package runner

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCallbackClient_ReportAction(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(body, &payload)

		mu.Lock()
		received = append(received, payload)
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := NewCallbackClient(server.URL, "test-token", logger)

	action := ActionOutput{
		Action:   "Test Action",
		Status:   "pass",
		ExitCode: 0,
		Duration: FromDuration(500 * time.Millisecond),
	}
	client.ReportAction("test-workflow", action)

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("expected 1 request, got %d", len(received))
	}

	payload := received[0]
	if payload["workflow_slug"] != "test-workflow" {
		t.Errorf("workflow_slug = %v, want test-workflow", payload["workflow_slug"])
	}
}

func TestCallbackClient_ReportComplete(t *testing.T) {
	var receivedPath string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := NewCallbackClient(server.URL, "my-token", logger)

	err := client.ReportComplete("completed", nil)
	if err != nil {
		t.Fatalf("ReportComplete: %v", err)
	}

	expected := "/internal/callback/my-token/complete"
	if receivedPath != expected {
		t.Errorf("path = %q, want %q", receivedPath, expected)
	}
}

func TestCallbackClient_RetryOnServerError(t *testing.T) {
	var attempts int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()

		if current <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := NewCallbackClient(server.URL, "tok", logger)

	err := client.ReportComplete("completed", nil)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestCallbackClient_NoRetryOnClientError(t *testing.T) {
	var attempts int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := NewCallbackClient(server.URL, "tok", logger)

	err := client.ReportComplete("completed", nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retry on 4xx), got %d", attempts)
	}
}

func TestCallbackClient_SendHeartbeat(t *testing.T) {
	var receivedBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	client := NewCallbackClient(server.URL, "tok", logger)

	client.SendHeartbeat("executing", "Check SSH", 45)

	if receivedBody["phase"] != "executing" {
		t.Errorf("phase = %v, want executing", receivedBody["phase"])
	}
	if receivedBody["current_action"] != "Check SSH" {
		t.Errorf("current_action = %v, want Check SSH", receivedBody["current_action"])
	}
}
