package vcenter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	vimtask "github.com/vmware/govmomi/task"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	cloneTaskWaitSlice          = 2 * time.Minute
	cloneTaskOperationalTimeout = 15 * time.Minute

	CloneOperationIDKey          = "guestinfo.crucible.clone_operation_id"
	CloneOperationSourceKey      = "guestinfo.crucible.clone_source_ref"
	CloneOperationPodVMKey       = "guestinfo.crucible.clone_pod_vm_id"
	CloneOperationHostKey        = "guestinfo.crucible.clone_host_moref"
	CloneOperationPoolKey        = "guestinfo.crucible.clone_pool_moref"
	CloneOperationComputeTypeKey = "guestinfo.crucible.clone_compute_type"
	CloneOperationComputeKey     = "guestinfo.crucible.clone_compute_moref"
	CloneOperationReplicaKey     = "guestinfo.crucible.clone_source_replica_id"
	CloneOperationTemplateKey    = "guestinfo.crucible.logical_template_id"
)

var ErrAmbiguousVMOwnership = errors.New("VM ownership cannot be proven")
var ErrCloneTaskFailed = errors.New("vCenter clone task failed")

func cloneOperationExtraConfig(params CloneVMParams) ([]types.BaseOptionValue, error) {
	if params.OperationID == "" {
		return nil, nil
	}
	if params.PodVMID == "" {
		return nil, errors.New("clone operation requires pod VM identity")
	}
	if params.LogicalTemplateID == "" ||
		params.ComputeResourceType == "" || params.ComputeResourceMoRef == "" ||
		params.HostMoRef == "" || params.ResourcePoolMoRef == "" {
		return nil, errors.New("clone operation requires complete template, compute, host, and resource pool identities")
	}
	return []types.BaseOptionValue{
		&types.OptionValue{Key: CloneOperationIDKey, Value: params.OperationID},
		&types.OptionValue{Key: CloneOperationSourceKey, Value: params.TemplateName},
		&types.OptionValue{Key: CloneOperationPodVMKey, Value: params.PodVMID},
		&types.OptionValue{Key: CloneOperationHostKey, Value: params.HostMoRef},
		&types.OptionValue{Key: CloneOperationPoolKey, Value: params.ResourcePoolMoRef},
		&types.OptionValue{Key: CloneOperationComputeTypeKey, Value: params.ComputeResourceType},
		&types.OptionValue{Key: CloneOperationComputeKey, Value: params.ComputeResourceMoRef},
		&types.OptionValue{Key: CloneOperationReplicaKey, Value: params.SourceReplicaID},
		&types.OptionValue{Key: CloneOperationTemplateKey, Value: params.LogicalTemplateID},
	}, nil
}

// Config holds vCenter connection settings.
type Config struct {
	URL        string // e.g., "https://vcenter.lab.jmal.io/sdk"
	User       string // e.g., "selfservice-svc@vsphere.local"
	Password   string
	Datacenter string // e.g., "JMAL-Datacenter"
	Datastore  string // e.g., "NAS-vmstore"
	VMFolder   string // e.g., "Student-VMs"
	// TemplateFolder is where template *build* VMs live, e.g.
	// "/JMAL-Datacenter/vm/templates". It is deliberately separate from
	// VMFolder: VMFolder holds ephemeral student pod VMs and is what the
	// orphan reconciler scans, so a long-lived template shell parked there
	// would be reported as an orphan forever. CreateBlankVM falls back to
	// this when the caller does not name a folder.
	TemplateFolder       string
	ResourcePools        []string // e.g., ["AMD-Cluster/Resources/Student-VMs", "Intel-Cluster/Resources/Student-VMs"]
	Hosts                []string // canonical ESXi allowlist for placement and standard-switch mutation
	HostReservedMemoryMB map[string]int64
	Insecure             bool // skip TLS verification
}

// Client wraps govmomi for self-service provisioning operations.
type Client struct {
	config       Config
	client       *govmomi.Client
	finder       *find.Finder
	datacenter   *object.Datacenter
	mu           sync.Mutex
	hostMu       sync.RWMutex
	allowedHosts []HostIdentity
	logger       *slog.Logger

	portGroupKeyOverride func(HostIdentity, types.HostPortGroup) string
}

// New creates a vCenter client (does not connect yet).
func New(cfg Config, logger *slog.Logger) *Client {
	return &Client{
		config: cfg,
		logger: logger,
	}
}

// Connect establishes a session with vCenter.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx)
}

// connectLocked does the actual connection work. Caller must hold c.mu.
func (c *Client) connectLocked(ctx context.Context) error {
	u, err := soap.ParseURL(c.config.URL)
	if err != nil {
		return fmt.Errorf("parse vCenter URL: %w", err)
	}
	u.User = url.UserPassword(c.config.User, c.config.Password)

	client, err := govmomi.NewClient(ctx, u, c.config.Insecure)
	if err != nil {
		return fmt.Errorf("connect to vCenter: %w", err)
	}

	// KeepAliveHandler with re-login: fires after 10 min idle, re-authenticates
	// if the session has expired. NEVER return a non-nil error from the handler —
	// that kills the keepalive goroutine permanently. If re-login fails here,
	// withRetry() on the next real operation will do a full reconnect.
	client.RoundTripper = session.KeepAliveHandler(client.RoundTripper, 10*time.Minute,
		func(rt soap.RoundTripper) error {
			ctx := context.Background()
			mgr := session.NewManager(client.Client)
			active, err := mgr.SessionIsActive(ctx)
			if err == nil && active {
				return nil
			}
			c.logger.Info("vCenter session expired, re-authenticating via keepalive")
			if loginErr := mgr.Login(ctx, u.User); loginErr != nil {
				c.logger.Error("keepalive re-login failed, will reconnect on next operation", "error", loginErr)
				return nil // keep goroutine alive; withRetry handles full reconnect
			}
			c.logger.Info("vCenter session re-authenticated via keepalive")
			return nil
		},
	)

	c.client = client
	c.finder = find.NewFinder(client.Client, true)

	// Set datacenter
	dc, err := c.finder.Datacenter(ctx, c.config.Datacenter)
	if err != nil {
		return fmt.Errorf("find datacenter %s: %w", c.config.Datacenter, err)
	}
	c.datacenter = dc
	c.finder.SetDatacenter(dc)

	c.logger.Info("connected to vCenter", "url", c.config.URL, "datacenter", c.config.Datacenter)
	return nil
}

