package vcenter

// VM performance and suspend operations — added by the idle-suspend feature
// (migration 000025). Kept in a separate file so the diff on client.go stays
// tight (that file is gofmt-dirty at HEAD on purpose).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// SuspendVM saves the VM's CPU and memory state to disk and halts execution.
// The VM can be resumed later with PowerOnVM, which resumes from the saved
// checkpoint rather than performing a fresh boot.
//
// Idempotent: returns nil if the VM is already suspended.
// Follows the same error-handling and task-waiting conventions as PowerOffVM.
func (c *Client) SuspendVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	return c.withRetry(ctx, "suspend VM", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		task, err := vm.Suspend(ctx)
		if err != nil {
			if isAlreadySuspendedErr(err) {
				return nil
			}
			return fmt.Errorf("suspend %s: %w", moref, err)
		}
		if err := task.Wait(ctx); err != nil {
			if isAlreadySuspendedErr(err) {
				return nil
			}
			return err
		}
		return nil
	})
}

func isAlreadySuspendedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "is already suspended")
}

// VMPerfSample holds the latest real-time performance counters for a single VM.
type VMPerfSample struct {
	// CPUUsage is cpu.usage.average in hundredths of a percent (0–10000).
	// e.g. 500 == 5.00 %. vCenter reports this as an integer * 100 from the
	// PercentageUnit (see PerfCounterInfo).
	CPUUsage int64
	// NetUsage is net.usage.average in KBps (combined Rx+Tx).
	NetUsage int64
	// Valid is false when no real-time data was returned for the VM (e.g. VM
	// powered off, stats collection not available). The idle evaluator treats
	// a missing sample as NOT idle — absence of a signal is never read as
	// evidence of idleness.
	Valid bool
}

// SamplePodVMPerf queries the latest cpu.usage.average and net.usage.average
// for every VM in morefs in a SINGLE batched QueryPerf SOAP call via
// performance.Manager.SampleByName. The call issues one
// []types.PerfQuerySpec (one spec per entity) in a single HTTP round-trip.
//
// Returns a map from MoRef value → VMPerfSample. Morefs for which vCenter
// returns no data (powered-off, tools absent, stats disabled) are absent from
// the map; callers must treat absence as "not idle."
func (c *Client) SamplePodVMPerf(ctx context.Context, morefs []string) (map[string]VMPerfSample, error) {
	if len(morefs) == 0 {
		return map[string]VMPerfSample{}, nil
	}
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	refs := make([]types.ManagedObjectReference, len(morefs))
	for i, m := range morefs {
		refs[i] = types.ManagedObjectReference{Type: "VirtualMachine", Value: m}
	}

	pm := performance.NewManager(c.client.Client)

	// Build the counter key→name reverse map from the cached CounterInfoByName
	// result, so we can decode counter IDs in the response without an extra RPC.
	infoByName, err := pm.CounterInfoByName(ctx)
	if err != nil {
		return nil, fmt.Errorf("perf counter info: %w", err)
	}
	keyToName := make(map[int32]string, len(infoByName))
	for name, info := range infoByName {
		keyToName[info.Key] = name
	}

	// Spec template: real-time interval (20 s), one sample per entity.
	// IntervalId=20 selects the vCenter real-time 20-second rollup.
	// MaxSample=1 requests only the latest sample per VM.
	spec := types.PerfQuerySpec{
		MaxSample:  1,
		IntervalId: 20,
	}

	// SampleByName issues a single QueryPerf SOAP request containing one
	// PerfQuerySpec per entity in refs. All N results come back in one
	// HTTP response — not N sequential calls.
	var series []types.BasePerfEntityMetricBase
	retryErr := c.withRetry(ctx, "sample pod VM performance", func() error {
		var sampleErr error
		series, sampleErr = pm.SampleByName(ctx, spec, []string{
			"cpu.usage.average",
			"net.usage.average",
		}, refs)
		return sampleErr
	})
	if retryErr != nil {
		return nil, fmt.Errorf("sample pod VM performance: %w", retryErr)
	}

	result := make(map[string]VMPerfSample, len(series))
	for _, base := range series {
		em, ok := base.(*types.PerfEntityMetric)
		if !ok {
			continue
		}
		sample := VMPerfSample{Valid: true}
		for _, v := range em.Value {
			ms, ok := v.(*types.PerfMetricIntSeries)
			if !ok || len(ms.Value) == 0 {
				continue
			}
			switch keyToName[ms.Id.CounterId] {
			case "cpu.usage.average":
				sample.CPUUsage = ms.Value[0]
			case "net.usage.average":
				sample.NetUsage = ms.Value[0]
			}
		}
		result[em.Entity.Value] = sample
	}
	return result, nil
}

