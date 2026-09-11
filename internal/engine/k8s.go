package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
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
	clientset        kubernetes.Interface
	dynamicClient    dynamic.Interface
	namespace        string
	runnerImage      string
	runnerNode       string
	trunkNIC         string
	imagePullSecrets []string
	metrics          *RunnerMetrics
	logger           *slog.Logger
}

// K8sConfig holds configuration for the K8s runner provisioner.
type K8sConfig struct {
	Namespace   string // K8s namespace for runner resources (default: selfservice)
	RunnerImage string // Container image for the runner (default: ghcr.io/jmal1/selfservice-crucible-runner:latest)
	RunnerNode  string // Node selector for runner pods (default: k3sv03)
	TrunkNIC    string // Host NIC carrying the pod VLAN trunk (default: ens224)
	EngineURL   string // Internal URL for runner callbacks

	// ImagePullSecrets names the dockerconfigjson Secrets used to pull
	// RunnerImage. Every Crucible GHCR package is private, so without this the
	// kubelet falls back to an anonymous token request and the pull fails with
	// 401 Unauthorized. Each Helm-managed workload gets this from
	// .Values.imagePullSecrets; the runner Job is built here in Go, so it must
	// be passed through explicitly.
	ImagePullSecrets []string
}

// runnerActiveDeadlineSeconds is the hard kill deadline for a runner Job.
//
// This was 600s. The Kali-based runner image (~1 GB) has to be pulled cold the
// first time it runs on a node, and a cold pull plus the assessment itself did
// not reliably fit inside 10 minutes. TestActiveDeadline_MatchesConfig guards
// the value so it is not silently reverted.
const runnerActiveDeadlineSeconds int64 = 900

// runnerConfigSecretKey is the key inside the runner Secret AND the subPath used
// to mount it. The two must stay equal: a subPath names a key within the volume,
// so a mismatch mounts an empty directory over the config path and the runner
// exits with "no such file or directory" on a path that visibly exists.
const runnerConfigSecretKey = "runner-config.json"

// runnerConfigMountPath is where the runner reads its config. It MUST equal
// runner.DefaultConfigPath, and it MUST be a file path inside the install root
// rather than the install root itself -- see the VolumeMount comment below.
const runnerConfigMountPath = "/opt/crucible/" + runnerConfigSecretKey

// podVLANTagBase maps a pod VLAN tag to its /24: tag 119 -> 10.100.19.0/24.
// This is the same convention encoded in vlan_pool.subnet (migration 000003)
// and in provisioner.createPod, and all three must agree.
const podVLANTagBase = 100

// runnerRangeStartHost/runnerRangeEndHost bound the host addresses host-local
// may hand a runner on a pod VLAN. They sit ABOVE the OPNsense DHCP pool
// (.10-.250) that student VMs draw from, so a runner can never take an address
// a pod VM might be offered. Four addresses is deliberate headroom: the engine
// refuses concurrent runs for one pod, so one is normally enough.
const (
	runnerRangeStartHost = 251
	runnerRangeEndHost   = 254
)

// imagePullSecretRefs converts secret names into LocalObjectReferences,
// returning nil for an empty list so the pod spec stays unchanged when no
// secrets are configured.
func imagePullSecretRefs(names []string) []corev1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			refs = append(refs, corev1.LocalObjectReference{Name: n})
		}
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
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
		clientset:        clientset,
		dynamicClient:    dynClient,
		namespace:        cfg.Namespace,
		runnerImage:      cfg.RunnerImage,
		runnerNode:       cfg.RunnerNode,
		trunkNIC:         cfg.TrunkNIC,
		imagePullSecrets: cfg.ImagePullSecrets,
		logger:           logger,
	}, nil
}

// NewK8sClientFromClients creates a K8sClient from pre-existing clients (for testing).
func NewK8sClientFromClients(clientset kubernetes.Interface, dynClient dynamic.Interface, cfg K8sConfig, logger *slog.Logger) *K8sClient {
	return &K8sClient{
		clientset:        clientset,
		dynamicClient:    dynClient,
		namespace:        cfg.Namespace,
		runnerImage:      cfg.RunnerImage,
		runnerNode:       cfg.RunnerNode,
		trunkNIC:         cfg.TrunkNIC,
		imagePullSecrets: cfg.ImagePullSecrets,
		logger:           logger,
	}
}

