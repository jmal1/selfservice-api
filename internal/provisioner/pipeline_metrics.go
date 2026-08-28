// Metrics for the template + image pipeline. Mirrors the design of
// DestroyFailedPusher and OrphanCountPusher: no prometheus client
// dependency, hand-serialized text exposition, pushed to Pushgateway,
// and a no-op when BaseURL is empty so the worker still does its job in
// environments without observability wired up.
//
// Namespaces:
//
//	crucible_image_*     — browser -> MinIO -> vCenter image staging
//	crucible_template_*  — the wizard lifecycle state machine
//
// Counter semantics note: this process keeps counters in memory and
// pushes their absolute value. A worker restart resets them to 0, which
// Prometheus handles natively as a counter reset, so rate()/increase()
// stay correct across restarts.
//
// Duration is exposed as a _sum/_count counter pair (a summary without
// quantiles) rather than a histogram, because hand-rolling bucket
// serialization is error-prone and the dashboards only need averages and
// throughput. Average = rate(_sum) / rate(_count).
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Result label values for the *_total counters.
const (
	MetricResultSuccess = "success"
	MetricResultError   = "error"
)

// PipelineMetrics accumulates image + template lifecycle counters and
// pushes the whole family on demand. Safe for concurrent use.
//
// Zero value is usable but inert (BaseURL empty => Push is a no-op);
// always construct via NewPipelineMetrics so the maps are non-nil.
type PipelineMetrics struct {
	// BaseURL is the Pushgateway root. Empty disables pushing.
	BaseURL string

	// Job is the Pushgateway {job} label, default crucible_pipeline.
	Job string

	// GroupingLabels are appended as /{key}/{value} pairs, sorted.
	GroupingLabels map[string]string

	// HTTP overrides the client; nil falls back to a 10s-timeout client.
	HTTP *http.Client

	mu sync.Mutex

	imageUploadTotal           map[string]float64 // kind|result
	imageImportTotal           map[string]float64 // kind|result
	imageImportDurSum          map[string]float64 // kind
	imageImportDurCount        map[string]float64 // kind
	imageImportBytes           map[string]float64 // kind
	imageUploadsStuck          float64
	imageUploadsStuckCollected bool

	templateTransitions    map[string]float64 // from|to
	templateVerify         map[string]float64 // result
	templateJobDurSum      map[string]float64 // job_type
	templateJobDurCount    map[string]float64 // job_type
	templateStates         map[string]float64 // state
	templateStuck          float64
	templateStuckCollected bool

	// Job retry metrics (migration 000026).
	jobRetries               map[string]float64 // type|reason
	jobRetryExhausted        map[string]float64 // type
	jobRetryPending          float64            // gauge: jobs sleeping between retries
	jobRetryPendingCollected bool               // true once SetJobRetryPending has run

	// L1 trust-tier revalidation metrics (migration 000027).
	templateValidation             map[string]float64 // template_id|result — counter
	templateLastValidated          map[string]float64 // template_id — gauge (unix seconds)
	templateLastValidatedCollected bool               // true once SetTemplateLastValidated has run

	vmPlacementTotal      map[string]float64 // host|compute|source
	vmPlacementHeadroomMB map[string]float64 // host
	vmPlacementDrift      map[string]float64 // kind
	vmPlacementRejections map[string]float64 // reason

	templateReplicaBuildTotal       map[string]float64 // result
	templateReplicaBuildDurSum      map[string]float64 // result
	templateReplicaBuildDurCount    map[string]float64 // result
	templateReplicaBuildPhases      map[string]float64 // phase
	templateReplicaBuildLastSuccess float64
	templateReplicaBuildLastFailure float64
	templateReplicaBuildsStuck      float64
	templateReplicaBuildsCollected  bool
}

