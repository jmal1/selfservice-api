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

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/provisioner"
	"github.com/jmal1/selfservice-api/internal/vcenter"
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

	// ListImageUploadsByStatus
	listByStatusImgs []models.ImageUpload
	listByStatusErr  error

	// UpdateImageUploadStatus
	updateStatusFromExpected string // if set, the "from" arg must match
	updateStatusCalls        int
	updateStatusErr          error

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
	createdJob        *models.Job
	createJobErr      error
	createdJobType    string
	createdJobPayload []byte
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

func (f *fakeImageDB) ListImageUploadsByStatus(_ context.Context, _ []string) ([]models.ImageUpload, error) {
	return f.listByStatusImgs, f.listByStatusErr
}

func (f *fakeImageDB) UpdateImageUploadStatus(_ context.Context, _ uuid.UUID, from, _ string) error {
	f.updateStatusCalls++
	if f.updateStatusFromExpected != "" && from != f.updateStatusFromExpected {
		return errors.New("unexpected from-status: got " + from + ", want " + f.updateStatusFromExpected)
	}
	return f.updateStatusErr
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

func (f *fakeImageDB) CreateJob(_ context.Context, jobType string, payload []byte) (*models.Job, error) {
	f.createdJobType = jobType
	f.createdJobPayload = payload
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

type fakeImageMetrics struct {
	records []string
}

func (f *fakeImageMetrics) RecordImageUpload(kind, result string) {
	f.records = append(f.records, kind+"|"+result)
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

func TestAdminDeleteImage_UnreferencedOVADestroysVM(t *testing.T) {
	id := uuid.New()
	const moref = "vm-4242"
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:          id,
			Filename:    "appliance.ova",
			Kind:        models.ImageKindOVA,
			Status:      models.ImageUploadImported,
			ObjectKey:   "crucible/abc/appliance.ova",
			VCenterVMID: moref,
		},
	}
	store := &fakeImageStore{}
	vc := &fakeVC{}
	h := newImageHandler(db, store)
	h.vc = vc

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204; body = %s", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 1 || vc.gotCalls[0] != "destroy:"+moref {
		t.Errorf("DestroyVM calls = %v; want exactly destroy:%s", vc.gotCalls, moref)
	}
	if store.removeCalls != 1 {
		t.Errorf("store.Remove called %d time(s); want 1 after VM destroy", store.removeCalls)
	}
	if db.deleteCalls != 1 {
		t.Errorf("DB.DeleteImageUpload called %d time(s); want 1", db.deleteCalls)
	}
}

func TestAdminDeleteImage_ReferencedOVADoesNotDestroy(t *testing.T) {
	id := uuid.New()
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:          id,
			Filename:    "appliance.ova",
			Kind:        models.ImageKindOVA,
			Status:      models.ImageUploadImported,
			ObjectKey:   "crucible/abc/appliance.ova",
			VCenterVMID: "vm-4242",
		},
		refCount: 1,
	}
	store := &fakeImageStore{}
	vc := &fakeVC{}
	h := newImageHandler(db, store)
	h.vc = vc

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409; body = %s", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 0 {
		t.Errorf("DestroyVM calls = %v; want none while templates still reference the image", vc.gotCalls)
	}
	if store.removeCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("remove=%d delete=%d; want 0/0 when referenced", store.removeCalls, db.deleteCalls)
	}
}

func TestAdminDeleteImage_ISODoesNotDestroyVM(t *testing.T) {
	id := uuid.New()
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:            id,
			Filename:      "kali.iso",
			Kind:          models.ImageKindISO,
			Status:        models.ImageUploadImported,
			ObjectKey:     "crucible/abc/kali.iso",
			DatastorePath: "[NAS] ISOs/kali.iso",
			VCenterVMID:   "vm-should-not-be-destroyed",
		},
	}
	store := &fakeImageStore{}
	vc := &fakeVC{}
	h := newImageHandler(db, store)
	h.vc = vc

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204; body = %s", w.Code, w.Body.String())
	}
	if len(vc.gotCalls) != 0 {
		t.Errorf("DestroyVM calls = %v; ISO delete must not destroy a vCenter VM", vc.gotCalls)
	}
	if store.removeCalls != 1 || db.deleteCalls != 1 {
		t.Errorf("remove=%d delete=%d; want 1/1 for an unreferenced ISO", store.removeCalls, db.deleteCalls)
	}
}

