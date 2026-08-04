package vcenter

// Unit tests for retrieveVMToolsStatus (the testable inner implementation of
// QueryVMToolsStatusBulk). These tests use a fake vmPropertyRetriever so no
// govmomi simulator is needed; the fake injects specific error/result shapes to
// exercise each branch.
//
// The four required cases are:
//   1. No deleted morefs  → single bulk call, fast path, fallback NOT run.
//   2. One deleted moref  → bulk fails, fallback retrieves surviving VMs, no error.
//   3. All morefs deleted → bulk fails, fallback skips all, empty map, no error.
//   4. Hard failure       → bulk fails with non-deleted error, error propagated.
//
// A vcsim-level happy-path test lives alongside these but is not required for
// the regression coverage — the fake tests above are the regression tests.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// fakeToolsRetriever is a test double for vmPropertyRetriever used in
// retrieveVMToolsStatus unit tests. It records every Retrieve call and returns
// configurable errors or tools-status results.
type fakeToolsRetriever struct {
	// calls accumulates the moref-value slices from each Retrieve invocation
	// so tests can assert call count and which refs were queried.
	calls [][]string

	// deletedMorefs contains moref values that should return a deleted-object
	// error. When the bulk call includes any deleted moref, the first call
	// returns the error (mirroring real vCenter: one bad ref fails the whole
	// batch). Per-moref fallback calls for a deleted moref also return it.
	deletedMorefs map[string]bool

	// tools maps moref value → whether VMware Tools is running. A moref
	// absent from this map is not included in results, simulating vCenter
	// omitting a VM (which callers treat as Tools-not-running).
	tools map[string]bool

	// genericErr, if non-nil, is returned on every Retrieve call regardless
	// of moref — for testing non-deleted hard failures (e.g. auth).
	genericErr error
}

func newFakeToolsRetriever() *fakeToolsRetriever {
	return &fakeToolsRetriever{
		deletedMorefs: map[string]bool{},
		tools:         map[string]bool{},
	}
}

func (f *fakeToolsRetriever) Retrieve(
	_ context.Context,
	objs []types.ManagedObjectReference,
	_ []string,
	dst interface{},
) error {
	refs := make([]string, len(objs))
	for i, o := range objs {
		refs[i] = o.Value
	}
	f.calls = append(f.calls, refs)

	if f.genericErr != nil {
		return f.genericErr
	}

	// If the batch includes any deleted moref, fail the whole call — this is
	// how real vCenter behaves: one bad ref poisons the entire Retrieve.
	for _, o := range objs {
		if f.deletedMorefs[o.Value] {
			return fmt.Errorf("ServerFaultCode: The object 'vim.VirtualMachine:%s' "+
				"has already been deleted or has not been completely created", o.Value)
		}
	}

	vmsPtr, ok := dst.(*[]mo.VirtualMachine)
	if !ok {
		return errors.New("fakeToolsRetriever: dst is not *[]mo.VirtualMachine")
	}
	var out []mo.VirtualMachine
	for _, obj := range objs {
		running, inMap := f.tools[obj.Value]
		if !inMap {
			continue
		}
		vm := mo.VirtualMachine{}
		vm.Self = obj
		vm.Guest = &types.GuestInfo{}
		if running {
			vm.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
		} else {
			vm.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
		}
		out = append(out, vm)
	}
	*vmsPtr = out
	return nil
}

// TestRetrieveVMToolsStatus_NoDeletions is the negative control for the fast
// path: when no VMs are deleted, the function MUST issue exactly one bulk
// Retrieve call and NOT fall back to per-moref retrieval. This test verifies
// that the fallback path is never triggered for the common case, so normal
// operation remains a single round-trip.
func TestRetrieveVMToolsStatus_NoDeletions(t *testing.T) {
	refs := []types.ManagedObjectReference{
		{Type: "VirtualMachine", Value: "vm-1"},
		{Type: "VirtualMachine", Value: "vm-2"},
	}
	retr := newFakeToolsRetriever()
	retr.tools["vm-1"] = true
	retr.tools["vm-2"] = false

	result, err := retrieveVMToolsStatus(context.Background(), refs, retr, slog.Default())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(retr.calls) != 1 {
		t.Errorf("fast path: expected exactly 1 bulk Retrieve call, got %d — "+
			"the fallback ran when it should not have, degrading performance needlessly",
			len(retr.calls))
	}
	if len(retr.calls[0]) != 2 {
		t.Errorf("bulk call should cover all 2 refs, got %d", len(retr.calls[0]))
	}
	if !result["vm-1"] {
		t.Error("vm-1 should have Tools running=true")
	}
	if result["vm-2"] {
		t.Error("vm-2 should have Tools running=false")
	}
}

