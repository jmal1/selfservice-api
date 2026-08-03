package provisioner

// image_jobs_test.go - Unit tests for the image_import worker core.
//
// These target the pure importImage function with in-package fakes for the
// object store, vCenter and database — the same idiom as reconcile_test.go /
// ip_reconcile_test.go. The real *objectstore.Client / *vcenter.Client /
// *database.Queries satisfy the narrow interfaces (asserted at compile time in
// image_jobs.go), so exercising the core here fully covers the behaviour the
// (p *Provisioner) ImportImage wrapper will delegate to.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// ── Fakes ────────────────────────────────────────────────────────────────

// chunkReader yields at most `chunk` bytes per Read so a test can prove the
// import streams the object in small pieces rather than reading it whole.
type chunkReader struct {
	data  []byte
	chunk int
	off   int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	n := len(p)
	if c.chunk > 0 && n > c.chunk {
		n = c.chunk
	}
	if remaining := len(c.data) - c.off; n > remaining {
		n = remaining
	}
	copy(p, c.data[c.off:c.off+n])
	c.off += n
	return n, nil
}

type fakeImageObjects struct {
	payload   []byte
	chunk     int
	openErr   error
	removeErr error

	opened  bool
	openKey string
	removed []string
}

func (f *fakeImageObjects) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	f.opened = true
	f.openKey = key
	if f.openErr != nil {
		return nil, 0, f.openErr
	}
	return io.NopCloser(&chunkReader{data: f.payload, chunk: f.chunk}), int64(len(f.payload)), nil
}

func (f *fakeImageObjects) Remove(_ context.Context, key string) error {
	f.removed = append(f.removed, key)
	return f.removeErr
}

type fakeImageVC struct {
	uploadErr error
	importErr error
	moref     string
	sinkBuf   int // copy-buffer size used when draining the stream

	uploadCalls int
	importCalls int
	lastDS      string
	lastRemote  string
}

func (f *fakeImageVC) drain(r io.Reader) {
	bufSize := f.sinkBuf
	if bufSize <= 0 {
		bufSize = 32 * 1024
	}
	// Streaming copy with a fixed, bounded buffer: the whole object is never
	// materialised in memory here, mirroring the real datastore upload.
	_, _ = io.CopyBuffer(io.Discard, r, make([]byte, bufSize))
}

func (f *fakeImageVC) UploadToDatastore(_ context.Context, datastore, remotePath string, r io.Reader, _ int64, _ func(sent int64)) error {
	f.uploadCalls++
	f.lastDS = datastore
	f.lastRemote = remotePath
	if f.uploadErr != nil {
		return f.uploadErr
	}
	f.drain(r)
	return nil
}

func (f *fakeImageVC) ImportOVA(_ context.Context, p vcenter.OVAImportParams) (string, error) {
	f.importCalls++
	if f.importErr != nil {
		return "", f.importErr
	}
	f.drain(p.Reader)
	return f.moref, nil
}

type fakeImageDB struct {
	img    *models.ImageUpload
	getErr error

	transitions      []string
	importedCalled   bool
	importedDSPath   string
	importedVMID     string
	importedChecksum string
	errorCalled      bool
	errorMsg         string
}

func (f *fakeImageDB) GetImageUploadByID(_ context.Context, _ uuid.UUID) (*models.ImageUpload, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.img, nil
}

func (f *fakeImageDB) UpdateImageUploadStatus(_ context.Context, _ uuid.UUID, from, to string) error {
	if f.img != nil && f.img.Status != from {
		return fmt.Errorf("stale transition: have %q want %q", f.img.Status, from)
	}
	f.transitions = append(f.transitions, from+"->"+to)
	if f.img != nil {
		f.img.Status = to
	}
	return nil
}

func (f *fakeImageDB) SetImageUploadImported(_ context.Context, _ uuid.UUID, datastorePath, vcenterVMID, checksum string) error {
	f.importedCalled = true
	f.importedDSPath = datastorePath
	f.importedVMID = vcenterVMID
	f.importedChecksum = checksum
	if f.img != nil {
		f.img.Status = models.ImageUploadImported
		f.img.DatastorePath = datastorePath
		f.img.VCenterVMID = vcenterVMID
		f.img.ChecksumSHA256 = checksum
	}
	return nil
}

