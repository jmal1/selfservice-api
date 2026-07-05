package vcenter

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// GuestExecRequest describes a single script invocation to run inside a
// running guest VM via VMware Tools.
//
// We deliberately accept the script as a string (not a command path) so the
// caller doesn't have to manage temp files inside the guest — RunScriptInGuest
// uploads the script to an OS-appropriate temp dir (/tmp on Linux,
// C:\Users\<GuestUser>\AppData\Local\Temp on Windows), executes it,
// captures stdout/stderr to companion files, and pulls everything back.
//
// Limits we enforce (and why):
//   - Script <= 64 KB: VMware Tools file-transfer over the wire works fine
//     for larger but the synthetic noise from huge scripts isn't worth the
//     debugging cost. Matches scriptvalidator's editor limit.
//   - Output <= 1 MB: prevents a runaway script from filling disk and
//     swamping the engine pod with multi-MB payloads to parse. Truncated
//     with a clear marker if exceeded.
//   - Wall-clock timeout: default 5 minutes, capped at 15 minutes. Anything
//     longer should be an offline batch job, not a synchronous Crucible
//     workflow.
type GuestExecRequest struct {
	VMMoref       string        // managed object reference of the target VM
	GuestUser     string        // guest OS account, e.g. "student"
	GuestPassword string        // guest OS password
	Language      string        // "bash" (Linux) or "powershell" (Windows)
	Script        string        // body of the script (no shebang required)
	Env           []string      // additional FOO=bar pairs for the script process
	Timeout       time.Duration // wall-clock timeout (default 5m, cap 15m)
	RunID         string        // run.id; used for /tmp filename uniqueness
	ActionSlug    string        // action.slug; used for /tmp filename uniqueness
}

// GuestExecResult is the captured output of a single guest-side script run.
//
// `Status` is the same shape the runner uses on its callback so the engine
// can stuff it straight into action_results without translation.
type GuestExecResult struct {
	ExitCode     int           // OS exit code (0 = success)
	Stdout       string        // truncated if it exceeded the output limit
	Stderr       string        // truncated if it exceeded the output limit
	Duration     time.Duration // wall-clock from StartProgram to ListProcesses-exit
	TimedOut     bool          // true if we had to TerminateProcessInGuest
	TruncatedOut bool          // true if stdout/stderr were >1 MB
}

const (
	maxGuestScriptBytes  = 64 * 1024
	maxGuestOutputBytes  = 1 * 1024 * 1024
	defaultGuestTimeout  = 5 * time.Minute
	maxGuestTimeout      = 15 * time.Minute
	guestPollInterval    = 1500 * time.Millisecond
)

// RunScriptInGuest uploads the script to the target VM via VMware Tools,
// starts it, polls for completion (or timeout-and-terminate), and returns
// captured stdout/stderr/exit code.
//
// Process flow inside the guest:
//
//  1. UploadFileToGuest <tmpBase>.sh|.ps1                       (the script)
//  2. UploadFileToGuest <tmpBase>.stdin                         (empty)
//  3. StartProgramInGuest /bin/sh|powershell.exe <script> > .stdout 2> .stderr
//  4. ListProcessesInGuest in a loop until ExitCode != nil OR timeout
//  5. If timeout: TerminateProcessInGuest, mark TimedOut=true
//  6. InitiateFileTransferFromGuest .stdout and .stderr
//  7. Delete <tmpBase>.* files
//
// `<tmpBase>` is OS-appropriate:
//   - Linux  : /tmp/crucible-<runID>-<slug>
//   - Windows: C:\Users\<GuestUser>\AppData\Local\Temp\crucible-<runID>-<slug>
//     (a user-profile temp dir that is writable even when VMware Tools
//     impersonates an Administrators-group user with a UAC-filtered token —
//     C:\tmp does not exist by default and the root of C:\ requires
//     elevation that filtered tokens lack, which historically broke every
//     Windows generalize attempt with a "Permission to perform this
//     operation was denied" ServerFaultCode on the first upload.)
//
// All of the above is wrapped in withRetry so a session expiry between any
// two steps gets handled transparently.
func (c *Client) RunScriptInGuest(ctx context.Context, req GuestExecRequest) (*GuestExecResult, error) {
	if err := validateGuestRequest(req); err != nil {
		return nil, err
	}
	timeout := normalizeGuestTimeout(req.Timeout)

	if err := c.ensureConnected(ctx); err != nil {
		return nil, fmt.Errorf("ensure vCenter connection: %w", err)
	}

	var result *GuestExecResult
	err := c.withRetry(ctx, "run script in guest", func() error {
		r, runErr := c.runScriptInGuestInner(ctx, req, timeout)
		if runErr != nil {
			return runErr
		}
		result = r
		return nil
	})
	return result, err
}

