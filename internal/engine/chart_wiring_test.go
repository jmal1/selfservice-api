package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helmPath resolves a file inside the deployed chart from this package.
func helmPath(parts ...string) string {
	base := []string{"..", "..", "deploy", "helm", "selfservice"}
	return filepath.Join(append(base, parts...)...)
}

// TestEngineChartWiresPushgateway guards runner observability at the point it
// actually broke.
//
// internal/engine/runner_metrics.go implements provisioning duration, job
// outcome, cleanup-failure and orphan metrics, and cmd/crucible-engine wires
// them — but only when ENGINE_PUSHGATEWAY_URL is set. The Helm chart never set
// it, so production logged "runner metrics disabled" on every start and not a
// single runner metric ever reached Prometheus. The code looked complete in
// review; only the deployment made it a no-op.
//
// Vault rule §9 requires new features to ship with monitoring, so treat a
// chart that cannot emit these metrics as a test failure.
func TestEngineChartWiresPushgateway(t *testing.T) {
	values, err := os.ReadFile(helmPath("values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	deployment, err := os.ReadFile(helmPath("templates", "engine-deployment.yaml"))
	if err != nil {
		t.Fatalf("read engine-deployment.yaml: %v", err)
	}

	v, d := string(values), string(deployment)

	if !strings.Contains(v, "pushgatewayURL:") {
		t.Error("values.yaml has no engine.pushgatewayURL; runner metrics will be disabled at runtime")
	}
	// An empty/commented-out default is the exact failure mode being guarded,
	// so require a real URL.
	if !strings.Contains(v, `pushgatewayURL: "http`) {
		t.Error("engine.pushgatewayURL must be set to a real URL, not blank — " +
			"a blank value silently disables every runner metric")
	}
	if !strings.Contains(d, "ENGINE_PUSHGATEWAY_URL") {
		t.Error("engine-deployment.yaml never sets ENGINE_PUSHGATEWAY_URL, so " +
			"cmd/crucible-engine takes the 'runner metrics disabled' branch")
	}
	if !strings.Contains(d, ".Values.engine.pushgatewayURL") {
		t.Error("engine-deployment.yaml must source ENGINE_PUSHGATEWAY_URL from " +
			".Values.engine.pushgatewayURL so it is configurable per environment")
	}
}

// TestEngineChartEnvMatchesCode keeps the chart and the binary's getEnv calls
// from drifting apart. Every RUNNER_*/ENGINE_* variable the engine reads should
// be settable from the chart; one that isn't is a feature that cannot be turned
// on in production.
func TestEngineChartEnvMatchesCode(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("..", "..", "cmd", "crucible-engine", "main.go"))
	if err != nil {
		t.Fatalf("read crucible-engine main.go: %v", err)
	}
	deployment, err := os.ReadFile(helmPath("templates", "engine-deployment.yaml"))
	if err != nil {
		t.Fatalf("read engine-deployment.yaml: %v", err)
	}
	d := string(deployment)

	// Variables the engine reads that are genuinely optional or supplied by
	// other means (downward API, secrets) rather than plain chart values.
	exempt := map[string]bool{
		"ENGINE_ID":        true, // defaults to pod name via downward API
		"ENGINE_NAMESPACE": true, // downward API
	}

	var missing []string
	for _, name := range envVarsRead(string(main)) {
		if exempt[name] || strings.Contains(d, name) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("cmd/crucible-engine reads env vars the Helm chart never sets, so they "+
			"cannot be configured in production: %s", strings.Join(missing, ", "))
	}
}

// envVarsRead extracts the names passed to getEnv("NAME", ...) / os.Getenv("NAME").
func envVarsRead(src string) []string {
	var out []string
	seen := map[string]bool{}
	for _, prefix := range []string{`getEnv("`, `os.Getenv("`} {
		rest := src
		for {
			i := strings.Index(rest, prefix)
			if i < 0 {
				break
			}
			rest = rest[i+len(prefix):]
			j := strings.IndexByte(rest, '"')
			if j < 0 {
				break
			}
			name := rest[:j]
			// Only ENGINE_/RUNNER_ vars; DB_/NATS_ are covered elsewhere in the chart.
			if (strings.HasPrefix(name, "ENGINE_") || strings.HasPrefix(name, "RUNNER_")) && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			rest = rest[j+1:]
		}
	}
	return out
}
