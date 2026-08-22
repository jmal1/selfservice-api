package vcenter

// preflight_methods.go — Implements the preflight.PreflightVCenter interface
// on *vcenter.Client.
//
// These are thin data-fetching wrappers around govmomi. The check logic lives
// in internal/vcenter/preflight/preflight.go; this file only exposes the data
// those checks need without duplicating any decision-making.
//
// Compile-time assertion: the interface is defined in the preflight package to
// avoid a dependency in the other direction. The assertion lives in
// internal/vcenter/preflight/preflight_assert_test.go where it can import both
// packages without creating a cycle.

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// FetchVMProps retrieves the key property set needed by preflight checks for
// the given VM moref. Returns a non-nil error if the moref does not resolve
// to a live VirtualMachine in this vCenter session.
func (c *Client) FetchVMProps(ctx context.Context, moref string) (*mo.VirtualMachine, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
	var props mo.VirtualMachine
	// Property set covers all 11 checks:
	//   runtime        → PF-02 (host), PF-11 (host)
	//   guest          → PF-07 (tools status)
	//   config.hardware.device → PF-05 (disk backings)
	//   snapshot       → PF-05 (snapshot chain)
	//   summary.storage→ PF-04 (provisioned size)
	if err := vm.Properties(ctx, vm.Reference(),
		[]string{"runtime", "guest", "config.hardware.device", "snapshot", "summary.storage"},
		&props); err != nil {
		return nil, fmt.Errorf("fetch VM props for %s: %w", moref, err)
	}
	return &props, nil
}

// DatastoreInfo returns the mo.Datastore for the named datastore, including
// its summary (free space in bytes) and host-mount list.
func (c *Client) DatastoreInfo(ctx context.Context, name string) (*mo.Datastore, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	var ds *mo.Datastore
	err := c.withRetry(ctx, "datastore info", func() error {
		obj, err := c.finder.Datastore(ctx, name)
		if err != nil {
			return fmt.Errorf("find datastore %q: %w", name, err)
		}
		var props mo.Datastore
		if err := obj.Properties(ctx, obj.Reference(), []string{"summary", "host"}, &props); err != nil {
			return fmt.Errorf("read datastore props: %w", err)
		}
		ds = &props
		return nil
	})
	return ds, err
}

// InFlightTasksForVM returns the tasks currently queued or running whose
// managed entity is the given VM moref.
//
// Implementation note: vCenter's TaskManager.RecentTask list is capped at 200
// entries and only reflects roughly the last 24 hours. Tasks older than that
// window (e.g. a very long running consolidation started days ago) are not
// visible. This is a best-effort check, not an exhaustive task audit.
func (c *Client) InFlightTasksForVM(ctx context.Context, vmMoref string) ([]types.TaskInfo, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	pc := property.DefaultCollector(c.client.Client)
	tm := task.NewManager(c.client.Client)
	var tmProps mo.TaskManager
	if err := pc.RetrieveOne(ctx, tm.Reference(), []string{"recentTask"}, &tmProps); err != nil {
		return nil, fmt.Errorf("read task manager: %w", err)
	}
	if len(tmProps.RecentTask) == 0 {
		return nil, nil
	}
	var tasks []mo.Task
	if err := pc.Retrieve(ctx, tmProps.RecentTask, []string{"info"}, &tasks); err != nil {
		return nil, fmt.Errorf("read task infos: %w", err)
	}
	var inflight []types.TaskInfo
	for _, t := range tasks {
		if t.Info.State != types.TaskInfoStateQueued &&
			t.Info.State != types.TaskInfoStateRunning {
			continue
		}
		if t.Info.Entity == nil {
			continue
		}
		if t.Info.Entity.Value == vmMoref {
			inflight = append(inflight, t.Info)
		}
	}
	return inflight, nil
}

// VMExistsInFolder returns true if a VM named vmName already exists in the
// vCenter folder at folderPath.
func (c *Client) VMExistsInFolder(ctx context.Context, folderPath, vmName string) (bool, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return false, err
	}
	var exists bool
	err := c.withRetry(ctx, "check VM in folder", func() error {
		folder, err := c.finder.Folder(ctx, folderPath)
		if err != nil {
			return fmt.Errorf("find folder %q: %w", folderPath, err)
		}
		moref, err := c.findVMInFolder(ctx, folder, vmName)
		if err != nil {
			return err
		}
		exists = moref != ""
		return nil
	})
	return exists, err
}

