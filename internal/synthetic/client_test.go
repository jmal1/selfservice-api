package synthetic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClient_AddsSessionCookie proves the session cookie is attached to every
// outgoing request. A regression here breaks every authenticated check.
func TestClient_AddsSessionCookie(t *testing.T) {
	var sawCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil {
			sawCookie = c.Value
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "cookie-value-abc")
	resp, err := c.Do(context.Background(), http.MethodGet, "/anything", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if sawCookie != "cookie-value-abc" {
		t.Errorf("server saw cookie %q, want %q", sawCookie, "cookie-value-abc")
	}
}

// TestClient_NoCookieWhenEmpty ensures we don't send `session=` (empty) which
// some middlewares treat differently from "no cookie at all".
func TestClient_NoCookieWhenEmpty(t *testing.T) {
	var sawCookie bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := r.Cookie("session")
		sawCookie = err == nil
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "")
	resp, _ := c.Do(context.Background(), http.MethodGet, "/", nil)
	if resp != nil {
		resp.Body.Close()
	}
	if sawCookie {
		t.Error("server should not have seen a session cookie when client cookie is empty")
	}
}

// TestClient_DoesNotFollowRedirects is critical: the AdminListUsers403 check
// would silently succeed if Go followed a 302 → /login → 200 chain. Pin the
// behaviour.
func TestClient_DoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/landing", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("LANDED"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "ignored")
	resp, err := c.Do(context.Background(), http.MethodGet, "/start", nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (CheckRedirect should stop at the redirect)", resp.StatusCode)
	}
}

// TestClient_TrimsTrailingSlashFromBaseURL pins URL composition: a baseURL of
// "https://x/" + path "/y" must produce "https://x/y" not "https://x//y".
func TestClient_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	c := NewClient("https://example.com/", "")
	if !strings.HasSuffix(c.BaseURL, "com") {
		t.Errorf("BaseURL = %q, expected trailing slash trimmed", c.BaseURL)
	}
}

// TestClient_DoExpectStatus_MatchesAndFails covers both branches of the
// helper used by every check.
func TestClient_DoExpectStatus_MatchesAndFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "")

	got, err := c.DoExpectStatus(context.Background(), http.MethodGet, "/ok", nil, http.StatusOK)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if got != http.StatusOK {
		t.Fatalf("happy path status = %d, want 200", got)
	}

	got, err = c.DoExpectStatus(context.Background(), http.MethodGet, "/boom", nil, http.StatusOK)
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
	if got != http.StatusInternalServerError {
		t.Fatalf("unhappy path status = %d, want 500", got)
	}
}
