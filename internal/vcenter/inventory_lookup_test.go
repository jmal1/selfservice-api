package vcenter

import (
	"context"
	"errors"
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func TestInventoryScanIgnoresDeletedChildAndContinues(t *testing.T) {
	readers := []vmInventoryReader{
		{
			moref: "vm-gone",
			read: func(context.Context, []string, *mo.VirtualMachine) error {
				return soap.WrapVimFault(&types.ManagedObjectNotFound{})
			},
		},
		{
			moref: "vm-live",
			read: func(_ context.Context, _ []string, dst *mo.VirtualMachine) error {
				dst.Name = "target"
				return nil
			},
		},
	}
	got, err := findVMNameInInventory(context.Background(), readers, "target")
	if err != nil {
		t.Fatalf("findVMNameInInventory: %v", err)
	}
	if got != "vm-live" {
		t.Fatalf("found %q, want vm-live", got)
	}
}

func TestInventoryTransportFailureRemainsRetryableLookupError(t *testing.T) {
	transportErr := errors.New("session transport unavailable")
	readers := []vmInventoryReader{{
		moref: "vm-1",
		read: func(context.Context, []string, *mo.VirtualMachine) error {
			return transportErr
		},
	}}
	_, err := findVMOperationInInventory(context.Background(), readers, CloneVMParams{
		OperationID:  "op",
		PodVMID:      "pod-vm",
		VMName:       "target",
		TemplateName: "vm-source",
	})
	if !errors.Is(err, transportErr) {
		t.Fatalf("inventory error = %v, want transport failure", err)
	}
	if errors.Is(err, ErrAmbiguousVMOwnership) {
		t.Fatalf("transport failure was misclassified as ambiguous ownership: %v", err)
	}
}

func TestOperationMarkerRequiresExactSourceAndPodVM(t *testing.T) {
	params := CloneVMParams{
		OperationID:       "operation-1",
		PodVMID:           "pod-vm-1",
		VMName:            "target",
		TemplateName:      "vm-source",
		HostMoRef:         "host-1",
		ResourcePoolMoRef: "resgroup-1",
	}
	markerParams := params
	runtimeHost := markerParams.HostMoRef
	reader := vmInventoryReader{
		moref: "vm-42",
		read: func(_ context.Context, _ []string, dst *mo.VirtualMachine) error {
			dst.Name = markerParams.VMName
			dst.Config = &types.VirtualMachineConfigInfo{
				ExtraConfig: []types.BaseOptionValue{
					&types.OptionValue{Key: CloneOperationIDKey, Value: markerParams.OperationID},
					&types.OptionValue{Key: CloneOperationSourceKey, Value: markerParams.TemplateName},
					&types.OptionValue{Key: CloneOperationPodVMKey, Value: markerParams.PodVMID},
					&types.OptionValue{Key: CloneOperationHostKey, Value: markerParams.HostMoRef},
					&types.OptionValue{Key: CloneOperationPoolKey, Value: markerParams.ResourcePoolMoRef},
				},
			}
			dst.Runtime.Host = &types.ManagedObjectReference{Type: "HostSystem", Value: runtimeHost}
			return nil
		},
	}
	got, err := findVMOperationInInventory(context.Background(), []vmInventoryReader{reader}, params)
	if err != nil || got != "vm-42" {
		t.Fatalf("marker lookup got=%q err=%v", got, err)
	}

	params.PodVMID = "different"
	if _, err := findVMOperationInInventory(context.Background(), []vmInventoryReader{reader}, params); !errors.Is(err, ErrAmbiguousVMOwnership) {
		t.Fatalf("mismatched immutable marker error = %v", err)
	}

	params.PodVMID = markerParams.PodVMID
	runtimeHost = "host-2"
	_, err = findVMOperationInInventory(context.Background(), []vmInventoryReader{reader}, params)
	var drift *PlacementDriftError
	if !errors.As(err, &drift) || drift.Kind != PlacementDriftHost {
		t.Fatalf("wrong-host clone marker error = %#v, want typed host drift", err)
	}
	if !errors.Is(err, ErrPlacementDrift) || !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("wrong-host clone marker error = %v, want placement drift and host-not-allowed", err)
	}
}
