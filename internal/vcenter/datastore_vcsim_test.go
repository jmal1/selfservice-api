package vcenter

// vcsim coverage for datastore.go.
//
// The datastore path helpers (ParseDatastorePath / DatastorePath) are pure and
// validate operator-supplied input, so they're table-driven and need no
// simulator. Upload / List / Delete run against a fresh vcsim VPX model booted
// by withSimulator (see guestops_vcsim_test.go). vcsim serves the datastore
// /folder HTTP endpoint and backs it with a real temp dir, so an uploaded file
// is visible to a subsequent HostDatastoreBrowser search and disappears after
// a delete — exactly the round-trips the image-upload flow depends on.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25"
)

const simDatastore = "LocalDS_0"

func TestParseDatastorePath(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantDS   string
		wantPath string
		wantErr  bool
	}{
		{name: "simple", in: "[NAS-BackupsAndISOS] ISOs/kali-2024.4.iso", wantDS: "NAS-BackupsAndISOS", wantPath: "ISOs/kali-2024.4.iso"},
		{name: "no subfolder", in: "[LocalDS_0] kali.iso", wantDS: "LocalDS_0", wantPath: "kali.iso"},
		{name: "extra spaces", in: "  [ds1]   dir/file.iso  ", wantDS: "ds1", wantPath: "dir/file.iso"},
		{name: "no brackets", in: "LocalDS_0 ISOs/kali.iso", wantErr: true},
		{name: "missing closing bracket", in: "[LocalDS_0 ISOs/kali.iso", wantErr: true},
		{name: "empty datastore", in: "[]  ISOs/kali.iso", wantErr: true},
		{name: "empty datastore no space", in: "[]", wantErr: true},
		{name: "empty path", in: "[LocalDS_0]", wantErr: true},
		{name: "empty path with spaces", in: "[LocalDS_0]    ", wantErr: true},
		{name: "path traversal", in: "[LocalDS_0] ../etc/passwd", wantErr: true},
		{name: "path traversal nested", in: "[LocalDS_0] ISOs/../../secret.iso", wantErr: true},
		{name: "empty input", in: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ds, p, err := ParseDatastorePath(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseDatastorePath(%q) = (%q, %q, nil), want error", tc.in, ds, p)
				}
				// The error must name the offending input so operators can act on it.
				if !strings.Contains(err.Error(), "invalid datastore path") {
					t.Errorf("error %q should mention 'invalid datastore path'", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDatastorePath(%q) unexpected error: %v", tc.in, err)
			}
			if ds != tc.wantDS || p != tc.wantPath {
				t.Errorf("ParseDatastorePath(%q) = (%q, %q), want (%q, %q)", tc.in, ds, p, tc.wantDS, tc.wantPath)
			}
		})
	}
}

func TestDatastorePath_RoundTrips(t *testing.T) {
	pairs := []struct{ ds, path string }{
		{"NAS-BackupsAndISOS", "ISOs/kali-2024.4.iso"},
		{"LocalDS_0", "kali.iso"},
		{"ds-1", "a/b/c/deep.iso"},
	}
	for _, p := range pairs {
		full := DatastorePath(p.ds, p.path)
		gotDS, gotPath, err := ParseDatastorePath(full)
		if err != nil {
			t.Fatalf("ParseDatastorePath(DatastorePath(%q,%q)=%q) error: %v", p.ds, p.path, full, err)
		}
		if gotDS != p.ds || gotPath != p.path {
			t.Errorf("round trip of (%q,%q) via %q = (%q,%q)", p.ds, p.path, full, gotDS, gotPath)
		}
	}
}

func TestUploadToDatastore_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		// A payload large enough to be read by the HTTP transport in one or
		// more chunks, so the progress callback fires at least once.
		payload := bytes.Repeat([]byte("crucible-iso-"), 6000) // ~78 KB
		size := int64(len(payload))

		var progress []int64
		cb := func(sent int64) { progress = append(progress, sent) }

		remote := "ISOs/kali-2024.4.iso"
		if err := c.UploadToDatastore(ctx, simDatastore, remote, bytes.NewReader(payload), size, cb); err != nil {
			t.Fatalf("UploadToDatastore: %v", err)
		}

		// Progress must have been reported, be monotonically non-decreasing,
		// and end exactly at the total size.
		if len(progress) == 0 {
			t.Fatal("progress callback was never invoked")
		}
		var prev int64
		for i, sent := range progress {
			if sent < prev {
				t.Fatalf("progress not monotonic at index %d: %d < %d", i, sent, prev)
			}
			prev = sent
		}
		if got := progress[len(progress)-1]; got != size {
			t.Errorf("final progress = %d, want %d", got, size)
		}

		// The uploaded file must now be discoverable via a recursive browse.
		files, err := c.ListDatastoreFiles(ctx, simDatastore, "ISOs", ".iso")
		if err != nil {
			t.Fatalf("ListDatastoreFiles: %v", err)
		}
		var found *DatastoreFile
		for i := range files {
			if files[i].Name == "kali-2024.4.iso" {
				found = &files[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("uploaded file not found in listing: %+v", files)
		}
		if found.SizeBytes != size {
			t.Errorf("listed size = %d, want %d", found.SizeBytes, size)
		}
		if found.FolderPath != "ISOs" {
			t.Errorf("FolderPath = %q, want %q", found.FolderPath, "ISOs")
		}
		wantPath := DatastorePath(simDatastore, "ISOs/kali-2024.4.iso")
		if found.Path != wantPath {
			t.Errorf("Path = %q, want %q", found.Path, wantPath)
		}
	})
}

