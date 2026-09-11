package synthetic

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client wraps an http.Client with a Crucible base URL and a pre-minted
// session cookie. It is intentionally minimal: each Check explicitly chooses
// methods, paths, and response decoding.
type Client struct {
	// BaseURL is the Crucible API root, e.g. "https://crucible.jmal.io".
	// MUST NOT contain a trailing slash; Path() prepends "/" if needed.
	BaseURL string

	// SessionCookie is the value of the `session` cookie. The mint function
	// is in auth.go; callers SHOULD rotate this on every Runner cycle to
	// limit blast radius if the token leaks via a log line or panic.
	SessionCookie string

	// HTTP is the underlying transport. The runner injects an http.Client
	// with a per-check timeout via WithTimeout, so callers should not set
	// HTTP.Timeout here (it would force every check to share the same value).
	HTTP *http.Client
}

// NewClient builds a Client with sane defaults. The transport disables
// automatic redirects so checks like "admin endpoint returns 403" don't get
// silently redirected to a login page.
func NewClient(baseURL, sessionCookie string) *Client {
	return &Client{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		SessionCookie: sessionCookie,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Do executes a request with the synthetic session cookie attached and a
// per-call timeout derived from ctx. The caller is responsible for closing
// resp.Body.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	full := c.BaseURL + path
	if _, err := url.Parse(full); err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", full, err)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Distinct UA so production log filters can ignore synthetic traffic if
	// they choose, and so an alert authoring a SLO can exclude these calls.
	req.Header.Set("User-Agent", "selfservice-synthetic-api-monitor/1")
	if c.SessionCookie != "" {
		req.AddCookie(&http.Cookie{Name: "session", Value: c.SessionCookie})
	}

	return c.HTTP.Do(req)
}

// DoExpectStatus is a convenience wrapper that fails if the response status
// differs from want. It always drains and closes the body. The returned int is
// the actual status seen (useful when want fails — the metric still records
// the real status).
func (c *Client) DoExpectStatus(ctx context.Context, method, path string, body io.Reader, want int) (int, error) {
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != want {
		return resp.StatusCode, fmt.Errorf("expected status %d, got %d", want, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
