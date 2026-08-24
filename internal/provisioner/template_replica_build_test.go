package provisioner

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type replicaBuildStoreFake struct {
	build             *models.TemplateReplicaBuild
	transitions       []string
	pendingReserved   bool
	finalized         bool
	finalizedTooEarly bool
	failedStatus      string
	failedPhase       string
}

func (f *replicaBuildStoreFake) GetTemplateReplicaBuildForJob(
	context.Context,
	uuid.UUID,
	string,
) (*models.TemplateReplicaBuild, error) {
	return f.build, nil
}

func (f *replicaBuildStoreFake) SaveTemplateReplicaBuildState(
	_ context.Context,
	build *models.TemplateReplicaBuild,
	expectedPhase, _ string,
) error {
	f.transitions = append(f.transitions, expectedPhase+"->"+build.Phase)
	return nil
}

func (f *replicaBuildStoreFake) EnsurePendingReplicaForBuild(
	_ context.Context,
	build *models.TemplateReplicaBuild,
	_ string,
) error {
	f.pendingReserved = true
	id := uuid.New()
	build.ResultReplicaID = &id
	return nil
}

func (f *replicaBuildStoreFake) FinalizeTemplateReplicaBuild(
	_ context.Context,
	build *models.TemplateReplicaBuild,
	_ string,
) error {
	if build.CleanupCompletedAt == nil {
		f.finalizedTooEarly = true
	}
	f.finalized = true
	build.Status = models.TemplateReplicaBuildReady
	build.Phase = models.TemplateReplicaBuildPhaseReady
	return nil
}

func (f *replicaBuildStoreFake) FailTemplateReplicaBuild(
	_ context.Context,
	build *models.TemplateReplicaBuild,
	_ string,
	status string,
	phase string,
	_ string,
	_ string,
) error {
	f.failedStatus = status
	f.failedPhase = phase
	build.Status = status
	build.Phase = phase
	return nil
}

type replicaBuildClientFake struct {
	startCloneCalls    int
	startCloneLostOnce bool
	findRetained       string
	findErr            error
	cloneTaskErr       error
	validateCloneErr   error
	waitCleanupCalls   int
	startResidueCalls  int
	startCanaryCleanup int
	retainedExists     bool
	findCanary         string
}

func (f *replicaBuildClientFake) StartReplicaBuildClone(
	ctx context.Context,
	params vcenter.ReplicaBuildCloneParams,
	arm func(context.Context) error,
) (string, string, error) {
	if params.SourceSnapshotMoref != "snapshot-source" {
		return "", "", errors.New("clone submission did not use the persisted source snapshot")
	}
	f.startCloneCalls++
	if err := arm(ctx); err != nil {
		return "", "", err
	}
	if f.startCloneLostOnce && f.startCloneCalls == 1 {
		return "", "", errors.New("connection reset by peer")
	}
	return "task-clone", "snapshot-source", nil
}

func (f *replicaBuildClientFake) FindReplicaBuildVM(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) (string, error) {
	return f.findRetained, f.findErr
}

func (f *replicaBuildClientFake) WaitReplicaBuildCloneTask(
	_ context.Context,
	task string,
) (string, error) {
	if task == "task-clone" && f.cloneTaskErr != nil {
		return "", f.cloneTaskErr
	}
	if task == "task-canary" {
		return "vm-canary", nil
	}
	return "vm-retained", nil
}

func (f *replicaBuildClientFake) ValidateReplicaBuildClone(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) error {
	return f.validateCloneErr
}

func (f *replicaBuildClientFake) StartReplicaBuildSnapshot(
	ctx context.Context,
	_, _, _, _ string,
	arm func(context.Context) error,
) (string, error) {
	if err := arm(ctx); err != nil {
		return "", err
	}
	return "task-snapshot", nil
}

func (f *replicaBuildClientFake) FindReplicaSnapshot(
	_ context.Context,
	moref, _ string,
) (string, error) {
	if moref == "vm-source" {
		return "snapshot-source", nil
	}
	return "snapshot-retained", nil
}

func (f *replicaBuildClientFake) WaitReplicaBuildSnapshotTask(
	context.Context,
	string,
) (string, error) {
	return "snapshot-retained", nil
}

