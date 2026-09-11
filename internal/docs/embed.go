// Package docs embeds the instructor wiki bundle (markdown + linked
// source files) and exposes a typed interface over it so the API
// handlers don't have to know about the on-disk shape.
//
// The bundle is produced by cmd/wiki-bundler and committed to the repo
// so `go build` works without a separate generate step. CI must run
// `make verify-wiki` to catch drift between the seeds and the bundle.
package docs

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/jmal1/selfservice-api/internal/wikitypes"
)

// bundleFS embeds the entire wiki bundle directory tree. The directory
// is prefixed with `_` so Go's build system ignores it when walking
// packages (the bundle contains real Go source files copied verbatim
// from the rest of the tree; they must NOT be compiled, only served).
//
// The `all:` prefix on the embed pattern is required so embed includes
// the underscore-prefixed directory itself (without it, the embed
// directive would skip the dir).
//
//go:embed all:_bundle
var bundleFS embed.FS

// ManifestEntry and Manifest are re-exported as type aliases from
// internal/wikitypes so existing API handler code (e.g. wiki.go's
// `docs.ManifestEntry` field) continues to compile unchanged after the
// schema-source-of-truth migration. New code should import wikitypes
// directly; these aliases exist purely for back-compat.
type (
	ManifestEntry = wikitypes.ManifestEntry
	Manifest      = wikitypes.Manifest
)

// Bundle is the in-memory representation of the wiki bundle. Construct
// via Load; the result is safe for concurrent use.
type Bundle struct {
	manifest *Manifest
	// index maps repo-relative path -> manifest entry for O(1) lookup
	// by handlers. Populated once during Load.
	index map[string]*ManifestEntry
}

// Sentinel errors so handlers can map to specific HTTP statuses.
var (
	ErrBundleEmpty = errors.New("wiki bundle is empty (run `make wiki-bundle`)")
	ErrNotFound    = errors.New("path not found in wiki bundle")
)

var (
	loadOnce sync.Once
	loadErr  error
	loaded   *Bundle
)

// Load returns the singleton Bundle, parsing the manifest on first
// call. Safe to call repeatedly — subsequent calls are a mutex-free
// read.
func Load() (*Bundle, error) {
	loadOnce.Do(func() {
		loaded, loadErr = newBundle()
	})
	return loaded, loadErr
}

func newBundle() (*Bundle, error) {
	data, err := bundleFS.ReadFile("_bundle/manifest.json")
	if err != nil {
		// Either the bundle directory is empty (dev hasn't run the
		// bundler) or the manifest is missing. Either way it's a build
		// hygiene problem, not a runtime fault — fail explicit.
		return nil, ErrBundleEmpty
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	idx := make(map[string]*ManifestEntry, len(m.Files))
	for i := range m.Files {
		idx[m.Files[i].Path] = &m.Files[i]
	}
	return &Bundle{manifest: &m, index: idx}, nil
}

// Manifest returns a copy of the manifest. The Files slice is sorted by
// path. Callers must not mutate the returned value.
func (b *Bundle) Manifest() *Manifest {
	return b.manifest
}

// Seeds returns the manifest seed paths in their original order. Used
// by the UI's "Start here" panel.
func (b *Bundle) Seeds() []string {
	out := make([]string, len(b.manifest.Seeds))
	copy(out, b.manifest.Seeds)
	return out
}

// Entry returns the manifest entry for a repo-relative path, or
// ErrNotFound. Path matching is exact — callers should normalize
// trailing slashes and `./` prefixes before calling.
func (b *Bundle) Entry(p string) (*ManifestEntry, error) {
	if e, ok := b.index[cleanPath(p)]; ok {
		return e, nil
	}
	return nil, ErrNotFound
}

// Read returns the raw bytes of a file in the bundle. Path is
// repo-relative (e.g. "internal/models/workflow_models.go").
func (b *Bundle) Read(p string) ([]byte, error) {
	cp := cleanPath(p)
	if _, ok := b.index[cp]; !ok {
		return nil, ErrNotFound
	}
	return bundleFS.ReadFile(path.Join("_bundle", cp))
}

// List returns all manifest entries sorted by path. Used for the
// /docs/index API endpoint.
func (b *Bundle) List() []ManifestEntry {
	out := make([]ManifestEntry, len(b.manifest.Files))
	copy(out, b.manifest.Files)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ListPrefix returns a copy of manifest entries directly under a repo-relative
// path prefix. It never reads bundle file contents.
func (b *Bundle) ListPrefix(prefix string) []ManifestEntry {
	prefix = strings.TrimSuffix(cleanPath(prefix), "/")
	if prefix == "" {
		return nil
	}
	prefix += "/"

	out := make([]ManifestEntry, 0)
	for _, entry := range b.manifest.Files {
		if strings.HasPrefix(entry.Path, prefix) {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// WalkFiles applies fn to every file in the bundle (excluding the
// manifest itself). Used by the zip-download handler. Returning a
// non-nil error from fn stops the walk and propagates the error.
func (b *Bundle) WalkFiles(fn func(entry ManifestEntry, data []byte) error) error {
	for _, e := range b.manifest.Files {
		data, err := bundleFS.ReadFile(path.Join("_bundle", e.Path))
		if err != nil {
			return err
		}
		if err := fn(e, data); err != nil {
			return err
		}
	}
	return nil
}

// FS returns the underlying embed.FS sub-rooted at the bundle directory
// so callers can use it directly with io/fs helpers (e.g.
// http.FileServer for development). Returns an error if the bundle
// subtree is missing.
func (b *Bundle) FS() (fs.FS, error) {
	return fs.Sub(bundleFS, "_bundle")
}

// cleanPath normalizes a request path for index lookup: trims surrounding
// whitespace, strips a leading slash, collapses '..' segments. The
// returned path uses forward slashes. Returns "" if the input would
// escape the bundle root.
func cleanPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	p = path.Clean("/" + p)
	p = strings.TrimPrefix(p, "/")
	if strings.HasPrefix(p, "../") || p == ".." {
		return ""
	}
	return p
}