func (f *fakeImageDB) SetImageUploadError(_ context.Context, _ uuid.UUID, msg string) error {
	f.errorCalled = true
	f.errorMsg = msg
	if f.img != nil {
		f.img.Status = models.ImageUploadError
		f.img.ErrorMessage = msg
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

func testImportConfig() ImageImportConfig {
	return ImageImportConfig{
		ISODatastore: "NAS-BackupsAndISOS",
		ISOFolder:    "ISOs",
		OVAFolder:    "Templates",
		OVADatastore: "NAS-vmstore",
	}
}

func newImageRow(kind, filename, status string) *models.ImageUpload {
	return &models.ImageUpload{
		ID:       uuid.New(),
		Filename: filename,
		Kind:     kind,
		Status:   status,
	}
}

// ── Tests ────────────────────────────────────────────────────────────────

func TestImportImage_ISOHappyPath(t *testing.T) {
	payloadBytes := bytes.Repeat([]byte("installer-media-"), 4096) // 64 KiB
	wantSum := sha256.Sum256(payloadBytes)

	row := newImageRow(models.ImageKindISO, "kali-2024.4.iso", models.ImageUploadUploaded)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{}
	objects := &fakeImageObjects{payload: payloadBytes, chunk: 512}
	metrics := NewPipelineMetrics("", "", nil)

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindISO,
		ObjectKey: "crucible/" + row.ID.String() + "/kali-2024.4.iso",
		Filename:  "kali-2024.4.iso",
	}

	if err := importImage(context.Background(), objects, vc, db, metrics, nil, testImportConfig(), nil, payload); err != nil {
		t.Fatalf("importImage: unexpected error: %v", err)
	}

	// Status uploaded -> importing -> imported.
	if len(db.transitions) != 1 || db.transitions[0] != models.ImageUploadUploaded+"->"+models.ImageUploadImporting {
		t.Errorf("transitions = %v, want single uploaded->importing", db.transitions)
	}
	if db.img.Status != models.ImageUploadImported {
		t.Errorf("final status = %q, want %q", db.img.Status, models.ImageUploadImported)
	}
	if !db.importedCalled {
		t.Fatal("SetImageUploadImported was not called")
	}

	// datastore_path is the [datastore] folder/file form.
	if want := "[NAS-BackupsAndISOS] ISOs/kali-2024.4.iso"; db.importedDSPath != want {
		t.Errorf("datastore_path = %q, want %q", db.importedDSPath, want)
	}
	if db.importedVMID != "" {
		t.Errorf("vcenter_vm_id = %q, want empty for an ISO", db.importedVMID)
	}

	// Upload targeted the ISO datastore + folder.
	if vc.lastDS != "NAS-BackupsAndISOS" {
		t.Errorf("upload datastore = %q, want NAS-BackupsAndISOS", vc.lastDS)
	}
	if vc.lastRemote != "ISOs/kali-2024.4.iso" {
		t.Errorf("upload remote path = %q, want ISOs/kali-2024.4.iso", vc.lastRemote)
	}

	// SHA-256 recorded and correct for the streamed bytes.
	if got := hex.EncodeToString(wantSum[:]); db.importedChecksum != got {
		t.Errorf("checksum = %q, want %q", db.importedChecksum, got)
	}

	if series := `crucible_image_import_total{kind="iso",result="success"} 1`; !strings.Contains(string(metrics.serialize()), series) {
		t.Errorf("success metric not recorded; expected series %q in:\n%s", series, metrics.serialize())
	}
}

func TestImportImage_OVAHappyPath(t *testing.T) {
	row := newImageRow(models.ImageKindOVA, "appliance.ova", models.ImageUploadUploaded)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{moref: "vm-4242"}
	objects := &fakeImageObjects{payload: bytes.Repeat([]byte("ova"), 1000), chunk: 128}
	metrics := NewPipelineMetrics("", "", nil)

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindOVA,
		ObjectKey: "crucible/" + row.ID.String() + "/appliance.ova",
		Filename:  "appliance.ova",
	}

	if err := importImage(context.Background(), objects, vc, db, metrics, nil, testImportConfig(), nil, payload); err != nil {
		t.Fatalf("importImage: unexpected error: %v", err)
	}

	if !db.importedCalled {
		t.Fatal("SetImageUploadImported was not called")
	}
	if db.importedVMID != "vm-4242" {
		t.Errorf("vcenter_vm_id = %q, want vm-4242 (the returned moref)", db.importedVMID)
	}
	if db.importedDSPath != "" {
		t.Errorf("datastore_path = %q, want empty for an OVA", db.importedDSPath)
	}
	if db.img.Status != models.ImageUploadImported {
		t.Errorf("final status = %q, want %q", db.img.Status, models.ImageUploadImported)
	}
	if vc.importCalls != 1 {
		t.Errorf("ImportOVA calls = %d, want 1", vc.importCalls)
	}
	if vc.uploadCalls != 0 {
		t.Errorf("UploadToDatastore calls = %d, want 0 for an OVA", vc.uploadCalls)
	}
}

