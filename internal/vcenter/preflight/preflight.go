package preflight

// preflight.go — Pre-clone sanity checks for the template wizard.
//
// Preflight checks are synchronous, cheap vCenter read-only queries that run
// before provisioning is enqueued. Their purpose is to surface deterministic
// failures — things that are *knowable before the clone starts* — so the
// instructor sees a plain-language diagnosis and fix immediately rather than
// a raw vCenter fault string five minutes later.
//
// What preflight cannot catch
// ---------------------------
// An intermittent clone fault was investigated (see git history and the
// comment in template_ops.go): the same source VM, cloned with identical
// parameters 68 seconds apart, failed then succeeded. Every static property
// was verified healthy. This is a transient environmental fault, not a
// deterministic one. A separate retry-with-backoff lane addresses that class
// of failure. Do not read a green preflight as "this clone will succeed" —
// read it as "the deterministic preconditions are satisfied."
//
// PF-06 (in-flight task detection) is the one check that *may* reduce the
// frequency of the intermittent fault by avoiding concurrent operations
// against the same source VM. It is a mitigation with unknown effectiveness,
// not a proven cure.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/jmal1/selfservice-api/internal/models"
)

// Result is the structured outcome of a single preflight check.
//
// Using a typed result rather than a bare bool means the UI can render
// every check — passing, failing, and the reason — without parsing error
// strings or building a parallel logging path. Never embed a raw vCenter
// fault in Detail or Fix; rewrite it in terms the instructor can act on.
type Result struct {
	ID       string `json:"id"`       // canonical check ID, e.g. "PF-01"
	Severity string `json:"severity"` // "block" or "warn"
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"` // what was actually observed, in plain language
	Fix      string `json:"fix"`    // what the instructor should do when OK is false (empty when OK is true)
}

// Params carries the non-vCenter inputs that the 11 checks need. Populated
// by the handler from the template row and the configured vcenter settings.
type Params struct {
	// SourceMoref is the vCenter managed-object reference of the source VM,
	// e.g. "vm-1234". Required for clone-based checks (PF-01 … PF-07, PF-11).
	// For clone_template drafts this is the moref the source template's
	// Crucible UUID was resolved to — see templates.ResolveCloneSourceMoref —
	// NOT the raw source_ref (which is a Crucible templates.id UUID).
	SourceMoref string

	// SourceResolveError, when non-empty, means the handler could not resolve
	// the draft's source to a live vCenter moref before running the checks
	// (e.g. a clone_template whose source template row has no vcenter linkage,
	// or a source VM that was deleted). PF-01 reports it as a blocking failure
	// so the instructor sees why the source could not be located.
	SourceResolveError string

	// TargetVMName is the intended name for the staging VM (PF-09).
	TargetVMName string

	// TargetFolderPath is the vCenter inventory path of the templates folder,
	// e.g. "JMAL-Datacenter/vm/Templates" (PF-09).
	TargetFolderPath string

	// DatastoreName is the configured target datastore, e.g. "NAS-vmstore"
	// (PF-03, PF-04, PF-10).
	DatastoreName string

	// StagingPortGroup is the port group for the staging NIC, e.g. "PG-VM-Lab"
	// (PF-11).
	StagingPortGroup string

	// SourceType distinguishes clone-based from ISO sources. One of
	// models.TemplateSource*.
	SourceType string

	// ISORef is the "[datastore] path/file.iso" reference for ISO sources (PF-10).
	ISORef string

	// GuestUsername / GuestPassword are checked for resolvability (PF-08).
	GuestUsername string
	GuestPassword string

	// ConfiguredResourcePoolPaths are the inventory paths from
	// vcenter.Config.ResourcePools, e.g. ["AMD-Cluster/Resources/Student-VMs"].
	// Used by PF-02 to verify the source cluster has a usable pool.
	ConfiguredResourcePoolPaths []string
}