// NewPipelineMetrics returns an initialized collector. baseURL may be
// empty, in which case Push() is a no-op but all Record* calls remain
// safe (useful in tests and in the API process).
func NewPipelineMetrics(baseURL, job string, grouping map[string]string) *PipelineMetrics {
	if job == "" {
		job = "crucible_pipeline"
	}
	return &PipelineMetrics{
		BaseURL:                      baseURL,
		Job:                          job,
		GroupingLabels:               grouping,
		imageUploadTotal:             map[string]float64{},
		imageImportTotal:             map[string]float64{},
		imageImportDurSum:            map[string]float64{},
		imageImportDurCount:          map[string]float64{},
		imageImportBytes:             map[string]float64{},
		templateTransitions:          map[string]float64{},
		templateVerify:               map[string]float64{},
		templateJobDurSum:            map[string]float64{},
		templateJobDurCount:          map[string]float64{},
		templateStates:               map[string]float64{},
		jobRetries:                   map[string]float64{},
		jobRetryExhausted:            map[string]float64{},
		templateValidation:           map[string]float64{},
		templateLastValidated:        map[string]float64{},
		vmPlacementTotal:             map[string]float64{},
		vmPlacementHeadroomMB:        map[string]float64{},
		vmPlacementDrift:             map[string]float64{},
		vmPlacementRejections:        map[string]float64{},
		templateReplicaBuildTotal:    map[string]float64{},
		templateReplicaBuildDurSum:   map[string]float64{},
		templateReplicaBuildDurCount: map[string]float64{},
		templateReplicaBuildPhases:   map[string]float64{},
	}
}

// RecordImageUpload counts an upload-lifecycle event. result is one of
// "created", "completed", "failed".
func (m *PipelineMetrics) RecordImageUpload(kind, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageUploadTotal[kind+"|"+result]++
}

// RecordImageImport counts a finished import job and its duration/bytes.
// result is MetricResultSuccess or MetricResultError.
func (m *PipelineMetrics) RecordImageImport(kind, result string, d time.Duration, bytesMoved int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageImportTotal[kind+"|"+result]++
	m.imageImportDurSum[kind] += d.Seconds()
	m.imageImportDurCount[kind]++
	if bytesMoved > 0 {
		m.imageImportBytes[kind] += float64(bytesMoved)
	}
}

// SetImageUploadsStuck sets the gauge of uploads wedged in a
// non-terminal state. Refreshed by the worker reconcile loop.
func (m *PipelineMetrics) SetImageUploadsStuck(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imageUploadsStuck = float64(n)
	m.imageUploadsStuckCollected = true
}

// RecordTemplateTransition counts a lifecycle state change. `to="error"`
// is the alertable series.
func (m *PipelineMetrics) RecordTemplateTransition(from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateTransitions[from+"|"+to]++
}

// RecordTemplateVerify counts an anti-brick gate outcome ("pass"/"fail").
func (m *PipelineMetrics) RecordTemplateVerify(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateVerify[result]++
}

// RecordTemplateJob records a finished template job's wall-clock, keyed
// by models.JobTypeTemplate* / JobTypeImageImport.
func (m *PipelineMetrics) RecordTemplateJob(jobType string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateJobDurSum[jobType] += d.Seconds()
	m.templateJobDurCount[jobType]++
}

// SetTemplateStates replaces the per-state template census. Cardinality
// is bounded by the number of lifecycle states (not templates) on
// purpose — a per-template label would be unbounded over time.
func (m *PipelineMetrics) SetTemplateStates(counts map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateStates = make(map[string]float64, len(counts))
	for state, n := range counts {
		m.templateStates[state] = float64(n)
	}
}

// SetTemplatesStuck sets the count of templates sitting in a
// non-terminal lifecycle state past the staleness threshold.
func (m *PipelineMetrics) SetTemplatesStuck(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateStuck = float64(n)
	m.templateStuckCollected = true
}

// RecordJobRetry increments the retry counter for a job type and reason.
// Called from processJobLifecycle whenever a retryable failure is rescheduled.
// jobType is one of models.JobType*; reason is one of RetryReason*.
func (m *PipelineMetrics) RecordJobRetry(jobType, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobRetries[jobType+"|"+reason]++
}

// RecordJobRetryExhausted increments the exhausted counter: the job reached
// max_retries and entered terminal 'failed' status.
// Called from processJobLifecycle after the last allowed attempt fails.
func (m *PipelineMetrics) RecordJobRetryExhausted(jobType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobRetryExhausted[jobType]++
}

// SetJobRetryPending sets the gauge of jobs currently sleeping between retry
// attempts. Refreshed by the retry-pending reconciler via CountRetryPendingJobs.
// The gauge is emitted by serialize only after this method has been called at
// least once — an uninitialized gauge is omitted entirely rather than
// scraping as 0 (which would be indistinguishable from "nothing is waiting").
func (m *PipelineMetrics) SetJobRetryPending(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobRetryPending = float64(n)
	m.jobRetryPendingCollected = true
}

