package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

// Recover returns middleware that catches a panic from any downstream handler,
// logs it with a stack trace and request context, and writes a structured JSON
// 500 so the client always receives a parseable body.
//
// This replaces chi's built-in middleware.Recoverer, which writes a bare
// text/plain / HTML 500 with no body the UI can parse. The Blueprints outage
// showed that when the server gives the browser nothing structured, the client
// surfaces a raw "API Error 500" — useless to a high-schooler. Here the body is
//
//	{"error": "internal server error", "request_id": "<id>"}
//
// which the UI maps to friendly copy while still exposing the request id for
// support correlation.
//
// The panic is logged (never swallowed silently): the stack goes to slog.Error
// with method, path, and request id so an operator can find the offending code.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Wrap the writer so the deferred recovery can reliably tell
			// whether the handler already started the response. Recover runs
			// as a global middleware (before chi's Logger wraps), so we must
			// do our own wrapping rather than type-asserting the incoming w.
			ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the sanctioned way for a handler to
				// abort without logging noise (e.g. WebSocket hijack teardown);
				// re-panic so net/http handles it as intended.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				reqID := chimiddleware.GetReqID(r.Context())
				logger.Error("panic recovered in HTTP handler",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", reqID,
					"stack", string(debug.Stack()),
				)

				// If a handler already started writing the response we cannot
				// safely change the status; just stop. WriteHeader on an
				// already-committed response is a no-op that logs a warning.
				if ww.Status() != 0 || ww.BytesWritten() > 0 {
					return
				}

				ww.Header().Set("Content-Type", "application/json")
				ww.WriteHeader(http.StatusInternalServerError)
				body := `{"error":"internal server error"`
				if reqID != "" {
					body += `,"request_id":` + quoteJSON(reqID)
				}
				body += `}`
				_, _ = ww.Write([]byte(body))
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

// quoteJSON returns a minimal JSON-quoted string. Request IDs from chi are
// composed of hostname + counter (safe chars), but we escape defensively so a
// crafted value can never break out of the JSON body.
func quoteJSON(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, c := range []byte(s) {
		switch c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				const hex = "0123456789abcdef"
				out = append(out, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			} else {
				out = append(out, c)
			}
		}
	}
	out = append(out, '"')
	return string(out)
}