// DatastoreFileExists returns true if filePath exists on the named datastore.
// Uses Datastore.Stat — a not-found fault from vCenter is translated to
// (false, nil) rather than an error.
func (c *Client) DatastoreFileExists(ctx context.Context, datastoreName, filePath string) (bool, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return false, err
	}
	var exists bool
	err := c.withRetry(ctx, "check datastore file", func() error {
		ds, err := c.finder.Datastore(ctx, datastoreName)
		if err != nil {
			return fmt.Errorf("find datastore %q: %w", datastoreName, err)
		}
		// Stat returns a FileInfo or a fault. FileNotFound is "file does not
		// exist", which is the normal not-present case — not an error.
		_, statErr := ds.Stat(ctx, filePath)
		if statErr == nil {
			exists = true
			return nil
		}
		// vCenter surfaces FileNotFound as a fault embedded in an error string.
		if strings.Contains(statErr.Error(), "FileNotFound") ||
			strings.Contains(statErr.Error(), "not found") ||
			strings.Contains(statErr.Error(), "cannot be found") {
			exists = false
			return nil
		}
		return fmt.Errorf("stat %s/%s: %w", datastoreName, filePath, statErr)
	})
	return exists, err
}

// HostPortGroupNames returns the names of standard vSwitch port groups
// configured on the given ESXi host.
//
// This returns only standard-switch port groups (config.network.portgroup).
// DVS port groups are not enumerated here; see the PF-11 comment for the
// documented limitation this creates.
func (c *Client) HostPortGroupNames(ctx context.Context, hostMoref string) ([]string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	host := object.NewHostSystem(c.client.Client,
		types.ManagedObjectReference{Type: "HostSystem", Value: hostMoref})
	var props mo.HostSystem
	if err := host.Properties(ctx, host.Reference(),
		[]string{"config.network.portgroup"}, &props); err != nil {
		return nil, fmt.Errorf("read host portgroups for %s: %w", hostMoref, err)
	}
	var names []string
	if props.Config != nil && props.Config.Network != nil {
		for _, pg := range props.Config.Network.Portgroup {
			names = append(names, pg.Spec.Name)
		}
	}
	return names, nil
}

// ClusterNameForHost returns the host's compute-resource name for read-only
// diagnostics. Placement authorization belongs exclusively to ResolvePlacement.
func (c *Client) ClusterNameForHost(ctx context.Context, hostMoref string) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	host := object.NewHostSystem(c.client.Client,
		types.ManagedObjectReference{Type: "HostSystem", Value: hostMoref})
	var hostProps mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"parent"}, &hostProps); err != nil {
		return "", fmt.Errorf("read host parent for %s: %w", hostMoref, err)
	}
	if hostProps.Parent == nil {
		return "", nil
	}
	parent := *hostProps.Parent
	crObj := object.NewComputeResource(c.client.Client, parent)
	var crProps mo.ComputeResource
	if err := crObj.Properties(ctx, crObj.Reference(), []string{"name"}, &crProps); err != nil {
		return "", fmt.Errorf("read cluster/compute-resource name: %w", err)
	}
	return crProps.Name, nil
}

// DatastoreHostMorefs returns the morefs of all ESXi hosts that have the
// named datastore accessible (mounted).
func (c *Client) DatastoreHostMorefs(ctx context.Context, datastoreName string) ([]string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	var morefs []string
	err := c.withRetry(ctx, "datastore host morefs", func() error {
		obj, err := c.finder.Datastore(ctx, datastoreName)
		if err != nil {
			return fmt.Errorf("find datastore %q: %w", datastoreName, err)
		}
		var props mo.Datastore
		if err := obj.Properties(ctx, obj.Reference(), []string{"host"}, &props); err != nil {
			return fmt.Errorf("read datastore host list: %w", err)
		}
		morefs = make([]string, 0, len(props.Host))
		for _, hm := range props.Host {
			morefs = append(morefs, hm.Key.Value)
		}
		return nil
	})
	return morefs, err
}

// ClusterHostMorefs returns the morefs of all ESXi hosts in the compute
// resource (cluster) that contains hostMoref. For standalone hosts not in a
// cluster, returns a single-element slice containing only hostMoref itself.
func (c *Client) ClusterHostMorefs(ctx context.Context, hostMoref string) ([]string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	host := object.NewHostSystem(c.client.Client,
		types.ManagedObjectReference{Type: "HostSystem", Value: hostMoref})
	var hostProps mo.HostSystem
	if err := host.Properties(ctx, host.Reference(), []string{"parent"}, &hostProps); err != nil {
		return nil, fmt.Errorf("read host parent for %s: %w", hostMoref, err)
	}
	if hostProps.Parent == nil {
		return []string{hostMoref}, nil
	}
	parent := *hostProps.Parent
	crObj := object.NewComputeResource(c.client.Client, parent)
	var crProps mo.ComputeResource
	if err := crObj.Properties(ctx, crObj.Reference(), []string{"host"}, &crProps); err != nil {
		return nil, fmt.Errorf("read cluster host list: %w", err)
	}
	morefs := make([]string, 0, len(crProps.Host))
	for _, h := range crProps.Host {
		morefs = append(morefs, h.Value)
	}
	return morefs, nil
}
