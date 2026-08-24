package vcenter

import (
	"strings"
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func replicaValidationFacts(name, diskFile string, tpmIdentity []byte) *replicaVMFacts {
	secureBoot := true
	config := &types.VirtualMachineConfigInfo{
		Name:     name,
		Firmware: string(types.GuestOsDescriptorFirmwareTypeEfi),
		BootOptions: &types.VirtualMachineBootOptions{
			EfiSecureBootEnabled: &secureBoot,
		},
		KeyId: &types.CryptoKeyId{
			KeyId: "shared-config-key-is-allowed",
			ProviderId: &types.KeyProviderId{
				Id: "provider-1",
			},
		},
	}
	facts := &replicaVMFacts{
		Props:    mo.VirtualMachine{Config: config},
		Provider: "provider-1",
		Disks: []*types.VirtualDisk{{
			VirtualDevice: types.VirtualDevice{
				Key: 2000,
				Backing: &types.VirtualDiskFlatVer2BackingInfo{
					VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: diskFile},
					DiskMode:                     string(types.VirtualDiskModePersistent),
				},
			},
		}},
	}
	if tpmIdentity != nil {
		facts.TPMs = []*types.VirtualTPM{{
			EndorsementKeyCertificate: [][]byte{tpmIdentity},
		}}
	}
	return facts
}

func TestValidateReplicaBuildHardwareNonVTPMAllowsSharedConfigKey(t *testing.T) {
	source := replicaValidationFacts("source", "[source] source.vmdk", nil)
	destination := replicaValidationFacts("destination", "[target] destination.vmdk", nil)
	if err := validateReplicaBuildHardware(source, destination); err != nil {
		t.Fatal(err)
	}
}

func TestValidateReplicaBuildHardwareVTPMRequiresDistinctIdentity(t *testing.T) {
	source := replicaValidationFacts("source", "[source] source.vmdk", []byte("source-ek"))
	destination := replicaValidationFacts("destination", "[target] destination.vmdk", []byte("destination-ek"))
	if err := validateReplicaBuildHardware(source, destination); err != nil {
		t.Fatal(err)
	}

	destination.TPMs[0].EndorsementKeyCertificate = [][]byte{[]byte("source-ek")}
	err := validateReplicaBuildHardware(source, destination)
	if err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("shared vTPM identity error=%v, want fail-closed reuse diagnosis", err)
	}
}

func TestValidateReplicaBuildHardwareRejectsSecurityAndBackingDrift(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*replicaVMFacts)
		wantErr string
	}{
		{
			name: "firmware mismatch",
			mutate: func(destination *replicaVMFacts) {
				destination.Props.Config.Firmware = string(types.GuestOsDescriptorFirmwareTypeBios)
			},
			wantErr: "firmware",
		},
		{
			name: "security provider mismatch",
			mutate: func(destination *replicaVMFacts) {
				destination.Provider = "provider-2"
			},
			wantErr: "security provider",
		},
		{
			name: "empty config key",
			mutate: func(destination *replicaVMFacts) {
				destination.Props.Config.KeyId.KeyId = ""
			},
			wantErr: "empty configuration encryption key",
		},
		{
			name: "parent backing retained",
			mutate: func(destination *replicaVMFacts) {
				destination.Disks[0].Backing.(*types.VirtualDiskFlatVer2BackingInfo).Parent =
					&types.VirtualDiskFlatVer2BackingInfo{}
			},
			wantErr: "parent backing",
		},
		{
			name: "source backing reused",
			mutate: func(destination *replicaVMFacts) {
				destination.Disks[0].Backing.(*types.VirtualDiskFlatVer2BackingInfo).FileName =
					"[source] source.vmdk"
			},
			wantErr: "reuses source backing",
		},
		{
			name: "unexpected ISO",
			mutate: func(destination *replicaVMFacts) {
				destination.CDROMs = []*types.VirtualCdrom{{
					VirtualDevice: types.VirtualDevice{
						Backing: &types.VirtualCdromIsoBackingInfo{
							VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
								FileName: "[iso] installer.iso",
							},
						},
					},
				}}
			},
			wantErr: "unexpected ISO",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := replicaValidationFacts("source", "[source] source.vmdk", nil)
			destination := replicaValidationFacts("destination", "[target] destination.vmdk", nil)
			tt.mutate(destination)
			err := validateReplicaBuildHardware(source, destination)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error=%v, want text %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReplicaSourceSnapshotRejectsDriftAndAmbiguity(t *testing.T) {
	info := &types.VirtualMachineSnapshotInfo{
		RootSnapshotList: []types.VirtualMachineSnapshotTree{{
			Snapshot: types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: "snapshot-1"},
			Name:     "base-image",
		}},
	}
	if err := validateReplicaSourceSnapshot(info, "base-image", "snapshot-1"); err != nil {
		t.Fatal(err)
	}
	if err := validateReplicaSourceSnapshot(info, "base-image", "snapshot-old"); err == nil ||
		!strings.Contains(err.Error(), "drifted") {
		t.Fatalf("snapshot drift error=%v", err)
	}
	info.RootSnapshotList = append(info.RootSnapshotList, types.VirtualMachineSnapshotTree{
		Snapshot: types.ManagedObjectReference{Type: "VirtualMachineSnapshot", Value: "snapshot-2"},
		Name:     "base-image",
	})
	if err := validateReplicaSourceSnapshot(info, "base-image", "snapshot-1"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate snapshot error=%v", err)
	}
}

func TestValidateReplicaBuildCanaryBackingsRequiresExactRetainedParent(t *testing.T) {
	retained := replicaValidationFacts("retained", "[replica] retained.vmdk", nil)
	canary := replicaValidationFacts("canary", "[student] canary.vmdk", nil)
	canaryBacking := canary.Disks[0].Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	canaryBacking.Parent = &types.VirtualDiskFlatVer2BackingInfo{
		VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
			FileName: "[replica] retained.vmdk",
		},
	}
	if err := validateReplicaBuildCanaryBackings(retained, canary); err != nil {
		t.Fatal(err)
	}
	canaryBacking.Parent.FileName = "[other] retained-lookalike.vmdk"
	if err := validateReplicaBuildCanaryBackings(retained, canary); err == nil ||
		!strings.Contains(err.Error(), "not an exact retained replica backing") {
		t.Fatalf("wrong parent backing error=%v", err)
	}
	canaryBacking.Parent = nil
	if err := validateReplicaBuildCanaryBackings(retained, canary); err == nil ||
		!strings.Contains(err.Error(), "not linked") {
		t.Fatalf("missing parent backing error=%v", err)
	}
}
