// template_health_metrics.go — replaceable Pushgateway snapshots for template health.
package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/jmal1/selfservice-api/internal/database"
)

// TemplateHealthSnapshot is built entirely from persisted state. It is safe to
// replace the Pushgateway grouping key because it contains every currently
// student-visible template; removed templates and obsolete label sets vanish.
type TemplateHealthSnapshot struct {
	CheckerUp *bool
	States    []database.TemplateHealthState
	CreatedAt time.Time
}

type TemplateHealthPusher struct {
	BaseURL        string
	Job            string
	GroupingLabels map[string]string
	HTTP           *http.Client
}

func NewTemplateHealthPusher(baseURL, job string, grouping map[string]string) *TemplateHealthPusher {
	if job == "" {
		job = "crucible_provision_worker"
	}
	return &TemplateHealthPusher{
		BaseURL:        baseURL,
		Job:            job,
		GroupingLabels: grouping,
	}
}

// ReplaceSnapshot uses Pushgateway PUT semantics. POST merges metric families
// and would retain deleted templates and old check_type series indefinitely.
func (p *TemplateHealthPusher) ReplaceSnapshot(ctx context.Context, snapshot TemplateHealthSnapshot) error {
	if p.BaseURL == "" {
		return nil
	}
	body := serializeTemplateHealthSnapshot(snapshot)
	target := destroyFailedPushURL(p.BaseURL, p.Job,
		mergeGrouping(p.GroupingLabels, "component", "template_health"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4")
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("replace pushgateway snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("pushgateway %d: %s", resp.StatusCode, string(buf))
	}
	return nil
}

func serializeTemplateHealthSnapshot(snapshot TemplateHealthSnapshot) []byte {
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now()
	}

	var b bytes.Buffer
	if snapshot.CheckerUp != nil {
		fmt.Fprintln(&b, "# HELP crucible_template_health_checker_up 1 when the latest reconciliation reached vCenter successfully; 0 for checker-level infrastructure failure.")
		fmt.Fprintln(&b, "# TYPE crucible_template_health_checker_up gauge")
		fmt.Fprintf(&b, "crucible_template_health_checker_up %d\n", boolMetric(*snapshot.CheckerUp))
	}

	fmt.Fprintln(&b, "# HELP crucible_template_health_status Persisted confirmed template health: 1 healthy, 0 unhealthy, -1 unknown. Raw attempts never set this metric directly.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_status gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_consecutive_failures Maximum persisted confirmed/pending failure count across check types.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_consecutive_failures gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_attempt_status Last persisted raw attempt outcome by check type: 1 passed, 0 failed.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_attempt_status gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_last_check_timestamp_seconds Unix timestamp of the last persisted raw attempt by template and check type.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_last_check_timestamp_seconds gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_last_check_duration_seconds Wall-clock duration of the last persisted raw attempt by template and check type.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_last_check_duration_seconds gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_fault_info Classified fault for the last failed raw attempt; full fault text remains in PostgreSQL and logs.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_fault_info gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_deep_confirmation_pending 1 while a first failed deep attempt awaits an independent confirmation job.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_deep_confirmation_pending gauge")
	fmt.Fprintln(&b, "# HELP crucible_template_health_deep_confirmation_due_timestamp_seconds Unix timestamp when the pending deep confirmation becomes claimable.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_deep_confirmation_due_timestamp_seconds gauge")

	sort.Slice(snapshot.States, func(i, j int) bool {
		return snapshot.States[i].TemplateName < snapshot.States[j].TemplateName
	})
	var checkerLastSuccess *time.Time
	checkerSnapshotComplete := len(snapshot.States) > 0
	for _, state := range snapshot.States {
		fmt.Fprintf(&b, "crucible_template_health_status{template=%q} %g\n",
			state.TemplateName, confirmedHealthValue(state.HealthStatus))
		fmt.Fprintf(&b, "crucible_template_health_consecutive_failures{template=%q} %d\n",
			state.TemplateName, state.ConsecutiveFailures)
		writeAttemptMetrics(&b, state.TemplateName, "structural",
			state.LastStructuralCheckAt, state.LastStructuralPassed,
			state.LastStructuralDurationSeconds, state.LastStructuralFaultClass)
		writeAttemptMetrics(&b, state.TemplateName, "deep",
			state.LastDeepCheckAt, state.LastDeepPassed,
			state.LastDeepDurationSeconds, state.LastDeepFaultClass)

		pending := state.PendingDeepFailureAt != nil
		fmt.Fprintf(&b, "crucible_template_health_deep_confirmation_pending{template=%q} %d\n",
			state.TemplateName, boolMetric(pending))
		if state.DeepConfirmationDueAt != nil {
			fmt.Fprintf(&b, "crucible_template_health_deep_confirmation_due_timestamp_seconds{template=%q} %d\n",
				state.TemplateName, state.DeepConfirmationDueAt.Unix())
		}
		if state.LastStructuralCheckAt == nil {
			checkerSnapshotComplete = false
		} else if checkerLastSuccess == nil || state.LastStructuralCheckAt.Before(*checkerLastSuccess) {
			at := *state.LastStructuralCheckAt
			checkerLastSuccess = &at
		}
	}

	if checkerSnapshotComplete && checkerLastSuccess != nil {
		fmt.Fprintln(&b, "# HELP crucible_template_health_checker_last_success_timestamp_seconds Conservative Unix time proving every currently visible template has a persisted structural result at least this fresh.")
		fmt.Fprintln(&b, "# TYPE crucible_template_health_checker_last_success_timestamp_seconds gauge")
		fmt.Fprintf(&b, "crucible_template_health_checker_last_success_timestamp_seconds %d\n", checkerLastSuccess.Unix())
	}
	fmt.Fprintln(&b, "# HELP crucible_template_health_snapshot_timestamp_seconds Unix time when this complete persisted-state snapshot was generated.")
	fmt.Fprintln(&b, "# TYPE crucible_template_health_snapshot_timestamp_seconds gauge")
	fmt.Fprintf(&b, "crucible_template_health_snapshot_timestamp_seconds %d\n", snapshot.CreatedAt.Unix())
	return b.Bytes()
}

func writeAttemptMetrics(
	b *bytes.Buffer,
	templateName string,
	checkType string,
	checkedAt *time.Time,
	passed *bool,
	durationSeconds *float64,
	faultClass *string,
) {
	if checkedAt == nil || passed == nil {
		return
	}
	fmt.Fprintf(b, "crucible_template_health_attempt_status{template=%q,check_type=%q} %d\n",
		templateName, checkType, boolMetric(*passed))
	fmt.Fprintf(b, "crucible_template_health_last_check_timestamp_seconds{template=%q,check_type=%q} %d\n",
		templateName, checkType, checkedAt.Unix())
	if durationSeconds != nil {
		fmt.Fprintf(b, "crucible_template_health_last_check_duration_seconds{template=%q,check_type=%q} %g\n",
			templateName, checkType, *durationSeconds)
	}
	if !*passed && faultClass != nil && *faultClass != "" {
		fmt.Fprintf(b, "crucible_template_health_fault_info{template=%q,check_type=%q,fault_class=%q} 1\n",
			templateName, checkType, *faultClass)
	}
}

func confirmedHealthValue(status string) float64 {
	switch status {
	case "healthy":
		return 1
	case "unhealthy":
		return 0
	default:
		return -1
	}
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}
