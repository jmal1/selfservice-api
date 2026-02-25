package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	events "github.com/jmal1/selfservice-api/internal/nats"
	"github.com/jmal1/selfservice-api/internal/opnsense"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// Provisioner orchestrates pod lifecycle operations.
type Provisioner struct {
	db       *database.Queries
	vc       *vcenter.Client
	opn      *opnsense.Client
	opnSSH   *opnsense.SSHClient
	nats     *events.Client
	logger   *slog.Logger
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

	// Mark in_progress
	if err := p.db.UpdateJobStatus(ctx, job.ID, models.JobStatusInProgress, nil); err != nil {
		return fmt.Errorf("update job status: %w", err)
	}
	p.publishProgress(job.ID, "started", "Job processing started")

	var err error
	switch job.Type {
	case models.JobTypePodCreate:
		err = p.CreatePod(ctx, job)
	case models.JobTypePodDestroy:
		err = p.DestroyPod(ctx, job)
	case models.JobTypeVMStart:
		err = p.PowerVM(ctx, job, "start")
	case models.JobTypeVMStop:
		err = p.PowerVM(ctx, job, "stop")
	case models.JobTypeVMRestart:
		err = p.PowerVM(ctx, job, "restart")
	default:
		err = fmt.Errorf("unknown job type: %s", job.Type)
	}

	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		_ = p.db.UpdateJobStatus(ctx, job.ID, models.JobStatusFailed, result)
		p.publishProgress(job.ID, "failed", err.Error())
		return err
	}

	_ = p.db.UpdateJobStatus(ctx, job.ID, models.JobStatusCompleted, nil)
	p.publishProgress(job.ID, "completed", "Job completed successfully")
	return nil
}

func (p *Provisioner) publishProgress(jobID uuid.UUID, step, message string) {
	if p.nats != nil {
		_ = p.nats.PublishJobStatus(jobID, step, message)
	}
}

// newRollbackEngine creates a rollback engine for a job.
func (p *Provisioner) newRollbackEngine(jobID uuid.UUID) *rollback.Engine {
	return rollback.New(jobID, &dbPersister{db: p.db}, p.logger)
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
}

