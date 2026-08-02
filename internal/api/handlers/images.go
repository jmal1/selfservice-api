package handlers

// images.go — Epic A: image upload (browser → MinIO → vCenter).
//
// WAVE 1 SCAFFOLD. The route wiring, request/response shapes, and the
// pure helper functions below are final; the handler bodies marked
// "not implemented" are filled in by Lane API (a6-api) and Lane STORE
// (a2-objectstore / a5-import-job).
//
// Routing note (corrected by Phase 0 finding P0-2): these are registered
// as a nested r.Route("/images", …) INSIDE the existing /admin block in
// routes.go, which already carries RequireRole(RoleInstructor). Do not
// add a second top-level /admin/images block — chi's Mount takes
// exclusive ownership of a prefix and the routes would be shadowed.
//
// Security invariants that must survive implementation:
//   - `kind` is derived from the file extension SERVER-SIDE and never
//     trusted from the client.
//   - Object keys are sanitized so they cannot escape the crucible/
//     prefix (no "..", no path separators, no absolute paths).
//   - Presigned URLs are scoped to one key and expire in 15 minutes.
//   - The size cap is enforced before presigning AND re-checked at
//     complete-time via Stat, because a client can PUT more bytes than
//     it declared.

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/jmal1/selfservice-api/internal/models"
)

// PresignTTLSeconds is the lifetime of every presigned upload URL.
// Deliberately short: a leaked URL is a write primitive into the bucket.
const PresignTTLSeconds = 15 * 60

// MaxImageUploadBytes caps a single upload at 16 GiB.
//
// Phase 0 finding P0-4: MinIO's data directory lives on stagingv01's
// ROOT filesystem with only ~85 GB free and no dedicated volume. The
// same filesystem hosts apt-cacher-ng, which every Linux template build
// depends on — so filling it has a blast radius well beyond uploads.
// 16 GiB is large enough for any Windows Server or Kali installer while
// leaving headroom for concurrent uploads.
const MaxImageUploadBytes int64 = 16 << 30

// MinFreeBytesAfterUpload is the free-space floor on the MinIO host. A
// create request that would breach it is refused with 507 rather than
// discovering the problem partway through a multi-GB PUT.
const MinFreeBytesAfterUpload int64 = 20 << 30

// ImagePartSizeBytes is the multipart chunk size. S3 requires >= 5 MiB
// for all but the final part; 64 MiB keeps the part count reasonable for
// a 16 GiB object (256 parts) while staying small enough that a failed
// part is cheap to retry.
const ImagePartSizeBytes int64 = 64 << 20

// ErrUnsupportedImageKind is returned when a filename's extension is not
// one we can import. Bare .ovf is rejected on purpose: without the
// sibling VMDKs an OVF descriptor is not importable, and accepting it
// would produce a confusing half-failure at import time.
var ErrUnsupportedImageKind = errors.New("unsupported image type: only .iso and .ova are accepted")

// CreateImageUploadRequest is the body of POST /admin/images.
//
// Note there is no `kind` field — it is derived from Filename. Accepting
// a client-supplied kind would let a caller upload an .exe labelled as
// an .iso and have the worker hand it to vCenter.
type CreateImageUploadRequest struct {
	Filename   string `json:"filename"`
	SizeBytes  int64  `json:"size_bytes"`
	ChecksumSHA256 string `json:"checksum_sha256,omitempty"`
}

// CreateImageUploadResponse hands the browser everything it needs to PUT
// the file directly to object storage without proxying through the API.
type CreateImageUploadResponse struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	ObjectKey string   `json:"object_key"`
	UploadID  string   `json:"upload_id"`
	PartSize  int64    `json:"part_size"`
	URLs      []string `json:"urls"`
	ExpiresIn int      `json:"expires_in"`
}

// CompletedPart is one finished multipart chunk, echoed back from the
// browser with the ETag the storage layer returned.
type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// CompleteImageUploadRequest is the body of POST /admin/images/{id}/complete.
type CompleteImageUploadRequest struct {
	Parts []CompletedPart `json:"parts"`
}

// --- Handlers (Wave 1 stubs — see file header) ---

