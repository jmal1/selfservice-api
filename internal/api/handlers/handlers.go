package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/templates"
	"github.com/jmal1/selfservice-api/internal/vcenter"
	"github.com/jmal1/selfservice-api/internal/vcenter/preflight"
)

// Handler holds shared dependencies for all API handlers.
type Handler struct {
	db              *database.Queries
	events          *events.Client
	jobEvents       jobCreatedPublisher
	jobStatusEvents jobStatusPublisher
	vc              VCenterConsole
	vcFolders       VCenterFolderEnumerator
	logger          *slog.Logger
	allowedOrigins  []string
	templatesFolder *TemplatesFolderHandler
	// healthDeps is the dependency bag for GET /admin/health. Optional
	// fields: nil pointers cause the corresponding probe to report
	// "not_configured" instead of failing.
	healthDeps HealthDeps

	// Image upload fields (Epic A). Set via WithImageStore / WithVCenterISOs.
	// nil means the feature is not configured; handlers return 503.
	imageStore   ImageStore         // object-store surface for presign/complete/stat/delete
	imgDB        imageDB            // image-specific DB queries (= h.db in production)
	imageMetrics imageUploadMetrics // best-effort upload lifecycle metrics
	isoLister    VCenterISOLister   // vCenter ISO datastore browser
	isoDatastore string             // which datastore to browse for ISOs
	isoCache     *isoDatastoreCache // 5-minute ISO listing cache

	// templates is an optional override for database.Queries.ListTemplatesForUser.
	// nil means the default h.db is used. For testing, this can be set to a fake
	// implementation. See templateStore() accessor.
	templates templateLister

	// templatePins and blueprintPins optionally override the pinning DB surface
	// so pin/reorder handlers can be tested without a live pgxpool.
	templatePins  templatePinStore
	blueprintPins blueprintPinStore

	// provDB is an optional override for the narrow DB surface used by the
	// template provision path (requireTemplateInState + advanceTemplateAndEnqueue).
	// nil means h.db is used. Tests inject a fake via WithProvisionDB to drive
	// AdminProvisionTemplate end-to-end without a real pgxpool. See provisionStore().
	provDB provisionDB

	// runsDB is an optional override for the narrow DB surface used by
	// AdminListRuns. nil means h.db is used. Tests inject a fake via
	// WithRunsDB. See runsStore().
	runsDB runsListDB

	// podsDB is an optional override for the narrow DB surface used by
	// ListPods. nil means h.db is used. Tests inject a fake via WithPodsDB.
	podsDB podsListDB

	// workflowActivationDB optionally overrides the activation boundary's
	// narrow database surface for handler tests. Production defaults to h.db.
	workflowActivationDB workflowActivationStore

	// Preflight checks. Set via WithPreflightVCenter. nil = not configured;
	// AdminPreflightTemplate returns 503 and the provision gate is skipped.
	vcPreflight  PreflightVCenter
	preflightCfg PreflightConfig

	// vcResolver resolves a clone_template source (a Crucible templates.id
	// UUID) to a live vCenter moref, mirroring the provision worker. Set from
	// the vCenter client in WithPreflightVCenter when it supports name
	// resolution; nil in tests that inject a resolver-less fake.
	vcResolver templates.VMNameResolver

	provisioningConfigured bool
	provisioningEnabled    bool
	provisioningMetrics    provisioningAdmissionMetrics
}

type jobCreatedPublisher interface {
	PublishJobCreated(jobID uuid.UUID, jobType string) error
}

func isNilJobCreatedPublisher(p jobCreatedPublisher) bool {
	if p == nil {
		return true
	}
	if client, ok := p.(*events.Client); ok {
		return client == nil
	}
	return false
}

type jobStatusPublisher interface {
	PublishRaw(subject string, evt events.Event) error
}

type imageUploadMetrics interface {
	RecordImageUpload(kind, result string)
}

// VCenterConsole is the interface for vCenter operations needed by the
// HTTP API layer. Backed by *vcenter.Client in production. Tests
// substitute a fake to drive specific behaviors (errors, slow calls).
//
// Methods are grouped by purpose:
//   - AcquireWebMKSTicket: powers the browser console (T3 / template
//     build console).
//   - DestroyVM: powers cleanup paths that need to delete a staging or
//     student VM as a side effect of an HTTP request (e.g. template
//     delete cascading to the in-flight build VM — see AdminDeleteTemplate).
type VCenterConsole interface {
	AcquireWebMKSTicket(ctx context.Context, moref string) (*vcenter.WebMKSTicket, error)
	// GetGuestInfo returns a non-blocking snapshot of a VM's guest state
	// (name, IP, tools status, power state). The wizard polls this for
	// the staging VM during provisioning / configuring / generalizing so
	// the instructor can SSH/RDP into the build VM without waiting.
	// Errors are surfaced; an empty IPAddress is normal during boot.
	GetGuestInfo(ctx context.Context, moref string) (*vcenter.GuestInfo, error)
	// DestroyVM powers off the VM (if running) and destroys it.
	// IDEMPOTENT: returns nil if the VM was already gone in vCenter.
	DestroyVM(ctx context.Context, moref string) error

	// Power controls for a template's staging VM (a raw vCenter moref, not a
	// pod VM). The wizard drives these so an instructor can start/stop/reboot
	// the build VM without leaving Crucible. They call the vCenter *task* and
	// return once it's accepted; they do not poll the guest power state.
	//   - PowerOnVM is idempotent: no error if the VM is already on.
	//   - PowerOffVM is idempotent: no error if the VM is already gone.
	//   - RestartVM is a graceful guest reboot (via VMware Tools).
	//   - ResetVM is a hard power-cycle.
	PowerOnVM(ctx context.Context, moref string) error
	PowerOffVM(ctx context.Context, moref string) error
	RestartVM(ctx context.Context, moref string) error
	ResetVM(ctx context.Context, moref string) error
}

// NewHandler creates a new Handler.
func NewHandler(db *database.Queries, events *events.Client, vc VCenterConsole, logger *slog.Logger, allowedOrigins []string) *Handler {
	return &Handler{
		db:              db,
		events:          events,
		jobEvents:       events,
		jobStatusEvents: events,
		vc:              vc,
		logger:          logger,
		allowedOrigins:  allowedOrigins,
	}
}

// WithProvisioningAdmission configures the API maintenance gate. When this
// option is not called, admission remains enabled for backward compatibility.
func (h *Handler) WithProvisioningAdmission(enabled bool, metrics provisioningAdmissionMetrics) *Handler {
	h.provisioningConfigured = true
	h.provisioningEnabled = enabled
	h.provisioningMetrics = metrics
	return h
}

// WithVCenterFolders enables the admin folder-enumeration endpoint by wiring
// a VCenterFolderEnumerator (typically *vcenter.Client) and the configured
// inventory path of the Templates folder.
func (h *Handler) WithVCenterFolders(vcf VCenterFolderEnumerator, templatesFolderPath string) *Handler {
	h.vcFolders = vcf
	h.templatesFolder = NewTemplatesFolderHandler(vcf, templatesFolderPath, func(ctx context.Context) ([]vcenterTemplateRow, error) {
		ts, err := h.db.ListAllTemplates(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]vcenterTemplateRow, 0, len(ts))
		for _, t := range ts {
			out = append(out, vcenterTemplateRow{
				ID:              t.ID.String(),
				Name:            t.Name,
				VCenterTemplate: t.VCenterTemplate,
			})
		}
		return out, nil
	})
	return h
}

