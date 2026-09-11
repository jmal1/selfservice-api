package main

import (
	"os"
	"strings"
	"testing"
)

func TestVMwareToolsResolvesHostAllowlistBeforeDispatcher(t *testing.T) {
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}

	source := string(body)
	resolve := strings.Index(source, "vcClient.ResolveProvisioningHosts(ctx)")
	dispatcher := strings.Index(source, "engine.NewVMwareToolsDispatcher")
	if resolve < 0 {
		t.Fatal("crucible-engine does not resolve VCENTER_HOSTS before enabling vmware_tools")
	}
	if dispatcher < 0 {
		t.Fatal("vmware_tools dispatcher construction not found")
	}
	if resolve > dispatcher {
		t.Fatal("vmware_tools dispatcher is constructed before VCENTER_HOSTS is resolved")
	}
}