// PreflightVCenter is the vCenter surface the 11 preflight checks need.
// *vcenter.Client satisfies this interface in production (wired in
// internal/vcenter/preflight_methods.go). Tests inject a fakeVCenter.
//
// Every method is a data-fetching wrapper — it returns structured data, not
// a pass/fail decision. The check functions below own the logic.
type PreflightVCenter interface {
	// FetchVMProps retrieves key properties for the given VM moref.
	// Returns a non-nil error if the moref does not resolve to a live VM.
	FetchVMProps(ctx context.Context, moref string) (*mo.VirtualMachine, error)

	// DatastoreInfo returns the mo.Datastore for the named datastore,
	// including its summary (free space) and host mount list.
	DatastoreInfo(ctx context.Context, name string) (*mo.Datastore, error)

	// InFlightTasksForVM returns tasks currently queued or running whose
	// managed entity is the given VM moref.
	InFlightTasksForVM(ctx context.Context, vmMoref string) ([]types.TaskInfo, error)

	// VMExistsInFolder returns true if a VM named vmName already exists in
	// the vCenter folder at folderPath.
	VMExistsInFolder(ctx context.Context, folderPath, vmName string) (bool, error)

	// DatastoreFileExists returns true if filePath exists in datastoreName.
	DatastoreFileExists(ctx context.Context, datastoreName, filePath string) (bool, error)

	// HostPortGroupNames returns the names of standard vSwitch port groups
	// visible on the ESXi host with the given moref.
	// NOTE: this returns only standard-switch portgroups. DVS portgroups are
	// not included; see pf11PortGroupOnAllHosts for the documented limitation.
	HostPortGroupNames(ctx context.Context, hostMoref string) ([]string, error)

	// ClusterNameForHost returns the name of the compute resource (cluster or
	// standalone) that the given ESXi host belongs to.
	ClusterNameForHost(ctx context.Context, hostMoref string) (string, error)

	// DatastoreHostMorefs returns the morefs of all ESXi hosts that have the
	// named datastore accessible/mounted.
	DatastoreHostMorefs(ctx context.Context, datastoreName string) ([]string, error)

	// ClusterHostMorefs returns the morefs of all ESXi hosts in the cluster
	// that contains the given host moref. Returns only the host itself for
	// standalone (non-clustered) hosts.
	ClusterHostMorefs(ctx context.Context, hostMoref string) ([]string, error)

	// EligiblePlacementHostMorefs evaluates the same strict allowlist, pool,
	// datastore, network, host-health, and capacity constraints used at the VM
	// creation boundary. A successful result contains only allowlisted hosts.
	EligiblePlacementHostMorefs(
		ctx context.Context,
		sourceMoref, datastore, network string,
		vcpus int32,
		ramMB int64,
	) ([]string, error)
}

// RunAll executes all 11 preflight checks and returns their results in
// canonical order (PF-01 … PF-11). Checks that do not apply to the given
// source type are skipped and returned as OK=true with an explanatory Detail.
//
// RunAll never returns a Go error. A check that cannot query vCenter is
// returned as a failing Result with a descriptive Detail, not a panic or an
// omission — partial vCenter outages should not silently suppress all results.
func RunAll(ctx context.Context, vc PreflightVCenter, p Params) []Result {
	isClone := p.SourceType == models.TemplateSourceCloneTemplate ||
		p.SourceType == models.TemplateSourceCloneVCenter
	isISO := p.SourceType == models.TemplateSourceISO

	results := make([]Result, 0, 11)
	results = append(results, pf01SourceResolves(ctx, vc, p))
	results = append(results, pf02ClusterHasResourcePool(ctx, vc, p))
	results = append(results, pf03DatastoreMountedOnCluster(ctx, vc, p))
	results = append(results, pf04DatastoreFreeSpace(ctx, vc, p))
	results = append(results, pf05DiskChainIntact(ctx, vc, p))
	results = append(results, pf06NoInFlightTasks(ctx, vc, p))

	if isClone {
		results = append(results, pf07ToolsPresent(ctx, vc, p))
	} else {
		results = append(results, Result{
			ID: "PF-07", Severity: "block", OK: true,
			Detail: "skipped: only applies to clone-based sources",
		})
	}

	results = append(results, pf08CredentialsResolvable(p))
	results = append(results, pf09TargetVMNameFree(ctx, vc, p))

	if isISO {
		results = append(results, pf10ISOExists(ctx, vc, p))
	} else {
		results = append(results, Result{
			ID: "PF-10", Severity: "block", OK: true,
			Detail: "skipped: only applies to ISO sources",
		})
	}

	results = append(results, pf11PortGroupOnAllHosts(ctx, vc, p))
	return results
}

