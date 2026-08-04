package provisioner

// template_jobs.go - Worker job handlers for the template creation
// wizard (T4). Two job types:
//
//  - template_provision  : clones a source VM into the staging folder,
//                          attaches NIC, powers on, waits for tools,
//                          and transitions template_state to
//                          'configuring' so the instructor can RDP/WebMKS
//                          in and install software.
//
//  - template_generalize : runs the OS-specific generalization script
//                          (cloud-init clean for Linux, sysprep for
//                          Windows) via VMware Tools, waits for the VM
//                          to power off, snapshots it as `base-image`,
//                          and transitions template_state to 'ready'.
//
// Both jobs are intentionally chunky (single function each) — splitting
// them into smaller steps would force premature abstraction without
// real reuse value. The rollback engine handles partial failures.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/provisioner/assets"
	"github.com/jmal1/selfservice-api/internal/templates"
	"github.com/jmal1/selfservice-api/internal/unattend"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// windowsUnattendGuestPath is where the Windows generalize step writes a
// freshly-rendered unattend.xml *before* invoking sysprep. We use the
// canonical Panther location so the file is on the same volume as the OS
// and survives sysprep's first-run pass (which renames it to
// unattend.xml under Panther\Unattend\ regardless of source path).
//
// IMPORTANT: this file must be written *every* generalize run. Sysprep
// ALWAYS scrubs plaintext <Password> elements in unattend.xml after a
// /generalize, replacing them with "*SENSITIVE*DATA*DELETED*". Once that
// happens the file is useless for subsequent clones (the LocalAccount
// password becomes the literal scrub-marker string, no Student account
// is ever created, and every L3 clone drops into manual OOBE). The fix
// is two-part: (1) the file we ship uses base64-encoded UTF-16LE
// passwords with <PlainText>false</PlainText>, which sysprep does NOT
// scrub; (2) we still overwrite the file every generalize run so a
// previously-scrubbed copy on a re-generalized L2 gets replaced. See
// internal/provisioner/assets/windows-unattend.xml and the wiki entry
// "Why Windows templates need a fresh unattend.xml on every generalize".
const windowsUnattendGuestPath = `C:\Windows\Panther\unattend.xml`

// TemplateProvisionPayload describes the work for a template_provision job.
//
// SourceType is one of models.TemplateSource* constants. SourceRef's
// meaning depends on SourceType:
//   - clone_template / clone_vcenter: vCenter VM moref of the source
//   - iso: "[datastore] path/to/installer.iso" — the installer media a
//     freshly-created blank VM boots from. See provisionTemplateFromISO.
//
// VMName must be unique within FolderPath. Convention is
// tpl-{template-slug}-{6-char-hex}.
//
// DiskGB / GuestID / UnattendMode / UnattendConfig are only consulted by the
// iso source path. DiskGB and GuestID size and identify the blank VM's shell;
// UnattendMode selects the automated-install family (or "manual"/empty for a
// hands-on console install), and UnattendConfig carries the mode-specific seed
// knobs unmarshalled into an unattend.Spec.
type TemplateProvisionPayload struct {
	TemplateID     uuid.UUID       `json:"template_id"`
	SourceType     string          `json:"source_type"`
	SourceRef      string          `json:"source_ref"`
	VMName         string          `json:"vm_name"`
	FolderPath     string          `json:"folder_path,omitempty"`
	StagingNetwork string          `json:"staging_network"`
	VCPUs          int32           `json:"vcpus,omitempty"`
	RAMmb          int64           `json:"ram_mb,omitempty"`
	DiskGB         int             `json:"disk_gb,omitempty"`
	GuestID        string          `json:"guest_id,omitempty"`
	UnattendMode   string          `json:"unattend_mode,omitempty"`
	UnattendConfig json.RawMessage `json:"unattend_config,omitempty"`
}

// TemplateGeneralizePayload describes the work for a template_generalize job.
//
// SECURITY: GuestUsername / GuestPassword are present in the job payload
// (jobs.payload, JSONB) for as long as the job is in flight. After
// completion (success OR failure), the worker overwrites them with
// "[redacted]" in the jobs.result column. The raw payload column is NOT
// scrubbed — we accept that risk because admins already see plaintext
// default_password in templates rows; the threat model is "everyone with
// jobs SELECT access already sees template credentials".
type TemplateGeneralizePayload struct {
	TemplateID    uuid.UUID `json:"template_id"`
	OSType        string    `json:"os_type"`
	GuestUsername string    `json:"guest_username"`
	GuestPassword string    `json:"guest_password"`
	VMMoref       string    `json:"vm_moref"`
	SnapshotName  string    `json:"snapshot_name,omitempty"` // defaults to "base-image"
}

// ProvisionTemplate implements JobTypeTemplateProvision.
//
// Flow:
//  1. Read template row; verify it's in 'provisioning' state (advance to
//     'error' on any irrecoverable failure).
//  2. Clone source VM into staging folder (idempotent — returns existing
//     moref if name collision).
//  3. Persist the new moref against the template row.
//  4. Attach NIC to staging network.
//  5. Power on, wait for VMware Tools (5 min).
//  6. Transition template_state → 'configuring'.
//
// On error, sets template_state → 'error' and surfaces the cause via the
// job result. The instructor can hit POST /admin/templates/:id/retry to
// reset to 'draft' and re-run.
func (p *Provisioner) ProvisionTemplate(ctx context.Context, job *models.Job) error {
	var payload TemplateProvisionPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template_provision payload: %w", err)
	}
	if payload.TemplateID == uuid.Nil {
		return fmt.Errorf("template_id is required")
	}
	if payload.StagingNetwork == "" {
		return fmt.Errorf("staging_network is required")
	}
	if payload.VMName == "" {
		return fmt.Errorf("vm_name is required")
	}

	tmpl, err := p.db.GetTemplateByID(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template %s: %w", payload.TemplateID, err)
	}
	if tmpl == nil {
		return fmt.Errorf("template %s not found", payload.TemplateID)
	}
	if tmpl.TemplateState != models.TemplateStateProvisioning {
		return fmt.Errorf("template %s is in state %q, expected %q",
			payload.TemplateID, tmpl.TemplateState, models.TemplateStateProvisioning)
	}

	// Source dispatch.
	//
	// IMPORTANT: source_ref semantics depend on source_type. Historically
	// this branch treated both as a raw moref, which silently broke the
	// clone_template flow because the UI submits the *Crucible templates.id
	// UUID* — not a vCenter moref. We now resolve clone_template's UUID
	// to a real moref by looking up the source template row and using
	// its vCenter VM ID (preferred, set by a prior wizard run) or its
	// vcenter_template name (fallback for legacy/static templates).
	var sourceMoref string
	switch payload.SourceType {
	case models.TemplateSourceCloneTemplate:
		if payload.SourceRef == "" {
			return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("source_ref required for source_type=%s", payload.SourceType))
		}
		sourceTemplateID, parseErr := uuid.Parse(payload.SourceRef)
		if parseErr != nil {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("source_ref %q is not a valid Crucible template UUID for source_type=clone_template: %w", payload.SourceRef, parseErr))
		}
		srcTmpl, srcErr := p.db.GetTemplateByID(ctx, sourceTemplateID)
		if srcErr != nil {
			return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("load source template %s: %w", sourceTemplateID, srcErr))
		}
		if srcTmpl == nil {
			return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("source template %s not found", sourceTemplateID))
		}
		switch {
		case srcTmpl.VCenterVMID != "":
			sourceMoref = srcTmpl.VCenterVMID
			p.logger.Info("resolved clone_template source via VCenterVMID",
				"source_template_id", sourceTemplateID, "moref", sourceMoref)
		case srcTmpl.VCenterTemplate != "":
			resolved, rerr := p.vc.ResolveVMByName(ctx, srcTmpl.VCenterTemplate)
			if rerr != nil {
				return p.markTemplateError(ctx, payload.TemplateID,
					fmt.Errorf("resolve source template %q to vCenter moref: %w", srcTmpl.VCenterTemplate, rerr))
			}
			sourceMoref = resolved
			p.logger.Info("resolved clone_template source via vCenterTemplate name",
				"source_template_id", sourceTemplateID, "vcenter_template", srcTmpl.VCenterTemplate, "moref", sourceMoref)
		default:
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("source template %s has neither vcenter_vm_id nor vcenter_template set; cannot resolve to a vCenter VM", sourceTemplateID))
		}
	case models.TemplateSourceCloneVCenter:
		if payload.SourceRef == "" {
			return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("source_ref required for source_type=%s", payload.SourceType))
		}
		sourceMoref = payload.SourceRef
	case models.TemplateSourceISO:
		// ISO installs diverge completely from the clone flow (create a blank
		// VM, optionally attach a generated seed ISO, run the installer, then
		// wait on a *long* deadline), so they get their own dependency-injected
		// core rather than falling through to the clone steps below. It does its
		// own error-state marking, so we return its result verbatim.
		return provisionTemplateFromISO(ctx, p.vc, p.db, p.pipeline, p.logger,
			func(step, message string) { p.publishProgress(job.ID, step, message) }, payload)
	default:
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("unknown source_type %q (must be one of clone_template, clone_vcenter, iso)", payload.SourceType))
	}

	// Step 1: clone (idempotent — returns existing moref if name collision)
	//
	// Acquire a per-source-VM lock so two concurrent clones from the same
	// source cannot overlap.  The most plausible trigger for the transient
	// "virtual disk is either corrupted or not a supported format" fault is
	// concurrent vSphere inventory operations against the same source VM.
	// The lock is in-process because the provision-worker runs as a single
	// replica (values.yaml replicaCount.worker: 1).
	p.publishProgress(job.ID, "create_vm", fmt.Sprintf("Cloning source VM %s → %s", sourceMoref, payload.VMName))
	releaseLock := p.acquireCloneLock(sourceMoref)
	moref, err := p.vc.CloneTemplateSourceVM(ctx, vcenter.TemplateCloneParams{
		SourceMoref: sourceMoref,
		VMName:      payload.VMName,
		FolderPath:  payload.FolderPath,
		Network:     payload.StagingNetwork,
		VCPUs:       payload.VCPUs,
		RAMmb:       payload.RAMmb,
	})
	releaseLock()
	if err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("clone source VM: %w", err))
	}

	// Step 2: persist the moref so we can resume / cancel / generalize later
	if err := p.db.SetTemplateVCenterVM(ctx, payload.TemplateID, moref); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("record vCenter VM ID: %w", err))
	}

	// Step 3: attach NIC to staging network (idempotent if already there)
	p.publishProgress(job.ID, "attach_network", fmt.Sprintf("Attaching NIC to %s", payload.StagingNetwork))
	if err := p.vc.AttachNetworkAdapter(ctx, moref, payload.StagingNetwork); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("attach NIC: %w", err))
	}

	// Step 4: power on
	p.publishProgress(job.ID, "power_on", "Powering on VM")
	if err := p.vc.PowerOnVM(ctx, moref); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("power on: %w", err))
	}

	// Step 5: wait for VMware Tools to come up. This is an ordinary first boot;
	// see cloneFirstBootToolsTimeout for why the deadline is what it is.
	p.publishProgress(job.ID, "wait_tools",
		fmt.Sprintf("Waiting for VMware Tools (up to %s)", cloneFirstBootToolsTimeout))
	if err := p.vc.WaitForTools(ctx, moref, cloneFirstBootToolsTimeout); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("wait for VMware Tools: %w — the VM powered on but never reported tools. "+
				"Either the guest is still booting (rare at this deadline), or open-vm-tools/VMware Tools "+
				"is not installed and enabled on the source VM", err))
	}

	// Step 6: advance to 'configuring' so the instructor can start setup
	p.publishProgress(job.ID, "update_state", "Marking template as configuring")
	if err := p.transitionTemplate(ctx, payload.TemplateID, models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("advance to configuring: %w", err))
	}

	return nil
}