// Disconnect closes the vCenter session.
func (c *Client) Disconnect(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		_ = c.client.Logout(ctx)
		c.client = nil
	}
}

// ensureConnected checks the session and reconnects if needed.
func (c *Client) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.client == nil {
		return c.connectLocked(ctx)
	}

	// Check if session is still valid
	mgr := session.NewManager(c.client.Client)
	_, err := mgr.UserSession(ctx)
	if err != nil {
		c.logger.Warn("vCenter session expired, reconnecting", "error", err)
		return c.connectLocked(ctx)
	}
	return nil
}

// Close disconnects from vCenter.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client.Logout(ctx)
	}
	return nil
}

// ---------- VM Operations ----------

// CloneVMParams holds parameters for cloning a VM.
type CloneVMParams struct {
	// TemplateName accepts either a vCenter inventory name (e.g.
	// "student-windows-11") or a managed-object reference like
	// "vm-8942". Wizard-published templates only persist the moref
	// (templates.vcenter_vm_id); legacy templates registered by name
	// persist the inventory name (templates.vcenter_template).
	// resolveSourceVM in cloneVMInner detects which form was passed.
	TemplateName          string
	LogicalTemplateID     string
	SourceReplicaID       string
	ComputeResourceType   string
	ComputeResourceMoRef  string
	VMName                string
	VCPUs                 int32
	RAMmb                 int64
	Network               string // port group name
	OSType                string // "linux" or "windows"
	Password              string // generated password for cloud-init
	OperationID           string // durable provisioning-attempt identity
	PodVMID               string // immutable database VM identity
	HostMoRef             string // immutable allowlisted destination host
	HostName              string // diagnostic name corresponding to HostMoRef
	ResourcePoolMoRef     string // resource pool selected with HostMoRef
	DRSControl            string
	ObservedFreeMemoryMB  int64
	ReservedMemoryMB      int64
	AllowMissingNetwork   bool
	PlannedMemoryMBByHost map[string]int64
	TargetHostMoRefs      []string
	SourceCandidates      []CloneSource
}

// CloneSource is one validated source VM candidate for a logical template.
type CloneSource struct {
	ReplicaID            string
	Ref                  string
	ComputeResourceType  string
	ComputeResourceMoRef string
}

// StartCloneVMOperation runs all read-only clone preparation before invoking
// arm. Once arm succeeds, the next remote call is CloneVM_Task itself.
func (c *Client) StartCloneVMOperation(
	ctx context.Context,
	params CloneVMParams,
	arm func(context.Context) error,
) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	if params.HostMoRef == "" || params.ResourcePoolMoRef == "" ||
		params.ComputeResourceType == "" || params.ComputeResourceMoRef == "" ||
		params.DRSControl == "" {
		return "", errors.New("clone placement must be resolved and persisted before submission")
	}
	return c.startCloneVMInner(ctx, params, arm)
}

// resolveSourceVM looks up a VM by either inventory name or moref. A
// "ref" beginning with "vm-" followed by digits is treated as a
// VirtualMachine managed-object reference and wrapped directly; any
// other value is resolved by name via the finder.
//
// Wizard-published templates persist only the moref
// (templates.vcenter_vm_id, e.g. "vm-8942") because the staging VM
// keeps its original name during publish — there is no friendly
// "vcenter_template" entry to look up. Legacy templates registered
// through the older UI persist the inventory name instead. Both
// shapes need to clone, so we accept either.
func (c *Client) resolveSourceVM(ctx context.Context, ref string) (*object.VirtualMachine, error) {
	if ref == "" {
		return nil, fmt.Errorf("template ref is empty")
	}
	if isVMMoref(ref) {
		return object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: ref}), nil
	}
	return c.finder.VirtualMachine(ctx, ref)
}