// AnyBlockFailed returns true if at least one block-severity check failed.
func AnyBlockFailed(results []Result) bool {
	for _, r := range results {
		if r.Severity == "block" && !r.OK {
			return true
		}
	}
	return false
}

// --- individual check functions ---

// cascadeIfSourceUnresolved returns a deterministic "Fix PF-01 first" failure
// when a clone-based check has no resolved source moref. The moref-dependent
// checks (PF-02 … PF-07, PF-11) must not depend on the vCenter call happening
// to error on an empty moref: the real API does (NoPermission/NotFound), but a
// fake — or a future client — may return an empty result instead, which would
// let a check show a misleading green when the source could not be resolved.
// Callers invoke this AFTER their ISO skip guard, so p.SourceMoref == "" here
// only ever means "clone source failed to resolve" (see PF-01 for the reason).
func cascadeIfSourceUnresolved(id, sev string, p Params) (Result, bool) {
	if p.SourceMoref == "" {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "source VM moref is not resolved",
			Fix:    "Fix PF-01 first."}, true
	}
	return Result{}, false
}

// pf01SourceResolves verifies the source VM moref resolves to a live
// managed-object reference in vCenter.
//
// Catches: deleted VM, mistyped moref, VM moved to a different datacenter.
// Does NOT catch disk corruption or transient clone failures.
func pf01SourceResolves(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-01", "block"
	if p.SourceType == models.TemplateSourceISO {
		// ISO installs have no source VM to resolve; PF-10 validates the ISO
		// file instead. Without this guard an ISO draft fails PF-01 forever
		// ("source moref is empty").
		return Result{ID: id, Severity: sev, OK: true,
			Detail: "skipped: no source VM to resolve (ISO source); PF-10 validates the ISO file"}
	}
	if p.SourceResolveError != "" {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("source VM reference could not be resolved: %s", p.SourceResolveError),
			Fix:    "Verify the source template still points at a live VM in vCenter, or re-pick the source in the wizard draft step."}
	}
	if p.SourceMoref == "" {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "source moref is empty",
			Fix:    "Re-create the template draft with a valid vCenter VM reference."}
	}
	if _, err := vc.FetchVMProps(ctx, p.SourceMoref); err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("moref %q does not resolve to a live VM: %v", p.SourceMoref, err),
			Fix:    "Verify the source VM still exists in vCenter and that the moref in the template draft is correct."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("moref %q resolves to a live VM", p.SourceMoref)}
}

// pf02ClusterHasResourcePool asks the canonical placement resolver to prove
// that an allowlisted host and configured pool are compatible with the source.
// It must not reproduce pool-selection logic or authorize a default pool.
//
// Catches: source VM placed in a cluster with no Crucible-managed pool.
// Does NOT catch: intermittent clone faults.
func pf02ClusterHasResourcePool(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-02", "block"
	if p.SourceType == models.TemplateSourceISO {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: no source moref (ISO source)"}
	}
	if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
		return r
	}
	if len(p.ConfiguredResourcePoolPaths) == 0 {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "no resource pool paths are configured; unpinned/default placement is prohibited",
			Fix:    "Set VCENTER_RESOURCE_POOLS to at least one pool path compatible with the source cluster and VCENTER_HOSTS."}
	}
	hosts, err := vc.EligiblePlacementHostMorefs(
		ctx,
		p.SourceMoref,
		p.DatastoreName,
		p.StagingPortGroup,
		1,
		512,
	)
	if err != nil || len(hosts) == 0 {
		detail := "the placement resolver returned no compatible allowlisted host"
		if err != nil {
			detail = fmt.Sprintf("configured pools cannot form an eligible placement with VCENTER_HOSTS: %v", err)
		}
		return Result{ID: id, Severity: sev, OK: false,
			Detail: detail,
			Fix:    "Use a configured pool in the source cluster that contains an allowed, healthy host with the required datastore and network."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("configured resource pool placement is eligible on allowlisted host(s): %v", hosts)}
}

