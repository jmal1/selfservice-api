package vcenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	ReplicaBuildMarkerKey       = "guestinfo.crucible.replica_build_id"
	ReplicaBuildKindKey         = "guestinfo.crucible.replica_build_kind"
	ReplicaBuildDestinationKind = "retained"
	ReplicaBuildCanaryKind      = "acceptance_canary"
)

var ErrReplicaBuildAmbiguous = errors.New("template replica build ownership is ambiguous")

// ReplicaBuildTarget is an explicit immutable vCenter destination. The API
// requires both MoRefs and human-readable inventory identities so typoed or
// stale operator input fails before any mutation.
type ReplicaBuildTarget struct {
	ComputeResourceType  string `json:"compute_resource_type"`
	ComputeResourceMoref string `json:"compute_resource_moref"`
	ComputeResourcePath  string `json:"compute_resource_path"`
	HostMoref            string `json:"host_moref"`
	HostName             string `json:"host_name"`
	ResourcePoolMoref    string `json:"resource_pool_moref"`
	ResourcePoolPath     string `json:"resource_pool_path"`
	DatastoreMoref       string `json:"datastore_moref"`
	DatastoreName        string `json:"datastore_name"`
	FolderMoref          string `json:"folder_moref"`
	FolderPath           string `json:"folder_path"`
	ProvisionDatastore   string `json:"provision_datastore"`
}

type ReplicaBuildCloneParams struct {
	BuildID               string
	OperationID           string
	SourceOperationID     string
	Kind                  string
	TemplateID            string
	SourceReplicaID       string
	SourceVMMoref         string
	SourceSnapshotName    string
	SourceSnapshotMoref   string
	DestinationName       string
	DestinationVMMoref    string
	DestinationSnapshot   string
	Target                ReplicaBuildTarget
	UseProvisionDatastore bool
}

type replicaVMFacts struct {
	VM       *object.VirtualMachine
	Props    mo.VirtualMachine
	Disks    []*types.VirtualDisk
	TPMs     []*types.VirtualTPM
	CDROMs   []*types.VirtualCdrom
	Provider string
}

