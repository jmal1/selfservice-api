package main

import (
	"path/filepath"
	"testing"
)

// TestResolveOutDir_AbsoluteIsHonoured is a regression guard for a bug that
// made `make verify-wiki` fail unconditionally.
//
// verify-wiki generates a fresh bundle into `$(mktemp -d)` and diffs it against
// the committed bundle. mktemp returns an ABSOLUTE path; the bundler used to do
// filepath.Join(repoRoot, outDir), and filepath.Join("/repo", "/tmp/x") is
// "/repo/tmp/x". So the fresh bundle was written inside the repo, the temp dir
// stayed empty, and the diff reported every committed file as missing. The
// guard therefore reported "bundle is stale" whether or not it was — and, being
// wired into CI, blocked every downstream job.
//
// t.TempDir() is used rather than a hardcoded "/tmp/..." because a
// leading-slash path is not absolute on Windows (filepath.IsAbs wants a volume),
// which would make this test pass on CI and fail for developers.
func TestResolveOutDir_AbsoluteIsHonoured(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(t.TempDir(), "bundle-check")

	if !filepath.IsAbs(abs) {
		t.Fatalf("test setup: %q is not absolute", abs)
	}

	got := resolveOutDir(root, abs)
	if got != abs {
		t.Fatalf("absolute -out was rewritten: got %q, want %q "+
			"(joining an absolute -out onto the repo root writes the bundle "+
			"into the repo and leaves the requested directory empty)", got, abs)
	}
}

func TestResolveOutDir_RelativeIsRepoRooted(t *testing.T) {
	root := t.TempDir()
	rel := filepath.FromSlash("internal/docs/_bundle")

	got := resolveOutDir(root, rel)
	want := filepath.Join(root, rel)
	if got != want {
		t.Fatalf("relative -out not resolved against repo root: got %q, want %q", got, want)
	}
}
