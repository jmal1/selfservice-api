package vcenter

// datastore.go - Datastore file operations for the image-upload feature.
//
// Instructors upload installer media (ISOs) and appliance images to a
// datastore before a template/pod references them. These helpers cover the
// three operations the upload flow needs — stream a file up, enumerate what
// is already there, and delete stale media — plus the "[datastore] path"
// reference parsing/formatting that validates operator-supplied input.
//
// All vCenter round-trips resolve the datastore through the shared finder
// (which already has the datacenter set by Connect), exactly like
// template_ops.go. Read/delete operations are wrapped in withRetry so an
// expired session reconnects transparently; UploadToDatastore is not, because
// its request body is a non-replayable stream.

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// DatastoreFile describes a single file discovered by ListDatastoreFiles.
type DatastoreFile struct {
	Name         string // "kali-2024.4.iso"
	Path         string // "[NAS-BackupsAndISOS] ISOs/kali-2024.4.iso"
	FolderPath   string // "ISOs"
	SizeBytes    int64
	ModifiedTime time.Time
}

// countingReader wraps an io.Reader and reports cumulative bytes read to a
// callback. It lets UploadToDatastore surface upload progress without pulling
// in soap's progress.Sinker machinery.
type countingReader struct {
	r        io.Reader
	sent     int64
	progress func(sent int64)
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.sent += int64(n)
		if cr.progress != nil {
			cr.progress(cr.sent)
		}
	}
	return n, err
}

// validateRemotePath rejects datastore paths that could escape the intended
// folder. Callers build these from user-supplied filenames (an uploaded ISO
// name flows from the browser through image_uploads into "ISOs/"+filename), and
// this writes to a datastore shared with VM disks and backups. The API layer
// sanitizes filenames, but a traversal here would be a write-anywhere primitive
// on production storage, so it is re-checked at the boundary that does the write.
func validateRemotePath(op, remotePath string) error {
	if remotePath == "" {
		return fmt.Errorf("%s: remote path is required", op)
	}
	if strings.HasPrefix(remotePath, "/") {
		return fmt.Errorf("%s: remote path %q must be relative to the datastore root", op, remotePath)
	}
	// Reject backslashes outright rather than normalizing: vSphere treats the
	// path as POSIX-ish, so a Windows-style separator is a sign the caller
	// built the path wrong and may have bypassed sanitization.
	if strings.ContainsAny(remotePath, "\\") {
		return fmt.Errorf("%s: remote path %q must use '/' separators", op, remotePath)
	}
	for _, seg := range strings.Split(remotePath, "/") {
		if strings.Contains(seg, "..") {
			return fmt.Errorf("%s: remote path %q must not contain '..'", op, remotePath)
		}
	}
	return nil
}

// UploadToDatastore streams r into datastore at remotePath (e.g. "ISOs/kali.iso").
// progress may be nil; when non-nil it is called with cumulative bytes sent.
//
// size must be the exact number of bytes r will yield: it becomes the HTTP
// Content-Length, and vSphere rejects a short or long body.
func (c *Client) UploadToDatastore(ctx context.Context, datastore, remotePath string, r io.Reader, size int64, progress func(sent int64)) error {
	if datastore == "" {
		return fmt.Errorf("UploadToDatastore: datastore name is required")
	}
	if err := validateRemotePath("UploadToDatastore", remotePath); err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("UploadToDatastore: reader is required")
	}

	ds, err := c.finder.Datastore(ctx, datastore)
	if err != nil {
		return fmt.Errorf("find datastore %q: %w", datastore, err)
	}

	var body io.Reader = r
	if progress != nil {
		body = &countingReader{r: r, progress: progress}
	}

	// Start from DefaultUpload so Method ("PUT") and Type are populated; an
	// empty Method would make net/http default to GET and silently drop the body.
	up := soap.DefaultUpload
	up.ContentLength = size

	if err := ds.Upload(ctx, body, remotePath, &up); err != nil {
		return fmt.Errorf("upload to %s: %w", DatastorePath(datastore, remotePath), err)
	}
	return nil
}