func (c *Client) ResolveReplicaBuildTarget(
	ctx context.Context,
	requested ReplicaBuildTarget,
) (*ReplicaBuildTarget, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	for field, value := range map[string]string{
		"compute_resource_type":  requested.ComputeResourceType,
		"compute_resource_moref": requested.ComputeResourceMoref,
		"compute_resource_path":  requested.ComputeResourcePath,
		"host_moref":             requested.HostMoref,
		"host_name":              requested.HostName,
		"resource_pool_moref":    requested.ResourcePoolMoref,
		"resource_pool_path":     requested.ResourcePoolPath,
		"datastore_moref":        requested.DatastoreMoref,
		"datastore_name":         requested.DatastoreName,
		"folder_moref":           requested.FolderMoref,
		"folder_path":            requested.FolderPath,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", field)
		}
	}
	switch requested.ComputeResourceType {
	case "ClusterComputeResource", "ComputeResource":
	default:
		return nil, fmt.Errorf("unsupported compute_resource_type %q", requested.ComputeResourceType)
	}

	hostRef := types.ManagedObjectReference{Type: "HostSystem", Value: requested.HostMoref}
	host := object.NewHostSystem(c.client.Client, hostRef)
	var hostProps mo.HostSystem
	if err := host.Properties(ctx, hostRef, []string{
		"name", "parent", "runtime.connectionState", "runtime.inMaintenanceMode",
		"datastore",
	}, &hostProps); err != nil {
		return nil, fmt.Errorf("resolve replica build host %s: %w", requested.HostMoref, err)
	}
	if hostProps.Name != requested.HostName || hostProps.Parent == nil ||
		hostProps.Parent.Type != requested.ComputeResourceType ||
		hostProps.Parent.Value != requested.ComputeResourceMoref {
		return nil, fmt.Errorf("replica build host identity does not match requested compute resource")
	}
	if hostProps.Runtime.ConnectionState != types.HostSystemConnectionStateConnected ||
		hostProps.Runtime.InMaintenanceMode {
		return nil, fmt.Errorf("replica build host %s is not connected and available", requested.HostName)
	}

	computeObject, err := c.finder.ObjectReference(ctx, *hostProps.Parent)
	if err != nil {
		return nil, fmt.Errorf("resolve replica build compute resource: %w", err)
	}
	computePath := inventoryPath(computeObject)
	if computePath != requested.ComputeResourcePath {
		return nil, fmt.Errorf(
			"replica build compute path is %q, expected %q",
			computePath,
			requested.ComputeResourcePath,
		)
	}

	poolRef := types.ManagedObjectReference{Type: "ResourcePool", Value: requested.ResourcePoolMoref}
	pool := object.NewResourcePool(c.client.Client, poolRef)
	var poolProps mo.ResourcePool
	if err := pool.Properties(ctx, poolRef, []string{"owner"}, &poolProps); err != nil {
		return nil, fmt.Errorf("resolve replica build resource pool: %w", err)
	}
	if poolProps.Owner.Type != requested.ComputeResourceType ||
		poolProps.Owner.Value != requested.ComputeResourceMoref {
		return nil, fmt.Errorf("replica build resource pool is not owned by the requested compute resource")
	}
	poolObject, err := c.finder.ObjectReference(ctx, poolRef)
	if err != nil {
		return nil, fmt.Errorf("resolve replica build resource pool path: %w", err)
	}
	if got := inventoryPath(poolObject); got != requested.ResourcePoolPath {
		return nil, fmt.Errorf("replica build resource pool path is %q, expected %q", got, requested.ResourcePoolPath)
	}

	dsRef := types.ManagedObjectReference{Type: "Datastore", Value: requested.DatastoreMoref}
	ds := object.NewDatastore(c.client.Client, dsRef)
	var dsProps mo.Datastore
	if err := ds.Properties(ctx, dsRef, []string{"name", "summary.accessible", "host"}, &dsProps); err != nil {
		return nil, fmt.Errorf("resolve replica build datastore: %w", err)
	}
	if dsProps.Name != requested.DatastoreName || !dsProps.Summary.Accessible {
		return nil, fmt.Errorf("replica build datastore identity is stale or inaccessible")
	}
	mounted := false
	for _, mount := range dsProps.Host {
		if mount.Key.Value == requested.HostMoref && mountAccessible(mount.MountInfo, dsProps.Summary.Accessible) {
			mounted = true
			break
		}
	}
	if !mounted {
		return nil, fmt.Errorf("replica build datastore %s is not accessible from host %s", requested.DatastoreName, requested.HostName)
	}

	folderRef := types.ManagedObjectReference{Type: "Folder", Value: requested.FolderMoref}
	folderObject, err := c.finder.ObjectReference(ctx, folderRef)
	if err != nil {
		return nil, fmt.Errorf("resolve replica build folder: %w", err)
	}
	if _, ok := folderObject.(*object.Folder); !ok {
		return nil, fmt.Errorf("replica build folder %s resolved as %T", requested.FolderMoref, folderObject)
	}
	if got := inventoryPath(folderObject); got != requested.FolderPath {
		return nil, fmt.Errorf("replica build folder path is %q, expected %q", got, requested.FolderPath)
	}

	requested.ProvisionDatastore = c.config.Datastore
	if requested.ProvisionDatastore == "" {
		return nil, errors.New("configured provisioning datastore is empty")
	}
	provisionDatastore, err := c.finder.Datastore(ctx, requested.ProvisionDatastore)
	if err != nil {
		return nil, fmt.Errorf("resolve configured provisioning datastore %q: %w", requested.ProvisionDatastore, err)
	}
	var provisionProps mo.Datastore
	if err := provisionDatastore.Properties(
		ctx,
		provisionDatastore.Reference(),
		[]string{"summary.accessible", "host"},
		&provisionProps,
	); err != nil {
		return nil, fmt.Errorf("read configured provisioning datastore: %w", err)
	}
	provisionMounted := false
	for _, mount := range provisionProps.Host {
		if mount.Key.Value == requested.HostMoref &&
			mountAccessible(mount.MountInfo, provisionProps.Summary.Accessible) {
			provisionMounted = true
			break
		}
	}
	if !provisionMounted {
		return nil, fmt.Errorf(
			"configured provisioning datastore %s is not accessible from host %s",
			requested.ProvisionDatastore,
			requested.HostName,
		)
	}
	return &requested, nil
}

