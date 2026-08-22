package provisioning

import (
	"os"
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
	} `yaml:"worker"`
	VCenter struct {
		Hosts         string `yaml:"hosts"`
		ResourcePools string `yaml:"resourcePools"`
	} `yaml:"vcenter"`
	Synthetic struct {
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

func TestProductionProvisioningContainmentRemainsClosed(t *testing.T) {
	values := loadChartValues(t, "values.prod.yaml")
	if values.ReplicaCount.Worker != 0 {
		t.Errorf("production worker replicas = %d, want 0 during incident containment", values.ReplicaCount.Worker)
	}
	if values.Provisioning.Enabled || values.Provisioning.WorkerClaimsEnabled {
		t.Fatalf("production provisioning controls = %+v, want both false", values.Provisioning)
	}
	if values.Synthetic.ProvisioningExpectedEnabled {
		t.Fatal("production synthetic must expect provisioning disabled")
	}
	if values.Synthetic.Lifecycle.Enabled || values.Synthetic.Runner.Enabled || values.Synthetic.Janitor.Enabled {
		t.Fatal("production lifecycle, runner, and destructive janitor synthetics must remain disabled during containment")
	}
	if values.VCenter.Hosts != "esxi1.lab.jmal.io" {
		t.Fatalf("production VCENTER_HOSTS = %q, want ESXi1 only", values.VCenter.Hosts)
	}
	if values.VCenter.ResourcePools != "/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs" {
		t.Fatalf("production resource pools = %q, want compatible Intel pool only", values.VCenter.ResourcePools)
	}
	if values.Worker.OrphanReconciler.Enabled ||
		values.Worker.NetworkReconciler.Enabled ||
		values.Worker.L1Validation.Enabled ||
		values.Worker.TemplateHealth.Enabled ||
		values.Worker.IdleEvaluator.Enabled ||
		!values.Worker.IdleEvaluator.DryRun {
		t.Fatalf("production worker background mutation controls are not contained: %+v", values.Worker)
	}
	for path, want := range map[string]string{
		"replicaCount.worker":                   "0",
		"provisioning.enabled":                  "false",
		"provisioning.workerClaimsEnabled":      "false",
		"synthetic.provisioningExpectedEnabled": "false",
		"synthetic.lifecycle.enabled":           "false",
		"synthetic.janitor.enabled":             "false",
		"synthetic.runner.enabled":              "false",
		"vcenter.hosts":                         "esxi1.lab.jmal.io",
		"vcenter.resourcePools":                 "/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs",
		"worker.orphanReconciler.enabled":       "false",
		"worker.networkReconciler.enabled":      "false",
		"worker.l1Validation.enabled":           "false",
		"worker.templateHealth.enabled":         "false",
		"worker.idleEvaluator.enabled":          "false",
		"worker.idleEvaluator.dryRun":           "true",
	} {
		parts := strings.Split(path, ".")
		if got := chartScalar(t, "values.prod.yaml", parts...); got != want {
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
			".Values.vcenter.datastore",
			".Values.vcenter.resourcePools",
			".Values.vcenter.hosts",
			".Values.provisioning.enabled",
		},
		"worker-deployment.yaml": {
			"WORKER_PROVISIONING_CLAIMS_ENABLED",
			".Values.provisioning.workerClaimsEnabled",
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
