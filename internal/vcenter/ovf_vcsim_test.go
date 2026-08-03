package vcenter

// vcsim coverage for ovf.go (ImportOVA).
//
// vcsim implements CreateImportSpec + ImportVApp + the HttpNfcLease upload
// pipeline for a single-disk VirtualSystem descriptor (see
// simulator/ovf_manager.go and simulator/resource_pool.go), so we can drive a
// full OVA import end-to-end against the simulator using a synthetic tar built
// from the canonical ttylinux descriptor shape.
//
// The reject-bare-.ovf path needs no simulator work beyond proving that no VM
// is created when the input isn't a tar.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/vim25"
)

const (
	simResourcePool = "/DC0/host/DC0_C0/Resources"
	simNetwork      = "VM Network"
)

// ovfDescriptorTemplate is the ttylinux single-disk descriptor (govmomi's
// canonical vcsim-importable OVF), parameterized on the disk file href and its
// size so the tar entry, the References size, and the upload Content-Length all
// agree.
const ovfDescriptorTemplate = `<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
          xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData">
  <References>
    <File ovf:href="%s" ovf:id="file1" ovf:size="%d"/>
  </References>
  <DiskSection>
    <Info>Virtual disk information</Info>
    <Disk ovf:capacity="30" ovf:capacityAllocationUnits="byte * 2^20" ovf:diskId="vmdisk1" ovf:fileRef="file1"
          ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized" ovf:populatedSize="1024"/>
  </DiskSection>
  <NetworkSection>
    <Info>The list of logical networks</Info>
    <Network ovf:name="nat">
      <Description>The nat network</Description>
    </Network>
  </NetworkSection>
  <VirtualSystem ovf:id="vm">
    <Info>A virtual machine</Info>
    <Name>synthetic-appliance</Name>
    <OperatingSystemSection ovf:id="36">
      <Info>The kind of installed guest operating system</Info>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>synthetic-appliance</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-09</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
        <rasd:Description>Number of Virtual CPUs</rasd:Description>
        <rasd:ElementName>1 virtual CPU(s)</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>1</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:Description>Memory Size</rasd:Description>
        <rasd:ElementName>32MB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>32</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:Description>IDE Controller</rasd:Description>
        <rasd:ElementName>ideController0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceType>5</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>0</rasd:AddressOnParent>
        <rasd:ElementName>disk0</rasd:ElementName>
        <rasd:HostResource>ovf:/disk/vmdisk1</rasd:HostResource>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>17</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>1</rasd:AddressOnParent>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Connection>nat</rasd:Connection>
        <rasd:Description>E1000 ethernet adapter on &quot;nat&quot;</rasd:Description>
        <rasd:ElementName>ethernet0</rasd:ElementName>
        <rasd:InstanceID>5</rasd:InstanceID>
        <rasd:ResourceSubType>E1000</rasd:ResourceSubType>
        <rasd:ResourceType>10</rasd:ResourceType>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

func tarEntry(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar WriteHeader(%q): %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar Write(%q): %v", name, err)
	}
}

// buildSyntheticOVA returns a tar (.ova) stream whose first entry is the OVF
// descriptor followed by the referenced disk file, with sizes kept consistent.
func buildSyntheticOVA(t *testing.T) []byte {
	t.Helper()
	diskHref := "synthetic-appliance-disk1.vmdk"
	vmdk := bytes.Repeat([]byte("VMDK"), 512) // 2048 bytes of arbitrary content
	descriptor := fmt.Sprintf(ovfDescriptorTemplate, diskHref, len(vmdk))

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tarEntry(t, tw, "synthetic-appliance.ovf", []byte(descriptor)) // descriptor MUST be first
	tarEntry(t, tw, diskHref, vmdk)
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	return buf.Bytes()
}

func countVMs(t *testing.T, ctx context.Context, vimc *vim25.Client) int {
	t.Helper()
	finder := find.NewFinder(vimc, true)
	dc, err := finder.Datacenter(ctx, "DC0")
	if err != nil {
		t.Fatalf("find DC0: %v", err)
	}
	finder.SetDatacenter(dc)
	vms, err := finder.VirtualMachineList(ctx, "*")
	if err != nil {
		t.Fatalf("VirtualMachineList: %v", err)
	}
	return len(vms)
}

func TestImportOVA_RejectsBareOVF(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		before := countVMs(t, ctx, vimc)

		// A raw .ovf descriptor (not wrapped in a tar) must be rejected before
		// any VM is created.
		bareOVF := []byte(fmt.Sprintf(ovfDescriptorTemplate, "disk1.vmdk", 2048))

		moref, err := c.ImportOVA(ctx, OVAImportParams{
			Reader:       bytes.NewReader(bareOVF),
			Size:         int64(len(bareOVF)),
			VMName:       "should-not-exist",
			Datastore:    simDatastore,
			ResourcePool: simResourcePool,
			Network:      simNetwork,
		})
		if err == nil {
			t.Fatalf("ImportOVA(bare .ovf) = %q, want error", moref)
		}
		if !errors.Is(err, ErrNotAnOVA) {
			t.Errorf("error %v should wrap ErrNotAnOVA", err)
		}
		if moref != "" {
			t.Errorf("moref = %q, want empty on rejection", moref)
		}

		after := countVMs(t, ctx, vimc)
		if after != before {
			t.Errorf("VM count changed from %d to %d; a rejected bare .ovf must not create a VM", before, after)
		}
	})
}

func TestImportOVA_vcsim(t *testing.T) {
	withSimulator(t, func(ctx context.Context, c *Client, vimc *vim25.Client) {
		before := countVMs(t, ctx, vimc)

		ova := buildSyntheticOVA(t)
		const vmName = "imported-appliance"

		moref, err := c.ImportOVA(ctx, OVAImportParams{
			Reader:       bytes.NewReader(ova),
			Size:         int64(len(ova)),
			VMName:       vmName,
			Datastore:    simDatastore,
			ResourcePool: simResourcePool,
			Network:      simNetwork,
		})
		if err != nil {
			// vcsim's OVF import is real but partial; if a genuine simulator
			// limitation surfaces here, surface it loudly rather than skipping
			// silently. (End-to-end import is exercised above in dev/CI against
			// this govmomi version, where it succeeds.)
			t.Fatalf("ImportOVA: %v", err)
		}
		if moref == "" {
			t.Fatal("ImportOVA returned an empty moref")
		}
		if !strings.HasPrefix(moref, "vm-") {
			t.Errorf("moref %q does not look like a VM reference (vm-NNN)", moref)
		}

		after := countVMs(t, ctx, vimc)
		if after != before+1 {
			t.Errorf("VM count = %d, want %d (import should create exactly one VM)", after, before+1)
		}

		// The created VM should be resolvable by the name we asked for.
		resolved, err := c.ResolveVMByName(ctx, vmName)
		if err != nil {
			t.Fatalf("ResolveVMByName(%q): %v", vmName, err)
		}
		if resolved != moref {
			t.Errorf("ResolveVMByName(%q) = %q, want %q", vmName, resolved, moref)
		}
	})
}