func (c *Client) runScriptInGuestInner(ctx context.Context, req GuestExecRequest, timeout time.Duration) (*GuestExecResult, error) {
	vm, err := c.vmFromMoref(req.VMMoref)
	if err != nil {
		return nil, err
	}

	// Refresh VM state — VMware Tools must be running and the VM powered on
	// or the guest operations manager will reject everything.
	var vmInfo mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"guest", "runtime"}, &vmInfo); err != nil {
		return nil, fmt.Errorf("refresh VM properties for %s: %w", req.VMMoref, err)
	}
	if vmInfo.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
		return nil, fmt.Errorf("VM %s is not powered on (state=%s); cannot run guest script",
			req.VMMoref, vmInfo.Runtime.PowerState)
	}
	if vmInfo.Guest == nil || vmInfo.Guest.ToolsRunningStatus != string(types.VirtualMachineToolsRunningStatusGuestToolsRunning) {
		return nil, fmt.Errorf("VMware Tools not running in guest on VM %s; install/start tools first", req.VMMoref)
	}

	opsMgr := guest.NewOperationsManager(c.client.Client, vm.Reference())
	procMgr, err := opsMgr.ProcessManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("get guest process manager: %w", err)
	}
	fileMgr, err := opsMgr.FileManager(ctx)
	if err != nil {
		return nil, fmt.Errorf("get guest file manager: %w", err)
	}

	auth := &types.NamePasswordAuthentication{
		Username: req.GuestUser,
		Password: req.GuestPassword,
	}

	// Resolve guest paths up front so cleanup can run even on error.
	tmpBase, err := guestTempBase(req.Language, req.GuestUser, req.RunID, req.ActionSlug)
	if err != nil {
		return nil, fmt.Errorf("resolve guest temp path: %w", err)
	}
	scriptPath := tmpBase + scriptSuffix(req.Language)
	stdoutPath := tmpBase + ".stdout"
	stderrPath := tmpBase + ".stderr"

	// Ensure best-effort cleanup of all our files no matter what fails below.
	defer func() {
		// Use a fresh context so cleanup still runs if the caller cancelled.
		cleanCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, p := range []string{scriptPath, stdoutPath, stderrPath} {
			_ = fileMgr.DeleteFile(cleanCtx, auth, p)
		}
	}()

	// Upload script body. Use a transfer URL that we POST to ourselves —
	// VMware Tools accepts a single chunk PUT-like flow via this URL.
	// The URL points at an ESXi host (not vCenter) which serves a VMCA-signed
	// cert that we have no reason to trust separately from the vCenter cert;
	// honor the same Insecure flag the soap client uses.
	httpClient := guestTransferClient(c.config.Insecure)
	if err := uploadGuestFile(ctx, httpClient, fileMgr, auth, scriptPath, []byte(req.Script)); err != nil {
		return nil, fmt.Errorf("upload script to guest: %w", err)
	}

	// Start the program. For bash: `bash -c "<script> > stdout 2> stderr"`
	// is unsafe because the script body might contain quotes; instead we use
	// `bash <scriptPath> > stdout 2> stderr` via /bin/sh -c.
	programPath, programArgs := buildGuestInvocation(req.Language, scriptPath, stdoutPath, stderrPath)
	spec := &types.GuestProgramSpec{
		ProgramPath: programPath,
		Arguments:   programArgs,
		EnvVariables: req.Env,
	}
	startedAt := time.Now()
	pid, err := procMgr.StartProgram(ctx, auth, spec)
	if err != nil {
		return nil, fmt.Errorf("start program in guest: %w", err)
	}

	// Poll for completion.
	deadline := startedAt.Add(timeout)
	result := &GuestExecResult{}
	var exitCode int
	for {
		// Allow cancellation of the outer context to abort mid-loop without
		// orphaning the guest process.
		select {
		case <-ctx.Done():
			_ = procMgr.TerminateProcess(context.Background(), auth, pid)
			return nil, ctx.Err()
		default:
		}

		procs, err := procMgr.ListProcesses(ctx, auth, []int64{pid})
		if err != nil {
			return nil, fmt.Errorf("list guest processes: %w", err)
		}
		if len(procs) == 0 {
			// Process is gone before we got an exit code (very unusual);
			// treat as unknown success-or-failure and read whatever's there.
			break
		}
		p := procs[0]
		if p.EndTime != nil {
			exitCode = int(p.ExitCode)
			result.Duration = p.EndTime.Sub(startedAt)
			break
		}
		if time.Now().After(deadline) {
			// Timeout — terminate and mark.
			_ = procMgr.TerminateProcess(ctx, auth, pid)
			result.TimedOut = true
			result.Duration = time.Since(startedAt)
			exitCode = -1
			break
		}
		select {
		case <-ctx.Done():
			_ = procMgr.TerminateProcess(context.Background(), auth, pid)
			return nil, ctx.Err()
		case <-time.After(guestPollInterval):
		}
	}
	result.ExitCode = exitCode

	// Download stdout and stderr (best-effort — script may have failed
	// before writing them). Reuse httpClient with the same TLS posture
	// as the upload so we don't choke on the ESXi host's VMCA cert.
	stdoutBytes, truncOut, err := downloadGuestFile(ctx, httpClient, fileMgr, auth, stdoutPath, maxGuestOutputBytes)
	if err == nil {
		result.Stdout = string(stdoutBytes)
		result.TruncatedOut = result.TruncatedOut || truncOut
	}
	stderrBytes, truncErr, err := downloadGuestFile(ctx, httpClient, fileMgr, auth, stderrPath, maxGuestOutputBytes)
	if err == nil {
		result.Stderr = string(stderrBytes)
		result.TruncatedOut = result.TruncatedOut || truncErr
	}

	return result, nil
}

