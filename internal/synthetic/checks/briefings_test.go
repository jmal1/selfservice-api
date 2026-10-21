package checks

import (
	"context"
	"net/http"
	"testing"

	"github.com/jmal1/selfservice-api/internal/synthetic"
)

func respond(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

func TestBriefingAdminRBAC(t *testing.T) {
	const path = "/api/v1/admin/templates/00000000-0000-0000-0000-000000000001/briefing/revisions"
	for code, wantOK := range map[int]bool{403: true, 200: false, 404: false} {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){path: respond(code, `[]`)})
		_, err := BriefingAdminRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
		if (err == nil) != wantOK {
			t.Errorf("status %d: err=%v, want pass=%v", code, err, wantOK)
		}
	}
}

func TestBriefingPodContract(t *testing.T) {
	const unknown = "/api/v1/pods/00000000-0000-0000-0000-000000000001/briefing"
	for _, tc := range []struct {
		name   string
		routes map[string]func(http.ResponseWriter, *http.Request)
		wantOK bool
	}{
		{"no pods", map[string]func(http.ResponseWriter, *http.Request){
			unknown: respond(404, `{}`), "/api/v1/pods": respond(200, `[]`)}, true},
		{"own pod", map[string]func(http.ResponseWriter, *http.Request){
			unknown: respond(404, `{}`), "/api/v1/pods": respond(200, `[{"id":"p1"}]`),
			"/api/v1/pods/p1/briefing": respond(200, `{"sections":[]}`)}, true},
		{"unknown pod answers 200", map[string]func(http.ResponseWriter, *http.Request){
			unknown: respond(200, `{"sections":[]}`), "/api/v1/pods": respond(200, `[]`)}, false},
		{"own pod forbidden", map[string]func(http.ResponseWriter, *http.Request){
			unknown: respond(404, `{}`), "/api/v1/pods": respond(200, `[{"id":"p1"}]`),
			"/api/v1/pods/p1/briefing": respond(403, `{}`)}, false},
		{"own pod missing sections", map[string]func(http.ResponseWriter, *http.Request){
			unknown: respond(404, `{}`), "/api/v1/pods": respond(200, `[{"id":"p1"}]`),
			"/api/v1/pods/p1/briefing": respond(200, `{}`)}, false},
	} {
		srv := newFakeAPI(t, tc.routes)
		_, err := BriefingPodContract.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: err=%v, want pass=%v", tc.name, err, tc.wantOK)
		}
	}
}

func TestBriefingAssetRBAC(t *testing.T) {
	const asset = "/api/v1/briefing-assets/0000000000000000000000000000000000000000000000000000000000000000"
	for _, tc := range []struct {
		name           string
		upload, lookup int
		wantOK         bool
	}{
		{"locked upload, readable route", 403, 404, true},
		{"upload open", 400, 404, false},
		{"read route missing for students", 403, 403, false},
	} {
		srv := newFakeAPI(t, map[string]func(http.ResponseWriter, *http.Request){
			"/api/v1/admin/briefing-assets": respond(tc.upload, `{}`),
			asset:                           respond(tc.lookup, `{}`),
		})
		_, err := BriefingAssetRBAC.Run(context.Background(), synthetic.NewClient(srv.URL, ""))
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: err=%v, want pass=%v", tc.name, err, tc.wantOK)
		}
	}
}
