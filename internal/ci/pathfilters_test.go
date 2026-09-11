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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
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
//
// This map must list every component in ci.yaml's build matrix. The Kali
// runner image is owned by jmal1/selfservice-crucible-runner and is intentionally
// absent here.
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

// TestPathFilters_BinaryChannelsCoversCIMatrix verifies that binaryChannels
// above lists every component in ci.yaml's build matrix.
//
// Without this, the coverage guard silently skips whichever component someone
// forgets to add — which is precisely how crucible-runner ended up with a
// filter that never rebuilt it on a Go change.  The failure mode is a guard
// that passes while the thing it guards is broken, so the completeness of the
// map has to be enforced mechanically rather than by review.
//
// ci.yaml's authoritative list is the shell line:
//
//	all='["api-gateway",...,"crucible-runner"]'
func TestPathFilters_BinaryChannelsCoversCIMatrix(t *testing.T) {
	root := findRepoRoot(t)

	for _, c := range ciMatrixComponents(t, root) {
		if _, ok := binaryChannels[c]; !ok {
			t.Errorf(
				"ci.yaml builds component %q but binaryChannels does not list it, so\n"+
					"TestPathFilters_CoverAllInternalPackages silently skips it and its\n"+
					"image can stop rebuilding on a dependency change without any test failing.\n"+
					"Add %q to binaryChannels in this file.",
				c, c,
			)
		}
	}
}

// ciMatrixComponents returns the authoritative list of components ci.yaml builds,
// parsed from its `all='[...]'` line. Every guard that needs to enumerate
// components reads it from here so none of them can drift from the workflow.
func ciMatrixComponents(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatalf("read ci.yaml: %v", err)
	}

	m := regexp.MustCompile(`all='(\[[^']*\])'`).FindSubmatch(data)
	if m == nil {
		t.Fatal("could not find the `all='[...]'` component list in ci.yaml; " +
			"if the matrix moved, update this test rather than deleting it")
	}
	var components []string
	if err := json.Unmarshal(m[1], &components); err != nil {
		t.Fatalf("parse component list %q: %v", m[1], err)
	}
	if len(components) == 0 {
		t.Fatal("ci.yaml component list is empty; parse is wrong")
	}
	return components
}

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

// dockerfileForComponent resolves each CI matrix component to its Dockerfile by
// reading ci.yaml's "Resolve Dockerfile + target" step, so this cannot drift from
// the workflow that actually builds the images.
//
// All four Alpine API images use the root multi-target Dockerfile. The Kali
// runner image is built in jmal1/selfservice-crucible-runner.
func dockerfileForComponent(t *testing.T, root string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatalf("read ci.yaml: %v", err)
	}
	ci := string(data)

	if strings.Contains(ci, `file=deploy/runner/Dockerfile`) {
		t.Fatal("ci.yaml still maps crucible-runner to deploy/runner/Dockerfile; " +
			"runner builds moved to jmal1/selfservice-crucible-runner")
	}
	if !strings.Contains(ci, `echo "file=Dockerfile"`) {
		t.Fatal("ci.yaml no longer falls back to the root Dockerfile; " +
			"update dockerfileForComponent to match the workflow")
	}

	out := map[string]string{}
	for _, c := range ciMatrixComponents(t, root) {
		out[c] = "Dockerfile"
	}
	return out
}

// copyRe matches a COPY instruction's arguments.
var copyRe = regexp.MustCompile(`(?m)^\s*COPY\s+(.*)$`)

// TestPathFilters_CoverDockerfileCopySources asserts that every repo path a
// component's Dockerfile COPYs from is covered by that component's path-filter
// channel.
//
// TestPathFilters_CoverAllInternalPackages walks the *Go import graph*, so it
// structurally cannot see a dependency that exists only as a Dockerfile COPY.
func TestPathFilters_CoverDockerfileCopySources(t *testing.T) {
	root := findRepoRoot(t)
	filters := loadFilters(t, root)

	var missing []string
	for component, dockerfile := range dockerfileForComponent(t, root) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dockerfile)))
		if err != nil {
			t.Fatalf("read %s for component %q: %v", dockerfile, component, err)
		}
		globs, ok := filters[component]
		if !ok {
			t.Errorf("component %q has no channel in path-filters.yaml", component)
			continue
		}

		for _, m := range copyRe.FindAllStringSubmatch(string(data), -1) {
			args := strings.Fields(m[1])
			if len(args) < 2 {
				continue
			}
			srcs, fromStage := copySources(args)
			if fromStage {
				continue // intra-build stage copy; depends on no repo path
			}
			for _, src := range srcs {
				// "." is the whole build context. A channel cannot
				// meaningfully enumerate that, and for the root Dockerfile
				// the effective dependency is the Go import graph, which
				// TestPathFilters_CoverAllInternalPackages already covers.
				if src == "." || src == "./" {
					continue
				}
				if isCovered(globs, src) || isCovered(globs, path.Dir(src)) {
					continue
				}
				// Root-level files like go.mod live in the 'shared' channel,
				// which rebuilds everything.
				if isCovered(filters["shared"], src) {
					continue
				}
				missing = append(missing, fmt.Sprintf(
					"  %s COPYs %q but the %q channel does not cover it\n"+
						"    (add '%s/**' to the %q channel in .github/path-filters.yaml)",
					dockerfile, src, component, path.Dir(src), component))
			}
		}
	}

	if len(missing) > 0 {
		t.Errorf("path-filters.yaml is missing coverage for %d Dockerfile COPY source(s).\n"+
			"A change to one of these will NOT rebuild the image that consumes it, so the\n"+
			"next deploy ships an image built from stale inputs:\n%s",
			len(missing), strings.Join(missing, "\n"))
	}
}

// copySources splits a COPY instruction's arguments into its source paths,
// reporting whether it is a --from=<stage> copy.
func copySources(args []string) (srcs []string, fromStage bool) {
	for _, a := range args[:len(args)-1] {
		switch {
		case strings.HasPrefix(a, "--from="):
			return nil, true
		case strings.HasPrefix(a, "--"):
			continue // e.g. --chown, --chmod
		default:
			srcs = append(srcs, strings.Trim(a, `"`))
		}
	}
	return srcs, false
}
