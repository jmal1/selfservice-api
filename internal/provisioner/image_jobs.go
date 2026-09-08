package provisioner

// image_jobs.go - Worker job handler for JobTypeImageImport (Epic A5).
//
// An image_import job takes an ISO/OVA that an instructor already uploaded
// to MinIO (see internal/objectstore + internal/api/handlers/images.go) and
// moves it into vCenter:
//
//   - ISO -> streamed onto the ISO datastore under ISOs/<file>, so the
//            template wizard / pod builder can mount it as install media.
//   - OVA -> imported via OVF into the Templates inventory folder, producing
//            a VM whose moref is stored on the image row. The wizard authors
//            a template with source_type=ovf and that moref as source_ref
//            (clone_vcenter remains valid for the same moref).
//
// Design mirrors the reconciler handlers in this package (reconcile.go,
// ip_reconcile.go): a thin (p *Provisioner) wrapper delegates to a pure,
// dependency-injected function that takes narrow interfaces. The real
// *objectstore.Client, *vcenter.Client and *database.Queries satisfy those
// interfaces structurally, so production wiring stays trivial while tests
// inject fakes without standing up MinIO / a govmomi simulator.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/objectstore"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// ImageImportPayload is the jobs.payload (JSONB) shape for an image_import
// job.
//
// ImageID is the ONLY required field. Kind/ObjectKey/Filename are optional
// enqueue-time hints; each falls back to the image_uploads row, which is
// re-read (and its status re-validated) before any work runs. Making the row
// authoritative is deliberate: the enqueue sites used to send a key that did
// not match these tags, which silently produced an unusable payload on every
// single import. Only the primary key needs to survive the JSON boundary.
type ImageImportPayload struct {
	ImageID   uuid.UUID `json:"image_id"`
	Kind      string    `json:"kind"`
	ObjectKey string    `json:"object_key"`
	Filename  string    `json:"filename"`
}

// imageObjectStore is the object-store subset the import job needs. The real
// *objectstore.Client satisfies it automatically.
type imageObjectStore interface {
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Remove(ctx context.Context, key string) error
}

// imageImportVCenter is the vCenter subset the import job needs.
type imageImportVCenter interface {
	UploadToDatastore(ctx context.Context, datastore, remotePath string, r io.Reader, size int64, progress func(sent int64)) error
	ImportOVA(ctx context.Context, p vcenter.OVAImportParams) (string, error)
	GetDatastoreFreeBytes(ctx context.Context, datastore string) (int64, error)
}

// imageImportDB is the database subset the import job needs.
type imageImportDB interface {
	GetImageUploadByID(ctx context.Context, id uuid.UUID) (*models.ImageUpload, error)
	UpdateImageUploadStatus(ctx context.Context, id uuid.UUID, from, to string) error
	SetImageUploadImported(ctx context.Context, id uuid.UUID, datastorePath, vcenterVMID, checksum string) error
	SetImageUploadError(ctx context.Context, id uuid.UUID, msg string) error
}

// Compile-time proof the real clients satisfy the narrow seams, so the
// production wrapper in create.go type-checks the moment it is added.
var (
	_ imageObjectStore   = (*objectstore.Client)(nil)
	_ imageImportVCenter = (*vcenter.Client)(nil)
	_ imageImportDB      = (*database.Queries)(nil)
)

// ImageImportConfig carries the vCenter placement settings the import job
// needs but that are not part of the (per-image) job payload.
//
// ISODatastore is deliberately its OWN setting rather than reusing the
// pod/VM datastore: the ISO datastore is "NAS-BackupsAndISOS" (trailing S),
// whereas the VM datastore defaults to "NAS-vmstore" (config.go). Conflating
// them uploads installer media to the wrong datastore.
type ImageImportConfig struct {
	ISODatastore    string // e.g. "NAS-BackupsAndISOS"
	ISOFolder       string // datastore-relative folder for ISOs, e.g. "ISOs"
	OVAFolder       string // vCenter inventory folder for imported OVA VMs (Templates)
	OVADatastore    string // datastore for the imported OVA's disks
	OVAResourcePool string // resource pool for the imported OVA (empty = default)
	OVANetwork      string // network to map the OVA's networks onto (empty = default)
}

// defaultISOFolder is used when ImageImportConfig.ISOFolder is empty.
const defaultISOFolder = "ISOs"

// importProgressStepPercent throttles UI progress so a multi-GB upload emits
// a bounded number of updates (one per 5% moved) instead of one per chunk.
const importProgressStepPercent = 5

// ImportImage is the job entrypoint for models.JobTypeImageImport. It unpacks
// the payload and delegates to importImage with the Provisioner's wired
// dependencies.
//
// If the worker was started without an object store (EnableImageImport never
// called), this fails with an actionable message rather than nil-panicking:
// the job is then retryable once the deployment is fixed, and the operator is
// told exactly which configuration is missing.
func (p *Provisioner) ImportImage(ctx context.Context, job *models.Job) error {
	if p.objects == nil {
		return fmt.Errorf("image import is not configured on this worker: set OBJECTSTORE_ENDPOINT/ACCESS_KEY/SECRET_KEY so the object store client is initialized")
	}
	var payload ImageImportPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse image_import payload: %w", err)
	}
	return importImage(ctx, p.objects, p.vc, p.db, p.pipeline, p.logger, p.imageCfg,
		func(step, message string) { p.publishProgress(job.ID, step, message) }, payload)
}

