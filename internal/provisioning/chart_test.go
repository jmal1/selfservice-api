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
	if values.Synthetic.Lifecycle.Enabled || values.Synthetic.Runner.Enabled {
		t.Fatal("production lifecycle and runner synthetics must remain disabled during containment")
	}
	if !values.Synthetic.Janitor.Enabled {
		t.Fatal("production cleanup janitor must remain enabled during containment")
	}
	for path, want := range map[string]string{
		"replicaCount.worker":                   "0",
		"provisioning.enabled":                  "false",
		"provisioning.workerClaimsEnabled":      "false",
		"synthetic.provisioningExpectedEnabled": "false",
		"synthetic.lifecycle.enabled":           "false",
		"synthetic.janitor.enabled":             "true",
		"synthetic.runner.enabled":              "false",
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
			".Values.provisioning.enabled",
		},
		"worker-deployment.yaml": {
			"WORKER_PROVISIONING_CLAIMS_ENABLED",
			".Values.provisioning.workerClaimsEnabled",
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