// isoInstallToolsTimeout bounds how long the unattended-install path waits for
// the installer to finish. Unlike a clone (which comes up in a couple of
// minutes), an OS install runs the whole installer end to end — partitioning,
// package unpack, first boot — so it needs a generous ceiling. A clone-length
// 5-minute wait would spuriously fail every automated install.
const isoInstallToolsTimeout = 60 * time.Minute

// isoInstalledBootTimeout bounds the *second* wait: after the install finishes
// and the CD-ROMs are detached, the VM is booted off its new disk and we wait
// for that system's VMware Tools. This is an ordinary first boot, so it gets an
// ordinary deadline rather than the install-length one.
const isoInstalledBootTimeout = 15 * time.Minute

// runningVMToolsCheckTimeout bounds the tools *sanity check* at the start of
// generalize. Unlike the first-boot waits, this VM has been powered on and
// actively configured by the instructor for some time, so tools are either
// already running or something is genuinely wrong. A short deadline is correct
// here: it fails fast instead of stalling a job for a quarter of an hour on a
// VM that will never report. Do not raise this to match the first-boot
// deadlines — they answer a different question.
const runningVMToolsCheckTimeout = 30 * time.Second

// cloneFirstBootToolsTimeout bounds how long we wait for VMware Tools after
// powering on a freshly cloned VM — both when provisioning a template and when
// smoke-testing one during verify.
//
// This is the *same class of wait* as isoInstalledBootTimeout (an ordinary
// first boot), so it gets the same deadline for the same reason. It was five
// minutes until 2026-08-04, when provisioning a clone of `Ubuntu 24.04 Server`
// (tpl-ubuntu-aacc6a, vm-13702) failed with
//
//	timed out after 5m0s waiting for VMware Tools ... (current status: "guestToolsNotRunning")
//
// and the VM was found reporting guestToolsRunning shortly afterwards. The
// template was healthy; the deadline was wrong, and the error told the
// instructor to install open-vm-tools on a VM that already had it.
//
// Five minutes is not a safe first-boot budget here: guest customization
// reboots the VM, so tools legitimately come up, disappear and come back within
// this window, and the clone sources sit on NFS-backed storage where that
// sequence routinely exceeds five minutes. Waiting longer costs nothing on the
// happy path — WaitForTools returns as soon as tools report — while a short
// deadline turns a working template into a hard failure.
const cloneFirstBootToolsTimeout = 15 * time.Minute

// isoProvisionVCenter is the vCenter subset the iso template-provision path
// needs. The real *vcenter.Client satisfies it (asserted below), so production
// wiring stays a plain method call while tests inject a fake without a govmomi
// simulator.
type isoProvisionVCenter interface {
	UploadToDatastore(ctx context.Context, datastore, remotePath string, r io.Reader, size int64, progress func(sent int64)) error
	CreateBlankVM(ctx context.Context, p vcenter.BlankVMParams) (string, error)
	PowerOnVM(ctx context.Context, moref string) error
	// WaitForPowerOff, not WaitForTools, is the unattended install's completion
	// signal — see the long comment at the call site.
	WaitForPowerOff(ctx context.Context, moref string, timeout time.Duration) error
	WaitForTools(ctx context.Context, moref string, timeout time.Duration) error
	DetachCDROMs(ctx context.Context, moref string) error
}

// isoProvisionDB is the database subset the iso template-provision path needs.
type isoProvisionDB interface {
	GetTemplateByID(ctx context.Context, id uuid.UUID) (*models.Template, error)
	SetTemplateVCenterVM(ctx context.Context, id uuid.UUID, vcenterVMID string) error
	UpdateTemplateLifecycleState(ctx context.Context, id uuid.UUID, from, to string) error
}

// Compile-time proof the real clients satisfy the narrow seams, so the
// production delegation in ProvisionTemplate keeps type-checking.
var (
	_ isoProvisionVCenter = (*vcenter.Client)(nil)
	_ isoProvisionDB      = (*database.Queries)(nil)
)

