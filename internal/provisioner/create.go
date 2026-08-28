package provisioner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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
	UpdateJobStatus(ctx context.Context, id uuid.UUID, workerID, status string, result []byte) error
	RetryJob(
		ctx context.Context,
		id uuid.UUID,
		nextAt time.Time,
		cleanupOnly bool,
		cleanupTarget []byte,
		workerID string,
	) error
}

type templateProvisionJobStatusUpdater interface {
	RetryTemplateProvisionJob(
		ctx context.Context,
		id uuid.UUID,
		nextAt time.Time,
		result []byte,
		workerID string,
	) error
	FailTemplateProvisionJob(
		ctx context.Context,
		id uuid.UUID,
		workerID string,
		result []byte,
	) (bool, error)
}

const (
	cleanupRescheduleWriteTimeout = 10 * time.Second
	cleanupRescheduleBackoffBase  = time.Second
	cleanupRescheduleBackoffMax   = 30 * time.Second
)

var _ pipelineMetricsSink = (*PipelineMetrics)(nil)
var _ jobStatusUpdater = (*database.Queries)(nil)
var _ templateProvisionJobStatusUpdater = (*database.Queries)(nil)

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
	workerID, claimedAt, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	leaseCtx, cancelLease := context.WithCancelCause(ctx)
	leaseDone := make(chan struct{})
	go func() {
		defer close(leaseDone)
		maintainJobLease(
			leaseCtx,
			p.db,
			job.ID,
			workerID,
			claimedAt,
			JobLeaseHeartbeatInterval,
			JobLeaseDuration,
			jobLeaseWriteTimeout,
			cancelLease,
		)
	}()

	result := processJobLifecycle(leaseCtx, p.db, p.pipeline, job, p.publishProgress, func(ctx context.Context, job *models.Job) error {
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
		case models.JobTypeTemplateReplicaBuild:
			return p.BuildTemplateSourceReplica(ctx, job)
		case models.JobTypeImageImport:
			return p.ImportImage(ctx, job)
		case models.JobTypeVMSuspend:
			return p.SuspendVM(ctx, job)
		default:
			return fmt.Errorf("unknown job type: %s", job.Type)
		}
	})
	cancelLease(errJobLeaseFinished)
	<-leaseDone
	return result
}

func isTemplateJobType(jobType string) bool {
	switch jobType {
	case models.JobTypeTemplateProvision, models.JobTypeTemplateGeneralize,
		models.JobTypeTemplateVerify, models.JobTypeTemplateRevalidate,
		models.JobTypeTemplateHealthConfirm, models.JobTypeTemplateReplicaBuild:
		return true
	default:
		return false
	}
}

func cleanupRescheduleBackoff(failures int) time.Duration {
	delay := cleanupRescheduleBackoffBase
	for i := 1; i < failures && delay < cleanupRescheduleBackoffMax; i++ {
		if delay > cleanupRescheduleBackoffMax/2 {
			return cleanupRescheduleBackoffMax
		}
		delay *= 2
	}
	if delay > cleanupRescheduleBackoffMax {
		return cleanupRescheduleBackoffMax
	}
	return delay
}

