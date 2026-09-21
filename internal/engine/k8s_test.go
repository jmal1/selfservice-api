package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jmal1/selfservice-api/internal/runner"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testK8sConfig() K8sConfig {
	return K8sConfig{
		Namespace:   "test-ns",
		RunnerImage: "ghcr.io/test/runner:latest",
		RunnerNode:  "test-node",
		TrunkNIC:    "eth1",
		EngineURL:   "http://engine:8081",
	}
}

func TestProvisionRunner_CreatesJobAndSecret(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)

	cfg := testK8sConfig()
	k8s := NewK8sClientFromClients(clientset, dynClient, cfg, testLogger())

	workflows := []runner.WorkflowDef{
		{Slug: "test-wf", Name: "Test", Script: "echo hello", TimeoutSeconds: 60},
	}
	target := runner.TargetConfig{IP: "10.100.5.10", OS: "linux", Username: "student", Password: "pass"}
	pod := runner.PodConfig{Subnet: "10.100.5.0/24", Index: 1}

	result, err := k8s.ProvisionRunner(context.Background(), RunnerSpec{
		RunID:         "abcd1234-5678-9012-3456-789012345678",
		CallbackToken: "callback-token-abc",
		EngineURL:     cfg.EngineURL,
		VLANTag:       105,
		Workflows:     workflows,
		Target:        target,
		Pod:           pod,
	})
	if err != nil {
		t.Fatalf("ProvisionRunner: %v", err)
	}

	// Verify job was created
	if result.JobName == "" {
		t.Fatal("expected non-empty JobName")
	}
	if result.SecretName == "" {
		t.Fatal("expected non-empty SecretName")
	}

	// Verify job exists in K8s
	job, err := clientset.BatchV1().Jobs("test-ns").Get(context.Background(), result.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// Check job spec
	if job.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "test-node" {
		t.Error("job not scheduled on correct node")
	}
	if len(job.Spec.Template.Spec.Tolerations) == 0 {
		t.Error("job missing runner toleration")
	}
	if job.Spec.Template.Annotations["k8s.v1.cni.cncf.io/networks"] != "pod-vlan-105" {
		t.Error("job missing Multus network annotation")
	}
	if *job.Spec.ActiveDeadlineSeconds != runnerActiveDeadlineSeconds {
		t.Errorf("ActiveDeadlineSeconds = %d, want %d", *job.Spec.ActiveDeadlineSeconds, runnerActiveDeadlineSeconds)
	}
	if *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Errorf("TTLSecondsAfterFinished = %d, want 300", *job.Spec.TTLSecondsAfterFinished)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit = %d, want 0", *job.Spec.BackoffLimit)
	}

	// Verify container spec
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "ghcr.io/test/runner:latest" {
		t.Errorf("container image = %q, want %q", container.Image, "ghcr.io/test/runner:latest")
	}
	if len(container.VolumeMounts) == 0 {
		t.Error("container missing volume mounts")
	}

	// Verify secret exists
	secret, err := clientset.CoreV1().Secrets("test-ns").Get(context.Background(), result.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if _, ok := secret.Data["runner-config.json"]; !ok {
		t.Error("secret missing runner-config.json")
	}

	// Verify labels
	if job.Labels["forge.crucible/run-id"] != "abcd1234-5678-9012-3456-789012345678" {
		t.Error("job missing run-id label")
	}
}

func TestCleanupRunner_DeletesResources(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job-config", Namespace: "test-ns"},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "test-job", Namespace: "test-ns"},
		},
	)

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	k8s := NewK8sClientFromClients(clientset, dynClient, testK8sConfig(), testLogger())

	err := k8s.CleanupRunner(context.Background(), "test-job", "test-job-config")
	if err != nil {
		t.Fatalf("CleanupRunner: %v", err)
	}

	// Verify secret deleted
	_, err = clientset.CoreV1().Secrets("test-ns").Get(context.Background(), "test-job-config", metav1.GetOptions{})
	if err == nil {
		t.Error("expected secret to be deleted")
	}

	// Verify job deleted
	_, err = clientset.BatchV1().Jobs("test-ns").Get(context.Background(), "test-job", metav1.GetOptions{})
	if err == nil {
		t.Error("expected job to be deleted")
	}
}

