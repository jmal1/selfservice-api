package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	loadCalls    int
}

func (f *fakeCloneOperationStore) GetVMCloneOperation(
	_ context.Context,
	_ uuid.UUID,
	_ string,
) (*models.VMCloneOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadCalls++
	f.events = append(f.events, "load")
	if f.operation == nil {
		return nil, nil
	}
	copy := *f.operation
	return &copy, nil
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
	validateErr   error
	validateErrs  []error
	configErrs    []error
	configParams  []vcenter.CloneVMParams
	resolveErr    error
	resolveCalls  int
	startCalls    int
	waitCalls     int
	findCalls     int
	validateCalls int
	configCalls   int
	cancelOnStart context.CancelFunc
}

func (f *fakeCloneOperationClient) ResolveClonePlacement(
	_ context.Context,
	params vcenter.CloneVMParams,
) (vcenter.CloneVMParams, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return vcenter.CloneVMParams{}, f.resolveErr
	}
	if params.HostMoRef == "" {
		params.HostMoRef = "host-1"
	}
	if params.HostName == "" {
		params.HostName = "ESXi1"
	}
	if params.ResourcePoolMoRef == "" {
		params.ResourcePoolMoRef = "resgroup-1"
	}
	if params.ComputeResourceType == "" {
		params.ComputeResourceType = "ClusterComputeResource"
	}
	if params.ComputeResourceMoRef == "" {
		params.ComputeResourceMoRef = "domain-c1"
	}
	if params.DRSControl == "" {
		params.DRSControl = models.VMPlacementDRSDisabled
	}
	return params, nil
}

