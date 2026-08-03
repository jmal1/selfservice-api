package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// K8sClient wraps the Kubernetes client for runner pod lifecycle management.
type K8sClient struct {
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
	namespace     string
	runnerImage   string
	runnerNode    string
	trunkNIC      string
	metrics       *RunnerMetrics
	logger        *slog.Logger
}

// K8sConfig holds configuration for the K8s runner provisioner.
type K8sConfig struct {
	Namespace   string // K8s namespace for runner resources (default: selfservice)
	RunnerImage string // Container image for the runner (default: ghcr.io/jmal1/selfservice-crucible-runner:latest)
	RunnerNode  string // Node selector for runner pods (default: k3sv03)
	TrunkNIC    string // Host NIC for macvlan (default: ens34)
	EngineURL   string // Internal URL for runner callbacks
}

// NewK8sClient creates a K8s client using in-cluster config.
func NewK8sClient(cfg K8sConfig, logger *slog.Logger) (*K8sClient, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}

	return &K8sClient{
		clientset:     clientset,
		dynamicClient: dynClient,
		namespace:     cfg.Namespace,
		runnerImage:   cfg.RunnerImage,
		runnerNode:    cfg.RunnerNode,
		trunkNIC:      cfg.TrunkNIC,
		logger:        logger,
	}, nil
}

// NewK8sClientFromClients creates a K8sClient from pre-existing clients (for testing).
func NewK8sClientFromClients(clientset kubernetes.Interface, dynClient dynamic.Interface, cfg K8sConfig, logger *slog.Logger) *K8sClient {
	return &K8sClient{
		clientset:     clientset,
		dynamicClient: dynClient,
		namespace:     cfg.Namespace,
		runnerImage:   cfg.RunnerImage,
		runnerNode:    cfg.RunnerNode,
		trunkNIC:      cfg.TrunkNIC,
		logger:        logger,
	}
}

// ProvisionResult holds the K8s resource names created for a run.
type ProvisionResult struct {
	JobName    string
	SecretName string
	NADName    string
	PodName    string // Set after the pod starts
}

// ProvisionRunner creates the K8s Secret, NetworkAttachmentDefinition (if needed),
// and Job for a runner execution.
func (k *K8sClient) ProvisionRunner(ctx context.Context, runID, callbackToken string, vlanTag int, workflows []runner.WorkflowDef, target runner.TargetConfig, pod runner.PodConfig, engineURL string) (*ProvisionResult, error) {
	start := time.Now()
	resourceName := fmt.Sprintf("crucible-runner-%s", runID[:8])
	nadName := fmt.Sprintf("pod-vlan-%d", vlanTag)

	// 1. Ensure NetworkAttachmentDefinition exists for this VLAN
	if err := k.ensureNAD(ctx, nadName, vlanTag); err != nil {
		return nil, fmt.Errorf("ensure NAD: %w", err)
	}

	// 2. Create Secret with runner config
	runnerConfig := runner.RunnerConfig{
		CallbackURL:   engineURL,
		CallbackToken: callbackToken,
		RunID:         runID,
		Workflows:     workflows,
		Target:        target,
		Pod:           pod,
	}

	configJSON, err := json.Marshal(runnerConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal runner config: %w", err)
	}

	secretName := resourceName + "-config"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app":                    "crucible-runner",
				"forge.crucible/run-id":  runID,
			},
		},
		Data: map[string][]byte{
			"runner-config.json": configJSON,
		},
	}

	if _, err := k.clientset.CoreV1().Secrets(k.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	k.logger.Info("created runner secret", "name", secretName, "run_id", runID)

	// 3. Create Job
	var activeDeadline int64 = 600     // 10 minutes hard kill
	var ttlAfterFinished int32 = 300   // 5 min cleanup
	var backoffLimit int32 = 0         // No retries

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app":                    "crucible-runner",
				"forge.crucible/run-id":  runID,
			},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &activeDeadline,
			TTLSecondsAfterFinished: &ttlAfterFinished,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                    "crucible-runner",
						"forge.crucible/run-id":  runID,
					},
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": nadName,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Tolerations: []corev1.Toleration{
						{
							Key:      "role",
							Operator: corev1.TolerationOpEqual,
							Value:    "runner",
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": k.runnerNode,
					},
					Containers: []corev1.Container{
						{
							Name:  "runner",
							Image: k.runnerImage,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("2"),
									corev1.ResourceMemory: resource.MustParse("2Gi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "runner-config",
									MountPath: "/opt/crucible",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "runner-config",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretName,
								},
							},
						},
					},
				},
			},
		},
	}

	createdJob, err := k.clientset.BatchV1().Jobs(k.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		// Cleanup the secret if job creation fails
		_ = k.clientset.CoreV1().Secrets(k.namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		return nil, fmt.Errorf("create job: %w", err)
	}

	k.metrics.ObserveRunnerProvision(time.Since(start))
	k.metrics.incRunnerActive(1)

	k.logger.Info("created runner job",
		"name", resourceName,
		"run_id", runID,
		"vlan", vlanTag,
		"node", k.runnerNode,
	)

	return &ProvisionResult{
		JobName:    createdJob.Name,
		SecretName: secretName,
		NADName:    nadName,
	}, nil
}

