package provisioner

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

type pipelineMetricsSpy struct {
	imageImports        []string
	imageUploads        []string
	templateTransitions []string
	templateVerifies    []string
	templateJobs        []string
	templateStates      []map[string]int
	templateStucks      []int
	pushes              int
	pushErr             error
}

func (s *pipelineMetricsSpy) RecordImageImport(kind, result string, d time.Duration, bytesMoved int64) {
	s.imageImports = append(s.imageImports, kind+"|"+result)
}

func (s *pipelineMetricsSpy) SetImageUploadsStuck(n int) {
	s.imageUploads = append(s.imageUploads, strconv.Itoa(n))
}

func (s *pipelineMetricsSpy) RecordTemplateTransition(from, to string) {
	s.templateTransitions = append(s.templateTransitions, from+"|"+to)
}

func (s *pipelineMetricsSpy) RecordTemplateVerify(result string) {
	s.templateVerifies = append(s.templateVerifies, result)
}

func (s *pipelineMetricsSpy) RecordTemplateJob(jobType string, d time.Duration) {
	s.templateJobs = append(s.templateJobs, jobType)
}

func (s *pipelineMetricsSpy) SetTemplateStates(counts map[string]int) {
	clone := make(map[string]int, len(counts))
	for k, v := range counts {
		clone[k] = v
	}
	s.templateStates = append(s.templateStates, clone)
}

func (s *pipelineMetricsSpy) SetTemplatesStuck(n int) {
	s.templateStucks = append(s.templateStucks, n)
}

func (s *pipelineMetricsSpy) Push(context.Context) error {
	s.pushes++
	return s.pushErr
}

// Retry metric stubs — satisfy pipelineMetricsSink; not asserted in these tests.
func (s *pipelineMetricsSpy) RecordJobRetry(_, _ string)       {}
func (s *pipelineMetricsSpy) RecordJobRetryExhausted(_ string) {}
func (s *pipelineMetricsSpy) SetJobRetryPending(_ int)         {}

// L1 trust-tier validation stubs — satisfy pipelineMetricsSink.
func (s *pipelineMetricsSpy) RecordTemplateValidation(_, _ string)         {}
func (s *pipelineMetricsSpy) SetTemplateLastValidated(_ string, _ float64) {}

var _ pipelineMetricsSink = (*pipelineMetricsSpy)(nil)
var _ templateReconcileMetrics = (*pipelineMetricsSpy)(nil)

type fakeTemplateMetricsDB struct {
	states        map[string]int
	stateErr      error
	stuck         int
	stuckErr      error
	stateCalls    int
	stuckCalls    int
	lastOlderThan time.Duration
}

func (f *fakeTemplateMetricsDB) CountTemplateStates(_ context.Context) (map[string]int, error) {
	f.stateCalls++
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	out := make(map[string]int, len(f.states))
	for k, v := range f.states {
		out[k] = v
	}
	return out, nil
}

func (f *fakeTemplateMetricsDB) CountStuckTemplates(_ context.Context, olderThan time.Duration) (int, error) {
	f.stuckCalls++
	f.lastOlderThan = olderThan
	if f.stuckErr != nil {
		return 0, f.stuckErr
	}
	return f.stuck, nil
}

type fakeJobStatusDB struct {
	updates []string
	err     error
}

func (f *fakeJobStatusDB) UpdateJobStatus(
	_ context.Context,
	_ uuid.UUID,
	_ string,
	status string,
	_ []byte,
) error {
	f.updates = append(f.updates, status)
	return f.err
}

// RetryJob stub — satisfies jobStatusUpdater; not asserted in these tests.
func (f *fakeJobStatusDB) RetryJob(
	_ context.Context,
	_ uuid.UUID,
	_ time.Time,
	_ bool,
	_ []byte,
	_ string,
) error {
	return nil
}

var _ jobStatusUpdater = (*fakeJobStatusDB)(nil)

type fakeTransitionMetricsDB struct {
	tmpl      *models.Template
	updates   []string
	updateErr error
}

func (f *fakeTransitionMetricsDB) GetTemplateByID(_ context.Context, _ uuid.UUID) (*models.Template, error) {
	return f.tmpl, nil
}

func (f *fakeTransitionMetricsDB) SetTemplateVCenterVM(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}

func (f *fakeTransitionMetricsDB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, from, to string) error {
	f.updates = append(f.updates, from+"->"+to)
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.tmpl != nil {
		if f.tmpl.TemplateState != from {
			return errors.New("stale transition: have " + f.tmpl.TemplateState + " want " + from)
		}
		f.tmpl.TemplateState = to
	}
	return nil
}

var _ isoProvisionDB = (*fakeTransitionMetricsDB)(nil)