// importImage streams the staged object for payload.ImageID out of the object
// store and into vCenter, updating the image_uploads row's lifecycle as it
// goes. It is the pure, dependency-injected core; (p *Provisioner).ImportImage
// is a thin wrapper over it.
//
// Contract highlights (each is covered by image_jobs_test.go):
//   - Requires status == uploaded. Any other status errors out BEFORE any
//     object-store or vCenter call, so a double-dispatch / retry race can
//     never re-import or corrupt an in-flight/finished row.
//   - Computes the SHA-256 while streaming (io.TeeReader), never buffering the
//     whole image — these are multi-GB files and the worker has little disk.
//   - On success: marks imported (recording checksum + placement) and THEN
//     deletes the object from MinIO. The staging host (/data/minio on
//     stagingv01) has only ~85 GB free on a filesystem shared with
//     apt-cacher-ng, so leaving imported objects would eventually break every
//     Linux template build. A delete failure is logged, not fatal — the
//     import already succeeded and a leaked object is the lesser problem.
//   - On failure: marks error with the cause and leaves the object in place so
//     a retry stays cheap.
//
// progress may be nil; when set it is called with a coarse step name and a
// human-readable message suitable for p.publishProgress.
func importImage(
	ctx context.Context,
	objects imageObjectStore,
	vc imageImportVCenter,
	db imageImportDB,
	metrics pipelineMetricsSink,
	logger *slog.Logger,
	cfg ImageImportConfig,
	progress func(step, message string),
	payload ImageImportPayload,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "image_import", "image_id", payload.ImageID)

	// image_id is the one field that cannot be recovered from anywhere else:
	// without it there is no row to load and no row to mark errored, so this
	// check necessarily stays above the fail() closure below. Every other
	// missing field IS recordable and is therefore validated after fail()
	// exists, so the operator sees a cause instead of a row frozen at
	// "uploaded" with no explanation.
	if payload.ImageID == uuid.Nil {
		return fmt.Errorf("image_import: image_id is required")
	}

	// ── Pre-flight validation (no side effects on object store / vCenter) ──
	//
	// A validation failure here is NOT counted as an import failure: it is a
	// benign race (e.g. a redelivered job hitting a row that is already
	// importing/imported), not a real vCenter/storage problem, so inflating
	// the error metric would produce false alerts.
	img, err := db.GetImageUploadByID(ctx, payload.ImageID)
	if err != nil {
		return fmt.Errorf("load image %s: %w", payload.ImageID, err)
	}
	if img == nil {
		return fmt.Errorf("image %s not found", payload.ImageID)
	}
	if img.Status != models.ImageUploadUploaded {
		return fmt.Errorf("image %s is in status %q, expected %q; refusing to import",
			payload.ImageID, img.Status, models.ImageUploadUploaded)
	}

	// Commit to importing. The guarded transition also closes the race: if a
	// concurrent worker already advanced the row, this returns stale and we
	// stop without touching vCenter.
	if err := db.UpdateImageUploadStatus(ctx, payload.ImageID,
		models.ImageUploadUploaded, models.ImageUploadImporting); err != nil {
		return fmt.Errorf("transition image %s to importing: %w", payload.ImageID, err)
	}

	kind := strings.ToLower(strings.TrimSpace(payload.Kind))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(img.Kind))
	}
	filename := payload.Filename
	if filename == "" {
		filename = img.Filename
	}
	objectKey := strings.TrimSpace(payload.ObjectKey)
	if objectKey == "" {
		objectKey = strings.TrimSpace(img.ObjectKey)
	}

	start := time.Now()

	// fail marks the row errored, records the error metric, and returns the
	// cause. It deliberately does NOT remove the object (retry must stay cheap).
	fail := func(cause error) error {
		if serr := db.SetImageUploadError(ctx, payload.ImageID, cause.Error()); serr != nil {
			log.Error("failed to mark image errored", "error", serr, "cause", cause)
		}
		if metrics != nil {
			metrics.RecordImageImport(kind, MetricResultError, time.Since(start), 0)
		}
		return cause
	}

	// An empty key here means neither the payload nor the row carries a
	// staged object, so there is nothing to stream. Recordable, unlike the
	// image_id case above.
	if objectKey == "" {
		return fail(fmt.Errorf("image_import: no staged object key on the payload or image %s", payload.ImageID))
	}

	rc, size, err := objects.Open(ctx, objectKey)
	if err != nil {
		return fail(fmt.Errorf("open staged object %q: %w", objectKey, err))
	}
	defer rc.Close()

	// Hash while streaming — the bytes vCenter consumes are the bytes hashed,
	// and nothing is ever held in memory in full.
	hasher := sha256.New()
	tee := io.TeeReader(rc, hasher)

	var datastorePath, vcenterVMID string

	switch kind {
	case models.ImageKindISO:
		isoFolder := cfg.ISOFolder
		if isoFolder == "" {
			isoFolder = defaultISOFolder
		}
		if cfg.ISODatastore == "" {
			return fail(fmt.Errorf("iso import: ISO datastore is not configured"))
		}

		// Pre-flight: check that the ISO datastore has enough headroom.
		// A failed check is treated as non-fatal (log + warn) so a transient
		// govmomi hiccup doesn't block every in-flight import. A confirmed
		// insufficient-space error IS fatal so we don't start streaming a
		// multi-GB file that will fail at 95%.
		freeBytes, fsErr := vc.GetDatastoreFreeBytes(ctx, cfg.ISODatastore)
		if fsErr != nil {
			log.Warn("could not determine datastore free space; proceeding without capacity check",
				"datastore", cfg.ISODatastore, "error", fsErr)
		} else if freeBytes < size {
			return fail(fmt.Errorf("iso import: not enough free space on datastore %q: need %d bytes, have %d bytes",
				cfg.ISODatastore, size, freeBytes))
		}

		remotePath := path.Join(isoFolder, filename)
		if progress != nil {
			progress("import", fmt.Sprintf("Streaming ISO to [%s] %s", cfg.ISODatastore, remotePath))
		}
		if err := vc.UploadToDatastore(ctx, cfg.ISODatastore, remotePath, tee, size,
			byteProgress(size, progress)); err != nil {
			return fail(fmt.Errorf("upload ISO to datastore: %w", err))
		}
		datastorePath = vcenter.DatastorePath(cfg.ISODatastore, remotePath)

	case models.ImageKindOVA:
		if progress != nil {
			progress("import", "Importing OVA appliance into vCenter")
		}
		// Resolve the portgroup here rather than trusting the wiring to set
		// it: ImportOVA hard-fails any OVA that declares a network when this
		// is empty, which is every real appliance, and the worker wiring did
		// omit it. Defaulting at the point of use is the same idiom as
		// defaultISOFolder above and as the template staging fallback in
		// template_jobs.go, and it lands an unbuilt appliance on the isolated
		// staging VLAN instead of anywhere it could reach the real lab.
		ovaNetwork := strings.TrimSpace(cfg.OVANetwork)
		if ovaNetwork == "" {
			ovaNetwork = models.CanonicalStagingNetwork
		}
		moref, err := vc.ImportOVA(ctx, vcenter.OVAImportParams{
			Reader:       tee,
			Size:         size,
			VMName:       ovaVMName(filename, payload.ImageID),
			FolderPath:   cfg.OVAFolder,
			Datastore:    cfg.OVADatastore,
			ResourcePool: cfg.OVAResourcePool,
			Network:      ovaNetwork,
		})
		if err != nil {
			return fail(fmt.Errorf("import OVA: %w", err))
		}
		vcenterVMID = moref

	default:
		return fail(fmt.Errorf("unsupported image kind %q (want %q or %q)",
			kind, models.ImageKindISO, models.ImageKindOVA))
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))

	if err := db.SetImageUploadImported(ctx, payload.ImageID, datastorePath, vcenterVMID, checksum); err != nil {
		return fail(fmt.Errorf("mark image %s imported: %w", payload.ImageID, err))
	}

	if progress != nil {
		progress("cleanup", "Releasing staged object from MinIO")
	}

	// Best-effort object release. The import already succeeded; a leaked
	// object is a lesser problem than failing the job and re-doing a multi-GB
	// transfer, so a delete error is logged, not returned.
	if err := objects.Remove(ctx, objectKey); err != nil {
		log.Warn("import succeeded but failed to release staged object; it will occupy space on the MinIO host until reaped",
			"object_key", objectKey, "error", err)
	}

	if metrics != nil {
		metrics.RecordImageImport(kind, MetricResultSuccess, time.Since(start), size)
	}

	log.Info("image import complete",
		"kind", kind, "size_bytes", size, "datastore_path", datastorePath,
		"vcenter_vm_id", vcenterVMID, "checksum_sha256", checksum)
	return nil
}

// byteProgress adapts UploadToDatastore's cumulative-bytes callback into
// throttled, human-readable progress updates (at most one per
// importProgressStepPercent). Returns nil when there's nothing to report to,
// so UploadToDatastore skips its counting wrapper entirely.
func byteProgress(size int64, progress func(step, message string)) func(sent int64) {
	if progress == nil || size <= 0 {
		return nil
	}
	lastBucket := -1
	return func(sent int64) {
		pct := int(sent * 100 / size)
		if pct > 100 {
			pct = 100
		}
		bucket := pct / importProgressStepPercent
		if bucket == lastBucket {
			return
		}
		lastBucket = bucket
		progress("import", fmt.Sprintf("Uploading… %d%%", pct))
	}
}

// ovaVMName derives a vCenter VM name from the uploaded filename, falling back
// to the image ID when the filename yields nothing usable. vCenter's OVF
// importer requires a non-empty entity name.
func ovaVMName(filename string, id uuid.UUID) string {
	base := strings.TrimSpace(filename)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return "ova-" + id.String()
	}
	return base
}