// vmFromMoref returns a govmomi object.VirtualMachine for the given moref.
// Centralized so callers don't construct ManagedObjectReferences inline.
func (c *Client) vmFromMoref(moref string) (*object.VirtualMachine, error) {
	if moref == "" {
		return nil, fmt.Errorf("empty VM moref")
	}
	ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: moref}
	return object.NewVirtualMachine(c.client.Client, ref), nil
}

// ValidateGuestCredentials reports whether the given username/password are
// accepted by the guest OS via VMware Tools. Returns nil when the guest
// authenticates the credentials and a non-nil error otherwise — including
// the "credentials rejected" case (InvalidGuestLogin), tools-not-ready, or
// any transport error. Callers that poll (e.g. the template smoke gate
// waiting for cloudbase-init / cloud-init to reset the account password)
// should treat ANY error as "not yet / no" and retry until their own
// deadline; this method deliberately does not retry internally.
//
// It performs no mutation and needs no elevated privileges — it is the
// lightest guest operation available, which is why the smoke gate uses it
// to confirm that guest customization actually switched the account to its
// generated password (rather than a heavier RunScriptInGuest probe that
// would itself depend on the very customization we're trying to verify).
//
// Pre-conditions: VM powered on with VMware Tools running.
func (c *Client) ValidateGuestCredentials(ctx context.Context, moref, guestUser, guestPassword string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return fmt.Errorf("ensure vCenter connection: %w", err)
	}
	vm, err := c.vmFromMoref(moref)
	if err != nil {
		return err
	}
	opsMgr := guest.NewOperationsManager(c.client.Client, vm.Reference())
	authMgr, err := opsMgr.AuthManager(ctx)
	if err != nil {
		return fmt.Errorf("get guest auth manager: %w", err)
	}
	auth := &types.NamePasswordAuthentication{
		Username: guestUser,
		Password: guestPassword,
	}
	if err := authMgr.ValidateCredentials(ctx, auth); err != nil {
		return fmt.Errorf("validate guest credentials for %q: %w", guestUser, err)
	}
	return nil
}

