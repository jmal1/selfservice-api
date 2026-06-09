package docs

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/wikitypes"
)

// updateGolden, when true, rewrites the golden files instead of
// asserting against them. Invoke via:
//
//	go test ./internal/docs -update
//
// Mandatory whenever you legitimately change the Manifest JSON shape;
// the failure message hint also reminds you.
var updateGolden = flag.Bool("update", false, "rewrite golden testdata files")

func TestLoad_BundleAvailable(t *testing.T) {
	b, err := Load()
	if err != nil {
		// In a fresh checkout someone may have nuked the bundle —
		// surface a clear message so they know what to run.
		t.Fatalf("Load failed: %v (try `make wiki-bundle`)", err)
	}
	if b == nil {
		t.Fatal("Load returned nil bundle without error")
	}
	if len(b.Manifest().Files) == 0 {
		t.Error("bundle has no files; manifest looks empty")
	}
	if len(b.Seeds()) == 0 {
		t.Error("bundle has no seeds; bundler was probably misconfigured")
	}
}

func TestLoad_SeedsAreInIndex(t *testing.T) {
	// Every seed must be reachable via Entry — that's the whole point
	// of seeding. Regression guard against a bundler bug that silently
	// drops the seed list.
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	for _, s := range b.Seeds() {
		if _, err := b.Entry(s); err != nil {
			t.Errorf("seed %q not in index", s)
		}
	}
}

func TestRead_KnownFile(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	// AGENTS.md is the canonical seed and must always be present.
	data, err := b.Read("AGENTS.md")
	if err != nil {
		t.Fatalf("Read AGENTS.md: %v", err)
	}
	if len(data) == 0 {
		t.Error("AGENTS.md is empty")
	}
	if !strings.Contains(string(data), "workflow") {
		t.Error("AGENTS.md doesn't contain the word 'workflow'; bundle may be wrong file")
	}
}

func TestRead_NotFound(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	_, err = b.Read("does/not/exist.md")
	if err != ErrNotFound {
		t.Errorf("Read non-existent: want ErrNotFound, got %v", err)
	}
}

func TestRead_PathTraversalRefused(t *testing.T) {
	// Defense in depth: even if a handler forwards a `../` path from the
	// URL, the bundle must refuse to serve outside its tree. cleanPath
	// normalizes, then the index lookup fails because traversal paths
	// aren't in the index.
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	bad := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"/etc/passwd",
		"AGENTS.md/../../../etc/passwd",
	}
	for _, p := range bad {
		if _, err := b.Read(p); err != ErrNotFound {
			// Either ErrNotFound or some other non-success is fine; what
			// we must not do is return file bytes from outside the bundle.
			t.Errorf("Read(%q) = err=%v; want ErrNotFound or other error, not success", p, err)
		}
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"AGENTS.md", "AGENTS.md"},
		{"/AGENTS.md", "AGENTS.md"},
		{"./AGENTS.md", "AGENTS.md"},
		{"foo/../bar.md", "bar.md"},
		// Traversal that "escapes" returns a relative name that's not
		// in the index — the safety property we care about is that
		// path.Clean strips the leading "../" segments before lookup,
		// so the bundle never accidentally reads outside _bundle/.
		{"../escape", "escape"},
		{"", ""},
		{"  AGENTS.md  ", "AGENTS.md"},
	}
	for _, tc := range tests {
		got := cleanPath(tc.in)
		if got != tc.want {
			t.Errorf("cleanPath(%q): want %q, got %q", tc.in, tc.want, got)
		}
	}
}

func TestList_Sorted(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	list := b.List()
	for i := 1; i < len(list); i++ {
		if list[i-1].Path > list[i].Path {
			t.Errorf("not sorted: %q before %q", list[i-1].Path, list[i].Path)
		}
	}
}