func TestListDatastoreFiles_FiltersByExtension(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		up := func(remote string) {
			data := []byte("x")
			if err := c.UploadToDatastore(ctx, simDatastore, remote, bytes.NewReader(data), int64(len(data)), nil); err != nil {
				t.Fatalf("UploadToDatastore(%q): %v", remote, err)
			}
		}
		up("media/ubuntu.iso")
		up("media/notes.txt")
		up("media/WINDOWS.ISO") // uppercase extension proves case-insensitivity

		files, err := c.ListDatastoreFiles(ctx, simDatastore, "media", ".iso")
		if err != nil {
			t.Fatalf("ListDatastoreFiles: %v", err)
		}

		got := map[string]bool{}
		for _, f := range files {
			got[f.Name] = true
		}
		if !got["ubuntu.iso"] {
			t.Errorf("expected ubuntu.iso in results, got %v", keysOf(got))
		}
		if !got["WINDOWS.ISO"] {
			t.Errorf("expected WINDOWS.ISO (uppercase ext) in results, got %v", keysOf(got))
		}
		if got["notes.txt"] {
			t.Errorf(".txt file must be filtered out, got %v", keysOf(got))
		}
	})
}

func TestDeleteDatastoreFile_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, _ *vim25.Client) {
		remote := "trash/stale.iso"
		data := []byte("delete me")
		if err := c.UploadToDatastore(ctx, simDatastore, remote, bytes.NewReader(data), int64(len(data)), nil); err != nil {
			t.Fatalf("UploadToDatastore: %v", err)
		}

		before, err := c.ListDatastoreFiles(ctx, simDatastore, "trash", ".iso")
		if err != nil {
			t.Fatalf("ListDatastoreFiles(before): %v", err)
		}
		if !containsName(before, "stale.iso") {
			t.Fatalf("precondition failed: stale.iso not present before delete: %+v", before)
		}

		if err := c.DeleteDatastoreFile(ctx, simDatastore, remote); err != nil {
			t.Fatalf("DeleteDatastoreFile: %v", err)
		}

		after, err := c.ListDatastoreFiles(ctx, simDatastore, "trash", ".iso")
		if err != nil {
			t.Fatalf("ListDatastoreFiles(after): %v", err)
		}
		if containsName(after, "stale.iso") {
			t.Errorf("stale.iso still present after delete: %+v", after)
		}
	})
}

func containsName(files []DatastoreFile, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestValidateRemotePath_RejectsTraversal covers the write side of the path
// contract. ParseDatastorePath guards paths an operator *types*; this guards
// paths the code *builds* — the import job concatenates "ISOs/" with a filename
// that originated in a browser upload. A traversal here would be a
// write-anywhere/delete-anywhere primitive on a datastore shared with live VM
// disks and backups, so it must fail closed rather than rely on the API layer
// having sanitized correctly.
func TestValidateRemotePath_RejectsTraversal(t *testing.T) {
	bad := []string{
		"",
		"/ISOs/kali.iso",        // absolute: escapes to datastore root semantics
		"../secret.iso",         // classic traversal
		"ISOs/../../secret.iso", // traversal mid-path
		"ISOs/..",               // trailing traversal
		"ISOs/....//x.iso",      // ".." as a substring, not a whole segment
		`ISOs\..\secret.iso`,    // Windows separators bypass '/' splitting
		`ISOs\kali.iso`,         // wrong separator at all
	}
	for _, p := range bad {
		if err := validateRemotePath("op", p); err == nil {
			t.Errorf("validateRemotePath(%q) = nil; want error", p)
		}
	}

	good := []string{
		"kali.iso",
		"ISOs/kali.iso",
		"ISOs/seed/ubuntu-seed.iso",
		"ISOs/kali-2024.4-installer-amd64.iso", // dots in the filename are fine
	}
	for _, p := range good {
		if err := validateRemotePath("op", p); err != nil {
			t.Errorf("validateRemotePath(%q) = %v; want nil", p, err)
		}
	}
}

// Guard the two exported methods actually call the validator, so removing the
// check from either one fails a test rather than silently widening access.
func TestDatastoreWrites_RejectTraversal(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	if err := c.UploadToDatastore(ctx, simDatastore, "ISOs/../../evil.iso", bytes.NewReader(nil), 0, nil); err == nil {
		t.Error("UploadToDatastore accepted a traversal path")
	}
	if err := c.DeleteDatastoreFile(ctx, simDatastore, "ISOs/../../evil.iso"); err == nil {
		t.Error("DeleteDatastoreFile accepted a traversal path")
	}
}