// provisionTemplateFromISO is the dependency-injected core for
// source_type=iso. It creates a blank VM that boots the installer ISO named by
// payload.SourceRef, optionally builds and attaches a seed ISO that drives an
// unattended install, powers on, and advances the template to 'configuring'.
//
// Contract (each bullet is covered by template_jobs_test.go):
//   - SourceRef MUST parse as a "[datastore] path" reference. A malformed ref
//     is caught BEFORE any VM is created, so a typo never orphans a shell.
//   - unattend_mode manual/empty skips the seed build AND WaitForTools (the OS
//     is not installed yet, so Tools can never appear); it parks the template
//     in 'configuring' for a hands-on console install.
//   - Any other unattend_mode builds a seed ISO via unattend.BuildSeedISO and
//     uploads it next to the installer. If the builder reports
//     ErrPreseedRequiresRemaster (debian_preseed cannot be seeded from a second
//     CD in pure Go) the job FAILS LOUDLY telling the operator to pick
//     unattend_mode=manual — it never silently downgrades to a 60-minute wait
//     for an automated install that was never going to run.
//   - On the unattended path it waits isoInstallToolsTimeout for Tools, then
//     DetachCDROMs so the finished template holds no ISO lock on the datastore.
//
// It marks the template 'error' (via the injected db) on any failure and
// returns the cause, so the caller returns its result verbatim.
func provisionTemplateFromISO(
	ctx context.Context,
	vc isoProvisionVCenter,
	db isoProvisionDB,
	metrics pipelineMetricsSink,
	logger *slog.Logger,
	progress func(step, message string),
	payload TemplateProvisionPayload,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	prog := func(step, message string) {
		if progress != nil {
			progress(step, message)
		}
	}
	markErr := func(cause error) error {
		return markTemplateErrorViaDB(ctx, db, metrics, logger, payload.TemplateID, cause)
	}

	// Validate the installer reference up front. ParseDatastorePath is strict
	// and returns actionable text; doing it here guarantees a bad ref never
	// reaches CreateBlankVM (so no orphaned VM) and surfaces the fix to the
	// operator instead of an opaque vCenter fault.
	isoDatastore, isoRemote, err := vcenter.ParseDatastorePath(payload.SourceRef)
	if err != nil {
		return markErr(fmt.Errorf(
			"source_ref %q is not a valid installer ISO datastore path for source_type=iso (want \"[datastore] path/to/installer.iso\"): %w",
			payload.SourceRef, err))
	}

	// Decide whether this is an automated install. manual/empty means the
	// operator installs the OS by hand over the console.
	mode := strings.TrimSpace(payload.UnattendMode)
	unattended := mode != "" && mode != models.UnattendModeManual

	var seedISOPath string
	if unattended {
		spec, serr := unattendSpecFromPayload(payload)
		if serr != nil {
			return markErr(fmt.Errorf("parse unattend_config for unattend_mode=%s: %w", mode, serr))
		}

		prog("build_seed", fmt.Sprintf("Building %s seed ISO", mode))
		seedName, seedData, berr := unattend.BuildSeedISO(spec)
		if berr != nil {
			// debian_preseed has no standalone seed ISO: debian-installer will
			// not read a preseed from a second CD, and this codebase cannot
			// remaster a bootable El Torito installer ISO in pure Go. Refuse
			// LOUDLY rather than silently degrade to a manual install — a silent
			// downgrade makes the operator wait isoInstallToolsTimeout for an
			// automation that was never going to run.
			if errors.Is(berr, unattend.ErrPreseedRequiresRemaster) {
				return markErr(fmt.Errorf(
					"unattend_mode=%s cannot be seeded in pure Go (debian-installer will not read a preseed from a second CD, and Crucible does not remaster the installer ISO here); set unattend_mode=manual and drive the installer over the VM console, or use an Ubuntu (cloudinit_cidata) / Windows (windows_autounattend) source instead: %w",
					mode, berr))
			}
			return markErr(fmt.Errorf("build seed ISO for unattend_mode=%s: %w", mode, berr))
		}

		// Land the seed alongside the installer on the same datastore so both
		// mount from one place; namespace it by VM name to avoid collisions
		// between concurrent template builds sharing an ISO folder.
		seedRemote := path.Join(path.Dir(isoRemote), payload.VMName+"-"+seedName)
		prog("upload_seed", fmt.Sprintf("Uploading seed ISO to %s", vcenter.DatastorePath(isoDatastore, seedRemote)))
		if uerr := vc.UploadToDatastore(ctx, isoDatastore, seedRemote,
			bytes.NewReader(seedData), int64(len(seedData)), nil); uerr != nil {
			return markErr(fmt.Errorf("upload seed ISO: %w", uerr))
		}
		seedISOPath = vcenter.DatastorePath(isoDatastore, seedRemote)
	}

	// Create the blank VM booting the installer ISO (and the seed as CD-ROM 1
	// when present). CreateBlankVM validates every field before its first
	// round-trip, so a bad DiskGB/GuestID/VCPU value never orphans a shell.
	prog("create_vm", fmt.Sprintf("Creating blank VM %s to install from %s", payload.VMName, payload.SourceRef))
	moref, err := vc.CreateBlankVM(ctx, vcenter.BlankVMParams{
		VMName:      payload.VMName,
		FolderPath:  payload.FolderPath,
		Network:     payload.StagingNetwork,
		GuestID:     payload.GuestID,
		VCPUs:       payload.VCPUs,
		RAMmb:       payload.RAMmb,
		DiskGB:      payload.DiskGB,
		ISOPath:     payload.SourceRef,
		SeedISOPath: seedISOPath,
	})
	if err != nil {
		return markErr(fmt.Errorf("create blank VM: %w", err))
	}

	// Persist the moref (mirrors the clone branch ordering) so a later
	// generalize / cancel / resume can find the VM.
	if err := db.SetTemplateVCenterVM(ctx, payload.TemplateID, moref); err != nil {
		return markErr(fmt.Errorf("record vCenter VM ID: %w", err))
	}

	prog("power_on", "Powering on VM to begin install")
	if err := vc.PowerOnVM(ctx, moref); err != nil {
		return markErr(fmt.Errorf("power on: %w", err))
	}

	if unattended {
		// Wait for the install to finish, NOT for Tools.
		//
		// The Ubuntu live-server ISO runs open-vm-tools in the installer
		// environment and reports Tools ~40s after power-on, with nothing yet
		// written to disk. The previous version of this code took that as
		// "installed", detached the CD-ROMs out from under the running
		// installer, and advanced the template to configuring with an empty
		// disk — a silent success that produced an unusable template.
		//
		// The generated autoinstall sets "shutdown: poweroff", so a power-off
		// is unambiguous: it can only happen once curtin has written the
		// target system.
		prog("wait_install", fmt.Sprintf("Waiting up to %s for the unattended install to finish (the VM powers itself off when done)", isoInstallToolsTimeout))
		if err := vc.WaitForPowerOff(ctx, moref, isoInstallToolsTimeout); err != nil {
			return markErr(fmt.Errorf(
				"wait for unattended install to finish: %w (check the VM console — the autoinstall may have stalled, or the seed was rejected)", err))
		}
		// Drop the CD-ROMs now that the OS is installed: a lingering ISO mount
		// keeps a lock on the datastore file that blocks deleting or replacing
		// the installer/seed later. Doing it while the VM is off also
		// guarantees the next boot comes off the disk, not the installer.
		prog("detach_cdrom", "Detaching installer and seed ISOs")
		if err := vc.DetachCDROMs(ctx, moref); err != nil {
			return markErr(fmt.Errorf("detach CD-ROMs after install: %w", err))
		}
		// Now boot the installed system and wait for its Tools. This is the
		// first point at which "Tools are running" actually means the guest OS
		// is up, and it is what the configuring stage needs in order to run
		// guest commands.
		prog("boot_installed", "Booting the installed system")
		if err := vc.PowerOnVM(ctx, moref); err != nil {
			return markErr(fmt.Errorf("power on installed system: %w", err))
		}
		prog("wait_tools", fmt.Sprintf("Waiting up to %s for the installed system's VMware Tools", isoInstalledBootTimeout))
		if err := vc.WaitForTools(ctx, moref, isoInstalledBootTimeout); err != nil {
			return markErr(fmt.Errorf(
				"wait for VMware Tools after first boot of the installed system: %w (the install completed but the guest did not come up with open-vm-tools running)", err))
		}
	} else {
		// Manual install: the OS is NOT installed yet, so WaitForTools would
		// always time out. Skip it and hand the VM (installer still mounted)
		// to the operator.
		prog("await_manual_install", "Blank VM is powered on with the installer ISO mounted — open the VM console (WebMKS) to install the OS, then run Generalize")
	}

	prog("update_state", "Marking template as configuring")
	if err := transitionTemplateViaDB(ctx, db, metrics, payload.TemplateID,
		models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		return markErr(fmt.Errorf("advance to configuring: %w", err))
	}

	return nil
}

// unattendSpecFromPayload builds the unattend.Spec that drives seed generation.
// The mode is authoritative from payload.UnattendMode; the remaining knobs
// (hostname, credentials, locale, extra packages) come from UnattendConfig,
// which is unmarshalled into the Spec's exported fields. A nil/empty config is
// fine — unattend applies its own defaults (default user "student", etc.).
func unattendSpecFromPayload(payload TemplateProvisionPayload) (unattend.Spec, error) {
	var spec unattend.Spec
	if len(payload.UnattendConfig) > 0 {
		if err := json.Unmarshal(payload.UnattendConfig, &spec); err != nil {
			return unattend.Spec{}, err
		}
	}
	spec.Mode = payload.UnattendMode
	return spec, nil
}

// transitionTemplateViaDB is the db-interface twin of
// (p *Provisioner).transitionTemplate: it gate-checks the move through the pure
// state machine before touching the row, so an illegal transition fails fast.
func transitionTemplateViaDB(ctx context.Context, db isoProvisionDB, metrics pipelineMetricsSink, id uuid.UUID, from, to string) error {
	if err := templates.CanTransition(from, to); err != nil {
		return fmt.Errorf("state machine rejected %s→%s: %w", from, to, err)
	}
	if err := db.UpdateTemplateLifecycleState(ctx, id, from, to); err != nil {
		return err
	}
	if metrics != nil {
		metrics.RecordTemplateTransition(from, to)
	}
	return nil
}