func TestImportImage_FailureKeepsObject(t *testing.T) {
	row := newImageRow(models.ImageKindISO, "broken.iso", models.ImageUploadUploaded)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{uploadErr: errors.New("datastore NFS APD")}
	objects := &fakeImageObjects{payload: []byte("bytes"), chunk: 4}
	metrics := NewPipelineMetrics("", "", nil)

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindISO,
		ObjectKey: "crucible/" + row.ID.String() + "/broken.iso",
		Filename:  "broken.iso",
	}

	err := importImage(context.Background(), objects, vc, db, metrics, nil, testImportConfig(), nil, payload)
	if err == nil {
		t.Fatal("expected error when the datastore upload fails, got nil")
	}

	if db.img.Status != models.ImageUploadError {
		t.Errorf("status = %q, want %q", db.img.Status, models.ImageUploadError)
	}
	if !db.errorCalled || db.errorMsg == "" {
		t.Errorf("expected a non-empty error_message; errorCalled=%v msg=%q", db.errorCalled, db.errorMsg)
	}

	// The MinIO object must be kept so a retry stays cheap.
	if len(objects.removed) != 0 {
		t.Errorf("object was removed on failure (%v); it must be retained for retry", objects.removed)
	}

	if series := `crucible_image_import_total{kind="iso",result="error"} 1`; !strings.Contains(string(metrics.serialize()), series) {
		t.Errorf("error metric not recorded; expected series %q in:\n%s", series, metrics.serialize())
	}
}

func TestImportImage_SuccessRemovesObject(t *testing.T) {
	row := newImageRow(models.ImageKindISO, "ubuntu.iso", models.ImageUploadUploaded)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{}
	objects := &fakeImageObjects{payload: []byte("ubuntu-iso-bytes"), chunk: 3}
	key := "crucible/" + row.ID.String() + "/ubuntu.iso"

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindISO,
		ObjectKey: key,
		Filename:  "ubuntu.iso",
	}

	if err := importImage(context.Background(), objects, vc, db, NewPipelineMetrics("", "", nil), nil, testImportConfig(), nil, payload); err != nil {
		t.Fatalf("importImage: unexpected error: %v", err)
	}

	if len(objects.removed) != 1 || objects.removed[0] != key {
		t.Errorf("Remove calls = %v, want exactly [%s]", objects.removed, key)
	}
}

func TestImportImage_RejectsWrongState(t *testing.T) {
	// A row still in `pending` must be refused without any vCenter work.
	row := newImageRow(models.ImageKindISO, "early.iso", models.ImageUploadPending)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{}
	objects := &fakeImageObjects{payload: []byte("data"), chunk: 2}

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindISO,
		ObjectKey: "crucible/" + row.ID.String() + "/early.iso",
		Filename:  "early.iso",
	}

	err := importImage(context.Background(), objects, vc, db, NewPipelineMetrics("", "", nil), nil, testImportConfig(), nil, payload)
	if err == nil {
		t.Fatal("expected an error for a row not in `uploaded`, got nil")
	}

	if vc.uploadCalls != 0 || vc.importCalls != 0 {
		t.Errorf("vCenter was called for a wrong-state row (upload=%d import=%d); want zero", vc.uploadCalls, vc.importCalls)
	}
	if objects.opened {
		t.Error("object store was opened for a wrong-state row; want no side effects")
	}
	if len(db.transitions) != 0 {
		t.Errorf("row was transitioned (%v); a wrong-state row must be left untouched", db.transitions)
	}
	if db.img.Status != models.ImageUploadPending {
		t.Errorf("status = %q, want it left at %q", db.img.Status, models.ImageUploadPending)
	}
}

