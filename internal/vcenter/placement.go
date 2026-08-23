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
	ErrReservedHeadroom      = errors.New("vCenter host reserved headroom would be violated")
)

// HostIdentity is the immutable inventory identity resolved from one
// VCENTER_HOSTS entry at process startup.
type HostIdentity struct {
	Name             string `json:"name"`
	InventoryPath    string `json:"inventory_path"`
	MoRef            string `json:"moref"`
	ComputeType      string `json:"compute_type"`
	ComputeMoRef     string `json:"compute_moref"`
	ReservedMemoryMB int64  `json:"reserved_memory_mb"`
}

// PlacementRequest describes the constraints shared by every VM creation path.
// PinnedHostMoRef and PinnedPoolMoRef are populated when resuming a durable
// clone operation and prohibit selecting a different destination.
type PlacementRequest struct {
	SourceHost            *types.ManagedObjectReference
	RequireSourceHost     bool
	ResourcePoolPath      string
	DatastoreName         string
	NetworkName           string
	VCPUs                 int32
	RAMMB                 int64
	PinnedHostMoRef       string
	PinnedPoolMoRef       string
	ExpectedComputeType   string
	ExpectedComputeMoRef  string
	AllowMissingNetwork   bool
	SkipCapacityChecks    bool
	PlannedMemoryMBByHost map[string]int64
	TargetHostMoRefs      []string
}

// Placement is a fully resolved, explicitly pinned vCenter destination.
type Placement struct {
	Host             *object.HostSystem
	Pool             *object.ResourcePool
	Datastore        *object.Datastore
	Identity         HostIdentity
	PoolMoRef        string
	PoolPath         string
	ComputeType      string
	ComputeMoRef     string
	FreeHostMB       int64
	FreePoolMB       int64
	ReservedMemoryMB int64
}

