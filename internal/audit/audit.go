// Package audit provides a structured, extensible audit logging framework.
//
// Usage:
//
//	audit.Log(ctx, db, "pod.delete",
//	    audit.Resource("pod", podID),
//	    audit.Detail("name", pod.Name),
//	)
//
// The helper extracts user_id and client IP from the request context
// (set by auth and RealIP middleware).
package audit

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

// Option configures an audit log entry.
type Option func(*entry)

type entry struct {
	resourceType *string
	resourceID   *uuid.UUID
	details      map[string]any
	ipAddress    string
	userID       *uuid.UUID
}

// Resource sets the resource type and ID for the audit entry.
func Resource(resourceType string, resourceID uuid.UUID) Option {
	return func(e *entry) {
		e.resourceType = &resourceType
		e.resourceID = &resourceID
	}
}

// Detail adds a key-value pair to the audit entry's details JSON.
func Detail(key string, value any) Option {
	return func(e *entry) {
		if e.details == nil {
			e.details = make(map[string]any)
		}
		e.details[key] = value
	}
}

// IP overrides the client IP (useful when context doesn't have it).
func IP(ip string) Option {
	return func(e *entry) {
		e.ipAddress = ip
	}
}

// User overrides the user ID (useful for unauthenticated actions like login_failed).
func User(userID uuid.UUID) Option {
	return func(e *entry) {
		e.userID = &userID
	}
}

// Log records an audit event. It extracts user_id and IP from context
// (set by auth and RealIP middleware) unless overridden via options.
//
// Action naming convention: {resource}.{verb}
//
//	auth.login, auth.logout, auth.login_failed
//	pod.create, pod.delete
//	vm.add, vm.delete, vm.start, vm.stop, vm.restart
//	console.open, console.close
//	admin.template.create, admin.template.update
//	api.request (middleware-generated)
func Log(ctx context.Context, db *database.Queries, action string, opts ...Option) {
	e := &entry{}
	for _, opt := range opts {
		opt(e)
	}

	// Extract user from context if not overridden
	if e.userID == nil {
		if uid := UserIDFromContext(ctx); uid != uuid.Nil {
			e.userID = &uid
		}
	}

	// Extract IP from context if not overridden
	if e.ipAddress == "" {
		e.ipAddress = ClientIPFromContext(ctx)
	}

	var detailsJSON []byte
	if len(e.details) > 0 {
		var err error
		detailsJSON, err = json.Marshal(e.details)
		if err != nil {
			slog.Warn("audit: failed to marshal details", "action", action, "error", err)
		}
	}

	var ipPtr *string
	if e.ipAddress != "" {
		ipPtr = &e.ipAddress
	}

	err := db.InsertAuditLog(ctx, models.AuditLog{
		UserID:       e.userID,
		Action:       action,
		ResourceType: e.resourceType,
		ResourceID:   e.resourceID,
		Details:      detailsJSON,
		IPAddress:    ipPtr,
	})
	if err != nil {
		slog.Error("audit: failed to write log", "action", action, "error", err)
	}
}

// FromRequest is a convenience that creates options from an http.Request.
func FromRequest(r *http.Request) Option {
	return IP(normalizeRemoteAddr(r.RemoteAddr))
}

func normalizeRemoteAddr(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	if ip := net.ParseIP(strings.Trim(remoteAddr, "[]")); ip != nil {
		return ip.String()
	}
	return remoteAddr
}

// contextKey is unexported to prevent collisions.
type contextKey string

const clientIPKey contextKey = "audit.clientIP"
const userIDKey contextKey = "audit.userID"

// WithClientIP stores the real client IP in context.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// ClientIPFromContext retrieves the real client IP from context.
func ClientIPFromContext(ctx context.Context) string {
	if ip, ok := ctx.Value(clientIPKey).(string); ok {
		return ip
	}
	return ""
}

// WithUserID stores the user ID in context for audit extraction.
func WithUserID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// UserIDFromContext retrieves the user ID from audit context.
func UserIDFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(userIDKey).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}
