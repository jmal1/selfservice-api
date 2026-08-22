package vcenter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	ErrHostNotAllowed        = errors.New("vCenter host is outside VCENTER_HOSTS")
	ErrAmbiguousHostIdentity = errors.New("vCenter host identity is ambiguous")
	ErrPlacementUnavailable  = errors.New("no eligible allowlisted vCenter placement")
)

// HostIdentity is the immutable inventory identity resolved from one
// VCENTER_HOSTS entry at process startup.
type HostIdentity struct {
	Name          string `json:"name"`
	InventoryPath string `json:"inventory_path"`
	MoRef         string `json:"moref"`
	ComputeMoRef  string `json:"compute_moref"`
}

// PlacementRequest describes the constraints shared by every VM creation path.
// PinnedHostMoRef and PinnedPoolMoRef are populated when resuming a durable
// clone operation and prohibit selecting a different destination.
type PlacementRequest struct {
	SourceHost        *types.ManagedObjectReference
	RequireSourceHost bool
	ResourcePoolPath  string
	DatastoreName     string
	NetworkName       string
	VCPUs             int32
	RAMMB             int64
	PinnedHostMoRef   string
	PinnedPoolMoRef   string
}

// Placement is a fully resolved, explicitly pinned vCenter destination.
type Placement struct {
	Host       *object.HostSystem
	Pool       *object.ResourcePool
	Datastore  *object.Datastore
	Identity   HostIdentity
	PoolMoRef  string
	PoolPath   string
	FreeHostMB int64
	FreePoolMB int64
}

type placementCandidate struct {
	placement Placement
	reasons   []string
}

func resolveHostIdentities(configured []string, inventory []HostIdentity) ([]HostIdentity, error) {
	if len(configured) == 0 {
		return nil, errors.New("VCENTER_HOSTS must contain at least one host")
	}

	resolved := make([]HostIdentity, 0, len(configured))
	seenRefs := make(map[string]string, len(configured))
	for _, configuredName := range configured {
		var matches []HostIdentity
		for _, candidate := range inventory {
			if strings.EqualFold(candidate.Name, configuredName) ||
				strings.EqualFold(candidate.InventoryPath, configuredName) {
				matches = append(matches, candidate)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("VCENTER_HOSTS entry %q does not resolve inside the configured datacenter", configuredName)
		case 1:
		default:
			return nil, fmt.Errorf("VCENTER_HOSTS entry %q is ambiguous across %d inventory hosts", configuredName, len(matches))
		}
		match := matches[0]
		if previous, exists := seenRefs[match.MoRef]; exists {
			return nil, fmt.Errorf(
				"VCENTER_HOSTS entries %q and %q resolve to the same immutable host %s",
				previous,
				configuredName,
				match.MoRef,
			)
		}
		if match.MoRef == "" || match.ComputeMoRef == "" {
			return nil, fmt.Errorf("VCENTER_HOSTS entry %q resolved without complete inventory identity", configuredName)
		}
		seenRefs[match.MoRef] = configuredName
		resolved = append(resolved, match)
	}
	return resolved, nil
}

// ResolveProvisioningHosts resolves VCENTER_HOSTS against the configured
// datacenter and freezes the resulting names and MoRefs for this process.
func (c *Client) ResolveProvisioningHosts(ctx context.Context) ([]HostIdentity, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	hosts, err := c.finder.HostSystemList(ctx, "*")
	if err != nil {
		return nil, fmt.Errorf("inventory hosts in datacenter %q: %w", c.config.Datacenter, err)
	}

	inventory := make([]HostIdentity, 0, len(hosts))
	for _, host := range hosts {
		var props mo.HostSystem
		if err := host.Properties(ctx, host.Reference(), []string{"name", "parent"}, &props); err != nil {
			return nil, fmt.Errorf("read host identity %s: %w", host.Reference().Value, err)
		}
		if props.Parent == nil {
			return nil, fmt.Errorf("host %q (%s) has no parent compute resource", props.Name, host.Reference().Value)
		}
		inventory = append(inventory, HostIdentity{
			Name:          props.Name,
			InventoryPath: host.InventoryPath,
			MoRef:         host.Reference().Value,
			ComputeMoRef:  props.Parent.Value,
		})
	}

	resolved, err := resolveHostIdentities(c.config.Hosts, inventory)
	if err != nil {
		return nil, err
	}

	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	if len(c.allowedHosts) > 0 {
		if len(c.allowedHosts) != len(resolved) {
			return nil, errors.New("resolved VCENTER_HOSTS identity changed after startup")
		}
		for i := range resolved {
			if c.allowedHosts[i] != resolved[i] {
				return nil, fmt.Errorf(
					"resolved VCENTER_HOSTS identity changed after startup: %s was %+v, now %+v",
					c.config.Hosts[i],
					c.allowedHosts[i],
					resolved[i],
				)
			}
		}
		return append([]HostIdentity(nil), c.allowedHosts...), nil
	}
	c.allowedHosts = append([]HostIdentity(nil), resolved...)
	for _, host := range resolved {
		c.logger.Info("resolved provisioning host allowlist",
			"host", host.Name,
			"inventory_path", host.InventoryPath,
			"moref", host.MoRef,
			"compute_moref", host.ComputeMoRef)
	}
	return append([]HostIdentity(nil), resolved...), nil
}

// AllowedHostMorefs returns the immutable host identities resolved at startup.
func (c *Client) AllowedHostMorefs() []string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	morefs := make([]string, 0, len(c.allowedHosts))
	for _, host := range c.allowedHosts {
		morefs = append(morefs, host.MoRef)
	}
	return morefs
}

