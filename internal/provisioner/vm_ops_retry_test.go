package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type fakeCloneHandoffStore struct {
	stageErrs             []error
	stageCalls            int
	stageTargets          [][]byte
	destructionStageErrs  []error
	destructionStageCalls int
	destructionTargets    [][]byte
	proofTargets          [][]byte
}

func TestShouldCompensateVMAddFailureProtectsRunningVM(t *testing.T) {
	job := &models.Job{Type: models.JobTypeVMAdd, RetryCount: 3, MaxRetries: 3}
	releaseErr := fmt.Errorf("%w: deadlock detected", errVMPlacementCapacityRelease)
	if shouldCompensateVMAddFailure(job, releaseErr, true) {
		t.Fatal("capacity-release failure would destroy a persisted running VM")
	}
	if !shouldCompensateVMAddFailure(job, errors.New("deterministic configuration failure"), false) {
		t.Fatal("terminal pre-running failure would skip exact clone compensation")
	}
	job.RetryCount = 0
	if shouldCompensateVMAddFailure(job, releaseErr, false) {
		t.Fatal("retryable pre-running failure would compensate before retries were exhausted")
	}
}

func (f *fakeCloneHandoffStore) StageVMCloneDestructionHandoff(
	ctx context.Context,
	_ uuid.UUID,
	target []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.destructionTargets = append(f.destructionTargets, append([]byte(nil), target...))
	call := f.destructionStageCalls
	f.destructionStageCalls++
	if call < len(f.destructionStageErrs) {
		return f.destructionStageErrs[call]
	}
	return nil
}

func (f *fakeCloneHandoffStore) StageVMCloneCleanup(
	ctx context.Context,
	_ uuid.UUID,
	_ string,
	target []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.stageTargets = append(f.stageTargets, append([]byte(nil), target...))
	call := f.stageCalls
	f.stageCalls++
	if call < len(f.stageErrs) {
		return f.stageErrs[call]
	}
	return nil
}