// isVMMoref reports whether s looks like a VirtualMachine managed-object
// reference (e.g. "vm-8942"). vCenter morefs use the prefix "vm-"
// followed by one or more digits; no legitimate inventory name should
// match this pattern.
func isVMMoref(s string) bool {
	if !strings.HasPrefix(s, "vm-") {
		return false
	}
	rest := s[3:]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (c *Client) startCloneVMInner(
	ctx context.Context,
	params CloneVMParams,
	arm func(context.Context) error,
) (string, error) {
	// Find template — accepts inventory name OR moref (e.g. "vm-8942").
	template, err := c.resolveSourceVM(ctx, params.TemplateName)
	if err != nil {
		return "", fmt.Errorf("find template %s: %w", params.TemplateName, err)
	}

	// Pre-fetch the template's runtime host so we can constrain the
	// resource-pool pick to the same cluster. Cross-cluster clones
	// (Intel↔AMD) get rejected by vCenter with the misleading
	// "virtual disk is either corrupted or not a supported format"
	// error for Windows guests with cpuid masks (see Round 10 history
	// in template_ops.go). Linked clones may sneak past clone-task
	// validation but then fail at first power-on — same root cause,
	// later surface. Pin to the source cluster up front.
	var tmplProps mo.VirtualMachine
	if err := template.Properties(ctx, template.Reference(), []string{"runtime"}, &tmplProps); err != nil {
		return "", fmt.Errorf("read template runtime: %w", err)
	}

	// Find target folder
	folder, err := c.finder.Folder(ctx, c.config.VMFolder)
	if err != nil {
		return "", fmt.Errorf("find folder %s: %w", c.config.VMFolder, err)
	}

	// Never adopt an existing VM by mutable inventory name. Resume is safe only
	// when the caller already persisted the exact MoRef on its pod_vms row.
	existing, lookupErr := c.findVMInFolderStrict(ctx, folder, params.VMName)
	if lookupErr != nil {
		return "", fmt.Errorf("verify clone target %q is unused: %w", params.VMName, lookupErr)
	}
	if existing != "" {
		return "", fmt.Errorf(
			"%w: clone target %q already exists without immutable ownership proof; manual cleanup required",
			ErrAmbiguousVMOwnership,
			params.VMName,
		)
	}

	// Re-resolve the pinned pair immediately before submission. This is a
	// validation pass, not a new placement decision: both immutable MoRefs came
	// from the durable clone operation.
	placement, err := c.ResolvePlacement(ctx, PlacementRequest{
		SourceHost:           tmplProps.Runtime.Host,
		RequireSourceHost:    true,
		DatastoreName:        c.config.Datastore,
		NetworkName:          params.Network,
		VCPUs:                params.VCPUs,
		RAMMB:                params.RAMmb,
		PinnedHostMoRef:      params.HostMoRef,
		PinnedPoolMoRef:      params.ResourcePoolMoRef,
		ExpectedComputeType:  params.ComputeResourceType,
		ExpectedComputeMoRef: params.ComputeResourceMoRef,
	})
	if err != nil {
		return "", fmt.Errorf("validate persisted clone placement: %w", err)
	}
	pool := placement.Pool
	dsRef := placement.Datastore.Reference()
	hostRef := placement.Host.Reference()

	// Build clone spec — use linked clones for fast provisioning.
	// Linked clones create a thin delta disk referencing the template's snapshot,
	// reducing clone time from minutes (full copy) to seconds.
	poolRef := pool.Reference()
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:      &poolRef,
			Datastore: &dsRef,
			Host:      &hostRef,
		},
		PowerOn:  false,
		Template: false,
	}
	if _, err := applyVTPMClonePolicy(ctx, template, &cloneSpec); err != nil {
		return "", err
	}
	if params.OperationID != "" {
		operationConfig, err := cloneOperationExtraConfig(params)
		if err != nil {
			return "", err
		}
		cloneSpec.Config = &types.VirtualMachineConfigSpec{
			ExtraConfig: operationConfig,
		}
	}

	// Get or create a snapshot on the template for linked cloning
	snapRef, err := c.ensureTemplateSnapshot(ctx, template, params.TemplateName)
	if err != nil {
		c.logger.Warn("linked clone unavailable, falling back to full clone",
			"template", params.TemplateName, "error", err)
	} else {
		cloneSpec.Snapshot = snapRef
		cloneSpec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
		c.logger.Info("using linked clone", "template", params.TemplateName)
	}

	c.logger.Info("cloning VM",
		"template", params.TemplateName,
		"name", params.VMName,
		"vcpus", params.VCPUs,
		"ram_mb", params.RAMmb,
		"network", params.Network,
		"host", placement.Identity.Name,
		"host_moref", placement.Identity.MoRef,
	)

	if arm != nil {
		if err := arm(ctx); err != nil {
			return "", fmt.Errorf("arm clone operation before submission: %w", err)
		}
	}
	task, err := template.Clone(ctx, folder, params.VMName, cloneSpec)
	if err != nil {
		if isDuplicateNameErr(err) {
			return "", fmt.Errorf("%w: start clone %q: %v", ErrAmbiguousVMOwnership, params.VMName, err)
		}
		return "", fmt.Errorf("start clone: %w", err)
	}
	return task.Reference().Value, nil
}

