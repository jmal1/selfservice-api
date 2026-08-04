package preflight_test

// preflight_test.go — Unit tests for all 11 preflight checks.
//
// Design: every check has a passing case and a failing "negative control"
// that constructs the exact fault the check guards against and verifies the
// check goes red. A check that is never observed failing is decoration.
//
// fakeVCenter is the narrow-interface fake. Its fields are plain maps so
// each test configures exactly the state it needs without wiring up
// govmomi/vcsim. This matches the narrow-interface pattern used elsewhere
// (see internal/provisioner/template_jobs.go § isoProvisionVCenter).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
	"github.com/jmal1/selfservice-api/internal/vcenter/preflight"
)

// Compile-time proof that *vcenter.Client satisfies the interface.
// Placed here (rather than in preflight.go itself) so the preflight package
// does not need to import vcenter, keeping the dependency direction simple.
var _ preflight.PreflightVCenter = (*vcenter.Client)(nil)

// --- fake implementation ---

type fakeVCenter struct {
	// keyed by moref
	vmProps  map[string]*mo.VirtualMachine
	vmErrors map[string]error
	// keyed by datastore name
	dsInfo   map[string]*mo.Datastore
	dsErrors map[string]error
	// keyed by moref
	tasks      map[string][]types.TaskInfo
	taskErrors map[string]error
	// keyed by "folderPath/vmName"
	vmInFolder map[string]bool
	vmFolderErr map[string]error
	// keyed by "datastoreName/filePath"
	dsFiles    map[string]bool
	dsFileErr  map[string]error
	// keyed by host moref
	portGroups    map[string][]string
	portGroupErrs map[string]error
	// keyed by host moref → cluster name
	clusterNames    map[string]string
	clusterNameErrs map[string]error
	// keyed by datastore name → host morefs
	dsHosts    map[string][]string
	dsHostErrs map[string]error
	// keyed by host moref → all cluster host morefs
	clusterHosts    map[string][]string
	clusterHostErrs map[string]error
}

func newFake() *fakeVCenter {
	return &fakeVCenter{
		vmProps:         make(map[string]*mo.VirtualMachine),
		vmErrors:        make(map[string]error),
		dsInfo:          make(map[string]*mo.Datastore),
		dsErrors:        make(map[string]error),
		tasks:           make(map[string][]types.TaskInfo),
		taskErrors:      make(map[string]error),
		vmInFolder:      make(map[string]bool),
		vmFolderErr:     make(map[string]error),
		dsFiles:         make(map[string]bool),
		dsFileErr:       make(map[string]error),
		portGroups:      make(map[string][]string),
		portGroupErrs:   make(map[string]error),
		clusterNames:    make(map[string]string),
		clusterNameErrs: make(map[string]error),
		dsHosts:         make(map[string][]string),
		dsHostErrs:      make(map[string]error),
		clusterHosts:    make(map[string][]string),
		clusterHostErrs: make(map[string]error),
	}
}