// ValidateReplicaBuildPrivileges checks only the entity-scoped privileges used
// by retained replica construction. It deliberately does not require target
// host membership in the provisioning allowlist.
func (c *Client) ValidateReplicaBuildPrivileges(
	ctx context.Context,
	sourceVMMoref string,
	target ReplicaBuildTarget,
) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	session, err := c.client.SessionManager.UserSession(ctx)
	if err != nil {
		return fmt.Errorf("load vCenter session for privilege validation: %w", err)
	}
	if session == nil || session.Key == "" {
		return errors.New("vCenter session has no identity for privilege validation")
	}
	sourceFacts, err := c.readReplicaVMFacts(ctx, sourceVMMoref)
	if err != nil {
		return fmt.Errorf("read replica source for privilege validation: %w", err)
	}
	sourcePrivileges := []string{"VirtualMachine.Provisioning.Clone"}
	folderPrivileges := []string{
		"VirtualMachine.Inventory.Create",
		"VirtualMachine.Inventory.Delete",
		"VirtualMachine.State.CreateSnapshot",
		"VirtualMachine.Config.AdvancedConfig",
		"VirtualMachine.Provisioning.Clone",
	}
	if len(sourceFacts.TPMs) != 0 {
		sourcePrivileges = append(sourcePrivileges, "Cryptographer.Clone")
		folderPrivileges = append(folderPrivileges, "Cryptographer.Clone")
	}
	checks := []struct {
		entity     types.ManagedObjectReference
		privileges []string
	}{
		{
			entity:     types.ManagedObjectReference{Type: "VirtualMachine", Value: sourceVMMoref},
			privileges: sourcePrivileges,
		},
		{
			entity: types.ManagedObjectReference{Type: "Folder", Value: target.FolderMoref},
			privileges: folderPrivileges,
		},
		{
			entity:     types.ManagedObjectReference{Type: "ResourcePool", Value: target.ResourcePoolMoref},
			privileges: []string{"Resource.AssignVMToPool"},
		},
		{
			entity:     types.ManagedObjectReference{Type: "Datastore", Value: target.DatastoreMoref},
			privileges: []string{"Datastore.AllocateSpace"},
		},
	}
	provisionDatastore, err := c.finder.Datastore(ctx, target.ProvisionDatastore)
	if err != nil {
		return fmt.Errorf("resolve provisioning datastore for privilege validation: %w", err)
	}
	if provisionDatastore.Reference().Value != target.DatastoreMoref {
		checks = append(checks, struct {
			entity     types.ManagedObjectReference
			privileges []string
		}{
			entity:     provisionDatastore.Reference(),
			privileges: []string{"Datastore.AllocateSpace"},
		})
	}
	manager := object.NewAuthorizationManager(c.client.Client)
	for _, check := range checks {
		granted, err := manager.HasPrivilegeOnEntity(ctx, check.entity, session.Key, check.privileges)
		if err != nil {
			return fmt.Errorf("validate privileges on %s: %w", check.entity, err)
		}
		var missing []string
		for i, privilege := range check.privileges {
			if i >= len(granted) || !granted[i] {
				missing = append(missing, privilege)
			}
		}
		if len(missing) != 0 {
			return fmt.Errorf(
				"vCenter service account lacks %s on %s",
				strings.Join(missing, ", "),
				check.entity,
			)
		}
	}
	return nil
}

func inventoryPath(ref object.Reference) string {
	switch v := ref.(type) {
	case *object.ClusterComputeResource:
		return v.InventoryPath
	case *object.ComputeResource:
		return v.InventoryPath
	case *object.ResourcePool:
		return v.InventoryPath
	case *object.Datastore:
		return v.InventoryPath
	case *object.Folder:
		return v.InventoryPath
	default:
		return ""
	}
}

func replicaBuildExtraConfig(buildID, operationID, kind string) []types.BaseOptionValue {
	return []types.BaseOptionValue{
		&types.OptionValue{Key: ReplicaBuildMarkerKey, Value: buildID},
		&types.OptionValue{Key: CloneOperationIDKey, Value: operationID},
		&types.OptionValue{Key: ReplicaBuildKindKey, Value: kind},
	}
}

func findSnapshotByName(nodes []types.VirtualMachineSnapshotTree, name string) (types.ManagedObjectReference, error) {
	var matches []types.ManagedObjectReference
	var walk func([]types.VirtualMachineSnapshotTree)
	walk = func(children []types.VirtualMachineSnapshotTree) {
		for _, node := range children {
			if node.Name == name {
				matches = append(matches, node.Snapshot)
			}
			walk(node.ChildSnapshotList)
		}
	}
	walk(nodes)
	switch len(matches) {
	case 0:
		return types.ManagedObjectReference{}, fmt.Errorf("snapshot %q was not found", name)
	case 1:
		return matches[0], nil
	default:
		return types.ManagedObjectReference{}, fmt.Errorf("snapshot %q is ambiguous across %d entries", name, len(matches))
	}
}

func (c *Client) validateReplicaBuildTarget(
	ctx context.Context,
	target ReplicaBuildTarget,
) error {
	_, err := c.ResolveReplicaBuildTarget(ctx, target)
	return err
}