// WaitCloneVMTask resumes an existing vCenter task reference. The wait has an
// operational deadline and always honors lease loss or worker shutdown.
func (c *Client) WaitCloneVMTask(ctx context.Context, taskRef string) (string, error) {
	if taskRef == "" {
		return "", errors.New("clone task reference is empty")
	}
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	overallCtx, cancelOverall := context.WithTimeout(ctx, cloneTaskOperationalTimeout)
	defer cancelOverall()

	ref := types.ManagedObjectReference{Type: "Task", Value: taskRef}
	task := object.NewTask(c.client.Client, ref)
	for {
		waitCtx, cancelWait := context.WithTimeout(overallCtx, cloneTaskWaitSlice)
		info, err := task.WaitForResult(waitCtx, nil)
		cancelWait()
		switch {
		case err == nil:
			vmRef, ok := info.Result.(types.ManagedObjectReference)
			if !ok || vmRef.Type != "VirtualMachine" || vmRef.Value == "" {
				return "", fmt.Errorf("clone task %s returned unexpected result %T", taskRef, info.Result)
			}
			c.logger.Info("VM clone task completed", "task", taskRef, "moref", vmRef.Value)
			return vmRef.Value, nil
		case errors.Is(err, context.DeadlineExceeded) && overallCtx.Err() == nil:
			c.logger.Warn("clone task still running after wait slice",
				"task", taskRef, "wait_slice", cloneTaskWaitSlice)
			continue
		case errors.Is(err, context.Canceled), errors.Is(overallCtx.Err(), context.DeadlineExceeded):
			waitErr := overallCtx.Err()
			if waitErr == nil {
				waitErr = err
			}
			return "", fmt.Errorf("wait for clone task %s: %w", taskRef, waitErr)
		case isNotAuthenticatedErr(err):
			reconnectCtx, cancelReconnect := context.WithTimeout(overallCtx, 30*time.Second)
			reconnectErr := c.Connect(reconnectCtx)
			cancelReconnect()
			if reconnectErr != nil {
				return "", fmt.Errorf("reconnect while waiting for clone task %s: %w", taskRef, reconnectErr)
			}
			task = object.NewTask(c.client.Client, ref)
		default:
			var taskErr vimtask.Error
			if errors.As(err, &taskErr) {
				if isDuplicateNameErr(err) {
					return "", fmt.Errorf("%w: clone task %s: %v", ErrAmbiguousVMOwnership, taskRef, err)
				}
				return "", fmt.Errorf("%w: task %s: %v", ErrCloneTaskFailed, taskRef, err)
			}
			return "", fmt.Errorf("wait for clone task %s: %w", taskRef, err)
		}
	}
}

// ConfigureClonedVM applies the mutable VM settings after the exact clone MoRef
// is known and has been staged for compensation.
func (c *Client) ConfigureClonedVM(ctx context.Context, moref string, params CloneVMParams) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.ValidateVMPlacement(ctx, moref, params.HostMoRef); err != nil {
		return fmt.Errorf("refuse to configure clone on invalid host: %w", err)
	}
	if err := c.EnsureVMPlacementControl(
		ctx,
		moref,
		params.HostMoRef,
		params.ComputeResourceType,
		params.ComputeResourceMoRef,
		params.DRSControl,
	); err != nil {
		return err
	}
	vmRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: moref}
	clonedVM := object.NewVirtualMachine(c.client.Client, vmRef)
	configSpec := types.VirtualMachineConfigSpec{
		NumCPUs:  params.VCPUs,
		MemoryMB: params.RAMmb,
	}
	netBacking := &types.VirtualEthernetCardNetworkBackingInfo{
		VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
			DeviceName: params.Network,
		},
	}

	// Set network adapter on the first NIC
	var vmMo mo.VirtualMachine
	if err := clonedVM.Properties(ctx, vmRef, []string{"config.hardware.device"}, &vmMo); err != nil {
		return fmt.Errorf("get cloned VM devices: %w", err)
	}

	for _, dev := range vmMo.Config.Hardware.Device {
		if nic, ok := dev.(types.BaseVirtualEthernetCard); ok {
			card := nic.GetVirtualEthernetCard()
			card.Backing = netBacking
			configSpec.DeviceChange = append(configSpec.DeviceChange, &types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationEdit,
				Device:    dev,
			})
			break
		}
	}

	// Inject guestinfo for guest OS customization (first-boot password).
	// The durable operation ID is a per-clone instance identity, so guest
	// agents cannot mistake this clone for a previously customized VM.
	customization, err := guestinfoCustomizationForInstance(
		params.OSType,
		params.Password,
		params.VMName,
		params.OperationID,
	)
	if err != nil {
		return err
	}
	configSpec.ExtraConfig = append(configSpec.ExtraConfig, customization...)

	reconfigTask, err := clonedVM.Reconfigure(ctx, configSpec)
	if err != nil {
		return fmt.Errorf("start reconfigure: %w", err)
	}
	if err := reconfigTask.Wait(ctx); err != nil {
		return fmt.Errorf("reconfigure task: %w", err)
	}
	if err := c.ValidateVMPlacementControl(
		ctx,
		moref,
		params.HostMoRef,
		params.ComputeResourceType,
		params.ComputeResourceMoRef,
		params.DRSControl,
	); err != nil {
		return err
	}

	c.logger.Info("VM reconfigured", "name", params.VMName, "vcpus", params.VCPUs, "ram_mb", params.RAMmb)
	return nil
}

// ensureTemplateSnapshot checks if the template has a snapshot for linked cloning.
// If no snapshot exists, it creates one. Returns the snapshot MoRef.
func (c *Client) ensureTemplateSnapshot(ctx context.Context, template *object.VirtualMachine, name string) (*types.ManagedObjectReference, error) {
	var vmMo mo.VirtualMachine
	if err := template.Properties(ctx, template.Reference(), []string{"snapshot"}, &vmMo); err != nil {
		return nil, fmt.Errorf("get snapshot info: %w", err)
	}

	// Use existing snapshot if available
	if vmMo.Snapshot != nil && vmMo.Snapshot.CurrentSnapshot != nil {
		c.logger.Info("template has existing snapshot", "template", name)
		return vmMo.Snapshot.CurrentSnapshot, nil
	}

	// Create a snapshot for linked cloning (no memory, no quiesce — template is powered off)
	if err := c.validateVMForMutation(ctx, template.Reference().Value, "", false); err != nil {
		return nil, fmt.Errorf("linked-clone snapshot creation prohibited: %w", err)
	}
	c.logger.Info("creating linked-clone base snapshot", "template", name)
	task, err := template.CreateSnapshot(ctx, "linked-clone-base", "Auto-created for linked clone provisioning", false, false)
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}

	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot task: %w", err)
	}

	snapRef := info.Result.(types.ManagedObjectReference)
	return &snapRef, nil
}

