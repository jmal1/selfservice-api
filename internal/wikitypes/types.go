// Package wikitypes holds the on-disk schema for the instructor wiki
// bundle manifest. It is the single source of truth for both the
// bundler (cmd/wiki-bundler) that PRODUCES manifest.json and the
// runtime package (internal/docs) that CONSUMES it.
//
// History: prior to extracting this package, the schema was duplicated
// in both call sites — adding a field on one side and forgetting the
// other caused a silent JSON field-drop bug (see git blame: PR #19).
// Centralizing the types eliminates that class of bug. The price is
// one extra leaf package, which has no third-party or transitive deps
// so it's safe to import from anywhere.
//
// Any new field added here is automatically picked up by:
//   - cmd/wiki-bundler (writes it to manifest.json)
//   - internal/docs.Bundle (deserializes it for the API to serve)
//
// The UI's WikiManifestEntry TypeScript type in selfservice-ui must
// be updated MANUALLY when fields change. See the schema-contract
// golden test in internal/docs/embed_test.go (TestManifestSchema) —
// it fails when the JSON shape drifts, forcing a conscious update.
package wikitypes

// ManifestEntry describes one file in the bundle. Path is repo-relative
// (the same path used in the source tree and in the embed.FS); size
// and sha let the API report cache-friendly ETags and let CI catch
// inadvertent drift.
type ManifestEntry struct {
	// Path is the bundle-relative path, e.g.
	// "docs/instructor/overview.md". Same as the path under the
	// repo's source tree.
	Path string `json:"path"`
	// Title is the human-friendly display name for the UI sidebar.
	// For markdown files it's the first `# H1` heading (whitespace
	// trimmed); for source files it's a humanised version of the
	// basename (filename + parenthesised language hint). Always
	// non-empty so the UI can render it unconditionally.
	Title string `json:"title"`
	// Size is the file size in bytes (informational; UI shows it
	// next to download links).
	Size int64 `json:"size"`
	// SHA256 is the hex digest of the raw file bytes. Used by the
	// API to compute ETags and by CI to catch inadvertent content
	// drift between the seeds and the bundle.
	SHA256 string `json:"sha256"`
	// IsMarkdown is true for .md files; the UI uses this to choose
	// between the markdown renderer and the source-code renderer.
	IsMarkdown bool `json:"is_markdown"`
	// LinksOut is the list of in-closure files this file references.
	// nil for non-markdown files. Useful for the UI's "referenced by"
	// panel (not yet implemented; reserved for future use).
	LinksOut []string `json:"links_out,omitempty"`
	// FromSeed indicates this file was passed in as a seed (top-level
	// instructor doc) rather than pulled in transitively. The UI
	// surfaces seeds in the "Start here" panel.
	FromSeed bool `json:"from_seed"`
}

// Manifest is the top-level bundle index. It's both written to disk
// (by cmd/wiki-bundler at build time) and consumed by the API at
// runtime (by internal/docs.Bundle).
type Manifest struct {
	// GeneratedAt is optional; left blank for reproducible builds.
	// When populated, it's an RFC3339 timestamp of when the bundler
	// ran. The bundler intentionally does NOT populate this by
	// default so two runs from the same seeds produce byte-identical
	// manifests.
	GeneratedAt string `json:"generated_at,omitempty"`
	// Seeds is the ordered list of seed paths the bundler was
	// invoked with. The UI's "Start here" panel uses Seeds[0] as the
	// default landing page; preserve order on changes.
	Seeds []string `json:"seeds"`
	// Files is the full bundle contents (seeds + transitive closure)
	// sorted by path for deterministic output.
	Files []ManifestEntry `json:"files"`
	// TotalBytes is the sum of all Files[].Size. Used by the UI to
	// show the bundle size in the sidebar footer.
	TotalBytes int64 `json:"total_bytes"`
}
