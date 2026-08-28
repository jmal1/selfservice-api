package provisioning

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type chartValues struct {
	ReplicaCount struct {
		Worker int `yaml:"worker"`
	} `yaml:"replicaCount"`
	Provisioning struct {
		Enabled             bool `yaml:"enabled"`
		WorkerClaimsEnabled bool `yaml:"workerClaimsEnabled"`
	} `yaml:"provisioning"`
	Worker struct {
		ShutdownGracePeriodSeconds int `yaml:"shutdownGracePeriodSeconds"`
		OrphanReconciler           struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"orphanReconciler"`
		NetworkReconciler struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"networkReconciler"`
		L1Validation struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"l1Validation"`
		TemplateHealth struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"templateHealth"`
		IdleEvaluator struct {
			Enabled bool `yaml:"enabled"`
			DryRun  bool `yaml:"dryRun"`
		} `yaml:"idleEvaluator"`
		PipelineReconciler struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"pipelineReconciler"`
		ContentFilter struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"contentFilter"`
	} `yaml:"worker"`
	VCenter struct {
		Hosts                     string `yaml:"hosts"`
		ResourcePools             string `yaml:"resourcePools"`
		PlacementReservedMemoryMB string `yaml:"placementReservedMemoryMB"`
		Insecure                  string `yaml:"insecure"`
	} `yaml:"vcenter"`
	Synthetic struct {
		Suspend                     bool `yaml:"suspend"`
		ProvisioningExpectedEnabled bool `yaml:"provisioningExpectedEnabled"`
		Lifecycle                   struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"lifecycle"`
		Janitor struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"janitor"`
		Runner struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"runner"`
	} `yaml:"synthetic"`
}

func loadChartValues(t *testing.T, name string) chartValues {
	t.Helper()
	path := filepath.Join("..", "..", "deploy", "helm", "selfservice", name)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var values chartValues
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return values
}

func chartScalar(t *testing.T, name string, path ...string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "deploy", "helm", "selfservice", name))
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	node := doc.Content[0]
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			t.Fatalf("%s path %v reached non-mapping node", name, path)
		}
		var next *yaml.Node
		for i := 0; i < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				next = node.Content[i+1]
				break
			}
		}
		if next == nil {
			t.Fatalf("%s is missing explicit value at %v", name, path)
		}
		node = next
	}
	return node.Value
}

func TestChartProvisioningDefaultsRemainCompatible(t *testing.T) {
	values := loadChartValues(t, "values.yaml")
	if !values.Provisioning.Enabled || !values.Provisioning.WorkerClaimsEnabled {
		t.Fatalf("default provisioning controls = %+v, want both true", values.Provisioning)
	}
	if !values.Synthetic.ProvisioningExpectedEnabled {
		t.Fatal("default synthetic must expect provisioning enabled")
	}
	if values.Worker.ShutdownGracePeriodSeconds < 120 {
		t.Fatalf(
			"worker shutdown grace = %d seconds, want at least the two-minute durable handoff window",
			values.Worker.ShutdownGracePeriodSeconds,
		)
	}
	for path, want := range map[string]string{
		"provisioning.enabled":                  "true",
		"provisioning.workerClaimsEnabled":      "true",
		"synthetic.provisioningExpectedEnabled": "true",
	} {
		parts := strings.Split(path, ".")
		if got := chartScalar(t, "values.yaml", parts...); got != want {
			t.Errorf("%s = %q, want explicit %q", path, got, want)
		}
	}
}

func TestProductionProvisioningKeepsClaimsGuardedAndFeedbackEnabled(t *testing.T) {
	values := loadChartValues(t, "values.prod.yaml")
	if values.ReplicaCount.Worker != 1 {
		t.Errorf("production worker replicas = %d, want exactly 1 while ESXi2 remains quarantined", values.ReplicaCount.Worker)
	}
	if !values.Provisioning.Enabled || values.Provisioning.WorkerClaimsEnabled {
		t.Fatalf("production provisioning controls = %+v, want admission on and chart-rendered worker claims gated", values.Provisioning)
	}
	if !values.Synthetic.ProvisioningExpectedEnabled {
		t.Fatal("production synthetic must expect provisioning enabled")
	}
	if values.Synthetic.Suspend {
		t.Fatal("production non-mutating API monitor CronJob must remain active")
	}
	if !values.Synthetic.Lifecycle.Enabled || !values.Synthetic.Runner.Enabled || !values.Synthetic.Janitor.Enabled {
		t.Fatalf("production mutating synthetics must remain enabled after the live provisioning proof: %+v", values.Synthetic)
	}
	if values.VCenter.Hosts != "esxi1.lab.jmal.io" {
		t.Fatalf("production VCENTER_HOSTS = %q, want ESXi1 only", values.VCenter.Hosts)
	}
	if values.VCenter.ResourcePools != "/JMAL-Datacenter/host/AMD-Cluster/Resources/Student-VMs" {
		t.Fatalf("production resource pools = %q, want ESXi1-compatible AMD pool only", values.VCenter.ResourcePools)
	}
	if values.VCenter.Insecure != "false" {
		t.Fatalf("production vCenter insecure = %q, want strict TLS", values.VCenter.Insecure)
	}
	if values.VCenter.PlacementReservedMemoryMB != "esxi1.lab.jmal.io=8192" {
		t.Fatalf("production placement reserve = %q, want ESXi1 8 GiB", values.VCenter.PlacementReservedMemoryMB)
	}
	if values.Worker.OrphanReconciler.Enabled ||
		values.Worker.NetworkReconciler.Enabled ||
		!values.Worker.L1Validation.Enabled ||
		values.Worker.TemplateHealth.Enabled ||
		values.Worker.IdleEvaluator.Enabled ||
		values.Worker.PipelineReconciler.Enabled ||
		!values.Worker.IdleEvaluator.DryRun {
		t.Fatalf("production worker background controls are not in the expected guarded-feedback state: %+v", values.Worker)
	}
	for path, want := range map[string]string{
		"replicaCount.worker":                   "1",
		"provisioning.enabled":                  "true",
		"provisioning.workerClaimsEnabled":      "false",
		"synthetic.provisioningExpectedEnabled": "true",
		"synthetic.lifecycle.enabled":           "true",
		"synthetic.janitor.enabled":             "true",
		"synthetic.runner.enabled":              "true",
		"vcenter.hosts":                         "esxi1.lab.jmal.io",
		"vcenter.resourcePools":                 "/JMAL-Datacenter/host/AMD-Cluster/Resources/Student-VMs",
		"vcenter.insecure":                      "false",
		"vcenter.placementReservedMemoryMB":     "esxi1.lab.jmal.io=8192",
		"worker.orphanReconciler.enabled":       "false",
		"worker.networkReconciler.enabled":      "false",
		"worker.l1Validation.enabled":           "true",
		"worker.templateHealth.enabled":         "false",
		"worker.idleEvaluator.enabled":          "false",
		"worker.idleEvaluator.dryRun":           "true",
		"worker.pipelineReconciler.enabled":     "false",
	} {
		parts := strings.Split(path, ".")
		if got := chartScalar(t, "values.prod.yaml", parts...); got != want {
			t.Errorf("%s = %q, want explicit %q", path, got, want)
		}
	}
}

func TestFullFleetOverlayRendersApprovedFinalState(t *testing.T) {
	values := loadChartValues(t, "values.full-fleet.yaml")
	if values.ReplicaCount.Worker != 4 {
		t.Fatalf("full-fleet worker replicas = %d, want 4", values.ReplicaCount.Worker)
	}
	if !values.Provisioning.Enabled || !values.Provisioning.WorkerClaimsEnabled {
		t.Fatalf("full-fleet provisioning controls = %+v, want enabled", values.Provisioning)
	}
	if !values.Synthetic.ProvisioningExpectedEnabled ||
		!values.Synthetic.Lifecycle.Enabled ||
		!values.Synthetic.Janitor.Enabled ||
		!values.Synthetic.Runner.Enabled {
		t.Fatalf("full-fleet synthetics are not fully enabled: %+v", values.Synthetic)
	}
	if !values.Worker.L1Validation.Enabled ||
		!values.Worker.TemplateHealth.Enabled ||
		!values.Worker.OrphanReconciler.Enabled ||
		!values.Worker.NetworkReconciler.Enabled ||
		!values.Worker.IdleEvaluator.Enabled ||
		values.Worker.IdleEvaluator.DryRun ||
		!values.Worker.PipelineReconciler.Enabled ||
		values.Worker.ContentFilter.Enabled {
		t.Fatalf("full-fleet producer controls are not approved: %+v", values.Worker)
	}
	for path, want := range map[string]string{
		"replicaCount.worker":               "4",
		"provisioning.enabled":              "true",
		"provisioning.workerClaimsEnabled":  "true",
		"vcenter.insecure":                  "false",
		"vcenter.hosts":                     "esxi1.lab.jmal.io,esxi2.lab.jmal.io,nuc1.lab.jmal.io,nuc2.lab.jmal.io,nuc3.lab.jmal.io",
		"vcenter.resourcePools":             "/JMAL-Datacenter/host/AMD-Cluster/Resources/Student-VMs,/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs",
		"vcenter.placementReservedMemoryMB": "esxi1.lab.jmal.io=8192,esxi2.lab.jmal.io=8192,nuc1.lab.jmal.io=4096,nuc2.lab.jmal.io=2048,nuc3.lab.jmal.io=2048",
		"synthetic.lifecycle.enabled":       "true",
		"synthetic.janitor.enabled":         "true",
		"synthetic.runner.enabled":          "true",
		"worker.l1Validation.enabled":       "true",
		"worker.templateHealth.enabled":     "true",
		"worker.orphanReconciler.enabled":   "true",
		"worker.networkReconciler.enabled":  "true",
		"worker.idleEvaluator.enabled":      "true",
		"worker.idleEvaluator.dryRun":       "false",
		"worker.pipelineReconciler.enabled": "true",
		"worker.contentFilter.enabled":      "false",
	} {
		parts := strings.Split(path, ".")
		if got := chartScalar(t, "values.full-fleet.yaml", parts...); got != want {
			t.Errorf("%s = %q, want explicit %q", path, got, want)
		}
	}
}

func TestChartWiresEveryProvisioningControl(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "helm", "selfservice", "templates")
	files := map[string][]string{
		"api-deployment.yaml": {
			"PROVISIONING_ENABLED",
			"PROVISIONING_PUSHGATEWAY_URL",
			"PROVISIONING_PUSHGATEWAY_JOB",
			"VCENTER_DATASTORE",
			"VCENTER_RESOURCE_POOLS",
			"VCENTER_HOSTS",
			"VCENTER_INSECURE",
			".Values.vcenter.datastore",
			".Values.vcenter.resourcePools",
			".Values.vcenter.hosts",
			".Values.vcenter.insecure",
			".Values.provisioning.enabled",
		},
		"worker-deployment.yaml": {
			"WORKER_PROVISIONING_CLAIMS_ENABLED",
			".Values.provisioning.workerClaimsEnabled",
			"VCENTER_INSECURE",
			".Values.vcenter.insecure",
			"OPNSENSE_SSH_HOST_KEY",
			"opnsense-ssh-host-key",
			".Values.opnsense.sshHostKey",
			"terminationGracePeriodSeconds: {{ .Values.worker.shutdownGracePeriodSeconds }}",
		},
		"engine-deployment.yaml": {
			"VCENTER_URL",
			"VCENTER_USER",
			"VCENTER_PASSWORD",
			"VCENTER_DATACENTER",
			"VCENTER_DATASTORE",
			"VCENTER_RESOURCE_POOLS",
			"VCENTER_HOSTS",
			"VCENTER_INSECURE",
			".Values.vcenter.datacenter",
			".Values.vcenter.datastore",
			".Values.vcenter.resourcePools",
			".Values.vcenter.hosts",
			".Values.vcenter.insecure",
		},
		"synthetic-cronjob.yaml": {
			"SYNTHETIC_PROVISIONING_EXPECTED_ENABLED",
			".Values.synthetic.provisioningExpectedEnabled",
			"SYNTHETIC_RUNNER_EXPECTED_ENABLED",
			".Values.synthetic.runner.enabled",
		},
	}
	for name, fragments := range files {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(body), fragment) {
				t.Errorf("%s does not wire %q", name, fragment)
			}
		}
	}
}

// TestSyntheticCronJobLifecycleEnabledAlwaysRendered is a structural guard
// against the exact 2026-08-25 dry-run failure: deploy.sh's
// validate_foundation_intent reads SYNTHETIC_LIFECYCLE_ENABLED via
// env_from_manifest, which requires EXACTLY ONE match in the rendered
// manifest. Wrapping the key in the `synthetic.lifecycle.enabled`
// conditional -- as it originally was -- makes it disappear entirely from a
// disabled (production) render, so env_from_manifest returns empty and the
// safety check fails closed with a misleading error. This test parses the
// raw template (no `helm` binary required, so it always runs in `go test
// ./...`) and fails if the key is removed, duplicated, hardcoded, or ever
// nested back inside that conditional. Real render assertions (exactly one
// explicit "false"/"true" per overlay) are enforced in
// .github/workflows/helm-lint.yaml and TestSyntheticCronJobLifecycleEnabledRendersAcrossOverlays.
func TestSyntheticCronJobLifecycleEnabledAlwaysRendered(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "helm", "selfservice", "templates", "synthetic-cronjob.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	const enabledLine = "- name: SYNTHETIC_LIFECYCLE_ENABLED"
	if n := strings.Count(src, enabledLine); n != 1 {
		t.Fatalf("synthetic-cronjob.yaml declares SYNTHETIC_LIFECYCLE_ENABLED %d times, want exactly 1", n)
	}
	enabledIdx := strings.Index(src, enabledLine)

	// Must stay wired to the toggle as an explicit quoted boolean, never a
	// hardcoded literal -- a hardcoded value would stop tracking
	// .Values.synthetic.lifecycle.enabled while still passing a naive
	// presence check.
	const valueExpr = "value: {{ .Values.synthetic.lifecycle.enabled | quote }}"
	if !strings.Contains(src, valueExpr) {
		t.Fatal("synthetic-cronjob.yaml does not render SYNTHETIC_LIFECYCLE_ENABLED from .Values.synthetic.lifecycle.enabled")
	}

	const ifGuard = "{{- if .Values.synthetic.lifecycle.enabled }}"
	ifIdx := strings.Index(src, ifGuard)
	if ifIdx == -1 {
		t.Fatal("synthetic-cronjob.yaml lost its lifecycle-enabled conditional guard")
	}
	// The matching {{- end }} for the lifecycle guard is the first {{- end }}
	// after the if; no other if/end pairs are nested between them today.
	const endGuard = "{{- end }}"
	relEndIdx := strings.Index(src[ifIdx:], endGuard)
	if relEndIdx == -1 {
		t.Fatal("synthetic-cronjob.yaml lifecycle-enabled conditional is never closed")
	}
	endIdx := ifIdx + relEndIdx

	// SYNTHETIC_LIFECYCLE_ENABLED must be declared OUTSIDE the
	// lifecycle-enabled conditional -- this is the exact regression this
	// test guards against: wrapping it back inside the conditional makes it
	// vanish from a disabled render.
	if enabledIdx > ifIdx && enabledIdx < endIdx {
		t.Fatal("SYNTHETIC_LIFECYCLE_ENABLED must not be nested inside the lifecycle-enabled conditional -- it must always render")
	}

	// The remaining lifecycle-only fields must stay gated: they are only
	// meaningful (and only have safe defaults) when lifecycle checks run.
	for _, name := range []string{
		"SYNTHETIC_LIFECYCLE_TEMPLATE",
		"SYNTHETIC_LIFECYCLE_READY_TIMEOUT",
		"SYNTHETIC_LIFECYCLE_DESTROY_TIMEOUT",
		"SYNTHETIC_LIFECYCLE_MAX_ATTEMPTS",
		"SYNTHETIC_LIFECYCLE_RETRY_BACKOFF",
	} {
		idx := strings.Index(src, "- name: "+name)
		if idx == -1 {
			t.Fatalf("synthetic-cronjob.yaml is missing %s", name)
		}
		if idx < ifIdx || idx > endIdx {
			t.Fatalf("%s must remain gated by the lifecycle-enabled conditional", name)
		}
	}
}

// TestSyntheticCronJobLifecycleEnabledRendersAcrossOverlays performs a real
// `helm template` render (the same tool deploy.sh's validate_foundation_intent
// ultimately consumes) and asserts SYNTHETIC_LIFECYCLE_ENABLED appears
// EXACTLY ONCE with the expected explicit value for both the production
// overlay (disabled) and the full-fleet overlay (enabled). It requires the
// `helm` binary and the chart's vendored dependencies (`helm dependency
// build`, run once with network access); it skips cleanly when either is
// unavailable rather than failing an offline `go test ./...` run. CI enforces
// this unconditionally via .github/workflows/helm-lint.yaml.
func TestSyntheticCronJobLifecycleEnabledRendersAcrossOverlays(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not available")
	}
	chartDir := filepath.Join("..", "..", "deploy", "helm", "selfservice")
	if _, err := os.Stat(filepath.Join(chartDir, "charts")); err != nil {
		t.Skip("chart dependencies not vendored; run `helm dependency build` in deploy/helm/selfservice first")
	}

	render := func(overlay string) string {
		t.Helper()
		args := []string{"template", "selfservice", ".", "-f", "values.yaml"}
		if overlay != "" {
			args = append(args, "-f", overlay)
		}
		args = append(args, "--show-only", "templates/synthetic-cronjob.yaml")
		cmd := exec.Command(helmPath, args...)
		cmd.Dir = chartDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm template (overlay=%q) failed: %v\n%s", overlay, err, out)
		}
		return string(out)
	}

	assertExactlyOne := func(t *testing.T, rendered, overlay, wantValue string) {
		t.Helper()
		const nameLine = "- name: SYNTHETIC_LIFECYCLE_ENABLED"
		if n := strings.Count(rendered, nameLine); n != 1 {
			t.Fatalf("overlay %q rendered SYNTHETIC_LIFECYCLE_ENABLED %d times, want exactly 1:\n%s", overlay, n, rendered)
		}
		idx := strings.Index(rendered, nameLine)
		line := rendered[idx:]
		if nl := strings.IndexByte(line, '\n'); nl != -1 {
			line = line[:nl]
		}
		valueLineStart := idx + len(line) + 1
		rest := rendered[valueLineStart:]
		if nl := strings.IndexByte(rest, '\n'); nl != -1 {
			rest = rest[:nl]
		}
		wantLine := `value: "` + wantValue + `"`
		if !strings.Contains(rest, wantLine) {
			t.Fatalf("overlay %q rendered SYNTHETIC_LIFECYCLE_ENABLED with %q, want %q", overlay, strings.TrimSpace(rest), wantLine)
		}
	}

	assertExactlyOne(t, render("values.prod.yaml"), "values.prod.yaml", "false")
	assertExactlyOne(t, render("values.full-fleet.yaml"), "values.full-fleet.yaml", "true")
}