// DestroyVM powers off (if running) and destroys a VM by MoRef.
// Idempotent: returns nil if the VM was already deleted.
func (c *Client) DestroyVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	return c.destroyVM(ctx, moref, func(validateCtx context.Context) error {
		return c.validateVMForMutation(validateCtx, moref, "", true)
	})
}

// DestroyVMWithPlacement destroys a VM only while its complete persisted
// placement identity still matches. The validation is repeated inside every
// retry so cleanup cannot follow a VM that moved to another host or compute
// resource.
func (c *Client) DestroyVMWithPlacement(
	ctx context.Context,
	moref, expectedHostMoref, computeType, computeMoref, control string,
	identity VMCloneIdentity,
) error {
	if expectedHostMoref == "" || computeType == "" || computeMoref == "" || control == "" {
		return newPlacementDrift(
			PlacementDriftCompute,
			nil,
			"exact cleanup placement identity is incomplete for VM %s",
			moref,
		)
	}
	if err := c.ensureConnected(ctx); err != nil {
		return fmt.Errorf("%w: connect to vCenter for exact VM cleanup: %w", ErrPlacementValidationUnavailable, err)
	}
	return c.destroyVM(ctx, moref, func(validateCtx context.Context) error {
		err := c.ValidateVMPlacementCleanupControl(
			validateCtx,
			moref,
			expectedHostMoref,
			computeType,
			computeMoref,
			control,
		)
		if err != nil && (isAlreadyDeletedErr(err) || isResourceNotFoundErr(err)) {
			return nil
		}
		if err != nil {
			return err
		}
		return c.ValidateVMCloneIdentity(validateCtx, moref, identity)
	})
}

func (c *Client) destroyVM(
	ctx context.Context,
	moref string,
	validate func(context.Context) error,
) error {
	return c.withRetry(ctx, "destroy VM", func() error {
		if err := validate(ctx); err != nil {
			return err
		}
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		// Try to power off first (ignore errors if already off or deleted)
		powerOffTask, err := vm.PowerOff(ctx)
		if err == nil {
			_ = powerOffTask.Wait(ctx)
		}

		// Destroy — treat "already deleted" as success
		destroyTask, err := vm.Destroy(ctx)
		if err != nil {
			if isAlreadyDeletedErr(err) || isResourceNotFoundErr(err) {
				c.logger.Info("VM already deleted", "moref", moref)
				return nil
			}
			return fmt.Errorf("destroy VM %s: %w", moref, err)
		}
		if err := destroyTask.Wait(ctx); err != nil {
			if isAlreadyDeletedErr(err) || isResourceNotFoundErr(err) {
				c.logger.Info("VM already deleted", "moref", moref)
				return nil
			}
			return fmt.Errorf("destroy VM task %s: %w", moref, err)
		}

		c.logger.Info("VM destroyed", "moref", moref)
		return nil
	})
}

// powerOnDiskReadyAttempts / powerOnDiskReadyDelay bound how long PowerOnVM
// keeps retrying a power-on that failed because the VM's disk was not yet
// readable. ~60s total, which comfortably covers the observed case without
// turning a genuinely corrupt disk into a long stall.
const (
	powerOnDiskReadyAttempts = 20
	powerOnDiskReadyDelay    = 3 * time.Second
)

// PowerOnVM powers on a VM.
// Idempotent: returns nil if the VM is already powered on.
//
// A power-on issued immediately after CreateVM_Task can lose a race with the
// datastore. The first ISO template build failed here with "The file specified
// is not a virtual disk", 234ms after the VM was created; vmware.log named the
// real reason:
//
//	VmfsExtentCommonOpen: possible extent truncation (?) realSize is 0,
//	size in descriptor 83886080
//	... failed to open: Size of extent in descriptor file larger than real size
//
// The descriptor was written and the flat extent was not yet materialized on
// the NFS datastore. Powering the same VM on minutes later succeeded with no
// other change, which is what identified this as a race rather than a bad
// disk. Clones never hit it because the clone task does not return until the
// data is written; only paths that build a VM from scratch — CreateBlankVM and
// ImportOVA — power on against a disk the storage may still be allocating.
//
// Retrying is preferred to sleeping before the power-on: the readiness delay
// depends on the datastore and the disk size, so any fixed sleep is either too
// short on a busy NAS or wasted on every fast case.
func (c *Client) PowerOnVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, moref, "", false); err != nil {
		return err
	}

	return c.withRetry(ctx, "power on VM", func() error {
		return retryWhileDiskNotReady(ctx, c.logger, moref, powerOnDiskReadyAttempts, powerOnDiskReadyDelay, func() error {
			vm := object.NewVirtualMachine(c.client.Client,
				types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

			task, err := vm.PowerOn(ctx)
			if err != nil {
				if isAlreadyPoweredOnErr(err) {
					return nil
				}
				return fmt.Errorf("power on %s: %w", moref, err)
			}
			if err := task.Wait(ctx); err != nil {
				if isAlreadyPoweredOnErr(err) {
					return nil
				}
				return err
			}
			return nil
		})
	})
}

// retryWhileDiskNotReady calls powerOn until it succeeds, fails for a reason
// other than an unreadable disk, or runs out of attempts. It is a free
// function taking its delay so tests can drive it with delay=0 rather than
// waiting out a real backoff.
func retryWhileDiskNotReady(ctx context.Context, logger *slog.Logger, moref string, attempts int, delay time.Duration, powerOn func() error) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = powerOn()
		if err == nil || !isDiskNotReadyErr(err) {
			return err
		}
		if attempt == attempts {
			break
		}
		if logger != nil {
			logger.Warn("VM disk not readable yet, retrying power on",
				"vm", moref, "attempt", attempt, "of", attempts, "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return fmt.Errorf("disk still not readable after %d attempts over ~%s (the datastore may not have finished allocating the disk): %w",
		attempts, time.Duration(attempts-1)*delay, err)
}