// ensureNAD creates a NetworkAttachmentDefinition for a VLAN if it doesn't exist.
// NADs are reusable across runs on the same VLAN.
func (k *K8sClient) ensureNAD(ctx context.Context, name string, vlanTag int) error {
	nadGVR := schema.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	}

	// Check if NAD already exists
	_, err := k.dynamicClient.Resource(nadGVR).Namespace(k.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		k.logger.Debug("NAD already exists", "name", name)
		return nil // Already exists
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("check NAD: %w", err)
	}

	// Create the NAD
	nadConfig := map[string]any{
		"cniVersion": "0.3.1",
		"type":       "macvlan",
		"master":     k.trunkNIC,
		"vlan":       vlanTag,
		"mode":       "bridge",
		"ipam": map[string]any{
			"type": "dhcp",
		},
	}
	configJSON, err := json.Marshal(nadConfig)
	if err != nil {
		return fmt.Errorf("marshal NAD config: %w", err)
	}

	nad := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":      name,
				"namespace": k.namespace,
				"labels": map[string]any{
					"app":                      "crucible-runner",
					"forge.crucible/vlan":      fmt.Sprintf("%d", vlanTag),
				},
			},
			"spec": map[string]any{
				"config": string(configJSON),
			},
		},
	}

	_, err = k.dynamicClient.Resource(nadGVR).Namespace(k.namespace).Create(ctx, nad, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create NAD: %w", err)
	}

	k.logger.Info("created NAD", "name", name, "vlan", vlanTag, "nic", k.trunkNIC)
	return nil
}

// CleanupRunner deletes the K8s Secret and Job for a completed run.
// The Job has TTLSecondsAfterFinished as a fallback, but we clean up
// eagerly when the engine receives the completion callback.
func (k *K8sClient) CleanupRunner(ctx context.Context, jobName, secretName string) error {
	// The runner Job is finished (or being force-cleaned); decrement the active gauge
	// regardless of whether the deletes succeed below.
	k.metrics.incRunnerActive(-1)

	var errs []string

	// Delete the secret first (contains callback token + workflow scripts)
	if err := k.clientset.CoreV1().Secrets(k.namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		k.metrics.RecordCleanupFailure("secret")
		errs = append(errs, fmt.Sprintf("delete secret %s: %v", secretName, err))
	} else {
		k.logger.Debug("deleted runner secret", "name", secretName)
	}

	// Delete the job (cascades to pod)
	propagation := metav1.DeletePropagationBackground
	if err := k.clientset.BatchV1().Jobs(k.namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		k.metrics.RecordCleanupFailure("job")
		errs = append(errs, fmt.Sprintf("delete job %s: %v", jobName, err))
	} else {
		k.logger.Debug("deleted runner job", "name", jobName)
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// CleanupOrphanedRunners finds and deletes runner Jobs that have completed
// but weren't cleaned up (e.g., engine missed the callback).
func (k *K8sClient) CleanupOrphanedRunners(ctx context.Context) (int, error) {
	jobs, err := k.clientset.BatchV1().Jobs(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=crucible-runner",
	})
	if err != nil {
		return 0, fmt.Errorf("list runner jobs: %w", err)
	}

	cleaned := 0
	for _, job := range jobs.Items {
		// Only cleanup completed/failed jobs
		if job.Status.CompletionTime == nil && job.Status.Failed == 0 {
			continue // Still running
		}

		secretName := job.Name + "-config"
		if err := k.CleanupRunner(ctx, job.Name, secretName); err != nil {
			k.logger.Warn("failed to cleanup orphaned runner",
				"job", job.Name,
				"error", err,
			)
			continue
		}
		cleaned++
		k.logger.Info("cleaned up orphaned runner", "job", job.Name)
	}

	if cleaned > 0 {
		k.metrics.RecordOrphansCleaned(cleaned)
	}
	return cleaned, nil
}
func (k *K8sClient) DeleteRunnerPod(ctx context.Context, jobName string) error {
	// List pods owned by this job
	pods, err := k.clientset.CoreV1().Pods(k.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil {
		return fmt.Errorf("list pods for job %s: %w", jobName, err)
	}

	for _, pod := range pods.Items {
		if err := k.clientset.CoreV1().Pods(k.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod.Name, err)
		}
		k.logger.Info("deleted runner pod", "pod", pod.Name, "job", jobName)
	}

	return nil
}
