package provisioner

import (
	"errors"
	"fmt"
	"testing"
)

func TestIgnoreMissingVMPlacementForPowerOff(t *testing.T) {
	notFound := fmt.Errorf("read placement: ManagedObjectNotFound: the object could not be found")
	if !ignoreMissingVMPlacementForPowerOff("stop", notFound) {
		t.Fatal("stop should preserve missing-VM idempotency")
	}
	for _, action := range []string{"start", "restart", "reset"} {
		if ignoreMissingVMPlacementForPowerOff(action, notFound) {
			t.Fatalf("%s unexpectedly ignored missing VM", action)
		}
	}
	if ignoreMissingVMPlacementForPowerOff("stop", errors.New("host placement drift")) {
		t.Fatal("stop ignored a genuine placement error")
	}
}
