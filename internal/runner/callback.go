package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// CallbackClient sends incremental results and heartbeats to the crucible-engine.
type CallbackClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
	logger     *slog.Logger
	maxRetries int
}

// NewCallbackClient creates a callback client for reporting to the engine.
func NewCallbackClient(callbackURL, token string, logger *slog.Logger) *CallbackClient {
	return &CallbackClient{
		baseURL: callbackURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger:     logger,
		maxRetries: 3,
	}
}

// ReportActionStart notifies the engine that an action has begun.
func (c *CallbackClient) ReportActionStart(workflowSlug, actionName string) {
	payload := map[string]string{
		"workflow_slug": workflowSlug,
		"action":        actionName,
		"event":         "action_start",
	}
	if err := c.post("/action", payload); err != nil {
		c.logger.Warn("failed to report action start", "action", actionName, "error", err)
	}
}

// ReportAction sends a completed action result to the engine.
func (c *CallbackClient) ReportAction(workflowSlug string, action ActionOutput) {
	payload := CallbackActionPayload{
		WorkflowSlug: workflowSlug,
		Action:       action,
	}
	if err := c.post("/action", payload); err != nil {
		c.logger.Warn("failed to report action", "action", action.Action, "error", err)
	}
}

// ReportWorkflow sends a completed workflow result to the engine.
func (c *CallbackClient) ReportWorkflow(result WorkflowRunResult) {
	payload := CallbackWorkflowPayload{Result: result}
	if err := c.post("/workflow", payload); err != nil {
		c.logger.Warn("failed to report workflow", "workflow", result.WorkflowSlug, "error", err)
	}
}

// ReportComplete sends the final run completion signal with all results.
func (c *CallbackClient) ReportComplete(status string, results []WorkflowRunResult) error {
	payload := CallbackCompletePayload{
		Status:  status,
		Results: results,
	}
	return c.post("/complete", payload)
}

// SendHeartbeat reports liveness to the engine.
func (c *CallbackClient) SendHeartbeat(phase, currentAction string, elapsedSeconds int) {
	payload := CallbackHeartbeatPayload{
		Phase:          phase,
		CurrentAction:  currentAction,
		ElapsedSeconds: elapsedSeconds,
	}
	if err := c.post("/heartbeat", payload); err != nil {
		c.logger.Warn("failed to send heartbeat", "error", err)
	}
}

// post sends a JSON POST to the callback endpoint with retry and backoff.
func (c *CallbackClient) post(path string, payload any) error {
	url := fmt.Sprintf("%s/internal/callback/%s%s", c.baseURL, c.token, path)

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			time.Sleep(backoff)
			c.logger.Debug("retrying callback", "attempt", attempt, "path", path)
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("HTTP request failed: %w", err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}

		lastErr = fmt.Errorf("callback returned %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// Client error — don't retry
			return lastErr
		}
	}

	return fmt.Errorf("callback failed after %d retries: %w", c.maxRetries, lastErr)
}