func (f *replicaBuildClientFake) StartReplicaBuildCanary(
	ctx context.Context,
	params vcenter.ReplicaBuildCloneParams,
	arm func(context.Context) error,
) (string, error) {
	if params.SourceVMMoref != "vm-retained" || params.DestinationVMMoref != "" {
		return "", errors.New("canary did not use retained VM as its source")
	}
	if err := arm(ctx); err != nil {
		return "", err
	}
	return "task-canary", nil
}

func (f *replicaBuildClientFake) FindReplicaBuildCanary(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) (string, error) {
	return f.findCanary, nil
}

func (f *replicaBuildClientFake) WaitReplicaBuildCleanupTask(
	context.Context,
	string,
) error {
	f.waitCleanupCalls++
	return nil
}

func (f *replicaBuildClientFake) ValidateReplicaBuildCanary(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) error {
	return nil
}

func (f *replicaBuildClientFake) StartReplicaBuildCanaryCleanup(
	ctx context.Context,
	_, _, _ string,
	arm func(context.Context) error,
) (string, bool, error) {
	f.startCanaryCleanup++
	if err := arm(ctx); err != nil {
		return "", false, err
	}
	return "task-cleanup", false, nil
}

func (f *replicaBuildClientFake) StartReplicaBuildResidueCleanup(
	ctx context.Context,
	_, _, _ string,
	arm func(context.Context) error,
) (string, bool, error) {
	f.startResidueCalls++
	if err := arm(ctx); err != nil {
		return "", false, err
	}
	return "task-residue", false, nil
}

func (f *replicaBuildClientFake) ReplicaBuildCanaryExists(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) (bool, error) {
	return false, nil
}

func (f *replicaBuildClientFake) ReplicaBuildRetainedVMExists(
	context.Context,
	vcenter.ReplicaBuildCloneParams,
) (bool, error) {
	return f.retainedExists, nil
}

func newReplicaBuildFixture() (*replicaBuildStoreFake, *models.Job) {
	jobID := uuid.New()
	templateID := uuid.New()
	buildID := uuid.New()
	sourceReplicaID := uuid.New()
	now := time.Now().UTC()
	build := &models.TemplateReplicaBuild{
		ID:                   buildID,
		TemplateID:           templateID,
		SourceReplicaID:      sourceReplicaID,
		JobID:                &jobID,
		OperationID:          uuid.NewString(),
		CanaryOperationID:    uuid.NewString(),
		SourceVMMoref:        "vm-source",
		SourceSnapshotName:   "base-image",
		DestinationName:      "retained-replica",
		ComputeResourceType:  "ClusterComputeResource",
		ComputeResourceMoref: "domain-c1",
		ComputeResourcePath:  "/dc/host/cluster",
		HostMoref:            "host-2",
		HostName:             "esxi2.example",
		ResourcePoolMoref:    "resgroup-2",
		ResourcePoolPath:     "/dc/host/cluster/Resources/Students",
		DatastoreMoref:       "datastore-2",
		DatastoreName:        "replica-ds",
		FolderMoref:          "group-v2",
		FolderPath:           "/dc/vm/templates",
		ProvisionDatastore:   "student-ds",
		Status:               models.TemplateReplicaBuildPending,
		Phase:                models.TemplateReplicaBuildPhasePending,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	payload := []byte(`{"build_id":"` + buildID.String() + `","template_id":"` + templateID.String() + `"}`)
	job := &models.Job{
		ID:         jobID,
		Type:       models.JobTypeTemplateReplicaBuild,
		Payload:    payload,
		Status:     models.JobStatusInProgress,
		MaxRetries: 20,
	}
	return &replicaBuildStoreFake{build: build}, job
}

func TestExecuteTemplateReplicaBuildRequiresCanaryCleanupBeforeReady(t *testing.T) {
	store, job := newReplicaBuildFixture()
	client := &replicaBuildClientFake{}

	if err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if !store.pendingReserved {
		t.Fatal("destination replica was not reserved pending before acceptance")
	}
	if !store.finalized || store.finalizedTooEarly {
		t.Fatalf("finalization = %t, too early = %t", store.finalized, store.finalizedTooEarly)
	}
	if client.waitCleanupCalls != 1 {
		t.Fatalf("cleanup task waits = %d, want 1", client.waitCleanupCalls)
	}
	if store.build.Status != models.TemplateReplicaBuildReady {
		t.Fatalf("build status = %s, want ready", store.build.Status)
	}
}

func TestExecuteTemplateReplicaBuildAcceptedResponseLostNeverResubmitsClone(t *testing.T) {
	store, job := newReplicaBuildFixture()
	client := &replicaBuildClientFake{
		startCloneLostOnce: true,
		findRetained:       "vm-retained",
	}

	err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-1")
	if err == nil || client.startCloneCalls != 1 {
		t.Fatalf("first run error = %v, clone submissions = %d", err, client.startCloneCalls)
	}
	if store.build.Phase != models.TemplateReplicaBuildPhaseCloneSubmitting {
		t.Fatalf("phase after lost response = %s, want clone_submitting", store.build.Phase)
	}
	if err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-2"); err != nil {
		t.Fatal(err)
	}
	if client.startCloneCalls != 1 {
		t.Fatalf("clone submissions after restart = %d, want exactly 1", client.startCloneCalls)
	}
}

