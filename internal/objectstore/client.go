// Package objectstore is a thin client over MinIO/S3 used to stage large
// installer images (ISO/OVA) uploaded from an instructor's browser before a
// worker job streams them into vCenter.
//
// The client deliberately exposes only the small surface the image-upload
// pipeline needs: presigned single-part and multipart uploads, multipart
// finalize/abort, stat, streaming reads, delete and bucket bootstrap. All
// object keys are namespaced under a configured prefix via Key, which is a
// security boundary: the MinIO service account's policy is scoped to
// <bucket>/<prefix>/*.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MaxPresignTTL is the hard upper bound for any presigned URL lifetime. Callers
// requesting a longer TTL are clamped down to this value rather than erroring.
const MaxPresignTTL = 15 * time.Minute

// defaultPresignTTL is used when a caller passes a zero or negative TTL.
const defaultPresignTTL = MaxPresignTTL

// Config holds the connection settings for the object store.
type Config struct {
	Endpoint  string // e.g. "https://s3.lab.jmal.io" — may include scheme
	AccessKey string
	SecretKey string
	Bucket    string
	Prefix    string // e.g. "crucible"
	UseSSL    bool
}

// Client is a thin wrapper around a minio.Core scoped to a single bucket and
// key prefix.
type Client struct {
	core   *minio.Core
	bucket string
	prefix string

	// endpoint and useSSL record the values actually handed to minio-go after
	// scheme resolution. They are retained primarily for introspection/testing.
	endpoint string
	useSSL   bool
}

// New validates cfg and constructs a Client. It returns a clear error when a
// required field is missing. A scheme on Endpoint ("http://" / "https://") is
// stripped before the bare host is handed to minio-go, and UseSSL is derived
// from that scheme when one is present.
func New(cfg Config) (*Client, error) {
	switch {
	case strings.TrimSpace(cfg.Endpoint) == "":
		return nil, errors.New("objectstore: endpoint is required")
	case cfg.Bucket == "":
		return nil, errors.New("objectstore: bucket is required")
	case cfg.AccessKey == "":
		return nil, errors.New("objectstore: access key is required")
	case cfg.SecretKey == "":
		return nil, errors.New("objectstore: secret key is required")
	}

	endpoint, useSSL := resolveEndpoint(cfg.Endpoint, cfg.UseSSL)

	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: init client: %w", err)
	}

	return &Client{
		core:     core,
		bucket:   cfg.Bucket,
		prefix:   cfg.Prefix,
		endpoint: endpoint,
		useSSL:   useSSL,
	}, nil
}

// resolveEndpoint strips a leading http(s):// scheme (deriving UseSSL from it)
// and returns a bare host[:port] suitable for minio.New. When no scheme is
// present, defaultSSL is honoured.
func resolveEndpoint(raw string, defaultSSL bool) (host string, useSSL bool) {
	host = strings.TrimSpace(raw)
	lower := strings.ToLower(host)
	switch {
	case strings.HasPrefix(lower, "https://"):
		useSSL = true
		host = host[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		useSSL = false
		host = host[len("http://"):]
	default:
		useSSL = defaultSSL
	}
	// Drop any path/query so only host[:port] remains — minio-go rejects
	// fully-qualified paths on the endpoint.
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	return host, useSSL
}

// clampPresignTTL bounds a requested presign lifetime to (0, MaxPresignTTL].
// Zero or negative values fall back to a sensible positive default so a caller
// never receives an already-expired or error-inducing URL.
func clampPresignTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultPresignTTL
	}
	if ttl > MaxPresignTTL {
		return MaxPresignTTL
	}
	return ttl
}

// Key joins the configured prefix with parts using "/" and cleans the result so
// it can never escape the prefix. Backslashes are normalised to "/", and empty,
// ".", and ".." segments are dropped. The returned key always begins with
// "<prefix>/" (when a prefix is configured), enforcing the storage boundary.
func (c *Client) Key(parts ...string) string {
	prefix := strings.Trim(strings.ReplaceAll(c.prefix, "\\", "/"), "/")
	joined := strings.Join(cleanSegments(parts), "/")
	if prefix == "" {
		return joined
	}
	return prefix + "/" + joined
}

// cleanSegments splits every part on both slash flavours and keeps only safe,
// non-empty path segments.
func cleanSegments(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ReplaceAll(p, "\\", "/")
		for _, seg := range strings.Split(p, "/") {
			// Drop empty, "." and any segment containing "..". The last check
			// is intentionally broader than a literal ".." so that dotted
			// oddities like "...." can never surface as a ".." substring in the
			// final key.
			if seg == "" || seg == "." || strings.Contains(seg, "..") {
				continue
			}
			out = append(out, seg)
		}
	}
	return out
}

