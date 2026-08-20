package provisioner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTemplateHealthConfirmationTimingIsPinnedInHelm guards the deployed
// scheduler contract rather than only the Go defaults.
func TestTemplateHealthConfirmationTimingIsPinnedInHelm(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "helm", "selfservice")
	values, err := os.ReadFile(filepath.Join(root, "values.prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := os.ReadFile(filepath.Join(root, "templates", "worker-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`confirmationBackoff: "5m"`, `confirmationReconcileInterval: "1m"`} {
		if !strings.Contains(string(values), fragment) {
			t.Errorf("production values do not pin %q", fragment)
		}
	}
	for _, env := range []string{
		"WORKER_TEMPLATE_HEALTH_CONFIRMATION_BACKOFF",
		"WORKER_TEMPLATE_HEALTH_CONFIRMATION_RECONCILE_INTERVAL",
	} {
		if !strings.Contains(string(deployment), env) {
			t.Errorf("worker deployment does not wire %s", env)
		}
	}
}