// TemplateSourceIdentity is the immutable inventory identity stored for one
// logical template's source replica.
type TemplateSourceIdentity struct {
	SourceVMMoref        string
	HostMoref            string
	HostName             string
	ComputeResourceType  string
	ComputeResourceMoref string
	ComputeResourcePath  string
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
			ComputeType:   props.Parent.Type,
			ComputeMoRef:  props.Parent.Value,
		})
	}

	resolved, err := resolveHostIdentities(c.config.Hosts, inventory)
	if err != nil {
		return nil, err
	}
	for i := range resolved {
		resolved[i].ReservedMemoryMB = c.config.HostReservedMemoryMB[c.config.Hosts[i]]
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

// ResolveTemplateSourceIdentity verifies that ref names a real VM on a
// currently allowlisted host and returns its immutable compute-resource
// identity. Registration stores this result rather than a mutable inventory
// name.
func (c *Client) ResolveTemplateSourceIdentity(
	ctx context.Context,
	ref string,
) (*TemplateSourceIdentity, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	vm, err := c.resolveSourceVM(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve template source %q: %w", ref, err)
	}
	var vmProps mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &vmProps); err != nil {
		return nil, fmt.Errorf("read template source %s host: %w", vm.Reference().Value, err)
	}
	if vmProps.Runtime.Host == nil {
		return nil, fmt.Errorf("%w: template source %s has no runtime host", ErrPlacementUnavailable, vm.Reference().Value)
	}
	hostIdentity, err := c.allowedHostByMoRef(vmProps.Runtime.Host.Value)
	if err != nil {
		return nil, fmt.Errorf("%w: template source host %s", ErrPlacementUnavailable, err)
	}
	computeRef := types.ManagedObjectReference{
		Type:  hostIdentity.ComputeType,
		Value: hostIdentity.ComputeMoRef,
	}
	compute, err := c.finder.ObjectReference(ctx, computeRef)
	if err != nil {
		return nil, fmt.Errorf("resolve source compute resource %s: %w", computeRef.Value, err)
	}
	computePath := ""
	switch typed := compute.(type) {
	case *object.ClusterComputeResource:
		computePath = typed.InventoryPath
	case *object.ComputeResource:
		computePath = typed.InventoryPath
	default:
		return nil, fmt.Errorf("source compute resource %s resolved as %T", computeRef.Value, compute)
	}
	return &TemplateSourceIdentity{
		SourceVMMoref:        vm.Reference().Value,
		HostMoref:            hostIdentity.MoRef,
		HostName:             hostIdentity.Name,
		ComputeResourceType:  hostIdentity.ComputeType,
		ComputeResourceMoref: hostIdentity.ComputeMoRef,
		ComputeResourcePath:  computePath,
	}, nil
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
	targetHosts := make(map[string]struct{}, len(request.TargetHostMoRefs))
	for _, moref := range request.TargetHostMoRefs {
		if _, err := c.allowedHostByMoRef(moref); err != nil {
			return nil, fmt.Errorf("%w: requested target host %s", ErrPlacementUnavailable, moref)
		}
		targetHosts[moref] = struct{}{}
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
	sourceComputeType := ""
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
		sourceComputeType = sourceProps.Parent.Type
	}
	if request.ExpectedComputeMoRef != "" &&
		(sourceCompute != request.ExpectedComputeMoRef ||
			(request.ExpectedComputeType != "" && sourceComputeType != request.ExpectedComputeType)) {
		return nil, fmt.Errorf(
			"%w: source compute resource is %s/%s, expected %s/%s",
			ErrPlacementUnavailable,
			sourceComputeType,
			sourceCompute,
			request.ExpectedComputeType,
			request.ExpectedComputeMoRef,
		)
	}

	pools, err := c.configuredPools(ctx, request.ResourcePoolPath, request.PinnedPoolMoRef)
	if err != nil {
		return nil, err
	}

	var candidates []placementCandidate
	var diagnostics []string
	headroomRejected := false
	for _, pool := range pools {
		var poolProps mo.ResourcePool
		if err := pool.Properties(ctx, pool.Reference(), []string{"name", "owner", "runtime.memory"}, &poolProps); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("pool %s unreadable: %v", pool.InventoryPath, err))
			continue
		}
		if sourceCompute != "" &&
			(poolProps.Owner.Value != sourceCompute ||
				(sourceComputeType != "" && poolProps.Owner.Type != sourceComputeType)) {
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
			if len(targetHosts) > 0 {
				if _, selected := targetHosts[identity.MoRef]; !selected {
					continue
				}
			}
			candidate := placementCandidate{
				placement: Placement{
					Host:             object.NewHostSystem(c.client.Client, types.ManagedObjectReference{Type: "HostSystem", Value: identity.MoRef}),
					Pool:             pool,
					Datastore:        datastore,
					Identity:         identity,
					PoolMoRef:        pool.Reference().Value,
					PoolPath:         pool.InventoryPath,
					ComputeType:      poolProps.Owner.Type,
					ComputeMoRef:     poolProps.Owner.Value,
					FreePoolMB:       freePoolMB,
					ReservedMemoryMB: identity.ReservedMemoryMB,
				},
			}
			if identity.ComputeMoRef != poolProps.Owner.Value ||
				(identity.ComputeType != "" && identity.ComputeType != poolProps.Owner.Type) {
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
				if !request.AllowMissingNetwork && !hasStandardPortGroup(&hostProps, request.NetworkName) {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf("standard port group %q is missing", request.NetworkName))
				}
				if !request.SkipCapacityChecks &&
					request.VCPUs > 0 && hostProps.Summary.Hardware != nil &&
					hostProps.Summary.Hardware.NumCpuCores > 0 &&
					int32(hostProps.Summary.Hardware.NumCpuCores) < request.VCPUs {
					candidate.reasons = append(candidate.reasons, fmt.Sprintf(
						"host has %d CPU cores, needs %d",
						hostProps.Summary.Hardware.NumCpuCores,
						request.VCPUs,
					))
				}
				candidate.placement.FreeHostMB = hostFreeMemoryMB(&hostProps)
				if !request.SkipCapacityChecks {
					plannedMemoryMB := request.PlannedMemoryMBByHost[identity.MoRef]
					requiredFreeMB := request.RAMMB + identity.ReservedMemoryMB + plannedMemoryMB
					if requiredFreeMB > 0 && candidate.placement.FreeHostMB < requiredFreeMB {
						headroomRejected = true
						candidate.reasons = append(candidate.reasons, fmt.Sprintf(
							"host has %d MB free, needs %d MB plus %d MB planned and %d MB reserved headroom",
							candidate.placement.FreeHostMB,
							request.RAMMB,
							plannedMemoryMB,
							identity.ReservedMemoryMB,
						))
					}
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
		if headroomRejected {
			return nil, fmt.Errorf(
				"%w: %w: %s",
				ErrPlacementUnavailable,
				ErrReservedHeadroom,
				strings.Join(diagnostics, "; "),
			)
		}
		return nil, fmt.Errorf("%w: %s", ErrPlacementUnavailable, strings.Join(diagnostics, "; "))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].placement.FreePoolMB != candidates[j].placement.FreePoolMB {
			return candidates[i].placement.FreePoolMB > candidates[j].placement.FreePoolMB
		}
		iAvailable := candidates[i].placement.FreeHostMB -
			request.PlannedMemoryMBByHost[candidates[i].placement.Identity.MoRef]
		jAvailable := candidates[j].placement.FreeHostMB -
			request.PlannedMemoryMBByHost[candidates[j].placement.Identity.MoRef]
		if iAvailable != jAvailable {
			return iAvailable > jAvailable
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
		"compute_type", selected.ComputeType,
		"compute_moref", selected.ComputeMoRef,
		"free_memory_mb", selected.FreeHostMB,
		"reserved_memory_mb", selected.ReservedMemoryMB,
		"datastore", request.DatastoreName,
		"network", request.NetworkName)
	return &selected, nil
}