func persistCleanupRetry(
	ctx context.Context,
	db jobStatusUpdater,
	job *models.Job,
	workerID string,
	cleanupTarget []byte,
) (time.Time, error) {
	failures := 0
	for {
		if failures > 0 {
			timer := time.NewTimer(cleanupRescheduleBackoff(failures))
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return time.Time{}, fmt.Errorf("worker stopped before cleanup retry was persisted: %w", ctx.Err())
			case <-timer.C:
			}
		}

		nextAt := time.Now().Add(RetryBackoff(job.RetryCount))
		writeCtx, cancel := context.WithTimeout(ctx, cleanupRescheduleWriteTimeout)
		err := db.RetryJob(writeCtx, job.ID, nextAt, true, cleanupTarget, workerID)
		cancel()
		if err == nil {
			return nextAt, nil
		}
		failures++
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
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	// Mark in_progress before dispatching the job body.
	if err := db.UpdateJobStatus(ctx, job.ID, workerID, models.JobStatusInProgress, nil); err != nil {
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

	err = classifyPlacementValidationFailure(dispatch(ctx, job))
	if err == nil {
		result, _ := json.Marshal(map[string]string{"message": "completed successfully"})
		if statusErr := db.UpdateJobStatus(ctx, job.ID, workerID, models.JobStatusCompleted, result); statusErr != nil {
			return fmt.Errorf("persist completed job status: %w", statusErr)
		}
		if publish != nil {
			publish(job.ID, "completed", "Job completed successfully")
		}
		return nil
	}

	// Error path: retry if possible, otherwise fail terminally.
	retryable, reason := ClassifyError(err, job.Type)
	cleanupOnly := jobPayloadCleanupOnly(job) || isCompensationRetry(err)
	if cleanupOnly &&
		!errors.Is(err, database.ErrTemplateReplicaBuildJobObsolete) &&
		!isCompensatedJobError(err) && !isManualCleanupRequired(err) {
		retryable = true
		reason = RetryReasonCleanup
	}
	if retryable && (cleanupOnly || job.RetryCount < job.MaxRetries) {
		var (
			nextAt   time.Time
			schedErr error
		)
		cleanupTarget := compensationRetryTarget(err)
		if cleanupOnly {
			nextAt, schedErr = persistCleanupRetry(ctx, db, job, workerID, cleanupTarget)
		} else if job.Type == models.JobTypeTemplateProvision {
			nextAt = time.Now().Add(RetryBackoff(job.RetryCount))
			result := marshalTemplateProvisionFailure(job.Result, err, job.RetryCount, job.MaxRetries)
			templateDB, ok := db.(templateProvisionJobStatusUpdater)
			if !ok {
				return fmt.Errorf("job store does not support template provision retry persistence")
			}
			schedErr = templateDB.RetryTemplateProvisionJob(ctx, job.ID, nextAt, result, workerID)
		} else {
			nextAt = time.Now().Add(RetryBackoff(job.RetryCount))
			schedErr = db.RetryJob(ctx, job.ID, nextAt, false, cleanupTarget, workerID)
		}
		if schedErr == nil {
			if pipeline != nil {
				pipeline.RecordJobRetry(job.Type, reason)
			}
			if publish != nil {
				message := fmt.Sprintf(
					"Retry %d/%d scheduled for %s (reason: %s)",
					job.RetryCount+1, job.MaxRetries, nextAt.Format(time.RFC3339), reason,
				)
				if cleanupOnly {
					message = fmt.Sprintf(
						"Cleanup retry %d scheduled for %s; it will remain pending until resolved",
						job.RetryCount+1, nextAt.Format(time.RFC3339),
					)
				}
				publish(job.ID, "retry_scheduled", message)
			}
			return nil // rescheduled; not a failure from the caller's perspective
		}
		if cleanupOnly {
			// Shutdown is the only exit from durable cleanup rescheduling.
			// Startup recovery hands this in-progress row to the next worker.
			return fmt.Errorf("reschedule durable cleanup: %w", schedErr)
		}
		if job.Type == models.JobTypeTemplateProvision {
			return fmt.Errorf("persist template provision retry: %w", schedErr)
		}
		// RetryJob itself failed (DB problem) — fall through to terminal failure.
	}

	// Terminal failure.
	retryExhausted := retryable && !cleanupOnly && job.RetryCount >= job.MaxRetries
	if retryExhausted && job.Type != models.JobTypeTemplateProvision {
		if pipeline != nil {
			pipeline.RecordJobRetryExhausted(job.Type)
		}
	}

	type jobResult struct {
		Error                 string `json:"error"`
		RawError              string `json:"raw_error,omitempty"`
		Attempts              int    `json:"attempts"`
		Compensated           bool   `json:"compensated,omitempty"`
		ManualCleanupRequired bool   `json:"manual_cleanup_required,omitempty"`
	}
	friendly := FriendlyError(err, job.RetryCount, job.MaxRetries)
	jr := jobResult{
		Error:                 friendly,
		Attempts:              job.RetryCount + 1,
		Compensated:           isCompensatedJobError(err),
		ManualCleanupRequired: isManualCleanupRequired(err),
	}
	if friendly != err.Error() {
		jr.RawError = err.Error()
	}
	result, _ := json.Marshal(jr)
	if job.Type == models.JobTypeTemplateProvision {
		result = marshalTemplateProvisionFailure(job.Result, err, job.RetryCount, job.MaxRetries)
		templateDB, ok := db.(templateProvisionJobStatusUpdater)
		if !ok {
			return fmt.Errorf("job store does not support terminal template provision persistence")
		}
		transitioned, statusErr := templateDB.FailTemplateProvisionJob(ctx, job.ID, workerID, result)
		if statusErr != nil {
			return fmt.Errorf("persist terminal template provision failure: %w", statusErr)
		}
		if transitioned && pipeline != nil {
			pipeline.RecordTemplateTransition(models.TemplateStateProvisioning, models.TemplateStateError)
		}
		if retryExhausted && pipeline != nil {
			pipeline.RecordJobRetryExhausted(job.Type)
		}
	} else if statusErr := db.UpdateJobStatus(ctx, job.ID, workerID, models.JobStatusFailed, result); statusErr != nil {
		return fmt.Errorf("persist terminal job status: %w", statusErr)
	}
	if publish != nil {
		event := "failed"
		if jr.Compensated {
			event = "compensated"
		} else if jr.ManualCleanupRequired {
			event = "manual_cleanup_required"
		}
		publish(job.ID, event, friendly)
	}
	return err
}

type templateProvisionFailureResult struct {
	Error           string `json:"error"`
	RawError        string `json:"raw_error,omitempty"`
	Attempts        int    `json:"attempts"`
	FirstError      string `json:"first_error"`
	FirstRawError   string `json:"first_raw_error,omitempty"`
	FirstAttempt    int    `json:"first_attempt"`
	CurrentError    string `json:"current_error"`
	CurrentRawError string `json:"current_raw_error,omitempty"`
	CurrentAttempt  int    `json:"current_attempt"`
}

func marshalTemplateProvisionFailure(previous []byte, cause error, retryCount, maxRetries int) []byte {
	currentError := FriendlyError(cause, retryCount, maxRetries)
	currentRawError := ""
	if cause != nil && currentError != cause.Error() {
		currentRawError = cause.Error()
	}
	attempt := retryCount + 1
	result := templateProvisionFailureResult{
		Error:           currentError,
		RawError:        currentRawError,
		Attempts:        attempt,
		FirstError:      currentError,
		FirstRawError:   currentRawError,
		FirstAttempt:    attempt,
		CurrentError:    currentError,
		CurrentRawError: currentRawError,
		CurrentAttempt:  attempt,
	}

	var prior templateProvisionFailureResult
	if len(previous) > 0 && json.Unmarshal(previous, &prior) == nil {
		switch {
		case prior.FirstError != "":
			result.FirstError = prior.FirstError
			result.FirstRawError = prior.FirstRawError
			result.FirstAttempt = prior.FirstAttempt
		case prior.Error != "":
			result.FirstError = prior.Error
			result.FirstRawError = prior.RawError
			result.FirstAttempt = prior.Attempts
		}
	}
	if result.FirstAttempt < 1 {
		result.FirstAttempt = 1
	}
	encoded, _ := json.Marshal(result)
	return encoded
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
func (p *Provisioner) newRollbackEngine(job *models.Job) (*rollback.Engine, error) {
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return nil, err
	}
	return rollback.New(job.ID, &dbPersister{db: p.db, workerID: workerID}, p.logger), nil
}

func (p *Provisioner) newPodCreateRollbackEngine(
	job *models.Job,
	podID uuid.UUID,
	vmSpecs []VMSpec,
) (*rollback.Engine, error) {
	rb, err := p.newRollbackEngine(job)
	if err != nil {
		return nil, err
	}
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
		record, err := p.db.GetPodPortGroupReceipt(ctx, podID)
		if err != nil {
			return err
		}
		var receipt vcenter.PortGroupReceipt
		if err := json.Unmarshal(record.Receipt, &receipt); err != nil {
			return err
		}
		switch record.State {
		case database.PodPortGroupReceiptPlanned:
			return p.db.MarkPodPortGroupRemoved(ctx, podID, record.Receipt)
		case database.PodPortGroupReceiptRemoved:
			return nil
		case database.PodPortGroupReceiptApplying, database.PodPortGroupReceiptActive:
		default:
			return fmt.Errorf("invalid durable port group receipt state %q", record.State)
		}
		receipt, err = vcenter.PortGroupReceiptWithKeys(receipt, record.Keys)
		if err != nil {
			return err
		}
		keys, err := p.vc.CapturePortGroupKeys(ctx, receipt)
		if err != nil {
			return err
		}
		if err := p.db.PersistPodPortGroupKeys(ctx, podID, record.Receipt, keys); err != nil {
			return err
		}
		for host, key := range record.Keys {
			if _, exists := keys[host]; !exists {
				keys[host] = key
			}
		}
		receipt, err = vcenter.PortGroupReceiptWithKeys(receipt, keys)
		if err != nil {
			return err
		}
		if err := p.db.WithVCenterPortGroupMutationLock(ctx, func(lockCtx context.Context) error {
			return p.vc.DeletePortGroupMutation(lockCtx, receipt)
		}); err != nil {
			return err
		}
		return p.db.MarkPodPortGroupRemoved(ctx, podID, record.Receipt)
	})
	for i := range vmSpecs {
		stepName := fmt.Sprintf("vm_clone_%d", i)
		podVMID := vmSpecs[i].PodVMID
		rb.RegisterUndo(stepName, func(ctx context.Context, data json.RawMessage) error {
			var target VMCloneCleanupTarget
			if err := json.Unmarshal(data, &target); err != nil {
				return err
			}
			if target.VCenterVMID == "" {
				var legacy struct {
					Moref string `json:"moref"`
				}
				if err := json.Unmarshal(data, &legacy); err != nil {
					return err
				}
				target = VMCloneCleanupTarget{
					PodID:       podID.String(),
					PodVMID:     podVMID.String(),
					VCenterVMID: legacy.Moref,
				}
			}
			resolved, err := p.resolvePodCloneCleanupIdentity(ctx, podID, podVMID, &target)
			if err != nil {
				return err
			}
			if err := p.destroyExactCloneTarget(ctx, resolved); err != nil {
				return err
			}
			if _, err := p.db.ClearPodVMVCenterReference(ctx, podVMID, resolved.VCenterVMID); err != nil {
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
	db       *database.Queries
	workerID string
}

func (d *dbPersister) SaveRollbackSteps(ctx context.Context, jobID uuid.UUID, steps []rollback.Step) error {
	data, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	err = d.db.UpdateJobRollbackSteps(ctx, jobID, d.workerID, data)
	if errors.Is(err, database.ErrJobLeaseLost) {
		return fmt.Errorf("%w: %v", rollback.ErrOwnershipLost, err)
	}
	return err
}

func (d *dbPersister) HandoffRollbackStep(ctx context.Context, jobID uuid.UUID, step rollback.Step) error {
	data, err := json.Marshal(step)
	if err != nil {
		return err
	}
	return d.db.AdoptJobRollbackStep(ctx, jobID, data)
}

// ---------- Pod Creation ----------

// CreatePodPayload is the expected shape of job.Payload for pod_create.
type CreatePodPayload struct {
	PodID          uuid.UUID                `json:"pod_id"`
	VMs            []VMSpec                 `json:"vms"`
	CleanupOnly    bool                     `json:"cleanup_only,omitempty"`
	CleanupTarget  *VMCloneCleanupTarget    `json:"cleanup_target,omitempty"`
	CleanupDone    bool                     `json:"cleanup_completed,omitempty"`
	CloneOperation *models.VMCloneOperation `json:"clone_operation,omitempty"`
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

type podCreateCleanupRetryError struct {
	err error
}

type compensatedJobError struct {
	err error
}

type compensationRetryError struct {
	err    error
	target *VMCloneCleanupTarget
}

type manualCleanupRequiredError struct {
	err error
}

func (e *compensatedJobError) Error() string {
	return e.err.Error()
}

func (e *compensatedJobError) Unwrap() error {
	return e.err
}

func (e *compensationRetryError) Error() string {
	return e.err.Error()
}

func (e *compensationRetryError) Unwrap() error {
	return e.err
}

func (e *manualCleanupRequiredError) Error() string {
	return e.err.Error()
}

func (e *manualCleanupRequiredError) Unwrap() error {
	return e.err
}

func (e *podCreateCleanupRetryError) Error() string {
	return e.err.Error()
}

func (e *podCreateCleanupRetryError) Unwrap() error {
	return e.err
}

func newPodCreateCleanupRetryError(stage string, cleanupErrs []error) error {
	joined := errors.Join(cleanupErrs...)
	if joined == nil {
		joined = errors.New("cleanup failed without a reported cause")
	}
	return &podCreateCleanupRetryError{
		err: fmt.Errorf("stale pod_create cleanup incomplete after %s: %w", stage, joined),
	}
}

func combineProvisioningAndCleanupErrors(cause, cleanupErr error) error {
	return fmt.Errorf("%w; cleanup: %w", cause, cleanupErr)
}

func isPodCreateCleanupRetry(err error) bool {
	var cleanupErr *podCreateCleanupRetryError
	return errors.As(err, &cleanupErr)
}

func isCompensationRetry(err error) bool {
	var retryErr *compensationRetryError
	return errors.As(err, &retryErr) || isPodCreateCleanupRetry(err)
}

func compensationRetryTarget(err error) []byte {
	var retryErr *compensationRetryError
	if !errors.As(err, &retryErr) || retryErr.target == nil {
		return nil
	}
	target, marshalErr := json.Marshal(retryErr.target)
	if marshalErr != nil {
		return nil
	}
	return target
}

func isCompensatedJobError(err error) bool {
	var compensatedErr *compensatedJobError
	return errors.As(err, &compensatedErr)
}

func isManualCleanupRequired(err error) bool {
	var manualErr *manualCleanupRequiredError
	return errors.As(err, &manualErr)
}

func newPodCreateCompensatedError(stage string) error {
	return &compensatedJobError{
		err: fmt.Errorf("pod provisioning failed; compensation completed after %s", stage),
	}
}

func jobPayloadCleanupOnly(job *models.Job) bool {
	var payload struct {
		CleanupOnly bool `json:"cleanup_only"`
	}
	return json.Unmarshal(job.Payload, &payload) == nil && payload.CleanupOnly
}

func podCreateCleanupOwnsStagedClone(payload CreatePodPayload) bool {
	var podVMID uuid.UUID
	if payload.CleanupTarget != nil {
		podID, targetPodVMID, err := validateVMCloneCleanupTarget(payload.CleanupTarget)
		if err != nil || podID != payload.PodID {
			return false
		}
		podVMID = targetPodVMID
	} else if payload.CloneOperation != nil {
		podID, err := uuid.Parse(payload.CloneOperation.PodID)
		if err != nil || podID != payload.PodID {
			return false
		}
		targetPodVMID, err := uuid.Parse(payload.CloneOperation.PodVMID)
		if err != nil {
			return false
		}
		podVMID = targetPodVMID
	} else {
		return false
	}
	for _, vmSpec := range payload.VMs {
		if vmSpec.PodVMID == podVMID {
			return true
		}
	}
	return false
}

func podVMSpecIDs(vmSpecs []VMSpec) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(vmSpecs))
	for _, vmSpec := range vmSpecs {
		ids = append(ids, vmSpec.PodVMID)
	}
	return ids
}

func (p *Provisioner) releaseVMPlacementCapacity(
	ctx context.Context,
	job *models.Job,
	podVMIDs []uuid.UUID,
) error {
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	persisted := make([]uuid.UUID, 0, len(podVMIDs))
	for _, podVMID := range podVMIDs {
		placement, loadErr := p.db.GetVMPlacement(ctx, podVMID)
		if loadErr != nil {
			return fmt.Errorf(
				"%w: load VM placement %s before capacity release: %w",
				errVMPlacementCapacityRelease,
				podVMID,
				loadErr,
			)
		}
		if placement == nil {
			continue
		}
		if placement.JobID != job.ID {
			return &manualCleanupRequiredError{err: fmt.Errorf(
				"VM placement %s belongs to job %s, not %s",
				podVMID,
				placement.JobID,
				job.ID,
			)}
		}
		persisted = append(persisted, podVMID)
	}
	if len(persisted) == 0 {
		return nil
	}
	if len(persisted) != len(podVMIDs) {
		return &manualCleanupRequiredError{err: fmt.Errorf(
			"refuse partial VM placement capacity release: found %d of %d placements",
			len(persisted),
			len(podVMIDs),
		)}
	}
	if err := p.db.ReleaseVMPlacementCapacity(ctx, job.ID, workerID, persisted); err != nil {
		return fmt.Errorf("%w: %w", errVMPlacementCapacityRelease, err)
	}
	return nil
}

func (p *Provisioner) cleanupPodCreateResources(
	ctx context.Context,
	job *models.Job,
	podID uuid.UUID,
	vmSpecs []VMSpec,
	rb *rollback.Engine,
	stage string,
	allowProvisioningTransition bool,
) error {
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()

	status, podExists, err := p.db.BeginPodCreateCleanup(
		cleanupCtx,
		job.ID,
		workerID,
		podID,
		allowProvisioningTransition,
	)
	if err != nil {
		if errors.Is(err, database.ErrUnsafePodCreateCleanupState) {
			return &manualCleanupRequiredError{err: err}
		}
		return &compensationRetryError{
			err: fmt.Errorf("prepare pod_create cleanup after %s: %w", stage, err),
		}
	}

	var cleanupErrs []error
	if err := p.cleanupStagedVMClone(cleanupCtx, job.ID, workerID); err != nil {
		cleanupErrs = append(cleanupErrs, err)
	}
	cleanupErrs = append(cleanupErrs, rb.Rollback(cleanupCtx)...)
	statusForLog := status
	if !podExists {
		statusForLog = "missing"
	}
	p.logger.Warn("stopped stale pod create and rolled back completed resources",
		"pod_id", podID, "status", statusForLog, "stage", stage, "rollback_errors", cleanupErrs)
	if len(cleanupErrs) > 0 {
		return newPodCreateCleanupRetryError(stage, cleanupErrs)
	}
	if err := p.releaseVMPlacementCapacity(cleanupCtx, job, podVMSpecIDs(vmSpecs)); err != nil {
		return newPodCreateCleanupRetryError(
			stage,
			[]error{fmt.Errorf("release compensated VM capacity: %w", err)},
		)
	}
	terminalVMStatus := models.VMStatusError
	if status == models.PodStatusDestroying ||
		status == models.PodStatusDestroyFailed ||
		status == models.PodStatusDestroyed {
		terminalVMStatus = models.VMStatusDeleted
	}
	for _, vmSpec := range vmSpecs {
		if _, updateErr := p.db.UpdatePodVMStatusFrom(
			cleanupCtx,
			vmSpec.PodVMID,
			[]string{
				models.VMStatusPending,
				models.VMStatusCloning,
				models.VMStatusConfiguring,
				models.VMStatusRunning,
				"cloned",
				"failed",
			},
			terminalVMStatus,
		); updateErr != nil {
			return newPodCreateCleanupRetryError(
				stage,
				[]error{fmt.Errorf("mark compensated VM %s %s: %w", vmSpec.PodVMID, terminalVMStatus, updateErr)},
			)
		}
	}
	if err := p.db.MarkJobCompensationCompleted(cleanupCtx, job.ID, workerID); err != nil {
		return newPodCreateCleanupRetryError(
			stage,
			[]error{fmt.Errorf("record completed compensation: %w", err)},
		)
	}
	return nil
}

func (p *Provisioner) failPodCreateWithCleanup(
	ctx context.Context,
	job *models.Job,
	payload CreatePodPayload,
	rb *rollback.Engine,
	stage string,
	cause error,
) error {
	cleanupErr := p.cleanupPodCreateResources(
		ctx,
		job,
		payload.PodID,
		payload.VMs,
		rb,
		stage,
		true,
	)
	if cleanupErr != nil {
		combinedErr := combineProvisioningAndCleanupErrors(cause, cleanupErr)
		if target := compensationRetryTarget(cause); target != nil {
			var cleanupTarget VMCloneCleanupTarget
			_ = json.Unmarshal(target, &cleanupTarget)
			return &compensationRetryError{
				err:    combinedErr,
				target: &cleanupTarget,
			}
		}
		return combinedErr
	}
	if isCompensationRetry(cause) {
		return cause
	}
	if isManualCleanupRequired(cause) {
		return cause
	}
	return &compensatedJobError{err: cause}
}

func (p *Provisioner) failPodCreateForStaleVM(
	ctx context.Context,
	job *models.Job,
	payload CreatePodPayload,
	rb *rollback.Engine,
	podID, podVMID uuid.UUID,
	moref, stage string,
) error {
	cause := error(fmt.Errorf("pod_create lost VM ownership during %s", stage))
	if err := p.stageVMCloneCleanup(ctx, job, podID, podVMID, moref); err != nil {
		cause = err
	}
	return p.failPodCreateWithCleanup(ctx, job, payload, rb, stage, cause)
}

func (p *Provisioner) failPodCreateAfterCloneError(
	ctx context.Context,
	job *models.Job,
	payload CreatePodPayload,
	rb *rollback.Engine,
	podID, podVMID uuid.UUID,
	moref, stage string,
	cause error,
) error {
	if moref != "" {
		if err := p.stageVMCloneCleanup(ctx, job, podID, podVMID, moref); err != nil {
			if !isCompensatedJobError(err) {
				if isManualCleanupRequired(err) || isCompensationRetry(err) {
					cause = err
				} else {
					cause = &compensationRetryError{
						err: fmt.Errorf("%v; stage exact clone %s for cleanup: %w", cause, moref, err),
					}
				}
			}
		}
	}
	return p.failPodCreateWithCleanup(ctx, job, payload, rb, stage, cause)
}

func (p *Provisioner) runPodCreateCleanup(
	ctx context.Context,
	job *models.Job,
	payload CreatePodPayload,
	rb *rollback.Engine,
) error {
	ownsStagedClone := podCreateCleanupOwnsStagedClone(payload)
	allowProvisioningTransition := podCreateCleanupCanTransition(payload, rb.Steps())
	if payload.CleanupTarget != nil && !ownsStagedClone {
		return &manualCleanupRequiredError{
			err: errors.New("cleanup-only pod_create has an invalid or mismatched exact clone target"),
		}
	}
	if err := p.cleanupPodCreateResources(
		ctx,
		job,
		payload.PodID,
		payload.VMs,
		rb,
		"cleanup-only retry",
		allowProvisioningTransition,
	); err != nil {
		return err
	}
	return newPodCreateCompensatedError("cleanup-only retry")
}

func podCreateCleanupCanTransition(payload CreatePodPayload, steps []rollback.Step) bool {
	return podCreateCleanupOwnsStagedClone(payload) || len(steps) > 0
}

func (p *Provisioner) stopPodCreateIfStale(
	ctx context.Context,
	job *models.Job,
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
		cleanupErr := p.cleanupPodCreateResources(ctx, job, podID, vmSpecs, rb, stage, false)
		if cleanupErr != nil {
			return true, cleanupErr
		}
		return true, newPodCreateCompensatedError(stage)
	}
	if pod.Status == models.PodStatusProvisioning {
		return false, nil
	}
	if pod.Status == models.PodStatusActive {
		p.logger.Info("pod create already completed; skipping duplicate job",
			"pod_id", podID, "stage", stage)
		return true, nil
	}

	cleanupErr := p.cleanupPodCreateResources(ctx, job, podID, vmSpecs, rb, stage, false)
	if cleanupErr != nil {
		return true, cleanupErr
	}
	return true, newPodCreateCompensatedError(stage)
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
func (p *Provisioner) CreatePod(ctx context.Context, job *models.Job) (retErr error) {
	var payload CreatePodPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse pod_create payload: %w", err)
	}
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return err
	}
	clonePreamblePending := payload.CloneOperation != nil
	defer func() {
		if clonePreamblePending {
			retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)
		}
	}()
	rb, err := p.newPodCreateRollbackEngine(job, payload.PodID, payload.VMs)
	if err != nil {
		return err
	}
	if payload.CleanupOnly {
		clonePreamblePending = false
		return p.runPodCreateCleanup(ctx, job, payload, rb)
	}

	// Get pod from DB
	pod, err := p.db.GetPodByID(ctx, payload.PodID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}
	if pod == nil {
		cleanupErr := p.cleanupPodCreateResources(
			ctx,
			job,
			payload.PodID,
			payload.VMs,
			rb,
			"initial pod lookup",
			false,
		)
		if cleanupErr != nil {
			return cleanupErr
		}
		return newPodCreateCompensatedError("initial pod lookup")
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
		if pod.Status == models.PodStatusActive {
			if payload.CloneOperation != nil {
				return persistedClonePreambleFailure(
					job,
					payload.CloneOperation,
					errors.New("pod became active before its persisted clone operation was reconciled"),
				)
			}
			if err := p.verifyActivePodCredentialAcceptance(ctx, pod); err != nil {
				return fmt.Errorf("verify already-active pod credentials: %w", err)
			}
			if err := p.releaseVMPlacementCapacity(ctx, job, podVMSpecIDs(payload.VMs)); err != nil {
				return fmt.Errorf("release already-active pod VM capacity reservations: %w", err)
			}

			return nil
		}
		p.logger.Warn("stale pod create job skipped because pod is no longer provisionable",
			"pod_id", pod.ID, "status", pod.Status, "job_id", job.ID)
		_, cleanupErr := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "job_start")
		return cleanupErr
	}

	vlanTag := pod.VLANID
	octet := vlanTag - 100
	subnet := pod.Subnet
	gateway := fmt.Sprintf("10.100.%d.1/24", octet)
	pgName := fmt.Sprintf("Pod-VLAN%d", vlanTag)
	if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "admission"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// Resolve and persist the complete source/compute/pool/host plan, then prove
	// the service account can install the mandatory DRS control before any
	// OPNsense, standard-switch, or clone mutation.
	placementSpecs := make([]vmPlacementSpec, 0, len(payload.VMs))
	for _, vmSpec := range payload.VMs {
		placementSpecs = append(placementSpecs, vmPlacementSpec{
			PodVMID:   vmSpec.PodVMID,
			SourceRef: vmSpec.TemplateName,
		})
	}
	placements, err := p.prepareVMPlacementPlan(ctx, job, placementSpecs, pgName, true, nil)
	if err != nil {
		planningErr := fmt.Errorf("plan VM placements: %w", err)
		if jobRetryAvailable(job, planningErr) {
			return planningErr
		}
		return p.failPodCreateWithCleanup(
			ctx,
			job,
			payload,
			rb,
			"placement planning",
			planningErr,
		)
	}
	if err := p.vc.ValidateDRSPlacementPrivileges(ctx, vcenter.DRSPlacementTargets(placements)); err != nil {
		return p.failPodCreateWithCleanup(
			ctx,
			job,
			payload,
			rb,
			"DRS privilege preflight",
			fmt.Errorf("validate mandatory DRS placement privileges: %w", err),
		)
	}
	if err := p.enforceExistingVMPlacements(ctx, job.ID, workerID, placements); err != nil {
		return p.failPodCreateWithCleanup(
			ctx,
			job,
			payload,
			rb,
			"existing VM placement enforcement",
			err,
		)
	}
	placementsByVM := placementPlanByPodVM(placements)
	targetHosts := selectedPlacementHosts(placements)

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
	if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "vlan_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	if err := p.opn.ReconfigureVLANs(ctx); err != nil {
		return p.failPodCreateWithCleanup(ctx, job, payload, rb, "reconfigure VLANs", fmt.Errorf("reconfigure VLANs: %w", err))
	}

	// --- Step 3: Assign OPNsense interface via SSH ---
	p.publishProgress(job.ID, "interface_assign", "Assigning OPNsense interface via SSH")

	ifName, err := p.opnSSH.AssignInterface(ctx, vlanTag, gateway)
	if err != nil {
		return p.failPodCreateWithCleanup(ctx, job, payload, rb, "assign interface", fmt.Errorf("assign interface: %w", err))
	}

	if err := rb.Record(ctx, "interface_assign", map[string]interface{}{"if_name": ifName, "vlan_tag": vlanTag}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "interface_assign"); err != nil {
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
			return p.failPodCreateWithCleanup(ctx, job, payload, rb, "create DHCP", fmt.Errorf("create DHCP: %w", err))
		}
	}

	dhcpPreexStr := "false"
	if dhcpPreexisting {
		dhcpPreexStr = "true"
	}
	if err := rb.Record(ctx, "dhcp_create", map[string]string{"uuid": dhcpUUID, "preexisting": dhcpPreexStr}); err != nil {
		return err
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "dhcp_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// Add the OPNsense interface to Kea's listened interfaces BEFORE reconfigure.
	// ReconfigureDHCP regenerates config and restarts Kea, so the interface must
	// be in the config before that happens.
	if err := p.opn.AddDHCPInterface(ctx, ifName); err != nil {
		return p.failPodCreateWithCleanup(
			ctx,
			job,
			payload,
			rb,
			"add DHCP interface",
			fmt.Errorf("add DHCP interface %s: %w", ifName, err),
		)
	}

	// Wait for interface to fully stabilize before restarting Kea.
	// The VLAN interface needs time after interface_configure() to be kernel-ready.
	time.Sleep(2 * time.Second)

	if err := p.opn.ReconfigureDHCP(ctx); err != nil {
		return p.failPodCreateWithCleanup(ctx, job, payload, rb, "reconfigure DHCP", fmt.Errorf("reconfigure DHCP: %w", err))
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
		if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "firewall_create"); err != nil {
			return err
		} else if stopped {
			return nil
		}
	}
	if err != nil {
		return p.failPodCreateWithCleanup(ctx, job, payload, rb, "ensure firewall rule", fmt.Errorf("ensure firewall rule: %w", err))
	}

	// --- Step 5: Create port groups only on selected ESXi hosts ---
	p.publishProgress(job.ID, "portgroup_create", fmt.Sprintf("Creating port group %s on selected hosts", pgName))

	err = p.db.WithVCenterPortGroupMutationLock(ctx, func(lockCtx context.Context) error {
		var (
			receipt    *vcenter.PortGroupReceipt
			receiptRaw json.RawMessage
		)
		record, receiptErr := p.db.GetPodPortGroupReceipt(lockCtx, pod.ID)
		switch {
		case receiptErr == nil:
			var persisted vcenter.PortGroupReceipt
			if err := json.Unmarshal(record.Receipt, &persisted); err != nil {
				return fmt.Errorf("decode durable pod port group receipt: %w", err)
			}
			if record.RemovedAt != nil {
				return fmt.Errorf("%w: pod %s port group ownership was already removed", database.ErrPortGroupReceiptConflict, pod.ID)
			}
			receipt = &persisted
			receiptWithKeys, keyErr := vcenter.PortGroupReceiptWithKeys(persisted, record.Keys)
			if keyErr != nil {
				return keyErr
			}
			receipt = &receiptWithKeys
			receiptRaw = record.Receipt
		case errors.Is(receiptErr, database.ErrPortGroupReceiptNotFound):
			planned, planErr := p.vc.PlanPortGroupMutationForHosts(lockCtx, pgName, vlanTag, targetHosts)
			if planErr != nil {
				return fmt.Errorf("plan port group mutation: %w", planErr)
			}
			receipt = &planned
			raw, marshalErr := json.Marshal(planned)
			if marshalErr != nil {
				return fmt.Errorf("marshal port group receipt: %w", marshalErr)
			}
			if persistErr := p.db.PersistPodPortGroupReceipt(
				lockCtx,
				job.ID,
				workerID,
				pod.ID,
				raw,
			); persistErr != nil {
				return fmt.Errorf("persist durable pod port group receipt: %w", persistErr)
			}
			receiptRaw = raw
		default:
			return receiptErr
		}
		if _, scopeErr := validatePortGroupReceiptScope(*receipt, pgName, vlanTag, targetHosts); scopeErr != nil {
			return scopeErr
		}
		// The independent ledger survives rollback checkpoints; the rollback
		// step drives compensation progress for this job.
		if recordErr := rb.Record(lockCtx, "portgroup_create", *receipt); recordErr != nil {
			return fmt.Errorf("persist port group rollback step: %w", recordErr)
		}
		if beginErr := p.db.BeginPodPortGroupMutation(
			lockCtx,
			job.ID,
			workerID,
			pod.ID,
			receiptRaw,
		); beginErr != nil {
			return fmt.Errorf("persist port group mutation intent: %w", beginErr)
		}
		if err := p.vc.ApplyPortGroupMutation(lockCtx, *receipt); err != nil {
			return err
		}
		keys, err := p.vc.CapturePortGroupKeys(lockCtx, *receipt)
		if err != nil {
			return fmt.Errorf("capture stable port group identities: %w", err)
		}
		if err := p.db.PersistPodPortGroupKeys(lockCtx, pod.ID, receiptRaw, keys); err != nil {
			return fmt.Errorf("persist stable port group identities: %w", err)
		}
		return nil
	})
	if err != nil {
		return p.failPodCreateWithCleanup(ctx, job, payload, rb, "create port groups", fmt.Errorf("create port groups: %w", err))
	}
	if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "portgroup_create"); err != nil {
		return err
	} else if stopped {
		return nil
	}

	// --- Step 6: Clone VMs ---
	var clonedVMs []int // indices of successfully cloned VMs
	for i, vmSpec := range payload.VMs {
		stepName := fmt.Sprintf("vm_clone_%d", i)
		p.publishProgress(job.ID, stepName, fmt.Sprintf("Cloning VM %s from %s", vmSpec.VMName, vmSpec.TemplateName))
		placement, ok := placementsByVM[vmSpec.PodVMID]
		if !ok {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"placement lookup",
				fmt.Errorf("durable placement is missing for pod VM %s", vmSpec.PodVMID),
			)
		}

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
		resumeCloneOperation := payload.CloneOperation != nil &&
			payload.CloneOperation.PodVMID == vmSpec.PodVMID.String()
		existingVMMoref := ""
		if podVMRow.VCenterVMID != nil {
			existingVMMoref = *podVMRow.VCenterVMID
		}
		if podVMRow.Status == models.VMStatusDeleted || podVMRow.Status == models.VMStatusError {
			return p.failPodCreateForStaleVM(
				ctx,
				job,
				payload,
				rb,
				pod.ID,
				vmSpec.PodVMID,
				existingVMMoref,
				"pre-clone terminal-state check",
			)
		}
		tmpl, err = p.db.GetTemplateByID(ctx, podVMRow.TemplateID)
		if err != nil {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"load template credential contract",
				fmt.Errorf("get template %s for pod VM %s: %w", podVMRow.TemplateID, vmSpec.PodVMID, err),
			)
		}
		if err := requireTemplateGuestCredentialAcceptanceForNewClone(
			tmpl,
			existingVMMoref,
			resumeCloneOperation,
		); err != nil {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"verify template credential acceptance",
				fmt.Errorf("template %s is not credential-ready: %w", podVMRow.TemplateID, err),
			)
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
		storedUsername, storedPassword, credentialErr := provisionedPodVMCredentials(
			kind,
			osType,
			podVMRow,
			tmpl,
			generatePassword,
		)
		if credentialErr != nil {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"resolve pod VM credentials",
				fmt.Errorf("resolve credentials for pod VM %s: %w", vmSpec.PodVMID, credentialErr),
			)
		}
		if err := p.db.UpdatePodVMCredentials(
			ctx,
			vmSpec.PodVMID,
			storedUsername,
			storedPassword,
		); err != nil {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"persist pod VM credentials",
				fmt.Errorf("persist credentials for pod VM %s before clone: %w", vmSpec.PodVMID, err),
			)
		}
		customizationPassword := ""
		if shouldGenerateGuestPassword(kind, osType) {
			customizationPassword = storedPassword
		}

		// Resume support: if a prior worker already cloned this VM (job was
		// recovered after a worker restart via RecoverStaleJobs), the
		// pod_vms row will have vcenter_vm_id set. Re-cloning with the
		// same name fails with "already exists", leaks the cloned VM, and
		// marks the pod failed. Reuse the existing clone instead. Mirrors
		// AddVM's resume path in vm_ops.go.
		var moref string
		if existingVMMoref != "" && !resumeCloneOperation {
			moref = existingVMMoref
			if placementErr := p.verifyPersistedVMPlacement(ctx, moref, placement); placementErr != nil {
				classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
					"refuse to resume persisted VM %s after placement validation failed: %w",
					moref,
					placementErr,
				))
				return p.failPodCreateAfterCloneError(
					ctx,
					job,
					payload,
					rb,
					pod.ID,
					vmSpec.PodVMID,
					moref,
					"resume placement validation",
					classifiedErr,
				)
			}
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
					return p.failPodCreateForStaleVM(
						ctx, job, payload, rb, pod.ID, vmSpec.PodVMID, currentMoref, "clone-state claim",
					)
				}
				if resumeCloneOperation {
					return p.failPodCreateWithCleanup(
						ctx,
						job,
						payload,
						rb,
						"persisted clone state reconciliation",
						fmt.Errorf(
							"VM %s entered state %s before clone operation %s could resume",
							vmSpec.PodVMID,
							current.Status,
							payload.CloneOperation.OperationID,
						),
					)
				}
				p.logger.Warn("skipping pod-create VM after losing provisioning state",
					"pod_id", pod.ID, "pod_vm_id", vmSpec.PodVMID, "vm_status", current.Status)
				continue
			}

			var cloneErr error
			cloneParams := cloneParamsFromPlacement(vcenter.CloneVMParams{
				TemplateName: vmSpec.TemplateName,
				VMName:       vmSpec.VMName,
				VCPUs:        int32(podVMRow.VCPUs),
				RAMmb:        int64(podVMRow.RAMMB),
				Network:      pgName,
				OSType:       osType,
				Password:     customizationPassword,
			}, placement)
			moref, cloneErr = executeDurableVMClone(
				ctx,
				p.db,
				p.vc,
				job.ID,
				workerID,
				pod.ID,
				vmSpec.PodVMID,
				cloneParams,
			)
			if cloneErr != nil {
				p.logger.Error("failed to clone VM", "vm", vmSpec.VMName, "error", cloneErr)
				cloneErr = classifyCloneOperationFailure(cloneErr)
				if cloneForwardRetryAvailable(job, cloneErr) {
					return cloneErr
				}
				return p.failPodCreateAfterCloneError(
					ctx,
					job,
					payload,
					rb,
					pod.ID,
					vmSpec.PodVMID,
					moref,
					"clone operation recovery",
					cloneErr,
				)
			}
			if existingVMMoref != "" && existingVMMoref != moref {
				return p.failPodCreateWithCleanup(
					ctx,
					job,
					payload,
					rb,
					"persisted clone reference reconciliation",
					fmt.Errorf(
						"refuse to overwrite existing pod VM reference %s with resumed clone %s",
						existingVMMoref,
						moref,
					),
				)
			}
			if resumeCloneOperation {
				clonePreamblePending = false
			}
			cleanupTarget := cloneCleanupTargetFromPlacement(pod.ID, moref, placement)
			if err := rb.Record(ctx, stepName, cleanupTarget); err != nil {
				return &compensationRetryError{
					err:    fmt.Errorf("persist rollback for cloned VM %s: %w", moref, err),
					target: cleanupTarget,
				}
			}

			// Persist the clone only while this job still owns the VM row. A
			// concurrent delete wins; its clone is removed or handed to a
			// durable cleanup-only retry.
			applied, updateErr = p.db.AdoptPodVMClone(
				ctx,
				job.ID,
				workerID,
				vmSpec.PodVMID,
				[]string{models.VMStatusCloning},
				moref,
				vmSpec.VMName,
				"cloned",
			)
			if updateErr != nil {
				return &compensationRetryError{
					err: fmt.Errorf("resolve atomically staged pod clone adoption for %s: %w", moref, updateErr),
				}
			}
			if !applied {
				return p.failPodCreateForStaleVM(
					ctx, job, payload, rb, pod.ID, vmSpec.PodVMID, moref, "clone adoption",
				)
			}
			if err := p.db.DisarmVMCloneCleanup(ctx, job.ID, workerID, vmSpec.PodVMID, moref); err != nil {
				cleanupTarget := cloneCleanupTargetFromPlacement(pod.ID, moref, placement)
				return &compensationRetryError{
					err:    fmt.Errorf("disarm adopted clone %s: %w", moref, err),
					target: cleanupTarget,
				}
			}
		}

		if stopped, err := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, stepName); err != nil {
			return err
		} else if stopped {
			return nil
		}
		clonedVMs = append(clonedVMs, i)
	}

	// If no VMs were cloned at all, rollback infrastructure
	if len(clonedVMs) == 0 {
		return p.failPodCreateWithCleanup(
			ctx,
			job,
			payload,
			rb,
			"all VM clones failed",
			errors.New("all VM clones failed"),
		)
	}

	// --- Step 7: Power on VMs by boot order and wait for IPs per group ---
	p.publishProgress(job.ID, "vm_poweron", "Powering on VMs")

	type vmPowerInfo struct {
		index             int
		vmSpec            VMSpec
		moref             string
		osType            string
		username          string
		password          string
		requiresReadiness bool
		alreadyRunning    bool
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
				return p.failPodCreateWithCleanup(
					ctx,
					job,
					payload,
					rb,
					"load pod VM for power-on",
					fmt.Errorf("get pod VM %s for power-on: %w", vmSpec.PodVMID, err),
				)
			}
			if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
				return p.failPodCreateWithCleanup(
					ctx,
					job,
					payload,
					rb,
					"power-on identity preflight",
					fmt.Errorf("pod VM %s has no vCenter identity", vmSpec.PodVMID),
				)
			}
			placement, ok := placementsByVM[vmSpec.PodVMID]
			if !ok {
				return p.failPodCreateWithCleanup(
					ctx,
					job,
					payload,
					rb,
					"power-on placement lookup",
					fmt.Errorf("durable placement is missing for pod VM %s", vmSpec.PodVMID),
				)
			}
			if placementErr := p.vc.ValidateVMPlacementControl(
				ctx,
				*podVM.VCenterVMID,
				placement.HostMoref,
				placement.ComputeResourceType,
				placement.ComputeResourceMoref,
				placement.DRSControl,
			); placementErr != nil {
				p.recordVMPlacementDrift(placementErr)
				classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
					"refuse to reuse persisted VM %s after placement validation failed: %w",
					*podVM.VCenterVMID,
					placementErr,
				))
				return p.failPodCreateAfterCloneError(
					ctx,
					job,
					payload,
					rb,
					pod.ID,
					vmSpec.PodVMID,
					*podVM.VCenterVMID,
					"power-on placement validation",
					classifiedErr,
				)
			}
			if podVM.Status == models.VMStatusRunning {
				groupPoweredOn = append(groupPoweredOn, vmPowerInfo{
					index:    i,
					vmSpec:   vmSpec,
					moref:    *podVM.VCenterVMID,
					osType:   podVM.OSType,
					username: podVM.GeneratedUsername,
					password: podVM.GeneratedPassword,
					requiresReadiness: !podVMCredentialAccepted(podVM) &&
						resolveTemplateKind(vmSpec.Kind) == models.TemplateKindCloneWithCustomize,
					alreadyRunning: true,
				})
				continue
			}
			if podVM.Status == models.VMStatusDeleted || podVM.Status == models.VMStatusError {
				return p.failPodCreateForStaleVM(
					ctx, job, payload, rb, pod.ID, vmSpec.PodVMID, *podVM.VCenterVMID, "power-on preflight",
				)
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
					groupPoweredOn = append(groupPoweredOn, vmPowerInfo{
						index:    i,
						vmSpec:   vmSpec,
						moref:    *podVM.VCenterVMID,
						osType:   current.OSType,
						username: current.GeneratedUsername,
						password: current.GeneratedPassword,
						requiresReadiness: !podVMCredentialAccepted(current) &&
							resolveTemplateKind(vmSpec.Kind) == models.TemplateKindCloneWithCustomize,
						alreadyRunning: true,
					})
					continue
				}
				if current.Status == models.VMStatusDeleted || current.Status == models.VMStatusError {
					return p.failPodCreateForStaleVM(
						ctx, job, payload, rb, pod.ID, vmSpec.PodVMID, *podVM.VCenterVMID, "power-on state claim",
					)
				}
				continue
			}

			if err := p.vc.PowerOnVM(ctx, *podVM.VCenterVMID); err != nil {
				classifiedErr := classifyPlacementValidationFailure(fmt.Errorf(
					"power on persisted VM %s: %w",
					*podVM.VCenterVMID,
					err,
				))
				if jobRetryAvailable(job, classifiedErr) {
					return classifiedErr
				}
				return p.failPodCreateWithCleanup(
					ctx,
					job,
					payload,
					rb,
					"power-on VM",
					classifiedErr,
				)
			}

			osType := vmSpec.OSType
			if podVM.OSType != "" {
				osType = podVM.OSType
			}
			groupPoweredOn = append(groupPoweredOn, vmPowerInfo{
				index:    i,
				vmSpec:   vmSpec,
				moref:    *podVM.VCenterVMID,
				osType:   osType,
				username: podVM.GeneratedUsername,
				password: podVM.GeneratedPassword,
				requiresReadiness: !podVMCredentialAccepted(podVM) &&
					resolveTemplateKind(vmSpec.Kind) == models.TemplateKindCloneWithCustomize,
			})
		}

		// Wait for IPs and prove generated credentials are accepted before
		// marking newly powered VMs running or starting the next boot group.
		// Skip the wait for any VM whose template was registered with
		// assign_ip=false — its network is owner-managed (DHCP/static inside
		// the guest), so the provisioner has no IP to record.
		var wg sync.WaitGroup
		credentialErrs := make(chan error, len(groupPoweredOn))
		for _, info := range groupPoweredOn {
			if info.requiresReadiness {
				wg.Add(1)
				go func(vmInfo vmPowerInfo) {
					defer wg.Done()
					err := enforcePodVMCredentialAcceptance(
						ctx,
						p.db,
						p.vc,
						vmInfo.vmSpec.PodVMID,
						vmInfo.vmSpec.Kind,
						vmInfo.osType,
						vmInfo.moref,
						vmInfo.username,
						vmInfo.password,
						podGuestCredentialReadyTimeout,
						podGuestCredentialRetryInterval,
						nil,
					)
					if err != nil {
						credentialErrs <- fmt.Errorf(
							"VM %s rejected its generated credential: %w",
							vmInfo.vmSpec.VMName,
							err,
						)
					}
				}(info)
			}
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
		close(credentialErrs)
		for credentialErr := range credentialErrs {
			return p.failPodCreateWithCleanup(
				ctx,
				job,
				payload,
				rb,
				"verify guest credentials",
				credentialErr,
			)
		}

		for _, info := range groupPoweredOn {
			if info.alreadyRunning {
				continue
			}
			applied, err = p.db.UpdatePodVMStatusFrom(
				ctx,
				info.vmSpec.PodVMID,
				[]string{models.VMStatusConfiguring},
				models.VMStatusRunning,
			)
			if err != nil {
				return fmt.Errorf("mark pod VM %s running: %w", info.vmSpec.PodVMID, err)
			}
			if !applied {
				return p.failPodCreateForStaleVM(
					ctx,
					job,
					payload,
					rb,
					pod.ID,
					info.vmSpec.PodVMID,
					info.moref,
					"mark credential-ready VM running",
				)
			}
		}

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
		_, stopErr := p.stopPodCreateIfStale(ctx, job, pod.ID, payload.VMs, rb, "activate")
		return stopErr
	}
	if err := p.releaseVMPlacementCapacity(ctx, job, podVMSpecIDs(payload.VMs)); err != nil {
		return fmt.Errorf("release running pod VM capacity reservations: %w", err)
	}

	p.logger.Info("pod created successfully", "pod_id", pod.ID, "vlan", vlanTag)
	return nil
}