// UploadFileToGuest writes `data` to `guestPath` inside the target VM via
// VMware Tools. Wraps the inner uploadGuestFile helper with connection
// setup so callers outside this file (e.g. the provisioner's
// GeneralizeTemplate step uploading a fresh unattend.xml before sysprep)
// don't have to reach for govmomi primitives.
//
// Pre-conditions: VM must be powered on AND VMware Tools must be running.
// Authentication is interactive-session NamePassword; the guest user must
// have write permission to guestPath (use an administrator account for
// system-protected paths like C:\Windows\Panther\unattend.xml).
//
// Idempotent overwrite: passes overwrite=true to InitiateFileTransferToGuest.
//
// Bounded payload: the data buffer is held entirely in memory, so callers
// should keep files under a few MB. Suitable for unattend.xml, registry
// dumps, small config files; NOT for ISOs.
func (c *Client) UploadFileToGuest(ctx context.Context, moref, guestUser, guestPassword, guestPath string, data []byte) error {
	if err := c.ensureConnected(ctx); err != nil {
		return fmt.Errorf("ensure vCenter connection: %w", err)
	}
	return c.withRetry(ctx, "upload file to guest", func() error {
		vm, err := c.vmFromMoref(moref)
		if err != nil {
			return err
		}
		var vmInfo mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"guest", "runtime"}, &vmInfo); err != nil {
			return fmt.Errorf("refresh VM properties for %s: %w", moref, err)
		}
		if vmInfo.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
			return fmt.Errorf("VM %s is not powered on (state=%s); cannot upload to guest",
				moref, vmInfo.Runtime.PowerState)
		}
		if vmInfo.Guest == nil || vmInfo.Guest.ToolsRunningStatus != string(types.VirtualMachineToolsRunningStatusGuestToolsRunning) {
			return fmt.Errorf("VMware Tools not running in guest on VM %s; install/start tools first", moref)
		}
		opsMgr := guest.NewOperationsManager(c.client.Client, vm.Reference())
		fileMgr, err := opsMgr.FileManager(ctx)
		if err != nil {
			return fmt.Errorf("get guest file manager: %w", err)
		}
		auth := &types.NamePasswordAuthentication{
			Username: guestUser,
			Password: guestPassword,
		}
		httpClient := guestTransferClient(c.config.Insecure)
		return uploadGuestFile(ctx, httpClient, fileMgr, auth, guestPath, data)
	})
}

