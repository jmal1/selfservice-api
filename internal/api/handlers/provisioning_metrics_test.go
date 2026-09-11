package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProvisioningAdmissionMetricsExposeBoundedRoutes(t *testing.T) {
	m := NewProvisioningAdmissionMetrics("", "", nil, false)
	m.RecordRejected(provisioningRoutePodCreate)
	m.RecordRejected("attacker-controlled-route")

	body := string(m.serialize())
	for _, fragment := range []string{
		"# HELP crucible_provisioning_admission_enabled Whether the API accepts new pod and VM provisioning requests",
		"crucible_provisioning_admission_enabled 0",
		`crucible_provisioning_admission_rejected_total{route="pod_create"} 1`,
		`crucible_provisioning_admission_rejected_total{route="blueprint_deploy"} 0`,
		`crucible_provisioning_admission_rejected_total{route="vm_add"} 0`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("metrics body missing %q:\n%s", fragment, body)
		}
	}
	if strings.Contains(body, "attacker-controlled-route") {
		t.Fatalf("metrics accepted an unbounded route label:\n%s", body)
	}
}

func TestProvisioningAdmissionMetricsPushIndependently(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	m := NewProvisioningAdmissionMetrics(srv.URL, "admission", map[string]string{"layer": "api"}, true)
	m.HTTP = srv.Client()
	if err := m.Push(context.Background()); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if !strings.Contains(gotBody, "crucible_provisioning_admission_enabled 1") {
		t.Fatalf("push body missing enabled gauge:\n%s", gotBody)
	}
}
