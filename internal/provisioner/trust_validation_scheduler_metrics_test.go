package provisioner

import (
	"errors"
	"strings"
	"testing"
)

func TestL1ValidationSchedulerMetricsExposeHealthContract(t *testing.T) {
	metrics := NewL1ValidationSchedulerMetrics("", "test", nil)
	metrics.ObserveRun(L1TrustValidationCounts{Due: 3, Enqueued: 2}, nil)
	body := string(metrics.serialize())

	for _, want := range []string{
		"crucible_l1_validation_scheduler_last_run_timestamp_seconds",
		"crucible_l1_validation_scheduler_last_success_timestamp_seconds",
		"crucible_l1_validation_scheduler_due_templates 3",
		"crucible_l1_validation_scheduler_enqueued_jobs 2",
		"crucible_l1_validation_scheduler_errors_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scheduler metrics missing %q:\n%s", want, body)
		}
	}
}

func TestL1ValidationSchedulerMetricsFailurePreservesFreshness(t *testing.T) {
	metrics := NewL1ValidationSchedulerMetrics("", "test", nil)
	metrics.ObserveRun(L1TrustValidationCounts{}, errors.New("db down"))
	body := string(metrics.serialize())

	if !strings.Contains(body, "crucible_l1_validation_scheduler_last_success_timestamp_seconds 0") {
		t.Fatalf("first failed run must expose zero success freshness:\n%s", body)
	}
	if !strings.Contains(body, "crucible_l1_validation_scheduler_errors_total 1") {
		t.Fatalf("failed run must increment error counter:\n%s", body)
	}
}

func TestL1ValidationSchedulerMetricsNilReceiverIsInert(t *testing.T) {
	var metrics *L1ValidationSchedulerMetrics
	metrics.ObserveRun(L1TrustValidationCounts{Due: 1}, errors.New("ignored"))
	if err := metrics.Push(t.Context()); err != nil {
		t.Fatalf("nil metrics Push returned %v", err)
	}
}
