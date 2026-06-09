package handlers

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jmal1/selfservice-api/internal/docs"
)

// docsBundle is loaded lazily on first request to avoid impacting
// startup time for deployments that have an empty bundle (e.g. dev
// workstations where `make wiki-bundle` hasn't run). The first request
// will return 503 in that case; subsequent requests after a real bundle
// is embedded into a freshly-built binary will succeed.
func docsBundle() (*docs.Bundle, error) { return docs.Load() }

// WikiIndex returns the manifest as JSON. The UI calls this once on
// page load to render the sidebar tree.
//
// Route: GET /api/v1/wiki/index
// Auth: instructor+admin (gated by routes.go middleware).
func (h *Handler) WikiIndex(w http.ResponseWriter, r *http.Request) {
	b, err := docsBundle()
	if err != nil {
		writeBundleError(w, h, "load bundle", err)
		return
	}
	// Sort entries for stable UI rendering and to make ETags deterministic.
	entries := b.List()
	resp := struct {
		Seeds      []string             `json:"seeds"`
		Files      []docs.ManifestEntry `json:"files"`
		TotalBytes int64                `json:"total_bytes"`
	}{
		Seeds:      b.Seeds(),
		Files:      entries,
		TotalBytes: b.Manifest().TotalBytes,
	}
	w.Header().Set("Content-Type", "application/json")
	// Static after build; client can cache aggressively.
	w.Header().Set("Cache-Control", "private, max-age=300")
	_ = json.NewEncoder(w).Encode(resp)
}

// WikiPage serves a single file from the bundle. Path is extracted from
// the chi URL parameter `*` (everything after /api/v1/wiki/page/). The
// response body is the raw file bytes; the UI decides whether to render
// it as markdown or as syntax-highlighted code based on the Content-Type.
//
// Route: GET /api/v1/wiki/page/{path...}
// Auth: instructor+admin.
func (h *Handler) WikiPage(w http.ResponseWriter, r *http.Request) {
	rawPath := chi.URLParam(r, "*")
	b, err := docsBundle()
	if err != nil {
		writeBundleError(w, h, "load bundle", err)
		return
	}
	entry, err := b.Entry(rawPath)
	if err != nil {
		http.Error(w, "not in wiki bundle", http.StatusNotFound)
		return
	}
	data, err := b.Read(rawPath)
	if err != nil {
		writeBundleError(w, h, "read file", err)
		return
	}
	// Set a precise Content-Type so clients with download intent
	// (browser "Save as", curl -O) end up with sensible defaults. All
	// our bundle file types are text/utf-8.
	w.Header().Set("Content-Type", contentTypeFor(entry.Path))
	w.Header().Set("X-Wiki-SHA256", entry.SHA256)
	w.Header().Set("ETag", `"`+entry.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, entry.SHA256) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	// download=1 query forces an attachment disposition so the link can
	// be used as a "Save .md for my AI agent" button without a separate
	// endpoint.
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename=%q`, path.Base(entry.Path)))
	}
	_, _ = w.Write(data)
}

// WikiZip streams the entire bundle as a single zip download. Path
// structure inside the zip preserves repo-relative layout so when the
// instructor extracts it into a folder and points an AI coding agent
// at it, all the `[text](internal/foo.go)` links in the markdown
// resolve correctly on disk.
//
// Route: GET /api/v1/wiki/bundle.zip
// Auth: instructor+admin.
func (h *Handler) WikiZip(w http.ResponseWriter, r *http.Request) {
	b, err := docsBundle()
	if err != nil {
		writeBundleError(w, h, "load bundle", err)
		return
	}
	filename := fmt.Sprintf("crucible-wiki-%s.zip", time.Now().UTC().Format("20060102"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))

	zw := zip.NewWriter(w)
	defer zw.Close()

	walkErr := b.WalkFiles(func(entry docs.ManifestEntry, data []byte) error {
		f, err := zw.Create(entry.Path)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	})
	if walkErr != nil {
		// At this point response headers are already sent; best we can do
		// is log and let the client see a truncated zip.
		h.logger.Error("wiki zip stream failed", "error", walkErr)
	}
}

// contentTypeFor returns the response Content-Type for a bundle path.
// All bundle files are utf-8 text; we differentiate so curl / wget pick
// up the right filename when saving and so the UI can decide whether to
// pipe through marked() (markdown) or highlight.js (code).
func contentTypeFor(p string) string {
	switch ext := strings.ToLower(path.Ext(p)); ext {
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	default:
		// Plain text for .go / .sql / .sh / .yaml — the UI's syntax
		// highlighter decides on display based on the path extension,
		// not Content-Type.
		return "text/plain; charset=utf-8"
	}
}

// writeBundleError maps internal docs errors to appropriate HTTP
// statuses. ErrBundleEmpty is the only one a deployment-time fix can
// resolve (run `make wiki-bundle`); the others are bugs.
func writeBundleError(w http.ResponseWriter, h *Handler, op string, err error) {
	switch {
	case errors.Is(err, docs.ErrBundleEmpty):
		http.Error(w, "wiki bundle not available — contact a platform engineer", http.StatusServiceUnavailable)
	case errors.Is(err, docs.ErrNotFound):
		http.Error(w, "not in wiki bundle", http.StatusNotFound)
	default:
		h.logger.Error("wiki "+op+" failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
