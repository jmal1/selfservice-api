package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runnerDiagnosticsMaxLen bounds the snapshot folded into a run's
// error_message. A pathological pod (CrashLoopBackOff for ten minutes) can
// accumulate dozens of events, and this string is rendered to students and
// instructors in the testing UI. Truncation is marked so a reader knows to go
// look at the engine log, which carries the untruncated value.
const runnerDiagnosticsMaxLen = 900

// maxRunnerDiagnosticEvents caps how many Warning events are quoted per pod.
// The newest are kept: for a pod that never started, the last event is almost
// always the actionable one (the final pull failure, the last FailedScheduling
// reason), while the earlier ones repeat it.
const maxRunnerDiagnosticEvents = 4

// DescribeRunner returns a compact, human-readable snapshot of a runner Job and
// the pods it owns: pod phase, the node it landed on, per-container waiting or
// terminated reasons, unmet pod conditions, and recent Warning events.
//
// It exists because the timeout watchdog calls CleanupRunner, which deletes the
// Job and with it the pods — destroying the only evidence of *why* a run never
// reported. Before this, every timed-out run recorded the identical string
// ("Run timed out after 10 minutes") whether the image failed to pull, the
// macvlan DHCP lease never arrived, the node was cordoned, the Job never
// created a pod at all, or the runner genuinely hung. All five are common and
// they demand completely different responses. Capture first, then clean up.
//
// It never returns an error: this runs on the failure path, and a diagnostics
// helper that can itself fail would just add a second unexplained failure on
// top of the first. Anything it cannot determine is reported as text.
func (k *K8sClient) DescribeRunner(ctx context.Context, jobName string) string {
	if k == nil || k.clientset == nil {
		return "no kubernetes client"
	}

	var parts []string

	job, err := k.clientset.BatchV1().Jobs(k.namespace).Get(ctx, jobName, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		// Worth stating plainly. A missing Job means something else already
		// removed it (an earlier cleanup, TTL controller, manual kubectl), so
		// the absence of pod detail below is expected rather than a gap.
		parts = append(parts, fmt.Sprintf("job %s: not found", jobName))
	case err != nil:
		parts = append(parts, fmt.Sprintf("job %s: lookup failed: %v", jobName, err))
	default:
		parts = append(parts, fmt.Sprintf("job %s: active=%d succeeded=%d failed=%d",
			jobName, job.Status.Active, job.Status.Succeeded, job.Status.Failed))
		for _, c := range job.Status.Conditions {
			if c.Status == corev1.ConditionTrue {
				parts = append(parts, fmt.Sprintf("job condition %s: %s %s",
					c.Type, c.Reason, c.Message))
			}
		}
	}

	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil {
		parts = append(parts, fmt.Sprintf("list pods: %v", err))
		return joinDiagnostics(parts)
	}
	if len(pods.Items) == 0 {
		// Not a formatting nicety — this is itself a diagnosis. A Job with no
		// pod means the Job controller never created one (quota exhausted,
		// admission webhook rejection), which points somewhere completely
		// different from a pod that was created and then failed.
		parts = append(parts, "no runner pods exist for this job")
		return joinDiagnostics(parts)
	}

	for _, pod := range pods.Items {
		parts = append(parts, describeRunnerPod(pod))
		parts = append(parts, k.podWarningEvents(ctx, pod.Name)...)
	}

	return joinDiagnostics(parts)
}

// describeRunnerPod summarises one pod's scheduling and container state.
func describeRunnerPod(pod corev1.Pod) string {
	node := pod.Spec.NodeName
	if node == "" {
		node = "<unscheduled>"
	}
	desc := fmt.Sprintf("pod %s: phase=%s node=%s", pod.Name, pod.Status.Phase, node)

	// An unmet PodScheduled/Ready condition carries the scheduler's own reason,
	// which is the single most useful field when a pod never starts.
	for _, c := range pod.Status.Conditions {
		if c.Status != corev1.ConditionTrue && (c.Reason != "" || c.Message != "") {
			desc += fmt.Sprintf(" | %s=False %s %s", c.Type, c.Reason, c.Message)
		}
	}

	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	for _, cs := range statuses {
		switch {
		case cs.State.Waiting != nil:
			desc += fmt.Sprintf(" | container %s waiting: %s %s",
				cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
		case cs.State.Terminated != nil:
			desc += fmt.Sprintf(" | container %s terminated: %s exit=%d %s",
				cs.Name, cs.State.Terminated.Reason,
				cs.State.Terminated.ExitCode, cs.State.Terminated.Message)
		case cs.State.Running != nil:
			// "Running but the run never reported" is a distinct and important
			// diagnosis: the infrastructure worked and the runner itself hung.
			desc += fmt.Sprintf(" | container %s running since %s",
				cs.Name, cs.State.Running.StartedAt.Format("15:04:05Z"))
		}
	}
	return desc
}

// podWarningEvents returns the most recent Warning events for a pod.
func (k *K8sClient) podWarningEvents(ctx context.Context, podName string) []string {
	evts, err := k.clientset.CoreV1().Events(k.namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=" + podName,
	})
	if err != nil {
		return []string{fmt.Sprintf("events for %s: %v", podName, err)}
	}

	// The field selector above is a transfer optimisation, not a guarantee:
	// fake clients ignore it entirely, and a selector typo would silently widen
	// the result set. Filtering here too means this function reports events for
	// the pod it was asked about under every client implementation.
	var matched []corev1.Event
	for _, e := range evts.Items {
		if e.InvolvedObject.Name == podName && e.Type == corev1.EventTypeWarning {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return nil
	}

	sort.Slice(matched, func(i, j int) bool {
		a, b := eventTime(matched[i]), eventTime(matched[j])
		return a.Before(&b)
	})
	if len(matched) > maxRunnerDiagnosticEvents {
		matched = matched[len(matched)-maxRunnerDiagnosticEvents:]
	}

	out := make([]string, 0, len(matched))
	for _, e := range matched {
		out = append(out, fmt.Sprintf("event %s: %s", e.Reason, strings.TrimSpace(e.Message)))
	}
	return out
}

// eventTime picks the most meaningful timestamp an Event carries. LastTimestamp
// is empty on events emitted through the events.k8s.io path, where EventTime is
// set instead; falling back keeps ordering correct for both.
func eventTime(e corev1.Event) (t metav1.Time) {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp
	}
	if !e.EventTime.IsZero() {
		return metav1.Time{Time: e.EventTime.Time}
	}
	return e.FirstTimestamp
}

func joinDiagnostics(parts []string) string {
	s := strings.Join(parts, "; ")
	if len(s) > runnerDiagnosticsMaxLen {
		s = s[:runnerDiagnosticsMaxLen] + "… (truncated, see engine log)"
	}
	return s
}
