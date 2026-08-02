package unattend

// iso.go wraps github.com/kdomanski/iso9660 (a pure-Go ISO9660 reader/writer)
// with small helpers for building a seed ISO from an in-memory set of files
// and for reading a flat ISO back (used by the seed generators' tests and by
// the CIData round-trip validation).

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kdomanski/iso9660"
)

// isoFile is a single file to place in a generated ISO. Path is the ISO path
// (e.g. "user-data" or "autounattend.xml"); nested paths use forward slashes.
type isoFile struct {
	Path string
	Data []byte
}

// buildISO writes the given files into a fresh ISO9660 image with the given
// volume label and returns the raw image bytes. Files are added in a
// deterministic (sorted) order so output is reproducible for identical input.
func buildISO(files []isoFile, volumeLabel string) ([]byte, error) {
	w, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("unattend: create iso writer: %w", err)
	}
	defer w.Cleanup()

	ordered := make([]isoFile, len(files))
	copy(ordered, files)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	for _, f := range ordered {
		if err := w.AddFile(bytes.NewReader(f.Data), f.Path); err != nil {
			return nil, fmt.Errorf("unattend: add %q to iso: %w", f.Path, err)
		}
	}

	var buf bytes.Buffer
	if err := w.WriteTo(&buf, volumeLabel); err != nil {
		return nil, fmt.Errorf("unattend: write iso: %w", err)
	}
	return buf.Bytes(), nil
}

// isoLabel returns the primary volume label of an ISO image.
func isoLabel(data []byte) (string, error) {
	img, err := iso9660.OpenImage(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return img.Label()
}

// isoReadRootFiles returns the regular files in the root directory of an ISO,
// keyed by their lower-cased name (ISO9660 identifiers are case-insensitive
// and this writer emits lower-case names).
func isoReadRootFiles(data []byte) (map[string][]byte, error) {
	img, err := iso9660.OpenImage(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	root, err := img.RootDir()
	if err != nil {
		return nil, err
	}
	children, err := root.GetChildren()
	if err != nil {
		return nil, err
	}

	files := make(map[string][]byte)
	for _, c := range children {
		if c.IsDir() {
			continue
		}
		r := c.Reader()
		if r == nil {
			continue
		}
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		files[strings.ToLower(c.Name())] = b
	}
	return files, nil
}