func TestExecuteTemplateReplicaBuildTaskFailureRetainsExactCleanupIdentity(t *testing.T) {
	store, job := newReplicaBuildFixture()
	taskErr := fmt.Errorf("%w: sabotage", vcenter.ErrCloneTaskFailed)
	client := &replicaBuildClientFake{
		cloneTaskErr: taskErr,
		findRetained: "vm-retained",
	}

	err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-1")
	if !errors.Is(err, taskErr) {
		t.Fatalf("error = %v, want task failure", err)
	}
	if store.build.DestinationVMMoref != "vm-retained" || !store.pendingReserved {
		t.Fatalf("cleanup identity was not retained: build=%+v reserved=%t", store.build, store.pendingReserved)
	}
}

func TestExecuteTemplateReplicaBuildValidationMismatchNeverCreatesSnapshot(t *testing.T) {
	store, job := newReplicaBuildFixture()
	client := &replicaBuildClientFake{
		validateCloneErr: errors.New("firmware, Secure Boot, vTPM, or security provider differs"),
	}

	err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-1")
	if err == nil {
		t.Fatal("expected validation mismatch")
	}
	if store.build.SnapshotTaskRef != "" || store.finalized {
		t.Fatalf("validation failure advanced to snapshot/readiness: %+v", store.build)
	}
}

func TestExecuteTemplateReplicaBuildCollisionFailsClosedWithoutResubmission(t *testing.T) {
	store, job := newReplicaBuildFixture()
	store.build.Phase = models.TemplateReplicaBuildPhaseCloneSubmitting
	client := &replicaBuildClientFake{
		findErr: vcenter.ErrReplicaBuildAmbiguous,
	}

	err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-2")
	if !errors.Is(err, vcenter.ErrReplicaBuildAmbiguous) {
		t.Fatalf("collision error=%v, want ambiguity", err)
	}
	if client.startCloneCalls != 0 {
		t.Fatalf("collision triggered %d blind clone resubmissions", client.startCloneCalls)
	}
}

func TestExecuteTemplateReplicaBuildRecoversPendingReservationAtValidation(t *testing.T) {
	store, job := newReplicaBuildFixture()
	store.build.Phase = models.TemplateReplicaBuildPhaseValidating
	store.build.Status = models.TemplateReplicaBuildRunning
	store.build.DestinationVMMoref = "vm-retained"
	client := &replicaBuildClientFake{}

	if err := executeTemplateReplicaBuild(context.Background(), store, client, job, "worker-2"); err != nil {
		t.Fatal(err)
	}
	if !store.pendingReserved || store.build.ResultReplicaID == nil || !store.finalized {
		t.Fatalf("validation recovery did not restore pending replica before readiness: %+v", store)
	}
}

