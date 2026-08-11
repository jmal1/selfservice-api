package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// A panicking handler must not crash the request with an empty/HTML body; the
// Recover middleware must convert it into a structured JSON 500 the UI can
// parse. This is the server half of the Blueprints-outage lesson: when the
// backend hands the browser nothing structured, the client shows a raw error.
func TestRecover_PanicBecomesJSON500(t *testing.T) {
	h := Recover(slog.New(slog.NewTextHandler(discard{}, nil)))(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			panic("boom")
		}))

	// Wrap with chi RequestID so the recovered body carries a request_id.
	chain := chimiddleware.RequestID(h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/anything", nil)

	// Must not propagate the panic out of ServeHTTP.
	chain.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q; want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (raw=%q)", err, rec.Body.String())
	}
	if body["error"] != "internal server error" {
		t.Errorf("error = %q; want %q", body["error"], "internal server error")
	}
	if body["request_id"] == "" {
		t.Errorf("request_id is empty; want the chi RequestID echoed back")
	}
}

// A handler that returns normally must pass through untouched — the middleware
// must not alter status, body, or headers on the happy path.
func TestRecover_PassThroughOnSuccess(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/thing", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201", rec.Code)
	}
	if rec.Body.String() != `{"ok":true}` {
		t.Errorf("body = %q; want passthrough unchanged", rec.Body.String())
	}
}

// If a handler already committed a status before panicking, the middleware must
// not attempt to rewrite the status code (that would only log a superfluous
// WriteHeader warning and cannot change what the client already received).
func TestRecover_DoesNotClobberCommittedResponse(t *testing.T) {
	h := Recover(slog.New(slog.NewTextHandler(discard{}, nil)))(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("partial"))
			panic("after commit")
		}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 (must not be rewritten to 500)", rec.Code)
	}
}

// http.ErrAbortHandler is the sanctioned "abort quietly" signal (used by
// WebSocket teardown). The middleware must re-panic it so net/http handles it,
// rather than swallowing it into a JSON 500.
func TestRecover_RepanicsErrAbortHandler(t *testing.T) {
	h := Recover(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler {
			t.Fatalf("recovered = %v; want ErrAbortHandler to be re-panicked", rec)
		}
	}()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ws", nil)
	h.ServeHTTP(rec, req)
	t.Fatal("expected ErrAbortHandler to propagate")
}

// quoteJSON must escape characters that could otherwise break out of the JSON
// string context, so a crafted request id can never corrupt the error body.
func TestQuoteJSON_Escaping(t *testing.T) {
	cases := map[string]string{
		`abc`:      `"abc"`,
		`a"b`:      `"a\"b"`,
		"a\\b":     `"a\\b"`,
		"a\nb":     `"a\nb"`,
		"tab\tend": `"tab\tend"`,
	}
	for in, want := range cases {
		if got := quoteJSON(in); got != want {
			t.Errorf("quoteJSON(%q) = %s; want %s", in, got, want)
		}
	}
	// Whatever quoteJSON emits must itself be valid JSON.
	var s string
	if err := json.Unmarshal([]byte(quoteJSON("a\"b\n\t\x01")), &s); err != nil {
		t.Fatalf("quoteJSON output not valid JSON: %v", err)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