func (c *Client) StartReplicaBuildClone(
	ctx context.Context,
	params ReplicaBuildCloneParams,
	arm func(context.Context) error,
) (string, string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", "", err
	}
	if params.BuildID == "" || params.OperationID == "" ||
		params.SourceVMMoref == "" || params.SourceSnapshotName == "" ||
		params.SourceSnapshotMoref == "" || params.DestinationName == "" {
		return "", "", errors.New("replica build clone identity is incomplete")
	}
	if _, err := c.ResolveReplicaBuildTarget(ctx, params.Target); err != nil {
		return "", "", err
	}
	source := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: params.SourceVMMoref,
	})
	var sourceProps mo.VirtualMachine
	if err := source.Properties(ctx, source.Reference(), []string{
		"runtime.powerState", "snapshot", "config.hardware.device",
	}, &sourceProps); err != nil {
		return "", "", fmt.Errorf("read source replica: %w", err)
	}
	if sourceProps.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		return "", "", fmt.Errorf("source replica %s must be powered off", params.SourceVMMoref)
	}
	if sourceProps.Snapshot == nil {
		return "", "", fmt.Errorf("source replica %s has no snapshot tree", params.SourceVMMoref)
	}
	snapshotRef, err := findSnapshotByName(sourceProps.Snapshot.RootSnapshotList, params.SourceSnapshotName)
	if err != nil {
		return "", "", err
	}
	if snapshotRef.Value != params.SourceSnapshotMoref {
		return "", "", fmt.Errorf(
			"source snapshot drifted before clone submission: current %s, persisted %s",
			snapshotRef.Value,
			params.SourceSnapshotMoref,
		)
	}
	if params.SourceSnapshotMoref != "" && params.SourceSnapshotMoref != snapshotRef.Value {
		return "", "", fmt.Errorf("source snapshot drifted from %s to %s", params.SourceSnapshotMoref, snapshotRef.Value)
	}

	folder := object.NewFolder(c.client.Client, types.ManagedObjectReference{
		Type: "Folder", Value: params.Target.FolderMoref,
	})
	existing, err := c.findVMInFolderStrict(ctx, folder, params.DestinationName)
	if err != nil {
		return "", "", err
	}
	if existing != "" {
		return "", "", fmt.Errorf("%w: destination name %q already exists", ErrReplicaBuildAmbiguous, params.DestinationName)
	}

	poolRef := types.ManagedObjectReference{Type: "ResourcePool", Value: params.Target.ResourcePoolMoref}
	dsRef := types.ManagedObjectReference{Type: "Datastore", Value: params.Target.DatastoreMoref}
	hostRef := types.ManagedObjectReference{Type: "HostSystem", Value: params.Target.HostMoref}
	folderRef := types.ManagedObjectReference{Type: "Folder", Value: params.Target.FolderMoref}
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:         &poolRef,
			Datastore:    &dsRef,
			Host:         &hostRef,
			Folder:       &folderRef,
			DiskMoveType: string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndDisallowSharing),
		},
		PowerOn:  false,
		Template: false,
		Snapshot: &snapshotRef,
		Config: &types.VirtualMachineConfigSpec{
			ExtraConfig: replicaBuildExtraConfig(params.BuildID, params.OperationID, ReplicaBuildDestinationKind),
		},
	}
	if _, err := applyVTPMClonePolicy(ctx, source, &cloneSpec); err != nil {
		return "", "", err
	}
	if arm != nil {
		if err := arm(ctx); err != nil {
			return "", "", fmt.Errorf("arm replica build clone: %w", err)
		}
	}
	task, err := source.Clone(ctx, folder, params.DestinationName, cloneSpec)
	if err != nil {
		if isDuplicateNameErr(err) {
			return "", "", fmt.Errorf("%w: %v", ErrReplicaBuildAmbiguous, err)
		}
		return "", "", fmt.Errorf("submit replica build clone: %w", err)
	}
	return task.Reference().Value, params.SourceSnapshotMoref, nil
}

func (c *Client) findReplicaBuildVM(
	ctx context.Context,
	params ReplicaBuildCloneParams,
	kind string,
) (string, error) {
	folder := object.NewFolder(c.client.Client, types.ManagedObjectReference{
		Type: "Folder", Value: params.Target.FolderMoref,
	})
	children, err := folder.Children(ctx)
	if err != nil {
		return "", err
	}
	var matching string
	for _, child := range children {
		vm, ok := child.(*object.VirtualMachine)
		if !ok {
			continue
		}
		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"name", "config.extraConfig"}, &props); err != nil {
			return "", err
		}
		if props.Name != params.DestinationName {
			continue
		}
		if props.Config == nil ||
			optionValueString(props.Config.ExtraConfig, ReplicaBuildMarkerKey) != params.BuildID ||
			optionValueString(props.Config.ExtraConfig, CloneOperationIDKey) != params.OperationID ||
			optionValueString(props.Config.ExtraConfig, ReplicaBuildKindKey) != kind {
			return "", fmt.Errorf("%w: VM %q exists without the exact build marker", ErrReplicaBuildAmbiguous, params.DestinationName)
		}
		if matching != "" {
			return "", fmt.Errorf("%w: multiple VMs match build %s", ErrReplicaBuildAmbiguous, params.BuildID)
		}
		matching = vm.Reference().Value
	}
	return matching, nil
}

func (c *Client) FindReplicaBuildVM(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	return c.findReplicaBuildVM(ctx, params, ReplicaBuildDestinationKind)
}

