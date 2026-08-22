package main

import (
	"os"
	"strings"
	"testing"
)

func TestCleanupOnlyWorkerCannotStartCloneSchedulers(t *testing.T) {
	if cloneSchedulerEnabled(false, true) {
		t.Fatal("clone-capable scheduler enabled while provisioning claims are disabled")
	}
	if !cloneSchedulerEnabled(true, true) {
		t.Fatal("configured scheduler should run when provisioning claims are enabled")
	}
	if cloneSchedulerEnabled(true, false) {
		t.Fatal("disabled scheduler unexpectedly enabled")
	}
}

func TestWorkerResolvesHostAllowlistBeforeProvisionerConstruction(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	source := string(body)
	resolve := strings.Index(source, "vcClient.ResolveProvisioningHosts(ctx)")
	provisioner := strings.Index(source, "provisioner.New(")
	if resolve < 0 {
		t.Fatal("worker does not resolve VCENTER_HOSTS at startup")
	}
	if provisioner < 0 {
		t.Fatal("provisioner construction not found")
	}
	if resolve > provisioner {
		t.Fatal("worker constructs the provisioner before VCENTER_HOSTS is resolved")
	}
}
