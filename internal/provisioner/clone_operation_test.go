package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type fakeCloneOperationStore struct {
	mu           sync.Mutex
	operation    *models.VMCloneOperation
	target       *VMCloneCleanupTarget
	events       []string
	persistErr   error
	persistCalls int
	abandonCalls int
}

func (f *fakeCloneOperationStore) PrepareVMCloneOperation(
	_ context.Context,
	_ uuid.UUID,
	_ string,
	candidate models.VMCloneOperation,
) (*models.VMCloneOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "prepare")
	if f.operation == nil {
		copy := candidate
		f.operation = &copy
	}
	copy := *f.operation
	return &copy, nil
}

func (f *fakeCloneOperationStore) ArmVMCloneOperation(
	_ context.Context,
	_ uuid.UUID,
	_, _ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "arm")
	f.operation.Phase = models.VMCloneOperationSubmitting
	return nil
}

func (f *fakeCloneOperationStore) AbandonUnsubmittedVMCloneOperation(
	_ context.Context,
	_ uuid.UUID,
	_, operationID string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandonCalls++
	f.events = append(f.events, "abandon")
	if f.operation != nil && f.operation.OperationID == operationID {
		f.operation = nil
	}
	return nil
}

func (f *fakeCloneOperationStore) PersistVMCloneTask(
	_ context.Context,
	_ uuid.UUID,
	_, _, taskRef string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persistCalls++
	f.events = append(f.events, "persist-task")
	if f.persistErr != nil {
		return f.persistErr
	}
	f.operation.TaskRef = taskRef
	f.operation.Phase = models.VMCloneOperationSubmitted
	return nil
}

func (f *fakeCloneOperationStore) StageVMCloneCleanup(
	_ context.Context,
	_ uuid.UUID,
	_ string,
	raw []byte,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "stage")
	var target VMCloneCleanupTarget
	if err := json.Unmarshal(raw, &target); err != nil {
		return err
	}
	f.target = &target
	return nil
}

func (f *fakeCloneOperationStore) CompleteVMCloneOperationWithoutResource(
	_ context.Context,
	_ uuid.UUID,
	_, _ string,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operation = nil
	f.events = append(f.events, "complete-empty")
	return nil
}

func (f *fakeCloneOperationStore) taskPersisted(taskRef string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.operation != nil && f.operation.TaskRef == taskRef
}

type fakeCloneOperationClient struct {
	store         *fakeCloneOperationStore
	preArmErr     error
	startErr      error
	waitErrs      []error
	taskRef       string
	waitMoref     string
	findMoref     string
	findErr       error
	startCalls    int
	waitCalls     int
	findCalls     int
	configCalls   int
	cancelOnStart context.CancelFunc
}

func (f *fakeCloneOperationClient) ResolveClonePlacement(
	_ context.Context,
	params vcenter.CloneVMParams,
) (vcenter.CloneVMParams, error) {
	if params.HostMoRef == "" {
		params.HostMoRef = "host-1"
	}
	if params.HostName == "" {
		params.HostName = "ESXi1"
	}
	if params.ResourcePoolMoRef == "" {
		params.ResourcePoolMoRef = "resgroup-1"
	}
	return params, nil
}

func (f *fakeCloneOperationClient) ValidateVMPlacement(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeCloneOperationClient) StartCloneVMOperation(
	ctx context.Context,
	_ vcenter.CloneVMParams,
	arm func(context.Context) error,
) (string, error) {
	if f.preArmErr != nil {
		return "", f.preArmErr
	}
	if err := arm(ctx); err != nil {
		return "", err
	}
	f.startCalls++
	if f.cancelOnStart != nil {
		f.cancelOnStart()
	}
	if f.startErr != nil {
		return "", f.startErr
	}
	return f.taskRef, nil
}

func TestPreSubmissionFailureDoesNotArmCleanup(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{preArmErr: errors.New("resource pool lookup failed")}

	_, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	)
	if err == nil || isCompensationRetry(err) {
		t.Fatalf("pre-submission error = %v, want ordinary retry classification", err)
	}
	if store.operation != nil || store.abandonCalls != 1 {
		t.Fatalf("pre-submission operation = %+v, abandon calls = %d; want cleared operation", store.operation, store.abandonCalls)
	}
	if client.startCalls != 0 || store.target != nil {
		t.Fatalf("pre-submission failure crossed external boundary: starts=%d target=%+v", client.startCalls, store.target)
	}
}

