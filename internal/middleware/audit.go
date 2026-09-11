package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
)

// AuditRequests returns middleware that logs mutating API requests (POST/PUT/DELETE/PATCH)
// to the audit log. It also performs debounced session heartbeat updates.
func AuditRequests(db *database.Queries) func(http.Handler) http.Handler {
	heartbeats := &sessionHeartbeats{
		lastTouch: make(map[uuid.UUID]time.Time),
		interval:  60 * time.Second,
	}

	// Background goroutine to deactivate stale sessions every 5 minutes
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			ctx := context.Background()
			if n, err := db.DeactivateStaleSessions(ctx, 30); err != nil {
				slog.Error("audit: failed to deactivate stale sessions", "error", err)
			} else if n > 0 {
				slog.Info("audit: deactivated stale sessions", "count", n)
			}
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Session heartbeat (debounced)
			sessionID := SessionIDFromContext(r.Context())
			if sessionID != uuid.Nil {
				heartbeats.touch(r.Context(), db, sessionID)
			}

			// Wrap response writer to capture status code
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)

			// Only audit mutating requests
			method := r.Method
			if method != http.MethodPost && method != http.MethodPut &&
				method != http.MethodDelete && method != http.MethodPatch {
				return
			}

			duration := time.Since(start)
			audit.Log(r.Context(), db, "api.request",
				audit.IP(r.RemoteAddr),
				audit.Detail("method", method),
				audit.Detail("path", r.URL.Path),
				audit.Detail("status", sw.status),
				audit.Detail("duration_ms", duration.Milliseconds()),
			)
		})
	}
}

// statusWriter wraps http.ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// sessionHeartbeats tracks the last time each session was touched,
// debouncing DB writes to at most once per interval.
type sessionHeartbeats struct {
	mu        sync.Mutex
	lastTouch map[uuid.UUID]time.Time
	interval  time.Duration
}

func (h *sessionHeartbeats) touch(ctx interface{ Done() <-chan struct{} }, db *database.Queries, sessionID uuid.UUID) {
	h.mu.Lock()
	last, ok := h.lastTouch[sessionID]
	now := time.Now()
	if ok && now.Sub(last) < h.interval {
		h.mu.Unlock()
		return
	}
	h.lastTouch[sessionID] = now
	h.mu.Unlock()

	// Fire-and-forget DB update
	go func() {
		ctx := context.Background()
		if err := db.TouchSession(ctx, sessionID); err != nil {
			slog.Warn("audit: session heartbeat failed", "session_id", sessionID, "error", err)
		}
	}()
}
