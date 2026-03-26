package runner

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

const (
	DefaultConfigPath = "/opt/crucible/runner-config.json"
	DefaultSocketPath = "/tmp/crucible-sidecar.sock"
	DefaultContextPath = "/tmp/crucible-context.json"
)

// LoadConfig reads and parses the runner configuration from a JSON file.
func LoadConfig(path string) (*RunnerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg RunnerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// Validate checks that all required fields are present.
func (c *RunnerConfig) Validate() error {
	if c.CallbackURL == "" {
		return fmt.Errorf("callback_url is required")
	}
	if c.CallbackToken == "" {
		return fmt.Errorf("callback_token is required")
	}
	if c.RunID == "" {
		return fmt.Errorf("run_id is required")
	}
	if len(c.Workflows) == 0 {
		return fmt.Errorf("at least one workflow is required")
	}
	for i, wf := range c.Workflows {
		if wf.Slug == "" {
			return fmt.Errorf("workflow[%d].slug is required", i)
		}
		if wf.Script == "" {
			return fmt.Errorf("workflow[%d].script is required", i)
		}
		if wf.TimeoutSeconds <= 0 {
			return fmt.Errorf("workflow[%d].timeout_seconds must be positive", i)
		}
	}
	if c.Target.IP == "" {
		return fmt.Errorf("target.ip is required")
	}
	return nil
}

// BuildEnv constructs the CRUCIBLE_* environment variables from the config.
// These are injected into every workflow bash process.
func (c *RunnerConfig) BuildEnv(workdir, socketPath, contextPath string) []string {
	configJSON, _ := json.Marshal(c)
	configB64 := base64.StdEncoding.EncodeToString(configJSON)

	env := []string{
		"CRUCIBLE_CONFIG_B64=" + configB64,
		"CRUCIBLE_TARGET_IP=" + c.Target.IP,
		"CRUCIBLE_TARGET_OS=" + c.Target.OS,
		"CRUCIBLE_TARGET_USERNAME=" + c.Target.Username,
		"CRUCIBLE_TARGET_PASSWORD=" + c.Target.Password,
		"CRUCIBLE_POD_SUBNET=" + c.Pod.Subnet,
		"CRUCIBLE_POD_INDEX=" + fmt.Sprintf("%d", c.Pod.Index),
		"CRUCIBLE_WORKDIR=" + workdir,
		"CRUCIBLE_SOCKET=" + socketPath,
		"CRUCIBLE_CONTEXT=" + contextPath,
	}

	// Inherit PATH and other essential vars from the container
	for _, key := range []string{"PATH", "HOME", "TERM", "LANG"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}

	return env
}
