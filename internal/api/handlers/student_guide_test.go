package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStudentGuideIndex_ReturnsOnlyStudentManifest(t *testing.T) {
	h := newTestHandlerForWiki(t)
	rec := httptest.NewRecorder()

	h.StudentGuideIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/index", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type: want application/json, got %q", rec.Header().Get("Content-Type"))
	}

	var manifest struct {
		Seeds []string `json:"seeds"`
		Files []struct {
			Path       string `json:"path"`
			IsMarkdown bool   `json:"is_markdown"`
			FromSeed   bool   `json:"from_seed"`
		} `json:"files"`
		TotalBytes int64 `json:"total_bytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.Seeds) != 1 || manifest.Seeds[0] != studentGuideOverview {
		t.Fatalf("Seeds = %v, want [%s]", manifest.Seeds, studentGuideOverview)
	}
	if len(manifest.Files) != 6 {
		t.Fatalf("student guide files = %d, want 6", len(manifest.Files))
	}
	if manifest.TotalBytes == 0 {
		t.Error("total_bytes is zero")
	}

	foundOverview := false
	for _, file := range manifest.Files {
		if !strings.HasPrefix(file.Path, studentGuidePrefix) {
			t.Errorf("non-student path leaked into manifest: %q", file.Path)
		}
		if !file.IsMarkdown {
			t.Errorf("student guide file %q is not markdown", file.Path)
		}
		if file.Path == studentGuideOverview {
			foundOverview = true
			if !file.FromSeed {
				t.Error("student overview must be the student-guide manifest seed")
			}
		}
	}
	if !foundOverview {
		t.Errorf("student overview %q missing from manifest", studentGuideOverview)
	}
}

func TestStudentGuidePage_ServesAllowlistedMarkdown(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req := withChiParam(
		httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/page/docs/student/overview.md", nil),
		"*",
		studentGuideOverview,
	)
	rec := httptest.NewRecorder()

	h.StudentGuidePage(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Errorf("Content-Type: want text/markdown, got %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("missing ETag")
	}
	if strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Error("student guide pages must not expose the wiki download behavior")
	}
	if !strings.Contains(rec.Body.String(), "Authentik") {
		t.Error("response does not contain the student overview")
	}
}

func TestStudentGuidePage_RejectsNonStudentAndTraversalPaths(t *testing.T) {
	h := newTestHandlerForWiki(t)
	for _, rawPath := range []string{
		"AGENTS.md",
		"internal/models/workflow_models.go",
		"docs/instructor/overview.md",
		"/docs/student/overview.md",
		"docs/student/../instructor/overview.md",
		"../docs/student/overview.md",
		`docs\student\overview.md`,
		"docs/student/does-not-exist.md",
	} {
		t.Run(rawPath, func(t *testing.T) {
			req := withChiParam(
				httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/page/"+rawPath, nil),
				"*",
				rawPath,
			)
			rec := httptest.NewRecorder()

			h.StudentGuidePage(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status for %q: want 404, got %d", rawPath, rec.Code)
			}
		})
	}
}

func TestStudentGuidePage_ETag304(t *testing.T) {
	h := newTestHandlerForWiki(t)
	req1 := withChiParam(
		httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/page/docs/student/overview.md", nil),
		"*",
		studentGuideOverview,
	)
	rec1 := httptest.NewRecorder()
	h.StudentGuidePage(rec1, req1)
	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("first response has no ETag")
	}

	req2 := withChiParam(
		httptest.NewRequest(http.MethodGet, "/api/v1/student-guide/page/docs/student/overview.md", nil),
		"*",
		studentGuideOverview,
	)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.StudentGuidePage(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("status: want 304, got %d", rec2.Code)
	}
}
