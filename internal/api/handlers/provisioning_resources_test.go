package handlers

import "testing"

func TestValidateProvisioningRAMRejectsUnaccountedCapacity(t *testing.T) {
	t.Parallel()

	for _, ramMB := range []int{-1, 0, minimumProvisioningRAMMB - 1} {
		if err := validateProvisioningRAM(ramMB); err == nil {
			t.Fatalf("validateProvisioningRAM(%d) succeeded, want rejection", ramMB)
		}
	}
	if err := validateProvisioningRAM(minimumProvisioningRAMMB); err != nil {
		t.Fatalf("validateProvisioningRAM(%d): %v", minimumProvisioningRAMMB, err)
	}
}