// markTemplateErrorViaDB is the db-interface twin of
// (p *Provisioner).markTemplateError: best-effort move to 'error', returning the
// original cause regardless so the job result surfaces the real problem.
func markTemplateErrorViaDB(ctx context.Context, db isoProvisionDB, metrics pipelineMetricsSink, logger *slog.Logger, id uuid.UUID, cause error) error {
	tmpl, err := db.GetTemplateByID(ctx, id)
	if err != nil || tmpl == nil {
		logger.Warn("could not load template for error transition", "template_id", id, "load_err", err)
		return cause
	}
	if err := templates.CanTransition(tmpl.TemplateState, models.TemplateStateError); err != nil {
		logger.Warn("cannot transition template to error", "template_id", id,
			"current_state", tmpl.TemplateState, "err", err)
		return cause
	}
	from := tmpl.TemplateState
	if err := db.UpdateTemplateLifecycleState(ctx, id, from, models.TemplateStateError); err != nil {
		logger.Warn("error transition failed", "template_id", id, "err", err)
		return cause
	}
	if metrics != nil {
		metrics.RecordTemplateTransition(from, models.TemplateStateError)
	}
	return cause
}

// GeneralizeTemplate implements JobTypeTemplateGeneralize.
//
// Flow:
//  1. Read template; require state == 'generalizing'.
//  2. Verify VMware Tools is still running (no-op if it is).
//  3. RunScriptInGuest with the OS-specific generalize command. Linux
//     uses cloud-init clean + truncate machine-id + remove host keys.
//     Windows uses sysprep /generalize /oobe /shutdown.
//  4. Wait for the VM to power off (the generalize commands shut down
//     the VM as their final step). Poll for up to 10 min.
//  5. Snapshot the powered-off VM as `base-image`.
//  6. Transition template_state → 'ready'.
//  7. Scrub credentials in the job result.
//
// On any failure, set template_state → 'error'.
func (p *Provisioner) GeneralizeTemplate(ctx context.Context, job *models.Job) error {
	var payload TemplateGeneralizePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template_generalize payload: %w", err)
	}
	if payload.TemplateID == uuid.Nil {
		return fmt.Errorf("template_id is required")
	}
	if payload.VMMoref == "" {
		return fmt.Errorf("vm_moref is required")
	}
	if payload.GuestUsername == "" || payload.GuestPassword == "" {
		return fmt.Errorf("guest_username and guest_password are required")
	}
	osType := strings.ToLower(payload.OSType)
	if osType != "linux" && osType != "windows" {
		return fmt.Errorf("os_type must be 'linux' or 'windows', got %q", payload.OSType)
	}
	snapshotName := payload.SnapshotName
	if snapshotName == "" {
		snapshotName = "base-image"
	}

	tmpl, err := p.db.GetTemplateByID(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	if tmpl == nil {
		return fmt.Errorf("template %s not found", payload.TemplateID)
	}
	if tmpl.TemplateState != models.TemplateStateGeneralizing {
		return fmt.Errorf("template %s is in state %q, expected %q",
			payload.TemplateID, tmpl.TemplateState, models.TemplateStateGeneralizing)
	}

	// Step 0 (recovery / idempotency): if the VM is already powered off when
	// this job starts, a *previous* generalize run's sysprep almost certainly
	// completed and shut the guest down AFTER that run's waitForPowerOff
	// deadline elapsed. Sysprep on feature-updated Windows 11 routinely takes
	// 12-20 min to finish its generalize pass before it powers the VM off; a
	// run that gave up at 10 min was marked 'error' even though the guest went
	// on to finish generalizing and shut itself down cleanly a minute or two
	// later. Re-running the full pipeline would then fail immediately at
	// WaitForTools (VM is off) and could never recover. Detect that here and
	// resume at the snapshot step instead of trying to sysprep an
	// already-generalized, powered-off image.
	//
	// In the normal flow the VM is powered ON and running when generalize
	// begins (it was just configured), so this branch is a no-op. A GetVM
	// error is non-fatal here — fall through to the normal path, which will
	// surface any real vCenter problem with better context.
	if props, gerr := p.vc.GetVM(ctx, payload.VMMoref); gerr == nil &&
		props.Runtime.PowerState == "poweredOff" {
		p.logger.Warn("generalize: VM already powered off at job start; assuming a "+
			"prior sysprep completed and resuming at snapshot",
			"template_id", payload.TemplateID, "vm_moref", payload.VMMoref)
		return p.finalizeGeneralizedTemplate(ctx, job, &payload, snapshotName)
	}

	// Step 1: tools sanity check
	p.publishProgress(job.ID, "verify_tools", "Verifying VMware Tools is running")
	if err := p.vc.WaitForTools(ctx, payload.VMMoref, runningVMToolsCheckTimeout); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("VMware Tools not running: %w", err))
	}

	// Step 1a (Windows only): fail-fast preflight BEFORE we invest time in
	// unattend upload / BitLocker / sysprep. Aborts cleanly if the sysprep
	// rearm count is exhausted (a guaranteed brick), warns on pending reboot.
	if osType == "windows" {
		p.publishProgress(job.ID, "preflight", "Running pre-sysprep preflight checks")
		if err := p.vc.WindowsSysprepPreflight(ctx, payload.VMMoref,
			payload.GuestUsername, payload.GuestPassword); err != nil {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("sysprep preflight failed: %w", err))
		}
	}

	// Step 1b (Windows only): upload a fresh unattend.xml *before* sysprep.
	// Sysprep scrubs plaintext passwords from any unattend.xml it processes,
	// so a previously-generalized L2 has a poisoned copy on disk. We always
	// overwrite it with the canonical encoded-password version from the
	// embedded assets so every generalize starts from a known-good file.
	// See windowsUnattendGuestPath docstring for the full bug story.
	if osType == "windows" {
		p.publishProgress(job.ID, "upload_unattend", "Uploading unattend.xml for sysprep")
		if err := p.vc.UploadFileToGuest(ctx, payload.VMMoref,
			payload.GuestUsername, payload.GuestPassword,
			windowsUnattendGuestPath, assets.WindowsUnattendXML); err != nil {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("upload unattend.xml: %w", err))
		}
	}

	// Step 1c (Windows only): ensure BitLocker is fully decrypted before
	// sysprep. A BitLocker-protected volume generalizes fine but every L3
	// clone then fails to boot (TPM-sealed key invalidated by the per-clone
	// hardware change, no recovery password in the OOBE flow) — a "bricked"
	// template the instructor only discovers hours later. Auto-decrypting
	// here removes that sharp edge; it's a no-op when C: is already
	// decrypted or BitLocker isn't present. Decryption of a mostly-empty
	// template disk is typically a few minutes; cap at 30 min.
	if osType == "windows" {
		p.publishProgress(job.ID, "bitlocker_decrypt", "Ensuring BitLocker is fully decrypted (can take several minutes)")
		if err := p.vc.EnsureBitLockerDecrypted(ctx, payload.VMMoref,
			payload.GuestUsername, payload.GuestPassword, 30*time.Minute); err != nil {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("ensure BitLocker decrypted before sysprep: %w", err))
		}
	}

	// Step 2: run generalize script
	p.publishProgress(job.ID, "run_generalize", fmt.Sprintf("Running %s generalization script", osType))
	runID := job.ID.String()
	script := generalizeScript(osType, runID)
	language := "bash"
	if osType == "windows" {
		language = "powershell"
	}
	_, err = p.vc.RunScriptInGuest(ctx, vcenter.GuestExecRequest{
		VMMoref:       payload.VMMoref,
		GuestUser:     payload.GuestUsername,
		GuestPassword: payload.GuestPassword,
		Language:      language,
		Script:        script,
		Timeout:       3 * time.Minute,
		RunID:         runID,
		ActionSlug:    "template-generalize",
	})
	// On Windows the script's last act is to launch sysprep, which powers the
	// guest off and kills the guest agent mid-call, so RunScriptInGuest
	// routinely returns an error even on a perfectly good run.
	//
	// We used to decide which case we were in by pattern-matching the error
	// string. That cannot work. vCenter reports "The guest operations agent
	// could not be contacted." BOTH when the guest shut itself down as
	// intended AND when VMware Tools never came up at all, so the message
	// carries no information that separates success from failure. It cost a
	// full rebuild cycle to learn that: a generalize run that completed
	// correctly was marked 'error' because its message was not in the hint
	// list, and adding the string would have made the opposite failure
	// (tools never started) silently succeed.
	//
	// Linux no longer has this problem at all: its script does not power the
	// guest off (see generalizeScript), so a real exit code always comes back
	// and the sentinel is still readable. Only Windows needs the salvage
	// logic below.
	if osType == "windows" && err != nil {
		switch confirmed, sErr := generalizeConfirmed(ctx, p.vc, payload.VMMoref, runID,
			generalizeSentinelAttempts, generalizeSentinelInterval); {
		case sErr != nil:
			// We could not reach vCenter to check. Fall back to the old
			// heuristic rather than failing a probably-good template, but say
			// clearly in the log that this outcome is unproven.
			if isExpectedShutdownErr(err) {
				p.logger.Warn("generalize completion UNVERIFIED: could not read sentinel, falling back to error-string heuristic",
					"template_id", payload.TemplateID, "script_err", err, "sentinel_err", sErr)
			} else {
				return p.markTemplateError(ctx, payload.TemplateID,
					fmt.Errorf("generalize script failed and completion could not be verified: %w (sentinel read failed: %v)", err, sErr))
			}
		case confirmed:
			p.logger.Info("generalize script completed (sentinel confirmed) then powered the guest off",
				"template_id", payload.TemplateID, "error", err)
		case isExpectedShutdownErr(err):
			// Looks like a shutdown race but the guest never stamped the
			// sentinel. On Windows that is expected: sysprep is launched
			// fire-and-forget and powers the machine off itself, so there is
			// no opportunity to stamp. On Linux it means the script did not
			// reach its final line.
			if osType == "windows" {
				p.logger.Info("generalize script connection lost mid-call (expected sysprep shutdown race)",
					"template_id", payload.TemplateID, "error", err)
			} else {
				return p.markTemplateError(ctx, payload.TemplateID,
					fmt.Errorf("generalize script did not run to completion: %w "+
						"(no completion sentinel; the guest went down before cleanup finished, "+
						"so machine-id and SSH host keys may still be baked into the template)", err))
			}
		default:
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("generalize script failed before VM shutdown: %w", err))
		}
	}

	// Step 2a (Linux): the script kept the guest alive, so the exit code is a
	// real verdict. Trust it, then corroborate with the sentinel while the
	// guest is still up - which is the only window in which guestinfo written
	// by the guest is readable at all.
	if osType != "windows" {
		if err != nil {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("generalize script failed: %w", err))
		}
		switch confirmed, sErr := generalizeConfirmed(ctx, p.vc, payload.VMMoref, runID,
			generalizeSentinelAttempts, generalizeSentinelInterval); {
		case sErr != nil:
			// vCenter unreadable. The exit code already said the script ran to
			// completion, so do not fail a probably-good template over a
			// missing second opinion - but say so plainly.
			p.logger.Warn("generalize sentinel unreadable; proceeding on exit code alone",
				"template_id", payload.TemplateID, "sentinel_err", sErr)
		case !confirmed:
			// Exit code 0 but the guest never stamped. `set -e` means the
			// stamp is the last thing the script does, so a zero exit without
			// a stamp means the script body did not actually execute in the
			// guest even though guest ops reported success.
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("generalize reported success but the guest never stamped the completion "+
					"sentinel %s, so the cleanup cannot be shown to have run; refusing to publish a "+
					"template that may still carry a baked-in machine-id and SSH host keys",
					generalizeSentinelKey))
		default:
			p.logger.Info("generalize script completed (exit code 0, sentinel confirmed)",
				"template_id", payload.TemplateID)
		}

		// Step 2b (Linux): we power the guest off, rather than the script, so
		// that everything above could be observed first. This call is expected
		// to fail - shutdown kills the guest agent mid-request - and ignoring
		// that error is only safe because completion is already proven.
		p.publishProgress(job.ID, "shutdown_guest", "Shutting the guest down")
		if _, offErr := p.vc.RunScriptInGuest(ctx, vcenter.GuestExecRequest{
			VMMoref:       payload.VMMoref,
			GuestUser:     payload.GuestUsername,
			GuestPassword: payload.GuestPassword,
			Language:      "bash",
			Script:        "sudo shutdown -h now",
			Timeout:       2 * time.Minute,
			RunID:         runID,
			ActionSlug:    "template-generalize-shutdown",
		}); offErr != nil {
			p.logger.Info("shutdown command returned an error (expected: it terminates its own agent)",
				"template_id", payload.TemplateID, "error", offErr)
		}
	}

	// Step 3: wait for shutdown. The generalize command shuts the VM down as
	// its final step, but on Windows that step is sysprep's generalize pass,
	// which on a feature-updated Windows 11 routinely takes 12-20 min before
	// it powers the guest off. A too-short wait here marks the template
	// 'error' while sysprep is still finishing — the guest then powers off a
	// minute later, leaving a successfully-generalized-but-'error' template
	// that only the Step 0 recovery branch can pick back up. Give Windows a
	// generous 25 min; Linux shutdown is near-instant so 10 min is plenty.
	powerOffWait := powerOffTimeout(osType)
	p.publishProgress(job.ID, "wait_shutdown", fmt.Sprintf("Waiting for VM to power off (up to %s)", powerOffWait))
	if err := p.waitForPowerOff(ctx, payload.VMMoref, powerOffWait); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("VM did not power off after generalize: %w (check the guest console)", err))
	}

	return p.finalizeGeneralizedTemplate(ctx, job, &payload, snapshotName)
}

