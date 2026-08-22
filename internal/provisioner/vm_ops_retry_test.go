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

func TestVMCloneCleanupTargetCarriesDurableReference(t *testing.T) {
	podID := uuid.New()
	podVMID := uuid.New()
	body, err := json.Marshal(VMCloneCleanupTarget{
		PodID:       podID.String(),
		PodVMID:     podVMID.String(),
		VCenterVMID: "vm-4242",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got VMCloneCleanupTarget
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.PodID != podID.String() || got.PodVMID != podVMID.String() ||
		got.VCenterVMID != "vm-4242" {
		t.Fatalf("cleanup target = %+v, want exact pod, VM, and MoRef", got)
	}
	if _, _, err := validateVMCloneCleanupTarget(&got); err != nil {
		t.Fatalf("valid exact target was rejected: %v", err)
	}
}

func TestVMCloneCleanupTargetRequiresExactMoRef(t *testing.T) {
	_, _, err := validateVMCloneCleanupTarget(&VMCloneCleanupTarget{
		PodID:   uuid.NewString(),
		PodVMID: uuid.NewString(),
	})
	if err == nil {
		t.Fatal("cleanup target without an exact MoRef was accepted")
	}
	if retryable, _ := ClassifyError(
		&manualCleanupRequiredError{err: err},
		models.JobTypeVMAdd,
	); retryable {
		t.Fatal("unsafe name-only cleanup was made retryable")
	}
}

func TestAddVMPayloadCleanupOnlyRoundTrip(t *testing.T) {
	want := AddVMPayload{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		CleanupOnly: true,
		CleanupTarget: &VMCloneCleanupTarget{
			PodID:       uuid.NewString(),
			PodVMID:     uuid.NewString(),
			VCenterVMID: "vm-4242",
		},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got AddVMPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CleanupOnly || got.CleanupTarget == nil ||
		got.CleanupTarget.VCenterVMID != want.CleanupTarget.VCenterVMID {
		t.Fatalf("cleanup payload = %+v, want %+v", got, want)
	}
}

func TestPodCreateCleanupProvisioningTransitionRequiresOwnedExactTarget(t *testing.T) {
	podID := uuid.New()
	podVMID := uuid.New()
	base := CreatePodPayload{
		PodID: podID,
		VMs:   []VMSpec{{PodVMID: podVMID}},
	}
	tests := []struct {
		name   string
		target *VMCloneCleanupTarget
		want   bool
	}{
		{name: "marker only", want: false},
		{
			name: "exact target",
			target: &VMCloneCleanupTarget{
				PodID:       podID.String(),
				PodVMID:     podVMID.String(),
				VCenterVMID: "vm-4242",
			},
			want: true,
		},
		{
			name: "different pod",
			target: &VMCloneCleanupTarget{
				PodID:       uuid.NewString(),
				PodVMID:     podVMID.String(),
				VCenterVMID: "vm-4242",
			},
			want: false,
		},
		{
			name: "different VM",
			target: &VMCloneCleanupTarget{
				PodID:       podID.String(),
				PodVMID:     uuid.NewString(),
				VCenterVMID: "vm-4242",
			},
			want: false,
		},
		{
			name: "missing MoRef",
			target: &VMCloneCleanupTarget{
				PodID:   podID.String(),
				PodVMID: podVMID.String(),
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := base
			payload.CleanupTarget = tc.target
			if got := podCreateCleanupOwnsStagedClone(payload); got != tc.want {
				t.Fatalf("podCreateCleanupOwnsStagedClone() = %t, want %t", got, tc.want)
			}
		})
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
		newPodCreateCleanupRetryError(
			"activate",
			[]error{errors.New("rollback vm_clone_0")},
		),
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
