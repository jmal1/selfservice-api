package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func diagClient(objs ...runtime.Object) *K8sClient {
	clientset := fake.NewSimpleClientset(objs...)
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	return NewK8sClientFromClients(clientset, dynClient, testK8sConfig(), testLogger())
}

func runnerJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		Status:     batchv1.JobStatus{Active: 1},
	}
}

func runnerPod(name, job string, mutate func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test-ns",
			Labels:    map[string]string{"job-name": job},
		},
		Spec:   corev1.PodSpec{NodeName: "k3sv03"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func warnEvent(name, podName, reason, msg string, at time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "test-ns"},
		InvolvedObject: corev1.ObjectReference{Name: podName, Namespace: "test-ns"},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        msg,
		LastTimestamp:  metav1.Time{Time: at},
	}
}

// TestDescribeRunner_ReportsImagePullFailure covers the failure this whole file
// exists for: a runner that never started. Before DescribeRunner every one of
// these recorded the identical "Run timed out after 10 minutes", because the
// watchdog deleted the Job (and therefore the pod) before anything was read
// off it.
func TestDescribeRunner_ReportsImagePullFailure(t *testing.T) {
	k := diagClient(
		runnerJob("crucible-runner-abc"),
		runnerPod("crucible-runner-abc-x1", "crucible-runner-abc", func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "runner",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: "Back-off pulling image",
				}},
			}}
		}),
		warnEvent("e1", "crucible-runner-abc-x1", "Failed",
			"Failed to pull image: 401 Unauthorized", time.Now()),
	)

	got := k.DescribeRunner(context.Background(), "crucible-runner-abc")

	for _, want := range []string{
		"crucible-runner-abc-x1",
		"Pending",
		"k3sv03",
		"ImagePullBackOff",
		"401 Unauthorized",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostics missing %q — an operator cannot tell an image-pull "+
				"failure from a hung runner.\ngot: %s", want, got)
		}
	}
}

// TestDescribeRunner_ReportsUnschedulablePod guards the case where the pod is
// created but never lands on a node — the k3sv03 taint/toleration or node
// pressure. The scheduler's reason lives on the PodScheduled condition, not on
// any container status, so it is easy to drop.
func TestDescribeRunner_ReportsUnschedulablePod(t *testing.T) {
	k := diagClient(
		runnerJob("crucible-runner-sched"),
		runnerPod("crucible-runner-sched-x1", "crucible-runner-sched", func(p *corev1.Pod) {
			p.Spec.NodeName = ""
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: "0/3 nodes are available: 1 node(s) had untolerated taint",
			}}
		}),
	)

	got := k.DescribeRunner(context.Background(), "crucible-runner-sched")

	if !strings.Contains(got, "Unschedulable") || !strings.Contains(got, "untolerated taint") {
		t.Errorf("scheduler reason lost — the PodScheduled condition is the only place it "+
			"appears.\ngot: %s", got)
	}
	if !strings.Contains(got, "<unscheduled>") {
		t.Errorf("an unscheduled pod must not report an empty node name.\ngot: %s", got)
	}
}

// TestDescribeRunner_ReportsRunningContainer is the opposite diagnosis and is
// just as important: infrastructure worked and the runner itself hung. That
// sends the reader to the runner's own code rather than to the node, the image
// or the network.
func TestDescribeRunner_ReportsRunningContainer(t *testing.T) {
	k := diagClient(
		runnerJob("crucible-runner-hung"),
		runnerPod("crucible-runner-hung-x1", "crucible-runner-hung", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "runner",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.Time{Time: time.Now()},
				}},
			}}
		}),
	)

	got := k.DescribeRunner(context.Background(), "crucible-runner-hung")
	if !strings.Contains(got, "container runner running since") {
		t.Errorf("a pod that is Running when the watchdog fires means the runner hung — "+
			"that must be stated, not omitted.\ngot: %s", got)
	}
}

// TestDescribeRunner_NoPodsIsItselfADiagnosis: a Job with no pod means the Job
// controller never created one (quota, admission webhook), which points
// somewhere entirely different from a pod that was created and failed.
func TestDescribeRunner_NoPodsIsItselfADiagnosis(t *testing.T) {
	k := diagClient(runnerJob("crucible-runner-nopods"))

	got := k.DescribeRunner(context.Background(), "crucible-runner-nopods")
	if !strings.Contains(got, "no runner pods exist") {
		t.Errorf("a Job with zero pods must say so explicitly.\ngot: %s", got)
	}
}