func TestCleanupRunner_HandlesAlreadyDeleted(t *testing.T) {
	clientset := fake.NewSimpleClientset() // empty — nothing to delete
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	k8s := NewK8sClientFromClients(clientset, dynClient, testK8sConfig(), testLogger())

	// Should not error on not-found
	err := k8s.CleanupRunner(context.Background(), "nonexistent-job", "nonexistent-secret")
	if err != nil {
		t.Fatalf("CleanupRunner on nonexistent: %v", err)
	}
}

func TestCleanupOrphanedRunners(t *testing.T) {
	now := metav1.Now()
	clientset := fake.NewSimpleClientset(
		// Completed job — should be cleaned up
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "crucible-runner-done", Namespace: "test-ns",
				Labels: map[string]string{"app": "crucible-runner"},
			},
			Status: batchv1.JobStatus{CompletionTime: &now},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "crucible-runner-done-config", Namespace: "test-ns"},
		},
		// Running job — should NOT be cleaned up
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "crucible-runner-active", Namespace: "test-ns",
				Labels: map[string]string{"app": "crucible-runner"},
			},
			Status: batchv1.JobStatus{Active: 1},
		},
	)

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	k8s := NewK8sClientFromClients(clientset, dynClient, testK8sConfig(), testLogger())

	cleaned, err := k8s.CleanupOrphanedRunners(context.Background())
	if err != nil {
		t.Fatalf("CleanupOrphanedRunners: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1", cleaned)
	}

	// Active job should still exist
	_, err = clientset.BatchV1().Jobs("test-ns").Get(context.Background(), "crucible-runner-active", metav1.GetOptions{})
	if err != nil {
		t.Error("active job should not have been deleted")
	}
}

func TestDeleteRunnerPod(t *testing.T) {
	clientset := fake.NewSimpleClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "crucible-runner-abc-xyz", Namespace: "test-ns",
				Labels: map[string]string{"job-name": "crucible-runner-abc"},
			},
		},
	)

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	k8s := NewK8sClientFromClients(clientset, dynClient, testK8sConfig(), testLogger())

	err := k8s.DeleteRunnerPod(context.Background(), "crucible-runner-abc")
	if err != nil {
		t.Fatalf("DeleteRunnerPod: %v", err)
	}

	// Pod should be deleted
	pods, _ := clientset.CoreV1().Pods("test-ns").List(context.Background(), metav1.ListOptions{
		LabelSelector: "job-name=crucible-runner-abc",
	})
	if len(pods.Items) != 0 {
		t.Error("expected pod to be deleted")
	}
}

func TestProvisionRunner_AtCapacity(t *testing.T) {
	active := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "crucible-runner-busy",
			Namespace: "test-ns",
			Labels:    map[string]string{"app": "crucible-runner"},
		},
		Status: batchv1.JobStatus{}, // no CompletionTime, Failed=0 → active
	}
	clientset := fake.NewSimpleClientset(active)
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)

	cfg := testK8sConfig()
	cfg.MaxConcurrentRunners = 1
	k8s := NewK8sClientFromClients(clientset, dynClient, cfg, testLogger())

	_, err := k8s.ProvisionRunner(context.Background(), RunnerSpec{
		RunID:         "abcd1234-5678-9012-3456-789012345678",
		CallbackToken: "tok",
		EngineURL:     cfg.EngineURL,
		VLANTag:       105,
		Workflows:     []runner.WorkflowDef{{Slug: "wf", Name: "W", Script: "true", TimeoutSeconds: 30}},
		Target:        runner.TargetConfig{IP: "10.100.5.10", OS: "linux"},
		Pod:           runner.PodConfig{Subnet: "10.100.5.0/24", Index: 1},
	})
	if err == nil {
		t.Fatal("expected capacity error, got nil")
	}
	if !errors.Is(err, ErrRunnerAtCapacity) {
		t.Fatalf("error = %v, want ErrRunnerAtCapacity", err)
	}
}