func (p *Provisioner) verifyActivePodCredentialAcceptance(
	ctx context.Context,
	pod *models.Pod,
) error {
	for i := range pod.VMs {
		vm := &pod.VMs[i]
		tmpl, err := p.db.GetTemplateByID(ctx, vm.TemplateID)
		if err != nil {
			return fmt.Errorf("load template %s for VM %s: %w", vm.TemplateID, vm.ID, err)
		}
		if resolveTemplateKind(tmpl.Kind) != models.TemplateKindCloneWithCustomize ||
			podVMCredentialAccepted(vm) {
			continue
		}
		if vm.Status != models.VMStatusRunning || vm.VCenterVMID == nil || *vm.VCenterVMID == "" {
			return fmt.Errorf("customized VM %s lacks a running vCenter identity", vm.ID)
		}
		if err := enforcePodVMCredentialAcceptance(
			ctx,
			p.db,
			p.vc,
			vm.ID,
			tmpl.Kind,
			vm.OSType,
			*vm.VCenterVMID,
			vm.GeneratedUsername,
			vm.GeneratedPassword,
			podGuestCredentialReadyTimeout,
			podGuestCredentialRetryInterval,
			nil,
		); err != nil {
			return fmt.Errorf("VM %s rejected its persisted credential: %w", vm.ID, err)
		}
	}
	return nil
}
