package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"golang.org/x/time/rate"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/scriptvalidator"
)

// scriptValidateFn is the package-level validation function used by
// AdminValidateScript. A function var (rather than threading a service
// through the Handler struct) keeps the diff small and lets tests swap in
// a deterministic fake without touching every handler constructor.
var scriptValidateFn = scriptvalidator.NewValidator().Validate

// scriptValidateFunc is the signature of scriptValidateFn — kept as a type
// so tests can construct fakes without depending on the validator package's
// internals.
type scriptValidateFunc func(ctx context.Context, language, script string, opts ...scriptvalidator.Options) (*scriptvalidator.Result, error)

// validateRateLimiter is a per-user token bucket. 30 requests/minute,
// burst of 10 — matches the "save-as-you-type" debounce pattern.
var (
	validateRateLimiterMu sync.Mutex
	validateRateLimiter   = map[string]*rate.Limiter{}
)

const (
	validateRateRPM   = 30
	validateRateBurst = 10
)

// getValidateLimiter returns the rate.Limiter for a given user key, creating
// one if absent. Limiters are cheap; we don't bother evicting until next
// background sweep.
func getValidateLimiter(userKey string) *rate.Limiter {
	validateRateLimiterMu.Lock()
	defer validateRateLimiterMu.Unlock()
	if l, ok := validateRateLimiter[userKey]; ok {
		return l
	}
	l := rate.NewLimiter(rate.Limit(float64(validateRateRPM)/60.0), validateRateBurst)
	validateRateLimiter[userKey] = l
	return l
}

// AdminValidateScript runs a shellcheck-style lint over a user-supplied script
// and returns normalized findings the editor renders as Monaco markers.
//
// POST /api/v1/admin/scripts/validate
//
//	{"language": "bash", "script": "echo $x"}
//
// 200 -> Result JSON (with findings + has_errors/has_warnings flags)
// 400 -> unsupported language or malformed body
// 413 -> script exceeds size cap (64 KB)
// 429 -> per-user rate limit exceeded
// 500 -> linter invocation failed (binary missing, internal error)
func (h *Handler) AdminValidateScript(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	userKey := userID.String()

	limiter := getValidateLimiter(userKey)
	if !limiter.Allow() {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "rate limit exceeded; try again in a moment", http.StatusTooManyRequests)
		return
	}

	var req struct {
		Language           string   `json:"language"`
		Script             string   `json:"script"`
		InputContextNames  []string `json:"input_context_names,omitempty"`
		OutputContextNames []string `json:"output_context_names,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Language == "" {
		req.Language = "bash"
	}
	if req.Script == "" {
		// Empty script = no findings. Save the linter the round trip.
		respondJSON(w, http.StatusOK, &scriptvalidator.Result{
			Language: req.Language,
			Findings: []scriptvalidator.Finding{},
		})
		return
	}

	res, err := scriptValidateFn(r.Context(), req.Language, req.Script, scriptvalidator.Options{
		InputContextNames:  req.InputContextNames,
		OutputContextNames: req.OutputContextNames,
	})
	if err != nil {
		switch {
		case errors.Is(err, scriptvalidator.ErrUnsupportedLanguage):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, scriptvalidator.ErrScriptTooLarge):
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		default:
			h.logger.Error("script validation failed", "error", err, "user_id", userID, "language", req.Language)
			http.Error(w, "script validator unavailable", http.StatusInternalServerError)
		}
		return
	}
	respondJSON(w, http.StatusOK, res)
}
