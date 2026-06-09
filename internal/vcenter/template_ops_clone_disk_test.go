package vcenter

import (
	"testing"

	"github.com/vmware/govmomi/vim25/types"
)

// TestChooseTemplateCloneDiskMoveType covers the full matrix of source
// shapes the template wizard might hand us. The decision is a one-liner
// but it's also the source of *two* "virtual disk is either corrupted
// or not a supported format" outages now, so it deserves its own test.
func TestChooseTemplateCloneDiskMoveType(t *testing.T) {
	consolidate := string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndConsolidate)

	cases := []struct {
		name              string
		hasSnapshot       bool
		isVCenterTemplate bool
		want              string
	}{
		{
			// Round-2 fix path: a plain Crucible template that has
			// already been used to spawn a student pod, so it carries
			// the auto-created `linked-clone-base` snapshot.
			name:              "vm_with_snapshot",
			hasSnapshot:       true,
			isVCenterTemplate: false,
			want:              consolidate,
		},
		{
			// Round-6 regression path: an unused source — either a
			// vCenter-marked Template, or a regular VM that nobody
			// has spawned a student pod from yet. Both must get the
			// empty DiskMoveType.
			name:              "vcenter_template_no_snapshot",
			hasSnapshot:       false,
			isVCenterTemplate: true,
			want:              "",
		},
		{
			name:              "plain_vm_no_snapshot",
			hasSnapshot:       false,
			isVCenterTemplate: false,
			want:              "",
		},
		{
			// Templates can't actually own snapshots in vCenter, but
			// if upstream ever lies we should still pick the safe
			// (empty) value rather than the consolidating one — the
			// consolidating call is what blew up against
			// student-windows-11.
			name:              "template_with_phantom_snapshot",
			hasSnapshot:       true,
			isVCenterTemplate: true,
			want:              "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseTemplateCloneDiskMoveType(tc.hasSnapshot, tc.isVCenterTemplate)
			if got != tc.want {
				t.Errorf("chooseTemplateCloneDiskMoveType(hasSnapshot=%v, isVCenterTemplate=%v) = %q, want %q",
					tc.hasSnapshot, tc.isVCenterTemplate, got, tc.want)
			}
		})
	}
}