// IsDiskNotReadyErr reports whether err is the datastore-not-ready /
// broken-flat signature ("is not a virtual disk" / "larger than real size" /
// "Cannot open the disk"). It is the exported classifier the provisioner uses
// to decide whether a failed pre-power-on disk probe means "recreate the disk"
// versus a genuinely different fault it must surface as-is. It delegates to the
// same narrow matcher used by the power-on retry so the two never drift.
func IsDiskNotReadyErr(err error) bool {
	return isDiskNotReadyErr(err)
}

// isDiskNotReadyErr reports whether a power-on failure is the datastore not
// having finished materializing the VM's disk. Deliberately narrow: it does
// not match "Failed to lock the file", which means another host holds the
// disk and is a genuinely different problem that retrying would only hide.
func isDiskNotReadyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "is not a virtual disk") ||
		strings.Contains(msg, "Cannot open the disk") ||
		strings.Contains(msg, "larger than real size")
}

// PowerOffVM powers off a VM.
// Idempotent: returns nil if the VM was already deleted.
func (c *Client) PowerOffVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, moref, "", true); err != nil {
		return err
	}

	return c.withRetry(ctx, "power off VM", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		task, err := vm.PowerOff(ctx)
		if err != nil {
			if isAlreadyDeletedErr(err) {
				return nil
			}
			return fmt.Errorf("power off %s: %w", moref, err)
		}
		if err := task.Wait(ctx); err != nil {
			if isAlreadyDeletedErr(err) {
				return nil
			}
			return err
		}
		return nil
	})
}

// RestartVM guest-restarts a VM (graceful reboot via VMware Tools).
func (c *Client) RestartVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, moref, "", false); err != nil {
		return err
	}

	return c.withRetry(ctx, "restart VM", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		return vm.RebootGuest(ctx)
	})
}

// ResetVM performs a hard reset (power cycle) on a VM.
func (c *Client) ResetVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, moref, "", false); err != nil {
		return err
	}

	return c.withRetry(ctx, "reset VM", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		task, err := vm.Reset(ctx)
		if err != nil {
			return fmt.Errorf("reset %s: %w", moref, err)
		}
		return task.Wait(ctx)
	})
}

// WaitForIP waits for VMware Tools to report an IP address.
func (c *Client) WaitForIP(ctx context.Context, moref string, timeout time.Duration) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	if err := c.ValidateVMPlacement(ctx, moref, ""); err != nil {
		return "", fmt.Errorf("refuse to wait for IP on invalid host: %w", err)
	}

	var ip string
	err := c.withRetry(ctx, "wait for IP", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		var waitErr error
		ip, waitErr = vm.WaitForIP(timeoutCtx, true)
		if waitErr != nil {
			return fmt.Errorf("wait for IP on %s: %w", moref, waitErr)
		}

		c.logger.Info("VM got IP", "moref", moref, "ip", ip)
		return nil
	})
	return ip, err
}

// GetVM retrieves a VM's properties by MoRef (for idempotency checks).
func (c *Client) GetVM(ctx context.Context, moref string) (*mo.VirtualMachine, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var props mo.VirtualMachine
	err := c.withRetry(ctx, "get VM", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		return vm.Properties(ctx, vm.Reference(), []string{"name", "runtime", "guest", "config"}, &props)
	})
	if err != nil {
		return nil, err
	}
	return &props, nil
}

// GuestInfo is a lightweight, JSON-friendly snapshot of a VM's
// guest/runtime state suitable for surfacing to the wizard UI. Unlike
// WaitForIP / WaitForTools, GetGuestInfo never blocks — it's a single
// property collector call that returns whatever is currently known, so
// the wizard's 5s polling loop drives the freshness.
//
// IPAddress may be empty if VMware Tools hasn't reported one yet (guest
// still booting, tools not installed). ToolsRunning lets the UI explain
// "waiting for VMware Tools…" vs "no IP assigned yet" instead of just
// hiding the connection box silently.
type GuestInfo struct {
	Name         string // vCenter VM name (used for .rdp filename, audit, display)
	IPAddress    string // primary guest IP if VMware Tools reports one, "" otherwise
	ToolsRunning bool   // true when guest.toolsRunningStatus == guestToolsRunning
	PoweredOn    bool   // true when runtime.powerState == poweredOn
}

// GetGuestInfo returns a non-blocking snapshot of the VM's guest/runtime
// state. Used by the template wizard to surface IP + name to the
// instructor without waiting for VMware Tools to come up.
//
// Returns an error only on transport/auth failures. A VM with no IP and
// no tools is a normal "still booting" state and returns a populated
// GuestInfo with empty IPAddress + ToolsRunning=false.
func (c *Client) GetGuestInfo(ctx context.Context, moref string) (*GuestInfo, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var props mo.VirtualMachine
	err := c.withRetry(ctx, "get guest info", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		return vm.Properties(ctx, vm.Reference(),
			[]string{"name", "runtime.powerState", "guest.ipAddress", "guest.net", "guest.toolsRunningStatus"},
			&props)
	})
	if err != nil {
		return nil, fmt.Errorf("get guest info for %s: %w", moref, err)
	}

	info := &GuestInfo{Name: props.Name}
	if props.Guest != nil {
		// Prefer a routable IPv4 from the per-NIC list. VMware Tools'
		// guest.ipAddress is whatever the guest calls "primary", which on
		// Windows is frequently the IPv6 link-local (fe80::…) — useless as an
		// RDP/SSH target (mstsc can't dial a link-local without a zone index).
		// Fall back to guest.ipAddress only if it's itself a usable IPv4.
		info.IPAddress = pickGuestIPv4(props.Guest)
		info.ToolsRunning = props.Guest.ToolsRunningStatus ==
			string(types.VirtualMachineToolsRunningStatusGuestToolsRunning)
	}
	info.PoweredOn = props.Runtime.PowerState == types.VirtualMachinePowerStatePoweredOn
	return info, nil
}