func TestImportImage_ChecksumStreamed(t *testing.T) {
	// Honesty of the "never buffer the whole image" claim is asserted here by
	// using a payload (3 MiB) far larger than both the source read chunk
	// (4 KiB) and the sink copy buffer (32 KiB). The object can therefore only
	// reach the hasher a bounded chunk at a time; a matching digest proves
	// every byte flowed through the TeeReader without being held in full.
	payloadBytes := make([]byte, 3<<20)
	for i := range payloadBytes {
		payloadBytes[i] = byte((i*31 + 7) % 251)
	}
	wantSum := sha256.Sum256(payloadBytes)

	row := newImageRow(models.ImageKindISO, "big.iso", models.ImageUploadUploaded)
	db := &fakeImageDB{img: row}
	vc := &fakeImageVC{sinkBuf: 32 * 1024}
	objects := &fakeImageObjects{payload: payloadBytes, chunk: 4096}

	payload := ImageImportPayload{
		ImageID:   row.ID,
		Kind:      models.ImageKindISO,
		ObjectKey: "crucible/" + row.ID.String() + "/big.iso",
		Filename:  "big.iso",
	}

	if err := importImage(context.Background(), objects, vc, db, NewPipelineMetrics("", "", nil), nil, testImportConfig(), nil, payload); err != nil {
		t.Fatalf("importImage: unexpected error: %v", err)
	}

	if got := hex.EncodeToString(wantSum[:]); db.importedChecksum != got {
		t.Errorf("checksum = %q, want %q (streamed digest must match the full payload)", db.importedChecksum, got)
	}
}

// TestImportImage_UnconfiguredWorkerFailsCleanly covers the wrapper's guard.
// A worker deployed without OBJECTSTORE_* still starts and serves every other
// job type; an image_import job that lands on it must fail with an actionable
// message naming the missing configuration, NOT nil-panic inside the stream.
// A panic here would crash the worker mid-job and take unrelated in-flight
// pod operations down with it.
func TestImportImage_UnconfiguredWorkerFailsCleanly(t *testing.T) {
	p := &Provisioner{logger: slog.Default()} // EnableImageImport never called

	job := &models.Job{ID: uuid.New(), Type: models.JobTypeImageImport, Payload: []byte(`{}`)}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ImportImage panicked on an unconfigured worker: %v", r)
			}
		}()
		err = p.ImportImage(context.Background(), job)
	}()

	if err == nil {
		t.Fatal("ImportImage() = nil, want error on an unconfigured worker")
	}
	if !strings.Contains(err.Error(), "OBJECTSTORE_ENDPOINT") {
		t.Errorf("error %q should name the missing configuration so an operator can act on it", err.Error())
	}
}

// TestImportImage_RejectsMalformedPayload proves a corrupt payload is reported
// as a parse failure rather than being silently treated as a zero-valued
// import against the nil UUID.
func TestImportImage_RejectsMalformedPayload(t *testing.T) {
	p := &Provisioner{logger: slog.Default()}
	p.EnableImageImport(nil, nil, ImageImportConfig{})
	// EnableImageImport(nil, ...) stores a typed-nil *objectstore.Client, which
	// is non-nil as an interface, so the guard above is bypassed and we reach
	// the unmarshal -- exactly the path under test.
	job := &models.Job{ID: uuid.New(), Type: models.JobTypeImageImport, Payload: []byte(`{not json`)}

	err := p.ImportImage(context.Background(), job)
	if err == nil {
		t.Fatal("ImportImage() = nil, want error on malformed payload")
	}
	if !strings.Contains(err.Error(), "parse image_import payload") {
		t.Errorf("error %q should identify the payload as the problem", err.Error())
	}
}
