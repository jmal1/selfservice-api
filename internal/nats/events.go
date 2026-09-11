package events

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/jmal1/selfservice-api/internal/config"
)

// Client wraps the NATS connection for publishing and subscribing to events.
type Client struct {
	conn   *nats.Conn
	logger *slog.Logger
}

// Event represents a message published to NATS.
type Event struct {
	Type      string `json:"type"`
	JobID     string `json:"job_id,omitempty"`
	PodID     string `json:"pod_id,omitempty"`
	VMID      string `json:"vm_id,omitempty"`
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	Step      int    `json:"step,omitempty"`
	TotalSteps int   `json:"total_steps,omitempty"`
}

// NATS subjects.
const (
	SubjectJobCreated  = "jobs.created"
	SubjectJobStatus   = "jobs.%s.status"  // jobs.<id>.status
	SubjectJobLog      = "jobs.%s.log"     // jobs.<id>.log
)

// NewClient connects to NATS and returns a Client.
func NewClient(cfg config.NATSConfig, logger *slog.Logger) (*Client, error) {
	conn, err := nats.Connect(cfg.URL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			logger.Warn("NATS disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			logger.Info("NATS reconnected", "url", nc.ConnectedUrl())
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to NATS: %w", err)
	}

	logger.Info("connected to NATS", "url", cfg.URL)
	return &Client{conn: conn, logger: logger}, nil
}

// PublishJobCreated notifies workers that a new job is available.
func (c *Client) PublishJobCreated(jobID uuid.UUID, jobType string) error {
	evt := Event{
		Type:  "job.created",
		JobID: jobID.String(),
		Status: jobType,
	}
	return c.publish(SubjectJobCreated, evt)
}

// PublishJobStatus publishes a job status update.
func (c *Client) PublishJobStatus(jobID uuid.UUID, status, message string) error {
	evt := Event{
		Type:    "job.status",
		JobID:   jobID.String(),
		Status:  status,
		Message: message,
	}
	return c.publish(fmt.Sprintf(SubjectJobStatus, jobID), evt)
}

// PublishJobProgress publishes a step progress update for a job.
func (c *Client) PublishJobProgress(jobID uuid.UUID, step, totalSteps int, message string) error {
	evt := Event{
		Type:       "job.progress",
		JobID:      jobID.String(),
		Step:       step,
		TotalSteps: totalSteps,
		Message:    message,
	}
	return c.publish(fmt.Sprintf(SubjectJobLog, jobID), evt)
}

// SubscribeJobCreated subscribes to new job notifications.
func (c *Client) SubscribeJobCreated(handler func(jobID string, jobType string)) (*nats.Subscription, error) {
	return c.conn.Subscribe(SubjectJobCreated, func(msg *nats.Msg) {
		var evt Event
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			c.logger.Error("failed to unmarshal job created event", "error", err)
			return
		}
		handler(evt.JobID, evt.Status)
	})
}

// Close cleanly disconnects from NATS.
func (c *Client) Close() {
	c.conn.Drain()
}

// IsConnected reports whether the underlying nats.Conn is in CONNECTED state.
// Used by /admin/health to report broker reachability without doing a round-trip.
func (c *Client) IsConnected() bool {
	return c.conn != nil && c.conn.IsConnected()
}

// ConnectedURL returns the NATS server URL the client is currently
// connected to (e.g. nats://selfservice-nats:4222), or "" if disconnected.
// Used in /admin/health diagnostic payloads.
func (c *Client) ConnectedURL() string {
	if c.conn == nil {
		return ""
	}
	return c.conn.ConnectedUrl()
}

func (c *Client) publish(subject string, evt Event) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := c.conn.Publish(subject, data); err != nil {
		return fmt.Errorf("publish to %s: %w", subject, err)
	}
	return nil
}

// PublishRaw publishes an Event to an arbitrary subject.
// Used by crucible-engine for testing.runs.* subjects.
func (c *Client) PublishRaw(subject string, evt Event) error {
	return c.publish(subject, evt)
}

// SubscribeRaw subscribes to an arbitrary subject and calls handler with decoded events.
func (c *Client) SubscribeRaw(subject string, handler func(evt Event)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subject, func(msg *nats.Msg) {
		var evt Event
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			c.logger.Error("failed to unmarshal event", "subject", subject, "error", err)
			return
		}
		handler(evt)
	})
}

// SubscribeRawWithMsg is a variant of SubscribeRaw that also surfaces the
// raw *nats.Msg to the handler — useful when the consumer needs subject,
// reply, or headers in addition to the decoded Event (e.g. the run-progress
// WS handler routes by subject suffix).
func (c *Client) SubscribeRawWithMsg(subject string, handler func(evt Event, msg *nats.Msg)) (*nats.Subscription, error) {
	return c.conn.Subscribe(subject, func(msg *nats.Msg) {
		var evt Event
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			c.logger.Error("failed to unmarshal event", "subject", subject, "error", err)
			return
		}
		handler(evt, msg)
	})
}
