package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigLoad(t *testing.T) {
	cfg := RunnerConfig{
		CallbackURL:   "http://crucible-engine:8080",
		CallbackToken: "test-token-abc123",
		RunID:         "run-001",
		Workflows: []WorkflowDef{
			{
				Slug:           "test-workflow",
				Name:           "Test Workflow",
				Script:         "#!/bin/bash\necho hello",
				TimeoutSeconds: 60,
			},
		},
		Target: TargetConfig{
			IP:       "10.100.5.10",
			OS:       "linux",
			Username: "student",
			Password: "password123",
		},
		Pod: PodConfig{
			Subnet: "10.100.5.0/24",
			Index:  1,
		},
	}

	// Write config to temp file
	dir := t.TempDir()
	path := filepath.Join(dir, "runner-config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Load and verify
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if loaded.CallbackURL != cfg.CallbackURL {
		t.Errorf("CallbackURL = %q, want %q", loaded.CallbackURL, cfg.CallbackURL)
	}
	if loaded.CallbackToken != cfg.CallbackToken {
		t.Errorf("CallbackToken = %q, want %q", loaded.CallbackToken, cfg.CallbackToken)
	}
	if loaded.RunID != cfg.RunID {
		t.Errorf("RunID = %q, want %q", loaded.RunID, cfg.RunID)
	}
	if len(loaded.Workflows) != 1 {
		t.Fatalf("len(Workflows) = %d, want 1", len(loaded.Workflows))
	}
	if loaded.Workflows[0].Slug != "test-workflow" {
		t.Errorf("Workflow.Slug = %q, want %q", loaded.Workflows[0].Slug, "test-workflow")
	}
	if loaded.Target.IP != "10.100.5.10" {
		t.Errorf("Target.IP = %q, want %q", loaded.Target.IP, "10.100.5.10")
	}
}

func TestConfigLoad_MissingFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestConfigLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("not json"), 0644)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestConfigValidate_MissingCallbackURL(t *testing.T) {
	cfg := &RunnerConfig{
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows:     []WorkflowDef{{Slug: "s", Script: "echo", TimeoutSeconds: 30}},
		Target:        TargetConfig{IP: "10.0.0.1"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing callback_url")
	}
}

func TestConfigValidate_NoWorkflows(t *testing.T) {
	cfg := &RunnerConfig{
		CallbackURL:   "http://engine",
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows:     []WorkflowDef{},
		Target:        TargetConfig{IP: "10.0.0.1"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for empty workflows")
	}
}

func TestConfigValidate_WorkflowMissingScript(t *testing.T) {
	cfg := &RunnerConfig{
		CallbackURL:   "http://engine",
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows:     []WorkflowDef{{Slug: "s", Script: "", TimeoutSeconds: 30}},
		Target:        TargetConfig{IP: "10.0.0.1"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for empty script")
	}
}

func TestConfigBuildEnv(t *testing.T) {
	cfg := &RunnerConfig{
		CallbackURL:   "http://engine",
		CallbackToken: "tok",
		RunID:         "run-1",
		Workflows:     []WorkflowDef{{Slug: "s", Script: "echo", TimeoutSeconds: 30}},
		Target: TargetConfig{
			IP:       "10.100.5.10",
			OS:       "linux",
			Username: "student",
			Password: "pass",
		},
		Pod: PodConfig{
			Subnet: "10.100.5.0/24",
			Index:  3,
		},
	}

	env := cfg.BuildEnv("/tmp/workdir", "/tmp/sock", "/tmp/ctx.json")

	envMap := make(map[string]string)
	for _, e := range env {
		parts := splitFirst(e, "=")
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	checks := map[string]string{
		"CRUCIBLE_TARGET_IP":       "10.100.5.10",
		"CRUCIBLE_TARGET_OS":       "linux",
		"CRUCIBLE_TARGET_USERNAME": "student",
		"CRUCIBLE_TARGET_PASSWORD": "pass",
		"CRUCIBLE_POD_SUBNET":      "10.100.5.0/24",
		"CRUCIBLE_POD_INDEX":       "3",
		"CRUCIBLE_WORKDIR":         "/tmp/workdir",
		"CRUCIBLE_SOCKET":          "/tmp/sock",
		"CRUCIBLE_CONTEXT":         "/tmp/ctx.json",
	}

	for key, want := range checks {
		if got, ok := envMap[key]; !ok {
			t.Errorf("missing env var %s", key)
		} else if got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Verify CRUCIBLE_CONFIG_B64 is present and decodable
	if _, ok := envMap["CRUCIBLE_CONFIG_B64"]; !ok {
		t.Error("missing CRUCIBLE_CONFIG_B64")
	}
}

func splitFirst(s, sep string) []string {
	for i := 0; i < len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
	}
	return []string{s}
}