// PresignPut returns a presigned PUT URL for a single-part upload.
func (c *Client) PresignPut(ctx context.Context, key string, ttl time.Duration) (string, error) {
	u, err := c.core.PresignedPutObject(ctx, c.bucket, key, clampPresignTTL(ttl))
	if err != nil {
		return "", fmt.Errorf("objectstore: presign put %q: %w", key, err)
	}
	return u.String(), nil
}

// PresignMultipart starts a multipart upload and returns the upload ID plus one
// presigned URL per part (1..parts). If any part fails to presign, the started
// upload is aborted best-effort so it does not strand storage.
func (c *Client) PresignMultipart(ctx context.Context, key string, parts int, ttl time.Duration) (uploadID string, urls []string, err error) {
	if parts < 1 {
		return "", nil, fmt.Errorf("objectstore: parts must be >= 1, got %d", parts)
	}
	ttl = clampPresignTTL(ttl)

	uploadID, err = c.core.NewMultipartUpload(ctx, c.bucket, key, minio.PutObjectOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("objectstore: start multipart %q: %w", key, err)
	}

	urls = make([]string, 0, parts)
	for part := 1; part <= parts; part++ {
		params := url.Values{}
		params.Set("uploadId", uploadID)
		params.Set("partNumber", strconv.Itoa(part))

		u, perr := c.core.Presign(ctx, http.MethodPut, c.bucket, key, ttl, params)
		if perr != nil {
			// Best-effort cleanup so a partial failure doesn't leave an
			// in-flight upload consuming storage on the space-constrained host.
			_ = c.core.AbortMultipartUpload(context.WithoutCancel(ctx), c.bucket, key, uploadID)
			return "", nil, fmt.Errorf("objectstore: presign part %d for %q: %w", part, key, perr)
		}
		urls = append(urls, u.String())
	}
	return uploadID, urls, nil
}

// CompleteMultipart finalizes a multipart upload.
func (c *Client) CompleteMultipart(ctx context.Context, key, uploadID string, parts []minio.CompletePart) error {
	if _, err := c.core.CompleteMultipartUpload(ctx, c.bucket, key, uploadID, parts, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: complete multipart %q: %w", key, err)
	}
	return nil
}

// AbortMultipart cancels an in-flight multipart upload so orphaned parts don't
// consume storage.
func (c *Client) AbortMultipart(ctx context.Context, key, uploadID string) error {
	if err := c.core.AbortMultipartUpload(ctx, c.bucket, key, uploadID); err != nil {
		return fmt.Errorf("objectstore: abort multipart %q: %w", key, err)
	}
	return nil
}

// Stat returns object metadata for key.
func (c *Client) Stat(ctx context.Context, key string) (minio.ObjectInfo, error) {
	info, err := c.core.StatObject(ctx, c.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return minio.ObjectInfo{}, fmt.Errorf("objectstore: stat %q: %w", key, err)
	}
	return info, nil
}

// Open returns a streaming reader for key plus the object size. The caller is
// responsible for closing the returned reader.
func (c *Client) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	rc, info, _, err := c.core.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("objectstore: open %q: %w", key, err)
	}
	return rc, info.Size, nil
}

// Remove deletes key from the bucket.
func (c *Client) Remove(ctx context.Context, key string) error {
	if err := c.core.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("objectstore: remove %q: %w", key, err)
	}
	return nil
}

// UsedBytes returns the total size of every object stored under the client's
// configured prefix. It is used to bound how much Crucible stages at once;
// the S3 API cannot report the host's actual filesystem free space.
func (c *Client) UsedBytes(ctx context.Context) (int64, error) {
	prefix := strings.Trim(strings.ReplaceAll(c.prefix, "\\", "/"), "/")
	if prefix != "" {
		prefix += "/"
	}
	var total int64
	for obj := range c.core.Client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return 0, fmt.Errorf("objectstore: list %q: %w", prefix, obj.Err)
		}
		total += obj.Size
	}
	return total, nil
}

// EnsureBucket creates the configured bucket if it does not already exist.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.core.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("objectstore: check bucket %q: %w", c.bucket, err)
	}
	if exists {
		return nil
	}
	if err := c.core.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("objectstore: create bucket %q: %w", c.bucket, err)
	}
	return nil
}