func TestReconcileTemplateMetrics_PublishesZeroCounts(t *testing.T) {
	db := &fakeTemplateMetricsDB{
		states: map[string]int{
			models.TemplateStateDraft:        0,
			models.TemplateStateConfiguring:  2,
			models.TemplateStateGeneralizing: 0,
			models.TemplateStateReady:        4,
			models.TemplateStateVerifying:    0,
			models.TemplateStateActive:       1,
			models.TemplateStateError:        0,
		},
		stuck: 0,
	}
	metrics := &pipelineMetricsSpy{}

	counts, err := reconcileTemplateMetrics(context.Background(), db, metrics, discardLogger(), TemplateReconcilerConfig{StaleThreshold: 30 * time.Minute})
	if err != nil {
		t.Fatalf("reconcileTemplateMetrics: %v", err)
	}
	if counts.Templates != 7 {
		t.Fatalf("template count = %d, want 7", counts.Templates)
	}
	if counts.Stuck != 0 {
		t.Fatalf("stuck count = %d, want 0", counts.Stuck)
	}
	if db.stateCalls != 1 || db.stuckCalls != 1 {
		t.Fatalf("db calls = states:%d stuck:%d, want 1 each", db.stateCalls, db.stuckCalls)
	}
	if db.lastOlderThan != 30*time.Minute {
		t.Fatalf("stale threshold = %s, want 30m", db.lastOlderThan)
	}
	if len(metrics.templateStates) != 1 {
		t.Fatalf("SetTemplateStates calls = %d, want 1", len(metrics.templateStates))
	}
	if got := metrics.templateStates[0][models.TemplateStateReady]; got != 4 {
		t.Fatalf("ready count = %d, want 4", got)
	}
	if len(metrics.templateStucks) != 1 || metrics.templateStucks[0] != 0 {
		t.Fatalf("SetTemplatesStuck calls = %v, want [0]", metrics.templateStucks)
	}
	if metrics.pushes != 1 {
		t.Fatalf("Push calls = %d, want 1", metrics.pushes)
	}
}

func TestReconcileTemplateMetrics_QueryErrorSkipsMetrics(t *testing.T) {
	db := &fakeTemplateMetricsDB{stateErr: errors.New("db down")}
	metrics := &pipelineMetricsSpy{}

	_, err := reconcileTemplateMetrics(context.Background(), db, metrics, discardLogger(), TemplateReconcilerConfig{StaleThreshold: 30 * time.Minute})
	if err == nil {
		t.Fatal("expected error when state census fails, got nil")
	}
	if db.stateCalls != 1 || db.stuckCalls != 0 {
		t.Fatalf("db calls = states:%d stuck:%d, want states=1 stuck=0", db.stateCalls, db.stuckCalls)
	}
	if len(metrics.templateStates) != 0 || len(metrics.templateStucks) != 0 || metrics.pushes != 0 {
		t.Fatalf("metrics should not be published on query error: %#v", metrics)
	}
}

func TestVerifyTemplate_RecordsFailureMetric(t *testing.T) {
	metrics := &pipelineMetricsSpy{}
	p := &Provisioner{pipeline: metrics, logger: discardLogger()}

	job := &models.Job{ID: uuid.New(), Type: models.JobTypeTemplateVerify, Payload: []byte(`{}`)}
	if err := p.VerifyTemplate(context.Background(), job); err == nil {
		t.Fatal("expected VerifyTemplate to fail with an empty payload, got nil")
	}
	if len(metrics.templateVerifies) != 1 || metrics.templateVerifies[0] != "fail" {
		t.Fatalf("verify metrics = %v, want [fail]", metrics.templateVerifies)
	}
}

func TestProcessJobLifecycle_RecordsTemplateJobDuration(t *testing.T) {
	db := &fakeJobStatusDB{}
	metrics := &pipelineMetricsSpy{}
	job := &models.Job{ID: uuid.New(), Type: models.JobTypeTemplateVerify}
	claimTestJob(job)

	err := processJobLifecycle(context.Background(), db, metrics, job, nil, func(context.Context, *models.Job) error { return nil })
	if err != nil {
		t.Fatalf("processJobLifecycle: %v", err)
	}
	if got, want := db.updates, []string{models.JobStatusInProgress, models.JobStatusCompleted}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("status updates = %v, want %v", got, want)
	}
	if len(metrics.templateJobs) != 1 || metrics.templateJobs[0] != models.JobTypeTemplateVerify {
		t.Fatalf("template job metrics = %v, want [%s]", metrics.templateJobs, models.JobTypeTemplateVerify)
	}
}

func TestTransitionTemplateViaDB_RecordsTransitionMetric(t *testing.T) {
	db := &fakeTransitionMetricsDB{
		tmpl: &models.Template{TemplateState: models.TemplateStateProvisioning},
	}
	metrics := &pipelineMetricsSpy{}

	if err := transitionTemplateViaDB(context.Background(), db, metrics, uuid.New(), models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		t.Fatalf("transitionTemplateViaDB: %v", err)
	}
	if len(db.updates) != 1 || db.updates[0] != models.TemplateStateProvisioning+"->"+models.TemplateStateConfiguring {
		t.Fatalf("db transitions = %v, want [provisioning->configuring]", db.updates)
	}
	if len(metrics.templateTransitions) != 1 || metrics.templateTransitions[0] != models.TemplateStateProvisioning+"|"+models.TemplateStateConfiguring {
		t.Fatalf("transition metrics = %v, want [provisioning|configuring]", metrics.templateTransitions)
	}
}

func TestMarkTemplateErrorViaDB_RecordsTransitionMetric(t *testing.T) {
	db := &fakeTransitionMetricsDB{
		tmpl: &models.Template{TemplateState: models.TemplateStateProvisioning},
	}
	metrics := &pipelineMetricsSpy{}

	cause := errors.New("boom")
	if err := markTemplateErrorViaDB(context.Background(), db, metrics, discardLogger(), uuid.New(), cause); !errors.Is(err, cause) {
		t.Fatalf("markTemplateErrorViaDB: %v", err)
	}
	if len(db.updates) != 1 || db.updates[0] != models.TemplateStateProvisioning+"->"+models.TemplateStateError {
		t.Fatalf("db transitions = %v, want [provisioning->error]", db.updates)
	}
	if len(metrics.templateTransitions) != 1 || metrics.templateTransitions[0] != models.TemplateStateProvisioning+"|"+models.TemplateStateError {
		t.Fatalf("transition metrics = %v, want [provisioning|error]", metrics.templateTransitions)
	}
}
