package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Executor runs workflow scripts as single bash processes with sidecar support.
type Executor struct {
	config   *RunnerConfig
	callback *CallbackClient
	logger   *slog.Logger
}

// NewExecutor creates a workflow executor.
func NewExecutor(config *RunnerConfig, callback *CallbackClient, logger *slog.Logger) *Executor {
	return &Executor{
		config:   config,
		callback: callback,
		logger:   logger,
	}
}

// RunWorkflow executes a single workflow and returns the result.
func (e *Executor) RunWorkflow(ctx context.Context, wf WorkflowDef) WorkflowRunResult {
	result := WorkflowRunResult{
		WorkflowSlug: wf.Slug,
		WorkflowName: wf.Name,
	}
	start := time.Now()
	defer func() { result.TotalDuration = FromDuration(time.Since(start)) }()

	// Create shared working directory
	workdir, err := os.MkdirTemp("", "crucible-*")
	if err != nil {
		result.Status = "error"
		result.Message = "Failed to create working directory"
		e.logger.Error("failed to create workdir", "error", err)
		return result
	}
	defer os.RemoveAll(workdir)

	// Set up paths
	socketPath := DefaultSocketPath
	contextPath := DefaultContextPath

	// Initialize empty context file
	if err := os.WriteFile(contextPath, []byte("{}"), 0644); err != nil {
		result.Status = "error"
		result.Message = "Failed to initialize context file"
		return result
	}

	// Build base environment
	env := e.config.BuildEnv(workdir, socketPath, contextPath)

	// Start sidecar
	sidecar := NewSidecar(socketPath, contextPath, e.callback, e.logger)
	defer sidecar.Close()

	sidecarCtx, sidecarCancel := context.WithCancel(ctx)
	defer sidecarCancel()

	resultCh := make(chan ActionOutput, 100)
	sidecarReady := make(chan struct{})
	go func() {
		close(sidecarReady)
		if err := sidecar.Listen(sidecarCtx, wf.Slug, resultCh); err != nil {
			e.logger.Error("sidecar listener error", "error", err)
		}
	}()
	<-sidecarReady

	// Brief pause to ensure socket is ready
	time.Sleep(50 * time.Millisecond)

	// Run setup script if declared
	if wf.Setup != "" {
		setupOut := e.runSetup(ctx, wf, env, workdir)
		result.SetupOutput = &setupOut
		if setupOut.Status != "pass" {
			result.Status = "error"
			result.Message = "Setup script failed"
			e.logger.Warn("setup script failed",
				"workflow", wf.Slug,
				"exit_code", setupOut.ExitCode,
			)
			return result
		}
	}

	// Start single bash process for entire workflow
	wfCtx, wfCancel := context.WithTimeout(ctx, time.Duration(wf.TimeoutSeconds)*time.Second)
	defer wfCancel()

	cmd := exec.CommandContext(wfCtx, "bash", "-c", wf.Script)
	cmd.Dir = workdir
	cmd.Env = append(env, sidecar.BuildContextEnv()...)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	// Start the bash process
	if err := cmd.Start(); err != nil {
		result.Status = "error"
		result.Message = "Failed to start workflow process"
		e.logger.Error("failed to start workflow", "error", err)
		return result
	}

	e.logger.Info("workflow started",
		"workflow", wf.Slug,
		"timeout", wf.TimeoutSeconds,
	)

	// Collect action results from sidecar as they arrive
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	result.Status = "pass"

	// Handling an action result must be identical whether it arrives while bash
	// is still running or during the post-exit drain below.
	//
	// It previously was not: the drain path only appended to ActionResults, so a
	// failing action that landed there did not set result.Status, produced no
	// student message, and was never reported incrementally to the engine.
	//
	// That is not an edge case. `run_action` writes its action_end event to the
	// sidecar socket and then the script ends, so on the LAST action of every
	// workflow both `resultCh` and `done` are ready at essentially the same
	// moment — and Go's select picks uniformly at random between ready cases.
	// A student's final check therefore had roughly a coin-flip chance of
	// failing without failing its workflow, which is a silent grading error in
	// the direction that is hardest to notice: too lenient.
	handleAction := func(actionOut ActionOutput, propagate bool) {
		if actionOut.Message == "" {
			actionOut.Message = extractStudentMessage(stdoutBuf.String())
		}

		result.ActionResults = append(result.ActionResults, actionOut)

		// Report action incrementally to engine
		e.callback.ReportAction(wf.Slug, actionOut)

		// No CTX_* injection happens here, and none can: the workflow is one
		// bash process (started below with cmd.Env fixed) and a process's
		// environment cannot be modified from outside after it starts. This
		// spot used to carry a comment claiming otherwise with no code under
		// it, which is why cross-action context passing was documented for
		// months while being impossible. run_action refreshes CTX_* from the
		// context file itself — see _ctx_export in deploy/runner/actions.sh.
		// BuildContextEnv below is still the correct seed for the first action.

		if propagate && actionOut.Status != "pass" {
			result.Status = actionOut.Status
			if actionOut.Message != "" {
				result.Message = actionOut.Message
			}
		}
	}

	for {
		select {
		case actionOut := <-resultCh:
			handleAction(actionOut, true)

		case err := <-done:
			// Bash process exited — drain remaining action events.
			//
			// sidecarCancel is deliberately NOT called before the drain: it
			// stops the listener, and any event still in flight on the socket
			// would be lost rather than delivered. The drain's own timeout
			// bounds how long we wait.
			drainTimeout := time.After(2 * time.Second)
		drainLoop:
			for {
				select {
				case actionOut, ok := <-resultCh:
					if !ok {
						break drainLoop
					}
					handleAction(actionOut, true)
				case <-drainTimeout:
					break drainLoop
				}
			}
			sidecarCancel()

			if err != nil && result.Status == "pass" {
				if wfCtx.Err() == context.DeadlineExceeded {
					result.Status = "timeout"
					result.Message = fmt.Sprintf("Workflow timed out after %ds", wf.TimeoutSeconds)
				} else {
					result.Status = "fail"
					if exitErr, ok := err.(*exec.ExitError); ok {
						result.Message = fmt.Sprintf("Workflow exited with code %d", exitErr.ExitCode())
					}
				}
			}

			e.logger.Info("workflow completed",
				"workflow", wf.Slug,
				"status", result.Status,
				"actions", len(result.ActionResults),
				"duration_ms", result.TotalDuration.Duration().Milliseconds(),
			)

			return result
		}
	}
}