// pickGuestIPv4 returns the best routable IPv4 address VMware Tools reports
// for a guest, or "" if none is known yet. It scans every NIC's addresses and
// skips loopback, link-local (169.254.0.0/16), and unspecified addresses, as
// well as all IPv6. If no per-NIC IPv4 qualifies it falls back to
// guest.ipAddress, but only when that too is a usable IPv4 — never returning
// an IPv6 link-local that the UI would turn into a dead RDP link.
func pickGuestIPv4(guest *types.GuestInfo) string {
	isRoutableV4 := func(s string) bool {
		ip := net.ParseIP(strings.TrimSpace(s))
		if ip == nil || ip.To4() == nil {
			return false
		}
		return !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
	}
	for _, nic := range guest.Net {
		if nic.IpConfig != nil {
			for _, addr := range nic.IpConfig.IpAddress {
				if isRoutableV4(addr.IpAddress) {
					return addr.IpAddress
				}
			}
		}
		for _, addr := range nic.IpAddress {
			if isRoutableV4(addr) {
				return addr
			}
		}
	}
	if isRoutableV4(guest.IpAddress) {
		return guest.IpAddress
	}
	return ""
}

// WebMKSTicket holds the result of a WebMKS ticket acquisition.
type WebMKSTicket struct {
	Host   string
	Port   int32
	Ticket string
}

// AcquireWebMKSTicket gets a WebMKS console ticket for a VM.
// The ticket can be used to open a WebSocket connection to the ESXi host.
func (c *Client) AcquireWebMKSTicket(ctx context.Context, moref string) (*WebMKSTicket, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var result *WebMKSTicket
	err := c.withRetry(ctx, "acquire webmks ticket", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		ticket, err := vm.AcquireTicket(ctx, string(types.VirtualMachineTicketTypeWebmks))
		if err != nil {
			return fmt.Errorf("acquire webmks ticket for %s: %w", moref, err)
		}

		c.logger.Info("acquired WebMKS ticket", "moref", moref, "host", ticket.Host, "port", ticket.Port)
		result = &WebMKSTicket{
			Host:   ticket.Host,
			Port:   ticket.Port,
			Ticket: ticket.Ticket,
		}
		return nil
	})
	return result, err
}

// ---------- Snapshot Operations ----------

// CreateVMSnapshot creates a snapshot of a VM and returns the snapshot's ManagedObjectReference value.
func (c *Client) CreateVMSnapshot(ctx context.Context, moref, name, description string) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}
	if err := c.validateVMForMutation(ctx, moref, "", false); err != nil {
		return "", err
	}

	var snapMoref string
	err := c.withRetry(ctx, "create snapshot", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		task, taskErr := vm.CreateSnapshot(ctx, name, description, false, false)
		if taskErr != nil {
			return fmt.Errorf("create snapshot for %s: %w", moref, taskErr)
		}

		info, taskErr := task.WaitForResult(ctx)
		if taskErr != nil {
			return fmt.Errorf("snapshot task for %s: %w", moref, taskErr)
		}

		ref, ok := info.Result.(types.ManagedObjectReference)
		if !ok {
			return fmt.Errorf("unexpected snapshot result type for %s", moref)
		}
		snapMoref = ref.Value
		return nil
	})
	if err != nil {
		return "", err
	}

	c.logger.Info("created VM snapshot", "moref", moref, "snapshot", snapMoref, "name", name)
	return snapMoref, nil
}

// RevertToSnapshot reverts a VM to the specified snapshot.
func (c *Client) RevertToSnapshot(ctx context.Context, vmMoref, snapshotMoref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, vmMoref, "", false); err != nil {
		return err
	}

	return c.withRetry(ctx, "revert snapshot", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoref})

		task, err := vm.RevertToSnapshot(ctx, snapshotMoref, true)
		if err != nil {
			return fmt.Errorf("revert snapshot %s on %s: %w", snapshotMoref, vmMoref, err)
		}

		if err := task.Wait(ctx); err != nil {
			return fmt.Errorf("revert snapshot task %s on %s: %w", snapshotMoref, vmMoref, err)
		}

		c.logger.Info("reverted VM to snapshot", "vm", vmMoref, "snapshot", snapshotMoref)
		return nil
	})
}

// SnapshotInfo holds snapshot metadata returned by ListVMSnapshots.
type SnapshotInfo struct {
	Name        string
	Description string
	Moref       string
	CreateTime  time.Time
}