// pf03DatastoreMountedOnCluster checks that at least one canonical allowlisted
// placement candidate satisfies the configured pool, datastore, network,
// health, and capacity constraints.
//
// Catches: NFS mount dropped from one cluster, datastore name wrong.
func pf03DatastoreMountedOnCluster(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-03", "block"
	if p.DatastoreName == "" {
		return Result{ID: id, Severity: sev, OK: true,
			Detail: "skipped: this template does not require a target datastore"}
	}
	if p.SourceType != models.TemplateSourceISO {
		if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
			return r
		}
	}
	hosts, err := vc.EligiblePlacementHostMorefs(
		ctx,
		p.SourceMoref,
		p.DatastoreName,
		p.StagingPortGroup,
		1,
		512,
	)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("no allowlisted host satisfies placement requirements: %v", err),
			Fix:    "Verify VCENTER_HOSTS, VCENTER_RESOURCE_POOLS, datastore mounts, host connection/maintenance state, capacity, and the staging port group."}
	}
	if len(hosts) == 0 {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "the placement resolver returned no allowlisted host",
			Fix:    "Verify VCENTER_HOSTS and the configured placement requirements."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("placement requirements are satisfied by allowlisted host(s): %v", hosts)}
}

// pf04DatastoreFreeSpace checks that the target datastore has at least
// 1.2× the source VM's provisioned size free.
//
// Catches: datastore nearly full before clone starts.
// Does NOT catch space consumed by concurrent clones between this check
// and the actual clone operation.
func pf04DatastoreFreeSpace(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-04", "block"
	if p.SourceType == models.TemplateSourceISO {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: no source moref (ISO source)"}
	}
	if p.DatastoreName == "" {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: target datastore not configured"}
	}
	if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
		return r
	}
	vm, err := vc.FetchVMProps(ctx, p.SourceMoref)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot read source VM properties: %v", err), Fix: "Fix PF-01 first."}
	}
	ds, err := vc.DatastoreInfo(ctx, p.DatastoreName)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot read datastore %q: %v", p.DatastoreName, err),
			Fix:    "Verify the datastore name in Crucible configuration."}
	}
	freeGB := ds.Summary.FreeSpace / (1 << 30)
	var provisionedGB int64
	if vm.Summary.Storage != nil {
		provisionedGB = (vm.Summary.Storage.Committed + vm.Summary.Storage.Uncommitted) / (1 << 30)
	}
	requiredGB := int64(float64(provisionedGB) * 1.2)
	if freeGB < requiredGB {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("datastore %q has %d GiB free; need %d GiB (1.2× source provisioned %d GiB)",
				p.DatastoreName, freeGB, requiredGB, provisionedGB),
			Fix: "Free space on the datastore, or choose a different target datastore in Crucible config."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("datastore %q has %d GiB free (need %d GiB for source %d GiB × 1.2)",
			p.DatastoreName, freeGB, requiredGB, provisionedGB)}
}

// pf05DiskChainIntact checks that the VM's disk chain is intact: if the VM
// has a snapshot tree the current-snapshot reference is non-nil, and each
// VirtualDisk has a non-empty backing filename.
//
// Catches: detached snapshot chain, disk backing without a file reference.
// Does NOT catch: missing delta files on a datastore vCenter hasn't scanned
// recently — full verification requires browsing every backing file, which
// is impractical pre-clone and outside the scope of a synchronous check.
func pf05DiskChainIntact(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-05", "block"
	if p.SourceType == models.TemplateSourceISO {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: no source moref (ISO source)"}
	}
	if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
		return r
	}
	vm, err := vc.FetchVMProps(ctx, p.SourceMoref)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot read source VM properties: %v", err), Fix: "Fix PF-01 first."}
	}
	// Snapshot chain consistency.
	if vm.Snapshot != nil && vm.Snapshot.CurrentSnapshot == nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "VM has a snapshot tree but CurrentSnapshot reference is nil — chain may be broken",
			Fix:    "Consolidate the VM's snapshots in vCenter, or delete orphaned snapshot entries."}
	}
	// Disk backing filenames.
	if vm.Config != nil {
		for _, device := range vm.Config.Hardware.Device {
			disk, ok := device.(*types.VirtualDisk)
			if !ok {
				continue
			}
			switch b := disk.Backing.(type) {
			case *types.VirtualDiskFlatVer2BackingInfo:
				if b.FileName == "" {
					label := "unknown disk"
					if disk.DeviceInfo != nil {
						label = disk.DeviceInfo.GetDescription().Summary
					}
					return Result{ID: id, Severity: sev, OK: false,
						Detail: fmt.Sprintf("disk %q has an empty backing filename — descriptor may be orphaned", label),
						Fix:    "Remove and re-add the disk in vCenter, or restore the backing file from backup."}
				}
			case *types.VirtualDiskSparseVer2BackingInfo:
				if b.FileName == "" {
					return Result{ID: id, Severity: sev, OK: false,
						Detail: "disk has an empty sparse backing filename",
						Fix:    "Remove and re-add the disk in vCenter, or restore the backing file from backup."}
				}
			}
		}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: "snapshot references and disk backing filenames are intact"}
}

