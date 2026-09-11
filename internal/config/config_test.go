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

func TestFailClosedTopologyDefaults(t *testing.T) {
	t.Setenv("VCENTER_URL", "")
	t.Setenv("VCENTER_HOSTS", "")
	t.Setenv("VCENTER_DATACENTER", "")
	t.Setenv("VCENTER_DATASTORE", "")
	t.Setenv("VCENTER_VM_FOLDER", "")
	t.Setenv("VCENTER_TEMPLATES_FOLDER", "")
	t.Setenv("VCENTER_RESOURCE_POOLS", "")
	t.Setenv("OPNSENSE_URL", "")
	t.Setenv("OPNSENSE_SSH_HOST", "")
	t.Setenv("OIDC_POST_LOGOUT_REDIRECT_URI", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.VCenter.URL != "" {
		t.Fatalf("VCenter.URL = %q, want empty fail-closed default", cfg.VCenter.URL)
	}
	if len(cfg.VCenter.Hosts) != 0 {
		t.Fatalf("VCenter.Hosts = %v, want empty fail-closed default", cfg.VCenter.Hosts)
	}
	if cfg.VCenter.Datacenter != "" || cfg.VCenter.Datastore != "" || cfg.VCenter.VMFolder != "" || cfg.VCenter.TemplatesFolder != "" {
		t.Fatalf("vCenter inventory defaults not empty: %+v", cfg.VCenter)
	}
	if len(cfg.VCenter.ResourcePools) != 0 {
		t.Fatalf("ResourcePools = %v, want empty", cfg.VCenter.ResourcePools)
	}
	if cfg.OPNsense.BaseURL != "" || cfg.OPNsense.SSHHost != "" {
		t.Fatalf("OPNsense defaults not empty: %+v", cfg.OPNsense)
	}
	if cfg.OIDC.PostLogoutRedirectURI != "https://crucible.example.test/login" {
		t.Fatalf("PostLogoutRedirectURI = %q", cfg.OIDC.PostLogoutRedirectURI)
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

func TestOPNsenseSSHHostKeyLoadsFromEnvironment(t *testing.T) {
	t.Setenv("OPNSENSE_SSH_HOST_KEY", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OPNsense.SSHHostKey != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest" {
		t.Fatalf("OPNsense.SSHHostKey = %q", cfg.OPNsense.SSHHostKey)
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

func TestVCenterHostsRejectsEmptyMemberAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "empty member", value: "esxi1.example.test,,esxi2.example.test"},
		{name: "duplicate", value: "esxi1.example.test,ESXI1.example.test"},
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
	t.Setenv("VCENTER_HOSTS", " esxi1.example.test , host3.example.test ")
	t.Setenv("VCENTER_PLACEMENT_RESERVED_MEMORY_MB", "ESXI1.example.test=8192,host3.example.test=4096")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"esxi1.example.test", "host3.example.test"}
	if len(cfg.VCenter.Hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", cfg.VCenter.Hosts, want)
	}
	for i := range want {
		if cfg.VCenter.Hosts[i] != want[i] {
			t.Fatalf("hosts = %v, want %v", cfg.VCenter.Hosts, want)
		}
	}
	if got := cfg.VCenter.HostReservedMemoryMB["esxi1.example.test"]; got != 8192 {
		t.Fatalf("esxi1 reserved memory = %d, want 8192", got)
	}
	if got := cfg.VCenter.HostReservedMemoryMB["host3.example.test"]; got != 4096 {
		t.Fatalf("host3 reserved memory = %d, want 4096", got)
	}
}

func TestVCenterPlacementReserveRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "unknown host", value: "esxi2.example.test=4096"},
		{name: "negative reserve", value: "esxi1.example.test=-1"},
		{name: "not integer", value: "esxi1.example.test=many"},
		{name: "duplicate host", value: "esxi1.example.test=1,ESXI1.example.test=2"},
		{name: "malformed", value: "esxi1.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("VCENTER_HOSTS", "esxi1.example.test")
			t.Setenv("VCENTER_PLACEMENT_RESERVED_MEMORY_MB", tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "VCENTER_PLACEMENT_RESERVED_MEMORY_MB") {
				t.Fatalf("Load() error = %v, want reserve configuration error", err)
			}
		})
	}
}
