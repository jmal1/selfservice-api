package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/objectstore"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// pipelineMetricsSink is the metrics surface the provisioner uses for the
// template + image pipeline. It is intentionally narrow so tests can inject a
// fake without pulling in the full PipelineMetrics implementation.
type pipelineMetricsSink interface {
	RecordImageImport(kind, result string, d time.Duration, bytesMoved int64)
	SetImageUploadsStuck(n int)
	RecordTemplateTransition(from, to string)
	RecordTemplateVerify(result string)
	RecordTemplateJob(jobType string, d time.Duration)
	SetTemplateStates(counts map[string]int)
	SetTemplatesStuck(n int)
	// Retry metrics — added with migration 000026.
	RecordJobRetry(jobType, reason string)
	RecordJobRetryExhausted(jobType string)
	SetJobRetryPending(n int)
	// L1 trust-tier revalidation metrics — added with migration 000027.
	RecordTemplateValidation(templateID, result string)
	SetTemplateLastValidated(templateID string, unixSec float64)
	Push(ctx context.Context) error
}

// jobStatusUpdater covers all job-row mutations that processJobLifecycle needs,
// including the retry scheduling path. *database.Queries satisfies this;
// tests inject a lightweight stub.
type jobStatusUpdater interface {
	UpdateJobStatus(ctx context.Context, id uuid.UUID, status string, result []byte) error
	RetryJob(ctx context.Context, id uuid.UUID, nextAt time.Time) error
}

var _ pipelineMetricsSink = (*PipelineMetrics)(nil)
var _ jobStatusUpdater = (*database.Queries)(nil)

// Provisioner orchestrates pod lifecycle operations.
type Provisioner struct {
	db     *database.Queries
	vc     *vcenter.Client
	opn    *opnsense.Client
	opnSSH *opnsense.SSHClient
	nats   *events.Client
	logger *slog.Logger

	// DestroyFailedPusher is optional. When set, RetryFailedDestroys pushes
	// the current count to Pushgateway so the
	// CruciblePodsStuckInDestroyFailed alert can fire within 15m. Nil disables.
	DestroyFailedPusher *DestroyFailedPusher

	// Image-import dependencies, set via EnableImageImport. They are optional
	// so a worker deployed without an object store still starts and serves
	// every other job type; image_import jobs then fail loudly with a clear
	// message rather than nil-panicking mid-upload.
	objects   imageObjectStore
	pipeline  pipelineMetricsSink
	imageCfg  ImageImportConfig
	healthCfg TemplateHealthReconcilerConfig

	// cloneMu serialises concurrent clones from the same source VM moref.
	// Key: source moref (string), value: chan struct{} (semaphore of size 1).
	// See acquireCloneLock.  One provision-worker replica is confirmed in
	// deploy/helm/selfservice/values.yaml (replicaCount.worker: 1), so an
	// in-process mutex is sufficient.
	cloneMu sync.Map
}

// EnablePipelineMetrics wires the shared pipeline metrics sink used by the
// template reconciler, template jobs, and image import jobs.
func (p *Provisioner) EnablePipelineMetrics(metrics pipelineMetricsSink) {
	p.pipeline = metrics
}

// EnableImageImport wires the dependencies needed to process image_import jobs.
// Called by the worker at startup once an object store is configured; when it
// is not called, ImportImage returns an explanatory message instead of panicking.
func (p *Provisioner) EnableImageImport(objects *objectstore.Client, metrics pipelineMetricsSink, cfg ImageImportConfig) {
	p.objects = objects
	if metrics != nil {
		p.pipeline = metrics
	}
	p.imageCfg = cfg
}

// ConfigureTemplateHealth stores the production configuration used by durable
// confirmation jobs. A job can outlive the leader cycle that created it.
func (p *Provisioner) ConfigureTemplateHealth(cfg TemplateHealthReconcilerConfig) {
	p.healthCfg = cfg
}

// New creates a provisioner with all required clients.
func New(
	db *database.Queries,
	vc *vcenter.Client,
	opn *opnsense.Client,
	opnSSH *opnsense.SSHClient,
	nats *events.Client,
	logger *slog.Logger,
) *Provisioner {
	return &Provisioner{
		db:     db,
		vc:     vc,
		opn:    opn,
		opnSSH: opnSSH,
		nats:   nats,
		logger: logger,
	}
}

