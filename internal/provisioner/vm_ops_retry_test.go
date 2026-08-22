package provisioner

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestShouldMarkVMAddErrorPreservesRetryableState(t *testing.T) {
	transient := errors.New("clone VM: The virtual disk is either corrupted or not a supported format")
	job := &models.Job{Type: models.JobTypeVMAdd, RetryCount: 0, MaxRetries: 3}
	if shouldMarkVMAddError(job, transient) {
		t.Fatal("retryable vm_add failure would be marked error before its retry")
	}

	job.RetryCount = job.MaxRetries
	if !shouldMarkVMAddError(job, transient) {
		t.Fatal("exhausted vm_add failure did not become terminal")
	}
}

func TestShouldMarkVMAddErrorMarksDeterministicFailure(t *testing.T) {
	job := &models.Job{Type: models.JobTypeVMAdd, RetryCount: 0, MaxRetries: 3}
	if !shouldMarkVMAddError(job, errors.New("clone VM: template_id not found")) {
		t.Fatal("deterministic vm_add failure did not become terminal")
	}
}

func TestStaleVMCloneCleanupPayloadCarriesDurableReference(t *testing.T) {
	podID := uuid.New()
	podVMID := uuid.New()
	body, err := staleVMCloneCleanupPayload(podID, podVMID, "vm-4242")
	if err != nil {
		t.Fatal(err)
	}
	var got DestroyVMPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.PodID != podID.String() || got.PodVMID != podVMID.String() ||
		got.VCenterVMID != "vm-4242" || !got.CleanupOnly {
		t.Fatalf("cleanup payload = %+v, want exact pod, VM, MoRef, and cleanup-only intent", got)
	}
}

func TestVCenterObjectNotFoundIsIdempotentCleanup(t *testing.T) {
	if !vcenterObjectNotFound(errors.New(`find VM "gone": object not found`)) {
		t.Fatal("vCenter not-found cleanup must be treated as already complete")
	}
	if vcenterObjectNotFound(errors.New("connection refused")) {
		t.Fatal("transport failure must not be treated as completed cleanup")
	}
}

func TestStaleVMCloneCleanupFailureIsRetryable(t *testing.T) {
	retryable, reason := ClassifyError(
		errors.New("stale VM clone cleanup failed: queue durable cleanup: connection refused"),
		models.JobTypeVMAdd,
	)
	if !retryable || reason != RetryReasonCleanup {
		t.Fatalf("retryable=%t reason=%q, want true/%q", retryable, reason, RetryReasonCleanup)
	}
}

func TestStalePodCreateCleanupFailureIsRetryable(t *testing.T) {
	retryable, reason := ClassifyError(
		errors.New("stale pod_create cleanup incomplete after activate: rollback vm_clone_0"),
		models.JobTypePodCreate,
	)
	if !retryable || reason != RetryReasonCleanup {
		t.Fatalf("retryable=%t reason=%q, want true/%q", retryable, reason, RetryReasonCleanup)
	}
}

func TestPodVMHasLiveCloneRejectsTerminalState(t *testing.T) {
	moref := "vm-4242"
	if !podVMHasLiveClone(&models.PodVM{Status: models.VMStatusRunning, VCenterVMID: &moref}, moref) {
		t.Fatal("running VM with matching MoRef must be recognized as the live clone")
	}
	if podVMHasLiveClone(&models.PodVM{Status: models.VMStatusDeleted, VCenterVMID: &moref}, moref) {
		t.Fatal("deleted VM must never be treated as a live clone")
	}
}