// runSetup executes the setup script with a 60-second hard timeout.
func (e *Executor) runSetup(ctx context.Context, wf WorkflowDef, env []string, workdir string) ActionOutput {
	setupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(setupCtx, "bash", "-c", wf.Setup)
	cmd.Dir = workdir
	cmd.Env = env

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	output := ActionOutput{
		Action:   "setup",
		Duration: FromDuration(duration),
	}

	if err != nil {
		output.Status = "fail"
		output.Message = "Setup script failed"
		if exitErr, ok := err.(*exec.ExitError); ok {
			output.ExitCode = exitErr.ExitCode()
		}
		if setupCtx.Err() == context.DeadlineExceeded {
			output.Status = "timeout"
			output.Message = "Setup script timed out after 60s"
		}
		e.logger.Warn("setup script output",
			"stdout", stdoutBuf.String(),
			"stderr", stderrBuf.String(),
		)
	} else {
		output.Status = "pass"
	}

	return output
}

// RunAll executes all workflows sequentially and returns all results.
func (e *Executor) RunAll(ctx context.Context) ([]WorkflowRunResult, error) {
	var results []WorkflowRunResult

	for i, wf := range e.config.Workflows {
		if ctx.Err() != nil {
			// Context cancelled — mark remaining as skipped
			for j := i; j < len(e.config.Workflows); j++ {
				results = append(results, WorkflowRunResult{
					WorkflowSlug: e.config.Workflows[j].Slug,
					WorkflowName: e.config.Workflows[j].Name,
					Status:       "skipped",
					Message:      "Run cancelled",
				})
			}
			break
		}

		e.logger.Info("executing workflow",
			"index", i+1,
			"total", len(e.config.Workflows),
			"workflow", wf.Slug,
		)

		result := e.RunWorkflow(ctx, wf)
		results = append(results, result)

		// Report workflow result to engine
		e.callback.ReportWorkflow(result)
	}

	return results, nil
}

// extractStudentMessage parses STUDENT_MSG: lines from stdout.
// Returns the last STUDENT_MSG found, or empty string.
func extractStudentMessage(stdout string) string {
	var lastMsg string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "STUDENT_MSG:") {
			lastMsg = strings.TrimSpace(strings.TrimPrefix(line, "STUDENT_MSG:"))
		}
	}
	return lastMsg
}