func (c *Client) resolvedHosts() ([]HostIdentity, error) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	if len(c.allowedHosts) == 0 {
		return nil, errors.New("VCENTER_HOSTS was not resolved at process startup")
	}
	return append([]HostIdentity(nil), c.allowedHosts...), nil
}

func (c *Client) allowedHostByMoRef(moref string) (HostIdentity, error) {
	hosts, err := c.resolvedHosts()
	if err != nil {
		return HostIdentity{}, err
	}
	for _, host := range hosts {
		if host.MoRef == moref {
			return host, nil
		}
	}
	return HostIdentity{}, fmt.Errorf("%w: %s", ErrHostNotAllowed, moref)
}

func mountAccessible(mount types.HostMountInfo, datastoreAccessible bool) bool {
	if mount.Mounted != nil && !*mount.Mounted {
		return false
	}
	if mount.Accessible != nil {
		return *mount.Accessible
	}
	return datastoreAccessible
}

func hasStandardPortGroup(props *mo.HostSystem, name string) bool {
	if name == "" {
		return true
	}
	if props.Config == nil || props.Config.Network == nil {
		return false
	}
	for _, portGroup := range props.Config.Network.Portgroup {
		if portGroup.Spec.Name == name {
			return true
		}
	}
	return false
}

func hostFreeMemoryMB(props *mo.HostSystem) int64 {
	if props.Summary.Hardware == nil || props.Summary.Hardware.MemorySize <= 0 {
		return 0
	}
	totalMB := props.Summary.Hardware.MemorySize / (1024 * 1024)
	usedMB := int64(props.Summary.QuickStats.OverallMemoryUsage)
	return totalMB - usedMB
}

func (c *Client) configuredPools(ctx context.Context, explicitPath, pinnedMoRef string) ([]*object.ResourcePool, error) {
	paths := c.config.ResourcePools
	if explicitPath != "" {
		paths = []string{explicitPath}
	}
	if len(paths) == 0 {
		return nil, errors.New("VCENTER_RESOURCE_POOLS must contain at least one pool; unpinned DefaultResourcePool fallback is disabled")
	}
	pools := make([]*object.ResourcePool, 0, len(paths))
	var failures []string
	for _, poolPath := range paths {
		pool, err := c.finder.ResourcePool(ctx, poolPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", poolPath, err))
			continue
		}
		pools = append(pools, pool)
	}
	if len(pools) == 0 {
		return nil, fmt.Errorf("no configured resource pool resolves: %s", strings.Join(failures, "; "))
	}
	if pinnedMoRef != "" {
		for _, pool := range pools {
			if pool.Reference().Value == pinnedMoRef {
				return []*object.ResourcePool{pool}, nil
			}
		}
		return nil, fmt.Errorf(
			"%w: persisted resource pool %s is outside VCENTER_RESOURCE_POOLS",
			ErrPlacementUnavailable,
			pinnedMoRef,
		)
	}
	return pools, nil
}