func (c *Client) waitTaskResult(
	ctx context.Context,
	taskRef string,
	resultType string,
) (string, error) {
	if taskRef == "" {
		return "", errors.New("vCenter task reference is empty")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	overallCtx, cancel := context.WithTimeout(ctx, cloneTaskOperationalTimeout)
	defer cancel()
	task := object.NewTask(c.client.Client, types.ManagedObjectReference{Type: "Task", Value: taskRef})
	for {
		waitCtx, cancelWait := context.WithTimeout(overallCtx, cloneTaskWaitSlice)
		info, err := task.WaitForResult(waitCtx, nil)
		cancelWait()
		if err == nil {
			if resultType == "" {
				return "", nil
			}
			ref, ok := info.Result.(types.ManagedObjectReference)
			if !ok || ref.Type != resultType || ref.Value == "" {
				return "", fmt.Errorf("task %s returned unexpected result %T", taskRef, info.Result)
			}
			return ref.Value, nil
		}
		if errors.Is(err, context.DeadlineExceeded) && overallCtx.Err() == nil {
			continue
		}
		if overallCtx.Err() != nil {
			return "", fmt.Errorf("wait for task %s: %w", taskRef, overallCtx.Err())
		}
		return "", fmt.Errorf("%w: task %s: %v", ErrCloneTaskFailed, taskRef, err)
	}
}

func (c *Client) WaitReplicaBuildCloneTask(ctx context.Context, taskRef string) (string, error) {
	return c.waitTaskResult(ctx, taskRef, "VirtualMachine")
}

func (c *Client) readReplicaVMFacts(ctx context.Context, moref string) (*replicaVMFacts, error) {
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: moref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{
		"runtime.powerState", "runtime.host", "resourcePool", "parent", "datastore",
		"snapshot", "config",
	}, &props); err != nil {
		return nil, err
	}
	if props.Config == nil {
		return nil, fmt.Errorf("VM %s configuration is unavailable", moref)
	}
	facts := &replicaVMFacts{VM: vm, Props: props}
	if props.Config.KeyId != nil && props.Config.KeyId.ProviderId != nil {
		facts.Provider = props.Config.KeyId.ProviderId.Id
	}
	for _, device := range props.Config.Hardware.Device {
		switch typed := device.(type) {
		case *types.VirtualDisk:
			facts.Disks = append(facts.Disks, typed)
		case *types.VirtualTPM:
			facts.TPMs = append(facts.TPMs, typed)
		case *types.VirtualCdrom:
			facts.CDROMs = append(facts.CDROMs, typed)
		}
	}
	return facts, nil
}

func secureBoot(config *types.VirtualMachineConfigInfo) bool {
	return config != nil && config.BootOptions != nil &&
		config.BootOptions.EfiSecureBootEnabled != nil &&
		*config.BootOptions.EfiSecureBootEnabled
}

func flatDiskBacking(disk *types.VirtualDisk) (*types.VirtualDiskFlatVer2BackingInfo, error) {
	backing, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	if !ok {
		return nil, fmt.Errorf("disk %d has unsupported backing %T", disk.Key, disk.Backing)
	}
	return backing, nil
}

func tpmIdentityHashes(tpms []*types.VirtualTPM) []string {
	var hashes []string
	for _, tpm := range tpms {
		for _, der := range append(
			append([][]byte(nil), tpm.EndorsementKeyCertificate...),
			tpm.EndorsementKeyCertificateSigningRequest...,
		) {
			sum := sha256.Sum256(der)
			hashes = append(hashes, hex.EncodeToString(sum[:]))
		}
	}
	sort.Strings(hashes)
	return hashes
}

func setsOverlap(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := seen[value]; ok {
			return true
		}
	}
	return false
}

func validateNoUnexpectedISO(facts *replicaVMFacts) error {
	for _, cdrom := range facts.CDROMs {
		if cdrom.Connectable != nil && (cdrom.Connectable.Connected || cdrom.Connectable.StartConnected) {
			return fmt.Errorf("VM %s has a connected CD-ROM", facts.Props.Config.Name)
		}
		if iso, ok := cdrom.Backing.(*types.VirtualCdromIsoBackingInfo); ok &&
			strings.TrimSpace(iso.FileName) != "" {
			return fmt.Errorf("VM %s retains unexpected ISO %q", facts.Props.Config.Name, iso.FileName)
		}
	}
	return nil
}

func (c *Client) ValidateReplicaBuildClone(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateReplicaBuildTarget(ctx, params.Target); err != nil {
		return err
	}
	source, err := c.readReplicaVMFacts(ctx, params.SourceVMMoref)
	if err != nil {
		return fmt.Errorf("read source replica facts: %w", err)
	}
	destination, err := c.readReplicaVMFacts(ctx, params.DestinationVMMoref)
	if err != nil {
		return fmt.Errorf("read destination replica facts: %w", err)
	}
	if source.Props.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff ||
		destination.Props.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		return errors.New("source and destination replicas must remain powered off")
	}
	if err := validateReplicaSourceSnapshot(
		source.Props.Snapshot,
		params.SourceSnapshotName,
		params.SourceSnapshotMoref,
	); err != nil {
		return err
	}
	if destination.Props.Runtime.Host == nil ||
		destination.Props.Runtime.Host.Value != params.Target.HostMoref ||
		destination.Props.ResourcePool == nil ||
		destination.Props.ResourcePool.Value != params.Target.ResourcePoolMoref ||
		destination.Props.Parent == nil ||
		destination.Props.Parent.Value != params.Target.FolderMoref {
		return errors.New("destination replica placement does not match the immutable target")
	}
	if len(destination.Props.Datastore) != 1 ||
		destination.Props.Datastore[0].Value != params.Target.DatastoreMoref {
		return errors.New("destination replica datastore does not exactly match the immutable target")
	}
	if destination.Props.Snapshot != nil {
		return errors.New("destination replica already has a snapshot before sealing")
	}
	if err := validateReplicaBuildHardware(source, destination); err != nil {
		return err
	}
	return c.ValidateReplicaBuildMarker(ctx, params.DestinationVMMoref, params.BuildID, params.OperationID, ReplicaBuildDestinationKind)
}

