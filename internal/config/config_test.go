package config

import (
	"strings"
	"testing"
)

func TestProvisioningConfigDefaultsEnabled(t *testing.T) {
	t.Setenv("PROVISIONING_ENABLED", "")
	t.Setenv("WORKER_PROVISIONING_CLAIMS_ENABLED", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Provisioning.Enabled {
		t.Error("Provisioning.Enabled = false, want backward-compatible default true")
	}
	if !cfg.Provisioning.WorkerClaimsEnabled {
		t.Error("Provisioning.WorkerClaimsEnabled = false, want backward-compatible default true")
	}
}

func TestProvisioningConfigAcceptsExplicitFalse(t *testing.T) {
	t.Setenv("PROVISIONING_ENABLED", "false")
	t.Setenv("WORKER_PROVISIONING_CLAIMS_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Provisioning.Enabled || cfg.Provisioning.WorkerClaimsEnabled {
		t.Fatalf("Provisioning = %+v, want both controls disabled", cfg.Provisioning)
	}
}

func TestProvisioningConfigRejectsInvalidBoolean(t *testing.T) {
	for _, key := range []string{"PROVISIONING_ENABLED", "WORKER_PROVISIONING_CLAIMS_ENABLED"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("PROVISIONING_ENABLED", "true")
			t.Setenv("WORKER_PROVISIONING_CLAIMS_ENABLED", "true")
			t.Setenv(key, "definitely")

			_, err := Load()
			if err == nil {
				t.Fatal("Load() succeeded with invalid maintenance boolean")
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %q, want it to identify %s", err, key)
			}
		})
	}
}

func TestVCenterHostsRejectsEmptyAndDuplicateEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty list", value: ""},
		{name: "empty member", value: "esxi1.lab.jmal.io,,esxi2.lab.jmal.io"},
		{name: "duplicate", value: "esxi1.lab.jmal.io,ESXI1.lab.jmal.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VCENTER_HOSTS", tc.value)
			_, err := Load()
			if err == nil {
				t.Fatal("Load() succeeded with an invalid VCENTER_HOSTS allowlist")
			}
			if !strings.Contains(err.Error(), "VCENTER_HOSTS") {
				t.Fatalf("error = %q, want it to identify VCENTER_HOSTS", err)
			}
		})
	}
}

func TestVCenterHostsParsesCanonicalAllowlist(t *testing.T) {
	t.Setenv("VCENTER_HOSTS", " esxi1.lab.jmal.io , nuc1.lab.jmal.io ")
	t.Setenv("VCENTER_PLACEMENT_RESERVED_MEMORY_MB", "ESXI1.lab.jmal.io=8192,nuc1.lab.jmal.io=4096")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"esxi1.lab.jmal.io", "nuc1.lab.jmal.io"}
	if len(cfg.VCenter.Hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", cfg.VCenter.Hosts, want)
	}
	for i := range want {
		if cfg.VCenter.Hosts[i] != want[i] {
			t.Fatalf("hosts = %v, want %v", cfg.VCenter.Hosts, want)
		}
		if got := cfg.VCenter.HostReservedMemoryMB["esxi1.lab.jmal.io"]; got != 8192 {
			t.Fatalf("ESXi1 reserved memory = %d, want 8192", got)
		}
		if got := cfg.VCenter.HostReservedMemoryMB["nuc1.lab.jmal.io"]; got != 4096 {
			t.Fatalf("nuc1 reserved memory = %d, want 4096", got)
		}
	}
}

func TestVCenterPlacementReserveRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "unknown host", value: "esxi2.lab.jmal.io=4096"},
		{name: "negative reserve", value: "esxi1.lab.jmal.io=-1"},
		{name: "not integer", value: "esxi1.lab.jmal.io=many"},
		{name: "duplicate host", value: "esxi1.lab.jmal.io=1,ESXI1.lab.jmal.io=2"},
		{name: "malformed", value: "esxi1.lab.jmal.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VCENTER_HOSTS", "esxi1.lab.jmal.io")
			t.Setenv("VCENTER_PLACEMENT_RESERVED_MEMORY_MB", tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "VCENTER_PLACEMENT_RESERVED_MEMORY_MB") {
				t.Fatalf("Load() error = %v, want reserve configuration error", err)
			}
		})
	}
}