// ResolvePlacement selects one compatible resource-pool/allowlisted-host pair.
// It never falls back to DefaultResourcePool and always returns an explicit host.
func (c *Client) ResolvePlacement(ctx context.Context, request PlacementRequest) (*Placement, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	allowedHosts, err := c.resolvedHosts()
	if err != nil {
		return nil, err
	}
	if request.DatastoreName == "" {
		request.DatastoreName = c.config.Datastore
	}
	if request.DatastoreName == "" {
		return nil, errors.New("placement requires a datastore")
	}

	datastore, err := c.finder.Datastore(ctx, request.DatastoreName)
	if err != nil {
		return nil, fmt.Errorf("find placement datastore %q: %w", request.DatastoreName, err)
	}
	var datastoreProps mo.Datastore
	if err := datastore.Properties(ctx, datastore.Reference(), []string{"summary", "host"}, &datastoreProps); err != nil {
		return nil, fmt.Errorf("read placement datastore %q: %w", request.DatastoreName, err)
	}
	if !datastoreProps.Summary.Accessible {
		return nil, fmt.Errorf("%w: datastore %q is not accessible", ErrPlacementUnavailable, request.DatastoreName)
	}
	datastoreHosts := make(map[string]types.HostMountInfo, len(datastoreProps.Host))
	for _, mount := range datastoreProps.Host {
		datastoreHosts[mount.Key.Value] = mount.MountInfo
	}

	sourceCompute := ""
	if request.RequireSourceHost && request.SourceHost == nil {
		return nil, fmt.Errorf(
			"%w: source VM has no runtime host assignment; source compute-resource compatibility cannot be proven",
			ErrPlacementUnavailable,
		)
	}
	if request.SourceHost != nil {
		if _, err := c.allowedHostByMoRef(request.SourceHost.Value); err != nil {
			return nil, fmt.Errorf(
				"%w: source host %s is outside the canonical provisioning allowlist",
				ErrPlacementUnavailable,
				request.SourceHost.Value,
			)
		}
		source := object.NewHostSystem(c.client.Client, *request.SourceHost)
		var sourceProps mo.HostSystem
		if err := source.Properties(ctx, source.Reference(), []string{"parent"}, &sourceProps); err != nil {
			return nil, fmt.Errorf("read source host %s parent: %w", request.SourceHost.Value, err)
		}
		if sourceProps.Parent == nil {
			return nil, fmt.Errorf("source host %s has no parent compute resource", request.SourceHost.Value)
		}
		sourceCompute = sourceProps.Parent.Value
	}

	pools, err := c.configuredPools(ctx, request.ResourcePoolPath, request.PinnedPoolMoRef)
	if err != nil {
		return nil, err
	}

	var candidates []placementCandidate
	var diagnostics []string
	for _, pool := range pools {
		var poolProps mo.ResourcePool
		if err := pool.Properties(ctx, pool.Reference(), []string{"name", "owner", "runtime.memory"}, &poolProps); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("pool %s unreadable: %v", pool.InventoryPath, err))
			continue
		}
		if sourceCompute != "" && poolProps.Owner.Value != sourceCompute {
			diagnostics = append(diagnostics, fmt.Sprintf(
				"pool %s belongs to compute resource %s, not source compute resource %s",
				pool.InventoryPath,
				poolProps.Owner.Value,
				sourceCompute,
			))
			continue
		}
		freePoolMB := (poolProps.Runtime.Memory.MaxUsage - poolProps.Runtime.Memory.OverallUsage) / (1024 * 1024)

		for _, identity := range allowedHosts {
			if request.PinnedHostMoRef != "" && identity.MoRef != request.PinnedHostMoRef {
				continue
			}
			candidate := placementCandidate{
				placement: Placement{
					Host:       object.NewHostSystem(c.client.Client, types.ManagedObjectReference{Type: "HostSystem", Value: identity.MoRef}),
					Pool:       pool,
					Datastore:  datastore,
					Identity:   identity,
					PoolMoRef:  pool.Reference().Value,
					PoolPath:   pool.InventoryPath,
					FreePoolMB: freePoolMB,
				},
			}
			if identity.ComputeMoRef != poolProps.Owner.Value {
				candidate.reasons = append(candidate.reasons, fmt.Sprintf("not a member of pool compute resource %s", poolProps.Owner.Value))
			}
			mount, mounted := datastoreHosts[identity.MoRef]
			if !mounted || !mountAccessible(mount, datastoreProps.Summary.Accessible) {
				candidate.reasons = append(candidate.reasons, fmt.Sprintf("datastore %q is not mounted and accessible", request.DatastoreName))
			}

			var hostProps mo.HostSystem
			if err := candidate.placement.Host.Properties(
				ctx,
				candidate.placement.Host.Reference(),
				[]string{"runtime", "summary", "config.network.portgroup"},
				&hostProps,
			); err != nil {
				candidate.reasons = append(candidate.reasons, fmt.Sprintf("host properties unreadable: %v", err))
			} else {
				if hostProps.Runtime.ConnectionState != types.HostSystemConnectionStateConnected {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf("connection state is %s", hostProps.Runtime.ConnectionState))
				}
				if hostProps.Runtime.InMaintenanceMode {
					candidate.reasons = append(candidate.reasons, "host is in maintenance mode")
				}
				if !hasStandardPortGroup(&hostProps, request.NetworkName) {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf("standard port group %q is missing", request.NetworkName))
				}
				if request.VCPUs > 0 && hostProps.Summary.Hardware != nil &&
					hostProps.Summary.Hardware.NumCpuCores > 0 &&
					int32(hostProps.Summary.Hardware.NumCpuCores) < request.VCPUs {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf(
						"host has %d CPU cores, needs %d",
						hostProps.Summary.Hardware.NumCpuCores,
						request.VCPUs,
					))
				}
				candidate.placement.FreeHostMB = hostFreeMemoryMB(&hostProps)
				if request.RAMMB > 0 && candidate.placement.FreeHostMB > 0 &&
					candidate.placement.FreeHostMB < request.RAMMB {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf(
						"host has %d MB free, needs %d MB",
						candidate.placement.FreeHostMB,
						request.RAMMB,
					))
				}
			}
			if len(candidate.reasons) == 0 {
				candidates = append(candidates, candidate)
			} else {
				diagnostics = append(diagnostics, fmt.Sprintf(
					"host %s (%s) with pool %s: %s",
					identity.Name,
					identity.MoRef,
					pool.InventoryPath,
					strings.Join(candidate.reasons, ", "),
				))
			}
		}
	}

	if len(candidates) == 0 {
		if request.PinnedHostMoRef != "" {
			if _, err := c.allowedHostByMoRef(request.PinnedHostMoRef); err != nil {
				diagnostics = append(diagnostics, err.Error())
			}
		}
		if len(diagnostics) == 0 {
			diagnostics = append(diagnostics, "no configured pool belongs to an allowlisted host")
		}
		return nil, fmt.Errorf("%w: %s", ErrPlacementUnavailable, strings.Join(diagnostics, "; "))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].placement.FreePoolMB != candidates[j].placement.FreePoolMB {
			return candidates[i].placement.FreePoolMB > candidates[j].placement.FreePoolMB
		}
		if candidates[i].placement.FreeHostMB != candidates[j].placement.FreeHostMB {
			return candidates[i].placement.FreeHostMB > candidates[j].placement.FreeHostMB
		}
		if candidates[i].placement.Identity.Name != candidates[j].placement.Identity.Name {
			return candidates[i].placement.Identity.Name < candidates[j].placement.Identity.Name
		}
		return candidates[i].placement.PoolPath < candidates[j].placement.PoolPath
	})
	selected := candidates[0].placement
	c.logger.Info("selected explicit VM placement",
		"host", selected.Identity.Name,
		"host_moref", selected.Identity.MoRef,
		"pool", selected.PoolPath,
		"pool_moref", selected.PoolMoRef,
		"datastore", request.DatastoreName,
		"network", request.NetworkName)
	return &selected, nil
}

