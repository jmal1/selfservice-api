package vcenter

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// applyVTPMClonePolicy inspects the source hardware and replaces any vTPM
// during cloning so the destination cannot access the source TPM's secrets.
func applyVTPMClonePolicy(
	ctx context.Context,
	source *object.VirtualMachine,
	cloneSpec *types.VirtualMachineCloneSpec,
) ([]types.BaseVirtualDevice, error) {
	if source == nil {
		return nil, fmt.Errorf("inspect source VM hardware for clone policy: source VM is nil")
	}
	if cloneSpec == nil {
		return nil, fmt.Errorf("inspect source VM hardware for clone policy: clone spec is nil")
	}

	var sourceProps mo.VirtualMachine
	if err := source.Properties(
		ctx,
		source.Reference(),
		[]string{"config.hardware.device"},
		&sourceProps,
	); err != nil {
		return nil, fmt.Errorf("inspect source VM hardware for clone policy: %w", err)
	}
	if sourceProps.Config == nil {
		return nil, fmt.Errorf("inspect source VM hardware for clone policy: source configuration is unavailable")
	}

	devices := sourceProps.Config.Hardware.Device
	for _, device := range devices {
		if _, ok := device.(*types.VirtualTPM); ok {
			cloneSpec.TpmProvisionPolicy = string(
				types.VirtualMachineCloneSpecTpmProvisionPolicyReplace,
			)
			break
		}
	}
	return devices, nil
}
