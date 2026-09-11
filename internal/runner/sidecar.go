package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Sidecar listens on a Unix socket for action events from the bash workflow
// process and manages context file snapshotting and CTX_* env var injection.
type Sidecar struct {
	socketPath  string
	contextPath string
	logger      *slog.Logger
	callback    *CallbackClient

	mu             sync.Mutex
	currentAction  string
	contextData    map[string]string
	listener       net.Listener
}

// NewSidecar creates a sidecar that listens for action events.
func NewSidecar(socketPath, contextPath string, callback *CallbackClient, logger *slog.Logger) *Sidecar {
	return &Sidecar{
		socketPath:  socketPath,
		contextPath: contextPath,
		logger:      logger,
		callback:    callback,
		contextData: make(map[string]string),
	}
}

// CurrentAction returns the name of the currently executing action.
func (s *Sidecar) CurrentAction() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentAction
}

// ContextSnapshot returns the current context data as JSON.
func (s *Sidecar) ContextSnapshot() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(s.contextData)
	if err != nil {
		return json.RawMessage("{}")
	}
	return data
}

// BuildContextEnv returns CTX_* environment variables derived from the context file.
// Values are sanitized to prevent shell injection.
func (s *Sidecar) BuildContextEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var envVars []string
	for key, value := range s.contextData {
		sanitized := sanitizeForEnv(value)
		envKey := "CTX_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
		envVars = append(envVars, envKey+"="+sanitized)
	}
	return envVars
}

// Listen starts the Unix socket listener and processes action events.
// It sends ActionOutput results on resultCh for each completed action.
// The caller should cancel ctx to stop listening.
func (s *Sidecar) Listen(ctx context.Context, workflowSlug string, resultCh chan<- ActionOutput) error {
	// Remove stale socket file
	os.Remove(s.socketPath)

	var err error
	s.listener, err = net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("create unix socket: %w", err)
	}

	// Make socket world-writable so bash process (possibly different user) can connect
	if err := os.Chmod(s.socketPath, 0666); err != nil {
		s.logger.Warn("failed to chmod socket", "error", err)
	}

	s.logger.Info("sidecar listening", "socket", s.socketPath)

	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled, clean shutdown
			}
			s.logger.Debug("socket accept error", "error", err)
			continue
		}

		go s.handleConnection(conn, workflowSlug, resultCh)
	}
}

func (s *Sidecar) handleConnection(conn net.Conn, workflowSlug string, resultCh chan<- ActionOutput) {
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var event ActionEvent
	if err := json.NewDecoder(conn).Decode(&event); err != nil {
		s.logger.Debug("failed to decode action event", "error", err)
		return
	}

	switch event.Event {
	case "action_start":
		s.mu.Lock()
		s.currentAction = event.Action
		s.mu.Unlock()

		s.logger.Info("action started", "action", event.Action)
		s.callback.ReportActionStart(workflowSlug, event.Action)

	case "action_end":
		s.logger.Info("action completed",
			"action", event.Action,
			"status", event.Status,
			"exit_code", event.ExitCode,
			"duration_ms", event.DurationMs,
		)

		// Snapshot context file
		ctxData := s.snapshotContext()

		s.mu.Lock()
		s.currentAction = ""
		s.mu.Unlock()

		resultCh <- ActionOutput{
			Action:   event.Action,
			Status:   event.Status,
			Message:  event.Message,
			ExitCode: event.ExitCode,
			Duration: FromDuration(time.Duration(event.DurationMs) * time.Millisecond),
			Context:  ctxData,
		}

	default:
		s.logger.Warn("unknown event type", "event", event.Event)
	}
}

// snapshotContext reads the context JSON file and updates internal state.
func (s *Sidecar) snapshotContext() json.RawMessage {
	data, err := os.ReadFile(s.contextPath)
	if err != nil {
		s.logger.Debug("failed to read context file", "error", err)
		return json.RawMessage("{}")
	}

	var ctx map[string]string
	if err := json.Unmarshal(data, &ctx); err != nil {
		s.logger.Debug("failed to parse context file", "error", err)
		return data
	}

	s.mu.Lock()
	s.contextData = ctx
	s.mu.Unlock()

	return data
}

// Close cleans up the socket file.
func (s *Sidecar) Close() {
	if s.listener != nil {
		s.listener.Close()
	}
	os.Remove(s.socketPath)
}

// sanitizeForEnv strips characters that could cause shell injection or env var corruption.
func sanitizeForEnv(value string) string {
	// Strip NUL bytes
	value = strings.ReplaceAll(value, "\x00", "")

	// Replace newlines with spaces (env vars are single-line)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")

	// Truncate to prevent env var size overflow
	const maxLen = 4096
	if len(value) > maxLen {
		value = value[:maxLen]
	}

	return value
}