func TestLegacyCloneOperationWithoutPlacementRequiresManualCleanup(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID: uuid.NewString(),
			PodID:       podID.String(),
			PodVMID:     podVMID.String(),
			TargetName:  params.VMName,
			SourceRef:   params.TemplateName,
			Phase:       models.VMCloneOperationSubmitting,
			PreparedAt:  time.Now(),
		},
	}
	client := &fakeCloneOperationClient{store: store}

	_, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	)
	var manualErr *manualCleanupRequiredError
	if !errors.As(err, &manualErr) {
		t.Fatalf("legacy clone operation error = %v, want manual cleanup", err)
	}
	if client.startCalls != 0 || client.waitCalls != 0 || client.findCalls != 0 || client.configCalls != 0 {
		t.Fatalf(
			"legacy clone operation issued vCenter work: start=%d wait=%d find=%d configure=%d",
			client.startCalls,
			client.waitCalls,
			client.findCalls,
			client.configCalls,
		)
	}
}

func (f *fakeCloneOperationClient) WaitCloneVMTask(_ context.Context, taskRef string) (string, error) {
	f.waitCalls++
	if f.store != nil && !f.store.taskPersisted(taskRef) {
		return "", errors.New("task wait started before task reference was durable")
	}
	if len(f.waitErrs) > 0 {
		err := f.waitErrs[0]
		f.waitErrs = f.waitErrs[1:]
		if err != nil {
			return "", err
		}
	}
	return f.waitMoref, nil
}

func (f *fakeCloneOperationClient) FindVMByCloneOperation(
	_ context.Context,
	_ vcenter.CloneVMParams,
) (string, error) {
	f.findCalls++
	return f.findMoref, f.findErr
}

func (f *fakeCloneOperationClient) ConfigureClonedVM(
	_ context.Context,
	_ string,
	_ vcenter.CloneVMParams,
) error {
	f.configCalls++
	return nil
}

func cloneOperationFixture() (uuid.UUID, string, uuid.UUID, uuid.UUID, vcenter.CloneVMParams) {
	return uuid.New(), "worker-a:claim-a", uuid.New(), uuid.New(), vcenter.CloneVMParams{
		TemplateName:      "vm-100",
		VMName:            "pod-target",
		VCPUs:             2,
		RAMmb:             4096,
		Network:           "Pod-VLAN123",
		HostMoRef:         "host-1",
		HostName:          "ESXi1",
		ResourcePoolMoRef: "resgroup-1",
	}
}

func TestDurableClonePersistsTaskBeforeWait(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:     store,
		taskRef:   "task-42",
		waitMoref: "vm-42",
	}

	moref, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	)
	if err != nil {
		t.Fatalf("executeDurableVMClone: %v", err)
	}
	if moref != "vm-42" {
		t.Fatalf("moref = %q, want vm-42", moref)
	}
	if client.startCalls != 1 || client.waitCalls != 1 || client.configCalls != 1 {
		t.Fatalf("calls start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-42" {
		t.Fatalf("exact clone target was not staged: %+v", store.target)
	}
}

func TestAcceptedResponseLostReconcilesMarkerWithoutSecondClone(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		startErr:  errors.New("transport closed after request"),
		findMoref: "vm-accepted",
	}

	_, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	)
	if !isCompensationRetry(err) {
		t.Fatalf("ambiguous submission error = %v, want compensation retry", err)
	}
	if store.operation == nil || store.operation.Phase != models.VMCloneOperationSubmitting {
		t.Fatalf("operation marker was not retained: %+v", store.operation)
	}
	if err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	); err != nil {
		t.Fatalf("reconcileCloneOperationForCleanup: %v", err)
	}
	if client.startCalls != 1 {
		t.Fatalf("CloneVM_Task submissions = %d, want 1", client.startCalls)
	}
	if client.configCalls != 0 {
		t.Fatalf("cleanup reconciliation made %d forward configuration calls", client.configCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-accepted" {
		t.Fatalf("marker reconciliation did not stage exact target: %+v", store.target)
	}
}

func TestPreparedOperationCleanupProvesNoSubmissionAndClearsIdentity(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID: uuid.NewString(),
			PodID:       podID.String(),
			PodVMID:     podVMID.String(),
			TargetName:  params.VMName,
			SourceRef:   params.TemplateName,
			Phase:       models.VMCloneOperationPrepared,
			PreparedAt:  time.Now(),
		},
	}
	client := &fakeCloneOperationClient{}

	if err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	); err != nil {
		t.Fatalf("reconcile prepared operation: %v", err)
	}
	if store.operation != nil || store.abandonCalls != 1 {
		t.Fatalf("operation = %+v, abandon calls = %d; want cleared identity", store.operation, store.abandonCalls)
	}
	if client.startCalls != 0 || client.waitCalls != 0 || client.findCalls != 0 || client.configCalls != 0 {
		t.Fatalf(
			"prepared cleanup crossed vCenter boundary: start=%d wait=%d find=%d configure=%d",
			client.startCalls,
			client.waitCalls,
			client.findCalls,
			client.configCalls,
		)
	}
}

