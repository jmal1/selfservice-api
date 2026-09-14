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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/unattend"
	"github.com/jmal1/selfservice-api/internal/vcenter"
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

// ImageStagingBudgetBytes caps the TOTAL size of objects Crucible keeps
// staged in MinIO at any one time.
//
// The S3 API cannot report the MinIO host's filesystem free space, so this
// deliberately bounds Crucible's own footprint rather than pretending to
// measure the disk. That is the half of the risk we actually control:
// per P0-4 stagingv01 has ~85 GB free on the root filesystem, so a 48 GiB
// ceiling leaves well over the 20 GB of headroom apt-cacher-ng and the OS
// need. Objects are removed on successful import, so this is a concurrency
// ceiling, not a lifetime quota.
const ImageStagingBudgetBytes int64 = 48 << 30

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
	Filename       string `json:"filename"`
	SizeBytes      int64  `json:"size_bytes"`
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

// --- Dependency interfaces used by the image handlers ---

// imageDB is the narrow database surface the image handlers require.
// Satisfied structurally by *database.Queries in production; use a fake
// in unit tests.
type imageDB interface {
	CreateImageUpload(ctx context.Context, img *models.ImageUpload) error
	GetImageUploadByID(ctx context.Context, id uuid.UUID) (*models.ImageUpload, error)
	ListImageUploads(ctx context.Context) ([]models.ImageUpload, error)
	ListImageUploadsByStatus(ctx context.Context, statuses []string) ([]models.ImageUpload, error)
	UpdateImageUploadStatus(ctx context.Context, id uuid.UUID, from, to string) error
	ResetImageUploadForRetry(ctx context.Context, id uuid.UUID) error
	SetImageUploadUploaded(ctx context.Context, id uuid.UUID, sizeBytes int64) error
	SetImageUploadError(ctx context.Context, id uuid.UUID, msg string) error
	DeleteImageUpload(ctx context.Context, id uuid.UUID) error
	CountTemplatesReferencingImage(ctx context.Context, datastorePath, vcenterVMID string) (int, error)
	HasActiveImageImportJob(ctx context.Context, imageID uuid.UUID) (bool, error)
	CreateJob(ctx context.Context, jobType string, payload []byte) (*models.Job, error)
}

// ISOEntry is one entry in the /admin/vcenter/isos response. It covers both
// ISOs discovered on the vCenter datastore (source="datastore") and ISOs that
// were uploaded through the image pipeline (source="uploaded"). Uploaded ISOs
// that are still in transit appear with disabled=true so the wizard can show
// them without making them selectable.
type ISOEntry struct {
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	FolderPath   string     `json:"folder_path,omitempty"`
	SizeBytes    int64      `json:"size_bytes,omitempty"`
	ModifiedTime *time.Time `json:"modified_time,omitempty"`
	// Source distinguishes entries: "uploaded" = came through the image pipeline,
	// "datastore" = found directly on the vCenter datastore by file browser.
	Source string `json:"source"`
	// Disabled is true when the entry is not yet usable as a template source
	// (e.g. still importing, or stuck in error). The wizard shows disabled
	// entries greyed-out rather than omitting them so instructors can see that
	// an import is in progress rather than wondering if their upload was lost.
	Disabled bool `json:"disabled"`
	// Status is populated for uploaded entries and mirrors image_uploads.status.
	Status string `json:"status,omitempty"`
	// ErrorMessage is set when Status=="error".
	ErrorMessage string `json:"error_message,omitempty"`
	// ImageID is the image_uploads.id for uploaded entries.
	ImageID string `json:"image_id,omitempty"`
}

// ImageStore is the narrow object-store interface the image handlers need.
// Satisfied structurally by *objectstore.Client in production.
type ImageStore interface {
	PresignMultipart(ctx context.Context, key string, parts int, ttl time.Duration) (uploadID string, urls []string, err error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []minio.CompletePart) error
	Stat(ctx context.Context, key string) (minio.ObjectInfo, error)
	Remove(ctx context.Context, key string) error
	UsedBytes(ctx context.Context) (int64, error)
}