// ResolveClonePlacement selects and records the destination before durable
// clone intent is persisted.
func (c *Client) ResolveClonePlacement(ctx context.Context, params CloneVMParams) (CloneVMParams, error) {
	return c.resolveClonePlacement(ctx, params, false)
}

// ResolveExistingClonePlacement reconstructs immutable placement for a clone
// adopted before vm_placements existed. Capacity is not charged again because
// the VM is already resident on the observed host.
func (c *Client) ResolveExistingClonePlacement(
	ctx context.Context,
	vmMoref string,
	params CloneVMParams,
) (CloneVMParams, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return CloneVMParams{}, err
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host", "resourcePool"}, &props); err != nil {
		return CloneVMParams{}, fmt.Errorf("read existing VM %s placement: %w", vmMoref, err)
	}
	if props.Runtime.Host == nil || props.Runtime.Host.Value == "" || props.ResourcePool == nil {
		return CloneVMParams{}, fmt.Errorf(
			"%w: existing VM %s has incomplete host or resource-pool placement",
			ErrPlacementUnavailable,
			vmMoref,
		)
	}
	params.HostMoRef = props.Runtime.Host.Value
	params.ResourcePoolMoRef = props.ResourcePool.Value
	return c.resolveClonePlacement(ctx, params, true)
}