// powerOffTimeout returns how long GeneralizeTemplate waits for the guest to
// power itself off after the generalize command runs. Windows sysprep's
// generalize pass on feature-updated Windows 11 routinely takes 12-20 min, so
// it gets a generous window; Linux `shutdown -h now` powers off in seconds.
func powerOffTimeout(osType string) time.Duration {
	if strings.ToLower(osType) == "windows" {
		return 25 * time.Minute
	}
	return 10 * time.Minute
}

// finalizeGeneralizedTemplate runs the terminal steps shared by the normal
// generalize path and the Step 0 "VM already powered off" recovery path:
// snapshot the powered-off VM as the base image, advance the template to
// 'ready', and scrub credentials from the in-process payload struct so the
// worker's success result doesn't echo them.
func (p *Provisioner) finalizeGeneralizedTemplate(ctx context.Context, job *models.Job, payload *TemplateGeneralizePayload, snapshotName string) error {
	// Step 4: snapshot
	p.publishProgress(job.ID, "create_snapshot", fmt.Sprintf("Snapshotting as %q", snapshotName))
	if _, err := p.vc.CreateVMSnapshot(ctx, payload.VMMoref, snapshotName,
		"Created by Crucible template wizard on "+time.Now().UTC().Format(time.RFC3339)); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("snapshot: %w", err))
	}

	// Step 5: advance to 'ready'
	p.publishProgress(job.ID, "update_state", "Marking template as ready")
	if err := p.transitionTemplate(ctx, payload.TemplateID, models.TemplateStateGeneralizing, models.TemplateStateReady); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("advance to ready: %w", err))
	}

	// Step 6: scrub credentials in this in-process payload struct so
	// when the worker writes the success result, the creds aren't echoed.
	// (The DB payload column still has them; documented above.)
	payload.GuestUsername = "[redacted]"
	payload.GuestPassword = "[redacted]"

	return nil
}

// TemplateVerifyPayload describes the work for a template_verify job. Kept
// minimal — the worker reads OS/network/spec/assign_ip straight off the
// template row so the payload can't drift from the source of truth.
type TemplateVerifyPayload struct {
	TemplateID uuid.UUID `json:"template_id"`
	VMMoref    string    `json:"vm_moref"` // source: the template's base-image VM
}