// RecordTemplateValidation counts a completed L1 revalidation run.
// result is "pass" or "fail". Called by RevalidateL1Template on every run
// so failures appear as a rising counter that can alert.
func (m *PipelineMetrics) RecordTemplateValidation(templateID, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateValidation[templateID+"|"+result]++
}

// SetTemplateLastValidated records the Unix timestamp of the most recent
// completed validation run for a template (pass or fail). Called by the L1
// trust-validation reconciler after each pass so the staleness gauge stays
// accurate. The gauge family is omitted entirely until this method has been
// called at least once — see the Collected pattern for imageUploadsStuck.
func (m *PipelineMetrics) SetTemplateLastValidated(templateID string, unixSec float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateLastValidated[templateID] = unixSec
	m.templateLastValidatedCollected = true
}

func (m *PipelineMetrics) RecordVMPlacement(host, compute, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vmPlacementTotal[host+"|"+compute+"|"+source]++
}

func (m *PipelineMetrics) RecordTemplateReplicaBuild(result string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateReplicaBuildTotal[result]++
	m.templateReplicaBuildDurSum[result] += d.Seconds()
	m.templateReplicaBuildDurCount[result]++
	now := float64(time.Now().Unix())
	if result == MetricResultSuccess {
		m.templateReplicaBuildLastSuccess = now
	} else {
		m.templateReplicaBuildLastFailure = now
	}
}

func (m *PipelineMetrics) SetTemplateReplicaBuildPhases(counts map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateReplicaBuildPhases = make(map[string]float64, len(counts))
	for phase, count := range counts {
		m.templateReplicaBuildPhases[phase] = float64(count)
	}
	m.templateReplicaBuildsCollected = true
}

func (m *PipelineMetrics) SetTemplateReplicaBuildsStuck(count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.templateReplicaBuildsStuck = float64(count)
	m.templateReplicaBuildsCollected = true
}

func (m *PipelineMetrics) SetVMPlacementHeadroom(host string, megabytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vmPlacementHeadroomMB[host] = float64(megabytes)
}

func (m *PipelineMetrics) RecordVMPlacementDrift(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vmPlacementDrift[kind]++
}

func (m *PipelineMetrics) RecordVMPlacementRejection(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vmPlacementRejections[reason]++
}

