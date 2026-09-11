package vcenter

import (
	"context"
	"errors"
	"fmt"

	"github.com/vmware/govmomi/fault"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func isManagedObjectNotFound(err error) bool {
	return err != nil && fault.Is(err, &types.ManagedObjectNotFound{})
}

type vmInventoryReader struct {
	moref string
	read  func(context.Context, []string, *mo.VirtualMachine) error
}

func inventoryVMReaders(children []object.Reference) []vmInventoryReader {
	readers := make([]vmInventoryReader, 0, len(children))
	for _, child := range children {
		vm, ok := child.(*object.VirtualMachine)
		if !ok {
			continue
		}
		readers = append(readers, vmInventoryReader{
			moref: vm.Reference().Value,
			read: func(ctx context.Context, paths []string, dst *mo.VirtualMachine) error {
				return vm.Properties(ctx, vm.Reference(), paths, dst)
			},
		})
	}
	return readers
}

func findVMNameInInventory(ctx context.Context, readers []vmInventoryReader, name string) (string, error) {
	for _, reader := range readers {
		var props mo.VirtualMachine
		if err := reader.read(ctx, []string{"name"}, &props); err != nil {
			if isManagedObjectNotFound(err) {
				continue
			}
			return "", fmt.Errorf("read VM name for %s: %w", reader.moref, err)
		}
		if props.Name == name {
			return reader.moref, nil
		}
	}
	return "", nil
}

func (c *Client) findVMInFolderStrict(ctx context.Context, folder *object.Folder, name string) (string, error) {
	children, err := folder.Children(ctx)
	if err != nil {
		return "", fmt.Errorf("list VM folder children: %w", err)
	}
	return findVMNameInInventory(ctx, inventoryVMReaders(children), name)
}

func optionValueString(values []types.BaseOptionValue, key string) string {
	for _, value := range values {
		option, ok := value.(*types.OptionValue)
		if ok && option.Key == key {
			if text, ok := option.Value.(string); ok {
				return text
			}
		}
	}
	return ""
}

func findVMOperationInInventory(
	ctx context.Context,
	readers []vmInventoryReader,
	params CloneVMParams,
) (string, error) {
	var matching string
	for _, reader := range readers {
		var props mo.VirtualMachine
		err := reader.read(ctx, []string{"name", "config.extraConfig", "runtime.host"}, &props)
		if err != nil {
			if isManagedObjectNotFound(err) {
				continue
			}
			return "", fmt.Errorf("read clone marker for %s: %w", reader.moref, err)
		}
		if props.Name != params.VMName {
			continue
		}
		if props.Config == nil ||
			optionValueString(props.Config.ExtraConfig, CloneOperationIDKey) != params.OperationID ||
			optionValueString(props.Config.ExtraConfig, CloneOperationSourceKey) != params.TemplateName ||
			optionValueString(props.Config.ExtraConfig, CloneOperationPodVMKey) != params.PodVMID ||
			optionValueString(props.Config.ExtraConfig, CloneOperationHostKey) != params.HostMoRef ||
			optionValueString(props.Config.ExtraConfig, CloneOperationPoolKey) != params.ResourcePoolMoRef ||
			optionValueString(props.Config.ExtraConfig, CloneOperationComputeTypeKey) != params.ComputeResourceType ||
			optionValueString(props.Config.ExtraConfig, CloneOperationComputeKey) != params.ComputeResourceMoRef ||
			optionValueString(props.Config.ExtraConfig, CloneOperationReplicaKey) != params.SourceReplicaID ||
			optionValueString(props.Config.ExtraConfig, CloneOperationTemplateKey) != params.LogicalTemplateID {
			return "", fmt.Errorf(
				"%w: clone target %q exists without operation marker %s",
				ErrAmbiguousVMOwnership,
				params.VMName,
				params.OperationID,
			)
		}
		if props.Runtime.Host == nil || props.Runtime.Host.Value != params.HostMoRef {
			return reader.moref, newPlacementDrift(
				PlacementDriftHost,
				ErrHostNotAllowed,
				"clone target %q is on host %v, expected %s",
				params.VMName,
				props.Runtime.Host,
				params.HostMoRef,
			)
		}
		if matching != "" && matching != reader.moref {
			return "", fmt.Errorf(
				"%w: multiple VMs match clone operation %s",
				ErrAmbiguousVMOwnership,
				params.OperationID,
			)
		}
		matching = reader.moref
	}
	return matching, nil
}

// FindVMByCloneOperation reconciles an ambiguous clone submission using the
// creation-time marker plus target/source scope. A same-name VM without the
// matching marker is never adopted.
func (c *Client) FindVMByCloneOperation(ctx context.Context, params CloneVMParams) (string, error) {
	if params.OperationID == "" || params.PodVMID == "" || params.VMName == "" ||
		params.LogicalTemplateID == "" || params.TemplateName == "" ||
		params.ComputeResourceType == "" || params.ComputeResourceMoRef == "" ||
		params.HostMoRef == "" || params.ResourcePoolMoRef == "" {
		return "", errors.New("clone reconciliation requires operation, pod VM, template, source, compute, host, and pool identities")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	folder, err := c.finder.Folder(ctx, c.config.VMFolder)
	if err != nil {
		return "", fmt.Errorf("find folder %s for clone reconciliation: %w", c.config.VMFolder, err)
	}
	children, err := folder.Children(ctx)
	if err != nil {
		return "", fmt.Errorf("list VM folder children for clone reconciliation: %w", err)
	}

	return findVMOperationInInventory(ctx, inventoryVMReaders(children), params)
}