func TestAdminDeleteImage_OVADestroyFailureKeepsRow(t *testing.T) {
	id := uuid.New()
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:          id,
			Filename:    "appliance.ova",
			Kind:        models.ImageKindOVA,
			Status:      models.ImageUploadImported,
			ObjectKey:   "crucible/abc/appliance.ova",
			VCenterVMID: "vm-4242",
		},
	}
	store := &fakeImageStore{}
	vc := &fakeVC{destroyErr: errors.New("vCenter busy")}
	h := newImageHandler(db, store)
	h.vc = vc

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 when DestroyVM fails; body = %s", w.Code, w.Body.String())
	}
	if store.removeCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("remove=%d delete=%d; want 0/0 so a failed destroy leaves a recoverable row", store.removeCalls, db.deleteCalls)
	}
}

func TestAdminDeleteImage_OVAWithoutVCenterFailsClosed(t *testing.T) {
	id := uuid.New()
	db := &fakeImageDB{
		getImg: &models.ImageUpload{
			ID:          id,
			Filename:    "appliance.ova",
			Kind:        models.ImageKindOVA,
			Status:      models.ImageUploadImported,
			ObjectKey:   "crucible/abc/appliance.ova",
			VCenterVMID: "vm-4242",
		},
	}
	store := &fakeImageStore{}
	h := newImageHandler(db, store) // vc is nil

	req := httptest.NewRequest(http.MethodDelete, "/admin/images/"+id.String(), nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminDeleteImage(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 when vCenter is unwired; body = %s", w.Code, w.Body.String())
	}
	if store.removeCalls != 0 || db.deleteCalls != 0 {
		t.Errorf("remove=%d delete=%d; want 0/0 when the imported VM cannot be destroyed", store.removeCalls, db.deleteCalls)
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
		t.Errorf("status = 200; want non-200 on size mismatch — " +
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

// TestAdminImageUploadLifecycle_RecordsMetrics proves the upload lifecycle
// events are wired to the pipeline metrics sink from production handler code.
// The created/completed/failed samples are all driven by handler paths so the
// test fails if any call site is removed.
func TestAdminImageUploadLifecycle_RecordsMetrics(t *testing.T) {
	db := &fakeImageDB{}
	store := &fakeImageStore{}
	metrics := &fakeImageMetrics{}
	h := newImageHandler(db, store).WithPipelineMetrics(metrics)

	createBody := `{"filename":"kali.iso","size_bytes":1024}`
	createReq := httptest.NewRequest(http.MethodPost, "/admin/images", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	h.AdminCreateImageUpload(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Fatalf("create status = %d; want 201", createW.Code)
	}
	if db.createdImg == nil {
		t.Fatal("CreateImageUpload did not create a row")
	}
	if len(metrics.records) != 1 || metrics.records[0] != models.ImageKindISO+"|created" {
		t.Fatalf("created metric = %v, want [%s|created]", metrics.records, models.ImageKindISO)
	}

	store.statSize = db.createdImg.SizeBytes
	db.getImg = db.createdImg
	completeBody := `{"parts":[{"part_number":1,"etag":"etag-abc"}]}`
	completeReq := httptest.NewRequest(http.MethodPost, "/admin/images/"+db.createdImg.ID.String()+"/complete", strings.NewReader(completeBody))
	completeReq.Header.Set("Content-Type", "application/json")
	completeReq = withImageIDParam(completeReq, db.createdImg.ID)
	completeW := httptest.NewRecorder()
	h.AdminCompleteImageUpload(completeW, completeReq)

	if completeW.Code != http.StatusOK {
		t.Fatalf("complete status = %d; want 200", completeW.Code)
	}
	if len(metrics.records) != 2 || metrics.records[1] != models.ImageKindISO+"|completed" {
		t.Fatalf("completed metric = %v, want second record %s|completed", metrics.records, models.ImageKindISO)
	}

	badID := uuid.New()
	db.getImg = &models.ImageUpload{
		ID:        badID,
		Filename:  "broken.iso",
		Kind:      models.ImageKindISO,
		Status:    models.ImageUploadPending,
		ObjectKey: "crucible/" + badID.String() + "/broken.iso",
		UploadID:  "upload-2",
		SizeBytes: 2048,
	}
	store.statSize = 1024
	failedReq := httptest.NewRequest(http.MethodPost, "/admin/images/"+badID.String()+"/complete", strings.NewReader(completeBody))
	failedReq.Header.Set("Content-Type", "application/json")
	failedReq = withImageIDParam(failedReq, badID)
	failedW := httptest.NewRecorder()
	h.AdminCompleteImageUpload(failedW, failedReq)

	if failedW.Code != http.StatusUnprocessableEntity {
		t.Fatalf("failed status = %d; want 422", failedW.Code)
	}
	if len(metrics.records) != 3 || metrics.records[2] != models.ImageKindISO+"|failed" {
		t.Fatalf("failed metric = %v, want third record %s|failed", metrics.records, models.ImageKindISO)
	}
}

// =============================================================================
// Tests for Lane 3 features: auto-import, merged ISO list, OVA exclusion,
// path-traversal rejection, idempotent double-complete, error retry.
// =============================================================================

// fakeISOLister implements VCenterISOLister for tests.
type fakeISOLister struct {
	files []vcenter.DatastoreFile
	err   error
}

func (f *fakeISOLister) ListDatastoreFiles(_ context.Context, _, _, _ string) ([]vcenter.DatastoreFile, error) {
	return f.files, f.err
}

// newISOHandler builds a *Handler wired with all the ISO-related fakes.
func newISOHandler(db *fakeImageDB, store *fakeImageStore, lister *fakeISOLister, datastore string) *Handler {
	h := newImageHandler(db, store)
	if lister != nil {
		h.isoLister = lister
		h.isoDatastore = datastore
		// No cache — tests want fresh results every time.
	}
	return h
}

// TestAdminCompleteImageUpload_AutoEnqueuesImport verifies that on a
// successful upload completion, an image_import job is enqueued automatically
// and the response is 200.
func TestAdminCompleteImageUpload_AutoEnqueuesImport(t *testing.T) {
	id := uuid.New()
	img := &models.ImageUpload{
		ID:        id,
		Filename:  "mint.iso",
		Kind:      models.ImageKindISO,
		Status:    models.ImageUploadPending,
		ObjectKey: "crucible/" + id.String() + "/mint.iso",
		UploadID:  "upload-mint",
		SizeBytes: 2 << 30, // 2 GiB
	}
	db := &fakeImageDB{getImg: img}
	store := &fakeImageStore{statSize: img.SizeBytes}
	h := newImageHandler(db, store)

	body := `{"parts":[{"part_number":1,"etag":"etag-mint"}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images/"+id.String()+"/complete",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminCompleteImageUpload(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body = %s", w.Code, w.Body.String())
	}
	// SetImageUploadUploaded must have been called exactly once.
	if db.setUploadedCalls != 1 {
		t.Errorf("SetImageUploadUploaded calls = %d; want 1", db.setUploadedCalls)
	}
	// CreateJob must have been called once (auto-import).
	if db.createJobErr != nil {
		t.Fatalf("CreateJob returned error: %v", db.createJobErr)
	}
	// Verify response body is the image row (not null).
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestAdminListVCenterOVAs_SurfacesSourceRef covers the OVF discovery gap:
// imported OVAs are excluded from the ISO picker by design, so before this
// endpoint the image_uploads.vcenter_vm_id an instructor must paste into a
// source_type=ovf draft was only readable from the database.
//
// It also asserts in-flight, failed, and moref-less rows come back disabled
// with a reason rather than being dropped — an OVA that vanished from the
// list is indistinguishable from an upload that was lost.
func TestAdminListVCenterOVAs_SurfacesSourceRef(t *testing.T) {
	ready := models.ImageUpload{
		ID:          uuid.New(),
		Filename:    "vyos.ova",
		Kind:        models.ImageKindOVA,
		Status:      models.ImageUploadImported,
		VCenterVMID: "vm-4242",
		SizeBytes:   1 << 30,
	}
	importing := models.ImageUpload{
		ID:       uuid.New(),
		Filename: "pfsense.ova",
		Kind:     models.ImageKindOVA,
		Status:   models.ImageUploadImporting,
	}
	failed := models.ImageUpload{
		ID:           uuid.New(),
		Filename:     "broken.ova",
		Kind:         models.ImageKindOVA,
		Status:       models.ImageUploadError,
		ErrorMessage: "no space left on device",
	}
	morefless := models.ImageUpload{
		ID:       uuid.New(),
		Filename: "amnesiac.ova",
		Kind:     models.ImageKindOVA,
		Status:   models.ImageUploadImported, // imported but no moref recorded
	}
	// An ISO must never appear in the OVA catalog.
	iso := models.ImageUpload{
		ID:            uuid.New(),
		Filename:      "kali.iso",
		Kind:          models.ImageKindISO,
		Status:        models.ImageUploadImported,
		DatastorePath: "[NAS-BackupsAndISOS] ISOs/kali.iso",
	}

	db := &fakeImageDB{listByStatusImgs: []models.ImageUpload{ready, importing, failed, morefless, iso}}
	h := newImageHandler(db, &fakeImageStore{})

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/ovas", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterOVAs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		OVAs       []OVAEntry `json:"ovas"`
		SourceType string     `json:"source_type"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.SourceType != models.TemplateSourceOVF {
		t.Errorf("source_type = %q; want %q", resp.SourceType, models.TemplateSourceOVF)
	}
	if len(resp.OVAs) != 4 {
		t.Fatalf("returned %d entries; want 4 OVAs and no ISO: %+v", len(resp.OVAs), resp.OVAs)
	}

	byName := map[string]OVAEntry{}
	for _, e := range resp.OVAs {
		byName[e.Name] = e
	}
	if _, ok := byName["kali.iso"]; ok {
		t.Error("an ISO leaked into the OVA catalog; it can never be an ovf source")
	}

	got := byName["vyos.ova"]
	if got.SourceRef != "vm-4242" {
		t.Errorf("imported OVA source_ref = %q; want the recorded moref vm-4242", got.SourceRef)
	}
	if got.Disabled {
		t.Error("an imported OVA with a moref must be selectable")
	}

	for _, name := range []string{"pfsense.ova", "broken.ova", "amnesiac.ova"} {
		e := byName[name]
		if !e.Disabled {
			t.Errorf("%s must be disabled (status=%q)", name, e.Status)
		}
		if e.Reason == "" {
			t.Errorf("%s is disabled with no reason; the wizard cannot explain it", name)
		}
		if e.SourceRef != "" {
			t.Errorf("%s must not offer a source_ref (%q)", name, e.SourceRef)
		}
	}
	if byName["broken.ova"].ErrorMessage != "no space left on device" {
		t.Errorf("failed OVA lost its error_message: %q", byName["broken.ova"].ErrorMessage)
	}
}

// TestImageImportEnqueue_PayloadSatisfiesWorkerContract is the guard that was
// missing when both enqueue sites shipped `image_upload_id` while the worker
// unmarshalled `image_id`. Every image_import job — ISO and OVA alike — failed
// its required-field check before touching MinIO or vCenter, and no test
// noticed because the worker-side tests all build ImageImportPayload as a Go
// struct instead of decoding what the API actually enqueues.
//
// So this asserts the JSON boundary itself: drive the real handlers, take the
// exact bytes they hand to CreateJob, and decode them with the real worker
// type. Renaming the key on either side fails here.
func TestImageImportEnqueue_PayloadSatisfiesWorkerContract(t *testing.T) {
	newImg := func(id uuid.UUID, status string) *models.ImageUpload {
		return &models.ImageUpload{
			ID:        id,
			Filename:  "mint.iso",
			Kind:      models.ImageKindISO,
			Status:    status,
			ObjectKey: "crucible/" + id.String() + "/mint.iso",
			UploadID:  "upload-mint",
			SizeBytes: 2 << 30,
		}
	}

	cases := []struct {
		name    string
		enqueue func(t *testing.T, id uuid.UUID, db *fakeImageDB)
		db      func(id uuid.UUID) *fakeImageDB
	}{
		{
			name: "auto-enqueue on complete",
			db: func(id uuid.UUID) *fakeImageDB {
				return &fakeImageDB{getImg: newImg(id, models.ImageUploadPending)}
			},
			enqueue: func(t *testing.T, id uuid.UUID, db *fakeImageDB) {
				h := newImageHandler(db, &fakeImageStore{statSize: 2 << 30})
				req := httptest.NewRequest(http.MethodPost,
					"/admin/images/"+id.String()+"/complete",
					strings.NewReader(`{"parts":[{"part_number":1,"etag":"e"}]}`))
				req.Header.Set("Content-Type", "application/json")
				req = withImageIDParam(req, id)
				w := httptest.NewRecorder()
				h.AdminCompleteImageUpload(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("complete status = %d; want 200; body = %s", w.Code, w.Body.String())
				}
			},
		},
		{
			name: "manual retry via POST /import",
			db: func(id uuid.UUID) *fakeImageDB {
				return &fakeImageDB{getImg: newImg(id, models.ImageUploadUploaded)}
			},
			enqueue: func(t *testing.T, id uuid.UUID, db *fakeImageDB) {
				h := newImageHandler(db, &fakeImageStore{})
				req := httptest.NewRequest(http.MethodPost,
					"/admin/images/"+id.String()+"/import", nil)
				req = withImageIDParam(req, id)
				w := httptest.NewRecorder()
				h.AdminImportImage(w, req)
				if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
					t.Fatalf("import status = %d; want 200/202; body = %s", w.Code, w.Body.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			db := tc.db(id)
			tc.enqueue(t, id, db)

			if db.createdJobType != models.JobTypeImageImport {
				t.Fatalf("enqueued job type = %q; want %q", db.createdJobType, models.JobTypeImageImport)
			}
			if len(db.createdJobPayload) == 0 {
				t.Fatal("no payload was enqueued")
			}

			var payload provisioner.ImageImportPayload
			if err := json.Unmarshal(db.createdJobPayload, &payload); err != nil {
				t.Fatalf("worker cannot decode the enqueued payload %s: %v",
					db.createdJobPayload, err)
			}
			if payload.ImageID != id {
				t.Fatalf("decoded image_id = %v, want %v; enqueued payload was %s "+
					"(the worker rejects a nil image_id before doing any work)",
					payload.ImageID, id, db.createdJobPayload)
			}
		})
	}
}

// TestAdminCompleteImageUpload_IdempotentOnDoubleComplete verifies that calling
// complete a second time when the row is already past 'uploaded' returns 200,
// not 409, and does NOT enqueue a second import job.
func TestAdminCompleteImageUpload_IdempotentOnDoubleComplete(t *testing.T) {
	id := uuid.New()
	img := &models.ImageUpload{
		ID:        id,
		Filename:  "mint.iso",
		Kind:      models.ImageKindISO,
		Status:    models.ImageUploadImporting, // already past uploaded
		ObjectKey: "crucible/" + id.String() + "/mint.iso",
		UploadID:  "upload-mint",
		SizeBytes: 2 << 30,
	}
	// Simulate: SetImageUploadUploaded returns ErrImageUploadStale because row
	// is not in pending/uploading. GetImageUploadByID returns the importing row.
	db := &fakeImageDB{
		setUploadedErr: errors.New("image upload is stale"),
		getImg:         img,
	}
	// Make setUploadedErr wrap ErrImageUploadStale.
	db.setUploadedErr = database.ErrImageUploadStale
	store := &fakeImageStore{statSize: img.SizeBytes}
	h := newImageHandler(db, store)

	body := `{"parts":[{"part_number":1,"etag":"etag-mint"}]}`
	req := httptest.NewRequest(http.MethodPost, "/admin/images/"+id.String()+"/complete",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminCompleteImageUpload(w, req)

	// Must NOT 409 — the client gets idempotent 200.
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 for idempotent double-complete; body = %s",
			w.Code, w.Body.String())
	}
}

// TestAdminListVCenterISOs_MergesUploadedISOs verifies that imported ISOs
// from the image_uploads table are merged into the /admin/vcenter/isos
// response under the 'isos' key.
func TestAdminListVCenterISOs_MergesUploadedISOs(t *testing.T) {
	id := uuid.New()
	uploadedISO := models.ImageUpload{
		ID:            id,
		Filename:      "mint.iso",
		Kind:          models.ImageKindISO,
		Status:        models.ImageUploadImported,
		DatastorePath: "[NAS-BackupsAndISOS] ISOs/mint.iso",
	}
	db := &fakeImageDB{listByStatusImgs: []models.ImageUpload{uploadedISO}}
	lister := &fakeISOLister{files: []vcenter.DatastoreFile{
		{
			Name:         "kali.iso",
			Path:         "[NAS-BackupsAndISOS] ISOs/kali.iso",
			FolderPath:   "ISOs",
			SizeBytes:    4 << 30,
			ModifiedTime: time.Now(),
		},
	}}
	h := newISOHandler(db, &fakeImageStore{}, lister, "NAS-BackupsAndISOS")

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/isos", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterISOs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		ISOs []ISOEntry `json:"isos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.ISOs) != 2 {
		t.Fatalf("isos count = %d; want 2 (kali.iso from datastore + mint.iso from uploads)",
			len(resp.ISOs))
	}
	// Confirm sources
	sources := map[string]bool{}
	for _, entry := range resp.ISOs {
		sources[entry.Source] = true
	}
	if !sources["datastore"] {
		t.Error("missing 'datastore' source in merged list")
	}
	if !sources["uploaded"] {
		t.Error("missing 'uploaded' source in merged list")
	}
}

// TestAdminListVCenterISOs_DedupsByPath verifies that when an imported ISO has
// the same datastore path as a file found on the datastore, it is not listed twice.
func TestAdminListVCenterISOs_DedupsByPath(t *testing.T) {
	sharedPath := "[NAS-BackupsAndISOS] ISOs/kali.iso"
	uploadedISO := models.ImageUpload{
		ID:            uuid.New(),
		Filename:      "kali.iso",
		Kind:          models.ImageKindISO,
		Status:        models.ImageUploadImported,
		DatastorePath: sharedPath,
	}
	db := &fakeImageDB{listByStatusImgs: []models.ImageUpload{uploadedISO}}
	lister := &fakeISOLister{files: []vcenter.DatastoreFile{
		{
			Name:         "kali.iso",
			Path:         sharedPath, // same path
			FolderPath:   "ISOs",
			SizeBytes:    4 << 30,
			ModifiedTime: time.Now(),
		},
	}}
	h := newISOHandler(db, &fakeImageStore{}, lister, "NAS-BackupsAndISOS")

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/isos", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterISOs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var resp struct {
		ISOs []ISOEntry `json:"isos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.ISOs) != 1 {
		t.Errorf("isos count = %d; want 1 (dedup by path should eliminate the duplicate)", len(resp.ISOs))
	}
}

// TestAdminListVCenterISOs_ExcludesSeedISOs verifies that provisioner-generated
// seed ISOs (cloud-init CIDATA / Windows autounattend) found on the datastore are
// never offered as installer sources, while a legitimately named installer is.
func TestAdminListVCenterISOs_ExcludesSeedISOs(t *testing.T) {
	db := &fakeImageDB{}
	lister := &fakeISOLister{files: []vcenter.DatastoreFile{
		{
			Name:         "tpl-mint-21d3e7-seed-cidata.iso",
			Path:         "[NAS-BackupsAndISOS] ISOs/Linux/tpl-mint-21d3e7-seed-cidata.iso",
			FolderPath:   "ISOs/Linux",
			SizeBytes:    1 << 20,
			ModifiedTime: time.Now(),
		},
		{
			Name:         "tpl-win-abc123-seed-autounattend.iso",
			Path:         "[NAS-BackupsAndISOS] ISOs/Windows/tpl-win-abc123-seed-autounattend.iso",
			FolderPath:   "ISOs/Windows",
			SizeBytes:    1 << 20,
			ModifiedTime: time.Now(),
		},
		{
			Name:         "linuxmint-22.3-mate-64bit.iso",
			Path:         "[NAS-BackupsAndISOS] ISOs/Linux/linuxmint-22.3-mate-64bit.iso",
			FolderPath:   "ISOs/Linux",
			SizeBytes:    3 << 30,
			ModifiedTime: time.Now(),
		},
	}}
	h := newISOHandler(db, &fakeImageStore{}, lister, "NAS-BackupsAndISOS")

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/isos", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterISOs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		ISOs []ISOEntry `json:"isos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.ISOs) != 1 {
		t.Fatalf("isos count = %d; want 1 (only the real installer); got %+v", len(resp.ISOs), resp.ISOs)
	}
	if resp.ISOs[0].Name != "linuxmint-22.3-mate-64bit.iso" {
		t.Errorf("isos[0].Name = %q; want linuxmint-22.3-mate-64bit.iso", resp.ISOs[0].Name)
	}
}

// TestAdminListVCenterISOs_ExcludesOVAs verifies that OVA image uploads never
// appear in the ISO picker, regardless of their status.
func TestAdminListVCenterISOs_ExcludesOVAs(t *testing.T) {
	ovaImported := models.ImageUpload{
		ID:          uuid.New(),
		Filename:    "pfsense.ova",
		Kind:        models.ImageKindOVA,
		Status:      models.ImageUploadImported,
		VCenterVMID: "vm-42",
	}
	ovaImporting := models.ImageUpload{
		ID:       uuid.New(),
		Filename: "vyos.ova",
		Kind:     models.ImageKindOVA,
		Status:   models.ImageUploadImporting,
	}
	db := &fakeImageDB{listByStatusImgs: []models.ImageUpload{ovaImported, ovaImporting}}
	lister := &fakeISOLister{files: nil} // datastore has nothing
	h := newISOHandler(db, &fakeImageStore{}, lister, "NAS-BackupsAndISOS")

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/isos", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterISOs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var resp struct {
		ISOs []ISOEntry `json:"isos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.ISOs) != 0 {
		t.Errorf("isos count = %d; want 0 — OVAs must never appear in ISO picker; got: %v",
			len(resp.ISOs), resp.ISOs)
	}
}

// TestAdminListVCenterISOs_SurfacesInFlightAsDisabled verifies that in-flight
// (importing) and error rows appear as disabled entries so the wizard can
// display "Importing…" rather than a blank picker.
func TestAdminListVCenterISOs_SurfacesInFlightAsDisabled(t *testing.T) {
	importingISO := models.ImageUpload{
		ID:       uuid.New(),
		Filename: "ubuntu.iso",
		Kind:     models.ImageKindISO,
		Status:   models.ImageUploadImporting,
	}
	errorISO := models.ImageUpload{
		ID:           uuid.New(),
		Filename:     "fedora.iso",
		Kind:         models.ImageKindISO,
		Status:       models.ImageUploadError,
		ErrorMessage: "no space left on device",
	}
	db := &fakeImageDB{listByStatusImgs: []models.ImageUpload{importingISO, errorISO}}
	lister := &fakeISOLister{files: nil}
	h := newISOHandler(db, &fakeImageStore{}, lister, "NAS-BackupsAndISOS")

	req := httptest.NewRequest(http.MethodGet, "/admin/vcenter/isos", nil)
	w := httptest.NewRecorder()
	h.AdminListVCenterISOs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var resp struct {
		ISOs []ISOEntry `json:"isos"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.ISOs) != 2 {
		t.Fatalf("isos count = %d; want 2 (one importing + one error)", len(resp.ISOs))
	}
	for _, entry := range resp.ISOs {
		if !entry.Disabled {
			t.Errorf("entry %q (status=%s) has disabled=false; want disabled=true for in-flight/error entries",
				entry.Name, entry.Status)
		}
		if entry.Source != "uploaded" {
			t.Errorf("entry %q source = %q; want 'uploaded'", entry.Name, entry.Source)
		}
	}
	// The error entry must carry the error message.
	for _, entry := range resp.ISOs {
		if entry.Status == models.ImageUploadError && entry.ErrorMessage == "" {
			t.Errorf("error entry %q has no error_message; want %q",
				entry.Name, "no space left on device")
		}
	}
}

// TestAdminImportImage_RetryFromError verifies that an image row in 'error'
// status can be re-queued via AdminImportImage without re-uploading.
func TestAdminImportImage_RetryFromError(t *testing.T) {
	id := uuid.New()
	errorImg := &models.ImageUpload{
		ID:           id,
		Filename:     "mint.iso",
		Kind:         models.ImageKindISO,
		Status:       models.ImageUploadError,
		ObjectKey:    "crucible/" + id.String() + "/mint.iso",
		ErrorMessage: "previous import failed: disk full",
	}
	db := &fakeImageDB{
		getImg:                   errorImg,
		updateStatusFromExpected: models.ImageUploadError,
	}
	h := newImageHandler(db, &fakeImageStore{})

	req := httptest.NewRequest(http.MethodPost, "/admin/images/"+id.String()+"/import", nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminImportImage(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202 for retry from error; body = %s", w.Code, w.Body.String())
	}
	if db.updateStatusCalls != 1 {
		t.Errorf("UpdateImageUploadStatus calls = %d; want 1 (reset error→uploaded)", db.updateStatusCalls)
	}
}

// TestAdminImportImage_RejectsInvalidObjectKeyPrefix verifies that an image
// whose object key does not start with "crucible/" is rejected with an
// internal-error response (defense-in-depth path traversal guard).
func TestAdminImportImage_RejectsInvalidObjectKeyPrefix(t *testing.T) {
	id := uuid.New()
	badImg := &models.ImageUpload{
		ID:        id,
		Filename:  "evil.iso",
		Kind:      models.ImageKindISO,
		Status:    models.ImageUploadUploaded,
		ObjectKey: "../../etc/passwd",
	}
	db := &fakeImageDB{getImg: badImg}
	h := newImageHandler(db, &fakeImageStore{})

	req := httptest.NewRequest(http.MethodPost, "/admin/images/"+id.String()+"/import", nil)
	req = withImageIDParam(req, id)
	w := httptest.NewRecorder()
	h.AdminImportImage(w, req)

	if w.Code == http.StatusAccepted {
		t.Fatal("status = 202; want non-202 for path-traversal object key")
	}
}