// ListDatastoreFiles browses `folder` on `datastore` recursively and returns
// files matching extension `ext` (e.g. ".iso"). ext is matched
// case-insensitively; an empty ext returns every file.
func (c *Client) ListDatastoreFiles(ctx context.Context, datastore, folder, ext string) ([]DatastoreFile, error) {
	if datastore == "" {
		return nil, fmt.Errorf("ListDatastoreFiles: datastore name is required")
	}

	// Normalize the extension to a leading-dot form so callers can pass
	// either "iso" or ".iso".
	wantExt := ext
	if wantExt != "" && !strings.HasPrefix(wantExt, ".") {
		wantExt = "." + wantExt
	}

	var files []DatastoreFile
	err := c.withRetry(ctx, "list datastore files", func() error {
		ds, err := c.finder.Datastore(ctx, datastore)
		if err != nil {
			return fmt.Errorf("find datastore %q: %w", datastore, err)
		}

		browser, err := ds.Browser(ctx)
		if err != nil {
			return fmt.Errorf("open datastore browser: %w", err)
		}

		// Match everything server-side and filter by extension in Go: the
		// server-side match pattern is case-sensitive, so we cannot rely on it
		// for the case-insensitive extension contract.
		spec := types.HostDatastoreBrowserSearchSpec{
			MatchPattern: []string{"*"},
			Query:        []types.BaseFileQuery{&types.FileQuery{}},
			Details: &types.FileQueryFlags{
				FileType:     true,
				FileSize:     true,
				Modification: true,
			},
		}

		searchRoot := ds.Path(folder)
		task, err := browser.SearchDatastoreSubFolders(ctx, searchRoot, &spec)
		if err != nil {
			return fmt.Errorf("search %s: %w", searchRoot, err)
		}

		info, err := task.WaitForResult(ctx, nil)
		if err != nil {
			return fmt.Errorf("search %s: %w", searchRoot, err)
		}

		results, ok := info.Result.(types.ArrayOfHostDatastoreBrowserSearchResults)
		if !ok {
			return fmt.Errorf("unexpected datastore search result type %T", info.Result)
		}

		files = files[:0]
		for _, res := range results.HostDatastoreBrowserSearchResults {
			var folderRef object.DatastorePath
			folderRef.FromString(res.FolderPath)
			dsName := folderRef.Datastore
			relFolder := folderRef.Path

			for _, bf := range res.File {
				// Skip directory entries; only real files are of interest.
				if _, isDir := bf.(*types.FolderFileInfo); isDir {
					continue
				}
				fi := bf.GetFileInfo()
				if fi == nil || fi.Path == "" {
					continue
				}
				if wantExt != "" && !strings.EqualFold(path.Ext(fi.Path), wantExt) {
					continue
				}

				relPath := fi.Path
				if relFolder != "" {
					relPath = path.Join(relFolder, fi.Path)
				}
				var modified time.Time
				if fi.Modification != nil {
					modified = *fi.Modification
				}
				files = append(files, DatastoreFile{
					Name:         fi.Path,
					Path:         DatastorePath(dsName, relPath),
					FolderPath:   relFolder,
					SizeBytes:    fi.FileSize,
					ModifiedTime: modified,
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// DeleteDatastoreFile deletes remotePath from datastore.
func (c *Client) DeleteDatastoreFile(ctx context.Context, datastore, remotePath string) error {
	if datastore == "" {
		return fmt.Errorf("DeleteDatastoreFile: datastore name is required")
	}
	if err := validateRemotePath("DeleteDatastoreFile", remotePath); err != nil {
		return err
	}

	full := DatastorePath(datastore, remotePath)
	return c.withRetry(ctx, "delete datastore file", func() error {
		fm := object.NewFileManager(c.client.Client)
		task, err := fm.DeleteDatastoreFile(ctx, full, c.datacenter)
		if err != nil {
			return fmt.Errorf("delete %s: %w", full, err)
		}
		if err := task.Wait(ctx); err != nil {
			return fmt.Errorf("delete %s: %w", full, err)
		}
		return nil
	})
}

// DatastorePath formats a "[datastore] path" reference.
func DatastorePath(datastore, remotePath string) string {
	if remotePath == "" {
		return fmt.Sprintf("[%s]", datastore)
	}
	return fmt.Sprintf("[%s] %s", datastore, remotePath)
}

// GetDatastoreFreeBytes returns the free space in bytes on the named
// datastore. It is used by the import worker to gate import attempts on
// available capacity — uploading a 5 GB ISO to a nearly-full datastore
// would break every in-flight template build.
func (c *Client) GetDatastoreFreeBytes(ctx context.Context, datastore string) (int64, error) {
	if datastore == "" {
		return 0, fmt.Errorf("GetDatastoreFreeBytes: datastore name is required")
	}
	var freeBytes int64
	err := c.withRetry(ctx, "get datastore free bytes", func() error {
		ds, err := c.finder.Datastore(ctx, datastore)
		if err != nil {
			return fmt.Errorf("find datastore %q: %w", datastore, err)
		}

		pc := property.DefaultCollector(c.client.Client)
		var dsMo mo.Datastore
		if err := pc.RetrieveOne(ctx, ds.Reference(), []string{"summary"}, &dsMo); err != nil {
			return fmt.Errorf("retrieve datastore summary for %q: %w", datastore, err)
		}
		freeBytes = dsMo.Summary.FreeSpace
		return nil
	})
	if err != nil {
		return 0, err
	}
	return freeBytes, nil
}

// ParseDatastorePath splits "[datastore] path/file.iso" into its parts.
// Returns an error for malformed input — this is used to validate operator
// input, so it is deliberately strict and returns actionable error text.
func ParseDatastorePath(full string) (datastore, remotePath string, err error) {
	s := strings.TrimSpace(full)
	if !strings.HasPrefix(s, "[") {
		return "", "", fmt.Errorf("invalid datastore path %q: must be in the form \"[datastore] path/to/file\"", full)
	}

	end := strings.Index(s, "]")
	if end < 0 {
		return "", "", fmt.Errorf("invalid datastore path %q: missing closing ']' bracket, expected \"[datastore] path/to/file\"", full)
	}

	datastore = strings.TrimSpace(s[1:end])
	remotePath = strings.TrimSpace(s[end+1:])

	if datastore == "" {
		return "", "", fmt.Errorf("invalid datastore path %q: datastore name (between the brackets) is empty", full)
	}
	if remotePath == "" {
		return "", "", fmt.Errorf("invalid datastore path %q: file path (after the brackets) is empty", full)
	}
	if strings.Contains(remotePath, "..") {
		return "", "", fmt.Errorf("invalid datastore path %q: path must not contain \"..\"", full)
	}

	return datastore, remotePath, nil
}
