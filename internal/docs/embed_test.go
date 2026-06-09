package docs

import (
	"strings"
	"testing"
)

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