func (f *fakeVCenter) FetchVMProps(_ context.Context, moref string) (*mo.VirtualMachine, error) {
	if err, ok := f.vmErrors[moref]; ok {
		return nil, err
	}
	v, ok := f.vmProps[moref]
	if !ok {
		return nil, errors.New("VM not found: " + moref)
	}
	return v, nil
}
func (f *fakeVCenter) DatastoreInfo(_ context.Context, name string) (*mo.Datastore, error) {
	if err, ok := f.dsErrors[name]; ok {
		return nil, err
	}
	d, ok := f.dsInfo[name]
	if !ok {
		return nil, errors.New("datastore not found: " + name)
	}
	return d, nil
}
func (f *fakeVCenter) InFlightTasksForVM(_ context.Context, vmMoref string) ([]types.TaskInfo, error) {
	if err, ok := f.taskErrors[vmMoref]; ok {
		return nil, err
	}
	return f.tasks[vmMoref], nil
}
func (f *fakeVCenter) VMExistsInFolder(_ context.Context, folderPath, vmName string) (bool, error) {
	key := folderPath + "/" + vmName
	if err, ok := f.vmFolderErr[key]; ok {
		return false, err
	}
	return f.vmInFolder[key], nil
}
func (f *fakeVCenter) DatastoreFileExists(_ context.Context, datastoreName, filePath string) (bool, error) {
	key := datastoreName + "/" + filePath
	if err, ok := f.dsFileErr[key]; ok {
		return false, err
	}
	return f.dsFiles[key], nil
}
func (f *fakeVCenter) HostPortGroupNames(_ context.Context, hostMoref string) ([]string, error) {
	if err, ok := f.portGroupErrs[hostMoref]; ok {
		return nil, err
	}
	return f.portGroups[hostMoref], nil
}
func (f *fakeVCenter) ClusterNameForHost(_ context.Context, hostMoref string) (string, error) {
	if err, ok := f.clusterNameErrs[hostMoref]; ok {
		return "", err
	}
	return f.clusterNames[hostMoref], nil
}
func (f *fakeVCenter) DatastoreHostMorefs(_ context.Context, datastoreName string) ([]string, error) {
	if err, ok := f.dsHostErrs[datastoreName]; ok {
		return nil, err
	}
	return f.dsHosts[datastoreName], nil
}
func (f *fakeVCenter) ClusterHostMorefs(_ context.Context, hostMoref string) ([]string, error) {
	if err, ok := f.clusterHostErrs[hostMoref]; ok {
		return nil, err
	}
	if hosts, ok := f.clusterHosts[hostMoref]; ok {
		return hosts, nil
	}
	return []string{hostMoref}, nil // default: standalone, just itself
}

// helpers for building fakeVCenter state

func hostRef(moref string) *types.ManagedObjectReference {
	return &types.ManagedObjectReference{Type: "HostSystem", Value: moref}
}

func vmWithHost(hostMoref string) *mo.VirtualMachine {
	return &mo.VirtualMachine{
		ManagedEntity: mo.ManagedEntity{},
		Runtime: types.VirtualMachineRuntimeInfo{
			Host: hostRef(hostMoref),
		},
		Guest: &types.GuestInfo{
			ToolsRunningStatus: string(types.VirtualMachineToolsRunningStatusGuestToolsRunning),
		},
		Config: &types.VirtualMachineConfigInfo{
			Hardware: types.VirtualHardware{
				Device: []types.BaseVirtualDevice{
					&types.VirtualDisk{
						VirtualDevice: types.VirtualDevice{
							DeviceInfo: &types.Description{
								Summary: "Hard disk 1",
							},
							Backing: &types.VirtualDiskFlatVer2BackingInfo{
								VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
									FileName: "[NAS-vmstore] templates/disk.vmdk",
								},
							},
						},
					},
				},
			},
		},
		Summary: types.VirtualMachineSummary{
			Storage: &types.VirtualMachineStorageSummary{
				Committed:   int64(20 * 1 << 30), // 20 GiB
				Uncommitted: int64(5 * 1 << 30),  // 5 GiB
			},
		},
	}
}

func datastoreWithFreeSpace(freeGiB int64) *mo.Datastore {
	return &mo.Datastore{
		Summary: types.DatastoreSummary{
			FreeSpace: freeGiB * (1 << 30),
		},
		Host: []types.DatastoreHostMount{
			{Key: types.ManagedObjectReference{Type: "HostSystem", Value: "host-1"}},
		},
	}
}

// --- tests ---

// PF-01: source moref resolves

func TestPF01_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf01 := findResult(t, r, "PF-01")
	if !pf01.OK {
		t.Fatalf("PF-01 should pass when VM resolves; got Detail=%q", pf01.Detail)
	}
}

