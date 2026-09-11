package runner

import (
	"encoding/json"
)

// RunnerConfig is the JSON configuration loaded from /opt/crucible/runner-config.json.
// The engine creates this as a K8s Secret and mounts it into the runner pod.
type RunnerConfig struct {
	CallbackURL   string       `json:"callback_url"`
	CallbackToken string       `json:"callback_token"`
	RunID         string       `json:"run_id"`
	Workflows     []WorkflowDef `json:"workflows"`
	Target        TargetConfig `json:"target"`
	Pod           PodConfig    `json:"pod"`

	// ActionLibrary is a bash file of function definitions generated from the
	// library actions in the database. The runner writes it to
	// ActionLibraryPath at startup and actions.sh sources it, which is what
	// makes `run_action "..." http_get ...` resolve. Without it every library
	// action fails with exit 127.
	ActionLibrary string `json:"action_library,omitempty"`
}

// WorkflowDef describes a single workflow to execute.
type WorkflowDef struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Script         string `json:"script"`
	Setup          string `json:"setup,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// TargetConfig holds the primary target VM information.
type TargetConfig struct {
	IP       string `json:"ip"`
	OS       string `json:"os"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// PodConfig holds pod-level metadata.
type PodConfig struct {
	Subnet string `json:"subnet"`
	Index  int    `json:"index"`
}

// ActionEvent is the JSON message received on the Unix socket from run_action in actions.sh.
//
// Message carries the student-facing explanation with the event rather than
// leaving the executor to scrape it out of the workflow's stdout. The two
// travel over independent channels — the Unix socket and the process's stdout
// pipe — and run_action necessarily sends the event before it echoes the
// output, so a stdout-only harvest loses the message whenever the Go reader has
// not yet drained the pipe. That race is invisible in tests (which write both
// synchronously) and shows up in production as an action that fails with no
// explanation at all.
type ActionEvent struct {
	Event      string `json:"event"`             // action_start, action_end
	Action     string `json:"action"`
	Status     string `json:"status"`            // pass, fail (only on action_end)
	ExitCode   int    `json:"exit_code"`         // (only on action_end)
	DurationMs int    `json:"duration_ms"`       // (only on action_end)
	Message    string `json:"message,omitempty"` // student-safe message (only on action_end)
}

// ActionOutput is the structured result of a single action execution.
type ActionOutput struct {
	Action   string          `json:"action"`
	Status   string          `json:"status"`    // pass, fail, error, timeout, skipped
	Message  string          `json:"message"`   // student-safe message
	ExitCode int             `json:"exit_code"`
	Duration Millis          `json:"duration_ms"`
	Context  json.RawMessage `json:"context,omitempty"`
}

// WorkflowRunResult is the complete result of executing one workflow.
type WorkflowRunResult struct {
	WorkflowSlug  string         `json:"workflow_slug"`
	WorkflowName  string         `json:"workflow_name"`
	Status        string         `json:"status"`  // pass, fail, error, timeout
	Message       string         `json:"message"` // last student message
	ActionResults []ActionOutput `json:"action_results"`
	SetupOutput   *ActionOutput  `json:"setup_output,omitempty"`
	TotalDuration Millis         `json:"total_duration_ms"`
}

// CallbackActionPayload is POSTed to the engine for each action result.
type CallbackActionPayload struct {
	WorkflowSlug string       `json:"workflow_slug"`
	Action       ActionOutput `json:"action"`
}

// CallbackWorkflowPayload is POSTed to the engine when a workflow completes.
type CallbackWorkflowPayload struct {
	Result WorkflowRunResult `json:"result"`
}

// CallbackCompletePayload is POSTed when the entire run finishes.
type CallbackCompletePayload struct {
	Status  string              `json:"status"` // completed, failed
	Results []WorkflowRunResult `json:"results"`
}

// CallbackHeartbeatPayload is POSTed periodically to prove liveness.
type CallbackHeartbeatPayload struct {
	Phase          string `json:"phase"`
	CurrentAction  string `json:"current_action,omitempty"`
	ElapsedSeconds int    `json:"elapsed_seconds"`
}