// ProvisionResult holds the K8s resource names created for a run.
type ProvisionResult struct {
	JobName    string
	SecretName string
	NADName    string
	PodName    string // Set after the pod starts
}

// RunnerSpec describes everything needed to provision one runner execution.
//
// This is a struct rather than a positional parameter list because the argument
// count had already reached eight, four of which were bare strings. Adding the
// action library as a ninth would have made a silent argument-order swap a
// realistic mistake -- one that compiles cleanly and surfaces only as a runner
// that cannot call home, which is precisely the class of failure that took the
// longest to diagnose here.
type RunnerSpec struct {
	RunID         string
	CallbackToken string
	EngineURL     string
	VLANTag       int
	Workflows     []runner.WorkflowDef
	Target        runner.TargetConfig
	Pod           runner.PodConfig

	// ActionLibrary is the generated bash function library (see
	// buildActionLibrary). Empty is legal and simply means no library actions
	// are injected.
	ActionLibrary string
}

// ProvisionRunner creates the K8s Secret, NetworkAttachmentDefinition (if needed),
// and Job for a runner execution.
func (k *K8sClient) ProvisionRunner(ctx context.Context, spec RunnerSpec) (*ProvisionResult, error) {
	start := time.Now()
	resourceName := fmt.Sprintf("crucible-runner-%s", spec.RunID[:8])
	nadName := fmt.Sprintf("pod-vlan-%d", spec.VLANTag)

	// 1. Ensure NetworkAttachmentDefinition exists for this VLAN
	if err := k.ensureNAD(ctx, nadName, spec.VLANTag); err != nil {
		return nil, fmt.Errorf("ensure NAD: %w", err)
	}

	// 2. Create Secret with runner config
	runnerConfig := runner.RunnerConfig{
		CallbackURL:   spec.EngineURL,
		CallbackToken: spec.CallbackToken,
		RunID:         spec.RunID,
		Workflows:     spec.Workflows,
		ActionLibrary: spec.ActionLibrary,
		Target:        spec.Target,
		Pod:           spec.Pod,
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
				"app":                   "crucible-runner",
				"forge.crucible/run-id": spec.RunID,
			},
		},
		Data: map[string][]byte{
			runnerConfigSecretKey: configJSON,
		},
	}

	if _, err := k.clientset.CoreV1().Secrets(k.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	k.logger.Info("created runner secret", "name", secretName, "run_id", spec.RunID)

	// 3. Create Job
	var activeDeadline int64 = runnerActiveDeadlineSeconds
	var ttlAfterFinished int32 = 300 // 5 min cleanup
	var backoffLimit int32 = 0       // No retries

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: k.namespace,
			Labels: map[string]string{
				"app":                   "crucible-runner",
				"forge.crucible/run-id": spec.RunID,
			},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &activeDeadline,
			TTLSecondsAfterFinished: &ttlAfterFinished,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                   "crucible-runner",
						"forge.crucible/run-id": spec.RunID,
					},
					Annotations: map[string]string{
						"k8s.v1.cni.cncf.io/networks": nadName,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: imagePullSecretRefs(k.imagePullSecrets),
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
									Name: "runner-config",
									// Mount the SINGLE config file, not the directory.
									//
									// /opt/crucible is the image's install root: the
									// binary lives at /opt/crucible/bin/crucible-runner
									// (and is on PATH) and the action library at
									// /opt/crucible/lib/actions.sh. Mounting a volume at
									// /opt/crucible replaces that whole directory, so the
									// container's own ENTRYPOINT vanished and every run
									// died before executing a single action with:
									//
									//   exec: "crucible-runner": executable file not
									//   found in $PATH
									//
									// subPath grafts just runner-config.json into the
									// existing directory, leaving bin/ and lib/ intact,
									// and keeps the file exactly at
									// runner.DefaultConfigPath so the runner contract is
									// unchanged.
									//
									// subPath volumes do not receive later Secret updates.
									// That is fine and in fact desirable here: the Secret
									// is written once per run and must not mutate under a
									// running assessment.
									MountPath: runnerConfigMountPath,
									SubPath:   runnerConfigSecretKey,
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
		"run_id", spec.RunID,
		"vlan", spec.VLANTag,
		"node", k.runnerNode,
	)

	return &ProvisionResult{
		JobName:    createdJob.Name,
		SecretName: secretName,
		NADName:    nadName,
	}, nil
}

