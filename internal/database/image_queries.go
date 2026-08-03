package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
)

// Queries for the image_uploads table (migration 000023) — staged ISO/OVA
// images on their way from an instructor's browser into vCenter.
//
// Lifecycle is enforced in the handlers/worker, not here; these helpers
// are deliberately thin so the state machine stays in one place
// (internal/provisioner/image_jobs.go).

// imageUploadSelectCols is the canonical column list for every
// image_uploads SELECT / RETURNING. Keep in lockstep with scanImageUpload.
const imageUploadSelectCols = `id, filename, kind, size_bytes, checksum_sha256,
		object_key, upload_id, status, datastore_path, vcenter_vm_id,
		error_message, uploaded_by, created_at, updated_at`

func scanImageUpload(row pgx.Row, img *models.ImageUpload) error {
	return row.Scan(
		&img.ID, &img.Filename, &img.Kind, &img.SizeBytes, &img.ChecksumSHA256,
		&img.ObjectKey, &img.UploadID, &img.Status, &img.DatastorePath, &img.VCenterVMID,
		&img.ErrorMessage, &img.UploadedBy, &img.CreatedAt, &img.UpdatedAt,
	)
}

// CreateImageUpload inserts a new staged-image row in `pending`. The
// caller is responsible for having already sanitized Filename and derived
// Kind from the extension server-side.
func (q *Queries) CreateImageUpload(ctx context.Context, img *models.ImageUpload) error {
	return scanImageUpload(q.pool.QueryRow(ctx, `
		INSERT INTO image_uploads (filename, kind, size_bytes, object_key, upload_id, status, uploaded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+imageUploadSelectCols+`
	`, img.Filename, img.Kind, img.SizeBytes, img.ObjectKey, img.UploadID,
		models.ImageUploadPending, img.UploadedBy), img)
}

// GetImageUploadByID returns a single staged image, or pgx.ErrNoRows.
func (q *Queries) GetImageUploadByID(ctx context.Context, id uuid.UUID) (*models.ImageUpload, error) {
	var img models.ImageUpload
	if err := scanImageUpload(q.pool.QueryRow(ctx, `
		SELECT `+imageUploadSelectCols+` FROM image_uploads WHERE id = $1
	`, id), &img); err != nil {
		return nil, err
	}
	return &img, nil
}

// ListImageUploads returns all staged images, newest first.
func (q *Queries) ListImageUploads(ctx context.Context) ([]models.ImageUpload, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+imageUploadSelectCols+` FROM image_uploads ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.ImageUpload{}
	for rows.Next() {
		var img models.ImageUpload
		if err := scanImageUpload(rows, &img); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// ListImageUploadsByStatus returns staged images in one of the given
// statuses, newest first. Used by the images picker in the template
// wizard (status='imported', kind='iso').
func (q *Queries) ListImageUploadsByStatus(ctx context.Context, statuses []string) ([]models.ImageUpload, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+imageUploadSelectCols+`
		FROM image_uploads WHERE status = ANY($1) ORDER BY created_at DESC
	`, statuses)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.ImageUpload{}
	for rows.Next() {
		var img models.ImageUpload
		if err := scanImageUpload(rows, &img); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// UpdateImageUploadStatus performs a guarded status transition. `from` is
// the expected current status; a mismatch returns ErrImageUploadStale so
// the caller can surface a 409 rather than silently double-importing.
func (q *Queries) UpdateImageUploadStatus(ctx context.Context, id uuid.UUID, from, to string) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE image_uploads SET status = $3, updated_at = now()
		WHERE id = $1 AND status = $2
	`, id, from, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: expected status %q", ErrImageUploadStale, from)
	}
	return nil
}

// SetImageUploadUploaded records the real object size + multipart upload
// id confirmed by Stat() and moves the row to `uploaded`.
func (q *Queries) SetImageUploadUploaded(ctx context.Context, id uuid.UUID, sizeBytes int64) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE image_uploads
		SET status = $2, size_bytes = $3, updated_at = now()
		WHERE id = $1 AND status IN ($4, $5)
	`, id, models.ImageUploadUploaded, sizeBytes,
		models.ImageUploadPending, models.ImageUploadUploading)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: not in pending/uploading", ErrImageUploadStale)
	}
	return nil
}

// SetImageUploadImported marks terminal success. Exactly one of
// datastorePath (ISO) / vcenterVMID (OVA) is expected to be non-empty.
func (q *Queries) SetImageUploadImported(ctx context.Context, id uuid.UUID, datastorePath, vcenterVMID, checksum string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE image_uploads
		SET status = $2, datastore_path = $3, vcenter_vm_id = $4,
		    checksum_sha256 = $5, error_message = '', updated_at = now()
		WHERE id = $1
	`, id, models.ImageUploadImported, datastorePath, vcenterVMID, checksum)
	return err
}

// SetImageUploadError marks terminal failure. The MinIO object is
// deliberately NOT removed by the caller on this path so a retry is cheap.
func (q *Queries) SetImageUploadError(ctx context.Context, id uuid.UUID, msg string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE image_uploads SET status = $2, error_message = $3, updated_at = now()
		WHERE id = $1
	`, id, models.ImageUploadError, msg)
	return err
}

// DeleteImageUpload removes the row. The caller must remove the MinIO
// object separately (and should do so first, so a failure leaves a
// recoverable row rather than an orphaned object).
func (q *Queries) DeleteImageUpload(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM image_uploads WHERE id = $1`, id)
	return err
}

// CountImageUploadsByStatus returns a status -> count map for every
// non-terminal status. Feeds the crucible_image_uploads_stuck gauge.
func (q *Queries) CountImageUploadsByStatus(ctx context.Context) (map[string]int, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT status, COUNT(*) FROM image_uploads GROUP BY status
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// CountStuckImageUploads returns how many rows have sat in a non-terminal
// status (uploading/importing) longer than olderThan. A non-zero value
// means either a browser walked away mid-upload or an import wedged;
// both leak MinIO storage on a host with only ~85 GB free.
func (q *Queries) CountStuckImageUploads(ctx context.Context, olderThan time.Duration) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM image_uploads
		WHERE status = ANY($1) AND updated_at < now() - $2::interval
	`, []string{models.ImageUploadUploading, models.ImageUploadImporting},
		fmt.Sprintf("%d seconds", int(olderThan.Seconds()))).Scan(&n)
	return n, err
}

// CountTemplatesReferencingImage reports how many templates point at this
// image's imported artifact, so DELETE can refuse with 409 instead of
// breaking a template's source_ref.
func (q *Queries) CountTemplatesReferencingImage(ctx context.Context, datastorePath, vcenterVMID string) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM templates
		WHERE ($1 <> '' AND source_ref = $1)
		   OR ($2 <> '' AND (source_ref = $2 OR vcenter_vm_id = $2))
	`, datastorePath, vcenterVMID).Scan(&n)
	return n, err
}
