package vcenter

import (
	"context"
	"encoding/base64"
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
	// TemplateFolder is where template *build* VMs live, e.g.
	// "/JMAL-Datacenter/vm/templates". It is deliberately separate from
	// VMFolder: VMFolder holds ephemeral student pod VMs and is what the
	// orphan reconciler scans, so a long-lived template shell parked there
	// would be reported as an orphan forever. CreateBlankVM falls back to
	// this when the caller does not name a folder.
	TemplateFolder string
	ResourcePools  []string // e.g., ["AMD-Cluster/Resources/Student-VMs", "Intel-Cluster/Resources/Student-VMs"]
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
	// TemplateName accepts either a vCenter inventory name (e.g.
	// "student-windows-11") or a managed-object reference like
	// "vm-8942". Wizard-published templates only persist the moref
	// (templates.vcenter_vm_id); legacy templates registered by name
	// persist the inventory name (templates.vcenter_template).
	// resolveSourceVM in cloneVMInner detects which form was passed.
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

// cloneVMInner contains the actual clone logic (called by CloneVM via withRetry).
func (c *Client) cloneVMInner(ctx context.Context, params CloneVMParams) (string, error) {
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

	// Idempotency: if a VM with this name already exists in the target
	// folder, return it instead of cloning again. The pod_create job may
	// be re-executed after a worker crash (RecoverStaleJobs re-queues any
	// in_progress job at worker startup). The provisioner-layer guard in
	// create.go covers the common case where the pod_vms row recorded
	// vcenter_vm_id before the worker died; this lower-level guard covers
	// the race where the clone task completed but the DB update did not.
	// Mirrors CloneTemplate's idempotency in template_ops.go.
	if existing, lookupErr := c.findVMInFolder(ctx, folder, params.VMName); lookupErr == nil && existing != "" {
		c.logger.Info("clone target already exists, reusing",
			"name", params.VMName, "moref", existing)
		return existing, nil
	}

	// Select best resource pool based on available resources, but
	// constrained to the template's cluster.
	pool, err := c.selectBestPoolInSourceCluster(ctx, tmplProps.Runtime.Host, params.VCPUs, params.RAMmb)
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

// resolvePlacementPool picks the resource pool for a VM that has no source VM
// to inherit placement from — a blank ISO shell or an imported OVA.
//
// The explicit path is honoured first, then the pools this deployment is
// actually configured with, and only then vSphere's notion of a "default"
// pool. That last fallback used to be the only one, and it is a trap: with
// more than one cluster in the datacenter, DefaultResourcePool cannot choose
// and fails with "default resource pool resolves to multiple instances,
// please specify" — an error that names no config key and no caller. No
// production caller sets ResourcePool (nothing builds a
// TemplateProvisionPayload with a pool), so every ISO template build on a
// multi-cluster vCenter failed there, which is exactly how this was found.
//
// Going through selectBestPool also means a template shell lands via the same
// RAM-weighted placement as every pod VM, rather than wherever vSphere would
// have guessed.
func (c *Client) resolvePlacementPool(ctx context.Context, explicitPath string, vcpus int32, ramMB int64) (*object.ResourcePool, error) {
	if explicitPath != "" {
		pool, err := c.finder.ResourcePool(ctx, explicitPath)
		if err != nil {
			return nil, fmt.Errorf("find resource pool %q: %w", explicitPath, err)
		}
		return pool, nil
	}

	if len(c.config.ResourcePools) > 0 {
		pool, err := c.selectBestPool(ctx, vcpus, ramMB)
		if err == nil {
			return pool, nil
		}
		// Configured but unusable (renamed, or vCenter refused the stats
		// lookup). Say so, then still try the default so a half-broken
		// config degrades instead of hard-failing.
		c.logger.Warn("configured resource pools unusable, falling back to datacenter default",
			"pools", c.config.ResourcePools, "error", err)
	}

	pool, err := c.finder.DefaultResourcePool(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve resource pool (set the params' ResourcePool or vcenter VCENTER_RESOURCE_POOLS config; the datacenter default is ambiguous with multiple clusters): %w", err)
	}
	return pool, nil
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
