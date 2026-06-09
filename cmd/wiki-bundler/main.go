// Command wiki-bundler walks a seed set of markdown files, follows
// relative links recursively, and copies the resulting closure of files
// into a bundle directory ready for embed.FS. A JSON manifest is written
// alongside so the API can serve a stable index without re-walking on
// every request.
//
// Why a build-time tool rather than runtime walking? The closure must be
// embedded with `//go:embed` so the api-gateway binary has no runtime
// dependency on the source tree, and embed paths are fixed at compile
// time. Computing the closure during `make build` keeps the bundle in
// sync with whatever the seed docs currently reference.
//
// Usage:
//
//	go run ./cmd/wiki-bundler \
//	    -repo-root .                                    \
//	    -out internal/docs/bundle                       \
//	    -seed AGENTS.md                                 \
//	    -seed docs/ai-prompts/build-workflow.md         \
//	    -seed docs/ai/build-workflow-prompt.md
//
// Exit codes:
//
//	0  bundle written successfully (or unchanged)
//	1  a referenced file is missing (link rot — fix the seed or remove the link)
//	2  bundle exceeded the safety cap (more files or bytes than expected)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Safety caps prevent a runaway closure from blowing up the binary. If
// we ever hit these the bundler exits non-zero rather than silently
// trimming — better to fail loudly than ship a half-bundle.
const (
	maxFiles      = 100
	maxTotalBytes = 4 * 1024 * 1024 // 4 MB
)

// allowedExts is the whitelist of file types the closure may contain.
// If a markdown link points to something outside this set we report it
// as a missing-extension warning and skip — most likely an external URL
// the author forgot to make absolute, or a .png embedded in docs we
// don't want to ship.
var allowedExts = map[string]bool{
	".md":   true,
	".go":   true,
	".sql":  true,
	".sh":   true,
	".json": true,
	".yaml": true,
	".yml":  true,
	".txt":  true,
}

// linkRegexp matches markdown inline links of the form `[text](path)`.
// It deliberately stops at `)` or `#` so anchor fragments are stripped
// (we serve whole files, not fragments). External links (http://,
// https://, mailto:) are filtered later — letting the regex match them
// is cheaper than two patterns.
var linkRegexp = regexp.MustCompile(`\]\(([^)#\s]+)(?:#[^)]*)?\)`)

// ManifestEntry describes one file in the bundle. Path is repo-relative
// (the same path used in the source tree and in the embed.FS); size and
// sha let the API report cache-friendly ETags and let CI catch
// inadvertent drift.
type ManifestEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	IsMarkdown bool   `json:"is_markdown"`
	// LinksOut is the list of in-closure files this file references. nil
	// for non-markdown files. Useful for the UI's "referenced by" panel.
	LinksOut []string `json:"links_out,omitempty"`
	// FromSeed indicates this file was passed in as a seed (top-level
	// instructor doc). Used by the UI to surface the "Start here" set.
	FromSeed bool `json:"from_seed"`
}

// Manifest is the top-level bundle index. It's both written to disk
// (bundle/manifest.json) and consumed by the API at runtime.
type Manifest struct {
	GeneratedAt string          `json:"generated_at,omitempty"` // optional; left blank for reproducible builds
	Seeds       []string        `json:"seeds"`
	Files       []ManifestEntry `json:"files"`
	TotalBytes  int64           `json:"total_bytes"`
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var (
		repoRoot string
		outDir   string
		seeds    stringSliceFlag
		verbose  bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the selfservice-api repo root")
	flag.StringVar(&outDir, "out", "internal/docs/_bundle", "output bundle directory (repo-relative)")
	flag.Var(&seeds, "seed", "seed markdown file (repo-relative); may be repeated")
	flag.BoolVar(&verbose, "v", false, "verbose logging")
	flag.Parse()

	if len(seeds) == 0 {
		fmt.Fprintln(os.Stderr, "wiki-bundler: -seed is required (at least one)")
		os.Exit(1)
	}

	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wiki-bundler: bad -repo-root: %v\n", err)
		os.Exit(1)
	}
	absOut := filepath.Join(absRoot, outDir)

	manifest, err := computeClosure(absRoot, seeds, verbose)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wiki-bundler: %v\n", err)
		// Distinguish the two failure modes so CI can branch on exit code.
		var lerr *linkRotError
		var serr *safetyCapError
		switch {
		case errors.As(err, &lerr):
			os.Exit(1)
		case errors.As(err, &serr):
			os.Exit(2)
		default:
			os.Exit(1)
		}
	}

	if err := writeBundle(absOut, absRoot, manifest, verbose); err != nil {
		fmt.Fprintf(os.Stderr, "wiki-bundler: write bundle: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("wiki-bundler: bundled %d files (%d bytes) into %s\n",
		len(manifest.Files), manifest.TotalBytes, outDir)
}

type linkRotError struct{ src, target string }

func (e *linkRotError) Error() string {
	return fmt.Sprintf("missing referenced file %q (linked from %q)", e.target, e.src)
}

type safetyCapError struct {
	kind     string
	got, max int64
}

func (e *safetyCapError) Error() string {
	return fmt.Sprintf("safety cap exceeded for %s: got %d, max %d", e.kind, e.got, e.max)
}