// Negative control: moref does not exist.
func TestPF01_Fail_MoRefMissing(t *testing.T) {
	f := newFake()
	// vm-404 is not in vmProps, so FetchVMProps returns an error.
	p := preflight.Params{SourceMoref: "vm-404", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf01 := findResult(t, r, "PF-01")
	if pf01.OK {
		t.Fatal("PF-01 should fail when moref does not resolve")
	}
	if pf01.Severity != "block" {
		t.Errorf("PF-01 severity = %q; want block", pf01.Severity)
	}
}

// PF-02: cluster has configured resource pool

func TestPF02_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterNames["host-1"] = "AMD-Cluster"
	p := preflight.Params{
		SourceMoref:                 "vm-1",
		SourceType:                  models.TemplateSourceCloneVCenter,
		ConfiguredResourcePoolPaths: []string{"AMD-Cluster/Resources/Student-VMs"},
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf02 := findResult(t, r, "PF-02")
	if !pf02.OK {
		t.Fatalf("PF-02 should pass; got Detail=%q", pf02.Detail)
	}
}

// Negative control: none of the configured pools are in the source cluster.
func TestPF02_Fail_NoPoolInCluster(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterNames["host-1"] = "Intel-Cluster"
	p := preflight.Params{
		SourceMoref:                 "vm-1",
		SourceType:                  models.TemplateSourceCloneVCenter,
		ConfiguredResourcePoolPaths: []string{"AMD-Cluster/Resources/Student-VMs"},
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf02 := findResult(t, r, "PF-02")
	if pf02.OK {
		t.Fatal("PF-02 should fail when no pool is in the source cluster")
	}
}

// PF-03: target datastore mounted on cluster

func TestPF03_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterHosts["host-1"] = []string{"host-1", "host-2"}
	f.dsHosts["NAS-vmstore"] = []string{"host-1", "host-2"}
	p := preflight.Params{
		SourceMoref:   "vm-1",
		DatastoreName: "NAS-vmstore",
		SourceType:    models.TemplateSourceCloneVCenter,
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf03 := findResult(t, r, "PF-03")
	if !pf03.OK {
		t.Fatalf("PF-03 should pass; got Detail=%q", pf03.Detail)
	}
}

// Negative control: one cluster host does not have the datastore.
func TestPF03_Fail_DatastoreNotOnAllHosts(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterHosts["host-1"] = []string{"host-1", "host-2", "host-3"}
	f.dsHosts["NAS-vmstore"] = []string{"host-1", "host-2"} // host-3 is missing
	p := preflight.Params{
		SourceMoref:   "vm-1",
		DatastoreName: "NAS-vmstore",
		SourceType:    models.TemplateSourceCloneVCenter,
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf03 := findResult(t, r, "PF-03")
	if pf03.OK {
		t.Fatal("PF-03 should fail when datastore is not mounted on all cluster hosts")
	}
}

// PF-04: datastore free space ≥ provisioned × 1.2

func TestPF04_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1") // 25 GiB provisioned; need ≥ 30 GiB
	f.dsInfo["NAS-vmstore"] = datastoreWithFreeSpace(50)
	p := preflight.Params{
		SourceMoref:   "vm-1",
		DatastoreName: "NAS-vmstore",
		SourceType:    models.TemplateSourceCloneVCenter,
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf04 := findResult(t, r, "PF-04")
	if !pf04.OK {
		t.Fatalf("PF-04 should pass; got Detail=%q", pf04.Detail)
	}
}

// Negative control: datastore has less than 1.2× provisioned.
func TestPF04_Fail_InsufficientSpace(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1") // 25 GiB provisioned; need ≥ 30 GiB
	f.dsInfo["NAS-vmstore"] = datastoreWithFreeSpace(10) // only 10 GiB free
	p := preflight.Params{
		SourceMoref:   "vm-1",
		DatastoreName: "NAS-vmstore",
		SourceType:    models.TemplateSourceCloneVCenter,
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf04 := findResult(t, r, "PF-04")
	if pf04.OK {
		t.Fatal("PF-04 should fail when datastore has insufficient free space")
	}
}

// PF-05: disk chain intact

func TestPF05_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1") // vmWithHost has a disk with non-empty FileName
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf05 := findResult(t, r, "PF-05")
	if !pf05.OK {
		t.Fatalf("PF-05 should pass; got Detail=%q", pf05.Detail)
	}
}

// Negative control: snapshot tree exists but CurrentSnapshot is nil.
func TestPF05_Fail_BrokenSnapshotChain(t *testing.T) {
	f := newFake()
	vm := vmWithHost("host-1")
	// A snapshot tree with nil CurrentSnapshot = broken chain.
	vm.Snapshot = &types.VirtualMachineSnapshotInfo{
		CurrentSnapshot: nil, // broken
	}
	f.vmProps["vm-broken"] = vm
	p := preflight.Params{SourceMoref: "vm-broken", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf05 := findResult(t, r, "PF-05")
	if pf05.OK {
		t.Fatal("PF-05 should fail when snapshot tree has nil CurrentSnapshot")
	}
}

// Negative control 2: disk has an empty backing FileName.
func TestPF05_Fail_EmptyDiskBacking(t *testing.T) {
	f := newFake()
	vm := vmWithHost("host-1")
	vm.Config.Hardware.Device = []types.BaseVirtualDevice{
		&types.VirtualDisk{
			VirtualDevice: types.VirtualDevice{
				DeviceInfo: &types.Description{Summary: "Hard disk 1"},
				Backing: &types.VirtualDiskFlatVer2BackingInfo{
					VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
						FileName: "", // empty = orphaned descriptor
					},
				},
			},
		},
	}
	f.vmProps["vm-nodisk"] = vm
	p := preflight.Params{SourceMoref: "vm-nodisk", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf05 := findResult(t, r, "PF-05")
	if pf05.OK {
		t.Fatal("PF-05 should fail when a disk has an empty backing filename")
	}
}

// PF-06: no in-flight tasks

func TestPF06_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.tasks["vm-1"] = nil // no tasks
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf06 := findResult(t, r, "PF-06")
	if !pf06.OK {
		t.Fatalf("PF-06 should pass when no tasks are in flight; got Detail=%q", pf06.Detail)
	}
}

// Negative control: a clone task is running on the source VM.
func TestPF06_Fail_CloneTaskInFlight(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	started := time.Now().Add(-30 * time.Second)
	f.tasks["vm-1"] = []types.TaskInfo{
		{
			Name:      "CloneVM_Task",
			State:     types.TaskInfoStateRunning,
			Entity:    &types.ManagedObjectReference{Type: "VirtualMachine", Value: "vm-1"},
			StartTime: &started,
		},
	}
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf06 := findResult(t, r, "PF-06")
	if pf06.OK {
		t.Fatal("PF-06 should fail when a clone task is in flight on the source VM")
	}
	// Detail should mention which task and how long it's been running.
	if !containsAny(pf06.Detail, "CloneVM_Task", "clone") {
		t.Errorf("PF-06 Detail should mention the task name; got %q", pf06.Detail)
	}
}

// PF-07: VMware Tools present and running (clone-based only)

func TestPF07_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1") // vmWithHost sets tools = running
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf07 := findResult(t, r, "PF-07")
	if !pf07.OK {
		t.Fatalf("PF-07 should pass when tools are running; got Detail=%q", pf07.Detail)
	}
}

// Negative control: tools status is "guestToolsNotRunning".
func TestPF07_Fail_ToolsNotRunning(t *testing.T) {
	f := newFake()
	vm := vmWithHost("host-1")
	vm.Guest.ToolsRunningStatus = string(types.VirtualMachineToolsRunningStatusGuestToolsNotRunning)
	f.vmProps["vm-no-tools"] = vm
	p := preflight.Params{SourceMoref: "vm-no-tools", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf07 := findResult(t, r, "PF-07")
	if pf07.OK {
		t.Fatal("PF-07 should fail when VMware Tools is not running")
	}
}

// PF-07 skipped for ISO sources.
func TestPF07_SkippedForISO(t *testing.T) {
	f := newFake()
	p := preflight.Params{SourceType: models.TemplateSourceISO, ISORef: "[ds] iso/file.iso"}
	r := preflight.RunAll(context.Background(), f, p)
	pf07 := findResult(t, r, "PF-07")
	if !pf07.OK {
		t.Fatal("PF-07 should be skipped (OK=true) for ISO sources")
	}
}

// PF-08: guest credentials resolvable (warn)

func TestPF08_Pass(t *testing.T) {
	p := preflight.Params{
		SourceType:    models.TemplateSourceCloneVCenter,
		GuestUsername: "student",
		GuestPassword: "s3cr3t",
	}
	r := preflight.RunAll(context.Background(), newFake(), p)
	pf08 := findResult(t, r, "PF-08")
	if !pf08.OK {
		t.Fatalf("PF-08 should pass when both credentials are set; got Detail=%q", pf08.Detail)
	}
	if pf08.Severity != "warn" {
		t.Errorf("PF-08 severity = %q; want warn", pf08.Severity)
	}
}

// Negative control: password is missing.
func TestPF08_Fail_MissingPassword(t *testing.T) {
	p := preflight.Params{
		SourceType:    models.TemplateSourceCloneVCenter,
		GuestUsername: "student",
		GuestPassword: "", // missing
	}
	r := preflight.RunAll(context.Background(), newFake(), p)
	pf08 := findResult(t, r, "PF-08")
	if pf08.OK {
		t.Fatal("PF-08 should fail when password is missing")
	}
	if pf08.Severity != "warn" {
		t.Errorf("PF-08 severity = %q; want warn (missing creds are a warn, not a block)", pf08.Severity)
	}
}

// PF-09: target VM name free

func TestPF09_Pass(t *testing.T) {
	f := newFake()
	// "DC/vm/Templates/tpl-ubuntu-abc123" does not exist
	p := preflight.Params{
		SourceType:       models.TemplateSourceCloneVCenter,
		TargetVMName:     "tpl-ubuntu-abc123",
		TargetFolderPath: "DC/vm/Templates",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf09 := findResult(t, r, "PF-09")
	if !pf09.OK {
		t.Fatalf("PF-09 should pass when no VM with that name exists; got Detail=%q", pf09.Detail)
	}
}

// Negative control: a VM with the intended target name already exists.
func TestPF09_Fail_NameCollision(t *testing.T) {
	f := newFake()
	f.vmInFolder["DC/vm/Templates/tpl-ubuntu-abc123"] = true
	p := preflight.Params{
		SourceType:       models.TemplateSourceCloneVCenter,
		TargetVMName:     "tpl-ubuntu-abc123",
		TargetFolderPath: "DC/vm/Templates",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf09 := findResult(t, r, "PF-09")
	if pf09.OK {
		t.Fatal("PF-09 should fail when a VM with the target name already exists")
	}
}

// PF-10: ISO file exists (ISO sources only)

func TestPF10_Pass(t *testing.T) {
	f := newFake()
	f.dsFiles["NAS-BackupsAndISOS/ISOs/ubuntu-24.04.iso"] = true
	p := preflight.Params{
		SourceType: models.TemplateSourceISO,
		ISORef:     "[NAS-BackupsAndISOS] ISOs/ubuntu-24.04.iso",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf10 := findResult(t, r, "PF-10")
	if !pf10.OK {
		t.Fatalf("PF-10 should pass when ISO exists; got Detail=%q", pf10.Detail)
	}
}

// Negative control: ISO file does not exist.
func TestPF10_Fail_ISONotFound(t *testing.T) {
	f := newFake()
	// dsFiles does NOT contain the key — file does not exist
	p := preflight.Params{
		SourceType: models.TemplateSourceISO,
		ISORef:     "[NAS-BackupsAndISOS] ISOs/missing.iso",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf10 := findResult(t, r, "PF-10")
	if pf10.OK {
		t.Fatal("PF-10 should fail when ISO file does not exist on the datastore")
	}
}

// Negative control 2: ISORef is malformed.
func TestPF10_Fail_MalformedRef(t *testing.T) {
	p := preflight.Params{
		SourceType: models.TemplateSourceISO,
		ISORef:     "not-a-valid-path",
	}
	r := preflight.RunAll(context.Background(), newFake(), p)
	pf10 := findResult(t, r, "PF-10")
	if pf10.OK {
		t.Fatal("PF-10 should fail when ISORef is not a valid datastore path")
	}
}

// PF-10 skipped for non-ISO sources.
func TestPF10_SkippedForClone(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	p := preflight.Params{SourceMoref: "vm-1", SourceType: models.TemplateSourceCloneVCenter}
	r := preflight.RunAll(context.Background(), f, p)
	pf10 := findResult(t, r, "PF-10")
	if !pf10.OK {
		t.Fatal("PF-10 should be skipped (OK=true) for clone sources")
	}
}

// PF-11: staging port group on all hosts (warn)

func TestPF11_Pass(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterHosts["host-1"] = []string{"host-1", "host-2"}
	f.portGroups["host-1"] = []string{"PG-VM-Lab", "VM Network"}
	f.portGroups["host-2"] = []string{"PG-VM-Lab", "VM Network"}
	p := preflight.Params{
		SourceMoref:      "vm-1",
		SourceType:       models.TemplateSourceCloneVCenter,
		StagingPortGroup: "PG-VM-Lab",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf11 := findResult(t, r, "PF-11")
	if !pf11.OK {
		t.Fatalf("PF-11 should pass when port group is on all hosts; got Detail=%q", pf11.Detail)
	}
	if pf11.Severity != "warn" {
		t.Errorf("PF-11 severity = %q; want warn", pf11.Severity)
	}
}

// Negative control: port group is missing on one cluster host.
func TestPF11_Fail_PortGroupMissingOnHost(t *testing.T) {
	f := newFake()
	f.vmProps["vm-1"] = vmWithHost("host-1")
	f.clusterHosts["host-1"] = []string{"host-1", "host-2"}
	f.portGroups["host-1"] = []string{"PG-VM-Lab"}
	f.portGroups["host-2"] = []string{"VM Network"} // PG-VM-Lab missing on host-2
	p := preflight.Params{
		SourceMoref:      "vm-1",
		SourceType:       models.TemplateSourceCloneVCenter,
		StagingPortGroup: "PG-VM-Lab",
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf11 := findResult(t, r, "PF-11")
	if pf11.OK {
		t.Fatal("PF-11 should fail when port group is missing on a cluster host")
	}
}

// AnyBlockFailed

func TestAnyBlockFailed(t *testing.T) {
	results := []preflight.Result{
		{ID: "PF-01", Severity: "block", OK: true},
		{ID: "PF-08", Severity: "warn", OK: false},
	}
	if preflight.AnyBlockFailed(results) {
		t.Error("AnyBlockFailed should be false when only warn checks fail")
	}
	results = append(results, preflight.Result{ID: "PF-03", Severity: "block", OK: false})
	if !preflight.AnyBlockFailed(results) {
		t.Error("AnyBlockFailed should be true when a block check fails")
	}
}

// --- helpers ---

func findResult(t *testing.T, results []preflight.Result, id string) preflight.Result {
	t.Helper()
	for _, r := range results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no result with ID %q in %v", id, results)
	return preflight.Result{}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