// EnsureBitLockerDecrypted makes sure the guest's C: volume is fully
// decrypted before a Windows template is sysprepped. Sysprep/generalize
// on a BitLocker-protected volume produces clones that fail to boot: the
// per-clone hardware change invalidates the TPM-sealed key and there is no
// recovery password wired into the OOBE flow, so the "bricked" template
// isn't discovered until someone attempts an L3 clone hours later. This
// helper removes that sharp edge by auto-decrypting instead of relying on
// the instructor to remember `manage-bde -off C:` first.
//
// Behavior (all idempotent, safe to call every generalize run):
//   - No BitLocker cmdlets / no protected volume  → no-op (returns nil).
//   - Already FullyDecrypted                       → no-op.
//   - Encrypted or mid-(en|de)cryption             → issues Disable-BitLocker
//     if not already decrypting, then polls until VolumeStatus ==
//     FullyDecrypted or the timeout elapses.
//
// The guest user must be an Administrator (Disable-BitLocker requires it);
// the wizard already documents that requirement for generalize.
func (c *Client) EnsureBitLockerDecrypted(ctx context.Context, moref, guestUser, guestPassword string, timeout time.Duration) error {
	// Idempotent check+kickoff script. Prints one sentinel line:
	//   BL:DECRYPTED       fully decrypted, safe to sysprep
	//   BL:NONE            BitLocker feature/volume absent, nothing to do
	//   BL:WORKING:<st>:<pct>   still (de|en)crypting; caller should poll
	//   BL:DISABLE_ERR:<msg>    Disable-BitLocker call failed (surfaced in status)
	const script = `$ErrorActionPreference = 'SilentlyContinue'
$v = $null
try { $v = Get-BitLockerVolume -MountPoint 'C:' -ErrorAction Stop } catch { Write-Output 'BL:NONE'; exit 0 }
if ($null -eq $v) { Write-Output 'BL:NONE'; exit 0 }
$status = [string]$v.VolumeStatus
$pct = 0; try { $pct = [int]$v.EncryptionPercentage } catch {}
if ($status -eq 'FullyDecrypted') { Write-Output 'BL:DECRYPTED'; exit 0 }
if ($status -ne 'DecryptionInProgress') {
  try { Disable-BitLocker -MountPoint 'C:' -ErrorAction Stop | Out-Null } catch { Write-Output ('BL:DISABLE_ERR:' + $_.Exception.Message) }
}
Write-Output ('BL:WORKING:' + $status + ':' + $pct)`

	deadline := time.Now().Add(timeout)
	lastStatus := "unknown"
	for {
		res, err := c.RunScriptInGuest(ctx, GuestExecRequest{
			VMMoref:       moref,
			GuestUser:     guestUser,
			GuestPassword: guestPassword,
			Language:      "powershell",
			Script:        script,
			Timeout:       2 * time.Minute,
			RunID:         moref,
			ActionSlug:    "bitlocker-decrypt",
		})
		if err != nil {
			return fmt.Errorf("check BitLocker status on %s: %w", moref, err)
		}
		out := strings.TrimSpace(res.Stdout)
		switch {
		case strings.Contains(out, "BL:DECRYPTED"):
			c.logger.Info("BitLocker fully decrypted", "moref", moref)
			return nil
		case strings.Contains(out, "BL:NONE"):
			c.logger.Info("BitLocker not present on guest; nothing to decrypt", "moref", moref)
			return nil
		}
		lastStatus = out
		c.logger.Info("waiting for BitLocker decryption", "moref", moref, "status", out)
		if time.Now().After(deadline) {
			return fmt.Errorf("C: volume still not fully decrypted after %s (last status: %q)", timeout, lastStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
}

// WindowsSysprepPreflight runs cheap fail-fast checks in a Windows guest
// *before* the generalize path invests any time in unattend upload,
// BitLocker decryption, or the sysprep run itself. It catches the classic
// "everything ran for 20 minutes then sysprep hard-failed and left the
// image unbootable" trap.
//
// Fatal (returns an error, generalize aborts cleanly):
//   - Sysprep rearm count exhausted (RemainingWindowsReArmCount == 0):
//     `sysprep /generalize` WILL fail on a rearm-exhausted image, so there
//     is no point continuing. The instructor must rebuild from an image
//     with rearm budget left.
//
// Advisory (logged, non-fatal):
//   - Pending reboot flags: sysprep usually still succeeds but is more
//     likely to fail; we surface it so a later failure is easier to explain.
//   - Rearm count undetermined (older OS / CIM class missing): we can't
//     assert, so we proceed rather than block a legitimate build.
func (c *Client) WindowsSysprepPreflight(ctx context.Context, moref, guestUser, guestPassword string) error {
	const script = `$ErrorActionPreference = 'SilentlyContinue'
$svc = Get-CimInstance -ClassName SoftwareLicensingService -ErrorAction SilentlyContinue
$rearm = -1
if ($svc -ne $null -and $svc.RemainingWindowsReArmCount -ne $null) { $rearm = [int]$svc.RemainingWindowsReArmCount }
$pending = $false
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') { $pending = $true }
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired') { $pending = $true }
if (Test-Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\PendingFileRenameOperations') { $pending = $true }
Write-Output ('PREFLIGHT:REARM=' + $rearm + ';PENDINGREBOOT=' + $pending)`

	res, err := c.RunScriptInGuest(ctx, GuestExecRequest{
		VMMoref:       moref,
		GuestUser:     guestUser,
		GuestPassword: guestPassword,
		Language:      "powershell",
		Script:        script,
		Timeout:       2 * time.Minute,
		RunID:         moref,
		ActionSlug:    "sysprep-preflight",
	})
	if err != nil {
		// Don't block generalize on a flaky preflight probe — log and continue.
		c.logger.Warn("sysprep preflight probe failed; continuing", "moref", moref, "error", err)
		return nil
	}
	out := strings.TrimSpace(res.Stdout)
	rearm, pending := parseSysprepPreflight(out)
	if pending {
		c.logger.Warn("sysprep preflight: guest reports a pending reboot; sysprep may be less reliable",
			"moref", moref)
	}
	switch {
	case rearm == 0:
		return fmt.Errorf("sysprep rearm count is exhausted (RemainingWindowsReArmCount=0); "+
			"sysprep /generalize will fail and brick the image — rebuild the template from a source "+
			"with rearm budget remaining (preflight output: %q)", out)
	case rearm < 0:
		c.logger.Info("sysprep preflight: rearm count undetermined; proceeding", "moref", moref, "output", out)
	default:
		c.logger.Info("sysprep preflight OK", "moref", moref, "rearm_remaining", rearm, "pending_reboot", pending)
	}
	return nil
}

// parseSysprepPreflight extracts the rearm count and pending-reboot flag
// from the "PREFLIGHT:REARM=<n>;PENDINGREBOOT=<bool>" sentinel line.
// Returns rearm=-1 when it can't be parsed (treated as "undetermined").
func parseSysprepPreflight(out string) (rearm int, pending bool) {
	rearm = -1
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "PREFLIGHT:") {
			continue
		}
		for _, part := range strings.Split(strings.TrimPrefix(line, "PREFLIGHT:"), ";") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key, val := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
			switch key {
			case "REARM":
				if n, perr := strconv.Atoi(val); perr == nil {
					rearm = n
				}
			case "PENDINGREBOOT":
				pending = strings.EqualFold(val, "true")
			}
		}
	}
	return rearm, pending
}

