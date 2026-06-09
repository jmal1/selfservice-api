package vcenter

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

// Config holds vCenter connection settings.
type Config struct {
	URL           string // e.g., "https://vcenter.lab.jmal.io/sdk"
	User          string // e.g., "selfservice-svc@vsphere.local"
	Password      string
	Datacenter    string   // e.g., "JMAL-Datacenter"
	Datastore     string   // e.g., "NAS-vmstore"
	VMFolder      string   // e.g., "Student-VMs"
	ResourcePools []string // e.g., ["AMD-Cluster/Resources/Student-VMs", "Intel-Cluster/Resources/Student-VMs"]
	Hosts         []string // ESXi hosts for port group operations
	Insecure      bool     // skip TLS verification
}

// Client wraps govmomi for self-service provisioning operations.
type Client struct {
	config     Config
	client     *govmomi.Client
	finder     *find.Finder
	datacenter *object.Datacenter
	mu         sync.Mutex
	logger     *slog.Logger
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
	TemplateName string
	VMName       string
	VCPUs        int32
	RAMmb        int64
	Network      string // port group name
	OSType       string // "linux" or "windows"
	Password     string // generated password for cloud-init
}

// CloneVM clones a template into the Student-VMs folder.
// Returns the VM's managed object reference (MoRef) as a string.
func (c *Client) CloneVM(ctx context.Context, params CloneVMParams) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	var moref string
	err := c.withRetry(ctx, "clone VM", func() error {
		var cloneErr error
		moref, cloneErr = c.cloneVMInner(ctx, params)
		return cloneErr
	})
	return moref, err
}

