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
