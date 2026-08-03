package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestImageKindFromFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
		wantErr  bool
	}{
		{"iso lower", "kali-2024.4-installer-amd64.iso", models.ImageKindISO, false},
		{"iso upper", "WINDOWS.ISO", models.ImageKindISO, false},
		{"iso mixed", "Ubuntu-24.04.Iso", models.ImageKindISO, false},
		{"ova", "appliance.ova", models.ImageKindOVA, false},
		{"ova upper", "APPLIANCE.OVA", models.ImageKindOVA, false},
		{"leading space tolerated", "  thing.iso  ", models.ImageKindISO, false},
		// Bare .ovf is useless without its sibling VMDKs — reject early
		// rather than failing confusingly at import time.
		{"bare ovf rejected", "descriptor.ovf", "", true},
		{"exe rejected", "payload.exe", "", true},
		{"vmdk rejected", "disk.vmdk", "", true},
		{"no extension", "kali", "", true},
		{"empty", "", "", true},
		{"extension only", ".iso", models.ImageKindISO, false},
		// Double extension must resolve on the LAST one, so
		// "evil.iso.exe" is an exe and is rejected.
		{"double extension uses last", "evil.iso.exe", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ImageKindFromFilename(tc.filename)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got kind %q", tc.filename, got)
				}
				if !errors.Is(err, ErrUnsupportedImageKind) {
					t.Fatalf("expected ErrUnsupportedImageKind, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.filename, err)
			}
			if got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeImageFilename(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "kali.iso", "kali.iso"},
		{"keeps dots dashes underscores", "kali-2024.4_amd64.iso", "kali-2024.4_amd64.iso"},
		{"spaces become dashes", "Windows Server 2025.iso", "Windows-Server-2025.iso"},
		{"unix traversal", "../../etc/passwd", "passwd"},
		{"windows traversal", `..\..\Windows\System32\cmd.exe`, "cmd.exe"},
		{"absolute unix path", "/var/lib/secret.iso", "secret.iso"},
		{"absolute windows path", `C:\images\thing.iso`, "thing.iso"},
		{"mixed separators", `foo/bar\baz.iso`, "baz.iso"},
		{"bare dotdot", "..", "image"},
		{"single dot", ".", "image"},
		{"empty", "", "image"},
		{"only bad chars", "!@#$%^&*()", "image"},
		{"strips quotes", `ka"li'.iso`, "kali.iso"},
		{"drops unicode", "kali-日本語.iso", "kali-.iso"},
		{"drops null byte", "kali\x00.iso", "kali.iso"},
		{"drops newline", "kali\n.iso", "kali.iso"},
		{"leading dots trimmed", "...hidden.iso", "hidden.iso"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeImageFilename(tc.input)
			if got != tc.want {
				t.Fatalf("SanitizeImageFilename(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// The whole point of sanitization: no input may produce a key that
// escapes the crucible/ prefix the MinIO service-account policy is
// scoped to.
func TestImageObjectKey_NeverEscapesPrefix(t *testing.T) {
	nasty := []string{
		"../../etc/passwd",
		`..\..\..\windows\system32\config\sam`,
		"/absolute/path.iso",
		"....//....//escape.iso",
		"a/../../b.iso",
		strings.Repeat("../", 50) + "deep.iso",
		"..",
		"",
		"normal.iso",
	}
	const id = "11111111-2222-3333-4444-555555555555"
	for _, in := range nasty {
		key := ImageObjectKey(id, in)

		if !strings.HasPrefix(key, "crucible/"+id+"/") {
			t.Errorf("key %q for input %q escaped the expected prefix", key, in)
		}
		if strings.Contains(key, "..") {
			t.Errorf("key %q for input %q contains a traversal sequence", key, in)
		}
		if strings.Count(key, "/") != 2 {
			t.Errorf("key %q for input %q has extra path separators", key, in)
		}
		if strings.Contains(key, `\`) {
			t.Errorf("key %q for input %q contains a backslash", key, in)
		}
	}
}

func TestSanitizeImageFilename_IsIdempotent(t *testing.T) {
	inputs := []string{
		"Windows Server 2025.iso", "../../etc/passwd", "kali-日本語.iso",
		"...hidden.iso", "", "!@#$", `C:\images\thing.iso`,
	}
	for _, in := range inputs {
		once := SanitizeImageFilename(in)
		twice := SanitizeImageFilename(once)
		if once != twice {
			t.Errorf("not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}

func TestSanitizeImageFilename_BoundsLength(t *testing.T) {
	got := SanitizeImageFilename(strings.Repeat("a", 500) + ".iso")
	if len(got) > 120 {
		t.Fatalf("length %d exceeds the 120-char bound", len(got))
	}
}

func TestPlanParts(t *testing.T) {
	const mib = 1 << 20
	const gib = 1 << 30
	tests := []struct {
		name string
		size int64
		part int64
		want int
	}{
		{"10 MiB is one part", 10 * mib, ImagePartSizeBytes, 1},
		{"exactly one part size", ImagePartSizeBytes, ImagePartSizeBytes, 1},
		{"one byte over rolls to two", ImagePartSizeBytes + 1, ImagePartSizeBytes, 2},
		{"12 GiB at 64 MiB parts", 12 * gib, ImagePartSizeBytes, 192},
		{"16 GiB cap at 64 MiB parts", 16 * gib, ImagePartSizeBytes, 256},
		{"zero size still gets a part", 0, ImagePartSizeBytes, 1},
		{"negative size still gets a part", -5, ImagePartSizeBytes, 1},
		{"zero part size falls back to default", 12 * gib, 0, 192},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanParts(tc.size, tc.part); got != tc.want {
				t.Fatalf("PlanParts(%d, %d) = %d, want %d", tc.size, tc.part, got, tc.want)
			}
		})
	}
}

// Every part must be at least 5 MiB or S3 rejects the multipart
// completion for all but the final chunk.
func TestImagePartSize_MeetsS3Minimum(t *testing.T) {
	const s3MinPartSize = 5 << 20
	if ImagePartSizeBytes < s3MinPartSize {
		t.Fatalf("part size %d is below the S3 minimum of %d", ImagePartSizeBytes, s3MinPartSize)
	}
}

func TestValidateImageUploadSize(t *testing.T) {
	tests := []struct {
		name       string
		size       int64
		wantStatus int
		wantErr    bool
	}{
		{"normal", 4 << 30, http.StatusOK, false},
		{"exactly at cap", MaxImageUploadBytes, http.StatusOK, false},
		{"one over cap", MaxImageUploadBytes + 1, http.StatusRequestEntityTooLarge, true},
		{"way over cap", 100 << 30, http.StatusRequestEntityTooLarge, true},
		{"zero", 0, http.StatusBadRequest, true},
		{"negative", -1, http.StatusBadRequest, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, err := ValidateImageUploadSize(tc.size)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error for size %d", tc.size)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for size %d: %v", tc.size, err)
			}
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// Phase 0 finding P0-4: MinIO sits on stagingv01's root filesystem with
// ~85 GB free, shared with apt-cacher-ng. This guards the cap against
// being casually raised back to the originally-planned 32 GiB.
func TestMaxImageUploadBytes_RespectsStagingDiskCeiling(t *testing.T) {
	const sixteenGiB = 16 << 30
	if MaxImageUploadBytes != sixteenGiB {
		t.Fatalf("upload cap is %d; expected 16 GiB (%d). MinIO shares an ~85 GB root "+
			"filesystem with apt-cacher-ng on stagingv01 — raising this needs a dedicated volume first",
			MaxImageUploadBytes, sixteenGiB)
	}
}

func TestPresignTTL_IsShort(t *testing.T) {
	if PresignTTLSeconds > 15*60 {
		t.Fatalf("presign TTL is %ds; must be <= 900s (a leaked URL is a write primitive into the bucket)", PresignTTLSeconds)
	}
}

// =============================================================================
// Handler integration tests (A6)
//
// These tests assert observable dependency behavior — not just HTTP status
// codes — by using fake implementations of imageDB and ImageStore that
// record every call and the arguments passed.
// =============================================================================

// --- fake imageDB ---

type fakeImageDB struct {
	// CreateImageUpload
	createCalls int
	createdImg  *models.ImageUpload
	createErr   error

	// GetImageUploadByID
	getImg *models.ImageUpload
	getErr error

	// ListImageUploads
	listImgs []models.ImageUpload
	listErr  error

	// SetImageUploadUploaded
	setUploadedCalls int
	setUploadedErr   error

	// SetImageUploadError
	setErrorCalls int
	setErrorMsg   string
	setErrorErr   error

	// DeleteImageUpload
	deleteCalls int
	deleteErr   error

	// CountTemplatesReferencingImage
	refCount    int
	refCountErr error

	// CreateJob
	createdJob   *models.Job
	createJobErr error
}

func (f *fakeImageDB) CreateImageUpload(_ context.Context, img *models.ImageUpload) error {
	f.createCalls++
	if f.createErr != nil {
		return f.createErr
	}
	img.ID = uuid.New()
	img.Status = models.ImageUploadPending
	img.CreatedAt = time.Now()
	img.UpdatedAt = time.Now()
	f.createdImg = img
	return nil
}

func (f *fakeImageDB) GetImageUploadByID(_ context.Context, _ uuid.UUID) (*models.ImageUpload, error) {
	return f.getImg, f.getErr
}

func (f *fakeImageDB) ListImageUploads(_ context.Context) ([]models.ImageUpload, error) {
	return f.listImgs, f.listErr
}

func (f *fakeImageDB) SetImageUploadUploaded(_ context.Context, _ uuid.UUID, _ int64) error {
	f.setUploadedCalls++
	return f.setUploadedErr
}

func (f *fakeImageDB) SetImageUploadError(_ context.Context, _ uuid.UUID, msg string) error {
	f.setErrorCalls++
	f.setErrorMsg = msg
	return f.setErrorErr
}

func (f *fakeImageDB) DeleteImageUpload(_ context.Context, _ uuid.UUID) error {
	f.deleteCalls++
	return f.deleteErr
}

func (f *fakeImageDB) CountTemplatesReferencingImage(_ context.Context, _, _ string) (int, error) {
	return f.refCount, f.refCountErr
}

func (f *fakeImageDB) CreateJob(_ context.Context, jobType string, _ []byte) (*models.Job, error) {
	if f.createJobErr != nil {
		return nil, f.createJobErr
	}
	if f.createdJob != nil {
		return f.createdJob, nil
	}
	return &models.Job{ID: uuid.New(), Type: jobType, Status: "pending"}, nil
}

// --- fake ImageStore ---

type fakeImageStore struct {
	presignCalls    int
	presignUploadID string
	presignURLs     []string
	presignErr      error

	completeCalls int
	completeErr   error

	statSize int64
	statErr  error

	removeCalls int
	removeErr   error

	usedBytes      int64
	usedBytesErr   error
	usedBytesCalls int
}

func (f *fakeImageStore) PresignMultipart(_ context.Context, _ string, _ int, _ time.Duration) (string, []string, error) {
	f.presignCalls++
	if f.presignErr != nil {
		return "", nil, f.presignErr
	}
	id := f.presignUploadID
	if id == "" {
		id = "test-upload-id"
	}
	urls := f.presignURLs
	if urls == nil {
		urls = []string{"https://s3.example.com/presigned/part1"}
	}
	return id, urls, nil
}

func (f *fakeImageStore) CompleteMultipart(_ context.Context, _, _ string, _ []minio.CompletePart) error {
	f.completeCalls++
	return f.completeErr
}

func (f *fakeImageStore) Stat(_ context.Context, _ string) (minio.ObjectInfo, error) {
	if f.statErr != nil {
		return minio.ObjectInfo{}, f.statErr
	}
	return minio.ObjectInfo{Size: f.statSize}, nil
}

func (f *fakeImageStore) Remove(_ context.Context, _ string) error {
	f.removeCalls++
	return f.removeErr
}

func (f *fakeImageStore) UsedBytes(_ context.Context) (int64, error) {
	f.usedBytesCalls++
	return f.usedBytes, f.usedBytesErr
}

// newImageHandler builds a *Handler wired with the given fakes.
func newImageHandler(db *fakeImageDB, store *fakeImageStore) *Handler {
	return &Handler{
		imgDB:      db,
		imageStore: store,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// withImageIDParam injects an {imageID} chi URL parameter into the request
// context so handlers that call chi.URLParam(r, "imageID") work in unit tests.
func withImageIDParam(r *http.Request, id uuid.UUID) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("imageID", id.String())
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// TestAdminCreateImageUpload_RejectsOversize asserts that a request whose
// size_bytes exceeds the cap gets a 413, AND that neither the object store
// presign nor the database create were ever called — over-limit files must
// not leave any dangling state.
func TestAdminCreateImageUpload_RejectsOversize(t *testing.T) {
	db := &fakeImageDB{}
	store := &fakeImageStore{}
	h := newImageHandler(db, store)

	// MaxImageUploadBytes is 16 GiB; send 16 GiB + 1 byte.
	body := `{"filename":"big.iso","size_bytes":17179869185}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.AdminCreateImageUpload(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", w.Code)
	}

	// No DB row must have been created.
	if db.createCalls != 0 {
		t.Errorf("DB.CreateImageUpload called %d time(s); want 0 — "+
			"oversize must be rejected before any DB write", db.createCalls)
	}
	// No presign must have been issued.
	if store.presignCalls != 0 {
		t.Errorf("store.PresignMultipart called %d time(s); want 0 — "+
			"oversize must be rejected before presigning", store.presignCalls)
	}
}

// TestAdminCreateImageUpload_IgnoresClientKind verifies that when the request
// body has a .iso filename, the server derives kind "iso" regardless of any
// other field the client might supply. This guards the critical security
// invariant: a caller cannot relabel a file type to force a different import path.
func TestAdminCreateImageUpload_IgnoresClientKind(t *testing.T) {
	db := &fakeImageDB{}
	store := &fakeImageStore{}
	h := newImageHandler(db, store)

	// 4 GiB is within limits.  CreateImageUploadRequest has no "kind"
	// JSON field (intentionally), but the test description says "client
	// sends kind:'ova' with a .iso filename".  We embed the unknown field
	// to confirm the decoder ignores it and the server derives from the extension.
	body := `{"filename":"kali-2025.iso","size_bytes":4294967296,"kind":"ova"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.AdminCreateImageUpload(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body = %s", w.Code, w.Body.String())
	}

	var resp CreateImageUploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Server must have derived "iso" from the extension, not "ova" from the client field.
	if resp.Kind != models.ImageKindISO {
		t.Errorf("response Kind = %q; want %q — server must derive kind from "+
			".iso extension, not trust the client", resp.Kind, models.ImageKindISO)
	}
	if db.createdImg == nil {
		t.Fatalf("DB.CreateImageUpload was not called")
	}
	if db.createdImg.Kind != models.ImageKindISO {
		t.Errorf("stored Kind = %q; want %q — DB row must reflect server-derived kind",
			db.createdImg.Kind, models.ImageKindISO)
	}
}

// TestAdminDeleteImage_RefusesReferenced asserts that when a template
// references the image, DELETE returns 409 Conflict AND the object store
// Remove method is never called — the referenced object must not be deleted.
func TestAdminDeleteImage_RefusesReferenced(t *testing.T) {
	id := uuid.New()
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:            id,
			Filename:      "kali.iso",
			Kind:          models.ImageKindISO,
			Status:        models.ImageUploadImported,
			ObjectKey:     "crucible/abc/kali.iso",
			DatastorePath: "[NAS] ISOs/kali.iso",
		},
		refCount: 2, // two templates reference this image
	}
	store := &fakeImageStore{}
	h := newImageHandler(db, store)

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 when image is referenced by templates", w.Code)
	}

	// The object store must NOT have been touched.
	if store.removeCalls != 0 {
		t.Errorf("store.Remove called %d time(s); want 0 — "+
			"must not delete object when referenced by a template", store.removeCalls)
	}

	// The DB row must NOT have been deleted.
	if db.deleteCalls != 0 {
		t.Errorf("DB.DeleteImageUpload called %d time(s); want 0 — "+
			"must not delete row when referenced by a template", db.deleteCalls)
	}
}

// TestAdminCompleteImageUpload_StatMismatch verifies that when the object
// store's Stat returns a different byte count from what was declared at
// upload-create time, the handler sets status to "error" (not "uploaded")
// and returns a non-200 status so the client knows the upload was rejected.
func TestAdminCompleteImageUpload_StatMismatch(t *testing.T) {
	id := uuid.New()
	const declaredSize = int64(4 << 30) // 4 GiB declared at create time
	const actualSize = int64(3 << 30)   // only 3 GiB actually stored

	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:        id,
			Filename:  "kali.iso",
			Kind:      models.ImageKindISO,
			Status:    models.ImageUploadPending,
			ObjectKey: "crucible/abc/kali.iso",
			UploadID:  "minio-upload-id",
			SizeBytes: declaredSize,
		},
	}
	store := &fakeImageStore{
		statSize: actualSize, // Stat returns fewer bytes than declared
	}
	h := newImageHandler(db, store)

	body := `{"parts":[{"part_number":1,"etag":"etag-abc"}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images/"+id.String()+"/complete",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminCompleteImageUpload(w, req)

	// Response must NOT be 200 OK — a truncated upload is not "uploaded".
	if w.Code == http.StatusOK {
		t.Errorf("status = 200; want non-200 on size mismatch — "+
			"a truncated upload must not transition to 'uploaded'")
	}

	// SetImageUploadError must have been called to record status=error.
	if db.setErrorCalls == 0 {
		t.Errorf("SetImageUploadError not called; want call to record error status on size mismatch")
	}
	// SetImageUploadUploaded must NOT have been called.
	if db.setUploadedCalls != 0 {
		t.Errorf("SetImageUploadUploaded called %d time(s); want 0 on size mismatch",
			db.setUploadedCalls)
	}
	if !strings.Contains(db.setErrorMsg, "size mismatch") {
		t.Errorf("error message %q does not contain 'size mismatch'", db.setErrorMsg)
	}
}

// TestAdminGetImage_MissingReturns404 verifies that GET for an unknown ID
// returns 404, never 500 — an unknown resource is a client error, not a
// server panic.
func TestAdminGetImage_MissingReturns404(t *testing.T) {
	db := &fakeImageDB{
		// pgx.ErrNoRows is what GetImageUploadByID returns for missing rows.
		getErr: pgx.ErrNoRows,
	}
	h := newImageHandler(db, &fakeImageStore{})

	unknownID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/admin/images/"+unknownID.String(), nil)
	req = withImageIDParam(req, unknownID)
	w := httptest.NewRecorder()
	h.AdminGetImage(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 for unknown image ID (never 500)", w.Code)
	}
}

// TestAdminCreateImageUpload_RefusesOverBudget proves the staging budget is
// actually ENFORCED, not merely declared. P0-4: MinIO shares stagingv01's
// ~85 GB root filesystem with apt-cacher-ng, so an unbounded staging area
// takes out every Linux template build too. Asserts nothing is presigned and
// no row is created, so a refused request leaves no dangling state.
func TestAdminCreateImageUpload_RefusesOverBudget(t *testing.T) {
	db := &fakeImageDB{}
	store := &fakeImageStore{
		usedBytes:       ImageStagingBudgetBytes - (1 << 30), // 1 GiB of headroom
		presignUploadID: "up-1",
		presignURLs:     []string{"https://example.invalid/part1"},
	}
	h := newImageHandler(db, store)

	body := `{"filename":"kali.iso","size_bytes":` + strconv.FormatInt(2<<30, 10) + `}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.AdminCreateImageUpload(w, req)

	if w.Code != http.StatusInsufficientStorage {
		t.Errorf("status = %d, want 507; body=%s", w.Code, w.Body.String())
	}
	if store.presignCalls != 0 {
		t.Errorf("presignCalls = %d, want 0 — a refused upload must not reserve storage", store.presignCalls)
	}
	if db.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 — a refused upload must not create a ghost row", db.createCalls)
	}
}

// TestAdminCreateImageUpload_AllowsWhenUsageUnknown pins the deliberate
// fail-open: a LIST hiccup must not block all uploads, because the per-upload
// cap still bounds the worst case. If this is ever changed to fail closed it
// should be a conscious decision, not an accident.
func TestAdminCreateImageUpload_AllowsWhenUsageUnknown(t *testing.T) {
	db := &fakeImageDB{}
	store := &fakeImageStore{
		usedBytesErr:    errors.New("minio unreachable"),
		presignUploadID: "up-1",
		presignURLs:     []string{"https://example.invalid/part1"},
	}
	h := newImageHandler(db, store)

	body := `{"filename":"kali.iso","size_bytes":1048576}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.AdminCreateImageUpload(w, req)

	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Errorf("status = %d, want 200/201; body=%s", w.Code, w.Body.String())
	}
	if store.usedBytesCalls != 1 {
		t.Errorf("usedBytesCalls = %d, want 1", store.usedBytesCalls)
	}
	if store.presignCalls != 1 {
		t.Errorf("presignCalls = %d, want 1 — upload should proceed when usage is unknown", store.presignCalls)
	}
}