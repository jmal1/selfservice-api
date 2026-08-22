package vcenter

import (
	"context"
	"fmt"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
)

func (c *Client) findVMInFolderStrict(ctx context.Context, folder *object.Folder, name string) (string, error) {
	children, err := folder.Children(ctx)
	if err != nil {
		return "", err
	}
	for _, child := range children {
		vm, ok := child.(*object.VirtualMachine)
		if !ok {
			continue
		}
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"name"}, &props); err != nil {
			return "", fmt.Errorf("read VM name for %s: %w", vm.Reference().Value, err)
		}
		if props.Name == name {
			return vm.Reference().Value, nil
		}
	}
	return "", nil
}