func (f *fakeCloneOperationClient) ValidateVMPlacement(_ context.Context, _, _ string) error {
	f.validateCalls++
	if len(f.validateErrs) > 0 {
		err := f.validateErrs[0]
		f.validateErrs = f.validateErrs[1:]
		return err
	}
	return f.validateErr
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

func TestDurableCloneRejectsMissingLogicalTemplateIDBeforePlacement(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	params.LogicalTemplateID = ""
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{}

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
	if err == nil || !strings.Contains(err.Error(), "logical template ID") {
		t.Fatalf("missing logical template ID error = %v", err)
	}
	if store.operation != nil || client.startCalls != 0 {
		t.Fatalf("missing logical template ID crossed durable boundary: operation=%+v starts=%d", store.operation, client.startCalls)
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
	params vcenter.CloneVMParams,
) error {
	f.configCalls++
	f.configParams = append(f.configParams, params)
	if len(f.configErrs) > 0 {
		err := f.configErrs[0]
		f.configErrs = f.configErrs[1:]
		return err
	}
	return nil
}

func cloneOperationFixture() (uuid.UUID, string, uuid.UUID, uuid.UUID, vcenter.CloneVMParams) {
	return uuid.New(), "worker-a:claim-a", uuid.New(), uuid.New(), vcenter.CloneVMParams{
		LogicalTemplateID:    uuid.NewString(),
		TemplateName:         "vm-100",
		VMName:               "pod-target",
		VCPUs:                2,
		RAMmb:                4096,
		Network:              "Pod-VLAN123",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoRef: "domain-c1",
		HostMoRef:            "host-1",
		HostName:             "ESXi1",
		ResourcePoolMoRef:    "resgroup-1",
		DRSControl:           models.VMPlacementDRSDisabled,
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
	if store.loadCalls != 1 || client.resolveCalls != 1 {
		t.Fatalf("fresh clone loads=%d resolves=%d, want one of each", store.loadCalls, client.resolveCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-42" {
		t.Fatalf("exact clone target was not staged: %+v", store.target)
	}
}

func TestDurableCloneResumeSkipsFreshPlacementResolution(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	sourceReplicaID := uuid.NewString()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID:          uuid.NewString(),
			PodID:                podID.String(),
			PodVMID:              podVMID.String(),
			LogicalTemplateID:    params.LogicalTemplateID,
			TargetName:           params.VMName,
			SourceReplicaID:      sourceReplicaID,
			SourceRef:            "vm-original-source",
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoref: params.ComputeResourceMoRef,
			HostMoref:            params.HostMoRef,
			HostName:             params.HostName,
			PoolMoref:            params.ResourcePoolMoRef,
			DRSControl:           params.DRSControl,
			TaskRef:              "task-resume",
			Phase:                models.VMCloneOperationSubmitted,
			PreparedAt:           time.Now(),
		},
	}
	client := &fakeCloneOperationClient{
		store:      store,
		resolveErr: errors.New("source was renamed and host capacity changed"),
		waitMoref:  "vm-resume",
	}
	retryParams := params
	retryParams.TemplateName = "vm-renamed-source"
	retryParams.SourceReplicaID = uuid.NewString()
	retryParams.ComputeResourceMoRef = "domain-sabotaged"
	retryParams.HostMoRef = "host-sabotaged"
	retryParams.ResourcePoolMoRef = "resgroup-sabotaged"
	retryParams.ObservedFreeMemoryMB = 1
	retryParams.Password = "PersistedPerPod1!"

	moref, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		retryParams,
	)
	if err != nil {
		t.Fatalf("resume exact clone after mutable placement drift: %v", err)
	}
	if moref != "vm-resume" {
		t.Fatalf("resumed moref = %q, want vm-resume", moref)
	}
	if client.resolveCalls != 0 || client.startCalls != 0 ||
		client.waitCalls != 1 || client.configCalls != 1 {
		t.Fatalf(
			"resolve=%d start=%d wait=%d configure=%d",
			client.resolveCalls,
			client.startCalls,
			client.waitCalls,
			client.configCalls,
		)
	}
	got := client.configParams[0]
	if got.OperationID != store.operation.OperationID ||
		got.TemplateName != "vm-original-source" ||
		got.SourceReplicaID != sourceReplicaID ||
		got.ComputeResourceMoRef != params.ComputeResourceMoRef ||
		got.HostMoRef != params.HostMoRef ||
		got.ResourcePoolMoRef != params.ResourcePoolMoRef ||
		got.Password != retryParams.Password {
		t.Fatalf("resume used mutable placement instead of persisted identity: %+v", got)
	}
}

func TestPersistedClonePreambleFailurePreservesRetryThenRequiresCompensation(t *testing.T) {
	op := &models.VMCloneOperation{OperationID: uuid.NewString()}
	job := &models.Job{
		Type:       models.JobTypeVMAdd,
		RetryCount: 0,
		MaxRetries: 2,
	}
	transient := persistedClonePreambleFailure(
		job,
		op,
		fmt.Errorf("reload pod before resume: %w", context.DeadlineExceeded),
	)
	if !jobRetryAvailable(job, transient) || isCompensationRetry(transient) {
		t.Fatalf("transient preamble failure lost forward retry: %T %v", transient, transient)
	}

	job.RetryCount = job.MaxRetries
	exhausted := persistedClonePreambleFailure(
		job,
		op,
		fmt.Errorf("reload pod before resume: %w", context.DeadlineExceeded),
	)
	if !isCompensationRetry(exhausted) {
		t.Fatalf("exhausted preamble failure did not enter exact compensation: %T %v", exhausted, exhausted)
	}

	terminal := persistedClonePreambleFailure(
		&models.Job{Type: models.JobTypeVMAdd, MaxRetries: 2},
		op,
		errors.New("pod entered destroyed"),
	)
	if !isCompensationRetry(terminal) {
		t.Fatalf("terminal preamble exit did not enter exact compensation: %T %v", terminal, terminal)
	}
	if isCompensatedJobError(terminal) {
		t.Fatal("persisted operation was incorrectly finalized as no-clone compensation")
	}
}

func TestDurableCloneReturnsExactTargetForConfirmedPlacementDrift(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{
		operation: &models.VMCloneOperation{
			OperationID:          uuid.NewString(),
			PodID:                podID.String(),
			PodVMID:              podVMID.String(),
			LogicalTemplateID:    params.LogicalTemplateID,
			TargetName:           params.VMName,
			SourceRef:            params.TemplateName,
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoref: params.ComputeResourceMoRef,
			HostMoref:            params.HostMoRef,
			HostName:             params.HostName,
			PoolMoref:            params.ResourcePoolMoRef,
			DRSControl:           params.DRSControl,
			Phase:                models.VMCloneOperationSubmitted,
			TaskRef:              "task-42",
			PreparedAt:           time.Now(),
		},
	}
	client := &fakeCloneOperationClient{
		store:     store,
		waitMoref: "vm-42",
		validateErr: &vcenter.PlacementDriftError{
			Kind:   vcenter.PlacementDriftHost,
			Detail: "wrong host",
		},
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
	if moref != "vm-42" {
		t.Fatalf("placement drift target = %q, want vm-42", moref)
	}
	if !isManualCleanupRequired(err) {
		t.Fatalf("placement drift error = %v, want manual cleanup", err)
	}
	if store.target == nil ||
		store.target.VCenterVMID != "vm-42" ||
		store.target.HostMoref != params.HostMoRef ||
		store.target.ComputeResourceMoref != params.ComputeResourceMoRef ||
		store.target.ResourcePoolMoref != params.ResourcePoolMoRef ||
		store.target.SourceRef != params.TemplateName {
		t.Fatalf("placement drift did not stage complete exact cleanup identity: %+v", store.target)
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

func TestCleanupReconciliationStagesExactIdentityBeforeReturningDrift(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	op := &models.VMCloneOperation{
		OperationID:          uuid.NewString(),
		PodID:                podID.String(),
		PodVMID:              podVMID.String(),
		LogicalTemplateID:    params.LogicalTemplateID,
		TargetName:           params.VMName,
		SourceRef:            params.TemplateName,
		ComputeResourceType:  params.ComputeResourceType,
		ComputeResourceMoref: params.ComputeResourceMoRef,
		HostMoref:            params.HostMoRef,
		HostName:             params.HostName,
		PoolMoref:            params.ResourcePoolMoRef,
		DRSControl:           params.DRSControl,
		Phase:                models.VMCloneOperationSubmitting,
		PreparedAt:           time.Now(),
	}
	store := &fakeCloneOperationStore{operation: op}
	client := &fakeCloneOperationClient{
		findMoref: "vm-moved",
		findErr: &vcenter.PlacementDriftError{
			Kind:   vcenter.PlacementDriftHost,
			Detail: "marked clone moved from its persisted host",
		},
	}

	target, err := reconcileCloneOperationTargetForCleanup(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		op,
	)
	if !isManualCleanupRequired(err) {
		t.Fatalf("cleanup reconciliation error = %v, want manual cleanup", err)
	}
	if target == nil || store.target == nil {
		t.Fatalf("cleanup reconciliation lost exact target: returned=%+v staged=%+v", target, store.target)
	}
	if target.VCenterVMID != "vm-moved" ||
		target.HostMoref != params.HostMoRef ||
		target.ComputeResourceMoref != params.ComputeResourceMoRef ||
		target.ResourcePoolMoref != params.ResourcePoolMoRef ||
		target.SourceRef != params.TemplateName {
		t.Fatalf("cleanup reconciliation target = %+v, want complete persisted identity", target)
	}
	if client.configCalls != 0 {
		t.Fatalf("cleanup reconciliation made %d forward configuration calls", client.configCalls)
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
			OperationID:          uuid.NewString(),
			PodID:                podID.String(),
			PodVMID:              podVMID.String(),
			LogicalTemplateID:    params.LogicalTemplateID,
			TargetName:           params.VMName,
			SourceRef:            params.TemplateName,
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoref: params.ComputeResourceMoRef,
			HostMoref:            params.HostMoRef,
			HostName:             params.HostName,
			PoolMoref:            params.ResourcePoolMoRef,
			DRSControl:           params.DRSControl,
			TaskRef:              "task-existing",
			Phase:                models.VMCloneOperationSubmitted,
			PreparedAt:           time.Now(),
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

func TestCloneTaskWaitTransientResumesPersistedTaskForward(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:     store,
		taskRef:   "task-timeout",
		waitMoref: "vm-eventual",
		waitErrs:  []error{context.DeadlineExceeded, nil},
	}
	job := &models.Job{Type: models.JobTypePodCreate, RetryCount: 0, MaxRetries: 3}

	if _, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		params,
	); !isCloneForwardRetry(err) || isCompensationRetry(err) ||
		!cloneForwardRetryAvailable(job, err) {
		t.Fatalf("timeout error = %v, want available forward retry", err)
	}
	if store.operation == nil ||
		store.operation.Phase != models.VMCloneOperationSubmitted ||
		store.operation.TaskRef != "task-timeout" {
		t.Fatalf("transient wait lost persisted task identity: %+v", store.operation)
	}

	retryParams := params
	retryParams.HostMoRef = "host-sabotaged"
	retryParams.ResourcePoolMoRef = "resgroup-sabotaged"
	moref, err := executeDurableVMClone(
		context.Background(),
		store,
		client,
		jobID,
		workerID,
		podID,
		podVMID,
		retryParams,
	)
	if err != nil {
		t.Fatalf("resume persisted task forward: %v", err)
	}
	if moref != "vm-eventual" {
		t.Fatalf("resumed task moref = %q, want vm-eventual", moref)
	}
	if client.startCalls != 1 || client.waitCalls != 2 || client.configCalls != 1 {
		t.Fatalf("start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
	}
	if store.target == nil || store.target.VCenterVMID != "vm-eventual" {
		t.Fatalf("eventual exact target = %+v", store.target)
	}
	if got := client.configParams[0]; got.HostMoRef != params.HostMoRef ||
		got.ResourcePoolMoRef != params.ResourcePoolMoRef {
		t.Fatalf("resumed task used mutable retry placement: %+v", got)
	}
}

func TestClonePlacementValidationFailureRequiresExactCompensation(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:        store,
		taskRef:      "task-validation",
		waitMoref:    "vm-validation",
		validateErrs: []error{context.DeadlineExceeded},
	}

	moref, err := executeDurableVMClone(
		context.Background(), store, client, jobID, workerID, podID, podVMID, params,
	)
	if moref != "vm-validation" || isCloneForwardRetry(err) || !isCompensationRetry(err) {
		t.Fatalf("placement validation failure = (%q, %v), want exact compensation", moref, err)
	}
	if store.target == nil || store.target.VCenterVMID != moref {
		t.Fatalf("placement validation failure lost staged exact target: %+v", store.target)
	}
	if client.startCalls != 1 || client.waitCalls != 1 ||
		client.validateCalls != 1 || client.configCalls != 0 {
		t.Fatalf(
			"start=%d wait=%d validate=%d configure=%d",
			client.startCalls,
			client.waitCalls,
			client.validateCalls,
			client.configCalls,
		)
	}
}

func TestCloneConfigurationFailureRequiresExactCompensation(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:      store,
		taskRef:    "task-configure",
		waitMoref:  "vm-configure",
		configErrs: []error{context.DeadlineExceeded},
	}

	moref, err := executeDurableVMClone(
		context.Background(), store, client, jobID, workerID, podID, podVMID, params,
	)
	if moref != "vm-configure" || isCloneForwardRetry(err) || !isCompensationRetry(err) {
		t.Fatalf("configuration failure = (%q, %v), want exact compensation", moref, err)
	}
	if store.target == nil || store.target.VCenterVMID != moref {
		t.Fatalf("configuration failure lost staged exact target: %+v", store.target)
	}
	if client.startCalls != 1 || client.waitCalls != 1 || client.configCalls != 1 {
		t.Fatalf("start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
	}
	if got := client.configParams[0]; got.HostMoRef != params.HostMoRef ||
		got.ResourcePoolMoRef != params.ResourcePoolMoRef {
		t.Fatalf("configuration used mutable placement: %+v", got)
	}
}

func TestCloneForwardRetryExhaustionRequiresCompensation(t *testing.T) {
	_, _, podID, podVMID, params := cloneOperationFixture()
	target := cloneCleanupTarget(podID, podVMID, "vm-exhausted", params)
	forwardErr := newCloneForwardRetryError(context.DeadlineExceeded, target)
	job := &models.Job{
		Type:       models.JobTypePodCreate,
		RetryCount: 3,
		MaxRetries: 3,
	}
	if cloneForwardRetryAvailable(job, forwardErr) {
		t.Fatal("exhausted clone failure retained a forward retry")
	}
	compensationErr := cloneForwardFailureToCompensation(forwardErr)
	if !isCompensationRetry(compensationErr) {
		t.Fatalf("exhausted clone error = %v, want compensation", compensationErr)
	}
	var retryErr *compensationRetryError
	if !errors.As(compensationErr, &retryErr) || retryErr.target != target {
		t.Fatalf("exhausted clone compensation lost exact target: %#v", compensationErr)
	}
}

func TestCloneTerminalTaskFailureRequiresCompensation(t *testing.T) {
	jobID, workerID, podID, podVMID, params := cloneOperationFixture()
	store := &fakeCloneOperationStore{}
	client := &fakeCloneOperationClient{
		store:   store,
		taskRef: "task-terminal",
		waitErrs: []error{fmt.Errorf(
			"%w: The virtual disk is either corrupted or not a supported format",
			vcenter.ErrCloneTaskFailed,
		)},
	}

	_, err := executeDurableVMClone(
		context.Background(), store, client, jobID, workerID, podID, podVMID, params,
	)
	if !isCompensationRetry(err) {
		t.Fatalf("terminal task error = %v, want compensation", err)
	}
	if isCloneForwardRetry(err) {
		t.Fatalf("terminal task error = %v, must not resume failed task", err)
	}
	if retryable, _ := ClassifyError(err, models.JobTypePodCreate); !retryable {
		t.Fatalf("terminal task compensation must remain durably retryable: %v", err)
	}
	if client.startCalls != 1 || client.waitCalls != 1 || client.configCalls != 0 {
		t.Fatalf("start=%d wait=%d configure=%d", client.startCalls, client.waitCalls, client.configCalls)
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
			OperationID:          uuid.NewString(),
			PodID:                podID.String(),
			PodVMID:              podVMID.String(),
			LogicalTemplateID:    params.LogicalTemplateID,
			TargetName:           params.VMName,
			SourceRef:            params.TemplateName,
			ComputeResourceType:  params.ComputeResourceType,
			ComputeResourceMoref: params.ComputeResourceMoRef,
			HostMoref:            params.HostMoRef,
			HostName:             params.HostName,
			PoolMoref:            params.ResourcePoolMoRef,
			DRSControl:           params.DRSControl,
			Phase:                models.VMCloneOperationSubmitting,
			PreparedAt:           time.Now().Add(-cloneSubmissionReconcileDeadline),
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
