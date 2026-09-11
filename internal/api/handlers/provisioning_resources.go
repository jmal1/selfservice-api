package handlers

import "fmt"

const minimumProvisioningRAMMB = 512

func validateProvisioningRAM(ramMB int) error {
	if ramMB < minimumProvisioningRAMMB {
		return fmt.Errorf("ram_mb must be at least %d", minimumProvisioningRAMMB)
	}
	return nil
}