// runSmokeCheck is the shared smoke-test core reused by both VerifyTemplate
// (publish gate) and RevalidateL1Template (periodic revalidation). It clones
// vmMoref as a throwaway VM, boots it, waits for VMware Tools, optionally
// waits for an IP, and for clone_with_customize templates validates that guest
// customization applied the generated password. The throwaway clone is ALWAYS
// destroyed on return, on every path. publish is called with (slug, message)
// progress pairs; pass nil to suppress events. Returns nil on success or a
// descriptive error identifying which check failed.
func (p *Provisioner) runSmokeCheck(ctx context.Context, tmpl *models.Template, vmMoref string, publish func(slug, msg string)) error {
	if publish == nil {
		publish = func(_, _ string) {}
	}
	osType := strings.ToLower(tmpl.OSType)
	network := tmpl.StagingNetwork
	if network == "" {
		network = "PG-VM-Lab"
	}
	vcpus := int32(tmpl.DefaultVCPUs)
	if vcpus <= 0 {
		vcpus = 2
	}
	ram := int64(tmpl.DefaultRAMMB)
	if ram <= 0 {
		ram = 4096
	}
	smokeName := fmt.Sprintf("smoke-%s-%d", tmpl.ID.String()[:8], time.Now().Unix())
	smokePassword := generatePassword(12)

	// The throwaway clone is ALWAYS destroyed, on every return path. Uses a
	// background context so cleanup still runs if the job context is
	// cancelled. DestroyVM treats an already-deleted VM as success, so a
	// double-destroy (defer + explicit) is harmless.
	var cloneMoref string
	defer func() {
		if cloneMoref == "" {
			return
		}
		if derr := p.vc.DestroyVM(context.Background(), cloneMoref); derr != nil {
			p.logger.Warn("smoke clone cleanup failed (manual cleanup may be needed)",
				"template_id", tmpl.ID, "moref", cloneMoref, "name", smokeName, "error", derr)
		}
	}()

	// Step 1: clone the base-image the same way a pod clone does.
	publish("smoke_clone", fmt.Sprintf("Cloning base-image for smoke test (%s)", smokeName))
	var err error
	cloneMoref, err = p.vc.CloneVM(ctx, vcenter.CloneVMParams{
		TemplateName: vmMoref,
		VMName:       smokeName,
		VCPUs:        vcpus,
		RAMmb:        ram,
		Network:      network,
		OSType:       osType,
		Password:     smokePassword,
	})
	if err != nil {
		return fmt.Errorf("smoke clone failed (template may be unclonable): %w", err)
	}

	// Step 2: power on.
	publish("smoke_power_on", "Powering on smoke-test clone")
	if err := p.vc.PowerOnVM(ctx, cloneMoref); err != nil {
		return fmt.Errorf("smoke clone power-on failed: %w", err)
	}

	// Step 3: wait for VMware Tools — proves the OS actually booted.
	publish("smoke_wait_tools",
		fmt.Sprintf("Waiting for the clone to boot (VMware Tools, up to %s)", cloneFirstBootToolsTimeout))
	if err := p.vc.WaitForTools(ctx, cloneMoref, cloneFirstBootToolsTimeout); err != nil {
		return fmt.Errorf("smoke clone did not boot: VMware Tools never reported within %s "+
			"(image may be bricked — check generalize/sysprep and BitLocker): %w",
			cloneFirstBootToolsTimeout, err)
	}

	// Step 4: wait for an IP (only if this template assigns one) — proves
	// networking + OOBE customization completed, not just that it powered on.
	if tmpl.AssignIP {
		publish("smoke_wait_ip", "Waiting for the clone to get an IP (5 min)")
		ip, err := p.vc.WaitForIP(ctx, cloneMoref, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("smoke clone got no IP within 5m (DHCP/network or OOBE failure): %w", err)
		}
		p.logger.Info("smoke clone got IP", "template_id", tmpl.ID, "ip", ip)
	}

	// Step 4b: for clone_with_customize templates, prove that guest
	// customization actually reset the account password. This is the check
	// that catches the "cloudbase-init disabled in the golden image" class
	// of bug (June-2026 template): the clone boots and even gets an IP, but
	// the Student/student account is still on its bootstrap password because
	// nothing inside the guest consumed the injected guestinfo. Without this
	// gate a broken image sails through to `active` and only fails when a
	// human logs in at L3.
	//
	// The clone was cloned with `smokePassword`, which the provisioner
	// injects via guestinfo for cloudbase-init (Windows) / cloud-init (Linux)
	// to apply. We poll ValidateGuestCredentials with that password: it fails
	// while the account is still on the bootstrap password (and during the
	// customization reboot), and succeeds once the agent has applied it. A
	// timeout means customization never ran → fail the gate.
	if shouldGenerateGuestPassword(tmpl.Kind, osType) {
		guestUser, _ := resolvePodVMCredentials(tmpl.Kind, osType, smokePassword, tmpl)
		publish("smoke_verify_customization",
			"Verifying guest customization applied (account password reset)")
		verr := pollGuestCredentials(ctx, func(c context.Context) error {
			return p.vc.ValidateGuestCredentials(c, cloneMoref, guestUser, smokePassword)
		}, 6*time.Minute, 15*time.Second)
		if verr != nil {
			return fmt.Errorf(
				"guest customization did not apply: the clone booted but the %q account was never switched to its generated password within 6m — cloudbase-init/cloud-init likely isn't running on this image (verify the agent is installed + enabled and its config includes the VMware guestinfo metadata service and the user-data/local-scripts plugin): %w",
				guestUser, verr)
		}
		p.logger.Info("smoke clone customization verified (password reset applied)",
			"template_id", tmpl.ID, "guest_user", guestUser)
	}

	return nil
}

// VerifyTemplate implements JobTypeTemplateVerify — the automated smoke
// test that hard-gates publish.
//
// It clones the freshly-generalized base-image exactly the way a student
// pod clone would (linked clone off the base-image snapshot), boots it,
// and waits for VMware Tools (+ an IP if the template assigns one) to prove
// the image actually comes up. The throwaway clone is always destroyed.
//
// Outcomes:
//   - all checks pass → template_state verifying → active + is_active=true
//     (the template becomes visible to students)
//   - any check fails → verifying → ready (retryable), error surfaced in the
//     job result so the instructor can fix the image and re-publish
//   - DB/state-machine failure → verifying → error via markTemplateError
//
// This catches "bricked image" regressions (unbootable sysprep, no network,
// BitLocker still on) BEFORE a student ever clones the template.
func (p *Provisioner) VerifyTemplate(ctx context.Context, job *models.Job) (err error) {
	if p.pipeline != nil {
		defer func() {
			result := "pass"
			if err != nil {
				result = "fail"
			}
			p.pipeline.RecordTemplateVerify(result)
		}()
	}
	var payload TemplateVerifyPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template_verify payload: %w", err)
	}
	if payload.TemplateID == uuid.Nil {
		return fmt.Errorf("template_id is required")
	}
	if payload.VMMoref == "" {
		return fmt.Errorf("vm_moref is required")
	}

	tmpl, err := p.db.GetTemplateByID(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	if tmpl == nil {
		return fmt.Errorf("template %s not found", payload.TemplateID)
	}
	if tmpl.TemplateState != models.TemplateStateVerifying {
		return fmt.Errorf("template %s is in state %q, expected %q",
			payload.TemplateID, tmpl.TemplateState, models.TemplateStateVerifying)
	}

	// Run the smoke check. On failure, move the template back to 'ready' so
	// the instructor can fix the image and re-publish.
	if checkErr := p.runSmokeCheck(ctx, tmpl, payload.VMMoref, func(slug, msg string) {
		p.publishProgress(job.ID, slug, msg)
	}); checkErr != nil {
		return p.verifyFailedToReady(ctx, tmpl.ID, checkErr)
	}

	// All checks passed — promote to active and make it visible.
	p.publishProgress(job.ID, "publish", "Smoke test passed — publishing template")
	if err := p.transitionTemplate(ctx, tmpl.ID, models.TemplateStateVerifying, models.TemplateStateActive); err != nil {
		return p.markTemplateError(ctx, tmpl.ID, fmt.Errorf("promote verified template to active: %w", err))
	}
	if err := p.db.SetTemplateActive(ctx, tmpl.ID, true); err != nil {
		// The state is already 'active'; failing to flip is_active means the
		// template won't show to students. Surface loudly rather than
		// silently ship a published-but-hidden template.
		return fmt.Errorf("template promoted to active but failed to set is_active=true (unpublish/republish to fix): %w", err)
	}

	return nil
}

// TemplateRevalidatePayload is the payload for a JobTypeTemplateRevalidate job.
// Manual L1 templates may enqueue this without a VM moref; RevalidateL1Template
// resolves template.VCenterTemplate by name at run time when needed.
type TemplateRevalidatePayload struct {
	TemplateID uuid.UUID `json:"template_id"`
	// VMMoref is the base-image VM's MoRef when already known. When empty,
	// the job resolves the template's vcenter_template name at run time.
	VMMoref string `json:"vm_moref"`
}

// revalidateL1CoreDB is the narrow DB surface used by revalidateL1TemplateCore.
//
// SetTemplateActive is listed explicitly so that tests can spy on it and
// confirm it is NEVER called (alert-only policy: a failed revalidation must
// not unpublish the template). Production code inside revalidateL1TemplateCore
// deliberately never invokes SetTemplateActive.
type revalidateL1CoreDB interface {
	SetTemplateValidationState(ctx context.Context, id uuid.UUID, result string, at time.Time) error
	SetTemplateActive(ctx context.Context, id uuid.UUID, active bool) error
}

// revalidateL1CorePipeline is the narrow metrics surface for revalidateL1TemplateCore.
//
// SetTemplateLastValidated is included because the L1 trust reconciler only
// refreshes that gauge on its own (weekly) tick. Without a push here, a
// template that has just been validated successfully keeps reporting its
// pre-validation timestamp — 0 for a never-validated template — until the next
// reconcile, so CrucibleTemplateValidationStale fires for up to a full interval
// *after* a passing run. Pushing on completion keeps the gauge in step with
// templates.last_validated_at, which this function writes a few lines below.
type revalidateL1CorePipeline interface {
	RecordTemplateValidation(templateID, result string)
	SetTemplateLastValidated(templateID string, unixSec float64)
}