// ResolveClonePlacement selects and records the destination before durable
// clone intent is persisted.
func (c *Client) ResolveClonePlacement(ctx context.Context, params CloneVMParams) (CloneVMParams, error) {
	source, err := c.resolveSourceVM(ctx, params.TemplateName)
	if err != nil {
		return CloneVMParams{}, fmt.Errorf("resolve clone source %s: %w", params.TemplateName, err)
	}
	var sourceProps mo.VirtualMachine
	if err := source.Properties(ctx, source.Reference(), []string{"runtime.host"}, &sourceProps); err != nil {
		return CloneVMParams{}, fmt.Errorf("read clone source host: %w", err)
	}
	placement, err := c.ResolvePlacement(ctx, PlacementRequest{
		SourceHost:        sourceProps.Runtime.Host,
		RequireSourceHost: true,
		DatastoreName:     c.config.Datastore,
		NetworkName:       params.Network,
		VCPUs:             params.VCPUs,
		RAMMB:             params.RAMmb,
		PinnedHostMoRef:   params.HostMoRef,
		PinnedPoolMoRef:   params.ResourcePoolMoRef,
	})
	if err != nil {
		return CloneVMParams{}, err
	}
	params.HostMoRef = placement.Identity.MoRef
	params.HostName = placement.Identity.Name
	params.ResourcePoolMoRef = placement.PoolMoRef
	return params, nil
}