// TestRetrieveVMToolsStatus_OneDeletedVM is the regression test for the live
// production bug: one stale moref in the database caused the bulk Retrieve to
// fail, which caused the entire idle-evaluator tick to be skipped with
// "skipping all VMs this tick".
//
// After the fix: the function detects the deleted-object fault, falls back to
// per-moref retrieval, and returns results for the surviving VMs with no error.
func TestRetrieveVMToolsStatus_OneDeletedVM(t *testing.T) {
	refs := []types.ManagedObjectReference{
		{Type: "VirtualMachine", Value: "vm-alive"},
		{Type: "VirtualMachine", Value: "vm-dead"},
	}
	retr := newFakeToolsRetriever()
	retr.tools["vm-alive"] = true
	retr.deletedMorefs["vm-dead"] = true // simulates deleted VM in vCenter

	result, err := retrieveVMToolsStatus(context.Background(), refs, retr, slog.Default())
	if err != nil {
		t.Fatalf("one deleted moref must not return an error (got: %v); "+
			"this is the production bug — a deleted VM was disabling auto-suspend for all VMs", err)
	}
	// Expected call sequence: 1 bulk (fails) + 2 per-moref (vm-alive ok, vm-dead skipped)
	wantCalls := 3
	if len(retr.calls) != wantCalls {
		t.Errorf("expected %d Retrieve calls (1 bulk + 2 per-moref), got %d",
			wantCalls, len(retr.calls))
	}
	if !result["vm-alive"] {
		t.Error("vm-alive should be present in result with Tools running=true")
	}
	if _, present := result["vm-dead"]; present {
		t.Error("vm-dead (deleted) must be absent from result; callers treat absence as Tools-not-running")
	}
}

// TestRetrieveVMToolsStatus_AllDeleted verifies the edge case where every moref
// in the batch refers to a deleted VM. The function must return an empty map
// and no error so callers treat all VMs as Tools-not-running (no suspension).
func TestRetrieveVMToolsStatus_AllDeleted(t *testing.T) {
	refs := []types.ManagedObjectReference{
		{Type: "VirtualMachine", Value: "vm-gone-1"},
		{Type: "VirtualMachine", Value: "vm-gone-2"},
	}
	retr := newFakeToolsRetriever()
	retr.deletedMorefs["vm-gone-1"] = true
	retr.deletedMorefs["vm-gone-2"] = true

	result, err := retrieveVMToolsStatus(context.Background(), refs, retr, slog.Default())
	if err != nil {
		t.Fatalf("all-deleted batch must not return an error, got: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("all-deleted batch: expected empty result map, got: %v", result)
	}
}

// TestRetrieveVMToolsStatus_HardFailure verifies that a genuine non-deleted
// failure (e.g. auth error, connection refused) is still propagated as an
// error and does NOT produce a silent empty result. This is the safety
// property: the fix must not convert real failures into phantom success.
func TestRetrieveVMToolsStatus_HardFailure(t *testing.T) {
	refs := []types.ManagedObjectReference{
		{Type: "VirtualMachine", Value: "vm-1"},
	}
	retr := newFakeToolsRetriever()
	retr.genericErr = errors.New("ServerFaultCode: NotAuthenticated")

	_, err := retrieveVMToolsStatus(context.Background(), refs, retr, slog.Default())
	if err == nil {
		t.Fatal("a non-deleted hard failure must propagate as an error, not return nil; " +
			"the fix must not silently swallow real failures")
	}
}
