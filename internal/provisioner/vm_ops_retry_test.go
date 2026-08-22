package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
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

type fakeCloneHandoffDestroyer struct {
	morefs []string
}

func (f *fakeCloneHandoffDestroyer) DestroyVM(ctx context.Context, moref string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.morefs = append(f.morefs, moref)
	return nil
}

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

func TestCanceledCloneHandoffDestroysExactTargetAndPersistsProof(t *testing.T) {
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
	destroyer := &fakeCloneHandoffDestroyer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	destroyed, err := persistVMCloneHandoff(
		ctx,
		store,
		destroyer,
		uuid.New(),
		"worker-a",
		targetJSON,
		target.VCenterVMID,
	)
	if err != nil {
		t.Fatalf("persistVMCloneHandoff returned %v", err)
	}
	if !destroyed {
		t.Fatal("canceled handoff neither persisted ownership nor destroyed the exact clone")
	}
	if store.stageCalls != 1 {
		t.Fatalf("stage calls = %d, want one bounded attempt before shutdown-safe cleanup", store.stageCalls)
	}
	if len(store.stageTargets) != 1 ||
		string(store.stageTargets[0]) != string(targetJSON) {
		t.Fatalf("exact target was not preserved during canceled handoff: %q", store.stageTargets)
	}
	if len(destroyer.morefs) != 1 || destroyer.morefs[0] != target.VCenterVMID {
		t.Fatalf("destroyed MoRefs = %v, want exact %q", destroyer.morefs, target.VCenterVMID)
	}
	if store.destructionStageCalls != 1 ||
		len(store.destructionTargets) != 1 ||
		string(store.destructionTargets[0]) != string(targetJSON) {
		t.Fatalf("durable pre-destroy handoff = %q, want exact target %q", store.destructionTargets, targetJSON)
	}
	if len(store.proofTargets) != 1 ||
		string(store.proofTargets[0]) != string(targetJSON) {
		t.Fatalf("destroyed-clone proof = %q, want exact target %q", store.proofTargets, targetJSON)
	}
}

func TestLostLeaseCloneHandoffDestroysExactMoRefAndRecordsProof(t *testing.T) {
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
	destroyer := &fakeCloneHandoffDestroyer{}

	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		destroyer,
		uuid.New(),
		"worker-a",
		targetJSON,
		target.VCenterVMID,
	)
	if err != nil {
		t.Fatalf("persistVMCloneHandoff returned %v", err)
	}
	if !destroyed {
		t.Fatal("lost-lease clone handoff did not use synchronous exact cleanup")
	}
	if store.destructionStageCalls != 1 ||
		len(store.destructionTargets) != 1 ||
		string(store.destructionTargets[0]) != string(targetJSON) {
		t.Fatalf("durable pre-destroy handoff = %q, want exact target %q", store.destructionTargets, targetJSON)
	}
	if len(destroyer.morefs) != 1 || destroyer.morefs[0] != target.VCenterVMID {
		t.Fatalf("destroyed MoRefs = %v, want exact %q", destroyer.morefs, target.VCenterVMID)
	}
	if len(store.proofTargets) != 1 ||
		string(store.proofTargets[0]) != string(targetJSON) {
		t.Fatalf("destroyed-clone proof = %q, want exact target %q", store.proofTargets, targetJSON)
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
	destroyer := &fakeCloneHandoffDestroyer{}

	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		destroyer,
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
	if len(destroyer.morefs) != 0 {
		t.Fatalf("already-destroyed target was destroyed again: %v", destroyer.morefs)
	}
	if len(store.proofTargets) != 0 {
		t.Fatal("existing destruction proof was redundantly rewritten")
	}
}

func TestLostLeaseHandoffRetriesDurableIntentBeforeDestroy(t *testing.T) {
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
		stageErrs:            []error{database.ErrJobLeaseLost},
		destructionStageErrs: []error{errors.New("database unavailable"), nil},
	}
	destroyer := &fakeCloneHandoffDestroyer{}

	destroyed, err := persistVMCloneHandoff(
		context.Background(),
		store,
		destroyer,
		uuid.New(),
		"worker-a:claim-a",
		targetJSON,
		target.VCenterVMID,
	)
	if err != nil {
		t.Fatalf("persistVMCloneHandoff returned %v", err)
	}
	if !destroyed {
		t.Fatal("lost-lease handoff did not complete exact destruction")
	}
	if store.destructionStageCalls != 2 {
		t.Fatalf("destruction-stage calls = %d, want transient failure then success", store.destructionStageCalls)
	}
	if len(destroyer.morefs) != 1 {
		t.Fatalf("destroy ran %d times, want once after durable handoff", len(destroyer.morefs))
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
