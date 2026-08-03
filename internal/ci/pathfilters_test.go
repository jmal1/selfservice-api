// Package ci contains guard tests for repository infrastructure.
//
// These tests run in CI via `go test ./...` and fail when
// .github/path-filters.yaml drifts out of sync with the actual Go dependency
// graph.
//
// What actually breaks when it drifts: the per-component channels drive the
// `build` job's matrix (ci.yaml `needs.changes.outputs.components`), NOT the
// `test` job — `test` is gated solely by the `go-tests` channel, which globs
// all of internal/** and cmd/**, so Go tests always run on any Go change.
//
// So a missing entry does not mean untested code. It means a component's
// image is NOT rebuilt when one of its transitive dependencies changes, and
// the next deploy silently ships a stale image without the merged change.
// That is a nastier bug to chase than a test failure: the code is correct,
// reviewed, and merged, but the running binary predates it.
package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	// gopkg.in/yaml.v3 is already present in go.mod (as an indirect dep)
	// so we use it rather than adding a new dependency or hand-rolling a
	// parser for the known-shape path-filters.yaml file.
	"gopkg.in/yaml.v3"
)

// binaryChannels maps each deployed cmd binary to its channel name in
// .github/path-filters.yaml.  wiki-bundler is omitted because it is a
// build-time tool with no matrix entry in ci.yaml and no deployed image.
var binaryChannels = map[string]string{
	"api-gateway":           "api-gateway",
	"provision-worker":      "provision-worker",
	"crucible-engine":       "crucible-engine",
	"synthetic-api-monitor": "synthetic-api-monitor",
}

// findRepoRoot walks up from the test's working directory until it finds
// a go.mod file, then returns that directory.  Walking up is more robust than
// a hardcoded relative path because `go test` may be invoked from any
// subdirectory.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("go.mod not found walking upward from %s", wd)
	return "" // unreachable; satisfies the compiler (t.Fatalf calls runtime.Goexit)
}

// loadFilters parses .github/path-filters.yaml relative to root and returns
// a map from channel name to list of glob patterns.
func loadFilters(t *testing.T, root string) map[string][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "path-filters.yaml"))
	if err != nil {
		t.Fatalf("read path-filters.yaml: %v", err)
	}
	var m map[string][]string
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse path-filters.yaml: %v", err)
	}
	return m
}

// modulePrefix reads the module directive from go.mod (e.g.
// "github.com/jmal1/selfservice-api") and returns it with a trailing slash so
// it can be used as a string prefix when filtering `go list` output.
func modulePrefix(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")) + "/"
		}
	}
	t.Fatal("'module' directive not found in go.mod")
	return ""
}

// goListInternalDeps returns the set of internal packages (relative to the
// module root, e.g. "internal/config") that ./cmd/<name>/ transitively
// imports.
//
// We shell out to `go list -deps` rather than scanning import blocks because
// the transitive closure is the only ground truth: a package may arrive
// through a dep-of-a-dep rather than a direct import in cmd/x.  The trade-off
// is ~1 s per binary; acceptable for a guard test that is not on the hot path.
func goListInternalDeps(t *testing.T, root, modPrefix, name string) []string {
	t.Helper()
	c := exec.Command("go", "list", "-deps", "./cmd/"+name+"/")
	c.Dir = root
	out, err := c.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/%s/: %v", name, err)
	}
	var pkgs []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, modPrefix+"internal/") {
			pkgs = append(pkgs, strings.TrimPrefix(line, modPrefix))
		}
	}
	return pkgs
}

