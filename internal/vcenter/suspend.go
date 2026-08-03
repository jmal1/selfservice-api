package vcenter

// VM performance and suspend operations — added by the idle-suspend feature
// (migration 000025). Kept in a separate file so the diff on client.go stays
// tight (that file is gofmt-dirty at HEAD on purpose).

import (
	"context"
	"fmt"
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

// QueryVMToolsStatusBulk fetches guest.toolsRunningStatus for all morefs in a
// SINGLE PropertyCollector Retrieve call. Returns a map from MoRef value to
// true (Tools running) or false (Tools not running or not installed).
//
// A moref absent from the result (e.g. VM deleted between the DB query and the
// vCenter call) is treated by callers as Tools-not-running.
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

	var vms []mo.VirtualMachine
	retryErr := c.withRetry(ctx, "query VM tools status bulk", func() error {
		pc := property.DefaultCollector(c.client.Client)
		return pc.Retrieve(ctx, refs, []string{"guest.toolsRunningStatus"}, &vms)
	})
	if retryErr != nil {
		return nil, fmt.Errorf("bulk tools status query: %w", retryErr)
	}

	result := make(map[string]bool, len(vms))
	for i := range vms {
		running := vms[i].Guest != nil &&
			vms[i].Guest.ToolsRunningStatus == string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
		result[vms[i].Self.Value] = running
	}
	return result, nil
}