// ensureNAD creates or reconciles a NetworkAttachmentDefinition for a VLAN.
// NADs are reusable across runs on the same VLAN.
//
// It deliberately reconciles the CNI *config*, not merely the NAD's existence.
// The original implementation returned early whenever a NAD of the right name
// was present, which meant a corrected config could never reach any VLAN that
// had already been used. That turned a deployed fix into a silent no-op: after
// shipping the macvlan->vlan correction below, pod-vlan-119 still carried the
// broken untagged config and would have kept putting the runner on the wrong
// network indefinitely.
func (k *K8sClient) ensureNAD(ctx context.Context, name string, vlanTag int) error {
	nadGVR := schema.GroupVersionResource{
		Group:    "k8s.cni.cncf.io",
		Version:  "v1",
		Resource: "network-attachment-definitions",
	}

	configJSON, err := k.nadConfigJSON(vlanTag)
	if err != nil {
		return err
	}

	existing, err := k.dynamicClient.Resource(nadGVR).Namespace(k.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		current, _, _ := unstructured.NestedString(existing.Object, "spec", "config")
		if sameCNIConfig(current, configJSON) {
			k.logger.Debug("NAD already correct", "name", name)
			return nil
		}
		k.logger.Warn("NAD config drifted, updating",
			"name", name, "vlan", vlanTag, "old", current, "new", configJSON)
		if err := unstructured.SetNestedField(existing.Object, configJSON, "spec", "config"); err != nil {
			return fmt.Errorf("set NAD config: %w", err)
		}
		if _, err := k.dynamicClient.Resource(nadGVR).Namespace(k.namespace).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update NAD: %w", err)
		}
		k.logger.Info("updated NAD", "name", name, "vlan", vlanTag, "nic", k.trunkNIC)
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("check NAD: %w", err)
	}

	nad := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "k8s.cni.cncf.io/v1",
			"kind":       "NetworkAttachmentDefinition",
			"metadata": map[string]any{
				"name":      name,
				"namespace": k.namespace,
				"labels": map[string]any{
					"app":                 "crucible-runner",
					"forge.crucible/vlan": fmt.Sprintf("%d", vlanTag),
				},
			},
			"spec": map[string]any{
				"config": configJSON,
			},
		},
	}

	if _, err := k.dynamicClient.Resource(nadGVR).Namespace(k.namespace).Create(ctx, nad, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create NAD: %w", err)
	}

	k.logger.Info("created NAD", "name", name, "vlan", vlanTag, "nic", k.trunkNIC)
	return nil
}

// sameCNIConfig compares two CNI configs semantically, so that key ordering or
// whitespace differences do not cause a pointless update on every run.
func sameCNIConfig(a, b string) bool {
	var ma, mb map[string]any
	if err := json.Unmarshal([]byte(a), &ma); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &mb); err != nil {
		return false
	}
	return reflect.DeepEqual(ma, mb)
}

