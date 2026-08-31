package auth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestBuildEndSessionURL(t *testing.T) {
	const (
		endpoint = "https://auth.jmal.io/application/o/selfservice/end-session/"
		postURI  = "https://crucible.jmal.io/login"
		idToken  = "eyJhbGci.eyJzdWIi.sig"
	)

	tests := []struct {
		name          string
		provider      Provider
		idToken       string
		wantEmpty     bool
		wantHint      string
		wantPostLogin string
	}{
		{
			name:          "full url with hint and redirect",
			provider:      Provider{endSessionEndpoint: endpoint, postLogoutRedirectURI: postURI},
			idToken:       idToken,
			wantHint:      idToken,
			wantPostLogin: postURI,
		},
		{
			name:          "no id_token still returns redirect url",
			provider:      Provider{endSessionEndpoint: endpoint, postLogoutRedirectURI: postURI},
			idToken:       "",
			wantHint:      "",
			wantPostLogin: postURI,
		},
		{
			name:      "no endpoint falls back to empty",
			provider:  Provider{endSessionEndpoint: "", postLogoutRedirectURI: postURI},
			idToken:   idToken,
			wantEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.provider.buildEndSessionURL(tt.idToken)
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("expected empty url, got %q", got)
				}
				return
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("result is not a valid url: %v", err)
			}
			q := u.Query()
			if q.Get("id_token_hint") != tt.wantHint {
				t.Errorf("id_token_hint = %q, want %q", q.Get("id_token_hint"), tt.wantHint)
			}
			if q.Get("post_logout_redirect_uri") != tt.wantPostLogin {
				t.Errorf("post_logout_redirect_uri = %q, want %q", q.Get("post_logout_redirect_uri"), tt.wantPostLogin)
			}
		})
	}
}

func TestLoginHandlerRedirectsWithCSRFStateCookie(t *testing.T) {
	provider := &Provider{
		oauth2Config: oauth2.Config{
			ClientID:    "selfservice-client",
			RedirectURL: "https://app.example.test/auth/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://idp.example.test/application/o/authorize/",
				TokenURL: "https://idp.example.test/application/o/token/",
			},
			Scopes: []string{"openid", "profile", "email"},
		},
		logger: slog.Default(),
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rec := httptest.NewRecorder()
	provider.LoginHandler(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTemporaryRedirect)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is not a valid URL: %v", err)
	}
	if got := loc.Query().Get("client_id"); got != "selfservice-client" {
		t.Fatalf("state flow client_id = %q, want %q", got, "selfservice-client")
	}
	if loc.Query().Get("redirect_uri") != "https://app.example.test/auth/callback" {
		t.Fatalf("redirect_uri = %q, want %q", loc.Query().Get("redirect_uri"), "https://app.example.test/auth/callback")
	}
	if loc.Query().Get("state") == "" {
		t.Fatal("state missing from redirect URL")
	}

	cookies := rec.Result().Cookies()
	var stateCookie *http.Cookie
	for i := range cookies {
		if cookies[i].Name == "oauth_state" {
			stateCookie = cookies[i]
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("oauth_state cookie was not set")
	}
	if !stateCookie.HttpOnly || !stateCookie.Secure || stateCookie.Path != "/" || stateCookie.MaxAge != 300 {
		t.Fatalf("oauth_state cookie has unexpected security attributes: %#v", stateCookie)
	}
	if stateCookie.Value != loc.Query().Get("state") {
		t.Fatalf("oauth_state cookie value %q does not match redirect state %q", stateCookie.Value, loc.Query().Get("state"))
	}
}

func TestCallbackHandlerRejectsStateMismatch(t *testing.T) {
	provider := &Provider{logger: slog.Default()}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=wrong-state", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "expected-state"})
	rec := httptest.NewRecorder()
	provider.CallbackHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "invalid state") {
		t.Fatalf("body = %q, want invalid state error", rec.Body.String())
	}
}