// ProcessJob dispatches a job to the correct workflow.
func (p *Provisioner) ProcessJob(ctx context.Context, job *models.Job) error {
	p.logger.Info("processing job", "id", job.ID, "type", job.Type)
	return processJobLifecycle(ctx, p.db, p.pipeline, job, p.publishProgress, func(ctx context.Context, job *models.Job) error {
		switch job.Type {
		case models.JobTypePodCreate:
			return p.CreatePod(ctx, job)
		case models.JobTypePodDestroy:
			return p.DestroyPod(ctx, job)
		case models.JobTypeVMStart:
			return p.PowerVM(ctx, job, "start")
		case models.JobTypeVMStop:
			return p.PowerVM(ctx, job, "stop")
		case models.JobTypeVMRestart:
			return p.PowerVM(ctx, job, "restart")
		case models.JobTypeVMReset:
			return p.PowerVM(ctx, job, "reset")
		case models.JobTypeVMDestroy:
			return p.DestroyVM(ctx, job)
		case models.JobTypeVMAdd:
			return p.AddVM(ctx, job)
		case models.JobTypeVMSnapshot:
			return p.SnapshotVM(ctx, job)
		case models.JobTypeVMRevert:
			return p.RevertVM(ctx, job)
		case models.JobTypeVMSnapshotDelete:
			return p.DeleteSnapshot(ctx, job)
		case models.JobTypeTemplateProvision:
			return p.ProvisionTemplate(ctx, job)
		case models.JobTypeTemplateGeneralize:
			return p.GeneralizeTemplate(ctx, job)
		case models.JobTypeTemplateVerify:
			return p.VerifyTemplate(ctx, job)
		case models.JobTypeTemplateRevalidate:
			return p.RevalidateL1Template(ctx, job)
		case models.JobTypeTemplateHealthConfirm:
			return p.ConfirmTemplateHealth(ctx, job)
		case models.JobTypeImageImport:
			return p.ImportImage(ctx, job)
		case models.JobTypeVMSuspend:
			return p.SuspendVM(ctx, job)
		default:
			return fmt.Errorf("unknown job type: %s", job.Type)
		}
	})
}

func isTemplateJobType(jobType string) bool {
	switch jobType {
	case models.JobTypeTemplateProvision, models.JobTypeTemplateGeneralize,
		models.JobTypeTemplateVerify, models.JobTypeTemplateRevalidate,
		models.JobTypeTemplateHealthConfirm:
		return true
	default:
		return false
	}
}

func processJobLifecycle(
	ctx context.Context,
	db jobStatusUpdater,
	pipeline pipelineMetricsSink,
	job *models.Job,
	publish func(uuid.UUID, string, string),
	dispatch func(context.Context, *models.Job) error,
) error {
	// Mark in_progress before dispatching the job body.
	if err := db.UpdateJobStatus(ctx, job.ID, models.JobStatusInProgress, nil); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	if pipeline != nil && isTemplateJobType(job.Type) {
		started := time.Now()
		defer func() {
			pipeline.RecordTemplateJob(job.Type, time.Since(started))
		}()
	}
	if publish != nil {
		publish(job.ID, "started", "Job processing started")
	}

	err := dispatch(ctx, job)
	if err == nil {
		result, _ := json.Marshal(map[string]string{"message": "completed successfully"})
		_ = db.UpdateJobStatus(ctx, job.ID, models.JobStatusCompleted, result)
		if publish != nil {
			publish(job.ID, "completed", "Job completed successfully")
		}
		return nil
	}

	// Error path: retry if possible, otherwise fail terminally.
	retryable, reason := ClassifyError(err, job.Type)
	if retryable && job.RetryCount < job.MaxRetries {
		nextAt := time.Now().Add(RetryBackoff(job.RetryCount))
		if schedErr := db.RetryJob(ctx, job.ID, nextAt); schedErr == nil {
			if pipeline != nil {
				pipeline.RecordJobRetry(job.Type, reason)
			}
			if publish != nil {
				publish(job.ID, "retry_scheduled", fmt.Sprintf(
					"Retry %d/%d scheduled for %s (reason: %s)",
					job.RetryCount+1, job.MaxRetries,
					nextAt.Format(time.RFC3339), reason,
				))
			}
			return nil // rescheduled; not a failure from the caller's perspective
		}
		// RetryJob itself failed (DB problem) — fall through to terminal failure.
	}

	// Terminal failure.
	if retryable && job.RetryCount >= job.MaxRetries {
		if pipeline != nil {
			pipeline.RecordJobRetryExhausted(job.Type)
		}
	}

	friendly := FriendlyError(err, job.RetryCount, job.MaxRetries)
	type jobResult struct {
		Error    string `json:"error"`
		RawError string `json:"raw_error,omitempty"`
		Attempts int    `json:"attempts"`
	}
	jr := jobResult{
		Error:    friendly,
		Attempts: job.RetryCount + 1,
	}
	if friendly != err.Error() {
		jr.RawError = err.Error()
	}
	result, _ := json.Marshal(jr)
	_ = db.UpdateJobStatus(ctx, job.ID, models.JobStatusFailed, result)
	if publish != nil {
		publish(job.ID, "failed", friendly)
	}
	return err
}

func (p *Provisioner) publishProgress(jobID uuid.UUID, step, message string) {
	if p.nats != nil {
		_ = p.nats.PublishJobStatus(jobID, step, message)
	}
}