func (f *fakeCloneHandoffStore) RecordDestroyedVMCloneHandoff(
	ctx context.Context,
	_ uuid.UUID,
	target []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.proofTargets = append(f.proofTargets, append([]byte(nil), target...))
	return nil
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

func fullCleanupTargetFixture() *VMCloneCleanupTarget {
	return &VMCloneCleanupTarget{
		PodID:                uuid.NewString(),
		PodVMID:              uuid.NewString(),
		VCenterVMID:          "vm-4242",
		LogicalTemplateID:    uuid.NewString(),
		SourceReplicaID:      uuid.NewString(),
		SourceRef:            "vm-source-amd",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		ResourcePoolMoref:    "resgroup-1",
		HostMoref:            "host-1",
		DRSControl:           vcenter.DRSControlDisabled,
	}
}

func TestMergeCloneCleanupIdentityRejectsPersistedIdentitySabotage(t *testing.T) {
	expected := fullCleanupTargetFixture()
	legacy := &VMCloneCleanupTarget{
		PodID:       expected.PodID,
		PodVMID:     expected.PodVMID,
		VCenterVMID: expected.VCenterVMID,
	}
	resolved, err := mergeCloneCleanupIdentity(legacy, expected)
	if err != nil {
		t.Fatalf("safe legacy cleanup target did not resolve through persisted identity: %v", err)
	}
	if resolved.HostMoref != expected.HostMoref ||
		resolved.ComputeResourceMoref != expected.ComputeResourceMoref ||
		resolved.ResourcePoolMoref != expected.ResourcePoolMoref ||
		resolved.SourceRef != expected.SourceRef {
		t.Fatalf("resolved legacy cleanup identity = %+v, want %+v", resolved, expected)
	}

	sabotages := map[string]func(*VMCloneCleanupTarget){
		"source":  func(target *VMCloneCleanupTarget) { target.SourceRef = "vm-source-intel" },
		"compute": func(target *VMCloneCleanupTarget) { target.ComputeResourceMoref = "domain-c2" },
		"pool":    func(target *VMCloneCleanupTarget) { target.ResourcePoolMoref = "resgroup-2" },
		"host":    func(target *VMCloneCleanupTarget) { target.HostMoref = "host-2" },
	}
	for name, sabotage := range sabotages {
		t.Run(name, func(t *testing.T) {
			actual := *expected
			sabotage(&actual)
			if _, err := mergeCloneCleanupIdentity(&actual, expected); err == nil {
				t.Fatalf("sabotaged %s identity was accepted: %+v", name, actual)
			}
		})
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

func TestCanceledCloneHandoffPersistsExactTargetWithoutStaleDestroy(t *testing.T) {
	target := VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCloneHandoffStore{
		stageErrs: []error{errors.New("database unavailable"), nil},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	destroyed, err := persistVMCloneHandoff(
		ctx,
		store,
		uuid.New(),
		"worker-a",
		targetJSON,
		target.VCenterVMID,
	)
	if err != nil {
		t.Fatalf("persistVMCloneHandoff returned %v", err)
	}
	if destroyed {
		t.Fatal("canceled owner destructively cleaned a clone after durable staging")
	}
	if store.stageCalls != 2 {
		t.Fatalf("stage calls = %d, want transient failure then durable handoff", store.stageCalls)
	}
	if len(store.stageTargets) != 2 ||
		string(store.stageTargets[1]) != string(targetJSON) {
		t.Fatalf("exact target was not preserved during canceled handoff: %q", store.stageTargets)
	}
	if store.destructionStageCalls != 0 || len(store.proofTargets) != 0 {
		t.Fatal("canceled owner attempted destructive fallback instead of durable successor handoff")
	}
}

func TestLostLeaseCloneHandoffNeverDestroysBehindSuccessor(t *testing.T) {
	target := VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCloneHandoffStore{
		stageErrs: []error{database.ErrJobLeaseLost},
	}
	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		uuid.New(),
		"worker-a",
		targetJSON,
		target.VCenterVMID,
	)
	if !errors.Is(err, database.ErrJobLeaseLost) {
		t.Fatalf("persistVMCloneHandoff error = %v, want lease lost", err)
	}
	if destroyed {
		t.Fatal("lost owner reported a destructive handoff")
	}
	if store.destructionStageCalls != 0 || len(store.proofTargets) != 0 {
		t.Fatal("lost owner destroyed or rewrote successor-owned state")
	}
}

func TestPreviouslyDestroyedCloneCannotBeStagedOrDestroyedAgain(t *testing.T) {
	target := VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCloneHandoffStore{
		stageErrs: []error{database.ErrVMCloneAlreadyDestroyed},
	}
	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		uuid.New(),
		"worker-a:claim-a",
		targetJSON,
		target.VCenterVMID,
	)
	if err != nil {
		t.Fatalf("persistVMCloneHandoff returned %v", err)
	}
	if !destroyed {
		t.Fatal("durable destruction proof was not treated as completed handoff")
	}
	if len(store.proofTargets) != 0 {
		t.Fatal("existing destruction proof was redundantly rewritten")
	}
}

func TestLostLeaseHandoffStopsWithoutDestructiveRetry(t *testing.T) {
	target := VMCloneCleanupTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-4242",
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeCloneHandoffStore{
		stageErrs: []error{database.ErrJobLeaseLost},
	}
	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		uuid.New(),
		"worker-a:claim-a",
		targetJSON,
		target.VCenterVMID,
	)
	if !errors.Is(err, database.ErrJobLeaseLost) || destroyed {
		t.Fatalf("destroyed=%t err=%v, want non-destructive lease-loss handoff", destroyed, err)
	}
	if store.stageCalls != 1 || store.destructionStageCalls != 0 {
		t.Fatalf("stage=%d destruction_stage=%d", store.stageCalls, store.destructionStageCalls)
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
		name      string
		target    *VMCloneCleanupTarget
		operation *models.VMCloneOperation
		want      bool
	}{
		{name: "marker only", want: false},
		{
			name: "durable operation",
			operation: &models.VMCloneOperation{
				OperationID: uuid.NewString(),
				PodID:       podID.String(),
				PodVMID:     podVMID.String(),
				TargetName:  "target",
				SourceRef:   "vm-source",
				Phase:       models.VMCloneOperationSubmitting,
				PreparedAt:  time.Now(),
			},
			want: true,
		},
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
			payload.CloneOperation = tc.operation
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

func TestPodCreateRollbackReceiptAuthorizesPreCloneCleanup(t *testing.T) {
	payload := CreatePodPayload{PodID: uuid.New()}
	if podCreateCleanupCanTransition(payload, nil) {
		t.Fatal("cleanup without a clone target or rollback receipt was authorized")
	}
	steps := []rollback.Step{{
		Name: "vlan",
		Data: json.RawMessage(`{"vlan_id":123}`),
	}}
	if !podCreateCleanupCanTransition(payload, steps) {
		t.Fatal("durable pre-clone rollback receipt did not authorize cleanup")
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