func TestCleanupTemplateReplicaBuildReconcilesLostDestroyWithoutResubmission(t *testing.T) {
	store, _ := newReplicaBuildFixture()
	store.build.Status = models.TemplateReplicaBuildCleanupRequired
	store.build.Phase = models.TemplateReplicaBuildPhaseResidueSubmitting
	store.build.ResumePhase = models.TemplateReplicaBuildPhaseValidating
	store.build.DestinationVMMoref = "vm-retained"
	now := time.Now().UTC()
	store.build.SubmissionStartedAt = &now
	client := &replicaBuildClientFake{retainedExists: false}

	if err := cleanupTemplateReplicaBuild(
		context.Background(),
		store,
		client,
		store.build,
		"worker-2",
	); err != nil {
		t.Fatal(err)
	}
	if client.startResidueCalls != 0 {
		t.Fatalf("ambiguous destroy reconciliation submitted %d duplicate destroy tasks", client.startResidueCalls)
	}
	if store.failedPhase != models.TemplateReplicaBuildPhaseResidueCleaned {
		t.Fatalf("cleanup terminal phase=%s, want residue_cleaned", store.failedPhase)
	}
}

func TestCleanupTemplateReplicaBuildNeverDeclaresArmedCloneAbsent(t *testing.T) {
	store, _ := newReplicaBuildFixture()
	store.build.Status = models.TemplateReplicaBuildCleanupRequired
	store.build.Phase = models.TemplateReplicaBuildPhaseCleanupRequired
	store.build.ResumePhase = models.TemplateReplicaBuildPhaseCloneSubmitting
	now := time.Now().UTC()
	store.build.SubmissionStartedAt = &now
	client := &replicaBuildClientFake{}

	err := cleanupTemplateReplicaBuild(
		context.Background(),
		store,
		client,
		store.build,
		"worker-2",
	)
	if err == nil || store.failedPhase != "" {
		t.Fatalf("armed clone cleanup error=%v terminal phase=%s, want reconciliation wait", err, store.failedPhase)
	}
}

func TestCleanupTemplateReplicaBuildReconcilesCanaryBeforeRetainedCleanup(t *testing.T) {
	store, _ := newReplicaBuildFixture()
	store.build.Status = models.TemplateReplicaBuildCleanupRequired
	store.build.Phase = models.TemplateReplicaBuildPhaseCanarySubmitting
	store.build.ResumePhase = models.TemplateReplicaBuildPhaseCanarySubmitting
	store.build.DestinationVMMoref = "vm-retained"
	now := time.Now().UTC()
	store.build.SubmissionStartedAt = &now
	client := &replicaBuildClientFake{findCanary: "vm-canary"}

	err := cleanupTemplateReplicaBuild(
		context.Background(),
		store,
		client,
		store.build,
		"worker-2",
	)
	if err == nil {
		t.Fatal("expected cleanup task reconciliation wait")
	}
	if store.build.CanaryVMMoref != "vm-canary" || client.startCanaryCleanup != 1 {
		t.Fatalf("canary reconciliation did not stage exact cleanup: build=%+v calls=%d", store.build, client.startCanaryCleanup)
	}
	if client.startResidueCalls != 0 {
		t.Fatalf("retained cleanup started before canary cleanup: %d calls", client.startResidueCalls)
	}
}

func TestCleanupTemplateReplicaBuildUsesResiduePreparedBeforeDestroySubmission(t *testing.T) {
	store, _ := newReplicaBuildFixture()
	store.build.Status = models.TemplateReplicaBuildCleanupRequired
	store.build.Phase = models.TemplateReplicaBuildPhaseCleanupSubmitted
	store.build.ResumePhase = models.TemplateReplicaBuildPhaseCleanupSubmitted
	store.build.DestinationVMMoref = "vm-retained"
	store.build.CanaryVMMoref = "vm-canary"
	store.build.CleanupTaskRef = "task-cleanup"
	client := &replicaBuildClientFake{}

	err := cleanupTemplateReplicaBuild(
		context.Background(),
		store,
		client,
		store.build,
		"worker-2",
	)
	if err == nil {
		t.Fatal("expected retained destroy reconciliation wait")
	}
	if client.startResidueCalls != 1 {
		t.Fatalf("retained destroy submissions=%d, want 1 after prepared transition", client.startResidueCalls)
	}
	if store.build.Phase != models.TemplateReplicaBuildPhaseResidueSubmitted {
		t.Fatalf("phase=%s, want residue_submitted", store.build.Phase)
	}
}