// revalidateL1TemplateCore records the outcome of a completed smoke check for
// an L1 template. It is a pure function (no vCenter dependency) so tests can
// inject fakes for both the DB and metrics and verify the alert-only policy.
//
// INVARIANT: this function NEVER calls db.SetTemplateActive — revalidation
// failures must not change template visibility. Tests verify this by
// injecting a spy that fails the test if SetTemplateActive is called.
func revalidateL1TemplateCore(
	ctx context.Context,
	db revalidateL1CoreDB,
	pipeline revalidateL1CorePipeline,
	logger *slog.Logger,
	tmpl *models.Template,
	checkErr error,
) {
	now := time.Now()
	result := "pass"
	if checkErr != nil {
		raw := checkErr.Error()
		const maxLen = 512
		if len(raw) > maxLen {
			raw = raw[:maxLen]
		}
		result = "fail: " + raw
		if logger != nil {
			logger.Error("l1 revalidation failed (alert only — template remains published)",
				"template_id", tmpl.ID, "name", tmpl.Name, "error", checkErr)
		}
	}

	if dbErr := db.SetTemplateValidationState(ctx, tmpl.ID, result, now); dbErr != nil {
		if logger != nil {
			logger.Warn("revalidation: failed to persist validation state",
				"template_id", tmpl.ID, "error", dbErr)
		}
	}
	if pipeline != nil {
		metricResult := "pass"
		if checkErr != nil {
			metricResult = "fail"
		}
		pipeline.RecordTemplateValidation(tmpl.ID.String(), metricResult)

		// Mirror templates.last_validated_at, which is written above for BOTH
		// outcomes. The staleness alert means "nobody has checked this
		// template recently", not "the check failed" — failures are carried by
		// RecordTemplateValidation's result label. Advancing the gauge on a
		// failed run therefore avoids double-alerting on one fault while still
		// letting the staleness alert catch a revalidation that stops running.
		pipeline.SetTemplateLastValidated(tmpl.ID.String(), float64(now.Unix()))
	}
}

type revalidateL1TemplateDB interface {
	revalidateL1CoreDB
	GetTemplateByID(ctx context.Context, id uuid.UUID) (*models.Template, error)
}

type revalidateL1TemplateVCenter interface {
	ResolveVMByName(ctx context.Context, name string) (string, error)
}

type revalidateL1TemplateSmokeCheck func(ctx context.Context, tmpl *models.Template, vmMoref string, publish func(slug, msg string)) error

var (
	_ revalidateL1TemplateDB      = (*database.Queries)(nil)
	_ revalidateL1TemplateVCenter = (*vcenter.Client)(nil)
)

// revalidateL1TemplateJob is the dependency-injected core for
// JobTypeTemplateRevalidate. It loads the template first, then resolves the
// source VM by name only when the payload did not already carry a MoRef.
func revalidateL1TemplateJob(
	ctx context.Context,
	db revalidateL1TemplateDB,
	vc revalidateL1TemplateVCenter,
	pipeline revalidateL1CorePipeline,
	logger *slog.Logger,
	job *models.Job,
	publish func(step, message string),
	runSmokeCheck revalidateL1TemplateSmokeCheck,
) error {
	if logger == nil {
		logger = slog.Default()
	}
	if publish == nil {
		publish = func(string, string) {}
	}

	var payload TemplateRevalidatePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse template_revalidate payload: %w", err)
	}
	if payload.TemplateID == uuid.Nil {
		return fmt.Errorf("template_id is required")
	}

	tmpl, err := db.GetTemplateByID(ctx, payload.TemplateID)
	if err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	if tmpl == nil {
		return fmt.Errorf("template %s not found", payload.TemplateID)
	}

	vmMoref := payload.VMMoref
	if vmMoref == "" {
		sourceVM := tmpl.VCenterTemplate
		if sourceVM == "" {
			return fmt.Errorf("template %q (%s) has no vm_moref in the job payload and no vcenter_template to resolve; cannot revalidate L1", tmpl.Name, tmpl.ID)
		}
		resolved, rerr := vc.ResolveVMByName(ctx, sourceVM)
		if rerr != nil {
			return fmt.Errorf("resolve source VM %q for template %q (%s): %w; the source VM may have been renamed or deleted in vCenter", sourceVM, tmpl.Name, tmpl.ID, rerr)
		}
		if resolved == "" {
			return fmt.Errorf("resolve source VM %q for template %q (%s) returned an empty moref; the source VM may have been renamed or deleted in vCenter", sourceVM, tmpl.Name, tmpl.ID)
		}
		vmMoref = resolved
		logger.Info("resolved l1 template source VM by name",
			"template_id", tmpl.ID, "name", tmpl.Name, "vcenter_template", sourceVM, "vm_moref", vmMoref)
	}

	checkErr := runSmokeCheck(ctx, tmpl, vmMoref, publish)

	revalidateL1TemplateCore(ctx, db, pipeline, logger, tmpl, checkErr)
	return checkErr
}

// RevalidateL1Template implements JobTypeTemplateRevalidate — the periodic
// smoke-clone health check for already-published L1-tier templates.
//
// Unlike VerifyTemplate (which gates initial publish), this handler:
//   - does NOT check or modify template_state
//   - does NOT touch is_active — the template STAYS published on failure
//
// When the job payload does not already carry a moref, it resolves the source
// VM by the template's vcenter_template name at run time so legacy/manual L1
// templates can still be revalidated.
//
// On failure it records the result in last_validation_result and emits the
// crucible_template_validation_total counter so an alert fires. The job is
// marked failed (for observability) but the template remains live.
func (p *Provisioner) RevalidateL1Template(ctx context.Context, job *models.Job) error {
	return revalidateL1TemplateJob(ctx, p.db, p.vc, p.pipeline, p.logger, job,
		func(slug, msg string) { p.publishProgress(job.ID, slug, msg) },
		p.runSmokeCheck,
	)
}

// verifyFailedToReady is the smoke-test failure path: it moves the template
// back to `ready` (a safe, retryable state) so the instructor can fix the
// image and re-publish, and returns the cause so the worker records it in
// the job result. Best-effort on the state move — if it can't reach ready
// we still surface the original cause.
func (p *Provisioner) verifyFailedToReady(ctx context.Context, id uuid.UUID, cause error) error {
	if err := templates.CanTransition(models.TemplateStateVerifying, models.TemplateStateReady); err == nil {
		if uerr := p.db.UpdateTemplateLifecycleState(ctx, id, models.TemplateStateVerifying, models.TemplateStateReady); uerr != nil {
			p.logger.Warn("verify: failed to move template back to ready after smoke failure",
				"template_id", id, "error", uerr)
		} else if p.pipeline != nil {
			p.pipeline.RecordTemplateTransition(models.TemplateStateVerifying, models.TemplateStateReady)
		}
	}
	return cause
}