func TestWalkFiles_VisitsAll(t *testing.T) {
	b, err := Load()
	if err != nil {
		t.Skip("bundle unavailable")
	}
	count := 0
	err = b.WalkFiles(func(e ManifestEntry, data []byte) error {
		if int64(len(data)) != e.Size {
			t.Errorf("size mismatch for %q: manifest=%d actual=%d", e.Path, e.Size, len(data))
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if count != len(b.Manifest().Files) {
		t.Errorf("walked %d files, manifest has %d", count, len(b.Manifest().Files))
	}
}

// TestManifestSchema is a schema-contract golden test. It serializes a
// fixed-data Manifest and diffs the resulting JSON against the
// checked-in golden file. The golden file captures every field name
// AND ordering that the API will emit on the wire.
//
// **What this protects against.** When someone adds a field to
// wikitypes.Manifest or wikitypes.ManifestEntry, this test fails
// (because the golden file is missing the new field). The fix is to
// run `go test ./internal/docs -update`, inspect the diff, and update
// the UI's WikiManifestEntry TypeScript type to match. Without this
// guardrail, a field add/remove silently changes the wire format and
// the UI either drops the new field (consumers ignore unknown JSON
// keys) or breaks on type mismatches.
//
// **Why a fixed-data sample, not the real bundle?** The real bundle
// has 21 entries that change whenever docs are edited; the golden
// would churn on every doc change and lose its schema-pinning power.
// A hand-written sample with exactly two entries (one markdown, one
// source file) covers every field combo without coupling to content.
func TestManifestSchema(t *testing.T) {
	sample := wikitypes.Manifest{
		// GeneratedAt intentionally non-empty so the field appears
		// in the JSON; the bundler omits it in production for
		// reproducible builds, but the test must cover both paths.
		GeneratedAt: "2026-01-01T00:00:00Z",
		Seeds:       []string{"docs/instructor/overview.md", "AGENTS.md"},
		Files: []wikitypes.ManifestEntry{
			{
				Path:       "docs/instructor/overview.md",
				Title:      "Crucible for Instructors — Overview",
				Size:       1234,
				SHA256:     "0000000000000000000000000000000000000000000000000000000000000000",
				IsMarkdown: true,
				LinksOut:   []string{"docs/instructor/workflows.md"},
				FromSeed:   true,
			},
			{
				Path:       "internal/api/routes/routes.go",
				Title:      "routes.go (Go)",
				Size:       5678,
				SHA256:     "1111111111111111111111111111111111111111111111111111111111111111",
				IsMarkdown: false,
				// LinksOut intentionally nil so the omitempty tag's
				// behaviour is locked in: source-file entries must
				// omit the field, not emit `"links_out": null`.
				FromSeed: false,
			},
		},
		TotalBytes: 6912,
	}

	got, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "manifest.golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("rewrote golden file: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (%s): %v\nrun `go test ./internal/docs -update` to create it", goldenPath, err)
	}
	// Normalize line endings: git on Windows may check out the golden
	// file with CRLF, but json.MarshalIndent always emits LF. Without
	// this, the test passes on Linux CI but fails for Windows
	// contributors after a fresh checkout.
	wantNorm := strings.ReplaceAll(string(want), "\r\n", "\n")
	if string(got) != wantNorm {
		t.Errorf("manifest JSON schema differs from golden file.\n"+
			"If you intentionally changed wikitypes.Manifest or "+
			"wikitypes.ManifestEntry, update the UI's WikiManifestEntry "+
			"TypeScript type in selfservice-ui to match, then run:\n"+
			"  go test ./internal/docs -update\n"+
			"to regenerate the golden file.\n\n"+
			"--- got ---\n%s\n--- want ---\n%s",
			string(got), wantNorm)
	}
}

// TestManifestSchema_EveryWireFieldDocumented asserts that the JSON
// keys we emit for ManifestEntry match a known-good allowlist. This
// catches the case where someone adds a field to wikitypes but the
// schema golden file is updated mechanically without the developer
// considering the UI side. The allowlist below is the source of truth
// for what the UI's WikiManifestEntry MUST accept.
func TestManifestSchema_EveryWireFieldDocumented(t *testing.T) {
	// Marshal a fully-populated entry so omitempty fields appear.
	e := wikitypes.ManifestEntry{
		Path:       "x",
		Title:      "x",
		Size:       1,
		SHA256:     "x",
		IsMarkdown: true,
		LinksOut:   []string{"x"},
		FromSeed:   true,
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// EVERY field below MUST also exist (with matching type) in the
	// UI's WikiManifestEntry TypeScript interface at
	// selfservice-ui/src/lib/api/client.ts. If you add to this set,
	// update that file in the same PR.
	wantFields := []string{"path", "title", "size", "sha256", "is_markdown", "links_out", "from_seed"}

	got := make(map[string]bool, len(m))
	for k := range m {
		got[k] = true
	}
	for _, want := range wantFields {
		if !got[want] {
			t.Errorf("expected field %q missing from JSON output (delete from wantFields if intentional)", want)
		}
		delete(got, want)
	}
	if len(got) > 0 {
		extra := make([]string, 0, len(got))
		for k := range got {
			extra = append(extra, k)
		}
		t.Errorf("unknown fields in JSON output: %v\nadd them to wantFields here AND to WikiManifestEntry in selfservice-ui/src/lib/api/client.ts", extra)
	}
}