// pf06NoInFlightTasks checks that no clone, consolidate, or relocate task is
// currently running or queued against the source VM.
//
// Context: the leading theory for an observed intermittent clone fault is
// concurrent vSphere operations against the same source VM (a synthetic job
// clones every 10 minutes; the wizard and pod provisioner also clone).
// PF-06 aims to reduce the frequency of that race by detecting when the
// source is already busy. It is a mitigation with UNKNOWN effectiveness —
// it does not eliminate the race window between this check passing and the
// clone actually starting. The retry-with-backoff lane handles the residual.
//
// Severity policy: PF-06 hard-blocks ONLY when it positively identifies an
// interfering in-flight task (clone/consolidate/relocate) on the source VM.
// If the task list cannot be read at all (e.g. the service account lacks the
// privilege to enumerate tasks → NoPermission), PF-06 degrades to a
// non-blocking warning: the provision worker does not gate on this check, so
// an unreadable task list must not block a provision the system can perform.
func pf06NoInFlightTasks(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-06", "block"
	if p.SourceType == models.TemplateSourceISO {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: no source moref (ISO source)"}
	}
	if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
		return r
	}
	tasks, err := vc.InFlightTasksForVM(ctx, p.SourceMoref)
	if err != nil {
		// Reading the task list is a best-effort mitigation, not a proven
		// precondition (see the doc comment above). The most common failure
		// here is a NoPermission fault: the vCenter service account can read
		// VM/datastore/cluster properties (PF-01…PF-05 passed) but lacks the
		// privilege to enumerate the TaskManager's tasks. When we cannot read
		// the task list we cannot determine whether an interfering task is
		// running — but the provision worker does not gate on this check and
		// clones successfully regardless. Blocking here would prevent a
		// provision the system can actually perform, so degrade to a
		// non-blocking warning rather than a hard block.
		return Result{ID: id, Severity: "warn", OK: false,
			Detail: fmt.Sprintf("could not verify in-flight tasks for VM %q: %v", p.SourceMoref, err),
			Fix: "Non-blocking — provisioning is not gated on this check. If this is a NoPermission " +
				"error, grant the vCenter service account read access to tasks (a role with System.Read " +
				"on the source VM and its parents) so PF-06 can detect concurrent clone/consolidate operations."}
	}
	// Filter to operations known to interfere with a concurrent clone.
	var blocking []string
	for _, t := range tasks {
		name := strings.ToLower(t.Name)
		if strings.Contains(name, "clone") ||
			strings.Contains(name, "consolidate") ||
			strings.Contains(name, "relocat") {
			started := ""
			if t.StartTime != nil {
				started = fmt.Sprintf(" (started %s ago)", time.Since(*t.StartTime).Truncate(time.Second))
			}
			blocking = append(blocking,
				fmt.Sprintf("%q [state: %s]%s", t.Name, t.State, started))
		}
	}
	if len(blocking) > 0 {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("source VM %q has %d in-flight task(s) that may interfere: %v",
				p.SourceMoref, len(blocking), blocking),
			Fix: "Wait for the in-flight operation to complete, then retry. If the task is stuck, cancel it in vCenter."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("no interfering in-flight tasks for VM %q", p.SourceMoref)}
}