// pollGuestCredentials repeatedly calls validate until it returns nil
// (credentials accepted by the guest) or the timeout elapses, sleeping
// `interval` between attempts. Transient errors — guest not ready, VMware
// Tools mid-reboot, or the account still on its bootstrap password while
// cloudbase-init/cloud-init is still running — are expected and swallowed
// until the deadline. On timeout it returns the last error seen so the
// caller can surface a useful cause.
//
// Pure (no vCenter/DB access of its own): the caller injects `validate`,
// which keeps the smoke-gate timing logic unit-testable without a live
// guest. Honors context cancellation between attempts.
func pollGuestCredentials(ctx context.Context, validate func(context.Context) error, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := validate(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			if lastErr == nil {
				lastErr = context.DeadlineExceeded
			}
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// before calling UpdateTemplateLifecycleState, so an illegal transition
// fails fast with a clear error instead of going through the DB.
func (p *Provisioner) transitionTemplate(ctx context.Context, id uuid.UUID, from, to string) error {
	if err := templates.CanTransition(from, to); err != nil {
		return fmt.Errorf("state machine rejected %s→%s: %w", from, to, err)
	}
	if err := p.db.UpdateTemplateLifecycleState(ctx, id, from, to); err != nil {
		return err
	}
	if p.pipeline != nil {
		p.pipeline.RecordTemplateTransition(from, to)
	}
	return nil
}

// markTemplateError moves the template to the error state and wraps the
// underlying cause for the job result. Best-effort: if the state move
// itself fails (e.g. the row is in a state that can't go to 'error'),
// log it and surface the original error.
func (p *Provisioner) markTemplateError(ctx context.Context, id uuid.UUID, cause error) error {
	tmpl, err := p.db.GetTemplateByID(ctx, id)
	if err != nil || tmpl == nil {
		p.logger.Warn("could not load template for error transition", "template_id", id, "load_err", err)
		return cause
	}
	if err := templates.CanTransition(tmpl.TemplateState, models.TemplateStateError); err != nil {
		p.logger.Warn("cannot transition template to error", "template_id", id,
			"current_state", tmpl.TemplateState, "err", err)
		return cause
	}
	if err := p.db.UpdateTemplateLifecycleState(ctx, id, tmpl.TemplateState, models.TemplateStateError); err != nil {
		p.logger.Warn("error transition failed", "template_id", id, "err", err)
		return cause
	}
	if p.pipeline != nil {
		p.pipeline.RecordTemplateTransition(tmpl.TemplateState, models.TemplateStateError)
	}
	return cause
}

// waitForPowerOff polls the VM's runtime.powerState until it reports
// poweredOff or the deadline elapses. Generalize scripts shut down the
// VM as their final step; we need to wait for that asynchronously.
func (p *Provisioner) waitForPowerOff(ctx context.Context, moref string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		props, err := p.vc.GetVM(ctx, moref)
		if err != nil {
			return fmt.Errorf("read VM state: %w", err)
		}
		if props.Runtime.PowerState == "poweredOff" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("VM still %s after %s", props.Runtime.PowerState, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

// generalizeScript returns the OS-specific generalization script body
// (no shebang — RunScriptInGuest wraps it in bash/powershell based on
// Language). Linux: cloud-init clean + machine-id reset + SSH host key
// removal + bash history wipe + shutdown. Windows: sysprep /generalize
// /oobe /shutdown via PowerShell.
//
// These scripts assume the guest user has passwordless sudo (Linux) or
// is an Administrator (Windows). The wizard UI documents this requirement.
// generalizeSentinelKey is the guestinfo variable the Linux generalize script
// stamps with the job ID as its final act before shutting the guest down.
//
// It exists because "the VM powered off" is NOT proof that generalize
// succeeded. A VM is also powered off when the script died halfway, when
// someone hit power-off in vCenter, or when the host evacuated it. Those cases
// leave a template that looks generalized and is not: /etc/machine-id still
// populated and the original SSH host keys still on disk, which every clone
// then shares. That failure is invisible on one clone and only shows up when a
// second student's pod collides with the first.
//
// The value is the job ID rather than a constant so a sentinel left behind by
// an EARLIER generalize attempt cannot be mistaken for this one's.
const generalizeSentinelKey = "guestinfo.crucible.generalize.job"

// generalizeScript returns the OS-specific generalization script. runID is
// stamped into the Linux sentinel; see generalizeSentinelKey.
func generalizeScript(osType, runID string) string {
	if osType == "windows" {
		// PowerShell. Start-Process so PowerShell doesn't wait for
		// sysprep (sysprep will kill the parent session as part of
		// shutdown). -Wait:$false means PS returns immediately and
		// the worker's waitForPowerOff catches the resulting power-off.
		//
		// Before launching sysprep we best-effort DISABLE Windows
		// "reserved storage". On feature-updated Windows 11, sysprep
		// /generalize otherwise fails with 0x800F0975
		// ("SYSPRP Sysprep_Generalize_Windows... reserved storage")
		// and — because we launch sysprep fire-and-forget — that
		// failure is invisible: sysprep exits, the VM never powers
		// off, and the worker's waitForPowerOff just times out after
		// 10 minutes with a misleading "VM did not power off" error.
		// Flipping ReserveManager\ActiveScenario + TiAttemptedInitialization
		// to 0 and running `dism /Set-ReservedStorageState /State:Disabled`
		// clears it with no reboot required. All of it is wrapped so it
		// is a harmless no-op on Windows builds without reserved storage.
		//
		// /unattend: points sysprep at the file we uploaded in Step 1b
		// so the OOBE pass on the *next* boot of any clone follows
		// our script (creates Student, runs FirstLogonCommands,
		// hands off to cloudbase-init for per-pod password injection).
		// Without it sysprep ignores our file and the clone drops into
		// interactive OOBE.
		return strings.Join([]string{
			`$ErrorActionPreference = 'SilentlyContinue'`,
			`$rm = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\ReserveManager'`,
			`if (Test-Path $rm) {`,
			`  Set-ItemProperty -Path $rm -Name 'ActiveScenario' -Value 0 -Type DWord -Force`,
			`  Set-ItemProperty -Path $rm -Name 'TiAttemptedInitialization' -Value 0 -Type DWord -Force`,
			`}`,
			`& dism.exe /Online /Set-ReservedStorageState /State:Disabled | Out-Null`,
			`Start-Process -FilePath "C:\Windows\System32\Sysprep\sysprep.exe" -ArgumentList "/generalize","/oobe","/shutdown","/quiet","/unattend:C:\Windows\Panther\unattend.xml" -NoNewWindow`,
		}, "\n")
	}
	// Linux. Keep as POSIX-safe so it works under dash if /bin/sh is dash.
	//
	// NOTE: this script deliberately does NOT power the guest off. It used to
	// end with `sudo shutdown -h now`, and that broke completion detection
	// outright:
	//
	//   1. Powering off from inside kills the guest agent mid-call, so
	//      RunScriptInGuest never returns an exit code. The worker was left
	//      pattern-matching an error string that carries no information (see
	//      the long comment at the call site).
	//   2. The guestinfo sentinel added to replace that heuristic ALSO could
	//      not survive, because guest-written guestinfo lives only in the
	//      running VM's config.extraConfig and vCenter CLEARS IT ON POWER-OFF.
	//      Verified on real hardware: a marker written via vmware-rpctool was
	//      present in `govc vm.info -e` immediately before a guest-initiated
	//      shutdown and absent immediately after, with nothing else changed.
	//      So the stamp was always erased by the very shutdown it was supposed
	//      to survive, and generalizeConfirmed could never return true on
	//      Linux - it reported "did not run to completion" for runs that had
	//      completed perfectly.
	//
	// Letting the script exit normally makes the whole question disappear:
	// `set -e` plus a real exit code is an unambiguous verdict, the sentinel
	// is readable while the guest is still up as a second signal, and the
	// worker issues the power-off itself afterwards (see GeneralizeTemplate
	// Step 2b). Windows keeps powering itself off because sysprep insists on
	// it, which is why that branch is exempted from the sentinel check.
	//
	// The rpctool line must be the LAST thing, and it must not be swallowed by
	// `|| true`: if we cannot record completion then we do not get to claim
	// completion. `set -e` already aborts before this point on any failed
	// cleanup step, so reaching the stamp means the cleanup ran.
	return strings.Join([]string{
		"set -e",
		"sudo cloud-init clean --logs --seed || true",
		"sudo truncate -s 0 /etc/machine-id",
		"sudo rm -f /var/lib/dbus/machine-id",
		"sudo rm -f /etc/ssh/ssh_host_*",
		"sudo apt-get clean 2>/dev/null || sudo dnf clean all 2>/dev/null || true",
		"history -c 2>/dev/null || true",
		"rm -f ~/.bash_history",
		fmt.Sprintf("sudo vmware-rpctool %q", "info-set "+generalizeSentinelKey+" "+runID),
	}, "\n")
}

// generalizeSentinelReader is the one-method vCenter subset the completion
// check needs. Narrow on purpose: the real *vcenter.Client satisfies it, so
// production stays a plain method call while tests can drive every branch
// (found, absent, stale, unreadable) without a govmomi simulator.
type generalizeSentinelReader interface {
	GetGuestInfoVar(ctx context.Context, moref, key string) (string, error)
}

var _ generalizeSentinelReader = (*vcenter.Client)(nil)

const (
	generalizeSentinelAttempts = 6
	generalizeSentinelInterval = 5 * time.Second
)

// generalizeConfirmed reports whether the guest stamped this run's completion
// sentinel.
//
// A guest write does not appear in config.extraConfig instantly - measured at
// a few seconds on real hardware - so a single immediate read can lose a race
// it would win a moment later. Poll briefly rather than treating the first
// empty read as a verdict.
//
// This must only be called while the guest is still POWERED ON. vCenter clears
// guest-written guestinfo when the VM powers off, so after a shutdown this
// function returns false regardless of what the guest did.
//
// A sentinel carrying a DIFFERENT run ID is treated as absent, not as success:
// that is a leftover from an earlier generalize attempt on the same VM, which
// is exactly the case a constant-valued sentinel would get wrong.
func generalizeConfirmed(ctx context.Context, vc generalizeSentinelReader, moref, runID string, attempts int, interval time.Duration) (bool, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		got, err := vc.GetGuestInfoVar(ctx, moref, generalizeSentinelKey)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			if strings.TrimSpace(got) == runID {
				return true, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
	}
	return false, lastErr
}

// isExpectedShutdownErr reports whether `err` looks like a benign
// "the guest powered off mid-call" failure rather than a real fault.
//
// This is a HINT ONLY and must never be the sole basis for declaring
// generalize successful. Every string below is also produced by genuine
// failures - most importantly "the guest operations agent could not be
// contacted", which vCenter returns both for a guest that shut itself down on
// purpose and for a guest whose VMware Tools never started. Use
// generalizeConfirmed for the actual verdict; this only decides how much
// benefit of the doubt to give when the sentinel cannot be read at all.
func isExpectedShutdownErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	hints := []string{
		"connection reset",
		"guest powered off",
		"tools not running",
		"operation was canceled",
		"guest operations agent could not be contacted",
	}
	for _, h := range hints {
		if strings.Contains(msg, h) {
			return true
		}
	}
	return false
}

// ensure the pgx import is treated as used even if a future refactor
// removes the only consumer above (markTemplateError previously used
// pgx.ErrNoRows). Without this Go would fail with "imported and not used".
// Compile-time guard, costs nothing at runtime.
var _ = pgx.ErrNoRows
var _ = errors.New