// VCenterISOLister is the vCenter surface for browsing ISO datastores.
// Satisfied structurally by *vcenter.Client in production.
type VCenterISOLister interface {
	ListDatastoreFiles(ctx context.Context, datastore, folder, ext string) ([]vcenter.DatastoreFile, error)
}

// isoDatastoreCache caches the raw vCenter ISO listing for a configurable
// TTL — the same pattern as templateFolderCache in templates_folder.go.
type isoDatastoreCache struct {
	mu        sync.RWMutex
	data      []vcenter.DatastoreFile
	fetchedAt time.Time
	ttl       time.Duration
}

func (c *isoDatastoreCache) get() ([]vcenter.DatastoreFile, int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data == nil {
		return nil, 0, false
	}
	age := time.Since(c.fetchedAt)
	if age > c.ttl {
		return nil, 0, false
	}
	return c.data, int(age.Seconds()), true
}

func (c *isoDatastoreCache) set(files []vcenter.DatastoreFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = files
	c.fetchedAt = time.Now()
}

// --- Handlers ---

// AdminCreateImageUpload validates the filename and size, creates the
// image_uploads row, and returns presigned PUT URLs.
func (h *Handler) AdminCreateImageUpload(w http.ResponseWriter, r *http.Request) {
	if h.imageStore == nil || h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	var req CreateImageUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Filename == "" {
		respondError(w, r, http.StatusBadRequest, "filename is required")
		return
	}

	// Derive kind from the file extension SERVER-SIDE.  The client's kind
	// field is intentionally absent from CreateImageUploadRequest so it
	// cannot be supplied at all.
	kind, err := ImageKindFromFilename(req.Filename)
	if err != nil {
		respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Size cap checked BEFORE touching the object store or the database so
	// a rejected request never leaves dangling state.
	if status, verr := ValidateImageUploadSize(req.SizeBytes); verr != nil {
		respondError(w, r, status, verr.Error())
		return
	}

	// Refuse up front if this upload would breach the staging budget, rather
	// than discovering it partway through a multi-GB PUT. A failed usage
	// lookup is not fatal: the per-upload cap still bounds the damage, and
	// hard-failing here would make every upload depend on a LIST succeeding.
	if used, uerr := h.imageStore.UsedBytes(r.Context()); uerr != nil {
		h.logger.Warn("staging usage check failed; allowing upload", "error", uerr)
	} else if used+req.SizeBytes > ImageStagingBudgetBytes {
		respondError(w, r, http.StatusInsufficientStorage, fmt.Sprintf(
			"insufficient staging space: %d bytes already staged plus %d requested exceeds the %d byte budget; import or delete a pending image first",
			used, req.SizeBytes, ImageStagingBudgetBytes,
		))
		return
	}

	// Generate a unique token for the object key.  This is distinct from
	// the DB row ID (which is assigned by the database on INSERT) so we
	// can build the key before the row exists.
	uploadToken := uuid.New()
	objectKey := ImageObjectKey(uploadToken.String(), req.Filename)

	numParts := PlanParts(req.SizeBytes, ImagePartSizeBytes)
	uploadID, urls, err := h.imageStore.PresignMultipart(
		r.Context(), objectKey, numParts,
		time.Duration(PresignTTLSeconds)*time.Second,
	)
	if err != nil {
		h.logger.Error("presign multipart failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to create upload session")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	img := &models.ImageUpload{
		Filename:       SanitizeImageFilename(req.Filename),
		Kind:           kind,
		SizeBytes:      req.SizeBytes,
		ObjectKey:      objectKey,
		UploadID:       uploadID,
		ChecksumSHA256: req.ChecksumSHA256,
		UploadedBy:     &userID,
	}
	if err := h.imgDB.CreateImageUpload(r.Context(), img); err != nil {
		h.logger.Error("create image upload row failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.recordImageUpload(kind, "created")

	if h.db != nil {
		audit.Log(r.Context(), h.db, "image.upload.create",
			audit.Resource("image_upload", img.ID),
			audit.IP(r.RemoteAddr),
			audit.Detail("filename", req.Filename),
			audit.Detail("kind", kind),
			audit.Detail("size_bytes", fmt.Sprintf("%d", req.SizeBytes)),
		)
	}

	respondJSON(w, http.StatusCreated, CreateImageUploadResponse{
		ID:        img.ID.String(),
		Kind:      kind,
		ObjectKey: objectKey,
		UploadID:  uploadID,
		PartSize:  ImagePartSizeBytes,
		URLs:      urls,
		ExpiresIn: PresignTTLSeconds,
	})
}

// AdminCompleteImageUpload finalizes the multipart upload, Stats the
// object to confirm the real size, and moves the row to `uploaded`.
func (h *Handler) AdminCompleteImageUpload(w http.ResponseWriter, r *http.Request) {
	if h.imageStore == nil || h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	imageID, err := uuid.Parse(chi.URLParam(r, "imageID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid image id")
		return
	}

	img, err := h.imgDB.GetImageUploadByID(r.Context(), imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "image upload not found")
			return
		}
		h.logger.Error("get image upload failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	var req CompleteImageUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	// Convert our request parts to minio's CompletePart type.
	mp := make([]minio.CompletePart, len(req.Parts))
	for i, p := range req.Parts {
		mp[i] = minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}

	if err := h.imageStore.CompleteMultipart(r.Context(), img.ObjectKey, img.UploadID, mp); err != nil {
		h.logger.Error("complete multipart failed", "error", err, "id", imageID)
		h.recordImageUpload(img.Kind, "failed")
		respondError(w, r, http.StatusBadGateway, "failed to complete upload: "+err.Error())
		return
	}

	// Re-check actual object size via Stat.  A client may have PUT fewer
	// bytes than it declared; importing a truncated image produces a
	// corrupt vCenter template.
	info, err := h.imageStore.Stat(r.Context(), img.ObjectKey)
	if err != nil {
		h.logger.Error("stat object failed after complete", "error", err, "id", imageID)
		_ = h.imgDB.SetImageUploadError(r.Context(), imageID, "stat after complete failed: "+err.Error())
		h.recordImageUpload(img.Kind, "failed")
		respondError(w, r, http.StatusBadGateway, "failed to verify upload size")
		return
	}

	if info.Size != img.SizeBytes {
		msg := fmt.Sprintf("size mismatch: declared %d bytes, got %d bytes; upload rejected", img.SizeBytes, info.Size)
		_ = h.imgDB.SetImageUploadError(r.Context(), imageID, msg)
		h.recordImageUpload(img.Kind, "failed")
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":         "size_mismatch",
			"declared_size": img.SizeBytes,
			"actual_size":   info.Size,
			"message":       msg,
		})
		return
	}

	if err := h.imgDB.SetImageUploadUploaded(r.Context(), imageID, info.Size); err != nil {
		if errors.Is(err, database.ErrImageUploadStale) {
			// The row is not in pending/uploading. This can happen when the
			// complete endpoint is called a second time (client retry). Re-fetch
			// to determine whether the row is already past 'uploaded' (in which
			// case we treat this as an idempotent success) or in a truly
			// unexpected state (error → 409).
			current, rerr := h.imgDB.GetImageUploadByID(r.Context(), imageID)
			if rerr == nil {
				switch current.Status {
				case models.ImageUploadUploaded,
					models.ImageUploadImporting,
					models.ImageUploadImported:
					// Import is already queued or done. Return the current row
					// so the client can see what happened without re-enqueueing.
					respondJSON(w, http.StatusOK, current)
					return
				}
			}
			h.recordImageUpload(img.Kind, "failed")
			respondError(w, r, http.StatusConflict, "upload is in unexpected state")
			return
		}
		h.logger.Error("set uploaded status failed", "error", err, "id", imageID)
		h.recordImageUpload(img.Kind, "failed")
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	h.recordImageUpload(img.Kind, "completed")

	// Auto-enqueue the image_import job. Both ISOs and OVAs are imported
	// automatically so the instructor never has to take a separate "Import"
	// step. The stale-check above ensures we only reach this point once per
	// upload; durable retries re-enter while the row remains importing.
	//
	// Failure to enqueue is logged but NOT fatal: the upload is safe in MinIO
	// and the instructor can trigger import manually from /admin/images.
	jobPayload, _ := json.Marshal(map[string]string{"image_id": imageID.String()})
	job, enqErr := h.imgDB.CreateJob(r.Context(), models.JobTypeImageImport, jobPayload)
	if enqErr != nil {
		h.logger.Error("auto-enqueue image import failed; upload succeeded but import not started",
			"error", enqErr, "id", imageID)
	} else {
		if h.events != nil {
			if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
				h.logger.Warn("failed to publish auto-enqueued import job event",
					"error", err, "job_id", job.ID)
			}
		}
		if h.db != nil {
			audit.Log(r.Context(), h.db, "image.import.auto_enqueued",
				audit.Resource("image_upload", imageID),
				audit.IP(r.RemoteAddr),
				audit.Detail("job_id", job.ID.String()),
				audit.Detail("object_key", img.ObjectKey),
				audit.Detail("filename", img.Filename),
				audit.Detail("size_bytes", fmt.Sprintf("%d", info.Size)),
			)
		}
	}

	fresh, _ := h.imgDB.GetImageUploadByID(r.Context(), imageID)
	if fresh == nil {
		fresh = img
		fresh.Status = models.ImageUploadUploaded
	}
	respondJSON(w, http.StatusOK, fresh)
}

// AdminImportImage enqueues the image_import job that streams the object
// from MinIO into vCenter.
func (h *Handler) AdminImportImage(w http.ResponseWriter, r *http.Request) {
	if h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	imageID, err := uuid.Parse(chi.URLParam(r, "imageID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid image id")
		return
	}

	img, err := h.imgDB.GetImageUploadByID(r.Context(), imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "image upload not found")
			return
		}
		h.logger.Error("get image upload failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if img.Status != models.ImageUploadUploaded && img.Status != models.ImageUploadError {
		respondJSON(w, http.StatusConflict, map[string]any{
			"error":           "invalid_status",
			"current_status":  img.Status,
			"required_status": models.ImageUploadUploaded,
			"message":         "image must be in 'uploaded' or 'error' status to import; use 'error' to retry a failed import without re-uploading",
		})
		return
	}

	// Defense-in-depth: the object key is set server-side, but verify it
	// hasn't been corrupted (e.g. by a DB write racing with a rename).
	if !strings.HasPrefix(img.ObjectKey, "crucible/") {
		h.logger.Error("image object key does not start with crucible/ prefix; refusing import",
			"id", imageID, "object_key", img.ObjectKey)
		respondError(w, r, http.StatusInternalServerError, "invalid object key: key does not match expected prefix")
		return
	}
	active, err := h.imgDB.HasActiveImageImportJob(r.Context(), imageID)
	if err != nil {
		h.logger.Error("check active image import job failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if active {
		respondJSON(w, http.StatusConflict, map[string]any{
			"error":   "import_already_active",
			"message": "an image import job is already pending or running for this image",
		})
		return
	}

	// For error rows, reset to 'uploaded' so the import worker picks it up.
	// The MinIO object is deliberately retained on failure (see image_jobs.go)
	// so a retry is cheap — no re-upload needed.
	if img.Status == models.ImageUploadError {
		if img.ObjectKey == "" {
			respondJSON(w, http.StatusConflict, map[string]any{
				"error":   "no_staged_object",
				"message": "cannot retry: the staged object is no longer available (was deleted after a successful import?)",
			})
			return
		}
		if err := h.imgDB.ResetImageUploadForRetry(r.Context(), imageID); err != nil {
			h.logger.Error("reset error row to uploaded failed", "error", err, "id", imageID)
			respondError(w, r, http.StatusInternalServerError, "internal error")
			return
		}
	}

	payload, _ := json.Marshal(map[string]string{"image_id": imageID.String()})
	job, err := h.imgDB.CreateJob(r.Context(), models.JobTypeImageImport, payload)
	if err != nil {
		h.logger.Error("create image import job failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if h.events != nil {
		if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
		}
	}

	if h.db != nil {
		audit.Log(r.Context(), h.db, "image.import",
			audit.Resource("image_upload", imageID),
			audit.IP(r.RemoteAddr),
			audit.Detail("job_id", job.ID.String()),
		)
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id":   job.ID,
		"image_id": imageID,
		"status":   "pending",
	})
}

// AdminListImages lists staged images and their status.
func (h *Handler) AdminListImages(w http.ResponseWriter, r *http.Request) {
	if h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	images, err := h.imgDB.ListImageUploads(r.Context())
	if err != nil {
		h.logger.Error("list image uploads failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, images)
}

// AdminGetImage returns a single staged image.
func (h *Handler) AdminGetImage(w http.ResponseWriter, r *http.Request) {
	if h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	imageID, err := uuid.Parse(chi.URLParam(r, "imageID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid image id")
		return
	}

	img, err := h.imgDB.GetImageUploadByID(r.Context(), imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "image upload not found")
			return
		}
		h.logger.Error("get image upload failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, img)
}

// AdminDeleteImage removes the object and the row. It refuses with 409
// if a template still references the image. An unreferenced imported OVA
// also destroys the exact vcenter_vm_id VM so the next upload of the same
// filename does not fault DuplicateName.
func (h *Handler) AdminDeleteImage(w http.ResponseWriter, r *http.Request) {
	if h.imageStore == nil || h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	imageID, err := uuid.Parse(chi.URLParam(r, "imageID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid image id")
		return
	}

	img, err := h.imgDB.GetImageUploadByID(r.Context(), imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondError(w, r, http.StatusNotFound, "image upload not found")
			return
		}
		h.logger.Error("get image upload failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Check for template references BEFORE touching the object store.
	// A referenced image must not be deleted — doing so would break any
	// template whose source_ref or vcenter_vm_id points at it.
	refCount, err := h.imgDB.CountTemplatesReferencingImage(r.Context(), img.DatastorePath, img.VCenterVMID)
	if err != nil {
		h.logger.Error("count template references failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if refCount > 0 {
		respondJSON(w, http.StatusConflict, map[string]any{
			"error":          "image_referenced",
			"template_count": refCount,
			"message":        fmt.Sprintf("image is referenced by %d template(s); remove those references first", refCount),
		})
		return
	}

	// Unreferenced imported OVAs own a Templates-folder VM. Destroy that
	// exact moref before dropping MinIO/DB state so a failed destroy leaves
	// a recoverable row. ISO delete is unchanged: installer files are not
	// vCenter VMs. DestroyVM is idempotent when the VM is already gone.
	if img.Kind == models.ImageKindOVA && strings.TrimSpace(img.VCenterVMID) != "" {
		if h.vc == nil {
			respondError(w, r, http.StatusServiceUnavailable, "vCenter is not configured; cannot destroy the imported OVA VM")
			return
		}
		if err := h.vc.DestroyVM(r.Context(), img.VCenterVMID); err != nil {
			h.logger.Error("destroy imported OVA VM failed", "error", err, "id", imageID, "moref", img.VCenterVMID)
			respondError(w, r, http.StatusInternalServerError, "failed to destroy imported OVA VM: "+err.Error())
			return
		}
	}

	// Delete the object store object first so that a failure leaves a
	// recoverable row rather than an orphaned object with no row to find it.
	if img.ObjectKey != "" {
		if err := h.imageStore.Remove(r.Context(), img.ObjectKey); err != nil {
			h.logger.Error("remove object failed", "error", err, "key", img.ObjectKey)
			respondError(w, r, http.StatusInternalServerError, "failed to remove image object: "+err.Error())
			return
		}
	}

	if err := h.imgDB.DeleteImageUpload(r.Context(), imageID); err != nil {
		h.logger.Error("delete image upload row failed", "error", err, "id", imageID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if h.db != nil {
		audit.Log(r.Context(), h.db, "image.delete",
			audit.Resource("image_upload", imageID),
			audit.IP(r.RemoteAddr),
			audit.Detail("filename", img.Filename),
			audit.Detail("kind", img.Kind),
		)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) recordImageUpload(kind, result string) {
	if h != nil && h.imageMetrics != nil {
		h.imageMetrics.RecordImageUpload(kind, result)
	}
}

// AdminListVCenterISOs browses the ISO datastore so the wizard can offer
// ISOs that were placed there outside Crucible, merging in any ISOs that
// were uploaded through the image pipeline. The response uses the "isos"
// key (not "files") and each entry carries a "source" field so the wizard
// can visually distinguish pipeline-uploaded ISOs from ones placed directly
// on the datastore.
//
// OVAs are intentionally excluded: they cannot be mounted as CD-ROM media
// and must be deployed via OVF import, which is a separate flow.
func (h *Handler) AdminListVCenterISOs(w http.ResponseWriter, r *http.Request) {
	if h.isoLister == nil {
		respondError(w, r, http.StatusServiceUnavailable, "vCenter ISO browsing not configured")
		return
	}

	forceRefresh := r.URL.Query().Get("refresh") == "true"

	var (
		files  []vcenter.DatastoreFile
		age    int
		cached bool
	)
	if !forceRefresh && h.isoCache != nil {
		if cachedFiles, cachedAge, ok := h.isoCache.get(); ok {
			files, age, cached = cachedFiles, cachedAge, true
		}
	}
	if files == nil {
		fresh, err := h.isoLister.ListDatastoreFiles(r.Context(), h.isoDatastore, "", ".iso")
		if err != nil {
			respondError(w, r, http.StatusBadGateway, "failed to list ISO datastore: "+err.Error())
			return
		}
		if h.isoCache != nil {
			h.isoCache.set(fresh)
		}
		files = fresh
		cached = false
	}

	// Build the merged list. Start with entries we found on the datastore.
	// Track by datastore path so we can de-dup against uploaded rows.
	seenByPath := make(map[string]bool, len(files))
	isos := make([]ISOEntry, 0, len(files))
	for _, f := range files {
		// Provisioner-generated seed ISOs (cloud-init CIDATA / Windows
		// autounattend) live in this same folder but are non-bootable
		// unattended-install media, never valid installer sources. Drop
		// them so the wizard cannot offer them.
		if unattend.IsSeedISOFilename(f.Name) {
			continue
		}
		seenByPath[f.Path] = true
		mod := f.ModifiedTime
		isos = append(isos, ISOEntry{
			Name:         f.Name,
			Path:         f.Path,
			FolderPath:   f.FolderPath,
			SizeBytes:    f.SizeBytes,
			ModifiedTime: &mod,
			Source:       "datastore",
		})
	}

	// Layer in uploaded ISOs from the image pipeline. Rows with status
	// 'imported' and a non-empty datastore_path are authoritative — they
	// may not yet be visible to the cache. In-flight rows (uploading,
	// uploaded, importing) and error rows are surfaced as disabled entries
	// so the instructor knows what is happening without having to check a
	// separate page.
	if h.imgDB != nil {
		allStatuses := []string{
			models.ImageUploadUploading,
			models.ImageUploadUploaded,
			models.ImageUploadImporting,
			models.ImageUploadImported,
			models.ImageUploadError,
		}
		uploadedRows, lErr := h.imgDB.ListImageUploadsByStatus(r.Context(), allStatuses)
		if lErr != nil {
			h.logger.Warn("failed to list uploaded images for ISO merge; proceeding with datastore-only list",
				"error", lErr)
		} else {
			for _, u := range uploadedRows {
				// OVAs are never mounted as CD-ROMs; exclude them.
				if u.Kind != models.ImageKindISO {
					continue
				}

				switch u.Status {
				case models.ImageUploadImported:
					// If we already have this path from the datastore listing,
					// skip — the datastore entry is canonical.
					if seenByPath[u.DatastorePath] {
						continue
					}
					if u.DatastorePath == "" {
						continue
					}
					name := u.Filename
					isos = append(isos, ISOEntry{
						Name:    name,
						Path:    u.DatastorePath,
						Source:  "uploaded",
						ImageID: u.ID.String(),
						Status:  u.Status,
					})
					seenByPath[u.DatastorePath] = true

				case models.ImageUploadUploading,
					models.ImageUploadUploaded,
					models.ImageUploadImporting:
					isos = append(isos, ISOEntry{
						Name:     u.Filename,
						Source:   "uploaded",
						Disabled: true,
						Status:   u.Status,
						ImageID:  u.ID.String(),
					})

				case models.ImageUploadError:
					isos = append(isos, ISOEntry{
						Name:         u.Filename,
						Source:       "uploaded",
						Disabled:     true,
						Status:       u.Status,
						ErrorMessage: u.ErrorMessage,
						ImageID:      u.ID.String(),
					})
				}
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"isos":              isos,
		"datastore":         h.isoDatastore,
		"cached":            cached,
		"cache_age_seconds": age,
	})
}

// OVAEntry is one entry in the /admin/vcenter/ovas response: an OVA that
// came through the image pipeline, plus the source_ref an `ovf` template
// draft needs.
//
// This exists because an imported OVA was previously undiscoverable. The ISO
// picker excludes OVAs by design (they are never mounted as CD-ROM media),
// and the templates-folder browser lists VM names with no link back to the
// upload that produced them — so the vcenter_vm_id an instructor must paste
// into source_ref was only visible by reading the database.
type OVAEntry struct {
	Name     string `json:"name"`
	ImageID  string `json:"image_id"`
	Status   string `json:"status"`
	Disabled bool   `json:"disabled"`
	// SourceRef is the imported VM's vCenter MoRef (image_uploads.vcenter_vm_id).
	// Pass it as source_ref on a source_type=ovf draft. Empty while the
	// import is still in flight or has failed.
	SourceRef string `json:"source_ref,omitempty"`
	// Reason explains a disabled entry so the wizard can say why an OVA is
	// not selectable yet instead of silently greying it out.
	Reason       string    `json:"reason,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	SizeBytes    int64     `json:"size_bytes,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// AdminListVCenterOVAs handles GET /api/v1/admin/vcenter/ovas. It lists every
// OVA known to the image pipeline: imported ones are selectable and carry the
// source_ref for an `ovf` draft, while in-flight and failed ones are returned
// disabled with a reason — the same "show it, don't hide it" contract as the
// ISO picker, so an instructor can see an import in progress rather than
// wondering whether their upload was lost.
func (h *Handler) AdminListVCenterOVAs(w http.ResponseWriter, r *http.Request) {
	if h.imgDB == nil {
		respondError(w, r, http.StatusServiceUnavailable, "image upload not configured")
		return
	}

	rows, err := h.imgDB.ListImageUploadsByStatus(r.Context(), []string{
		models.ImageUploadUploading,
		models.ImageUploadUploaded,
		models.ImageUploadImporting,
		models.ImageUploadImported,
		models.ImageUploadError,
	})
	if err != nil {
		h.logger.Error("list image uploads for OVA catalog failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	ovas := make([]OVAEntry, 0, len(rows))
	for _, u := range rows {
		if u.Kind != models.ImageKindOVA {
			continue
		}
		entry := OVAEntry{
			Name:      u.Filename,
			ImageID:   u.ID.String(),
			Status:    u.Status,
			SizeBytes: u.SizeBytes,
			CreatedAt: u.CreatedAt,
		}
		switch u.Status {
		case models.ImageUploadImported:
			// An imported row with no MoRef cannot be used as a source and
			// is surfaced rather than dropped: silently omitting it would
			// hide a real inconsistency in the import that wrote it.
			if u.VCenterVMID == "" {
				entry.Disabled = true
				entry.Reason = "import finished without recording a vCenter VM id; re-import this OVA"
			} else {
				entry.SourceRef = u.VCenterVMID
			}
		case models.ImageUploadError:
			entry.Disabled = true
			entry.Reason = "import failed; retry with POST /admin/images/{id}/import"
			entry.ErrorMessage = u.ErrorMessage
		default:
			entry.Disabled = true
			entry.Reason = "still being uploaded or imported"
		}
		ovas = append(ovas, entry)
	}

	// Deterministic ordering; instructors scan this list visually.
	sort.Slice(ovas, func(i, j int) bool { return ovas[i].Name < ovas[j].Name })

	respondJSON(w, http.StatusOK, map[string]any{
		"ovas": ovas,
		// Echo the draft fields these entries feed, so the caller does not
		// have to hard-code the coupling between this list and the wizard.
		"source_type": models.TemplateSourceOVF,
	})
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