// cloneVMInner contains the actual clone logic (called by CloneVM via withRetry).
func (c *Client) cloneVMInner(ctx context.Context, params CloneVMParams) (string, error) {
	// Find template
	template, err := c.finder.VirtualMachine(ctx, params.TemplateName)
	if err != nil {
		return "", fmt.Errorf("find template %s: %w", params.TemplateName, err)
	}

	// Find target folder
	folder, err := c.finder.Folder(ctx, c.config.VMFolder)
	if err != nil {
		return "", fmt.Errorf("find folder %s: %w", c.config.VMFolder, err)
	}

	// Select best resource pool based on available resources
	pool, err := c.selectBestPool(ctx, params.VCPUs, params.RAMmb)
	if err != nil {
		return "", fmt.Errorf("select resource pool: %w", err)
	}

	// Find datastore
	ds, err := c.finder.Datastore(ctx, c.config.Datastore)
	if err != nil {
		return "", fmt.Errorf("find datastore %s: %w", c.config.Datastore, err)
	}
	dsRef := ds.Reference()

	// For standard vSwitch port groups, construct the backing info directly by name.
	// finder.Network() can't find per-host port groups; using the name directly works
	// because vCenter resolves it on the target host at clone time.
	netBacking := &types.VirtualEthernetCardNetworkBackingInfo{
		VirtualDeviceDeviceBackingInfo: types.VirtualDeviceDeviceBackingInfo{
			DeviceName: params.Network,
		},
	}

	// Build clone spec — use linked clones for fast provisioning.
	// Linked clones create a thin delta disk referencing the template's snapshot,
	// reducing clone time from minutes (full copy) to seconds.
	poolRef := pool.Reference()
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:      &poolRef,
			Datastore: &dsRef,
		},
		PowerOn:  false,
		Template: false,
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
	)

	task, err := template.Clone(ctx, folder, params.VMName, cloneSpec)
	if err != nil {
		return "", fmt.Errorf("start clone: %w", err)
	}

	info, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("clone task: %w", err)
	}

	vmRef := info.Result.(types.ManagedObjectReference)
	c.logger.Info("VM cloned", "name", params.VMName, "moref", vmRef.Value)

	// Reconfigure the cloned VM: CPU, RAM, and network.
	clonedVM := object.NewVirtualMachine(c.client.Client, vmRef)

	configSpec := types.VirtualMachineConfigSpec{
		NumCPUs:  params.VCPUs,
		MemoryMB: params.RAMmb,
	}

	// Set network adapter on the first NIC
	var vmMo mo.VirtualMachine
	if err := clonedVM.Properties(ctx, vmRef, []string{"config.hardware.device"}, &vmMo); err != nil {
		return vmRef.Value, fmt.Errorf("get cloned VM devices: %w", err)
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

	// Inject guestinfo for guest OS customization
	if params.Password != "" {
		var userdata, metadata string

		if params.OSType == "linux" {
			// cloud-init format
			userdata = fmt.Sprintf(`#cloud-config
password: %s
chpasswd:
  expire: false
ssh_pwauth: true
hostname: %s
`, params.Password, params.VMName)
			metadata = fmt.Sprintf(`{"instance-id": "%s", "local-hostname": "%s"}`, params.VMName, params.VMName)
		} else if params.OSType == "windows" {
			// cloudbase-init: UserDataPlugin runs #ps1 script to set password.
			// SetHostNamePlugin reads local-hostname from metadata.
			// Plugin order in cloudbase-init.conf must have UserData before SetHostName
			// (SetHostName triggers a reboot).
			userdata = fmt.Sprintf(`#ps1_sysnative
$password = ConvertTo-SecureString '%s' -AsPlainText -Force
Get-LocalUser -Name 'Student' | Set-LocalUser -Password $password
`, params.Password)
			metadata = fmt.Sprintf(`{"instance-id": "%s", "local-hostname": "%s", "admin_pass": "%s"}`,
				params.VMName, params.VMName, params.Password)
		}

		if userdata != "" {
			configSpec.ExtraConfig = append(configSpec.ExtraConfig,
				&types.OptionValue{Key: "guestinfo.userdata", Value: base64.StdEncoding.EncodeToString([]byte(userdata))},
				&types.OptionValue{Key: "guestinfo.userdata.encoding", Value: "base64"},
			)
		}
		if metadata != "" {
			configSpec.ExtraConfig = append(configSpec.ExtraConfig,
				&types.OptionValue{Key: "guestinfo.metadata", Value: base64.StdEncoding.EncodeToString([]byte(metadata))},
				&types.OptionValue{Key: "guestinfo.metadata.encoding", Value: "base64"},
			)
		}
	}

	reconfigTask, err := clonedVM.Reconfigure(ctx, configSpec)
	if err != nil {
		return vmRef.Value, fmt.Errorf("start reconfigure: %w", err)
	}
	if err := reconfigTask.Wait(ctx); err != nil {
		return vmRef.Value, fmt.Errorf("reconfigure task: %w", err)
	}

	c.logger.Info("VM reconfigured", "name", params.VMName, "vcpus", params.VCPUs, "ram_mb", params.RAMmb)
	return vmRef.Value, nil
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

	return c.withRetry(ctx, "destroy VM", func() error {
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
			if isAlreadyDeletedErr(err) {
				c.logger.Info("VM already deleted", "moref", moref)
				return nil
			}
			return fmt.Errorf("destroy VM %s: %w", moref, err)
		}
		if err := destroyTask.Wait(ctx); err != nil {
			if isAlreadyDeletedErr(err) {
				c.logger.Info("VM already deleted", "moref", moref)
				return nil
			}
			return fmt.Errorf("destroy VM task %s: %w", moref, err)
		}

		c.logger.Info("VM destroyed", "moref", moref)
		return nil
	})
}

