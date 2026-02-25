package vcenter

import (
	"context"
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

	u, err := soap.ParseURL(c.config.URL)
	if err != nil {
		return fmt.Errorf("parse vCenter URL: %w", err)
	}
	u.User = url.UserPassword(c.config.User, c.config.Password)

	client, err := govmomi.NewClient(ctx, u, c.config.Insecure)
	if err != nil {
		return fmt.Errorf("connect to vCenter: %w", err)
	}

	// Enable keepalive to auto-reconnect
	client.RoundTripper = session.KeepAlive(client.RoundTripper, 5*time.Minute)

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
		c.mu.Unlock()
		err := c.Connect(ctx)
		c.mu.Lock()
		return err
	}

	// Check if session is still valid
	mgr := session.NewManager(c.client.Client)
	_, err := mgr.UserSession(ctx)
	if err != nil {
		c.logger.Warn("vCenter session expired, reconnecting", "error", err)
		c.mu.Unlock()
		err = c.Connect(ctx)
		c.mu.Lock()
		return err
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
}

// CloneVM clones a template into the Student-VMs folder.
// Returns the VM's managed object reference (MoRef) as a string.
func (c *Client) CloneVM(ctx context.Context, params CloneVMParams) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

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

	// Find network (port group)
	net, err := c.finder.Network(ctx, params.Network)
	if err != nil {
		return "", fmt.Errorf("find network %s: %w", params.Network, err)
	}
	netBacking, err := net.EthernetCardBackingInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("get network backing: %w", err)
	}

	// Build clone spec
	poolRef := pool.Reference()
	cloneSpec := types.VirtualMachineCloneSpec{
		Location: types.VirtualMachineRelocateSpec{
			Pool:      &poolRef,
			Datastore: &dsRef,
		},
		PowerOn:  false,
		Template: false,
		Config: &types.VirtualMachineConfigSpec{
			NumCPUs:  params.VCPUs,
			MemoryMB: params.RAMmb,
		},
	}

	// Set network adapter on the first NIC
	var templateMo mo.VirtualMachine
	err = template.Properties(ctx, template.Reference(), []string{"config.hardware.device"}, &templateMo)
	if err != nil {
		return "", fmt.Errorf("get template devices: %w", err)
	}

	for _, dev := range templateMo.Config.Hardware.Device {
		if nic, ok := dev.(types.BaseVirtualEthernetCard); ok {
			card := nic.GetVirtualEthernetCard()
			card.Backing = netBacking
			cloneSpec.Location.DeviceChange = append(cloneSpec.Location.DeviceChange, types.BaseVirtualDeviceConfigSpec(
				&types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationEdit,
					Device:    dev,
				},
			))
			break // only configure the first NIC
		}
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
	return vmRef.Value, nil
}

// DestroyVM powers off (if running) and destroys a VM by MoRef.
func (c *Client) DestroyVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	// Try to power off first (ignore errors if already off)
	powerOffTask, err := vm.PowerOff(ctx)
	if err == nil {
		_ = powerOffTask.Wait(ctx)
	}

	// Destroy
	destroyTask, err := vm.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("destroy VM %s: %w", moref, err)
	}
	if err := destroyTask.Wait(ctx); err != nil {
		return fmt.Errorf("destroy VM task %s: %w", moref, err)
	}

	c.logger.Info("VM destroyed", "moref", moref)
	return nil
}

// PowerOnVM powers on a VM.
func (c *Client) PowerOnVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	task, err := vm.PowerOn(ctx)
	if err != nil {
		return fmt.Errorf("power on %s: %w", moref, err)
	}
	return task.Wait(ctx)
}

// PowerOffVM powers off a VM.
func (c *Client) PowerOffVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	task, err := vm.PowerOff(ctx)
	if err != nil {
		return fmt.Errorf("power off %s: %w", moref, err)
	}
	return task.Wait(ctx)
}

// RestartVM guest-restarts a VM (graceful reboot via VMware Tools).
func (c *Client) RestartVM(ctx context.Context, moref string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	return vm.RebootGuest(ctx)
}

// WaitForIP waits for VMware Tools to report an IP address.
func (c *Client) WaitForIP(ctx context.Context, moref string, timeout time.Duration) (string, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return "", err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ip, err := vm.WaitForIP(timeoutCtx, true)
	if err != nil {
		return "", fmt.Errorf("wait for IP on %s: %w", moref, err)
	}

	c.logger.Info("VM got IP", "moref", moref, "ip", ip)
	return ip, nil
}

// GetVM retrieves a VM's properties by MoRef (for idempotency checks).
func (c *Client) GetVM(ctx context.Context, moref string) (*mo.VirtualMachine, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}

	vm := object.NewVirtualMachine(c.client.Client,
		types.ManagedObjectReference{Type: "VirtualMachine", Value: moref})

	var props mo.VirtualMachine
	err := vm.Properties(ctx, vm.Reference(), []string{"name", "runtime", "guest", "config"}, &props)
	if err != nil {
		return nil, err
	}
	return &props, nil
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

// ---------- Port Group Operations ----------

// CreatePortGroupOnAllHosts creates a standard vSwitch port group on every configured ESXi host.
func (c *Client) CreatePortGroupOnAllHosts(ctx context.Context, pgName string, vlanID int) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	for _, hostName := range c.config.Hosts {
		if err := c.createPortGroup(ctx, hostName, pgName, vlanID); err != nil {
			return fmt.Errorf("create port group on %s: %w", hostName, err)
		}
	}

	c.logger.Info("port group created on all hosts", "name", pgName, "vlan_id", vlanID, "hosts", len(c.config.Hosts))
	return nil
}

// DeletePortGroupOnAllHosts removes a port group from every configured ESXi host.
func (c *Client) DeletePortGroupOnAllHosts(ctx context.Context, pgName string) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}

	var lastErr error
	for _, hostName := range c.config.Hosts {
		if err := c.deletePortGroup(ctx, hostName, pgName); err != nil {
			c.logger.Warn("failed to delete port group", "host", hostName, "pg", pgName, "error", err)
			lastErr = err
			// Continue trying other hosts
		}
	}
	return lastErr
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

	return ns.RemovePortGroup(ctx, pgName)
}

// FindTemplate looks up a VM template by name in the datacenter.
func (c *Client) FindTemplate(ctx context.Context, name string) (*object.VirtualMachine, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, err
	}
	return c.finder.VirtualMachine(ctx, name)
}

// Ping verifies vCenter connectivity.
func (c *Client) Ping(ctx context.Context) error {
	if err := c.ensureConnected(ctx); err != nil {
		return err
	}
	_, err := c.finder.Datacenter(ctx, c.config.Datacenter)
	return err
}