// pf07ToolsPresent checks VMware Tools on the source VM. Applies to clone-based
// sources only; RunAll skips it for ISO.
//
// Template source VMs are normally powered OFF — they are not left running
// idly — and a running tools daemon is neither observable on a powered-off VM
// nor required for cloning. PF-07 is therefore power-state aware:
//
//   - Source powered OFF (the expected template state): verify tools are
//     *installed* via the guest tools version status, which vCenter retains
//     across power cycles. Installed → pass. Not installed / never reported →
//     a NON-BLOCKING warning. It never hard-blocks a provision the worker can
//     perform (the clone path does not require the source's tools to be running).
//   - Source powered ON: require tools to be *running*; a powered-on source
//     with a dead tools service is a genuine, blockable signal.
//
// Catches (powered on): source VM missing open-vm-tools / VMware Tools, or the
// tools service is stopped.
func pf07ToolsPresent(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-07", "block"
	if p.SourceType == models.TemplateSourceISO {
		return Result{ID: id, Severity: sev, OK: true, Detail: "skipped: no source moref"}
	}
	if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
		return r
	}
	vm, err := vc.FetchVMProps(ctx, p.SourceMoref)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot read source VM properties: %v", err), Fix: "Fix PF-01 first."}
	}

	poweredOn := vm.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn

	// Installed status derives from the tools version status, which vCenter
	// retains even while the VM is powered off. Empty means vCenter has never
	// received a guest report (e.g. the VM has never been powered on).
	var versionStatus string
	if vm.Guest != nil {
		versionStatus = vm.Guest.ToolsVersionStatus2
	}
	installed := versionStatus != "" &&
		versionStatus != string(types.VirtualMachineToolsVersionStatusGuestToolsNotInstalled)

	if !poweredOn {
		// Powered-off is the expected state for a template source. Never block:
		// running-tools state is not observable and provisioning does not need it.
		if installed {
			return Result{ID: id, Severity: sev, OK: true,
				Detail: fmt.Sprintf("VMware Tools is installed (version status: %q); source VM is powered off (expected for template sources), so running status is not checked", versionStatus)}
		}
		return Result{ID: id, Severity: "warn", OK: false,
			Detail: "source VM is powered off and VMware Tools does not appear installed (no guest tools version reported); clones may lack guest tools",
			Fix:    "Non-blocking. If clones need guest tools, power the source on once with open-vm-tools / VMware Tools installed so vCenter records the tools version, then power off."}
	}

	// Powered on: a live source should have tools running.
	if vm.Guest == nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "source VM is powered on but vCenter returned no guest info",
			Fix:    "Ensure open-vm-tools or VMware Tools is installed and running on the source VM."}
	}
	const running = string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	if vm.Guest.ToolsRunningStatus != running {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("source VM is powered on but VMware Tools running status is %q (want %q)",
				vm.Guest.ToolsRunningStatus, running),
			Fix: `Ensure open-vm-tools or VMware Tools is installed and enabled, and wait for status to reach "guestToolsRunning".`}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("VMware Tools is running (version status: %q)", vm.Guest.ToolsVersionStatus2)}
}

// pf08CredentialsResolvable checks that guest credentials (username +
// password) can be resolved from the template without extra input. This is
// warn-only: the generalize step accepts credentials in its request body,
// so missing defaults do not block the clone — but they will cause
// the generalize step to fail unless the instructor supplies them there.
func pf08CredentialsResolvable(p Params) Result {
	id, sev := "PF-08", "warn"
	if p.GuestUsername != "" && p.GuestPassword != "" {
		return Result{ID: id, Severity: sev, OK: true,
			Detail: fmt.Sprintf("guest username %q and password are set on the template", p.GuestUsername)}
	}
	var missing []string
	if p.GuestUsername == "" {
		missing = append(missing, "guest_username")
	}
	if p.GuestPassword == "" {
		missing = append(missing, "guest_password")
	}
	return Result{ID: id, Severity: sev, OK: false,
		Detail: fmt.Sprintf("template is missing: %s", strings.Join(missing, ", ")),
		Fix:    "Set default_username and default_password on the template, or supply credentials when calling the Generalize step."}
}

// pf09TargetVMNameFree checks that no VM with the intended target name
// already exists in the templates folder.
//
// Catches: name collision with a previous incomplete wizard run or a
// manually-created VM in the templates folder. Note: the template wizard
// generates a randomised suffix (tpl-<slug>-<6hex>), so collisions are
// rare but not impossible.
func pf09TargetVMNameFree(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-09", "block"
	if p.TargetVMName == "" || p.TargetFolderPath == "" {
		return Result{ID: id, Severity: sev, OK: true,
			Detail: "skipped: target VM name or folder path not set"}
	}
	exists, err := vc.VMExistsInFolder(ctx, p.TargetFolderPath, p.TargetVMName)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot check folder %q for existing VM %q: %v",
				p.TargetFolderPath, p.TargetVMName, err),
			Fix: "Check vCenter connectivity."}
	}
	if exists {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("VM named %q already exists in folder %q",
				p.TargetVMName, p.TargetFolderPath),
			Fix: "Delete or rename the existing VM in vCenter, or the wizard will reuse it on the next run (idempotency behaviour)."}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("name %q is free in folder %q", p.TargetVMName, p.TargetFolderPath)}
}