// ValidateVMPlacement rejects an existing or recovered VM that is not on the
// expected immutable host, or whose host is no longer allowlisted.
func (c *Client) ValidateVMPlacement(ctx context.Context, vmMoref, expectedHostMoRef string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &props); err != nil {
		return fmt.Errorf("read VM %s placement: %w", vmMoref, err)
	}
	if props.Runtime.Host == nil || props.Runtime.Host.Value == "" {
		return fmt.Errorf("%w: VM %s has no runtime host assignment", ErrHostNotAllowed, vmMoref)
	}
	if _, err := c.allowedHostByMoRef(props.Runtime.Host.Value); err != nil {
		return fmt.Errorf("%w: VM %s is on %s", err, vmMoref, props.Runtime.Host.Value)
	}
	if expectedHostMoRef != "" && props.Runtime.Host.Value != expectedHostMoRef {
		return fmt.Errorf(
			"%w: VM %s is on %s, expected persisted host %s",
			ErrHostNotAllowed,
			vmMoref,
			props.Runtime.Host.Value,
			expectedHostMoRef,
		)
	}
	return nil
}

func (c *Client) validateVMForMutation(
	ctx context.Context,
	vmMoref, expectedHostMoref string,
	missingIsSuccess bool,
) error {
	err := c.ValidateVMPlacement(ctx, vmMoref, expectedHostMoref)
	if err == nil {
		return nil
	}
	if missingIsSuccess && (isAlreadyDeletedErr(err) || isResourceNotFoundErr(err)) {
		return nil
	}
	return fmt.Errorf("refuse vCenter mutation for VM %s: %w", vmMoref, err)
}

// EligiblePlacementHostMorefs exposes the shared resolver to preflight without
// allowing preflight to make a placement decision of its own.
func (c *Client) EligiblePlacementHostMorefs(
	ctx context.Context,
	sourceMoref, datastore, network string,
	vcpus int32,
	ramMB int64,
) ([]string, error) {
	var sourceHost *types.ManagedObjectReference
	if sourceMoref != "" {
		source, err := c.resolveSourceVM(ctx, sourceMoref)
		if err != nil {
			return nil, err
		}
		var props mo.VirtualMachine
		if err := source.Properties(ctx, source.Reference(), []string{"runtime.host"}, &props); err != nil {
			return nil, err
		}
		sourceHost = props.Runtime.Host
	}
	placement, err := c.ResolvePlacement(ctx, PlacementRequest{
		SourceHost:        sourceHost,
		RequireSourceHost: sourceMoref != "",
		DatastoreName:     datastore,
		NetworkName:       network,
		VCPUs:             vcpus,
		RAMMB:             ramMB,
	})
	if err != nil {
		return nil, err
	}
	return []string{placement.Identity.MoRef}, nil
}