// computeClosure does a BFS from seeds, expanding markdown files only
// (other file types are leaves). Returns a sorted, deduplicated manifest.
func computeClosure(repoRoot string, seeds []string, verbose bool) (*Manifest, error) {
	visited := map[string]*ManifestEntry{}
	queue := append([]string(nil), seeds...)

	// Mark seeds so the UI can highlight them.
	seedSet := map[string]bool{}
	for _, s := range seeds {
		seedSet[filepath.ToSlash(s)] = true
	}

	for len(queue) > 0 {
		current := filepath.ToSlash(queue[0])
		queue = queue[1:]
		if _, seen := visited[current]; seen {
			continue
		}

		ext := strings.ToLower(filepath.Ext(current))
		if !allowedExts[ext] {
			if verbose {
				fmt.Fprintf(os.Stderr, "  skip (disallowed ext): %s\n", current)
			}
			continue
		}

		abs := filepath.Join(repoRoot, current)
		info, err := os.Stat(abs)
		if err != nil {
			return nil, &linkRotError{src: "<closure>", target: current}
		}
		if info.IsDir() {
			if verbose {
				fmt.Fprintf(os.Stderr, "  skip (directory): %s\n", current)
			}
			continue
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", current, err)
		}
		sum := sha256.Sum256(data)
		entry := &ManifestEntry{
			Path:       current,
			Size:       info.Size(),
			SHA256:     hex.EncodeToString(sum[:]),
			IsMarkdown: ext == ".md",
			FromSeed:   seedSet[current],
		}
		visited[current] = entry

		if entry.IsMarkdown {
			for _, link := range extractLinks(string(data)) {
				if isExternal(link) {
					continue
				}
				resolved := resolveLink(current, link)
				resExt := strings.ToLower(filepath.Ext(resolved))
				if !allowedExts[resExt] {
					if verbose {
						fmt.Fprintf(os.Stderr, "  skip (disallowed ext on link): %s -> %s\n", current, resolved)
					}
					continue
				}
				// Verify the link target exists. If not, fail loudly: a
				// broken link in the wiki is a worse user experience than
				// a failed build (the instructor would download a bundle
				// where their AI agent can't find half the references).
				if _, err := os.Stat(filepath.Join(repoRoot, resolved)); err != nil {
					return nil, &linkRotError{src: current, target: resolved}
				}
				entry.LinksOut = append(entry.LinksOut, resolved)
				if _, seen := visited[resolved]; !seen {
					queue = append(queue, resolved)
				}
			}
			sort.Strings(entry.LinksOut)
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "  +%s (%d bytes, %d outbound)\n", current, entry.Size, len(entry.LinksOut))
		}
	}

	// Materialize to a sorted slice.
	paths := make([]string, 0, len(visited))
	for p := range visited {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	m := &Manifest{Seeds: append([]string(nil), seeds...)}
	for _, p := range paths {
		e := visited[p]
		m.Files = append(m.Files, *e)
		m.TotalBytes += e.Size
	}

	if int64(len(m.Files)) > maxFiles {
		return nil, &safetyCapError{kind: "files", got: int64(len(m.Files)), max: maxFiles}
	}
	if m.TotalBytes > maxTotalBytes {
		return nil, &safetyCapError{kind: "bytes", got: m.TotalBytes, max: maxTotalBytes}
	}

	return m, nil
}

func extractLinks(md string) []string {
	matches := linkRegexp.FindAllStringSubmatch(md, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	return out
}

func isExternal(link string) bool {
	if link == "" {
		return true
	}
	low := strings.ToLower(link)
	for _, p := range []string{"http://", "https://", "mailto:", "ftp://"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// resolveLink turns a markdown link relative to the source file into a
// repo-relative slash path. Handles `./foo`, `../foo`, and bare names.
// Absolute paths (starting with `/`) are treated as repo-root anchored.
func resolveLink(srcRepoPath, link string) string {
	link = strings.TrimSpace(link)
	if strings.HasPrefix(link, "/") {
		return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(link)), "/")
	}
	srcDir := filepath.ToSlash(filepath.Dir(srcRepoPath))
	if srcDir == "." {
		srcDir = ""
	}
	joined := filepath.ToSlash(filepath.Clean(filepath.Join(srcDir, link)))
	return joined
}

// writeBundle wipes the output directory and writes the closure into it,
// preserving the original repo-relative paths so links like
// `[text](internal/foo.go)` resolve identically inside the bundle.
//
// Wiping rather than diffing is safe because the bundle is a build
// artifact — never edited by hand — and avoids stale files from a prior
// closure that has since shrunk.
func writeBundle(outDir, repoRoot string, m *Manifest, verbose bool) error {
	if err := os.RemoveAll(outDir); err != nil {
		return fmt.Errorf("remove existing bundle: %w", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	for _, entry := range m.Files {
		src := filepath.Join(repoRoot, entry.Path)
		dst := filepath.Join(outDir, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy %s: %w", entry.Path, err)
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "  wrote %s\n", dst)
		}
	}

	// Manifest written last so a partial bundle never produces a
	// readable index — if any prior step failed, callers will see no
	// manifest and refuse to serve the wiki.
	manifestPath := filepath.Join(outDir, "manifest.json")
	f, err := os.Create(manifestPath)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