// pf10ISOExists verifies that the installer ISO path from source_ref
// actually exists on its datastore. Applies to ISO sources only.
//
// Catches: typo in the datastore path, ISO moved or deleted, wrong
// datastore name in source_ref.
func pf10ISOExists(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-10", "block"
	if p.ISORef == "" {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: "ISO source_ref is empty",
			Fix:    `Set source_ref to the installer ISO datastore path, e.g. "[NAS-BackupsAndISOS] ISOs/kali.iso".`}
	}
	// ParseDatastorePath is defined in internal/vcenter/datastore.go and
	// validates the "[datastore] path" format strictly.
	dsName, filePath, err := parseDatastorePath(p.ISORef)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("source_ref %q is not a valid datastore path: %v", p.ISORef, err),
			Fix:    `Use the format "[datastoreName] path/to/file.iso".`}
	}
	exists, err := vc.DatastoreFileExists(ctx, dsName, filePath)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("cannot browse datastore %q: %v", dsName, err),
			Fix:    "Check that the datastore is accessible and the Crucible service account has read permission."}
	}
	if !exists {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("ISO file %q not found in datastore %q", filePath, dsName),
			Fix:    fmt.Sprintf(`Upload the ISO to "%s" in datastore %q, or correct the source_ref path.`, filePath, dsName)}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("ISO %q found in datastore %q", filePath, dsName)}
}

// pf11PortGroupOnAllHosts blocks unless the shared resolver can select an
// allowlisted host with the required standard portgroup and all other
// placement constraints.
func pf11PortGroupOnAllHosts(ctx context.Context, vc PreflightVCenter, p Params) Result {
	id, sev := "PF-11", "block"
	if p.StagingPortGroup == "" {
		return Result{ID: id, Severity: sev, OK: true,
			Detail: "skipped: this template does not require a staging port group"}
	}
	if p.SourceType != models.TemplateSourceISO {
		if r, cascade := cascadeIfSourceUnresolved(id, sev, p); cascade {
			return r
		}
	}
	hosts, err := vc.EligiblePlacementHostMorefs(
		ctx,
		p.SourceMoref,
		p.DatastoreName,
		p.StagingPortGroup,
		1,
		512,
	)
	if err != nil {
		return Result{ID: id, Severity: sev, OK: false,
			Detail: fmt.Sprintf("no allowlisted host can use standard port group %q: %v", p.StagingPortGroup, err),
			Fix:    fmt.Sprintf("Create standard port group %q on an allowlisted host and verify its pool/datastore/health requirements.", p.StagingPortGroup)}
	}
	return Result{ID: id, Severity: sev, OK: true,
		Detail: fmt.Sprintf("standard port group %q is available on eligible allowlisted host(s): %v",
			p.StagingPortGroup, hosts)}
}

// parseDatastorePath is a local copy of vcenter.ParseDatastorePath to avoid
// a circular import between preflight and its parent vcenter package.
// Both must be kept in sync. See internal/vcenter/datastore.go.
func parseDatastorePath(full string) (datastore, remotePath string, err error) {
	s := strings.TrimSpace(full)
	if !strings.HasPrefix(s, "[") {
		return "", "", fmt.Errorf("invalid datastore path %q: must be in the form \"[datastore] path/to/file\"", full)
	}
	end := strings.Index(s, "]")
	if end < 0 {
		return "", "", fmt.Errorf("invalid datastore path %q: missing closing ']'", full)
	}
	datastore = strings.TrimSpace(s[1:end])
	remotePath = strings.TrimSpace(s[end+1:])
	if datastore == "" {
		return "", "", fmt.Errorf("invalid datastore path %q: datastore name is empty", full)
	}
	if remotePath == "" {
		return "", "", fmt.Errorf("invalid datastore path %q: file path is empty", full)
	}
	return datastore, remotePath, nil
}
