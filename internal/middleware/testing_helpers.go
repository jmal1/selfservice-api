package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
)

// WithRoleForTest returns a new request whose context carries the given role,
// mirroring what the Auth middleware injects after validating a real session.
// Use this in tests that exercise request handlers directly without standing
// up the full OIDC auth stack.
func WithRoleForTest(r *http.Request, role string) *http.Request {
	ctx := context.WithValue(r.Context(), roleKey, role)
	return r.WithContext(ctx)
}

// NewRequestWithRole is a convenience wrapper for
//
//	middleware.WithRoleForTest(httptest.NewRequest(...), role)
//
// for the common case of creating a fresh test request with a role already set.
func NewRequestWithRole(method, target, role string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return WithRoleForTest(r, role)
}
