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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/provisioner/assets"
	"github.com/jmal1/selfservice-api/internal/templates"
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
//   - iso: datastore path to the ISO (not yet implemented; returns error)
//
// VMName must be unique within FolderPath. Convention is
// tpl-{template-slug}-{6-char-hex}.
type TemplateProvisionPayload struct {
	TemplateID     uuid.UUID `json:"template_id"`
	SourceType     string    `json:"source_type"`
	SourceRef      string    `json:"source_ref"`
	VMName         string    `json:"vm_name"`
	FolderPath     string    `json:"folder_path,omitempty"`
	StagingNetwork string    `json:"staging_network"`
	VCPUs          int32     `json:"vcpus,omitempty"`
	RAMmb          int64     `json:"ram_mb,omitempty"`
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
	TemplateID     uuid.UUID `json:"template_id"`
	OSType         string    `json:"os_type"`
	GuestUsername  string    `json:"guest_username"`
	GuestPassword  string    `json:"guest_password"`
	VMMoref        string    `json:"vm_moref"`
	SnapshotName   string    `json:"snapshot_name,omitempty"` // defaults to "base-image"
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
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("ISO-based template provisioning is not yet implemented (T4 follow-up); use clone_template or clone_vcenter"))
	default:
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("unknown source_type %q (must be one of clone_template, clone_vcenter, iso)", payload.SourceType))
	}

	// Step 1: clone (idempotent — returns existing moref if name collision)
	p.publishProgress(job.ID, "create_vm", fmt.Sprintf("Cloning source VM %s → %s", sourceMoref, payload.VMName))
	moref, err := p.vc.CloneTemplateSourceVM(ctx, vcenter.TemplateCloneParams{
		SourceMoref: sourceMoref,
		VMName:      payload.VMName,
		FolderPath:  payload.FolderPath,
		Network:     payload.StagingNetwork,
		VCPUs:       payload.VCPUs,
		RAMmb:       payload.RAMmb,
	})
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

	// Step 5: wait for VMware Tools to come up (5 min). If the source VM
	// doesn't have open-vm-tools / VMware Tools installed, this will time
	// out and surface a clear error to the instructor.
	p.publishProgress(job.ID, "wait_tools", "Waiting for VMware Tools (5 min)")
	if err := p.vc.WaitForTools(ctx, moref, 5*time.Minute); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("wait for VMware Tools: %w (install open-vm-tools on the source VM before publishing)", err))
	}

	// Step 6: advance to 'configuring' so the instructor can start setup
	p.publishProgress(job.ID, "update_state", "Marking template as configuring")
	if err := p.transitionTemplate(ctx, payload.TemplateID, models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID, fmt.Errorf("advance to configuring: %w", err))
	}

	return nil
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

	// Step 1: tools sanity check
	p.publishProgress(job.ID, "verify_tools", "Verifying VMware Tools is running")
	if err := p.vc.WaitForTools(ctx, payload.VMMoref, 30*time.Second); err != nil {
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
	script := generalizeScript(osType)
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
		RunID:         job.ID.String(),
		ActionSlug:    "template-generalize",
	})
	// RunScriptInGuest may return an error because the VM shut itself
	// down mid-script — that's the EXPECTED outcome for generalize. Only
	// swallow that flavor of error; everything else (auth failure, missing
	// temp dir, tools crash, script syntax error) is a real failure and
	// must surface immediately so the admin doesn't wait 10 minutes for
	// waitForPowerOff to time out on a VM that was never going to shut
	// down (see bug-generalize-error-swallow / bug-windows-guest-ops-temp-
	// path discovered 2026-06-09 during the student-windows-11-v2 build).
	if err != nil {
		if isExpectedShutdownErr(err) {
			p.logger.Info("generalize script connection lost mid-call (expected shutdown race)",
				"template_id", payload.TemplateID, "error", err)
		} else {
			return p.markTemplateError(ctx, payload.TemplateID,
				fmt.Errorf("generalize script failed before VM shutdown: %w", err))
		}
	}

	// Step 3: wait for shutdown (10 min)
	p.publishProgress(job.ID, "wait_shutdown", "Waiting for VM to power off (10 min)")
	if err := p.waitForPowerOff(ctx, payload.VMMoref, 10*time.Minute); err != nil {
		return p.markTemplateError(ctx, payload.TemplateID,
			fmt.Errorf("VM did not power off after generalize: %w (check the guest console)", err))
	}

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
func (p *Provisioner) VerifyTemplate(ctx context.Context, job *models.Job) error {
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
	p.publishProgress(job.ID, "smoke_clone", fmt.Sprintf("Cloning base-image for smoke test (%s)", smokeName))
	smokePassword := generatePassword(12)
	cloneMoref, err = p.vc.CloneVM(ctx, vcenter.CloneVMParams{
		TemplateName: payload.VMMoref,
		VMName:       smokeName,
		VCPUs:        vcpus,
		RAMmb:        ram,
		Network:      network,
		OSType:       osType,
		Password:     smokePassword,
	})
	if err != nil {
		return p.verifyFailedToReady(ctx, tmpl.ID,
			fmt.Errorf("smoke clone failed (template may be unclonable): %w", err))
	}

	// Step 2: power on.
	p.publishProgress(job.ID, "smoke_power_on", "Powering on smoke-test clone")
	if err := p.vc.PowerOnVM(ctx, cloneMoref); err != nil {
		return p.verifyFailedToReady(ctx, tmpl.ID,
			fmt.Errorf("smoke clone power-on failed: %w", err))
	}

	// Step 3: wait for VMware Tools — proves the OS actually booted.
	p.publishProgress(job.ID, "smoke_wait_tools", "Waiting for the clone to boot (VMware Tools, 5 min)")
	if err := p.vc.WaitForTools(ctx, cloneMoref, 5*time.Minute); err != nil {
		return p.verifyFailedToReady(ctx, tmpl.ID,
			fmt.Errorf("smoke clone did not boot: VMware Tools never reported within 5m (image may be bricked — check generalize/sysprep and BitLocker): %w", err))
	}

	// Step 4: wait for an IP (only if this template assigns one) — proves
	// networking + OOBE customization completed, not just that it powered on.
	if tmpl.AssignIP {
		p.publishProgress(job.ID, "smoke_wait_ip", "Waiting for the clone to get an IP (5 min)")
		ip, err := p.vc.WaitForIP(ctx, cloneMoref, 5*time.Minute)
		if err != nil {
			return p.verifyFailedToReady(ctx, tmpl.ID,
				fmt.Errorf("smoke clone got no IP within 5m (DHCP/network or OOBE failure): %w", err))
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
		p.publishProgress(job.ID, "smoke_verify_customization",
			"Verifying guest customization applied (account password reset)")
		verr := pollGuestCredentials(ctx, func(c context.Context) error {
			return p.vc.ValidateGuestCredentials(c, cloneMoref, guestUser, smokePassword)
		}, 6*time.Minute, 15*time.Second)
		if verr != nil {
			return p.verifyFailedToReady(ctx, tmpl.ID, fmt.Errorf(
				"guest customization did not apply: the clone booted but the %q account was never switched to its generated password within 6m — cloudbase-init/cloud-init likely isn't running on this image (verify the agent is installed + enabled and its config includes the VMware guestinfo metadata service and the user-data/local-scripts plugin): %w",
				guestUser, verr))
		}
		p.logger.Info("smoke clone customization verified (password reset applied)",
			"template_id", tmpl.ID, "guest_user", guestUser)
	}

	// Step 5: all checks passed — promote to active and make it visible.
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
	return p.db.UpdateTemplateLifecycleState(ctx, id, from, to)
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
func generalizeScript(osType string) string {
	if osType == "windows" {
		// PowerShell. Start-Process so PowerShell doesn't wait for
		// sysprep (sysprep will kill the parent session as part of
		// shutdown). -Wait:$false means PS returns immediately and
		// the worker's waitForPowerOff catches the resulting power-off.
		//
		// /unattend: points sysprep at the file we uploaded in Step 1b
		// so the OOBE pass on the *next* boot of any clone follows
		// our script (creates Student, runs FirstLogonCommands,
		// hands off to cloudbase-init for per-pod password injection).
		// Without it sysprep ignores our file and the clone drops into
		// interactive OOBE.
		return `Start-Process -FilePath "C:\Windows\System32\Sysprep\sysprep.exe" -ArgumentList "/generalize","/oobe","/shutdown","/quiet","/unattend:C:\Windows\Panther\unattend.xml" -NoNewWindow`
	}
	// Linux. Keep as POSIX-safe so it works under dash if /bin/sh is dash.
	return strings.Join([]string{
		"set -e",
		"sudo cloud-init clean --logs --seed || true",
		"sudo truncate -s 0 /etc/machine-id",
		"sudo rm -f /var/lib/dbus/machine-id",
		"sudo rm -f /etc/ssh/ssh_host_*",
		"sudo apt-get clean 2>/dev/null || sudo dnf clean all 2>/dev/null || true",
		"history -c 2>/dev/null || true",
		"rm -f ~/.bash_history",
		"sudo shutdown -h now",
	}, "\n")
}

// isExpectedShutdownErr reports whether `err` looks like a benign
// "the guest powered off mid-call" failure rather than a real fault.
// VMware Tools reports a connection reset / "process disappeared"
// flavor of error in this case.
func isExpectedShutdownErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	hints := []string{"connection reset", "guest powered off", "tools not running", "operation was canceled"}
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