// Push serializes the current values and POSTs them. No-op when BaseURL
// is empty.
func (m *PipelineMetrics) Push(ctx context.Context) error {
	if m.BaseURL == "" {
		return nil
	}
	body := m.serialize()
	target := pipelinePushURL(m.BaseURL, m.Job, m.GroupingLabels)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

func (m *PipelineMetrics) serialize() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b bytes.Buffer

	writeCounter2(&b, "crucible_image_upload_total",
		"Image upload lifecycle events by kind and result.",
		"kind", "result", m.imageUploadTotal)

	writeCounter2(&b, "crucible_image_import_total",
		"Image import job outcomes by kind and result.",
		"kind", "result", m.imageImportTotal)

	writeCounter1(&b, "crucible_image_import_duration_seconds_sum",
		"Cumulative wall-clock seconds spent importing images, by kind.",
		"kind", m.imageImportDurSum)
	writeCounter1(&b, "crucible_image_import_duration_seconds_count",
		"Number of completed image imports, by kind. Average = rate(sum)/rate(count).",
		"kind", m.imageImportDurCount)

	writeCounter1(&b, "crucible_image_import_bytes_total",
		"Cumulative bytes streamed from object storage into vCenter, by kind.",
		"kind", m.imageImportBytes)

	if m.imageUploadsStuckCollected {
		b.WriteString("# HELP crucible_image_uploads_stuck Staged image uploads wedged in a non-terminal state past the staleness threshold.\n")
		b.WriteString("# TYPE crucible_image_uploads_stuck gauge\n")
		fmt.Fprintf(&b, "crucible_image_uploads_stuck %g\n", m.imageUploadsStuck)
	}

	writeCounter2(&b, "crucible_template_transition_total",
		"Template lifecycle state transitions.",
		"from", "to", m.templateTransitions)

	writeCounter1(&b, "crucible_template_verify_result_total",
		"Template verify-gate outcomes (pass/fail).",
		"result", m.templateVerify)

	writeCounter1(&b, "crucible_template_job_duration_seconds_sum",
		"Cumulative wall-clock seconds per template job type.",
		"job_type", m.templateJobDurSum)
	writeCounter1(&b, "crucible_template_job_duration_seconds_count",
		"Completed template jobs per type. Average = rate(sum)/rate(count).",
		"job_type", m.templateJobDurCount)

	writeGauge1(&b, "crucible_template_state",
		"Number of templates currently in each lifecycle state.",
		"state", m.templateStates)

	if m.templateStuckCollected {
		b.WriteString("# HELP crucible_template_stuck Templates sitting in a non-terminal lifecycle state past the staleness threshold.\n")
		b.WriteString("# TYPE crucible_template_stuck gauge\n")
		fmt.Fprintf(&b, "crucible_template_stuck %g\n", m.templateStuck)
	}

	writeCounter2(&b, "crucible_job_retries_total",
		"Job retry attempts by job type and reason (transient_clone, connection, timeout, unavailable).",
		"type", "reason", m.jobRetries)

	writeCounter1(&b, "crucible_job_retry_exhausted_total",
		"Jobs that reached max_retries and entered terminal failed status, by type.",
		"type", m.jobRetryExhausted)

	// Omit the gauge entirely until the reconciler has run at least once.
	// An unset gauge reads as 0, which is indistinguishable from "nothing is
	// waiting to retry" and cannot fire an alert — exactly the permanently-zero
	// lie we are guarding against (see Collected pattern for the stuck gauges).
	if m.jobRetryPendingCollected {
		b.WriteString("# HELP crucible_job_retry_pending Jobs currently sleeping between retry attempts (next_attempt_at > now()).\n")
		b.WriteString("# TYPE crucible_job_retry_pending gauge\n")
		fmt.Fprintf(&b, "crucible_job_retry_pending %g\n", m.jobRetryPending)
	}

	writeCounter2(&b, "crucible_template_validation_total",
		"Template credential revalidation outcomes by template_id and result (pass/fail).",
		"template_id", "result", m.templateValidation)

	// Omit the staleness gauge family until the L1 trust-validation reconciler
	// has run at least once. Before that, an absent series is honest ("never
	// validated") rather than a false "0 = recently validated". Matches the
	// Collected pattern used by imageUploadsStuck and jobRetryPending above.
	if m.templateLastValidatedCollected {
		writeGauge1(&b, "crucible_template_last_validated_timestamp",
			"Unix timestamp of the most recent completed L1 validation run per template (pass or fail). Alert when now()-value exceeds the validation interval.",
			"template_id", m.templateLastValidated)
	}

	writeCounter3(&b, "crucible_vm_placement_total",
		"Durable VM placements by target host, compute resource, and source replica.",
		"host", "compute", "source", m.vmPlacementTotal)
	writeGauge1(&b, "crucible_vm_placement_headroom_megabytes",
		"Free host memory remaining after planned VM memory and configured reserve.",
		"host", m.vmPlacementHeadroomMB)
	writeCounter1(&b, "crucible_vm_placement_drift_total",
		"Detected immutable placement or DRS-control drift.",
		"kind", m.vmPlacementDrift)
	writeCounter1(&b, "crucible_vm_placement_rejections_total",
		"Rejected VM placements by reason.",
		"reason", m.vmPlacementRejections)

	writeCounter1(&b, "crucible_template_replica_build_total",
		"Retained template source replica build outcomes.",
		"result", m.templateReplicaBuildTotal)
	writeCounter1(&b, "crucible_template_replica_build_duration_seconds_sum",
		"Cumulative wall-clock seconds spent in retained replica builds.",
		"result", m.templateReplicaBuildDurSum)
	writeCounter1(&b, "crucible_template_replica_build_duration_seconds_count",
		"Completed retained replica builds by result.",
		"result", m.templateReplicaBuildDurCount)
	if m.templateReplicaBuildsCollected {
		writeGauge1(&b, "crucible_template_replica_build_phase",
			"Current retained replica build operations by durable phase.",
			"phase", m.templateReplicaBuildPhases)
		b.WriteString("# HELP crucible_template_replica_build_stuck Retained replica build operations with no durable phase update past the staleness threshold.\n")
		b.WriteString("# TYPE crucible_template_replica_build_stuck gauge\n")
		fmt.Fprintf(&b, "crucible_template_replica_build_stuck %g\n", m.templateReplicaBuildsStuck)
	}
	b.WriteString("# HELP crucible_template_replica_build_last_success_timestamp_seconds Unix time of the latest successful retained replica build.\n")
	b.WriteString("# TYPE crucible_template_replica_build_last_success_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_template_replica_build_last_success_timestamp_seconds %g\n", m.templateReplicaBuildLastSuccess)
	b.WriteString("# HELP crucible_template_replica_build_last_failure_timestamp_seconds Unix time of the latest failed retained replica build attempt.\n")
	b.WriteString("# TYPE crucible_template_replica_build_last_failure_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_template_replica_build_last_failure_timestamp_seconds %g\n", m.templateReplicaBuildLastFailure)

	b.WriteString("# HELP crucible_pipeline_run_timestamp_seconds Unix time of the latest pipeline metrics push.\n")
	b.WriteString("# TYPE crucible_pipeline_run_timestamp_seconds gauge\n")
	fmt.Fprintf(&b, "crucible_pipeline_run_timestamp_seconds %d\n", time.Now().Unix())

	return b.Bytes()
}