func (c *Client) resolveClonePlacement(
	ctx context.Context,
	params CloneVMParams,
	skipCapacityChecks bool,
) (CloneVMParams, error) {
	sources := params.SourceCandidates
	if len(sources) == 0 {
		sources = []CloneSource{{
			ReplicaID:            params.SourceReplicaID,
			Ref:                  params.TemplateName,
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoRef: params.ComputeResourceMoRef,
		}}
	}
	type resolvedClonePlacement struct {
		params    CloneVMParams
		placement *Placement
	}
	var (
		resolved         []resolvedClonePlacement
		diagnostics      []string
		headroomRejected bool
	)
	for _, candidate := range sources {
		source, err := c.resolveSourceVM(ctx, candidate.Ref)
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("source %s: %v", candidate.Ref, err))
			continue
		}
		var sourceProps mo.VirtualMachine
		if err := source.Properties(ctx, source.Reference(), []string{"runtime.host"}, &sourceProps); err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("source %s host: %v", candidate.Ref, err))
			continue
		}
		placement, err := c.ResolvePlacement(ctx, PlacementRequest{
			SourceHost:            sourceProps.Runtime.Host,
			RequireSourceHost:     true,
			DatastoreName:         c.config.Datastore,
			NetworkName:           params.Network,
			VCPUs:                 params.VCPUs,
			RAMMB:                 params.RAMmb,
			PinnedHostMoRef:       params.HostMoRef,
			PinnedPoolMoRef:       params.ResourcePoolMoRef,
			ExpectedComputeType:   candidate.ComputeResourceType,
			ExpectedComputeMoRef:  candidate.ComputeResourceMoRef,
			AllowMissingNetwork:   params.AllowMissingNetwork,
			SkipCapacityChecks:    skipCapacityChecks,
			PlannedMemoryMBByHost: params.PlannedMemoryMBByHost,
			TargetHostMoRefs:      params.TargetHostMoRefs,
		})
		if err != nil {
			if errors.Is(err, ErrReservedHeadroom) {
				headroomRejected = true
			}
			diagnostics = append(diagnostics, fmt.Sprintf("source %s: %v", candidate.Ref, err))
			continue
		}
		selected := params
		selected.TemplateName = candidate.Ref
		selected.SourceReplicaID = candidate.ReplicaID
		selected.ComputeResourceType = placement.ComputeType
		selected.ComputeResourceMoRef = placement.ComputeMoRef
		selected.HostMoRef = placement.Identity.MoRef
		selected.HostName = placement.Identity.Name
		selected.ResourcePoolMoRef = placement.PoolMoRef
		if placement.ComputeType == "ClusterComputeResource" {
			selected.DRSControl = "disabled"
		} else {
			selected.DRSControl = "standalone"
		}
		selected.ObservedFreeMemoryMB = placement.FreeHostMB
		selected.ReservedMemoryMB = placement.ReservedMemoryMB
		selected.PlannedMemoryMBByHost = nil
		selected.TargetHostMoRefs = nil
		selected.SourceCandidates = nil
		resolved = append(resolved, resolvedClonePlacement{params: selected, placement: placement})
	}
	if len(resolved) == 0 {
		if len(diagnostics) == 0 {
			diagnostics = append(diagnostics, "no source candidates were provided")
		}
		if headroomRejected {
			return CloneVMParams{}, fmt.Errorf(
				"%w: %w: %s",
				ErrPlacementUnavailable,
				ErrReservedHeadroom,
				strings.Join(diagnostics, "; "),
			)
		}
		return CloneVMParams{}, fmt.Errorf("%w: %s", ErrPlacementUnavailable, strings.Join(diagnostics, "; "))
	}
	sort.SliceStable(resolved, func(i, j int) bool {
		if resolved[i].placement.FreePoolMB != resolved[j].placement.FreePoolMB {
			return resolved[i].placement.FreePoolMB > resolved[j].placement.FreePoolMB
		}
		if resolved[i].placement.FreeHostMB != resolved[j].placement.FreeHostMB {
			return resolved[i].placement.FreeHostMB > resolved[j].placement.FreeHostMB
		}
		if resolved[i].params.HostName != resolved[j].params.HostName {
			return resolved[i].params.HostName < resolved[j].params.HostName
		}
		return resolved[i].params.TemplateName < resolved[j].params.TemplateName
	})
	return resolved[0].params, nil
}

// ValidateVMPlacement rejects an existing or recovered VM that is not on the
// expected immutable host, or whose host is no longer allowlisted.
func (c *Client) ValidateVMPlacement(ctx context.Context, vmMoref, expectedHostMoRef string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return fmt.Errorf("%w: connect to vCenter: %w", ErrPlacementValidationUnavailable, err)
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type:  "VirtualMachine",
		Value: vmMoref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"runtime.host"}, &props); err != nil {
		return fmt.Errorf("%w: read VM %s placement: %w", ErrPlacementValidationUnavailable, vmMoref, err)
	}
	if props.Runtime.Host == nil || props.Runtime.Host.Value == "" {
		return newPlacementDrift(
			PlacementDriftHost,
			ErrHostNotAllowed,
			"VM %s has no runtime host assignment",
			vmMoref,
		)
	}
	if _, err := c.allowedHostByMoRef(props.Runtime.Host.Value); err != nil {
		return newPlacementDrift(
			PlacementDriftHost,
			err,
			"VM %s is on %s",
			vmMoref,
			props.Runtime.Host.Value,
		)
	}
	if expectedHostMoRef != "" && props.Runtime.Host.Value != expectedHostMoRef {
		return newPlacementDrift(
			PlacementDriftHost,
			ErrHostNotAllowed,
			"VM %s is on %s, expected persisted host %s",
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