// acquireCloneLock serialises concurrent clones from the same source VM.
//
// Returns a release function that MUST be called (via defer) on every path.
//
// Pattern: a buffered channel of capacity 1 acts as a per-moref semaphore.
// LoadOrStore guarantees all callers for a given moref see the same channel
// regardless of order.  A send blocks until the token is available; the
// release function reads it back.
//
// The map is never cleaned up (leaks one chan per unique moref over the
// lifetime of the process), which is intentional: the number of source VMs
// is bounded and bounded-small, and channel garbage-collection would add
// complexity without benefit.
func (p *Provisioner) acquireCloneLock(sourceMoref string) func() {
	ch := make(chan struct{}, 1)
	actual, _ := p.cloneMu.LoadOrStore(sourceMoref, ch)
	token := actual.(chan struct{})
	token <- struct{}{}       // acquire (blocks if another goroutine holds it)
	return func() { <-token } // release
}

// newRollbackEngine creates a rollback engine for a job.
func (p *Provisioner) newRollbackEngine(jobID uuid.UUID) *rollback.Engine {
	return rollback.New(jobID, &dbPersister{db: p.db}, p.logger)
}

func (p *Provisioner) newPodCreateRollbackEngine(job *models.Job, vmSpecs []VMSpec) (*rollback.Engine, error) {
	rb := p.newRollbackEngine(job.ID)
	rb.RegisterUndo("vlan_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			UUID        string `json:"uuid"`
			Preexisting string `json:"preexisting"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if d.Preexisting == "true" {
			return nil
		}
		if err := p.opn.DeleteVLAN(ctx, d.UUID); err != nil {
			return err
		}
		return p.opn.ReconfigureVLANs(ctx)
	})
	rb.RegisterUndo("interface_assign", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			IfName  string `json:"if_name"`
			VLANTag int    `json:"vlan_tag"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		var removedIf string
		if d.VLANTag > 0 {
			var err error
			removedIf, err = p.opnSSH.UnassignInterfaceByVLAN(ctx, d.VLANTag)
			if err != nil {
				return err
			}
		} else {
			if err := p.opnSSH.UnassignInterface(ctx, d.IfName); err != nil {
				return err
			}
			removedIf = d.IfName
		}
		if removedIf != "" {
			_ = p.opn.RemoveDHCPInterface(ctx, removedIf)
		}
		return nil
	})
	rb.RegisterUndo("dhcp_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			UUID        string `json:"uuid"`
			Preexisting string `json:"preexisting"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if d.Preexisting == "true" {
			return nil
		}
		if err := p.opn.DeleteDHCPSubnet(ctx, d.UUID); err != nil {
			return err
		}
		return p.opn.ReconfigureDHCP(ctx)
	})
	rb.RegisterUndo("firewall_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if err := p.opn.DeleteFirewallRule(ctx, d.UUID); err != nil {
			return err
		}
		return p.opn.ApplyFirewall(ctx)
	})
	rb.RegisterUndo("portgroup_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			Name        string `json:"name"`
			Preexisting string `json:"preexisting"`
		}
		if err := json.Unmarshal(data, &d); err != nil {
			return err
		}
		if d.Preexisting == "true" {
			return nil
		}
		return p.vc.DeletePortGroupOnAllHosts(ctx, d.Name)
	})
	for i := range vmSpecs {
		stepName := fmt.Sprintf("vm_clone_%d", i)
		podVMID := vmSpecs[i].PodVMID
		rb.RegisterUndo(stepName, func(ctx context.Context, data json.RawMessage) error {
			var d struct{ Moref string }
			if err := json.Unmarshal(data, &d); err != nil {
				return err
			}
			_ = p.vc.PowerOffVM(ctx, d.Moref)
			if err := p.vc.DestroyVM(ctx, d.Moref); err != nil && !vcenterObjectNotFound(err) {
				return err
			}
			if _, err := p.db.ClearPodVMVCenterReference(ctx, podVMID, d.Moref); err != nil {
				return err
			}
			return nil
		})
	}
	if len(job.RollbackSteps) > 0 {
		var steps []rollback.Step
		if err := json.Unmarshal(job.RollbackSteps, &steps); err != nil {
			return nil, fmt.Errorf("parse persisted rollback steps: %w", err)
		}
		rb.LoadSteps(steps)
	}
	return rb, nil
}

// dbPersister implements rollback.Persister using the database.
type dbPersister struct {
	db *database.Queries
}

func (d *dbPersister) SaveRollbackSteps(ctx context.Context, jobID uuid.UUID, steps []rollback.Step) error {
	data, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	return d.db.UpdateJobRollbackSteps(ctx, jobID, data)
}

// ---------- Pod Creation ----------

// CreatePodPayload is the expected shape of job.Payload for pod_create.
type CreatePodPayload struct {
	PodID uuid.UUID `json:"pod_id"`
	VMs   []VMSpec  `json:"vms"`
}

// VMSpec describes a VM to create within a pod.
type VMSpec struct {
	PodVMID      uuid.UUID `json:"pod_vm_id"`
	TemplateName string    `json:"template_name"` // vCenter template name
	VMName       string    `json:"vm_name"`       // desired VM name
	VCPUs        int32     `json:"vcpus"`
	RAMMB        int64     `json:"ram_mb"`
	DiskGB       int       `json:"disk_gb"`
	OSType       string    `json:"os_type"` // "linux" or "windows"
	BootOrder    int       `json:"boot_order"`
	// Kind mirrors templates.kind. Empty string is treated as
	// "clone_with_customize" for backward compatibility with pre-T3 payloads.
	Kind string `json:"kind,omitempty"`
	// AssignIP defaults to true. When the API layer explicitly sends false
	// (template was registered with assign_ip=false), the provisioner attaches
	// the NIC but skips WaitForIP, leaving pod_vms.ip_address NULL.
	AssignIP bool `json:"assign_ip"`
}

func (p *Provisioner) stopPodCreateIfStale(
	ctx context.Context,
	podID uuid.UUID,
	vmSpecs []VMSpec,
	rb *rollback.Engine,
	stage string,
) (bool, error) {
	pod, err := p.db.GetPodByID(ctx, podID)
	if err != nil {
		return true, fmt.Errorf("recheck pod state after %s: %w", stage, err)
	}
	if pod == nil {
		rbErrs := rb.Rollback(ctx)
		p.logger.Warn("pod disappeared during create; rolled back completed resources",
			"pod_id", podID, "stage", stage, "rollback_errors", rbErrs)
		if len(rbErrs) > 0 {
			return true, fmt.Errorf("stale pod_create cleanup incomplete after %s: %v", stage, rbErrs)
		}
		return true, nil
	}
	if pod.Status == models.PodStatusProvisioning {
		return false, nil
	}
	if pod.Status == models.PodStatusActive {
		p.logger.Info("pod create already completed; skipping duplicate job",
			"pod_id", podID, "stage", stage)
		return true, nil
	}

	rbErrs := rb.Rollback(ctx)
	cleanupErrs := append([]error(nil), rbErrs...)
	if pod.Status == models.PodStatusDestroying ||
		pod.Status == models.PodStatusDestroyFailed ||
		pod.Status == models.PodStatusDestroyed {
		for _, vmSpec := range vmSpecs {
			if _, updateErr := p.db.UpdatePodVMStatusFrom(
				ctx,
				vmSpec.PodVMID,
				[]string{
					models.VMStatusPending,
					models.VMStatusCloning,
					models.VMStatusConfiguring,
					models.VMStatusRunning,
					"cloned",
					"failed",
				},
				models.VMStatusDeleted,
			); updateErr != nil {
				p.logger.Warn("failed to preserve deleted VM status while stopping stale pod create",
					"pod_id", podID, "pod_vm_id", vmSpec.PodVMID, "error", updateErr)
				cleanupErrs = append(cleanupErrs, fmt.Errorf("mark VM %s deleted: %w", vmSpec.PodVMID, updateErr))
			}
		}
	}
	p.logger.Warn("stopped stale pod create and rolled back completed resources",
		"pod_id", podID, "status", pod.Status, "stage", stage, "rollback_errors", cleanupErrs)
	if len(cleanupErrs) > 0 {
		return true, fmt.Errorf("stale pod_create cleanup incomplete after %s: %v", stage, cleanupErrs)
	}
	return true, nil
}

// generatePassword creates a random password with uppercase, lowercase, digits, and a special char.
func generatePassword(length int) string {
	const (
		upper   = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower   = "abcdefghjkmnpqrstuvwxyz"
		digits  = "23456789"
		special = "!@#$%&*"
	)

	// Guarantee at least one of each class
	pick := func(charset string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		return charset[n.Int64()]
	}

	buf := make([]byte, length)
	buf[0] = pick(upper)
	buf[1] = pick(lower)
	buf[2] = pick(digits)
	buf[3] = pick(special)

	all := upper + lower + digits
	for i := 4; i < length; i++ {
		buf[i] = pick(all)
	}

	// Shuffle (Fisher-Yates)
	for i := length - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		buf[i], buf[j.Int64()] = buf[j.Int64()], buf[i]
	}
	return string(buf)
}

// CreatePod executes the full pod creation workflow with rollback.
func (p *Provisioner) CreatePod(ctx context.Context, job *models.Job) error {
	var payload CreatePodPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse pod_create payload: %w", err)
	}
	rb, err := p.newPodCreateRollbackEngine(job, payload.VMs)
	if err != nil {
		return err
	}

	// Get pod from DB
	pod, err := p.db.GetPodByID(ctx, payload.PodID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}
	if pod == nil {
		rbErrs := rb.Rollback(ctx)
		if len(rbErrs) > 0 {
			return fmt.Errorf("pod %s not found; persisted cleanup incomplete: %v", payload.PodID, rbErrs)
		}
		return nil
	}

	// --- Step 1: Update pod status to provisioning ---
	p.publishProgress(job.ID, "pod_update", "Setting pod status to provisioning")
	applied, err := p.db.UpdatePodStatusFrom(
		ctx,
		pod.ID,
		[]string{models.PodStatusPending, models.PodStatusProvisioning},
		models.PodStatusProvisioning,
		"",
	)
	if err != nil {
		return fmt.Errorf("update pod status: %w", err)
	}
	if !applied {
		p.logger.Warn("stale pod create job skipped because pod is no longer provisionable",
			"pod_id", pod.ID, "status", pod.Status, "job_id", job.ID)
		_, cleanupErr := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "job_start")
		return cleanupErr
	}

	vlanTag := pod.VLANID
	octet := vlanTag - 100
	subnet := pod.Subnet
	gateway := fmt.Sprintf("10.100.%d.1/24", octet)
	pgName := fmt.Sprintf("Pod-VLAN%d", vlanTag)
	if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "admission"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// --- Step 2: Create VLAN on OPNsense ---
	p.publishProgress(job.ID, "vlan_create", fmt.Sprintf("Creating VLAN %d on OPNsense", vlanTag))

	// Idempotency: check if VLAN already exists
	existing, _ := p.opn.GetVLANByTag(ctx, vlanTag)
	var vlanUUID string
	vlanPreexisting := false
	if existing != nil {
		vlanUUID = existing.UUID
		vlanPreexisting = true
		p.logger.Info("VLAN already exists", "tag", vlanTag, "uuid", vlanUUID)
	} else {
		vlanUUID, err = p.opn.CreateVLAN(ctx, "vmx1", vlanTag, fmt.Sprintf("Pod-VLAN%d", vlanTag))
		if err != nil {
			return fmt.Errorf("create VLAN: %w", err)
		}
	}

	preexStr := "false"
	if vlanPreexisting {
		preexStr = "true"
	}
	if err := rb.Record(ctx, "vlan_create", map[string]string{"uuid": vlanUUID, "preexisting": preexStr}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "vlan_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	if err := p.opn.ReconfigureVLANs(ctx); err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("reconfigure VLANs (rollback errors: %v): %w", rbErrs, err)
	}

	// --- Step 3: Assign OPNsense interface via SSH ---
	p.publishProgress(job.ID, "interface_assign", "Assigning OPNsense interface via SSH")

	ifName, err := p.opnSSH.AssignInterface(ctx, vlanTag, gateway)
	if err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("assign interface (rollback errors: %v): %w", rbErrs, err)
	}

	if err := rb.Record(ctx, "interface_assign", map[string]interface{}{"if_name": ifName, "vlan_tag": vlanTag}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "interface_assign"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// --- Step 4: Create DHCP subnet ---
	p.publishProgress(job.ID, "dhcp_create", fmt.Sprintf("Creating DHCP subnet %s", subnet))

	existingDHCP, _ := p.opn.GetDHCPSubnetByNetwork(ctx, subnet)
	var dhcpUUID string
	dhcpPreexisting := false
	if existingDHCP != nil {
		dhcpUUID = existingDHCP.UUID
		dhcpPreexisting = true
		p.logger.Info("DHCP subnet already exists", "subnet", subnet, "uuid", dhcpUUID)
	} else {
		poolRange := fmt.Sprintf("10.100.%d.10-10.100.%d.250", octet, octet)
		dhcpUUID, err = p.opn.CreateDHCPSubnet(ctx, subnet, poolRange, gateway)
		if err != nil {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("create DHCP (rollback errors: %v): %w", rbErrs, err)
		}
	}

	dhcpPreexStr := "false"
	if dhcpPreexisting {
		dhcpPreexStr = "true"
	}
	if err := rb.Record(ctx, "dhcp_create", map[string]string{"uuid": dhcpUUID, "preexisting": dhcpPreexStr}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "dhcp_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// Add the OPNsense interface to Kea's listened interfaces BEFORE reconfigure.
	// ReconfigureDHCP regenerates config and restarts Kea, so the interface must
	// be in the config before that happens.
	if err := p.opn.AddDHCPInterface(ctx, ifName); err != nil {
		p.logger.Warn("failed to add DHCP interface", "interface", ifName, "error", err)
	}

	// Wait for interface to fully stabilize before restarting Kea.
	// The VLAN interface needs time after interface_configure() to be kernel-ready.
	time.Sleep(2 * time.Second)

	if err := p.opn.ReconfigureDHCP(ctx); err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("reconfigure DHCP (rollback errors: %v): %w", rbErrs, err)
	}

	// --- Step 4b: Create firewall rule to allow pod traffic ---
	p.publishProgress(job.ID, "firewall_create", "Creating firewall rule for pod network")

	fwRuleUUID, fwRuleCreated, err := ensurePodFirewallRule(
		ctx,
		p.opn,
		int(vlanTag),
		ifName,
		subnet,
		defaultMaxFirewallRules,
		defaultFirewallCleanupLimit,
	)
	if fwRuleCreated {
		if err := rb.Record(ctx, "firewall_create", map[string]string{"uuid": fwRuleUUID}); err != nil {
			return err
		}
		if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "firewall_create"); err != nil {
			return err
		} else if stopped {
			return nil
		}
	}
	if err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("ensure firewall rule (rollback errors: %v): %w", rbErrs, err)
	}

	// --- Step 5: Create port groups on all ESXi hosts ---
	p.publishProgress(job.ID, "portgroup_create", fmt.Sprintf("Creating port group %s on all hosts", pgName))

	if err := p.vc.CreatePortGroupOnAllHosts(ctx, pgName, vlanTag); err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("create port groups (rollback errors: %v): %w", rbErrs, err)
	}

	// Port groups are always idempotent (already-exists is ignored), so treat as preexisting
	// if the VLAN was preexisting (they go together)
	pgPreexStr := "false"
	if vlanPreexisting {
		pgPreexStr = "true"
	}
	if err := rb.Record(ctx, "portgroup_create", map[string]string{"name": pgName, "preexisting": pgPreexStr}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "portgroup_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// --- Step 6: Clone VMs ---
	var clonedVMs []int // indices of successfully cloned VMs
	for i, vmSpec := range payload.VMs {
		stepName := fmt.Sprintf("vm_clone_%d", i)
		p.publishProgress(job.ID, stepName, fmt.Sprintf("Cloning VM %s from %s", vmSpec.VMName, vmSpec.TemplateName))

		// Load the template once so we can branch on kind + reuse default
		// credentials for the no-customize / registered-existing paths. We
		// also reuse the loaded podVM row for the resume-clone idempotency
		// check below.
		var tmpl *models.Template
		var podVMRow *models.PodVM
		podVMRow, err = p.db.GetPodVM(ctx, vmSpec.PodVMID)
		if err != nil {
			return fmt.Errorf("get pod VM %s before clone: %w", vmSpec.PodVMID, err)
		}
		if podVMRow.Status == models.VMStatusDeleted || podVMRow.Status == models.VMStatusError {
			moref := ""
			if podVMRow.VCenterVMID != nil {
				moref = *podVMRow.VCenterVMID
			}
			if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, moref); cleanupErr != nil {
				return cleanupErr
			}
			p.logger.Warn("skipping stale pod-create VM in terminal state",
				"pod_id", pod.ID, "pod_vm_id", vmSpec.PodVMID, "vm_status", podVMRow.Status)
			continue
		}
		if t, tmplErr := p.db.GetTemplateByID(ctx, podVMRow.TemplateID); tmplErr == nil {
			tmpl = t
		}

		osType := vmSpec.OSType
		if osType == "" && tmpl != nil {
			osType = tmpl.OSType
		}

		kind := resolveTemplateKind(vmSpec.Kind)
		if kind != vmSpec.Kind && vmSpec.Kind != "" {
			p.logger.Warn("unknown template kind, defaulting to clone_with_customize",
				"vm", vmSpec.VMName, "kind", vmSpec.Kind)
		}

		// Branch on kind:
		//   * clone_with_customize  — generate a fresh password and inject
		//     it via guestinfo. The clone path runs sysprep / cloud-init.
		//   * clone_no_customize    — clone the source (linked, like today)
		//     but skip credential injection. Use the template's static
		//     default_username / default_password if present.
		//   * registered_existing_vm — same code path as clone_no_customize;
		//     the source VM is treated as the canonical golden image, and
		//     CloneVM already does a linked clone off its current snapshot.
		var generatedPassword string
		if shouldGenerateGuestPassword(kind, osType) {
			generatedPassword = generatePassword(12)
		}

		// Resume support: if a prior worker already cloned this VM (job was
		// recovered after a worker restart via RecoverStaleJobs), the
		// pod_vms row will have vcenter_vm_id set. Re-cloning with the
		// same name fails with "already exists", leaks the cloned VM, and
		// marks the pod failed. Reuse the existing clone instead. Mirrors
		// AddVM's resume path in vm_ops.go.
		var moref string
		if podVMRow != nil && podVMRow.VCenterVMID != nil && *podVMRow.VCenterVMID != "" {
			moref = *podVMRow.VCenterVMID
			p.logger.Info("resuming pod create — VM already cloned",
				"vm", vmSpec.VMName, "moref", moref, "pod_vm_id", vmSpec.PodVMID)
		} else {
			applied, updateErr := p.db.UpdatePodVMStatusFrom(
				ctx,
				vmSpec.PodVMID,
				[]string{models.VMStatusPending, models.VMStatusCloning, models.VMStatusConfiguring},
				models.VMStatusCloning,
			)
			if updateErr != nil {
				return fmt.Errorf("claim pod VM %s for clone: %w", vmSpec.PodVMID, updateErr)
			}
			if !applied {
				current, lookupErr := p.db.GetPodVM(ctx, vmSpec.PodVMID)
				if lookupErr != nil {
					return fmt.Errorf("reload pod VM %s after clone state changed: %w", vmSpec.PodVMID, lookupErr)
				}
				currentMoref := ""
				if current.VCenterVMID != nil {
					currentMoref = *current.VCenterVMID
				}
				if current.Status == models.VMStatusDeleted || current.Status == models.VMStatusError {
					if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, currentMoref); cleanupErr != nil {
						return cleanupErr
					}
				}
				p.logger.Warn("skipping pod-create VM after losing provisioning state",
					"pod_id", pod.ID, "pod_vm_id", vmSpec.PodVMID, "vm_status", current.Status)
				continue
			}

			var cloneErr error
			moref, cloneErr = p.vc.CloneVM(ctx, vcenter.CloneVMParams{
				TemplateName: vmSpec.TemplateName,
				VMName:       vmSpec.VMName,
				VCPUs:        vmSpec.VCPUs,
				RAMmb:        vmSpec.RAMMB,
				Network:      pgName,
				OSType:       osType,
				Password:     generatedPassword,
			})
			if cloneErr != nil {
				p.logger.Error("failed to clone VM", "vm", vmSpec.VMName, "error", cloneErr)
				_, _ = p.db.UpdatePodVMStatusFrom(
					ctx,
					vmSpec.PodVMID,
					[]string{models.VMStatusCloning},
					models.VMStatusError,
				)
				continue // Skip this VM, try the rest
			}

			// Persist the clone only while this job still owns the VM row. A
			// concurrent delete wins; its clone is removed or handed to a
			// durable vm_destroy cleanup job.
			applied, updateErr = p.db.UpdatePodVMFrom(
				ctx,
				vmSpec.PodVMID,
				[]string{models.VMStatusCloning},
				moref,
				vmSpec.VMName,
				"cloned",
			)
			if updateErr != nil {
				if current, lookupErr := p.db.GetPodVM(ctx, vmSpec.PodVMID); lookupErr == nil &&
					podVMHasLiveClone(current, moref) {
					applied = true
				} else {
					if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, moref); cleanupErr != nil {
						return fmt.Errorf("record cloned VM: %v; cleanup: %w", updateErr, cleanupErr)
					}
					return fmt.Errorf("record cloned VM after compensating cleanup: %w", updateErr)
				}
			}
			if !applied {
				current, lookupErr := p.db.GetPodVM(ctx, vmSpec.PodVMID)
				if lookupErr == nil && podVMHasLiveClone(current, moref) {
					applied = true
				}
			}
			if !applied {
				if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, moref); cleanupErr != nil {
					return cleanupErr
				}
				continue
			}
		}

		// Resolve credentials to record on the pod_vms row. Pure helper —
		// see kind_helpers.go for the policy + unit tests.
		storedUsername, storedPassword := resolvePodVMCredentials(kind, osType, generatedPassword, tmpl)
		_ = p.db.UpdatePodVMCredentials(ctx, vmSpec.PodVMID, storedUsername, storedPassword)

		if err := rb.Record(ctx, stepName, map[string]string{"moref": moref}); err != nil {
			return err
		}
		if stopped, err := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, stepName); err != nil {
			return err
		} else if stopped {
			return nil
		}
		clonedVMs = append(clonedVMs, i)
	}

	// If no VMs were cloned at all, rollback infrastructure
	if len(clonedVMs) == 0 {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("all VM clones failed (rollback errors: %v)", rbErrs)
	}

	// --- Step 7: Power on VMs by boot order and wait for IPs per group ---
	p.publishProgress(job.ID, "vm_poweron", "Powering on VMs")

	type vmPowerInfo struct {
		index  int
		vmSpec VMSpec
		moref  string
	}

	// Group cloned VMs by boot order
	bootGroups := make(map[int][]int) // boot_order -> clonedVM indices
	for _, i := range clonedVMs {
		bo := payload.VMs[i].BootOrder
		bootGroups[bo] = append(bootGroups[bo], i)
	}
	var bootOrders []int
	for bo := range bootGroups {
		bootOrders = append(bootOrders, bo)
	}
	sort.Ints(bootOrders)

	// Power on each boot-order group sequentially; VMs within a group start in parallel
	var toPowerOn []vmPowerInfo
	for _, bo := range bootOrders {
		var groupPoweredOn []vmPowerInfo
		for _, i := range bootGroups[bo] {
			vmSpec := payload.VMs[i]
			podVM, err := p.db.GetPodVM(ctx, vmSpec.PodVMID)
			if err != nil {
				p.logger.Error("failed to get pod VM for power-on", "vm", vmSpec.VMName, "error", err)
				continue
			}
			if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
				p.logger.Error("pod VM has no moref", "vm", vmSpec.VMName)
				_, _ = p.db.UpdatePodVMStatusFrom(
					ctx,
					vmSpec.PodVMID,
					[]string{"cloned", models.VMStatusCloning, models.VMStatusConfiguring},
					models.VMStatusError,
				)
				continue
			}
			if podVM.Status == models.VMStatusRunning {
				groupPoweredOn = append(groupPoweredOn, vmPowerInfo{index: i, vmSpec: vmSpec, moref: *podVM.VCenterVMID})
				continue
			}
			if podVM.Status == models.VMStatusDeleted || podVM.Status == models.VMStatusError {
				if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, *podVM.VCenterVMID); cleanupErr != nil {
					return cleanupErr
				}
				continue
			}
			applied, err = p.db.UpdatePodVMStatusFrom(
				ctx,
				vmSpec.PodVMID,
				[]string{"cloned", models.VMStatusCloning, models.VMStatusConfiguring},
				models.VMStatusConfiguring,
			)
			if err != nil {
				return fmt.Errorf("claim pod VM %s for power-on: %w", vmSpec.PodVMID, err)
			}
			if !applied {
				current, lookupErr := p.db.GetPodVM(ctx, vmSpec.PodVMID)
				if lookupErr != nil {
					return fmt.Errorf("reload pod VM %s after power-on state changed: %w", vmSpec.PodVMID, lookupErr)
				}
				if current.Status == models.VMStatusRunning &&
					current.VCenterVMID != nil && *current.VCenterVMID == *podVM.VCenterVMID {
					groupPoweredOn = append(groupPoweredOn, vmPowerInfo{index: i, vmSpec: vmSpec, moref: *podVM.VCenterVMID})
					continue
				}
				if current.Status == models.VMStatusDeleted || current.Status == models.VMStatusError {
					if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, *podVM.VCenterVMID); cleanupErr != nil {
						return cleanupErr
					}
				}
				continue
			}

			if err := p.vc.PowerOnVM(ctx, *podVM.VCenterVMID); err != nil {
				p.logger.Error("failed to power on VM", "vm", vmSpec.VMName, "error", err)
				_, _ = p.db.UpdatePodVMStatusFrom(
					ctx,
					vmSpec.PodVMID,
					[]string{models.VMStatusConfiguring},
					models.VMStatusError,
				)
				continue
			}

			applied, err = p.db.UpdatePodVMStatusFrom(
				ctx,
				vmSpec.PodVMID,
				[]string{models.VMStatusConfiguring},
				models.VMStatusRunning,
			)
			if err != nil {
				return fmt.Errorf("mark pod VM %s running: %w", vmSpec.PodVMID, err)
			}
			if !applied {
				if cleanupErr := p.cleanupStaleVMClone(ctx, pod.ID, vmSpec.PodVMID, vmSpec.VMName, *podVM.VCenterVMID); cleanupErr != nil {
					return cleanupErr
				}
				continue
			}
			groupPoweredOn = append(groupPoweredOn, vmPowerInfo{index: i, vmSpec: vmSpec, moref: *podVM.VCenterVMID})
		}

		// Wait for IPs in this boot-order group before starting the next group.
		// Skip the wait for any VM whose template was registered with
		// assign_ip=false — its network is owner-managed (DHCP/static inside
		// the guest), so the provisioner has no IP to record.
		var wg sync.WaitGroup
		for _, info := range groupPoweredOn {
			if !info.vmSpec.AssignIP {
				p.logger.Info("skipping WaitForIP (template assign_ip=false)", "vm", info.vmSpec.VMName)
				continue
			}
			wg.Add(1)
			go func(vmInfo vmPowerInfo) {
				defer wg.Done()
				ip, err := p.vc.WaitForIP(ctx, vmInfo.moref, 5*time.Minute)
				if err != nil {
					p.logger.Warn("timeout waiting for VM IP", "vm", vmInfo.vmSpec.VMName, "error", err)
					return
				}
				_ = p.db.UpdatePodVMIP(ctx, vmInfo.vmSpec.PodVMID, ip)
				p.logger.Info("VM got IP", "vm", vmInfo.vmSpec.VMName, "ip", ip)
			}(info)
		}
		wg.Wait()

		toPowerOn = append(toPowerOn, groupPoweredOn...)
	}

	// --- Step 7b: Take initial snapshots for restore-to-original (non-fatal) ---
	for _, info := range toPowerOn {
		snapMoref, snapErr := p.vc.CreateVMSnapshot(ctx, info.moref, "initial", "Auto-created at provisioning")
		if snapErr != nil {
			p.logger.Warn("failed to create initial snapshot (non-fatal)", "vm", info.vmSpec.VMName, "error", snapErr)
			continue
		}
		snap := &models.VMSnapshot{
			PodVMID:           info.vmSpec.PodVMID,
			Name:              "initial",
			Description:       "Original state at provisioning",
			VCenterSnapshotID: snapMoref,
			IsInitial:         true,
		}
		if dbErr := p.db.CreateVMSnapshot(ctx, snap); dbErr != nil {
			p.logger.Warn("failed to record initial snapshot in DB", "vm", info.vmSpec.VMName, "error", dbErr)
		}
	}

	// --- Step 8: Mark pod active ---
	//
	// Compare-and-swap on "provisioning" rather than an unconditional write. A destroy job
	// runs independently of this one and can complete while we are still working -- vCenter
	// can take minutes to report a VM's IP, and the destroy tears the VMs down in that
	// window. Writing "active" unconditionally meant the slow create won simply by finishing
	// last, resurrecting the pod as active with no VM behind it. Nothing detects that: the
	// API, the UI and the quota accounting all trust this column.
	p.publishProgress(job.ID, "pod_active", "Pod is active")
	applied, err = p.db.UpdatePodStatusFrom(ctx, pod.ID, []string{"provisioning"}, "active", "")
	if err != nil {
		return fmt.Errorf("update pod to active: %w", err)
	}
	if !applied {
		_, stopErr := p.stopPodCreateIfStale(ctx, pod.ID, payload.VMs, rb, "activate")
		return stopErr
	}

	p.logger.Info("pod created successfully", "pod_id", pod.ID, "vlan", vlanTag)
	return nil
}