func validateReplicaSourceSnapshot(
	info *types.VirtualMachineSnapshotInfo,
	name, persistedMoref string,
) error {
	if info == nil {
		return errors.New("source replica snapshot tree disappeared")
	}
	current, err := findSnapshotByName(info.RootSnapshotList, name)
	if err != nil {
		return err
	}
	if persistedMoref == "" || current.Value != persistedMoref {
		return fmt.Errorf(
			"source snapshot drifted: current %s, persisted %s",
			current.Value,
			persistedMoref,
		)
	}
	return nil
}

func validateReplicaBuildHardware(source, destination *replicaVMFacts) error {
	if source == nil || destination == nil || source.Props.Config == nil || destination.Props.Config == nil {
		return errors.New("source and destination configuration facts are required")
	}
	if destination.Props.Config.Firmware != source.Props.Config.Firmware ||
		secureBoot(destination.Props.Config) != secureBoot(source.Props.Config) ||
		len(destination.TPMs) != len(source.TPMs) ||
		destination.Provider != source.Provider {
		return errors.New("destination firmware, Secure Boot, vTPM, or security provider differs from source")
	}
	if destination.Props.Config.KeyId == nil || destination.Props.Config.KeyId.KeyId == "" {
		return errors.New("destination replica has an empty configuration encryption key")
	}
	if len(source.Disks) != len(destination.Disks) {
		return errors.New("destination disk count differs from source")
	}
	sourceFiles := make(map[string]struct{}, len(source.Disks))
	for _, disk := range source.Disks {
		backing, err := flatDiskBacking(disk)
		if err != nil {
			return fmt.Errorf("source %w", err)
		}
		sourceFiles[backing.FileName] = struct{}{}
	}
	for _, disk := range destination.Disks {
		backing, err := flatDiskBacking(disk)
		if err != nil {
			return fmt.Errorf("destination %w", err)
		}
		if backing.Parent != nil {
			return fmt.Errorf("destination disk %d still has a parent backing", disk.Key)
		}
		if _, reused := sourceFiles[backing.FileName]; reused {
			return fmt.Errorf("destination disk %d reuses source backing %q", disk.Key, backing.FileName)
		}
		if backing.DiskMode != string(types.VirtualDiskModePersistent) {
			return fmt.Errorf("destination disk %d has unexpected mode %q", disk.Key, backing.DiskMode)
		}
	}
	if err := validateNoUnexpectedISO(destination); err != nil {
		return err
	}
	if len(source.TPMs) > 0 {
		sourceHashes := tpmIdentityHashes(source.TPMs)
		destinationHashes := tpmIdentityHashes(destination.TPMs)
		if len(sourceHashes) == 0 || len(destinationHashes) == 0 {
			return errors.New("vTPM source and destination must expose EK certificate or CSR identity")
		}
		if setsOverlap(sourceHashes, destinationHashes) {
			return errors.New("destination vTPM EK certificate/CSR identity was reused from source")
		}
	}
	return nil
}

func (c *Client) ValidateReplicaBuildMarker(
	ctx context.Context,
	moref, buildID, operationID, kind string,
) error {
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: moref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.extraConfig"}, &props); err != nil {
		return err
	}
	if props.Config == nil ||
		optionValueString(props.Config.ExtraConfig, ReplicaBuildMarkerKey) != buildID ||
		optionValueString(props.Config.ExtraConfig, CloneOperationIDKey) != operationID ||
		optionValueString(props.Config.ExtraConfig, ReplicaBuildKindKey) != kind {
		return ErrReplicaBuildAmbiguous
	}
	return nil
}

func (c *Client) StartReplicaBuildSnapshot(
	ctx context.Context,
	moref, buildID, operationID, name string,
	arm func(context.Context) error,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	if err := c.ValidateReplicaBuildMarker(ctx, moref, buildID, operationID, ReplicaBuildDestinationKind); err != nil {
		return "", err
	}
	facts, err := c.readReplicaVMFacts(ctx, moref)
	if err != nil {
		return "", err
	}
	if facts.Props.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		return "", errors.New("destination replica must be powered off before snapshot")
	}
	if facts.Props.Snapshot != nil {
		return "", errors.New("destination replica already has a snapshot")
	}
	if arm != nil {
		if err := arm(ctx); err != nil {
			return "", err
		}
	}
	task, err := facts.VM.CreateSnapshot(ctx, name, "Crucible retained source replica clone base", false, false)
	if err != nil {
		return "", fmt.Errorf("submit replica snapshot: %w", err)
	}
	return task.Reference().Value, nil
}