// globCoversPackage reports whether the dorny/paths-filter glob pattern covers
// the given package path (e.g. "internal/config/**" covers "internal/config"
// and "internal/config/sub").
//
// dorny/paths-filter treats ** as matching zero-or-more path segments, unlike
// Go's filepath.Match which does not support **.  We handle the two forms
// that appear in path-filters.yaml:
//
//   - "prefix/**"  → covers prefix itself and everything beneath it.
//   - "prefix/*"   → covers exactly one child level below prefix.
//   - anything else → exact string equality.
func globCoversPackage(glob, pkg string) bool {
	glob = filepath.ToSlash(glob)
	pkg = filepath.ToSlash(pkg)
	switch {
	case strings.HasSuffix(glob, "/**"):
		prefix := strings.TrimSuffix(glob, "/**")
		return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
	case strings.HasSuffix(glob, "/*"):
		prefix := strings.TrimSuffix(glob, "/*")
		if !strings.HasPrefix(pkg, prefix+"/") {
			return false
		}
		return !strings.Contains(strings.TrimPrefix(pkg, prefix+"/"), "/")
	default:
		return glob == pkg
	}
}

// isCovered reports whether pkg is matched by any glob in the list.
func isCovered(globs []string, pkg string) bool {
	for _, g := range globs {
		if globCoversPackage(g, pkg) {
			return true
		}
	}
	return false
}

// TestPathFilters_CoverAllInternalPackages verifies that every internal
// package a binary transitively imports is listed (directly or via the
// "shared" channel) in .github/path-filters.yaml.
//
// If this test fails, the error message names the missing package and which
// channel it belongs in so the fix is clear without reading the test source.
func TestPathFilters_CoverAllInternalPackages(t *testing.T) {
	root := findRepoRoot(t)
	filters := loadFilters(t, root)
	modPrefix := modulePrefix(t, root)

	shared := filters["shared"]

	var failures []string

	for cmd, channel := range binaryChannels {
		globs, ok := filters[channel]
		if !ok {
			failures = append(failures, fmt.Sprintf(
				"channel %q (for cmd/%s) not found in path-filters.yaml — add it",
				channel, cmd,
			))
			continue
		}

		for _, pkg := range goListInternalDeps(t, root, modPrefix, cmd) {
			if isCovered(shared, pkg) || isCovered(globs, pkg) {
				continue
			}
			failures = append(failures, fmt.Sprintf(
				"cmd/%s → %s  (add '%s/**' to the %q channel in .github/path-filters.yaml)",
				cmd, pkg, pkg, channel,
			))
		}
	}

	if len(failures) > 0 {
		t.Errorf(
			"path-filters.yaml is missing coverage for %d package(s).\n"+
				"A change to one of these will NOT rebuild the consuming component's\n"+
				"image, so the next deploy ships a stale binary without the change:\n"+
				"  %s\n\n"+
				"Edit .github/path-filters.yaml and add each missing path.",
			len(failures), strings.Join(failures, "\n  "),
		)
	}
}

// TestPathFilters_NoDanglingEntries verifies that every internal/, cmd/, and
// deploy/ path listed in .github/path-filters.yaml refers to a directory that
// exists on disk.  This catches typos and packages that were deleted without
// cleaning the filter file.
func TestPathFilters_NoDanglingEntries(t *testing.T) {
	root := findRepoRoot(t)
	filters := loadFilters(t, root)

	var failures []string

	for channel, globs := range filters {
		for _, glob := range globs {
			// Only validate structured repo paths; skip bare file globs
			// such as 'go.mod', 'Dockerfile', and '.github/**' which are
			// either single files or meta-paths not rooted in a package dir.
			if !strings.HasPrefix(glob, "internal/") &&
				!strings.HasPrefix(glob, "cmd/") &&
				!strings.HasPrefix(glob, "deploy/") {
				continue
			}

			// Derive the base directory by stripping any trailing wildcard.
			base := glob
			if i := strings.Index(base, "/**"); i >= 0 {
				base = base[:i]
			} else if i := strings.Index(base, "/*"); i >= 0 {
				base = base[:i]
			}

			dir := filepath.Join(root, filepath.FromSlash(base))
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				failures = append(failures, fmt.Sprintf(
					"channel %q: glob %q → directory %q does not exist on disk",
					channel, glob, base,
				))
			}
		}
	}

	if len(failures) > 0 {
		t.Errorf(
			"path-filters.yaml has %d dangling entries (directory does not exist).\n"+
				"Remove or correct these entries in .github/path-filters.yaml:\n"+
				"  %s",
			len(failures), strings.Join(failures, "\n  "),
		)
	}
}