// WithImageStore wires the object store for image uploads. In production
// pass the *objectstore.Client; pass a fake ImageStore in tests.
// h.db is reused as the imageDB so it must be set before calling this.
func (h *Handler) WithImageStore(store ImageStore) *Handler {
	h.imageStore = store
	h.imgDB = h.db // *database.Queries satisfies imageDB structurally
	return h
}

// WithPipelineMetrics wires the image-upload metric sink. The handler only
// records lifecycle events; the caller is responsible for pushing the sink.
func (h *Handler) WithPipelineMetrics(metrics imageUploadMetrics) *Handler {
	h.imageMetrics = metrics
	return h
}

// WithVCenterISOs wires vCenter ISO datastore browsing backed by a
// 5-minute cache. datastore is the vCenter-internal name
// (e.g. "NAS-BackupsAndISOS" — note the trailing capital S).
func (h *Handler) WithVCenterISOs(lister VCenterISOLister, datastore string) *Handler {
	h.isoLister = lister
	h.isoDatastore = datastore
	h.isoCache = &isoDatastoreCache{ttl: 5 * time.Minute}
	return h
}

// templateStore returns the templateLister to use for template lookups.
// If h.templates is set (for testing), it is used; otherwise h.db is returned.
func (h *Handler) templateStore() templateLister {
	if h.templates != nil {
		return h.templates
	}
	return h.db
}