// TestDescribeRunner_MissingJobDoesNotFail: this runs on the failure path, so it
// must degrade to text rather than add a second unexplained failure.
func TestDescribeRunner_MissingJobDoesNotFail(t *testing.T) {
	k := diagClient()

	got := k.DescribeRunner(context.Background(), "crucible-runner-gone")
	if got == "" {
		t.Fatal("DescribeRunner returned an empty string; it must always explain itself")
	}
	if !strings.Contains(got, "not found") {
		t.Errorf("a missing Job must be reported as such.\ngot: %s", got)
	}
}

// TestDescribeRunner_IgnoresOtherPodsEvents is the guard for the fake-client
// trap: fake clientsets ignore FieldSelector entirely, so relying on the
// selector alone would attribute an unrelated pod's events to the runner and
// send an operator chasing the wrong workload. The Go-side filter is what makes
// the behaviour identical under both real and fake clients.
func TestDescribeRunner_IgnoresOtherPodsEvents(t *testing.T) {
	k := diagClient(
		runnerJob("crucible-runner-abc"),
		runnerPod("crucible-runner-abc-x1", "crucible-runner-abc", nil),
		warnEvent("e-other", "some-unrelated-pod", "OOMKilled",
			"UNRELATED-POD-EVENT", time.Now()),
	)

	got := k.DescribeRunner(context.Background(), "crucible-runner-abc")
	if strings.Contains(got, "UNRELATED-POD-EVENT") {
		t.Errorf("events from another pod leaked into the runner diagnostics.\ngot: %s", got)
	}
}

// TestDescribeRunner_KeepsNewestEvents: a pod that backs off for ten minutes
// accumulates dozens of near-identical events. The newest are the actionable
// ones, and the whole string is bounded because it lands in a student-visible
// error_message.
func TestDescribeRunner_KeepsNewestEvents(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	objs := []runtime.Object{
		runnerJob("crucible-runner-noisy"),
		runnerPod("crucible-runner-noisy-x1", "crucible-runner-noisy", nil),
	}
	for i := 0; i < maxRunnerDiagnosticEvents+3; i++ {
		objs = append(objs, warnEvent(
			"e"+string(rune('a'+i)), "crucible-runner-noisy-x1",
			"BackOff", "attempt-"+string(rune('a'+i)),
			base.Add(time.Duration(i)*time.Minute),
		))
	}

	got := k8sDescribe(t, objs, "crucible-runner-noisy")

	if strings.Contains(got, "attempt-a") {
		t.Errorf("oldest event retained; newest events are the actionable ones.\ngot: %s", got)
	}
	newest := "attempt-" + string(rune('a'+maxRunnerDiagnosticEvents+2))
	if !strings.Contains(got, newest) {
		t.Errorf("newest event %q dropped.\ngot: %s", newest, got)
	}
	if strings.Count(got, "event BackOff") != maxRunnerDiagnosticEvents {
		t.Errorf("expected exactly %d events, got %d.\ngot: %s",
			maxRunnerDiagnosticEvents, strings.Count(got, "event BackOff"), got)
	}
}

// TestDescribeRunner_TruncatesAndSaysSo bounds a value that is rendered to
// students. Silent truncation would be worse than none — a reader would not
// know the message was cut and would not go to the engine log for the rest.
func TestDescribeRunner_TruncatesAndSaysSo(t *testing.T) {
	huge := strings.Repeat("x", runnerDiagnosticsMaxLen*2)
	k := diagClient(
		runnerJob("crucible-runner-big"),
		runnerPod("crucible-runner-big-x1", "crucible-runner-big", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name:  "runner",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Message: huge}},
			}}
		}),
	)

	got := k.DescribeRunner(context.Background(), "crucible-runner-big")
	if len(got) > runnerDiagnosticsMaxLen+64 {
		t.Errorf("diagnostics not bounded: %d chars", len(got))
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("truncation must be announced so a reader knows to check the engine log.\ngot: %s", got)
	}
}

func k8sDescribe(t *testing.T, objs []runtime.Object, job string) string {
	t.Helper()
	return diagClient(objs...).DescribeRunner(context.Background(), job)
}