// PowerOnVM powers on a VM.
// Idempotent: returns nil if the VM is already powered on.
func (c *Client) PowerOnVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	return c.withRetry(ctx, "power on VM", func() error {
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
}

// PowerOffVM powers off a VM.
// Idempotent: returns nil if the VM was already deleted.
func (c *Client) PowerOffVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
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

// ---------- Resource Pool Selection ----------

// selectBestPool queries all configured resource pools and returns the one
// with the most available memory, weighted by the requested VM size.
func (c *Client) selectBestPool(ctx context.Context, vcpus int32, ramMB int64) (*object.ResourcePool, error) {
	if len(c.config.ResourcePools) == 0 {
		return nil, fmt.Errorf("no resource pools configured")
	}

	// If only one pool, use it directly
	if len(c.config.ResourcePools) == 1 {
		pool, err := c.finder.ResourcePool(ctx, c.config.ResourcePools[0])
		if err != nil {
			return nil, fmt.Errorf("find resource pool %s: %w", c.config.ResourcePools[0], err)
		}
		return pool, nil
	}

	type candidate struct {
		pool      *object.ResourcePool
		name      string
		freeMemMB int64
	}

	var best *candidate
	for _, poolPath := range c.config.ResourcePools {
		pool, err := c.finder.ResourcePool(ctx, poolPath)
		if err != nil {
			c.logger.Warn("resource pool not found, skipping", "pool", poolPath, "error", err)
			continue
		}

		var props mo.ResourcePool
		err = pool.Properties(ctx, pool.Reference(), []string{"runtime.memory"}, &props)
		if err != nil {
			c.logger.Warn("failed to get pool stats, skipping", "pool", poolPath, "error", err)
			continue
		}

		// Calculate free memory: reservationUsed tracks actual consumption
		overallUsage := props.Runtime.Memory.OverallUsage
		maxUsage := props.Runtime.Memory.MaxUsage
		freeMem := (maxUsage - overallUsage) / (1024 * 1024) // bytes → MB

		c.logger.Info("resource pool stats",
			"pool", poolPath,
			"free_mb", freeMem,
			"used_mb", overallUsage/(1024*1024),
			"max_mb", maxUsage/(1024*1024),
		)

		if best == nil || freeMem > best.freeMemMB {
			best = &candidate{pool: pool, name: poolPath, freeMemMB: freeMem}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no available resource pools found")
	}

	// Check if the best pool has enough memory for the requested VM
	if best.freeMemMB < ramMB {
		c.logger.Warn("best pool has less free memory than requested",
			"pool", best.name, "free_mb", best.freeMemMB, "requested_mb", ramMB)
	}

	c.logger.Info("selected resource pool", "pool", best.name, "free_mb", best.freeMemMB)
	return best.pool, nil
}

// selectBestPoolInSourceCluster is the same RAM-based selection as
// selectBestPool but constrained to resource pools that live in the
// same cluster as the source host. Used by template clones because
// vCenter rejects cross-cluster clones with a misleading "virtual
// disk is either corrupted or not a supported format" error when CPU
// vendor differs (Intel ↔ AMD) or when guest cpuid masks were set on
// the source.
//
// If sourceHost is nil (shouldn't happen for a real running source,
// but possible for synthetic tests), falls back to the unconstrained
// selectBestPool.
func (c *Client) selectBestPoolInSourceCluster(ctx context.Context, sourceHost *types.ManagedObjectReference, vcpus int32, ramMB int64) (*object.ResourcePool, error) {
	if sourceHost == nil {
		c.logger.Warn("source host unknown, falling back to unconstrained pool selection")
		return c.selectBestPool(ctx, vcpus, ramMB)
	}

	// Walk Host → ComputeResource (or ClusterComputeResource) parent.
	// The parent's resource pool path is the prefix any same-cluster
	// pool will share.
	var hostProps mo.HostSystem
	hostObj := object.NewHostSystem(c.client.Client, *sourceHost)
	if err := hostObj.Properties(ctx, hostObj.Reference(), []string{"parent"}, &hostProps); err != nil {
		return nil, fmt.Errorf("read source host parent: %w", err)
	}
	if hostProps.Parent == nil {
		return nil, fmt.Errorf("source host %s has no parent compute resource", sourceHost.Value)
	}

	var cr mo.ComputeResource
	crObj := object.NewComputeResource(c.client.Client, *hostProps.Parent)
	if err := crObj.Properties(ctx, crObj.Reference(), []string{"name", "resourcePool"}, &cr); err != nil {
		return nil, fmt.Errorf("read source cluster: %w", err)
	}
	clusterName := cr.Name
	c.logger.Info("source cluster identified",
		"host", sourceHost.Value,
		"cluster", clusterName)

	// Filter configured pools to those in the source cluster. Pool paths
	// look like "/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs",
	// so we match by the "/<clusterName>/" segment.
	wanted := "/" + clusterName + "/"
	var candidates []string
	for _, poolPath := range c.config.ResourcePools {
		if strings.Contains(poolPath, wanted) {
			candidates = append(candidates, poolPath)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no configured resource pools in source cluster %q (configured pools: %v)", clusterName, c.config.ResourcePools)
	}

	if len(candidates) == 1 {
		pool, err := c.finder.ResourcePool(ctx, candidates[0])
		if err != nil {
			return nil, fmt.Errorf("find resource pool %s: %w", candidates[0], err)
		}
		c.logger.Info("selected resource pool (single in source cluster)", "pool", candidates[0])
		return pool, nil
	}

	// Multiple candidates: pick the one with most free memory, same as
	// selectBestPool's policy.
	type candidate struct {
		pool      *object.ResourcePool
		name      string
		freeMemMB int64
	}
	var best *candidate
	for _, poolPath := range candidates {
		pool, err := c.finder.ResourcePool(ctx, poolPath)
		if err != nil {
			c.logger.Warn("resource pool not found, skipping", "pool", poolPath, "error", err)
			continue
		}
		var props mo.ResourcePool
		if err := pool.Properties(ctx, pool.Reference(), []string{"runtime.memory"}, &props); err != nil {
			c.logger.Warn("failed to get pool stats, skipping", "pool", poolPath, "error", err)
			continue
		}
		freeMem := (props.Runtime.Memory.MaxUsage - props.Runtime.Memory.OverallUsage) / (1024 * 1024)
		c.logger.Info("resource pool stats (source-cluster constrained)",
			"pool", poolPath,
			"free_mb", freeMem)
		if best == nil || freeMem > best.freeMemMB {
			best = &candidate{pool: pool, name: poolPath, freeMemMB: freeMem}
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no available resource pools in source cluster %q", clusterName)
	}
	if best.freeMemMB < ramMB {
		c.logger.Warn("best same-cluster pool has less free memory than requested",
			"pool", best.name, "free_mb", best.freeMemMB, "requested_mb", ramMB)
	}
	c.logger.Info("selected resource pool", "pool", best.name, "free_mb", best.freeMemMB, "constrained_to_cluster", clusterName)
	return best.pool, nil
}

// ---------- Port Group Operations ----------

// CreatePortGroupOnAllHosts creates a standard vSwitch port group on every configured ESXi host.
func (c *Client) CreatePortGroupOnAllHosts(ctx context.Context, pgName string, vlanID int) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	return c.withRetry(ctx, "create port groups", func() error {
		for _, hostName := range c.config.Hosts {
			if err := c.createPortGroup(ctx, hostName, pgName, vlanID); err != nil {
				return fmt.Errorf("create port group on %s: %w", hostName, err)
			}
		}
		c.logger.Info("port group created on all hosts", "name", pgName, "vlan_id", vlanID, "hosts", len(c.config.Hosts))
		return nil
	})
}

// DeletePortGroupOnAllHosts removes a port group from every configured ESXi host.
func (c *Client) DeletePortGroupOnAllHosts(ctx context.Context, pgName string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	return c.withRetry(ctx, "delete port groups", func() error {
		var lastErr error
		for _, hostName := range c.config.Hosts {
			if err := c.deletePortGroup(ctx, hostName, pgName); err != nil {
				c.logger.Warn("failed to delete port group", "host", hostName, "pg", pgName, "error", err)
				lastErr = err
			}
		}
		return lastErr
	})
}

func (c *Client) createPortGroup(ctx context.Context, hostName, pgName string, vlanID int) error {
	host, err := c.finder.HostSystem(ctx, hostName)
	if err != nil {
		return fmt.Errorf("find host %s: %w", hostName, err)
	}

	ns, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return fmt.Errorf("get network system: %w", err)
	}

	spec := types.HostPortGroupSpec{
		Name:        pgName,
		VlanId:      int32(vlanID),
		VswitchName: "vSwitch0",
		Policy:      types.HostNetworkPolicy{},
	}

	if err := ns.AddPortGroup(ctx, spec); err != nil {
		// Idempotency: if the port group already exists, treat as success
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "AlreadyExists") {
			c.logger.Info("port group already exists", "host", hostName, "name", pgName)
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) deletePortGroup(ctx context.Context, hostName, pgName string) error {
	host, err := c.finder.HostSystem(ctx, hostName)
	if err != nil {
		return fmt.Errorf("find host %s: %w", hostName, err)
	}

	ns, err := host.ConfigManager().NetworkSystem(ctx)
	if err != nil {
		return fmt.Errorf("get network system: %w", err)
	}

	if err := ns.RemovePortGroup(ctx, pgName); err != nil {
		// Idempotent: port group already removed
		if isResourceNotFoundErr(err) {
			c.logger.Info("port group already removed", "host", hostName, "name", pgName)
			return nil
		}
		return err
	}
	return nil
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