// uploadGuestFile uploads a single file to the guest via the
// InitiateFileTransferToGuest URL. The govmomi API returns a URL that the
// caller must HTTP-PUT bytes to; we wrap that pattern here.
//
// The URL points at the ESXi host the VM lives on (not vCenter). The host
// serves a VMCA-signed cert that, in homelabs and dev clusters, often isn't
// trusted by the worker's CA pool. Pass in an httpClient whose TLS posture
// matches the soap client used to talk to vCenter so the two stay
// consistent — there is no scenario where trusting vCenter and rejecting an
// ESXi host that vCenter owns makes sense.
func uploadGuestFile(ctx context.Context, httpClient *http.Client, fm *guest.FileManager, auth types.BaseGuestAuthentication, path string, data []byte) error {
	attrs := &types.GuestFileAttributes{}
	url, err := fm.InitiateFileTransferToGuest(ctx, auth, path, attrs, int64(len(data)), true)
	if err != nil {
		return fmt.Errorf("initiate upload of %s: %w", path, err)
	}
	url = strings.ReplaceAll(url, "*", "vcenter") // vSphere sometimes returns a wildcard host
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(string(data)))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	req.ContentLength = int64(len(data))
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upload file %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload %s: HTTP %d %s", path, resp.StatusCode, string(body))
	}
	return nil
}

// downloadGuestFile pulls a file from the guest, capped at maxBytes. The
// second return value reports whether the file exceeded the cap. See
// uploadGuestFile for the rationale on the httpClient parameter.
func downloadGuestFile(ctx context.Context, httpClient *http.Client, fm *guest.FileManager, auth types.BaseGuestAuthentication, path string, maxBytes int) ([]byte, bool, error) {
	info, err := fm.InitiateFileTransferFromGuest(ctx, auth, path)
	if err != nil {
		return nil, false, fmt.Errorf("initiate download of %s: %w", path, err)
	}
	url := strings.ReplaceAll(info.Url, "*", "vcenter")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build download request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("download %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("download %s: HTTP %d", path, resp.StatusCode)
	}
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	truncated := false
	if len(data) > maxBytes {
		data = data[:maxBytes]
		truncated = true
	}
	return data, truncated, nil
}