// CreatePod executes the full pod creation workflow with rollback.
func (p *Provisioner) CreatePod(ctx context.Context, job *models.Job) error {
	var payload CreatePodPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse pod_create payload: %w", err)
	}

	// Get pod from DB
	pod, err := p.db.GetPodByID(ctx, payload.PodID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}

	rb := p.newRollbackEngine(job.ID)
	vlanTag := pod.VLANID
	podIndex := pod.PodIndex
	subnet := fmt.Sprintf("10.100.%d.0/24", podIndex)
	gateway := fmt.Sprintf("10.100.%d.1/24", podIndex)
	pgName := fmt.Sprintf("Pod-%03d-VLAN%d", podIndex, vlanTag)

	// --- Step 1: Update pod status to provisioning ---
	p.publishProgress(job.ID, "pod_update", "Setting pod status to provisioning")
	if err := p.db.UpdatePodStatus(ctx, pod.ID, "provisioning", ""); err != nil {
		return fmt.Errorf("update pod status: %w", err)
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
		vlanUUID, err = p.opn.CreateVLAN(ctx, "vmx1", vlanTag, fmt.Sprintf("Pod-%03d", podIndex))
		if err != nil {
			return fmt.Errorf("create VLAN: %w", err)
		}
	}

	rb.RegisterUndo("vlan_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			UUID        string `json:"uuid"`
			Preexisting string `json:"preexisting"`
		}
		json.Unmarshal(data, &d)
		if d.Preexisting == "true" {
			return nil // don't delete pre-existing resources
		}
		if err := p.opn.DeleteVLAN(ctx, d.UUID); err != nil {
			return err
		}
		return p.opn.ReconfigureVLANs(ctx)
	})
	preexStr := "false"
	if vlanPreexisting {
		preexStr = "true"
	}
	if err := rb.Record(ctx, "vlan_create", map[string]string{"uuid": vlanUUID, "preexisting": preexStr}); err != nil {
		return err
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

	rb.RegisterUndo("interface_assign", func(ctx context.Context, data json.RawMessage) error {
		var d struct{ IfName string }
		json.Unmarshal(data, &d)
		return p.opnSSH.UnassignInterface(ctx, d.IfName)
	})
	if err := rb.Record(ctx, "interface_assign", map[string]string{"if_name": ifName}); err != nil {
		return err
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
		poolRange := fmt.Sprintf("10.100.%d.10-10.100.%d.250", podIndex, podIndex)
		dhcpUUID, err = p.opn.CreateDHCPSubnet(ctx, subnet, poolRange, gateway)
		if err != nil {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("create DHCP (rollback errors: %v): %w", rbErrs, err)
		}
	}

	rb.RegisterUndo("dhcp_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			UUID        string `json:"uuid"`
			Preexisting string `json:"preexisting"`
		}
		json.Unmarshal(data, &d)
		if d.Preexisting == "true" {
			return nil
		}
		if err := p.opn.DeleteDHCPSubnet(ctx, d.UUID); err != nil {
			return err
		}
		return p.opn.ReconfigureDHCP(ctx)
	})
	dhcpPreexStr := "false"
	if dhcpPreexisting {
		dhcpPreexStr = "true"
	}
	if err := rb.Record(ctx, "dhcp_create", map[string]string{"uuid": dhcpUUID, "preexisting": dhcpPreexStr}); err != nil {
		return err
	}

	if err := p.opn.ReconfigureDHCP(ctx); err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("reconfigure DHCP (rollback errors: %v): %w", rbErrs, err)
	}

	// --- Step 5: Create port groups on all ESXi hosts ---
	p.publishProgress(job.ID, "portgroup_create", fmt.Sprintf("Creating port group %s on all hosts", pgName))

	if err := p.vc.CreatePortGroupOnAllHosts(ctx, pgName, vlanTag); err != nil {
		rbErrs := rb.Rollback(ctx)
		return fmt.Errorf("create port groups (rollback errors: %v): %w", rbErrs, err)
	}

	rb.RegisterUndo("portgroup_create", func(ctx context.Context, data json.RawMessage) error {
		var d struct {
			Name        string `json:"name"`
			Preexisting string `json:"preexisting"`
		}
		json.Unmarshal(data, &d)
		if d.Preexisting == "true" {
			return nil
		}
		return p.vc.DeletePortGroupOnAllHosts(ctx, d.Name)
	})
	// Port groups are always idempotent (already-exists is ignored), so treat as preexisting
	// if the VLAN was preexisting (they go together)
	pgPreexStr := "false"
	if vlanPreexisting {
		pgPreexStr = "true"
	}
	if err := rb.Record(ctx, "portgroup_create", map[string]string{"name": pgName, "preexisting": pgPreexStr}); err != nil {
		return err
	}

	// --- Step 6: Clone VMs ---
	for i, vmSpec := range payload.VMs {
		stepName := fmt.Sprintf("vm_clone_%d", i)
		p.publishProgress(job.ID, stepName, fmt.Sprintf("Cloning VM %s from %s", vmSpec.VMName, vmSpec.TemplateName))

		moref, err := p.vc.CloneVM(ctx, vcenter.CloneVMParams{
			TemplateName: vmSpec.TemplateName,
			VMName:       vmSpec.VMName,
			VCPUs:        vmSpec.VCPUs,
			RAMmb:        vmSpec.RAMMB,
			Network:      pgName,
		})
		if err != nil {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("clone VM %s (rollback errors: %v): %w", vmSpec.VMName, rbErrs, err)
		}

		// Update pod_vms record with vCenter details
		_ = p.db.UpdatePodVM(ctx, vmSpec.PodVMID, moref, vmSpec.VMName, "cloned")

		rb.RegisterUndo(stepName, func(ctx context.Context, data json.RawMessage) error {
			var d struct{ Moref string }
			json.Unmarshal(data, &d)
			return p.vc.DestroyVM(ctx, d.Moref)
		})
		if err := rb.Record(ctx, stepName, map[string]string{"moref": moref}); err != nil {
			return err
		}
	}

	// --- Step 7: Power on VMs and wait for IPs ---
	for i, vmSpec := range payload.VMs {
		stepName := fmt.Sprintf("vm_poweron_%d", i)
		p.publishProgress(job.ID, stepName, fmt.Sprintf("Powering on %s", vmSpec.VMName))

		// Get moref from DB
		podVM, err := p.db.GetPodVM(ctx, vmSpec.PodVMID)
		if err != nil {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("get pod VM %s (rollback errors: %v): %w", vmSpec.PodVMID, rbErrs, err)
		}

		if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("pod VM %s has no moref (rollback errors: %v)", vmSpec.PodVMID, rbErrs)
		}
		vmMoref := *podVM.VCenterVMID

		if err := p.vc.PowerOnVM(ctx, vmMoref); err != nil {
			rbErrs := rb.Rollback(ctx)
			return fmt.Errorf("power on %s (rollback errors: %v): %w", vmSpec.VMName, rbErrs, err)
		}

		// Wait for IP (5 minute timeout per VM)
		ip, err := p.vc.WaitForIP(ctx, vmMoref, 5*time.Minute)
		if err != nil {
			p.logger.Warn("timeout waiting for VM IP", "vm", vmSpec.VMName, "error", err)
			// Don't rollback for IP timeout — VM is still usable
		} else {
			_ = p.db.UpdatePodVMIP(ctx, vmSpec.PodVMID, ip)
		}

		_ = p.db.UpdatePodVMStatus(ctx, vmSpec.PodVMID, "running")
	}

	// --- Step 8: Mark pod active ---
	p.publishProgress(job.ID, "pod_active", "Pod is active")
	if err := p.db.UpdatePodStatus(ctx, pod.ID, "active", ""); err != nil {
		return fmt.Errorf("update pod to active: %w", err)
	}

	p.logger.Info("pod created successfully", "pod_id", pod.ID, "vlan", vlanTag)
	return nil
}