// ListVMSnapshots returns all snapshots for a VM.
// GetGuestInfoVar reads a single guestinfo.* variable out of the VM's
// extraConfig.
//
// This is deliberately read from config.extraConfig rather than through guest
// operations, because extraConfig is part of the VM's configuration and stays
// readable when the guest is POWERED OFF. That property is the whole point: it
// lets a script inside the guest record "I finished" as its last act before
// shutting the machine down, and lets us read that record afterwards. Guest
// ops cannot do this, since the agent is gone the moment the guest goes down.
//
// A guest sets one of these with:
//
//	vmware-rpctool "info-set guestinfo.some.key some-value"
//
// Returns ("", nil) when the key is not present, since absence is a normal
// answer here and not an error.
func (c *Client) GetGuestInfoVar(ctx context.Context, moref, key string) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	var props mo.VirtualMachine
	err := c.withRetry(ctx, "get guestinfo var", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})
		return vm.Properties(ctx, vm.Reference(), []string{"config.extraConfig"}, &props)
	})
	if err != nil {
		return "", fmt.Errorf("get guestinfo %q for %s: %w", key, moref, err)
	}
	if props.Config == nil {
		return "", nil
	}
	for _, opt := range props.Config.ExtraConfig {
		ov := opt.GetOptionValue()
		if ov == nil || ov.Key != key {
			continue
		}
		s, _ := ov.Value.(string)
		return s, nil
	}
	return "", nil
}

func (c *Client) ListVMSnapshots(ctx context.Context, moref string) ([]SnapshotInfo, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	var result []SnapshotInfo
	err := c.withRetry(ctx, "list snapshots", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

		var props mo.VirtualMachine
		if err := vm.Properties(ctx, vm.Reference(), []string{"snapshot"}, &props); err != nil {
			return fmt.Errorf("get snapshot properties for %s: %w", moref, err)
		}

		result = nil
		if props.Snapshot == nil {
			return nil
		}

		// Flatten the snapshot tree
		var flatten func([]types.VirtualMachineSnapshotTree)
		flatten = func(nodes []types.VirtualMachineSnapshotTree) {
			for _, node := range nodes {
				result = append(result, SnapshotInfo{
					Name:        node.Name,
					Description: node.Description,
					Moref:       node.Snapshot.Value,
					CreateTime:  node.CreateTime,
				})
				flatten(node.ChildSnapshotList)
			}
		}
		flatten(props.Snapshot.RootSnapshotList)
		return nil
	})
	return result, err
}

// RemoveVMSnapshot removes a single snapshot from a VM (does not remove children).
func (c *Client) RemoveVMSnapshot(ctx context.Context, vmMoref, snapshotMoref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	if err := c.validateVMForMutation(ctx, vmMoref, "", false); err != nil {
		return err
	}

	return c.withRetry(ctx, "remove snapshot", func() error {
		vm := object.NewVirtualMachine(c.client.Client,
			types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoref})

		consolidate := true
		task, err := vm.RemoveSnapshot(ctx, snapshotMoref, false, &consolidate)
		if err != nil {
			return fmt.Errorf("remove snapshot %s from %s: %w", snapshotMoref, vmMoref, err)
		}

		if err := task.Wait(ctx); err != nil {
			return fmt.Errorf("remove snapshot task %s from %s: %w", snapshotMoref, vmMoref, err)
		}

		c.logger.Info("removed VM snapshot", "vm", vmMoref, "snapshot", snapshotMoref)
		return nil
	})
}

// FindTemplate looks up a VM template by name in the datacenter.
func (c *Client) FindTemplate(ctx context.Context, name string) (*object.VirtualMachine, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	var result *object.VirtualMachine
	err := c.withRetry(ctx, "find template", func() error {
		var findErr error
		result, findErr = c.finder.VirtualMachine(ctx, name)
		return findErr
	})
	return result, err
}

// Ping verifies vCenter connectivity.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	return c.withRetry(ctx, "ping", func() error {
		_, err := c.finder.Datacenter(ctx, c.config.Datacenter)
		return err
	})
}

// withRetry executes fn; if it returns NotAuthenticated, does a full reconnect and retries once.
func (c *Client) withRetry(ctx context.Context, op string, fn func() error) error {
	err := fn()
	if err == nil || !isNotAuthenticatedErr(err) {
		return err
	}
	c.logger.Warn("NotAuthenticated during operation, reconnecting",
		"op", op, "error", err)
	if reconErr := c.Connect(ctx); reconErr != nil {
		return fmt.Errorf("reconnect after NotAuthenticated: %w", reconErr)
	}
	return fn()
}

// isNotAuthenticatedErr checks if an error chain contains a vSphere NotAuthenticated fault.
func isNotAuthenticatedErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "NotAuthenticated") ||
		strings.Contains(strings.ToLower(msg), "not authenticated") ||
		strings.Contains(strings.ToLower(msg), "session is not authenticated")
}

func isDuplicateNameErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicatename") ||
		strings.Contains(msg, "duplicate name") ||
		strings.Contains(msg, "already exists")
}

// isAlreadyDeletedErr checks if a vSphere error indicates the object was already deleted.
func isAlreadyDeletedErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "has already been deleted") ||
		strings.Contains(msg, "not been completely created")
}

// isAlreadyPoweredOnErr checks if a vSphere error indicates the VM is already powered on.
func isAlreadyPoweredOnErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "current state (Powered on)") ||
		strings.Contains(msg, "InvalidPowerState")
}

// isResourceNotFoundErr checks if a vSphere error indicates the resource was not found.
func isResourceNotFoundErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "could not be found") ||
		strings.Contains(msg, "NotFound")
}

// IsVMNotFoundError reports whether vSphere says the referenced VM no longer
// exists. Power-off uses this to preserve its idempotent delete-tolerant path.
func IsVMNotFoundError(err error) bool {
	return err != nil && (isAlreadyDeletedErr(err) || isResourceNotFoundErr(err))
}
