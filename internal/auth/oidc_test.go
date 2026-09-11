package auth

import (
	"net/url"
	"testing"
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