// vmPropertyRetriever is the narrow interface used by QueryVMToolsStatusBulk
// for both the bulk and per-moref fallback retrieval steps.
// property.DefaultCollector satisfies it. Tests inject a fake to exercise the
// deleted-VM fallback path without a full govmomi simulator.
type vmPropertyRetriever interface {
	Retrieve(ctx context.Context, objs []types.ManagedObjectReference, ps []string, dst interface{}) error
}

// QueryVMToolsStatusBulk fetches guest.toolsRunningStatus for all morefs in a
// SINGLE PropertyCollector Retrieve call. Returns a map from MoRef value to
// true (Tools running) or false (Tools not running or not installed).
//
// A moref absent from the result (e.g. VM deleted between the DB query and the
// vCenter call) is treated by callers as Tools-not-running.
//
// If the bulk call fails because one or more VMs have already been deleted,
// the function falls back to per-moref retrieval and omits the deleted refs
// from the result rather than returning an error. Only genuine non-deleted
// failures (auth errors, network problems, etc.) are propagated as errors.
func (c *Client) QueryVMToolsStatusBulk(ctx context.Context, morefs []string) (map[string]bool, error) {
	if len(morefs) == 0 {
		return map[string]bool{}, nil
	}
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	refs := make([]types.ManagedObjectReference, len(morefs))
	for i, m := range morefs {
		refs[i] = types.ManagedObjectReference{Type: "VirtualMachine", Value: m}
	}

	// withRetry handles session expiry (NotAuthenticated). Deleted-object
	// errors are not retryable and pass through; retrieveVMToolsStatus handles
	// them in the fallback path.
	var result map[string]bool
	retryErr := c.withRetry(ctx, "query VM tools status bulk", func() error {
		pc := property.DefaultCollector(c.client.Client)
		var err error
		result, err = retrieveVMToolsStatus(ctx, refs, pc, c.logger)
		return err
	})
	return result, retryErr
}

// retrieveVMToolsStatus is the testable implementation behind QueryVMToolsStatusBulk.
//
// Fast path: issues a single bulk Retrieve over all refs (one PropertyCollector
// round-trip). If that succeeds, returns immediately without a fallback.
//
// Degraded path: if the bulk call fails with a deleted-object fault (which real
// vCenter returns when any moref in the batch refers to a destroyed VM), falls
// back to per-moref retrieval. Refs that fail with a deleted-object error are
// silently omitted from the result (absent = treated as Tools-not-running by
// callers). Any non-deleted error is returned immediately as a hard failure so
// genuine problems (auth, connectivity) still surface.
func retrieveVMToolsStatus(
	ctx context.Context,
	refs []types.ManagedObjectReference,
	retr vmPropertyRetriever,
	logger *slog.Logger,
) (map[string]bool, error) {
	// Fast path: single bulk Retrieve (one PropertyCollector round-trip).
	var vms []mo.VirtualMachine
	err := retr.Retrieve(ctx, refs, []string{"guest.toolsRunningStatus"}, &vms)
	if err == nil {
		return vmToolsStatusMap(vms), nil
	}
	if !isAlreadyDeletedErr(err) {
		// Hard failure — auth error, connection error, etc. Surface it.
		return nil, fmt.Errorf("bulk tools status query: %w", err)
	}

	// Degraded path: at least one moref refers to a deleted VM. Retrieve
	// per-moref so we return results for the VMs that still exist.
	logger.Warn("bulk VM tools status query: encountered deleted VM reference, falling back to per-moref",
		"total_refs", len(refs))

	result := make(map[string]bool, len(refs))
	skipped := 0
	for _, ref := range refs {
		var single []mo.VirtualMachine
		if perErr := retr.Retrieve(ctx, []types.ManagedObjectReference{ref},
			[]string{"guest.toolsRunningStatus"}, &single); perErr != nil {
			if isAlreadyDeletedErr(perErr) {
				skipped++
				continue
			}
			return nil, fmt.Errorf("bulk tools status query: %w", perErr)
		}
		for i := range single {
			running := single[i].Guest != nil &&
				single[i].Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
			result[single[i].Self.Value] = running
		}
	}
	logger.Warn("bulk VM tools status fallback complete",
		"skipped_deleted", skipped, "total_refs", len(refs))
	return result, nil
}

// vmToolsStatusMap builds a moref-value→running map from a []mo.VirtualMachine slice.
func vmToolsStatusMap(vms []mo.VirtualMachine) map[string]bool {
	result := make(map[string]bool, len(vms))
	for i := range vms {
		running := vms[i].Guest != nil &&
			vms[i].Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
		result[vms[i].Self.Value] = running
	}
	return result
}
