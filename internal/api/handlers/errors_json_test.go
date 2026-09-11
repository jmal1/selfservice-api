package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// respondError is the JSON counterpart of http.Error. Handlers on the
// user-facing form surface use it so the browser (which parses the body as
// JSON) actually receives the failure reason instead of a null ApiError.body.
func TestRespondError_ShapeAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", nil)

	respondError(rec, req, http.StatusBadRequest, "name is required")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q; want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v (raw=%q)", err, rec.Body.String())
	}
	if body["error"] != "name is required" {
		t.Errorf("error = %q; want %q", body["error"], "name is required")
	}
	// No RequestID middleware in the chain -> request_id must be omitted, not
	// present-but-empty (keeps the contract clean for the UI).
	if _, ok := body["request_id"]; ok {
		t.Errorf("request_id should be omitted when no chi RequestID is set; got %q", body["request_id"])
	}
}

// When the chi RequestID middleware has stamped the context, respondError must
// echo it so a support request can be correlated with server logs.
func TestRespondError_EchoesRequestID(t *testing.T) {
	var captured *httptest.ResponseRecorder
	h := chimiddleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondError(w, r, http.StatusConflict, "nope")
	}))
	captured = httptest.NewRecorder()
	h.ServeHTTP(captured, httptest.NewRequest(http.MethodGet, "/x", nil))

	var body map[string]string
	if err := json.Unmarshal(captured.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["request_id"] == "" {
		t.Errorf("request_id empty; want the chi RequestID echoed into the error body")
	}
}

// The whole point of the conversion: a validation rejection must now be a
// JSON body carrying the reason, so the UI can surface it. Previously
// http.Error emitted text/plain and ApiError.body was null.
func TestAdminCreateBlueprint_InvalidInputReturnsJSONError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty_name_and_vms", `{"name":"","vms":[]}`, "name and at least one VM are required"},
		{"zero_vms", `{"name":"AD Lab","vms":[]}`, "name and at least one VM are required"},
		{"malformed_json", `{`, "invalid request body"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{logger: slog.Default()} // db nil: validation runs before DB
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blueprints", bytes.NewReader([]byte(tc.body)))
			req = withRoleAndUser(req, models.RoleInstructor, uuid.New())

			rec := httptest.NewRecorder()
			h.AdminCreateBlueprint(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type = %q; want application/json (a plaintext body means the UI sees null)", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("error body is not JSON: %v (raw=%q)", err, rec.Body.String())
			}
			if body["error"] != tc.want {
				t.Errorf("error = %q; want %q", body["error"], tc.want)
			}
		})
	}
}