// nadConfigJSON builds the CNI config attaching a runner to a pod VLAN.
//
// This uses the `vlan` CNI plugin, NOT `macvlan`.
//
// The original config was {"type":"macvlan","master":ens224,"vlan":<tag>,...}.
// The macvlan plugin has no `vlan` option, and CNI plugins ignore unknown
// JSON fields, so the tag was silently discarded: the plugin returned
// success and attached an UNTAGGED macvlan to the trunk NIC. Verified
// empirically on k3sv03 — invoking macvlan with "vlan":119 created no
// ens224.119 device and the interface's parent was ens224 itself. In
// production the runner then DHCP'd on the trunk's native VLAN and was
// handed a home-LAN address (192.0.2.109/22) instead of a pod-VLAN one,
// so it both leaked onto the wrong network and could never reach the
// student VM it was meant to assess.
//
// The `vlan` plugin creates a real 802.1Q sub-interface of master with the
// given vlanId and moves it into the container's netns. Verified on k3sv03:
// yields "vlan protocol 802.1Q id 119" and a DHCP lease of 10.100.19.11/24
// from the pod VLAN's gateway.
//
// A side effect worth knowing: the resulting interface inherits the parent
// vNIC's MAC rather than inventing one, so it does not depend on the
// vSphere vSwitch security exceptions (promiscuous / forged transmits /
// MAC changes) that a macvlan child requires.
//
// Because the plugin moves a single sub-interface into the netns, only one
// container per node may hold a given VLAN at a time. That matches the
// runner model: one VLAN per pod, and the engine refuses concurrent runs
// for the same pod.
//
// The `vlan` plugin binary is not part of k3s's bundled CNI set and must be
// staged into /var/lib/rancher/k3s/data/cni on every runner node — see
// k3sv03-Runner-Node-Runbook.
//
// IPAM is `host-local`, NOT `dhcp`, and deliberately declares no routes.
//
// With `dhcp`, OPNsense answered the lease with a router option, so the CNI
// dhcp plugin returned a 0.0.0.0/0 route and the kernel installed a SECOND
// default route on net1 — ahead of Flannel's. Nothing in the pod's routing
// table covers the Service CIDR (10.43.0.0/16), so cluster DNS and the
// engine's ClusterIP both fell through to that default, left via the pod
// VLAN, and vanished. The runner ran its workflow perfectly and then could
// not deliver a single result. Verified on k3sv03: with dhcp the pod gets
// "default via 10.100.19.1 dev net1" first and the engine is unreachable;
// with the config below it gets exactly one default (via Flannel), reaches
// the engine (HTTP 200), and still reaches the target VM on the pod VLAN.
//
// Omitting "routes" is the whole fix: CNI only installs routes an IPAM plugin
// returns, so host-local yields nothing but the connected /24. Neither of the
// Multus-level knobs helps — "default-route" in the network annotation does
// not strip a route the delegate already installed, and the dhcp plugin's
// skipDefaultGateway is not honoured by v1.6.2. Both were tried on k3sv03.
//
// The address range is .251-.254, deliberately OUTSIDE the OPNsense DHCP pool
// of .10-.250 that provisioner.createPod hands to student VMs, so a runner can
// never collide with a pod VM. host-local keeps its allocations in node-local
// state, so concurrent runners on one VLAN still get distinct addresses.
// The subnet convention (10.100.<tag-100>.0/24) is the same one encoded in
// vlan_pool.subnet and in provisioner.createPod.
func (k *K8sClient) nadConfigJSON(vlanTag int) (string, error) {
	octet := vlanTag - podVLANTagBase
	if octet < 1 || octet > 254 {
		return "", fmt.Errorf("vlan tag %d is outside the pod VLAN range %d-%d",
			vlanTag, podVLANTagBase+1, podVLANTagBase+254)
	}

	nadConfig := map[string]any{
		"cniVersion": "0.3.1",
		"type":       "vlan",
		"master":     k.trunkNIC,
		"vlanId":     vlanTag,
		"ipam": map[string]any{
			"type": "host-local",
			"ranges": []any{
				[]any{map[string]any{
					"subnet":     fmt.Sprintf("10.100.%d.0/24", octet),
					"rangeStart": fmt.Sprintf("10.100.%d.%d", octet, runnerRangeStartHost),
					"rangeEnd":   fmt.Sprintf("10.100.%d.%d", octet, runnerRangeEndHost),
				}},
			},
		},
	}
	configJSON, err := json.Marshal(nadConfig)
	if err != nil {
		return "", fmt.Errorf("marshal NAD config: %w", err)
	}
	return string(configJSON), nil
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
