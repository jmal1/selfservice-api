// Package handlers.
//
// run_progress_ws.go exposes a WebSocket that streams workflow-run progress
// events to the admin run-detail page. It replaces the page's 2-second poll
// loop on GET /api/v1/admin/runs/{runID}.
//
// Wire protocol (JSON over text WS frames):
//
//	{"type":"running","ts":"2026-06-07T08:15:00Z","message":"Executing 5 workflows"}
//	{"type":"workflow_start","ts":...,"message":"check-uptime: vmware_tools dispatch"}
//	{"type":"action_complete","ts":...,"message":"check-uptime: nginx-running (pass)"}
//	{"type":"workflow_complete","ts":...,"message":"check-uptime: pass"}
//	{"type":"completed","ts":...,"message":"Run completed — 5 workflows"}
//	{"type":"failed","ts":...,"message":"<reason>"}
//
// The server-side filter on subject `testing.runs.<podID>.progress` is naturally
// per-pod; the handler additionally filters on run_id so a single pod with many
// concurrent runs streams cleanly. The WS closes when the run reaches a terminal
// status, when the client disconnects, or when the connection idles for
// readDeadline.
package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"

	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// runProgressFrame is what we send over the wire. Mirrors the engine's
// publishRunEvent shape but tagged with a timestamp so the UI can sort/dedupe.
type runProgressFrame struct {
	Type    string    `json:"type"`
	RunID   string    `json:"run_id"`
	PodID   string    `json:"pod_id"`
	Message string    `json:"message,omitempty"`
	TS      time.Time `json:"ts"`
}

// Terminal event types that should cause the WS to close after the frame is
// delivered. Keeping this as a const map lets us add cancelled/etc later
// without touching the dispatch loop.
var terminalEventTypes = map[string]struct{}{
	"completed": {},
	"failed":    {},
	"cancelled": {},
	"timeout":   {},
}

// RunProgressWS upgrades the connection to WebSocket and streams progress
// frames for the requested run until it reaches a terminal status.
//
// Auth: same session-cookie auth as the rest of /api/v1. We additionally
// check that the caller owns the pod that produced the run OR is an admin
// — non-owners receive 403 BEFORE the upgrade.
func (h *Handler) RunProgressWS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := middleware.UserIDFromContext(ctx)
	role := middleware.RoleFromContext(ctx)

	runIDStr := chi.URLParam(r, "runID")
	runID, err := uuid.Parse(runIDStr)
	if err != nil {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	// Load the run so we can (a) authorize and (b) bail out early if it's
	// already terminal — saves the client a round-trip when the page loads
	// after the run finished.
	run, err := h.db.GetRun(ctx, runID)
	if err != nil {
		h.logger.Error("run progress ws: get run failed", "run_id", runID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if run == nil {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}

	// Authorize: owner of the pod or admin
	pod, err := h.db.GetPodByID(ctx, run.PodID)
	if err != nil || pod == nil {
		http.Error(w, "pod not found", http.StatusNotFound)
		return
	}
	if pod.OwnerID != userID && role != models.RoleAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Configure origin check using allowed origins from config (same pattern
	// as VMConsoleWS).
	upgrader := wsUpgrader
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		for _, allowed := range h.allowedOrigins {
			if origin == allowed {
				return true
			}
		}
		return false
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the error response
		return
	}
	defer conn.Close()

	// Send an initial snapshot frame so the UI has something to render
	// immediately even if the run is between events.
	snapshot := runProgressFrame{
		Type:    "snapshot",
		RunID:   run.ID.String(),
		PodID:   run.PodID.String(),
		Message: "status=" + run.Status,
		TS:      time.Now().UTC(),
	}
	if err := conn.WriteJSON(snapshot); err != nil {
		return
	}

	// If the run is already terminal, send a final frame and close. The UI
	// will fall back to GET /admin/runs/{id} for the full result body.
	if isTerminalRunStatus(run.Status) {
		_ = conn.WriteJSON(runProgressFrame{
			Type:    run.Status,
			RunID:   run.ID.String(),
			PodID:   run.PodID.String(),
			Message: "run already terminal",
			TS:      time.Now().UTC(),
		})
		return
	}

	// Subscribe to NATS. Subject pattern matches engine.publishRunEvent:
	//   testing.runs.<podID>.progress
	subject := "testing.runs." + run.PodID.String() + ".progress"
	frames := make(chan runProgressFrame, 32)
	doneCtx, cancelSub := context.WithCancel(r.Context())
	defer cancelSub()

	sub, err := h.events.SubscribeRawWithMsg(subject, func(evt events.Event, _ *nats.Msg) {
		// Filter to events for THIS run. The engine emits subject-per-pod
		// (not per-run) because the runID is in the payload, so we have to
		// match here.
		if evt.JobID != run.ID.String() {
			return
		}
		frame := runProgressFrame{
			Type:    evt.Type,
			RunID:   evt.JobID,
			PodID:   evt.PodID,
			Message: evt.Message,
			TS:      time.Now().UTC(),
		}
		// Non-blocking send so a slow client doesn't wedge the NATS handler.
		select {
		case frames <- frame:
		default:
			h.logger.Warn("run progress ws: dropping frame, client too slow",
				"run_id", run.ID, "type", evt.Type)
		}
	})
	if err != nil {
		h.logger.Error("run progress ws: subscribe failed", "subject", subject, "error", err)
		_ = conn.WriteJSON(runProgressFrame{
			Type:    "error",
			RunID:   run.ID.String(),
			Message: "subscribe failed",
			TS:      time.Now().UTC(),
		})
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	// Spawn a reader goroutine to detect client disconnects. We don't care
	// about the data — anything from the client (or EOF) triggers shutdown.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		conn.SetReadLimit(1024) // we don't expect input; small limit
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Ping every 25s to keep the WS alive across proxies that idle-close
	// after 30-60s. Also bounds how long we'd hold a stale connection if
	// NATS is silent (e.g. engine crashed mid-run).
	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()

	// Hard ceiling. A workflow run shouldn't take more than 30 min; if it
	// does the watchdog will fail it and we'll see a terminal frame, but
	// belt-and-suspenders ensures we never leak a connection forever.
	maxLifetime := time.NewTimer(45 * time.Minute)
	defer maxLifetime.Stop()

	for {
		select {
		case <-doneCtx.Done():
			return
		case <-clientGone:
			return
		case <-maxLifetime.C:
			_ = conn.WriteJSON(runProgressFrame{
				Type:    "timeout",
				RunID:   run.ID.String(),
				Message: "ws lifetime exceeded; reconnect to continue",
				TS:      time.Now().UTC(),
			})
			return
		case <-pingTicker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case frame := <-frames:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(frame); err != nil {
				return
			}
			if _, terminal := terminalEventTypes[frame.Type]; terminal {
				return
			}
		}
	}
}

// isTerminalRunStatus tells us whether the run is already done (so we can
// short-circuit the WS) — mirrors models.RunStatus* values.
func isTerminalRunStatus(s string) bool {
	switch s {
	case models.RunStatusCompleted, models.RunStatusFailed,
		models.RunStatusCancelled, models.RunStatusTimeout:
		return true
	default:
		return false
	}
}

// Ensure the JSON encoding stays in scope (silences unused-import linter when
// the rest of the file changes; cheap and harmless).
var _ = json.Marshal