// guestTransferClient returns the HTTP client used for VMware Tools guest
// file transfers. The govmomi soap client is configured to honor a "Insecure"
// flag at connect time (Config.Insecure → InsecureSkipVerify on its TLS
// config). Guest file transfers happen out-of-band over a fresh HTTP client
// pointed at an ESXi host; if the caller's vCenter session is insecure,
// downstream PUTs to ESXi must also be insecure or every transfer will fail
// with x509: certificate signed by unknown authority. Conversely if Insecure
// is false, we DO verify — that matches a hardened deployment where VMCA has
// been added to the worker's trust store.
//
// Timeout is generous (60s) because uploads can be megabytes over slow links.
func guestTransferClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: insecure, //nolint:gosec // honors the same flag as the vCenter soap client
			},
		},
	}
}

func scriptSuffix(language string) string {
	switch strings.ToLower(language) {
	case "powershell", "pwsh":
		return ".ps1"
	default:
		return ".sh"
	}
}

// guestTempBase returns the OS-appropriate temp file base path inside the
// guest (no extension). On Windows we deliberately target the GuestUser's
// own AppData\Local\Temp directory: it is guaranteed to exist for a logged-in
// user, the user owns it (so VMware Tools can read/write/delete files there
// even with a UAC-filtered token from interactive logon), and we don't have
// to chase the surprisingly tight default ACLs on C:\Windows\Temp or
// elevation requirements for C:\.
//
// guestUser is the local account name (no DOMAIN\ prefix supported — vCenter
// guest-ops with local accounts is what Crucible uses for all template ops).
// runID and actionSlug feed filename uniqueness so concurrent jobs don't
// collide.
func guestTempBase(language, guestUser, runID, actionSlug string) (string, error) {
	switch strings.ToLower(language) {
	case "powershell", "pwsh":
		if guestUser == "" {
			return "", fmt.Errorf("guestUser is required to resolve Windows temp path")
		}
		if strings.ContainsAny(guestUser, `\/:*?"<>|`) {
			return "", fmt.Errorf("guestUser %q contains characters invalid in a Windows path", guestUser)
		}
		return fmt.Sprintf(`C:\Users\%s\AppData\Local\Temp\crucible-%s-%s`,
			guestUser, runID, actionSlug), nil
	default:
		return fmt.Sprintf("/tmp/crucible-%s-%s", runID, actionSlug), nil
	}
}

// buildGuestInvocation returns the (programPath, arguments) pair to feed
// StartProgramInGuest so stdout/stderr land in the expected files. The
// shell is told to read the script file (so the body never has to be
// quoted or escaped) and redirect its own streams.
func buildGuestInvocation(language, scriptPath, stdoutPath, stderrPath string) (string, string) {
	switch strings.ToLower(language) {
	case "powershell", "pwsh":
		args := fmt.Sprintf(`-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%s" *> "%s" 2> "%s"`,
			scriptPath, stdoutPath, stderrPath)
		return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, args
	default:
		args := fmt.Sprintf(`-c 'bash "%s" > "%s" 2> "%s"'`, scriptPath, stdoutPath, stderrPath)
		return "/bin/sh", args
	}
}

func validateGuestRequest(req GuestExecRequest) error {
	if req.VMMoref == "" {
		return fmt.Errorf("vm moref required")
	}
	if req.GuestUser == "" || req.GuestPassword == "" {
		return fmt.Errorf("guest credentials required")
	}
	if req.RunID == "" || req.ActionSlug == "" {
		return fmt.Errorf("run ID and action slug required for tmp file uniqueness")
	}
	if len(req.Script) == 0 {
		return fmt.Errorf("script body is empty")
	}
	if len(req.Script) > maxGuestScriptBytes {
		return fmt.Errorf("script is %d bytes, max %d", len(req.Script), maxGuestScriptBytes)
	}
	switch strings.ToLower(req.Language) {
	case "bash", "sh", "shell", "powershell", "pwsh":
		return nil
	default:
		return fmt.Errorf("unsupported guest script language: %q", req.Language)
	}
}

func normalizeGuestTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		return defaultGuestTimeout
	}
	if t > maxGuestTimeout {
		return maxGuestTimeout
	}
	return t
}