type templatePinStore interface {
	ReorderTemplates(ctx context.Context, pins map[uuid.UUID]database.PinState, pinnedBy uuid.UUID) error
	SetTemplatePin(ctx context.Context, templateID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error
}

type blueprintPinStore interface {
	ReorderBlueprints(ctx context.Context, pins map[uuid.UUID]database.PinState, pinnedBy uuid.UUID) error
	SetBlueprintPin(ctx context.Context, blueprintID uuid.UUID, pinned bool, pinOrder int, pinnedBy uuid.UUID) error
}

func (h *Handler) templatePinStore() templatePinStore {
	if h.templatePins != nil {
		return h.templatePins
	}
	return h.db
}

func (h *Handler) blueprintPinStore() blueprintPinStore {
	if h.blueprintPins != nil {
		return h.blueprintPins
	}
	return h.db
}

// provisionDB is the narrow slice of *database.Queries that the template
// provision path needs. Declaring it as an interface lets tests inject a fake
// without a live pgxpool — see Handler.provisionStore() and WithProvisionDB.
type provisionDB interface {
	GetTemplateByID(ctx context.Context, id uuid.UUID) (*models.Template, error)
	UpdateTemplateLifecycleState(ctx context.Context, id uuid.UUID, from, to string) error
	CreateJob(ctx context.Context, jobType string, payload []byte) (*models.Job, error)
	// SetTemplateVCenterVM clears or records the staging VM moref. Cancel uses
	// it with an empty string after DestroyVM so draft rows do not keep a stale
	// pointer at a destroyed inventory object.
	SetTemplateVCenterVM(ctx context.Context, id uuid.UUID, vcenterVMID string) error
}

// provisionStore returns the provisionDB in use. h.provDB is non-nil only in
// tests; production code always falls through to h.db.
func (h *Handler) provisionStore() provisionDB {
	if h.provDB != nil {
		return h.provDB
	}
	return h.db
}

// WithProvisionDB injects a fake provisionDB for testing AdminProvisionTemplate
// end-to-end without a real database connection. Do not call from production code.
func (h *Handler) WithProvisionDB(db provisionDB) *Handler {
	h.provDB = db
	return h
}

// runsListDB is the narrow slice of *database.Queries that AdminListRuns needs.
// Declaring it as an interface lets tests inject a fake without a live pgxpool
// — see Handler.runsStore() and WithRunsDB.
type runsListDB interface {
	ListAllRunsFiltered(ctx context.Context, filter database.RunsListFilter) ([]models.Run, error)
}

// runsStore returns the runsListDB in use. h.runsDB is non-nil only in
// tests; production code always falls through to h.db.
func (h *Handler) runsStore() runsListDB {
	if h.runsDB != nil {
		return h.runsDB
	}
	return h.db
}

// WithRunsDB injects a fake runsListDB for testing AdminListRuns without a real
// database connection. Do not call from production code.
func (h *Handler) WithRunsDB(db runsListDB) *Handler {
	h.runsDB = db
	return h
}

// podsListDB is the narrow slice of *database.Queries that ListPods needs.
// Declaring it as an interface lets tests inject a fake without a live pgxpool
// — see Handler.podsStore() and WithPodsDB.
type podsListDB interface {
	ListAllPods(ctx context.Context) ([]models.Pod, error)
	ListPodsByOwner(ctx context.Context, ownerID uuid.UUID) ([]models.Pod, error)
}

// podsStore returns the podsListDB in use. h.podsDB is non-nil only in
// tests; production code always falls through to h.db.
func (h *Handler) podsStore() podsListDB {
	if h.podsDB != nil {
		return h.podsDB
	}
	return h.db
}

// WithPodsDB injects a fake podsListDB for testing ListPods without a real
// database connection. Do not call from production code.
func (h *Handler) WithPodsDB(db podsListDB) *Handler {
	h.podsDB = db
	return h
}

// roleSeesAllPods reports whether the caller should receive every non-destroyed
// pod (with owner attribution). Instructors and admins share this view; students
// only see their own pods.
func roleSeesAllPods(role string) bool {
	return role == models.RoleAdmin || role == models.RoleInstructor
}

// PreflightVCenter is a type alias so the handlers package can name the
// interface without importing the preflight package directly in every file.
type PreflightVCenter = preflight.PreflightVCenter

// WithPreflightVCenter wires the vCenter preflight surface and its static
// configuration. Call this from main after connecting to vCenter to enable
// the preflight checks on the provision endpoint and the standalone
// /preflight endpoint. If not called, preflight is skipped and
// AdminPreflightTemplate returns 503.
func (h *Handler) WithPreflightVCenter(vc PreflightVCenter, cfg PreflightConfig) *Handler {
	h.vcPreflight = vc
	h.preflightCfg = cfg
	// The production vCenter client can resolve a source template's inventory
	// name to a moref; a resolver-less test fake cannot. Capture it when
	// present so clone_template preflight resolves exactly like the worker.
	if r, ok := vc.(templates.VMNameResolver); ok {
		h.vcResolver = r
	}
	return h
}

// AdminListVCenterTemplatesFolder returns enumerated VMs in the configured
// templates folder plus per-VM Crucible registration status. Cache TTL is 5
// minutes; pass ?refresh=true to bypass. Returns 503 if vCenter is not wired.
func (h *Handler) AdminListVCenterTemplatesFolder(w http.ResponseWriter, r *http.Request) {
	if h.templatesFolder == nil {
		respondError(w, r, http.StatusServiceUnavailable, "vCenter folder enumeration not configured")
		return
	}
	h.templatesFolder.ServeHTTP(w, r)
}

// invalidateTemplatesFolderCache should be called by template
// create/update/delete handlers so a newly-registered template appears in the
// admin browser immediately on the next request.
func (h *Handler) invalidateTemplatesFolderCache() {
	if h.templatesFolder != nil {
		h.templatesFolder.cache.Invalidate()
	}
}

func (h *Handler) auditLog(ctx context.Context, action string, opts ...audit.Option) {
	if h.db == nil {
		return
	}
	audit.Log(ctx, h.db, action, opts...)
}

// --- Pod Handlers ---

// ListPods returns the caller's pods. Instructors and admins receive every
// non-destroyed pod with owner attribution; students receive only their own.
func (h *Handler) ListPods(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	var pods []models.Pod
	var err error
	if roleSeesAllPods(role) {
		pods, err = h.podsStore().ListAllPods(r.Context())
	} else {
		pods, err = h.podsStore().ListPodsByOwner(r.Context(), userID)
	}
	if err != nil {
		h.logger.Error("list pods failed", "error", err, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	hideUnreadyPodCredentialsFromList(pods)
	respondJSON(w, http.StatusOK, pods)
}

// GetPod returns a specific pod with its VMs.
func (h *Handler) GetPod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil {
		h.logger.Error("get pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	// Verify ownership (or admin)
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	hideUnreadyPodCredentials(pod)
	respondJSON(w, http.StatusOK, pod)
}

// hideUnreadyPodCredentials prevents the API from showing either a pending
// generated password or the template's bootstrap password while the worker is
// still proving guest authentication. A credential becomes user-visible only
// with the running state that now follows successful authentication.
func hideUnreadyPodCredentials(pod *models.Pod) {
	if pod == nil {
		return
	}
	for i := range pod.VMs {
		vm := &pod.VMs[i]
		staticCredentials := vm.TemplateKind == models.TemplateKindCloneNoCustomize ||
			vm.TemplateKind == models.TemplateKindRegisteredExistingVM
		customizedCredentialsAccepted := vm.GuestCredentialsVerifiedAt != nil &&
			vm.VCenterVMID != nil &&
			*vm.VCenterVMID != "" &&
			vm.GuestCredentialsVerifiedVMID != nil &&
			*vm.GuestCredentialsVerifiedVMID == *vm.VCenterVMID
		if vm.Status == models.VMStatusRunning &&
			(staticCredentials || customizedCredentialsAccepted) {
			continue
		}
		vm.DefaultUsername = ""
		vm.DefaultPassword = ""
		vm.GeneratedUsername = ""
		vm.GeneratedPassword = ""
	}
}

func hideUnreadyPodCredentialsFromList(pods []models.Pod) {
	for i := range pods {
		hideUnreadyPodCredentials(&pods[i])
	}
}

// validatePodOwnerAccess returns 0 when the caller owns the pod or is an admin.
func validatePodOwnerAccess(podOwnerID, userID uuid.UUID, role string) int {
	if podOwnerID == userID || role == models.RoleAdmin {
		return 0
	}
	return http.StatusForbidden
}

// CreatePod creates the pod + VMs in the DB, then queues a provisioning job.
func (h *Handler) CreatePod(w http.ResponseWriter, r *http.Request) {
	if h.rejectProvisioning(w, r, provisioningRoutePodCreate) {
		return
	}

	var req models.CreatePodRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || len(req.VMs) == 0 {
		respondError(w, r, http.StatusBadRequest, "name and at least one VM are required")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	// Fetch user for quota check
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		respondError(w, r, http.StatusInternalServerError, "user not found")
		return
	}

	// Check quotas
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		h.logger.Error("get resource usage failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Resolve templates and calculate requested resources
	type resolvedVM struct {
		req      models.VMRequest
		template models.Template
		vcpus    int
		ramMB    int
		diskGB   int
	}
	var resolved []resolvedVM
	var totalVCPUs, totalRAM int

	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	for _, vm := range req.VMs {
		var found *models.Template
		for _, t := range templates {
			if t.ID == vm.TemplateID {
				found = &t
				break
			}
		}
		if found == nil {
			respondError(w, r, http.StatusBadRequest, "template not found or not accessible: "+vm.TemplateID.String())
			return
		}

		// Defense in depth: ensure students cannot use instructor_only templates
		if role == models.RoleStudent && found.Visibility == "instructor_only" {
			respondError(w, r, http.StatusForbidden, "template not found or not accessible: "+vm.TemplateID.String())
			return
		}

		vcpus := found.DefaultVCPUs
		if vm.VCPUs != nil {
			vcpus = *vm.VCPUs
		}
		ramMB := found.DefaultRAMMB
		if vm.RAMMB != nil {
			ramMB = *vm.RAMMB
		}
		if err := validateProvisioningRAM(ramMB); err != nil {
			respondError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		diskGB := found.DefaultDiskGB
		if vm.DiskGB != nil {
			diskGB = *vm.DiskGB
		}
		totalVCPUs += vcpus
		totalRAM += ramMB
		resolved = append(resolved, resolvedVM{req: vm, template: *found, vcpus: vcpus, ramMB: ramMB, diskGB: diskGB})
	}

	if err := ValidateQuotas(usage, user, 1, totalVCPUs, totalRAM); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
		} else {
			h.logger.Error("quota validation failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Compute expiration based on role
	var expiresAt *time.Time
	switch role {
	case models.RoleStudent:
		t := time.Now().Add(7 * 24 * time.Hour)
		expiresAt = &t
	case models.RoleInstructor:
		t := time.Now().Add(30 * 24 * time.Hour)
		expiresAt = &t
	}

	// Generate a short random salt for VM naming
	saltBytes := make([]byte, 3)
	if _, err := rand.Read(saltBytes); err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	salt := hex.EncodeToString(saltBytes)

	ctx := r.Context()

	// Begin transaction: create pod, checkout VLAN, create pod_vms
	tx, err := h.db.Pool().Begin(ctx)
	if err != nil {
		h.logger.Error("begin tx failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx)

	if err := h.validateQuotaTx(ctx, tx, userID, 1, totalVCPUs, totalRAM); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
		} else {
			h.logger.Error("transactional quota validation failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	podID := uuid.New()

	// Insert pod record first (FK target for vlan_pool.pod_id)
	var podCreatedAt, podUpdatedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO pods (id, owner_id, name, salt, vlan_id, subnet, status, expires_at)
		VALUES ($1, $2, $3, $4, 0, '', 'pending', $5)
		RETURNING created_at, updated_at
	`, podID, userID, req.Name, salt, expiresAt).Scan(
		&podCreatedAt, &podUpdatedAt,
	)
	if err != nil {
		h.logger.Error("create pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Checkout VLAN (now the pod exists, so FK is satisfied)
	vlanTag, subnet, err := h.db.CheckoutVLAN(ctx, tx, podID, "all")
	if err != nil {
		h.logger.Error("checkout VLAN failed", "error", err)
		respondError(w, r, http.StatusConflict, "no available VLAN slots")
		return
	}

	// Update pod with allocated VLAN info
	_, err = tx.Exec(ctx, `
		UPDATE pods SET vlan_id = $1, subnet = $2 WHERE id = $3
	`, vlanTag, subnet, podID)
	if err != nil {
		h.logger.Error("create pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Insert pod_vm records and build worker VM specs
	type workerVMSpec struct {
		PodVMID      uuid.UUID `json:"pod_vm_id"`
		TemplateName string    `json:"template_name"`
		VMName       string    `json:"vm_name"`
		VCPUs        int32     `json:"vcpus"`
		RAMMB        int64     `json:"ram_mb"`
		DiskGB       int       `json:"disk_gb"`
		OSType       string    `json:"os_type"`
		// Kind mirrors templates.kind so the provisioner can branch between
		// clone_with_customize / clone_no_customize / registered_existing_vm
		// without re-reading the template row.
		Kind string `json:"kind"`
		// AssignIP mirrors templates.assign_ip. When false, the provisioner
		// attaches the NIC but skips WaitForIP and leaves pod_vms.ip_address
		// NULL — the guest is expected to manage its own networking.
		AssignIP bool `json:"assign_ip"`
	}
	var vmSpecs []workerVMSpec

	for _, rv := range resolved {
		var vmID uuid.UUID
		var vmCreatedAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')
			RETURNING id, created_at
		`, podID, rv.template.ID, rv.req.DisplayName, rv.vcpus, rv.ramMB, rv.diskGB).Scan(&vmID, &vmCreatedAt)
		if err != nil {
			h.logger.Error("create pod_vm failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
			return
		}

		vmName := salt + "-" + sanitizeName(rv.req.DisplayName)
		kind := rv.template.Kind
		if kind == "" {
			kind = models.TemplateKindCloneWithCustomize
		}
		vmSpecs = append(vmSpecs, workerVMSpec{
			PodVMID:      vmID,
			TemplateName: rv.template.VCenterRef(),
			VMName:       vmName,
			VCPUs:        int32(rv.vcpus),
			RAMMB:        int64(rv.ramMB),
			DiskGB:       rv.diskGB,
			OSType:       rv.template.OSType,
			Kind:         kind,
			AssignIP:     rv.template.AssignIP,
		})
	}

	// Create job payload matching worker's CreatePodPayload struct
	type jobPayload struct {
		PodID   uuid.UUID      `json:"pod_id"`
		PodName string         `json:"pod_name"`
		VMs     []workerVMSpec `json:"vms"`
		UserID  string         `json:"user_id"`
	}
	payload, err := json.Marshal(jobPayload{
		PodID:   podID,
		PodName: req.Name,
		VMs:     vmSpecs,
		UserID:  userID.String(),
	})
	if err != nil {
		h.logger.Error("marshal pod create job payload failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to queue provisioning job")
		return
	}

	job, err := h.db.CreatePodCreateJobTx(ctx, tx, podID, payload)
	if err != nil {
		h.logger.Error("create job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "failed to queue provisioning job")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("commit tx failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	// Notify workers via NATS
	if err := h.jobEvents.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err, "job_id", job.ID)
	}

	// Audit
	audit.Log(r.Context(), h.db, "pod.create",
		audit.Resource("job", job.ID),
		audit.FromRequest(r),
		audit.Detail("pod_id", podID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"pod_id": podID,
		"status": "pending",
	})
}

// DeletePod cancels a never-started pending pod or queues a destruction job.
func (h *Handler) DeletePod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	if pod.Status == models.PodStatusDestroying {
		respondError(w, r, http.StatusConflict, "pod is already being destroyed")
		return
	}

	if pod.Status == models.PodStatusDestroyed {
		if pod.ErrorMessage == nil || *pod.ErrorMessage != models.PodErrorCancelledBeforeProvisioning {
			respondError(w, r, http.StatusConflict, "pod is already destroyed")
			return
		}
	}

	if decision, err := h.db.CancelPendingPodIfNeverStarted(r.Context(), podID); err != nil {
		h.logger.Error("cancel pod before start failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	} else if decision != nil && (decision.Outcome == database.PodDeletionOutcomeCancelled ||
		decision.Outcome == database.PodDeletionOutcomeAlreadyCancelled) {
		if decision.Outcome == database.PodDeletionOutcomeCancelled &&
			h.jobStatusEvents != nil &&
			decision.JobID != uuid.Nil {
			if err := h.jobStatusEvents.PublishRaw(
				fmt.Sprintf(events.SubjectJobStatus, decision.JobID),
				events.Event{
					Type:    "job.status",
					JobID:   decision.JobID.String(),
					Status:  models.JobStatusFailed,
					Message: models.PodErrorCancelledBeforeProvisioning,
				},
			); err != nil {
				h.logger.Warn("failed to publish job status event", "job_id", decision.JobID, "error", err)
			}
		}
		audit.Log(r.Context(), h.db, "pod.delete",
			audit.Resource("pod", podID),
			audit.FromRequest(r),
			audit.Detail("mode", "cancelled"),
			audit.Detail("pod_name", pod.Name),
			audit.Detail("pod_status", models.PodStatusDestroyed),
			audit.Detail("job_id", decision.JobID.String()),
		)
		body := map[string]any{
			"pod_id":     podID,
			"status":     "cancelled",
			"pod_status": models.PodStatusDestroyed,
		}
		if decision.JobID != uuid.Nil {
			body["job_id"] = decision.JobID
		}
		respondJSON(w, http.StatusOK, body)
		return
	}

	payload, _ := json.Marshal(map[string]string{"pod_id": podID.String(), "pod_name": pod.Name, "user_id": userID.String()})
	job, created, err := h.db.CreatePodDestroyJob(r.Context(), podID, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodDestroyBlockedByMutator) {
			respondError(w, r, http.StatusConflict, "pod has an operation in progress; retry deletion after it completes")
			return
		}
		if errors.Is(err, database.ErrPodJobRejected) {
			respondError(w, r, http.StatusConflict, "pod is already being destroyed")
			return
		}
		h.logger.Error("create destroy job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	jobCreatedPublisher := h.jobEvents
	if isNilJobCreatedPublisher(jobCreatedPublisher) {
		jobCreatedPublisher = h.events
	}
	if created && !isNilJobCreatedPublisher(jobCreatedPublisher) {
		if err := jobCreatedPublisher.PublishJobCreated(job.ID, job.Type); err != nil {
			h.logger.Warn("failed to publish job created event", "error", err)
		}
	}

	audit.Log(r.Context(), h.db, "pod.delete",
		audit.Resource("pod", podID),
		audit.FromRequest(r),
		audit.Detail("pod_name", pod.Name),
		audit.Detail("job_id", job.ID.String()),
	)

	responseStatus := http.StatusAccepted
	if !created && (job.Status == models.JobStatusCompleted || job.Status == models.JobStatusFailed) {
		responseStatus = http.StatusOK
	}
	respondJSON(w, responseStatus, map[string]any{
		"job_id": job.ID,
		"status": job.Status,
	})
}

func (h *Handler) AdminFinalizeOrphanedPodDestroy(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}
	var req struct {
		VLANID            int    `json:"vlan_id"`
		Subnet            string `json:"subnet"`
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleAdmin || userID == uuid.Nil {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}
	snapshot, err := h.db.FinalizeOrphanedPodDestroy(r.Context(), userID, podID, database.PodDestroyRecoveryAttestation{
		VLANID:            req.VLANID,
		Subnet:            req.Subnet,
		ConfirmationToken: req.ConfirmationToken,
	})
	if err != nil {
		if errors.Is(err, database.ErrPodDestroyRecoveryPrecondition) {
			respondError(w, r, http.StatusConflict, err.Error())
			return
		}
		h.logger.Error("finalize orphaned destroy failed", "pod_id", podID, "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	audit.Log(r.Context(), h.db, "pod.finalize_orphaned_destroy",
		audit.Resource("pod", podID),
		audit.FromRequest(r),
		audit.Detail("vlan_id", req.VLANID),
		audit.Detail("subnet", req.Subnet),
		audit.Detail("confirmation_token", req.ConfirmationToken),
		audit.Detail("destroy_job_id", snapshot.DestroyJobID.String()),
	)
	respondJSON(w, http.StatusOK, map[string]any{
		"pod_id":         podID,
		"status":         models.PodStatusDestroyed,
		"destroy_job_id": snapshot.DestroyJobID,
		"snapshot":       snapshot,
	})
}

// ExtendPod allows a user to extend their pod's expiration (attestation).
func (h *Handler) ExtendPod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	if pod.Status != models.PodStatusActive {
		respondError(w, r, http.StatusConflict, "pod must be active to extend")
		return
	}

	// Calculate new expiry based on role (+7d student, +30d instructor/admin)
	var extension time.Duration
	switch role {
	case models.RoleInstructor, models.RoleAdmin:
		extension = 30 * 24 * time.Hour
	default:
		extension = 7 * 24 * time.Hour
	}
	newExpiry := time.Now().Add(extension)

	attestation, err := h.db.ExtendPod(r.Context(), podID, userID, newExpiry)
	if err != nil {
		if errors.Is(err, database.ErrPodExtensionRejected) {
			respondError(w, r, http.StatusConflict, "pod cannot be extended after destruction is queued")
			return
		}
		h.logger.Error("extend pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "pod.extend",
		audit.Resource("pod", podID),
		audit.FromRequest(r),
		audit.Detail("previous_expires_at", fmt.Sprintf("%v", attestation.PreviousExpiresAt)),
		audit.Detail("new_expires_at", newExpiry.Format(time.RFC3339)),
	)

	respondJSON(w, http.StatusOK, map[string]any{
		"pod_id":           podID,
		"expires_at":       newExpiry.Format(time.RFC3339),
		"extended_by_days": int(extension.Hours() / 24),
	})
}

// AdminExtendPod allows an admin to extend any pod's expiration.
func (h *Handler) AdminExtendPod(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	if pod.Status != models.PodStatusActive {
		respondError(w, r, http.StatusConflict, "pod must be active to extend")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	newExpiry := time.Now().Add(30 * 24 * time.Hour)

	if _, err := h.db.ExtendPod(r.Context(), podID, userID, newExpiry); err != nil {
		if errors.Is(err, database.ErrPodExtensionRejected) {
			respondError(w, r, http.StatusConflict, "pod cannot be extended after destruction is queued")
			return
		}
		h.logger.Error("admin extend pod failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	audit.Log(r.Context(), h.db, "pod.admin_extend",
		audit.Resource("pod", podID),
		audit.FromRequest(r),
		audit.Detail("new_expires_at", newExpiry.Format(time.RFC3339)),
	)

	respondJSON(w, http.StatusOK, map[string]any{
		"pod_id":           podID,
		"expires_at":       newExpiry.Format(time.RFC3339),
		"extended_by_days": 30,
	})
}

// DeleteVM queues a single VM destruction job.
func (h *Handler) DeleteVM(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid vm id")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	// Verify VM belongs to this pod
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		respondError(w, r, http.StatusNotFound, "vm not found in this pod")
		return
	}
	if vm.Status == models.VMStatusDeleted {
		respondError(w, r, http.StatusConflict, "vm is already deleted")
		return
	}

	vmName := vm.DisplayName
	if vm.VCenterVMName != nil {
		vmName = *vm.VCenterVMName
	}
	payload, _ := json.Marshal(map[string]string{
		"pod_id":    podID.String(),
		"pod_vm_id": vmID.String(),
		"pod_name":  pod.Name,
		"vm_name":   vmName,
		"user_id":   userID.String(),
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, models.JobTypeVMDestroy, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			respondError(w, r, http.StatusConflict, "pod is not available for VM operations")
			return
		}
		h.logger.Error("create vm destroy job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.delete",
		audit.Resource("vm", vmID),
		audit.FromRequest(r),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// AddVM queues a job to add a VM to an existing pod.
func (h *Handler) AddVM(w http.ResponseWriter, r *http.Request) {
	if h.rejectProvisioning(w, r, provisioningRouteVMAdd) {
		return
	}

	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}

	var req models.AddVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DisplayName == "" {
		respondError(w, r, http.StatusBadRequest, "display_name is required")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if pod.OwnerID != userID && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	if pod.Status != models.PodStatusActive {
		respondError(w, r, http.StatusConflict, "pod must be active to add VMs")
		return
	}

	if !pod.AllowVMAdditions && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "this environment does not allow adding VMs")
		return
	}

	// Resolve template
	templates, err := h.db.ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	var found *models.Template
	for _, t := range templates {
		if t.ID == req.TemplateID {
			found = &t
			break
		}
	}
	if found == nil {
		respondError(w, r, http.StatusBadRequest, "template not found or not accessible")
		return
	}

	// Check quotas
	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		respondError(w, r, http.StatusInternalServerError, "user not found")
		return
	}
	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	vcpus := found.DefaultVCPUs
	if req.VCPUs != nil {
		vcpus = *req.VCPUs
	}
	ram := found.DefaultRAMMB
	if req.RAMMB != nil {
		ram = *req.RAMMB
	}
	if err := validateProvisioningRAM(ram); err != nil {
		respondError(w, r, http.StatusBadRequest, err.Error())
		return
	}
	diskGB := found.DefaultDiskGB
	if req.DiskGB != nil {
		diskGB = *req.DiskGB
	}

	if err := ValidateQuotas(usage, user, 0, vcpus, ram); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
		} else {
			h.logger.Error("quota validation failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	// Create pod_vm record
	vm := &models.PodVM{
		ID:          uuid.New(),
		PodID:       podID,
		TemplateID:  req.TemplateID,
		DisplayName: req.DisplayName,
		VCPUs:       vcpus,
		RAMMB:       ram,
		DiskGB:      diskGB,
		Status:      models.VMStatusPending,
	}
	// Queue vm_add job
	payload, _ := json.Marshal(map[string]string{
		"pod_id":        podID.String(),
		"pod_vm_id":     vm.ID.String(),
		"template_name": found.VCenterRef(),
		"vm_name":       pod.Salt + "-" + sanitizeName(req.DisplayName),
		"display_name":  req.DisplayName,
		"user_id":       userID.String(),
	})
	tx, err := h.db.Pool().Begin(r.Context())
	if err != nil {
		h.logger.Error("begin vm add tx failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(r.Context())

	if err := h.validateQuotaTx(r.Context(), tx, userID, 0, vcpus, ram); err != nil {
		var qe *QuotaError
		if errors.As(err, &qe) {
			respondError(w, r, http.StatusConflict, qe.Error())
		} else {
			h.logger.Error("transactional quota validation failed", "error", err)
			respondError(w, r, http.StatusInternalServerError, "internal error")
		}
		return
	}

	job, err := h.db.CreateVMAddJobTx(r.Context(), tx, vm, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			respondError(w, r, http.StatusConflict, "pod must be active to add VMs")
			return
		}
		h.logger.Error("create vm add job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.logger.Error("commit vm add tx failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.jobEvents.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm.add",
		audit.Resource("vm", vm.ID),
		audit.FromRequest(r),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"vm_id":  vm.ID,
		"status": "pending",
	})
}

// VMPowerAction handles start/stop/restart for a VM within a pod.
func (h *Handler) VMPowerAction(w http.ResponseWriter, r *http.Request) {
	podID, err := uuid.Parse(chi.URLParam(r, "podID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid pod id")
		return
	}
	vmID, err := uuid.Parse(chi.URLParam(r, "vmID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid vm id")
		return
	}

	// Determine action from the last path segment
	action := path.Base(r.URL.Path)
	var jobType string
	switch action {
	case "start":
		jobType = models.JobTypeVMStart
	case "stop":
		jobType = models.JobTypeVMStop
	case "restart":
		jobType = models.JobTypeVMRestart
	case "reset":
		jobType = models.JobTypeVMReset
	default:
		respondError(w, r, http.StatusBadRequest, "unknown power action")
		return
	}

	pod, err := h.db.GetPodByID(r.Context(), podID)
	if err != nil || pod == nil {
		respondError(w, r, http.StatusNotFound, "pod not found")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())
	if status := validatePodOwnerAccess(pod.OwnerID, userID, role); status != 0 {
		respondError(w, r, status, "forbidden")
		return
	}

	// Verify VM belongs to this pod
	vm, err := h.db.GetPodVM(r.Context(), vmID)
	if err != nil || vm.PodID != podID {
		respondError(w, r, http.StatusNotFound, "vm not found in this pod")
		return
	}
	if vm.Status == models.VMStatusDeleted {
		respondError(w, r, http.StatusConflict, "vm is deleted")
		return
	}

	payload, _ := json.Marshal(map[string]string{
		"pod_id":    podID.String(),
		"pod_vm_id": vmID.String(),
		"user_id":   userID.String(),
		"vm_name":   vm.DisplayName,
		"pod_name":  pod.Name,
	})
	job, err := h.db.CreateVMJob(r.Context(), podID, vmID, jobType, payload)
	if err != nil {
		if errors.Is(err, database.ErrPodJobRejected) {
			respondError(w, r, http.StatusConflict, "pod is not available for VM operations")
			return
		}
		h.logger.Error("create vm power job failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	if err := h.events.PublishJobCreated(job.ID, job.Type); err != nil {
		h.logger.Warn("failed to publish job created event", "error", err)
	}

	audit.Log(r.Context(), h.db, "vm."+action,
		audit.Resource("vm", vmID),
		audit.FromRequest(r),
		audit.Detail("job_id", job.ID.String()),
	)

	respondJSON(w, http.StatusAccepted, map[string]any{
		"job_id": job.ID,
		"status": "pending",
	})
}

// ListTemplates returns templates accessible to the current user.
// Internal templates (is_internal=true, e.g. synthetic-noop) are
// filtered out here so they don't appear in the user-facing picker
// at /pods/new. Two escape hatches keep system flows working:
//
//  1. Admins should use /admin/templates (ListAllTemplates) which
//     shows everything regardless of is_internal.
//  2. Users with an EXPLICIT per-user-id template_access grant still
//     see the template even if is_internal=true. This is how the
//     synthetic monitor user keeps access to synthetic-noop so the
//     UI E2E lifecycle check (which walks /pods/new) continues to
//     find the template card. Role-based grants alone do not bypass
//     the filter — only an explicit user_id row in template_access.
//
// The server-side pod-create / vm-add / blueprint handlers continue
// to use ListTemplatesForUser unfiltered so internal template IDs
// resolve correctly.
func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())
	role := middleware.RoleFromContext(r.Context())

	templates, err := h.templateStore().ListTemplatesForUser(r.Context(), userID, role)
	if err != nil {
		h.logger.Error("list templates failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	explicit, err := h.templateStore().ListExplicitTemplateAccessForUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("list explicit template access failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	visible := templates[:0]
	for _, t := range templates {
		if t.IsInternal {
			if _, ok := explicit[t.ID]; !ok {
				continue
			}
		}
		visible = append(visible, t)
	}
	respondJSON(w, http.StatusOK, newTemplatePublicList(visible))
}

// --- Template Pinning (migration 000030) ---

// AdminReorderTemplates updates the pin state and ordering for templates.
// Instructor/admin only. Request body is a map of template IDs to {pinned, pin_order}.
// All updates are atomic; either all succeed or none do. Returns 403 if role < instructor.
func (h *Handler) AdminReorderTemplates(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	var reqBody map[string]struct {
		Pinned   bool `json:"pinned"`
		PinOrder int  `json:"pin_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	// Convert string keys to UUIDs
	req := make(map[uuid.UUID]database.PinState)
	for k, v := range reqBody {
		id, err := uuid.Parse(k)
		if err != nil {
			respondError(w, r, http.StatusBadRequest, "invalid template id: "+k)
			return
		}
		req[id] = database.PinState{Pinned: v.Pinned, PinOrder: v.PinOrder}
	}

	userID := middleware.UserIDFromContext(r.Context())
	err := h.templatePinStore().ReorderTemplates(r.Context(), req, userID)
	if err != nil {
		if errors.Is(err, database.ErrTemplateNotFound) {
			h.logger.Warn("reorder templates target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified template not found")
			return
		}
		h.logger.Error("reorder templates failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "templates.reorder",
		audit.FromRequest(r),
		audit.Detail("count", fmt.Sprintf("%d", len(req))),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminSetTemplatePin pins a single template at the requested position.
func (h *Handler) AdminSetTemplatePin(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	templateID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}

	var req struct {
		PinOrder int `json:"pin_order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	if err := h.templatePinStore().SetTemplatePin(r.Context(), templateID, true, req.PinOrder, userID); err != nil {
		if errors.Is(err, database.ErrTemplateNotFound) {
			h.logger.Warn("set template pin target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified template not found")
			return
		}
		h.logger.Error("set template pin failed", "error", err, "template_id", templateID, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "template.pin",
		audit.Resource("template", templateID),
		audit.FromRequest(r),
		audit.Detail("pin_order", fmt.Sprintf("%d", req.PinOrder)),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// AdminUnpinTemplate clears the pinned state for a single template.
func (h *Handler) AdminUnpinTemplate(w http.ResponseWriter, r *http.Request) {
	role := middleware.RoleFromContext(r.Context())
	if role != models.RoleInstructor && role != models.RoleAdmin {
		respondError(w, r, http.StatusForbidden, "forbidden")
		return
	}

	templateID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}

	userID := middleware.UserIDFromContext(r.Context())
	if err := h.templatePinStore().SetTemplatePin(r.Context(), templateID, false, 0, userID); err != nil {
		if errors.Is(err, database.ErrTemplateNotFound) {
			h.logger.Warn("unpin template target not found", "error", err, "user_id", userID)
			respondError(w, r, http.StatusNotFound, "Specified template not found")
			return
		}
		h.logger.Error("unpin template failed", "error", err, "template_id", templateID, "user_id", userID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.auditLog(r.Context(), "template.unpin",
		audit.Resource("template", templateID),
		audit.FromRequest(r),
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Job Handlers ---

// GetJobStatus returns the current status of a job.
func (h *Handler) GetJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID, err := uuid.Parse(chi.URLParam(r, "jobID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid job id")
		return
	}

	job, err := h.db.GetJob(r.Context(), jobID)
	if err != nil || job == nil {
		respondError(w, r, http.StatusNotFound, "job not found")
		return
	}

	respondJSON(w, http.StatusOK, models.JobStatusResponse{
		ID:        job.ID,
		Type:      job.Type,
		Status:    job.Status,
		CreatedAt: job.CreatedAt.Format(time.RFC3339),
	})
}

// ListMyJobs returns the current user's recent jobs.
func (h *Handler) ListMyJobs(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	jobs, err := h.db.ListJobsByUser(r.Context(), userID)
	if err != nil {
		h.logger.Error("list user jobs failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if jobs == nil {
		jobs = []models.Job{}
	}
	respondJSON(w, http.StatusOK, jobs)
}

// --- Auth Handlers ---

// GetMe returns the current user's profile and resource usage.
func (h *Handler) GetMe(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserIDFromContext(r.Context())

	user, err := h.db.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		respondError(w, r, http.StatusNotFound, "user not found")
		return
	}

	usage, err := h.db.GetResourceUsage(r.Context(), userID)
	if err != nil {
		h.logger.Error("get resource usage failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	usage.MaxVCPUs = user.MaxVCPUs
	usage.MaxRAMMB = user.MaxRAMMB
	usage.MaxPods = user.MaxPods

	respondJSON(w, http.StatusOK, models.MeResponse{
		User:          *user,
		ResourceUsage: *usage,
	})
}

// --- Admin Handlers ---

// AdminListUsers returns all users (admin only).
func (h *Handler) AdminListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.db.ListUsers(r.Context())
	if err != nil {
		h.logger.Error("admin list users failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, users)
}

// AdminUpdateQuotas updates a user's resource quotas.
func (h *Handler) AdminUpdateQuotas(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid user id")
		return
	}

	var req models.UpdateQuotaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.db.UpdateUserQuotas(r.Context(), userID, req); err != nil {
		h.logger.Error("update quotas failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AdminListTemplates returns all templates including inactive (admin only).
func (h *Handler) AdminListTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := h.db.ListAllTemplates(r.Context())
	if err != nil {
		h.logger.Error("admin list templates failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	respondJSON(w, http.StatusOK, templates)
}

// AdminCreateTemplate creates a new template.
func (h *Handler) AdminCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req models.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.VCenterTemplate == "" || req.OSType == "" {
		respondError(w, r, http.StatusBadRequest, "name, vcenter_template, and os_type are required")
		return
	}

	tmpl, err := h.db.CreateTemplate(r.Context(), req)
	if err != nil {
		h.logger.Error("create template failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	h.invalidateTemplatesFolderCache()
	respondJSON(w, http.StatusCreated, tmpl)
}

// AdminSetTemplateAccess sets access rules for a template.
func (h *Handler) AdminSetTemplateAccess(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}

	var req models.SetTemplateAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.db.SetTemplateAccess(r.Context(), templateID, req.Rules); err != nil {
		h.logger.Error("set template access failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// AdminUpdateTemplate partially updates a template.
func (h *Handler) AdminUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}

	var req models.UpdateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	tmpl, err := h.db.UpdateTemplate(r.Context(), templateID, req)
	if err != nil {
		if errors.Is(err, database.ErrTemplateStale) {
			respondError(w, r, http.StatusConflict, "template was modified by another user; refresh and try again")
			return
		}
		h.logger.Error("update template failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if tmpl == nil {
		respondError(w, r, http.StatusNotFound, "template not found")
		return
	}

	h.invalidateTemplatesFolderCache()
	respondJSON(w, http.StatusOK, tmpl)
}

// AdminDeleteTemplate deletes a template and (if it has a staging or
// post-build VM in vCenter) destroys that VM first so we don't orphan
// it. Refuses with 409 when a worker is actively building the VM
// (states `provisioning` / `generalizing`), when an ACTIVE pod still
// depends on the template, or when a blueprint references it — in each
// case the operator must resolve the dependency first.
//
// Order matters: guard checks and the vCenter destroy happen BEFORE the
// row delete. If the vCenter call fails we keep the row so the operator
// can retry — otherwise we'd leak the moref forever. The row delete goes
// through DeleteTemplateWithHistory, which also clears leftover pod_vms
// rows from long-destroyed pods; without that the NOT NULL / NO ACTION
// pod_vms_template_id_fkey rejects the delete with SQLSTATE 23503 and the
// operator gets a 500 after the staging VM has already been destroyed.
func (h *Handler) AdminDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}
	unsafeReplicaBuild, err := h.db.HasUnsafeTemplateReplicaBuildForDeletion(r.Context(), templateID)
	if err != nil {
		h.logger.Error("delete template: check unsafe replica builds failed", "error", err, "template_id", templateID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if unsafeReplicaBuild {
		respondError(w, r, http.StatusConflict,
			"template has an active, accepted, or not-fully-cleaned source replica build")
		return
	}

	tmpl, err := h.db.GetTemplateByID(r.Context(), templateID)
	if err != nil {
		h.logger.Error("load template for delete failed", "error", err, "template_id", templateID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if tmpl == nil {
		// Treat as already-deleted — idempotent.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if refuse, reason := templateDeleteStateRefusal(tmpl.TemplateState); refuse {
		h.writeStateConflict(w, tmpl, reason)
		return
	}

	// Refuse (409) if an ACTIVE pod still depends on this template. Deleting
	// it would strand a live student VM's provenance, and the destroyed-pod
	// cleanup below would happily remove the live pod_vms row too. Historical
	// (destroyed-pod) rows are fine — DeleteTemplateWithHistory mops those up.
	if _, deps, derr := h.db.ListTemplateDependents(r.Context(), templateID); derr != nil {
		h.logger.Error("delete template: list dependents failed", "error", derr, "template_id", templateID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	} else if len(deps) > 0 {
		respondError(w, r, http.StatusConflict, fmt.Sprintf("template is in use by %d active pod VM(s); destroy those pods before deleting the template", len(deps)))
		return
	}

	// Refuse (409) if a blueprint references this template. blueprint_vms is a
	// NOT NULL / NO ACTION FK, so this is a real dependency that would 500 the
	// delete otherwise; the operator must detach it from the blueprint first.
	if n, berr := h.db.CountTemplateBlueprintRefs(r.Context(), templateID); berr != nil {
		h.logger.Error("delete template: count blueprint refs failed", "error", berr, "template_id", templateID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	} else if n > 0 {
		respondError(w, r, http.StatusConflict, fmt.Sprintf("template is used by %d blueprint VM definition(s); remove it from those blueprints before deleting", n))
		return
	}

	var (
		destroyErr error
		destroyed  bool
	)
	err = h.db.WithTemplateReplicaBuildLifecycleLock(
		r.Context(),
		templateID,
		func(lockCtx context.Context) error {
			unsafe, err := h.db.HasUnsafeTemplateReplicaBuildForDeletion(lockCtx, templateID)
			if err != nil {
				return err
			}
			if unsafe {
				return database.ErrTemplateReplicaBuildConflict
			}
			if tmpl.VCenterVMID != "" {
				if h.vc == nil {
					// Production always wires vc; this branch protects test/dev
					// configs from silently orphaning VMs.
					h.logger.Warn("template delete: vCenter client not configured, leaving VM in place",
						"template_id", tmpl.ID, "moref", tmpl.VCenterVMID)
				} else {
					if err := h.vc.DestroyVM(lockCtx, tmpl.VCenterVMID); err != nil {
						destroyErr = err
						return err
					}
					destroyed = true
				}
			}
			return h.db.DeleteTemplateWithHistory(lockCtx, templateID)
		},
	)
	if errors.Is(err, database.ErrTemplateReplicaBuildConflict) {
		respondError(w, r, http.StatusConflict,
			"template has an active, accepted, or not-fully-cleaned source replica build")
		return
	}
	if destroyErr != nil {
		h.logger.Error("template delete: destroy staging VM failed",
			"template_id", tmpl.ID, "moref", tmpl.VCenterVMID, "error", destroyErr)
		audit.Log(r.Context(), h.db, "template.delete_failed",
			audit.Resource("template", tmpl.ID),
			audit.FromRequest(r),
			audit.Detail("moref", tmpl.VCenterVMID),
			audit.Detail("error", destroyErr.Error()),
		)
		respondError(w, r, http.StatusBadGateway, "failed to destroy staging VM in vCenter: "+destroyErr.Error())
		return
	}
	if err != nil {
		h.logger.Error("delete template failed", "error", err, "template_id", templateID)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if destroyed {
		h.logger.Info("template delete: destroyed staging VM",
			"template_id", tmpl.ID, "moref", tmpl.VCenterVMID)
	}

	audit.Log(r.Context(), h.db, "template.delete",
		audit.Resource("template", tmpl.ID),
		audit.FromRequest(r),
		audit.Detail("name", tmpl.Name),
		audit.Detail("state", tmpl.TemplateState),
		audit.Detail("moref", tmpl.VCenterVMID),
	)
	h.invalidateTemplatesFolderCache()
	w.WriteHeader(http.StatusNoContent)
}

// templateDeleteStateRefusal reports whether AdminDeleteTemplate should
// refuse the request because a worker job is currently mid-flight on
// the staging VM. Returning (false, "") means proceed.
//
// We refuse for `provisioning` (worker is cloning right now) and
// `generalizing` (worker is running sysprep / cloud-init). For all
// other states the VM is at rest — `configuring` means the operator
// is interacting with it directly via the build console, but no
// worker job is running, so it's safe to destroy. `ready` / `active`
// have a converted-to-template VM. `error` has whatever state the
// worker left behind, and the whole point of delete-with-cleanup is
// to recover from those. `draft` has no VM.
func templateDeleteStateRefusal(state string) (bool, string) {
	switch state {
	case models.TemplateStateProvisioning:
		return true, "cannot delete while provisioning; wait for the job to settle or cancel first"
	case models.TemplateStateGeneralizing:
		return true, "cannot delete while generalizing; wait for the job to settle or cancel first"
	case models.TemplateStateVerifying:
		return true, "cannot delete while verifying (smoke test running); wait for the job to settle first"
	}
	return false, ""
}

// AdminListTemplateDependents returns VMs that depend on a template's base disk.
func (h *Handler) AdminListTemplateDependents(w http.ResponseWriter, r *http.Request) {
	templateID, err := uuid.Parse(chi.URLParam(r, "templateID"))
	if err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid template id")
		return
	}

	templateName, vms, err := h.db.ListTemplateDependents(r.Context(), templateID)
	if err != nil {
		h.logger.Error("list template dependents failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if vms == nil {
		vms = []models.TemplateDependentVM{}
	}

	resp := models.TemplateDependentsResponse{
		TemplateID:   templateID.String(),
		TemplateName: templateName,
		ActiveVMs:    len(vms),
		VMs:          vms,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// AdminListJobs returns all jobs.
func (h *Handler) AdminListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.db.ListAllJobs(r.Context())
	if err != nil {
		h.logger.Error("admin list jobs failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if jobs == nil {
		jobs = []models.Job{}
	}
	respondJSON(w, http.StatusOK, jobs)
}

// AdminListAuditLog returns audit log entries.
func (h *Handler) AdminListAuditLog(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListAuditLog(r.Context())
	if err != nil {
		h.logger.Error("admin list audit log failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if entries == nil {
		entries = []models.AuditLog{}
	}
	respondJSON(w, http.StatusOK, entries)
}

// --- VLAN Pool Admin Handlers ---

// AdminListVLANPool returns all VLAN pool entries.
func (h *Handler) AdminListVLANPool(w http.ResponseWriter, r *http.Request) {
	entries, err := h.db.ListVLANPool(r.Context())
	if err != nil {
		h.logger.Error("admin list vlan pool failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if entries == nil {
		entries = []models.VLANPoolEntry{}
	}
	respondJSON(w, http.StatusOK, entries)
}

// AdminAddVLAN adds a new VLAN to the pool.
func (h *Handler) AdminAddVLAN(w http.ResponseWriter, r *http.Request) {
	var req models.AddVLANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.VLANTag < 1 || req.VLANTag > 4094 || req.Subnet == "" || req.HostScope == "" {
		respondError(w, r, http.StatusBadRequest, "vlan_tag (1-4094), subnet, and host_scope are required")
		return
	}

	entry, err := h.db.AddVLAN(r.Context(), req)
	if err != nil {
		h.logger.Error("add VLAN failed", "error", err)
		respondError(w, r, http.StatusConflict, "failed to add VLAN (may already exist)")
		return
	}

	respondJSON(w, http.StatusCreated, entry)
}

// AdminUpdateVLAN updates a VLAN pool entry's scope.
func (h *Handler) AdminUpdateVLAN(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "vlanID")
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid vlan id")
		return
	}

	var req models.UpdateVLANRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid request body")
		return
	}

	entry, err := h.db.UpdateVLAN(r.Context(), id, req)
	if err != nil {
		h.logger.Error("update VLAN failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if entry == nil {
		respondError(w, r, http.StatusNotFound, "VLAN not found")
		return
	}

	respondJSON(w, http.StatusOK, entry)
}

// AdminRemoveVLAN removes a VLAN from the pool (only if unallocated).
func (h *Handler) AdminRemoveVLAN(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "vlanID")
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		respondError(w, r, http.StatusBadRequest, "invalid vlan id")
		return
	}

	if err := h.db.RemoveVLAN(r.Context(), id); err != nil {
		h.logger.Error("remove VLAN failed", "error", err)
		respondError(w, r, http.StatusConflict, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// Health returns service health status.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- Helpers ---

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// respondError writes a structured JSON error body:
//
//	{"error": "<message>", "request_id": "<chi request id>"}
//
// It is the JSON counterpart of the plain-text http.Error used historically.
// The UI's apiFetch parses the response body as JSON, so http.Error (which
// emits text/plain) left ApiError.body null and the real reason ("name is
// required", etc.) never reached the user. Handlers on the user-facing form
// surface should use respondError so the client can surface the actual reason.
//
// request_id is best-effort: it echoes the chi RequestID middleware value when
// present so a support request can be correlated with server logs.
func respondError(w http.ResponseWriter, r *http.Request, status int, message string) {
	body := map[string]string{"error": message}
	if reqID := chimiddleware.GetReqID(r.Context()); reqID != "" {
		body["request_id"] = reqID
	}
	respondJSON(w, status, body)
}

// sanitizeName converts a display name to a DNS-safe slug.
func sanitizeName(name string) string {
	result := make([]byte, 0, len(name))
	for _, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
		} else if c == ' ' || c == '_' || c == '.' {
			if len(result) > 0 && result[len(result)-1] != '-' {
				result = append(result, '-')
			}
		}
	}
	// Trim trailing dash
	for len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// --- Paginated Audit Log ---

// AdminSearchAuditLog returns filtered, paginated audit log entries.
func (h *Handler) AdminSearchAuditLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := database.AuditLogFilter{
		Action:       q.Get("action"),
		ResourceType: q.Get("resource_type"),
	}

	if p := q.Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &filter.Page)
	}
	if pp := q.Get("per_page"); pp != "" {
		fmt.Sscanf(pp, "%d", &filter.PerPage)
	}
	if uid := q.Get("user_id"); uid != "" {
		if id, err := uuid.Parse(uid); err == nil {
			filter.UserID = &id
		}
	}
	if rid := q.Get("resource_id"); rid != "" {
		if id, err := uuid.Parse(rid); err == nil {
			filter.ResourceID = &id
		}
	}
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			filter.Since = &t
		}
	}
	if u := q.Get("until"); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			filter.Until = &t
		}
	}

	page, err := h.db.ListAuditLogPaginated(r.Context(), filter)
	if err != nil {
		h.logger.Error("admin search audit log failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if page.Entries == nil {
		page.Entries = []models.AuditLog{}
	}
	respondJSON(w, http.StatusOK, page)
}

// --- Session Admin ---

// AdminListSessions returns all active user sessions.
func (h *Handler) AdminListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.db.ListActiveSessions(r.Context())
	if err != nil {
		h.logger.Error("admin list sessions failed", "error", err)
		respondError(w, r, http.StatusInternalServerError, "internal error")
		return
	}
	if sessions == nil {
		sessions = []database.ActiveSession{}
	}
	respondJSON(w, http.StatusOK, sessions)
}