func TestSyntheticCronJobRunnerExpectedEnabledRendersAcrossOverlays(t *testing.T) {
	helmPath, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not available")
	}
	chartDir := filepath.Join("..", "..", "deploy", "helm", "selfservice")
	if _, err := os.Stat(filepath.Join(chartDir, "charts")); err != nil {
		t.Skip("chart dependencies not vendored; run `helm dependency build` in deploy/helm/selfservice first")
	}

	render := func(overlay string) string {
		t.Helper()
		args := []string{"template", "selfservice", ".", "-f", "values.yaml"}
		if overlay != "" {
			args = append(args, "-f", overlay)
		}
		args = append(args, "--show-only", "templates/synthetic-cronjob.yaml")
		cmd := exec.Command(helmPath, args...)
		cmd.Dir = chartDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helm template (overlay=%q) failed: %v\n%s", overlay, err, out)
		}
		return string(out)
	}

	assertExactlyOne := func(t *testing.T, rendered, overlay, wantValue string) {
		t.Helper()
		const nameLine = "- name: SYNTHETIC_RUNNER_EXPECTED_ENABLED"
		if n := strings.Count(rendered, nameLine); n != 1 {
			t.Fatalf("overlay %q rendered SYNTHETIC_RUNNER_EXPECTED_ENABLED %d times, want exactly 1:\n%s", overlay, n, rendered)
		}
		idx := strings.Index(rendered, nameLine)
		line := rendered[idx:]
		if nl := strings.IndexByte(line, '\n'); nl != -1 {
			line = line[:nl]
		}
		valueLineStart := idx + len(line) + 1
		rest := rendered[valueLineStart:]
		if nl := strings.IndexByte(rest, '\n'); nl != -1 {
			rest = rest[:nl]
		}
		wantLine := `value: "` + wantValue + `"`
		if !strings.Contains(rest, wantLine) {
			t.Fatalf("overlay %q rendered SYNTHETIC_RUNNER_EXPECTED_ENABLED with %q, want %q", overlay, strings.TrimSpace(rest), wantLine)
		}
	}

	assertExactlyOne(t, render("values.prod.yaml"), "values.prod.yaml", "false")
	assertExactlyOne(t, render("values.full-fleet.yaml"), "values.full-fleet.yaml", "true")
}
