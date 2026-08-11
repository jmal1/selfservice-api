package handlers

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// A blueprint update that would empty the blueprint (blank name and/or zero
// VMs) must be rejected with 400 before it ever reaches the database. This is
// the invariant the create path already enforces; without it on update, a
// blueprint could be persisted with 0 VMs, which the API then serializes with
// the `vms` key omitted (json:"vms,omitempty") and crashes the admin UI.
//
// These cases are validated ahead of any DB access, so a nil-db Handler is
// sufficient to exercise them — reaching the DB would panic and fail the test,
// which also proves the ordering.
func TestAdminUpdateBlueprint_RejectsEmpty(t *testing.T) {
	bpID := uuid.New()
	validVM := `{"template_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","display_name":"DC","boot_order":0,"quantity":1}`

	cases := []struct {
		name string
		body string
	}{
		{"blank_name_with_vm", `{"name":"","vms":[` + validVM + `]}`},
		{"named_but_zero_vms", `{"name":"AD Lab","vms":[]}`},
		{"named_but_missing_vms_key", `{"name":"AD Lab"}`},
		{"blank_name_and_zero_vms", `{"name":"","vms":[]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{logger: slog.Default()} // db is nil on purpose
			req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/blueprints/"+bpID.String(), bytes.NewReader([]byte(tc.body)))
			req = withRoleAndUser(req, models.RoleInstructor, uuid.New())
			req = withRouteParam(req, "blueprintID", bpID)

			w := httptest.NewRecorder()
			h.AdminUpdateBlueprint(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("body %s: got status %d, want 400", tc.body, w.Code)
			}
		})
	}
}

// A well-formed update (name + at least one VM) must pass validation. With a
// nil db the handler panics when it proceeds to the DB lookup — that panic,
// recovered here, is precisely the proof that validation did NOT short-circuit
// a valid request. If validation wrongly rejected it we'd see a 400 instead.
func TestAdminUpdateBlueprint_ValidRequestPassesValidation(t *testing.T) {
	bpID := uuid.New()
	body := `{"name":"AD Lab","vms":[{"template_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","display_name":"DC","boot_order":0,"quantity":1}]}`

	defer func() {
		if recover() == nil {
			t.Fatalf("expected handler to proceed past validation to the (nil) DB and panic; it did not")
		}
	}()

	h := &Handler{logger: slog.Default()} // db is nil: reaching it means validation passed
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/blueprints/"+bpID.String(), bytes.NewReader([]byte(body)))
	req = withRoleAndUser(req, models.RoleInstructor, uuid.New())
	req = withRouteParam(req, "blueprintID", bpID)

	w := httptest.NewRecorder()
	h.AdminUpdateBlueprint(w, req)
}
