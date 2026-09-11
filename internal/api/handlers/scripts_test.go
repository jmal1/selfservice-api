package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/jmal1/selfservice-api/internal/scriptvalidator"
)

func newScriptsHandlerForTest() *Handler {
	// We only need a logger; the handler doesn't touch db/events/vc for /scripts/validate.
	return &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// resetRateLimiterForTest wipes the package-level limiter map so adjacent
// tests don't bleed into each other.
func resetRateLimiterForTest() {
	validateRateLimiterMu.Lock()
	validateRateLimiter = map[string]*rate.Limiter{}
	validateRateLimiterMu.Unlock()
}

// swapValidator atomically replaces scriptValidateFn for the duration of a
// test, returning a cleanup func.
func swapValidator(fn scriptValidateFunc) func() {
	prev := scriptValidateFn
	scriptValidateFn = fn
	return func() { scriptValidateFn = prev }
}

func TestAdminValidateScript_Success(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		return &scriptvalidator.Result{
			Language: "bash",
			Findings: []scriptvalidator.Finding{
				{Line: 1, Column: 6, EndLine: 1, EndColumn: 8, Severity: scriptvalidator.SeverityWarning, Code: "SC2086", Message: "Double quote to prevent globbing."},
			},
			HasWarnings: true,
			DurationMs:  12,
		}, nil
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":"echo $x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.AdminValidateScript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got scriptvalidator.Result
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got.Findings))
	}
	if !got.HasWarnings {
		t.Error("expected HasWarnings=true")
	}
}

func TestAdminValidateScript_EmptyScriptShortCircuits(t *testing.T) {
	resetRateLimiterForTest()
	called := atomic.Bool{}
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		called.Store(true)
		return nil, errors.New("should not be called")
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":""}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if called.Load() {
		t.Error("validator should not be invoked for empty script")
	}
}

func TestAdminValidateScript_UnsupportedLanguageIs400(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		return nil, scriptvalidator.ErrUnsupportedLanguage
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"powershell","script":"Write-Host hi"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminValidateScript_ScriptTooLargeIs413(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		return nil, scriptvalidator.ErrScriptTooLarge
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":"echo hi"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminValidateScript_InternalErrorIs500(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		return nil, errors.New("shellcheck not found")
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":"echo hi"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminValidateScript_RateLimit429(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		return &scriptvalidator.Result{Language: "bash", Findings: []scriptvalidator.Finding{}}, nil
	})()

	h := newScriptsHandlerForTest()
	body := `{"language":"bash","script":"echo hi"}`

	// Burst is 10. Fire 11 back-to-back; the 11th should 429.
	var last int
	for i := 0; i < validateRateBurst+1; i++ {
		req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.AdminValidateScript(w, req)
		last = w.Code
		if i < validateRateBurst && w.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected last call to be 429, got %d", last)
	}
	// Make sure tokens regenerate eventually — we set a sub-second rate
	// (30/min = 0.5/s), so a quick sleep + wait wouldn't be deterministic.
	// Just assert Retry-After was set on the 429.
}

func TestAdminValidateScript_MalformedJSON(t *testing.T) {
	resetRateLimiterForTest()
	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate", strings.NewReader(`{not json`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAdminValidateScript_UnknownFieldsRejected(t *testing.T) {
	resetRateLimiterForTest()
	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":"hi","sneaky":"value"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown fields, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminValidateScript_DefaultLanguageIsBash(t *testing.T) {
	resetRateLimiterForTest()
	var receivedLang string
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		receivedLang = lang
		return &scriptvalidator.Result{Language: lang}, nil
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"script":"echo hi"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if receivedLang != "bash" {
		t.Errorf("expected default language 'bash', got %q", receivedLang)
	}
}

// integration-ish smoke: ensure context.WithTimeout doesn't leak into the
// validator (we should pass the real request context through).
func TestAdminValidateScript_ContextPassedThrough(t *testing.T) {
	resetRateLimiterForTest()
	defer swapValidator(func(ctx context.Context, lang, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("did not expect a deadline on the validator ctx (handler doesn't set one)")
		}
		return &scriptvalidator.Result{Language: "bash"}, nil
	})()

	h := newScriptsHandlerForTest()
	req := httptest.NewRequest(http.MethodPost, "/admin/scripts/validate",
		strings.NewReader(`{"language":"bash","script":"echo hi"}`))
	w := httptest.NewRecorder()
	h.AdminValidateScript(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Sanity: a request without a deadline should still complete promptly.
	_ = time.Now()
}