// AdminCreateImageUpload validates the filename and size, creates the
// image_uploads row, and returns presigned PUT URLs.
func (h *Handler) AdminCreateImageUpload(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminCompleteImageUpload finalizes the multipart upload, Stats the
// object to confirm the real size, and moves the row to `uploaded`.
func (h *Handler) AdminCompleteImageUpload(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminImportImage enqueues the image_import job that streams the object
// from MinIO into vCenter.
func (h *Handler) AdminImportImage(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminListImages lists staged images and their status.
func (h *Handler) AdminListImages(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminGetImage returns a single staged image.
func (h *Handler) AdminGetImage(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminDeleteImage removes the object and the row. It must refuse with
// 409 if a template still references the image.
func (h *Handler) AdminDeleteImage(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// AdminListVCenterISOs browses the ISO datastore so the wizard can offer
// ISOs that were placed there outside Crucible.
func (h *Handler) AdminListVCenterISOs(w http.ResponseWriter, r *http.Request) {
	respondNotImplemented(w, "a6-api")
}

// respondNotImplemented is the Wave 1 placeholder. It returns 501 with
// the owning todo id so a stub reaching an environment is immediately
// traceable to the lane that still owes the implementation.
func respondNotImplemented(w http.ResponseWriter, todo string) {
	respondJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "not implemented yet",
		"todo":  todo,
	})
}

// --- Pure helpers (implemented now; A9 tests target these directly) ---

// ImageKindFromFilename derives the image kind from the file extension.
// This is the ONLY place kind is determined — see the security note in
// the file header.
func ImageKindFromFilename(filename string) (string, error) {
	ext := strings.ToLower(path.Ext(strings.TrimSpace(filename)))
	switch ext {
	case ".iso":
		return models.ImageKindISO, nil
	case ".ova":
		return models.ImageKindOVA, nil
	default:
		return "", ErrUnsupportedImageKind
	}
}

// SanitizeImageFilename reduces an arbitrary client-supplied filename to
// a safe basename: no directory components, no traversal, no control or
// non-ASCII characters, and a bounded length.
//
// It intentionally does NOT try to preserve the caller's exact name —
// the display name is stored separately in the `filename` column; this
// value only ever forms part of an object key.
func SanitizeImageFilename(filename string) string {
	// Strip any directory component from BOTH separator conventions
	// before anything else, so "..\..\etc\passwd" and "../../etc/passwd"
	// both collapse to the final element.
	filename = strings.ReplaceAll(filename, "\\", "/")
	filename = path.Base(path.Clean("/" + filename))

	var b strings.Builder
	for _, r := range filename {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '\t':
			// Only real horizontal whitespace becomes a dash. Newlines
			// and other control characters are dropped outright — using
			// unicode.IsSpace here would turn "kali\n.iso" into
			// "kali-.iso", silently encoding an injection attempt into
			// the stored key instead of erasing it.
			b.WriteRune('-')
		default:
			// Drop everything else: control chars, unicode homoglyphs,
			// quoting characters, and path separators that survived.
		}
	}
	out := strings.Trim(b.String(), ".-")

	// Collapse any run of dots so ".." can never reappear.
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	if len(out) > 120 {
		out = out[:120]
	}
	if out == "" {
		out = "image"
	}
	return out
}

// ImageObjectKey builds the storage key for an upload. The crucible/
// prefix is what the scoped MinIO service account policy is bound to, so
// escaping it would also escape the credential's authorization.
func ImageObjectKey(imageID, filename string) string {
	return fmt.Sprintf("crucible/%s/%s", imageID, SanitizeImageFilename(filename))
}

// PlanParts returns how many multipart chunks a file of the given size
// needs. Zero-length and negative sizes yield a single part so the
// caller still gets a usable presigned URL.
func PlanParts(sizeBytes, partSize int64) int {
	if partSize <= 0 {
		partSize = ImagePartSizeBytes
	}
	if sizeBytes <= partSize {
		return 1
	}
	parts := sizeBytes / partSize
	if sizeBytes%partSize != 0 {
		parts++
	}
	return int(parts)
}

// ValidateImageUploadSize enforces the cap. Returns the HTTP status the
// handler should use alongside the error, so the 413-vs-400 decision
// lives in one place.
func ValidateImageUploadSize(sizeBytes int64) (int, error) {
	if sizeBytes <= 0 {
		return http.StatusBadRequest, errors.New("size_bytes must be greater than zero")
	}
	if sizeBytes > MaxImageUploadBytes {
		return http.StatusRequestEntityTooLarge, fmt.Errorf(
			"file is %d bytes; the maximum upload size is %d bytes (16 GiB)",
			sizeBytes, MaxImageUploadBytes)
	}
	return http.StatusOK, nil
}
