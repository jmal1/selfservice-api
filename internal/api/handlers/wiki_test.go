package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// newTestHandlerForWiki returns a minimal Handler suitable for testing
// the wiki endpoints. The wiki handlers don't touch the database,
// events, or vCenter — only the embedded bundle — so we can construct a
// Handler with zero values for the other deps.
func newTestHandlerForWiki(t *testing.T) *Handler {
	t.Helper()
	return &Handler{logger: slog.Default()}
}

// withChiParam attaches a chi RouteContext to the request so
// chi.URLParam(r, "*") returns the expected wildcard. Tests construct
// the request directly instead of going through the router, so we have
// to inject the params chi normally extracts.
func withChiParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

func TestWikiIndex_ReturnsManifest(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/index", nil)
	rec := httptest.NewRecorder()
	h.WikiIndex(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Seeds []string `json:"seeds"`
		Files []struct {
			Path     string `json:"path"`
			FromSeed bool   `json:"from_seed"`
		} `json:"files"`
		TotalBytes int64 `json:"total_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Seeds) == 0 {
		t.Error("response has no seeds")
	}
	if len(resp.Files) == 0 {
		t.Error("response has no files")
	}
	if resp.TotalBytes == 0 {
		t.Error("total_bytes is zero")
	}
}

func TestWikiPage_ServesMarkdown(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/AGENTS.md", nil)
	req = withChiParam(req, "*", "AGENTS.md")
	rec := httptest.NewRecorder()
	h.WikiPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Errorf("Content-Type: want text/markdown, got %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("missing ETag header")
	}
	if !strings.Contains(rec.Body.String(), "workflow") {
		t.Error("body doesn't look like AGENTS.md")
	}
}

func TestWikiPage_ServesSourceFile(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/internal/models/workflow_models.go", nil)
	req = withChiParam(req, "*", "internal/models/workflow_models.go")
	rec := httptest.NewRecorder()
	h.WikiPage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("Content-Type: want text/plain, got %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "package models") {
		t.Error("body doesn't look like workflow_models.go")
	}
}

func TestWikiPage_NotFound(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/does/not/exist.md", nil)
	req = withChiParam(req, "*", "does/not/exist.md")
	rec := httptest.NewRecorder()
	h.WikiPage(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: want 404, got %d", rec.Code)
	}
}

func TestWikiPage_DownloadDisposition(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/AGENTS.md?download=1", nil)
	req = withChiParam(req, "*", "AGENTS.md")
	rec := httptest.NewRecorder()
	h.WikiPage(rec, req)

	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "AGENTS.md") {
		t.Errorf("Content-Disposition: want attachment for AGENTS.md, got %q", cd)
	}
}

func TestWikiPage_ETag304(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req1 := withChiParam(httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/AGENTS.md", nil), "*", "AGENTS.md")
	rec1 := httptest.NewRecorder()
	h.WikiPage(rec1, req1)
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag returned")
	}
	req2 := withChiParam(httptest.NewRequest(http.MethodGet, "/api/v1/wiki/page/AGENTS.md", nil), "*", "AGENTS.md")
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.WikiPage(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("status: want 304, got %d", rec2.Code)
	}
}

func TestWikiZip_StreamsValidZip(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/wiki/bundle.zip", nil)
	rec := httptest.NewRecorder()
	h.WikiZip(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Errorf("Content-Type: want application/zip, got %q", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".zip") {
		t.Errorf("Content-Disposition: want zip attachment, got %q", cd)
	}

	// Parse the body as a zip and assert AGENTS.md is present with
	// its repo-relative path preserved.
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("parse zip: %v", err)
	}
	var foundAgents, foundCode bool
	for _, f := range zr.File {
		if f.Name == "AGENTS.md" {
			foundAgents = true
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("open AGENTS.md in zip: %v", err)
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			if !strings.Contains(string(data), "workflow") {
				t.Error("AGENTS.md in zip doesn't look right")
			}
		}
		if f.Name == "internal/models/workflow_models.go" {
			foundCode = true
		}
	}
	if !foundAgents {
		t.Error("AGENTS.md missing from zip")
	}
	if !foundCode {
		t.Error("internal/models/workflow_models.go missing from zip (closure walk broken?)")
	}
}