func TestRecoveredCleanupResumesPersistedTaskWithoutForwardCalls(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID: uuid.NewString(),
			PodID:       podID.String(),
			PodVMID:     podVMID.String(),
			TargetName:  params.VMName,
			SourceRef:   params.TemplateName,
			HostMoref:   params.HostMoRef,
			HostName:    params.HostName,
			PoolMoref:   params.ResourcePoolMoRef,
			TaskRef:     "task-existing",
			Phase:       models.VMCloneOperationSubmitted,
			PreparedAt:  time.Now(),
		},
	}
	client := &fakeCloneOperationClient{
		store:     store,
		waitMoref: "vm-resumed",
	}

	err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	)
	if err != nil {
		t.Fatalf("reconcileCloneOperationForCleanup: %v", err)
	}
	if client.startCalls != 0 || client.waitCalls != 1 || client.configCalls != 0 {
		t.Fatalf("start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-resumed" {
		t.Fatalf("resumed task target = %+v", store.target)
	}
}

func TestCloneTaskTimeoutRemainsResumable(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:     store,
		taskRef:   "task-timeout",
		waitMoref: "vm-eventual",
		waitErrs:  []error{context.DeadlineExceeded, nil},
	}

	if _, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	); !isCompensationRetry(err) {
		t.Fatalf("timeout error = %v, want compensation retry", err)
	}
	err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	)
	if err != nil {
		t.Fatalf("resumed cleanup: %v", err)
	}
	if client.startCalls != 1 || client.waitCalls != 2 || client.configCalls != 0 {
		t.Fatalf("start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-eventual" {
		t.Fatalf("eventual cleanup target = %+v", store.target)
	}
}

func TestCanceledAfterSubmissionRetainsMarkerAndExitsBounded(t *testing.T) {
	oldTimeout := cloneOperationHandoffTimeout
	cloneOperationHandoffTimeout = 30 * time.Millisecond
	defer func() { cloneOperationHandoffTimeout = oldTimeout }()

	ctx, cancel := context.WithCancel(context.Background())
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{persistErr: errors.New("database unavailable")}
	client := &fakeCloneOperationClient{
		store:         store,
		taskRef:       "task-not-persisted",
		findMoref:     "vm-marked",
		cancelOnStart: cancel,
	}
	started := time.Now()
	_, err := executeDurableVMClone(ctx, store, client, jobID, workerID, podID, podVMID, params)
	if !isCompensationRetry(err) {
		t.Fatalf("handoff error = %v, want compensation retry", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown-safe task handoff took %s", elapsed)
	}
	if store.operation == nil || store.operation.OperationID == "" ||
		store.operation.Phase != models.VMCloneOperationSubmitting {
		t.Fatalf("durable operation marker lost after cancellation: %+v", store.operation)
	}
	store.persistErr = nil
	if err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	); err != nil {
		t.Fatalf("marker recovery: %v", err)
	}
	if client.startCalls != 1 || store.target == nil || store.target.VCenterVMID != "vm-marked" {
		t.Fatalf("start=%d target=%+v", client.startCalls, store.target)
	}
}

func TestUnresolvedSubmissionEscalatesWithoutDuplicateClone(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID: uuid.NewString(),
			PodID:       podID.String(),
			PodVMID:     podVMID.String(),
			TargetName:  params.VMName,
			SourceRef:   params.TemplateName,
			HostMoref:   params.HostMoRef,
			HostName:    params.HostName,
			PoolMoref:   params.ResourcePoolMoRef,
			Phase:       models.VMCloneOperationSubmitting,
			PreparedAt:  time.Now().Add(-cloneSubmissionReconcileDeadline),
		},
	}
	client := &fakeCloneOperationClient{}
	err := reconcileCloneOperationForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		store.operation,
	)
	if !isManualCleanupRequired(err) {
		t.Fatalf("unresolved operation error = %v, want manual cleanup", err)
	}
	if client.startCalls != 0 || client.configCalls != 0 {
		t.Fatalf("unresolved cleanup made forward calls: start=%d configure=%d", client.startCalls, client.configCalls)
	}
}