func (c *Client) FindReplicaSnapshot(
	ctx context.Context,
	moref, name string,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: moref,
	})
	var props mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"snapshot"}, &props); err != nil {
		return "", err
	}
	if props.Snapshot == nil {
		return "", nil
	}
	ref, err := findSnapshotByName(props.Snapshot.RootSnapshotList, name)
	if err != nil {
		if strings.Contains(err.Error(), "was not found") {
			return "", nil
		}
		return "", err
	}
	return ref.Value, nil
}

func (c *Client) WaitReplicaBuildSnapshotTask(ctx context.Context, taskRef string) (string, error) {
	return c.waitTaskResult(ctx, taskRef, "VirtualMachineSnapshot")
}

func (c *Client) StartReplicaBuildCanary(
	ctx context.Context,
	params ReplicaBuildCloneParams,
	arm func(context.Context) error,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	if _, err := c.ResolveReplicaBuildTarget(ctx, params.Target); err != nil {
		return "", err
	}
	if params.SourceVMMoref == "" || params.DestinationSnapshot == "" {
		return "", errors.New("accepted replica and snapshot identities are required")
	}
	if params.SourceOperationID == "" {
		return "", errors.New("retained replica operation identity is required")
	}
	if err := c.ValidateReplicaBuildMarker(
		ctx,
		params.SourceVMMoref,
		params.BuildID,
		params.SourceOperationID,
		ReplicaBuildDestinationKind,
	); err != nil {
		return "", err
	}
	source := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: params.SourceVMMoref,
	})
	folder := object.NewFolder(c.client.Client, types.ManagedObjectReference{
		Type: "Folder", Value: params.Target.FolderMoref,
	})
	if existing, err := c.findVMInFolderStrict(ctx, folder, params.DestinationName); err != nil {
		return "", err
	} else if existing != "" {
		return "", fmt.Errorf("%w: canary name %q already exists", ErrReplicaBuildAmbiguous, params.DestinationName)
	}
	provisionDS, err := c.finder.Datastore(ctx, params.Target.ProvisionDatastore)
	if err != nil {
		return "", err
	}
	poolRef := types.ManagedObjectReference{Type: "ResourcePool", Value: params.Target.ResourcePoolMoref}
	hostRef := types.ManagedObjectReference{Type: "HostSystem", Value: params.Target.HostMoref}
	dsRef := provisionDS.Reference()
	folderRef := types.ManagedObjectReference{Type: "Folder", Value: params.Target.FolderMoref}
	snapshotRef := types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: params.DestinationSnapshot}
	spec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:         &poolRef,
			Host:         &hostRef,
			Datastore:    &dsRef,
			Folder:       &folderRef,
			DiskMoveType: string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking),
		},
		PowerOn:  false,
		Template: false,
		Snapshot: &snapshotRef,
		Config: &types.VirtualMachineConfigSpec{
			ExtraConfig: replicaBuildExtraConfig(params.BuildID, params.OperationID, ReplicaBuildCanaryKind),
		},
	}
	if _, err := applyVTPMClonePolicy(ctx, source, &spec); err != nil {
		return "", err
	}
	if arm != nil {
		if err := arm(ctx); err != nil {
			return "", err
		}
	}
	task, err := source.Clone(ctx, folder, params.DestinationName, spec)
	if err != nil {
		return "", fmt.Errorf("submit replica acceptance canary: %w", err)
	}
	return task.Reference().Value, nil
}

func (c *Client) FindReplicaBuildCanary(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	return c.findReplicaBuildVM(ctx, params, ReplicaBuildCanaryKind)
}

func (c *Client) ValidateReplicaBuildCanary(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.ValidateReplicaBuildMarker(ctx, params.DestinationVMMoref, params.BuildID, params.OperationID, ReplicaBuildCanaryKind); err != nil {
		return err
	}
	canary, err := c.readReplicaVMFacts(ctx, params.DestinationVMMoref)
	if err != nil {
		return err
	}
	if canary.Props.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOff {
		return errors.New("replica acceptance canary unexpectedly powered on")
	}
	if len(canary.Props.Datastore) != 1 {
		return errors.New("replica acceptance canary spans unexpected datastores")
	}
	ds := object.NewDatastore(c.client.Client, canary.Props.Datastore[0])
	var dsProps mo.Datastore
	if err := ds.Properties(ctx, ds.Reference(), []string{"name"}, &dsProps); err != nil {
		return err
	}
	if dsProps.Name != params.Target.ProvisionDatastore {
		return fmt.Errorf("acceptance canary datastore is %q, expected %q", dsProps.Name, params.Target.ProvisionDatastore)
	}
	if len(canary.Disks) == 0 {
		return errors.New("replica acceptance canary has no disks")
	}
	retained, err := c.readReplicaVMFacts(ctx, params.SourceVMMoref)
	if err != nil {
		return fmt.Errorf("read retained replica backing facts: %w", err)
	}
	if err := validateReplicaBuildCanaryBackings(retained, canary); err != nil {
		return err
	}
	return validateNoUnexpectedISO(canary)
}