// writeCounter1 emits a counter family keyed by a single label. Keys are
// sorted so the exposition is byte-stable (makes tests deterministic).
func writeCounter1(b *bytes.Buffer, name, help, label string, vals map[string]float64) {
	writeFamily(b, name, help, "counter", []string{label}, vals)
}

// writeCounter2 emits a counter family whose map keys are "a|b".
func writeCounter2(b *bytes.Buffer, name, help, l1, l2 string, vals map[string]float64) {
	writeFamily(b, name, help, "counter", []string{l1, l2}, vals)
}

func writeCounter3(b *bytes.Buffer, name, help, l1, l2, l3 string, vals map[string]float64) {
	writeFamily(b, name, help, "counter", []string{l1, l2, l3}, vals)
}

func writeGauge1(b *bytes.Buffer, name, help, label string, vals map[string]float64) {
	writeFamily(b, name, help, "gauge", []string{label}, vals)
}

func writeFamily(b *bytes.Buffer, name, help, typ string, labels []string, vals map[string]float64) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)
	if len(vals) == 0 {
		return
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.Split(k, "|")
		var lb strings.Builder
		lb.WriteString("{")
		for i, l := range labels {
			if i > 0 {
				lb.WriteString(",")
			}
			v := ""
			if i < len(parts) {
				v = parts[i]
			}
			fmt.Fprintf(&lb, `%s="%s"`, l, escapeLabelValue(v))
		}
		lb.WriteString("}")
		fmt.Fprintf(b, "%s%s %g\n", name, lb.String(), vals[k])
	}
}

// escapeLabelValue escapes the three characters the text exposition
// format requires escaping inside a label value.
func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func pipelinePushURL(base, job string, grouping map[string]string) string {
	// Reuses the same URL shape as the sibling pushers in this package.
	return destroyFailedPushURL(base, job, grouping)
}

// RunPusher flushes the accumulated metric families to Pushgateway every
// interval until ctx is cancelled.
//
// This loop is what makes every Record*/Set* call above observable. Those
// methods only mutate in-process counters; nothing reaches Prometheus until
// Push serializes and POSTs them. Without this loop the whole family is
// silently absent from Prometheus, and an absent series renders on a
// dashboard exactly like a zero one -- "no image imports failed" and "the
// importer has never run once" look identical. Recording without flushing is
// therefore worse than having no metric at all, because it looks healthy.
//
// A push failure is logged and retried on the next tick rather than being
// fatal: Pushgateway being down must not take the worker down with it.
func (m *PipelineMetrics) RunPusher(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	if m == nil || m.BaseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	log := logger.With("component", "pipeline_metrics_pusher")
	log.Info("pipeline metrics pusher started", "interval", interval, "job", m.Job)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush so counters accumulated since the last tick are not
			// lost on a normal shutdown or rolling restart.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := m.Push(flushCtx); err != nil {
				log.Warn("final pipeline metrics push failed", "error", err)
			}
			cancel()
			log.Info("pipeline metrics pusher stopped")
			return
		case <-ticker.C:
			if err := m.Push(ctx); err != nil {
				log.Warn("pipeline metrics push failed", "error", err)
			}
		}
	}
}
