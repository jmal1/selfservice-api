package provisioner

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

func validateDestroyHostIsolation(source string) error {
	vmFailure := strings.Index(source, `errors = append(errors, fmt.Errorf("destroy VM %s: %w", *vm.VCenterVMID, err))`)
	if vmFailure < 0 {
		return fmt.Errorf("destroy path has no VM failure branch")
	}
	vmContinue := strings.Index(source[vmFailure:], "continue")
	vmDeleted := strings.Index(source[vmFailure:], `UpdatePodVMStatus(ctx, vm.ID, "deleted")`)
	if vmContinue < 0 || vmDeleted < 0 || vmContinue > vmDeleted {
		return fmt.Errorf("failed or disallowed VM destruction can be marked deleted")
	}

	deleteStart := strings.Index(source, "WithVCenterPortGroupMutationLock")
	deleteEnd := strings.Index(source[deleteStart:], "// --- Step 4")
	releaseVLAN := strings.Index(source, "ReleaseVLAN")
	if deleteStart < 0 || deleteEnd < 0 || releaseVLAN < 0 || deleteStart > releaseVLAN {
		return fmt.Errorf("portgroup cleanup does not precede VLAN release")
	}
	deleteBlock := source[deleteStart : deleteStart+deleteEnd]
	if !strings.Contains(deleteBlock, "return p.failPodDestroy(ctx, pod.ID, errors)") {
		return fmt.Errorf("portgroup deletion failure can fall through to VLAN release")
	}
	if !strings.Contains(deleteBlock, "DeletePortGroupMutation") {
		return fmt.Errorf("destroy path does not use the durable portgroup receipt")
	}
	return nil
}

func TestDestroyRetainsDisallowedVMAndNetworkStateOnCleanupFailure(t *testing.T) {
	body, err := os.ReadFile("destroy.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDestroyHostIsolation(string(body)); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyIsolationGuardDetectsFallthrough(t *testing.T) {
	body, err := os.ReadFile("destroy.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	deleteStart := strings.Index(source, "WithVCenterPortGroupMutationLock")
	sabotagedTail := strings.Replace(
		source[deleteStart:],
		"return p.failPodDestroy(ctx, pod.ID, errors)",
		"_ = p.failPodDestroy(ctx, pod.ID, errors)",
		1,
	)
	sabotaged := source[:deleteStart] + sabotagedTail
	if err := validateDestroyHostIsolation(sabotaged); err == nil {
		t.Fatal("destroy isolation guard accepted portgroup cleanup fallthrough")
	}
}

func TestUnsafePortGroupReceiptsRequireManualCleanup(t *testing.T) {
	for _, err := range []error{
		database.ErrPortGroupReceiptNotFound,
		vcenter.ErrInvalidPortGroupReceipt,
		vcenter.ErrLegacyPortGroupReceipt,
		vcenter.ErrHostNotAllowed,
		vcenter.ErrAmbiguousHostIdentity,
		fmt.Errorf("wrapped: %w", vcenter.ErrHostNotAllowed),
	} {
		if !portGroupReceiptRequiresManualCleanup(err) {
			t.Errorf("error %q did not require manual cleanup", err)
		}
	}
	if portGroupReceiptRequiresManualCleanup(errors.New("temporary database outage")) {
		t.Fatal("transient receipt load error incorrectly requires manual cleanup")
	}
}