func validateReplicaBuildCanaryBackings(retained, canary *replicaVMFacts) error {
	if retained == nil || canary == nil {
		return errors.New("retained replica and canary backing facts are required")
	}
	if len(retained.Disks) != len(canary.Disks) {
		return errors.New("replica acceptance canary disk count differs from retained replica")
	}
	retainedBackings := make(map[int32]map[string]struct{}, len(retained.Disks))
	for _, disk := range retained.Disks {
		backing, err := flatDiskBacking(disk)
		if err != nil {
			return fmt.Errorf("retained replica %w", err)
		}
		if _, duplicate := retainedBackings[disk.Key]; duplicate {
			return fmt.Errorf("retained replica has duplicate disk key %d", disk.Key)
		}
		chain := make(map[string]struct{})
		for current := backing; current != nil; current = current.Parent {
			if current.FileName != "" {
				chain[current.FileName] = struct{}{}
			}
		}
		retainedBackings[disk.Key] = chain
	}
	for _, disk := range canary.Disks {
		backing, err := flatDiskBacking(disk)
		if err != nil {
			return err
		}
		if backing.Parent == nil || backing.Parent.FileName == "" {
			return fmt.Errorf("acceptance canary disk %d is not linked to an exact parent backing", disk.Key)
		}
		expected, ok := retainedBackings[disk.Key]
		if !ok {
			return fmt.Errorf("acceptance canary disk key %d has no retained counterpart", disk.Key)
		}
		if _, ok := expected[backing.Parent.FileName]; !ok {
			return fmt.Errorf(
				"acceptance canary disk %d parent %q is not an exact retained replica backing",
				disk.Key,
				backing.Parent.FileName,
			)
		}
	}
	return nil
}

func (c *Client) StartReplicaBuildCanaryCleanup(
	ctx context.Context,
	moref, buildID, operationID string,
	arm func(context.Context) error,
) (string, bool, error) {
	return c.startReplicaBuildVMCleanup(
		ctx,
		moref,
		buildID,
		operationID,
		ReplicaBuildCanaryKind,
		arm,
	)
}

func (c *Client) StartReplicaBuildResidueCleanup(
	ctx context.Context,
	moref, buildID, operationID string,
	arm func(context.Context) error,
) (string, bool, error) {
	return c.startReplicaBuildVMCleanup(
		ctx,
		moref,
		buildID,
		operationID,
		ReplicaBuildDestinationKind,
		arm,
	)
}

func (c *Client) startReplicaBuildVMCleanup(
	ctx context.Context,
	moref, buildID, operationID, kind string,
	arm func(context.Context) error,
) (string, bool, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", false, err
	}
	if err := c.ValidateReplicaBuildMarker(ctx, moref, buildID, operationID, kind); err != nil {
		if isAlreadyDeletedErr(err) || isResourceNotFoundErr(err) {
			return "", true, nil
		}
		return "", false, err
	}
	if arm != nil {
		if err := arm(ctx); err != nil {
			return "", false, err
		}
	}
	vm := object.NewVirtualMachine(c.client.Client, types.ManagedObjectReference{
		Type: "VirtualMachine", Value: moref,
	})
	task, err := vm.Destroy(ctx)
	if err != nil {
		if isAlreadyDeletedErr(err) || isResourceNotFoundErr(err) {
			return "", true, nil
		}
		return "", false, fmt.Errorf("submit acceptance canary cleanup: %w", err)
	}
	return task.Reference().Value, false, nil
}

func (c *Client) WaitReplicaBuildCleanupTask(ctx context.Context, taskRef string) error {
	_, err := c.waitTaskResult(ctx, taskRef, "")
	return err
}

func (c *Client) ReplicaBuildCanaryExists(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) (bool, error) {
	if params.DestinationVMMoref == "" {
		return false, errors.New("acceptance canary exact MoRef is required")
	}
	return c.replicaBuildExactVMExists(
		ctx,
		params.DestinationVMMoref,
		params.BuildID,
		params.OperationID,
		ReplicaBuildCanaryKind,
	)
}

func (c *Client) ReplicaBuildRetainedVMExists(
	ctx context.Context,
	params ReplicaBuildCloneParams,
) (bool, error) {
	if params.DestinationVMMoref == "" {
		return false, errors.New("retained replica exact MoRef is required")
	}
	return c.replicaBuildExactVMExists(
		ctx,
		params.DestinationVMMoref,
		params.BuildID,
		params.OperationID,
		ReplicaBuildDestinationKind,
	)
}

func (c *Client) replicaBuildExactVMExists(
	ctx context.Context,
	moref, buildID, operationID, kind string,
) (bool, error) {
	err := c.ValidateReplicaBuildMarker(ctx, moref, buildID, operationID, kind)
	if err == nil {
		return true, nil
	}
	if isAlreadyDeletedErr(err) || isResourceNotFoundErr(err) {
		return false, nil
	}
	return false, err
}
