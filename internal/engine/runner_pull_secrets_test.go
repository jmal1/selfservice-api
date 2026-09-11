package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// provisionForTest runs ProvisionRunner against fake clients and returns the
// created Job.
func provisionForTest(t *testing.T, cfg K8sConfig) *batchv1.Job {
	t.Helper()

	clientset := fake.NewSimpleClientset()
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	k8s := NewK8sClientFromClients(clientset, dynClient, cfg, testLogger())

	result, err := k8s.ProvisionRunner(context.Background(), RunnerSpec{
		RunID:         "abcd1234-5678-9012-3456-789012345678",
		CallbackToken: "callback-token-abc",
		EngineURL:     cfg.EngineURL,
		VLANTag:       119,
		Workflows:     []runner.WorkflowDef{{Slug: "wf", Name: "WF", Script: "true", TimeoutSeconds: 60}},
		Target:        runner.TargetConfig{IP: "10.100.19.10", OS: "linux", Username: "student", Password: "pw"},
		Pod:           runner.PodConfig{Subnet: "10.100.19.0/24", Index: 1},
	})
	if err != nil {
		t.Fatalf("ProvisionRunner: %v", err)
	}

	job, err := clientset.BatchV1().Jobs(cfg.Namespace).Get(context.Background(), result.JobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	return job
}

// TestProvisionRunner_SetsImagePullSecrets guards the defect that made the
// Kali runner unusable the first time it was ever deployed.
//
// Every Crucible GHCR package is private. Each Helm-managed workload receives
// imagePullSecrets from .Values.imagePullSecrets, but the runner Job's pod spec
// is constructed in Go and inherited nothing. The kubelet therefore requested
// an anonymous pull token, GHCR replied 401, and the Job sat in
// ImagePullBackOff until ActiveDeadlineSeconds killed it — surfacing to the
// user only as "Run timed out after 10 minutes" with no mention of the
// registry.
//
// It stayed latent because the runner image had never built successfully
// before, so nothing had ever attempted the pull.
func TestProvisionRunner_SetsImagePullSecrets(t *testing.T) {
	cfg := testK8sConfig()
	cfg.ImagePullSecrets = []string{"ghcr-pull-secret"}

	job := provisionForTest(t, cfg)

	if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("ImagePullSecrets = %v, want exactly one entry. Without it the "+
			"kubelet pulls anonymously and GHCR returns 401 for every private "+
			"Crucible image.", job.Spec.Template.Spec.ImagePullSecrets)
	}
	if job.Spec.Template.Spec.ImagePullSecrets[0].Name != "ghcr-pull-secret" {
		t.Errorf("ImagePullSecrets[0].Name = %q, want %q", job.Spec.Template.Spec.ImagePullSecrets[0].Name, "ghcr-pull-secret")
	}
}

// TestProvisionRunner_NoImagePullSecretsWhenUnconfigured ensures an empty
// config produces no entry at all, rather than a reference to a Secret named
// "" (which the API server rejects, turning a missing setting into a hard
// provisioning failure).
func TestProvisionRunner_NoImagePullSecretsWhenUnconfigured(t *testing.T) {
	cfg := testK8sConfig()
	cfg.ImagePullSecrets = nil

	job := provisionForTest(t, cfg)

	if len(job.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("ImagePullSecrets = %v, want none when unconfigured", job.Spec.Template.Spec.ImagePullSecrets)
	}
}

// TestImagePullSecretRefs_SkipsBlanks covers the parsing edge cases that would
// otherwise produce a reference to a Secret named "".
func TestImagePullSecretRefs_SkipsBlanks(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty slice", []string{}, nil},
		{"only blanks", []string{"", "   "}, nil},
		{"trims", []string{" ghcr-pull-secret "}, []string{"ghcr-pull-secret"}},
		{"drops blanks among real", []string{"a", "", "b"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := imagePullSecretRefs(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d refs %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i].Name != tt.want[i] {
					t.Errorf("ref[%d] = %q, want %q", i, got[i].Name, tt.want[i])
				}
			}
		})
	}
}

// TestActiveDeadline_MatchesConfig pins the runner Job deadline.
//
// The plan raised it from 600s to 900s because the Kali runner image must be
// pulled cold on first use. Reverting it would reintroduce timeouts that look
// like assessment failures rather than infrastructure slowness, so the value is
// asserted explicitly rather than read back from the same constant.
func TestActiveDeadline_MatchesConfig(t *testing.T) {
	if runnerActiveDeadlineSeconds != 900 {
		t.Fatalf("runnerActiveDeadlineSeconds = %d, want 900", runnerActiveDeadlineSeconds)
	}

	job := provisionForTest(t, testK8sConfig())
	if job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("ActiveDeadlineSeconds is nil — the Job would run forever")
	}
	if *job.Spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("Job ActiveDeadlineSeconds = %d, want 900", *job.Spec.ActiveDeadlineSeconds)
	}
}

// TestEngineMainWiresImagePullSecrets is a wiring guard in the spirit of
// internal/ci/wiring_test.go: the K8sConfig field can exist and be honoured by
// ProvisionRunner while cmd/crucible-engine never populates it, which is
// exactly how this class of bug has shipped repeatedly in this repo.
func TestEngineMainWiresImagePullSecrets(t *testing.T) {
	path := filepath.Join("..", "..", "cmd", "crucible-engine", "main.go")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(body)

	if !strings.Contains(src, "ImagePullSecrets:") {
		t.Error("cmd/crucible-engine/main.go never sets K8sConfig.ImagePullSecrets, " +
			"so every runner Job pulls anonymously and fails with 401 on private images")
	}
	if !strings.Contains(src, "RUNNER_IMAGE_PULL_SECRETS") {
		t.Error("cmd/crucible-engine/main.go must read RUNNER_IMAGE_PULL_SECRETS so the " +
			"chart can configure it")
	}
	if !strings.Contains(src, `"ghcr-pull-secret"`) {
		t.Error("the default must be ghcr-pull-secret: an empty default reproduces the " +
			"original outage whenever the chart value is missing")
	}
}

// TestEngineChartWiresRunnerPullSecret guards the other half of the wiring —
// the chart must actually pass the value through to the engine.
func TestEngineChartWiresRunnerPullSecret(t *testing.T) {
	body, err := os.ReadFile(helmPath("templates", "engine-deployment.yaml"))
	if err != nil {
		t.Fatalf("read engine-deployment.yaml: %v", err)
	}
	d := string(body)

	if !strings.Contains(d, "RUNNER_IMAGE_PULL_SECRETS") {
		t.Error("engine-deployment.yaml never sets RUNNER_IMAGE_PULL_SECRETS; the runner " +
			"Job would fall back to the code default and drift from .Values.imagePullSecrets")
	}
	if !strings.Contains(d, ".Values.imagePullSecrets") {
		t.Error("RUNNER_IMAGE_PULL_SECRETS must be derived from .Values.imagePullSecrets so " +
			"the runner Job and the Helm-managed workloads cannot disagree")
	}
}
