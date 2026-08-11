package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// callReachedDB runs a handler that has a nil db and reports whether it
// panicked. The blueprint create/update handlers validate the request body
// *before* any DB access, so with a nil db the only way to panic is to have
// gotten past validation and dereferenced h.db. A panic therefore means
// "validation accepted this input"; no panic means "validation rejected it
// (or the URL/id was bad) before touching the DB".
func callReachedDB(h *Handler, w *httptest.ResponseRecorder, handler func(http.ResponseWriter, *http.Request), r *http.Request) (reachedDB bool) {
	defer func() {
		if rec := recover(); rec != nil {
			reachedDB = true
		}
	}()
	handler(w, r)
	return false
}

// decodedIsEmpty reports whether a raw body, decoded the way the handler
// decodes it, represents an "empty" blueprint (blank name or zero VMs) — the
// exact shape that caused the production outage (a 0-VM blueprint serializes
// with the `vms` key omitted and crashed the admin UI).
func decodedIsEmpty(body []byte) bool {
	var req struct {
		Name string            `json:"name"`
		VMs  []json.RawMessage `json:"vms"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return true // undecodable input is certainly not a valid blueprint
	}
	return req.Name == "" || len(req.VMs) == 0
}

// FuzzAdminUpdateBlueprint throws arbitrary bytes at the update handler and
// asserts two safety properties for every input:
//
//  1. The handler never returns a 5xx and never panics on input that should
//     have been rejected before the DB — i.e. bad input degrades to a clean
//     4xx, never a server error.
//  2. The "a blueprint can never be emptied" invariant cannot be bypassed:
//     if validation let the request through (it reached the nil DB and
//     panicked), the decoded body MUST have had a non-empty name and >=1 VM.
//
// This is the durable, CI-safe guard against the class of bug behind the
// Blueprints outage: no byte sequence may talk the update path into persisting
// an empty blueprint, and no byte sequence may crash the server.
func FuzzAdminUpdateBlueprint(f *testing.F) {
	seeds := []string{
		`{"name":"AD Lab","vms":[{"template_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","display_name":"DC","boot_order":0,"quantity":1}]}`,
		`{"name":"AD Lab","vms":[]}`,
		`{"name":"","vms":[]}`,
		`{"name":"AD Lab"}`,
		`{}`,
		``,
		`not json`,
		`{"name":123,"vms":"nope"}`,
		`{"name":"x","vms":[{"quantity":-1,"boot_order":9999999999}]}`,
		`[]`,
		`null`,
		`{"vms":[` + `{"template_id":"not-a-uuid"}` + `]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	bpID := uuid.New()

	f.Fuzz(func(t *testing.T, body []byte) {
		h := &Handler{logger: slog.Default()} // nil db on purpose
		req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/blueprints/"+bpID.String(), bytes.NewReader(body))
		req = withRoleAndUser(req, models.RoleInstructor, uuid.New())
		req = withRouteParam(req, "blueprintID", bpID)
		w := httptest.NewRecorder()

		reachedDB := callReachedDB(h, w, h.AdminUpdateBlueprint, req)

		if reachedDB {
			// Validation accepted the request. It must not have been empty.
			if decodedIsEmpty(body) {
				t.Fatalf("update validation accepted an empty blueprint (or panicked on invalid input): %q", body)
			}
			return
		}

		// Rejected before the DB: must be a clean client error, never a 5xx.
		if w.Code >= 500 {
			t.Fatalf("update returned %d on input %q; want a 4xx", w.Code, body)
		}
	})
}

// FuzzAdminCreateBlueprint is the same guard for the create path, which shares
// the invariant. Create takes no route id, so a rejection is always about the
// body itself.
func FuzzAdminCreateBlueprint(f *testing.F) {
	seeds := []string{
		`{"name":"AD Lab","vms":[{"template_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","display_name":"DC","boot_order":0,"quantity":1}]}`,
		`{"name":"AD Lab","vms":[]}`,
		`{"name":""}`,
		`{}`,
		``,
		`garbage`,
		`{"name":true,"vms":{}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		h := &Handler{logger: slog.Default()} // nil db on purpose
		req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints", bytes.NewReader(body))
		req = withRoleAndUser(req, models.RoleInstructor, uuid.New())
		w := httptest.NewRecorder()

		reachedDB := callReachedDB(h, w, h.AdminCreateBlueprint, req)

		if reachedDB {
			if decodedIsEmpty(body) {
				t.Fatalf("create validation accepted an empty blueprint (or panicked on invalid input): %q", body)
			}
			return
		}

		if w.Code >= 500 {
			t.Fatalf("create returned %d on input %q; want a 4xx", w.Code, body)
		}
	})
}
