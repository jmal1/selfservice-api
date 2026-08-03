package engine

import (
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/runner"
)

// TestRunnerConfigMount_DoesNotShadowInstallRoot guards the defect that stopped
// the runner from executing a single action, ever.
//
// The image installs itself under /opt/crucible: the binary at
// /opt/crucible/bin/crucible-runner (on PATH, and the image ENTRYPOINT) and the
// action library at /opt/crucible/lib/actions.sh. The engine mounted the config
// Secret at /opt/crucible, and a volume mounted at a directory REPLACES that
// directory. So the container's own entrypoint disappeared at container-init
// time and every run failed with:
//
//	exec: "crucible-runner": executable file not found in $PATH
//
// Nothing else could catch this. The image is correct in isolation (verified by
// contract_test.go against the Dockerfile) and the pod spec is correct in
// isolation; only their composition at runtime is broken, and no unit test
// composes them. It survived until a real Job was scheduled on a real node.
func TestRunnerConfigMount_DoesNotShadowInstallRoot(t *testing.T) {
	job := provisionForTest(t, K8sConfig{
		Namespace:   "selfservice",
		RunnerImage: "ghcr.io/jmal1/selfservice-crucible-runner:latest",
		RunnerNode:  "k3sv03",
		TrunkNIC:    "ens224",
		EngineURL:   "http://engine:8081",
	})

	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 volume mount, got %d", len(mounts))
	}
	m := mounts[0]

	// The install root itself must never be a mount target.
	installRoot := "/opt/crucible"
	if strings.TrimSuffix(m.MountPath, "/") == installRoot {
		t.Fatalf("volume is mounted at the install root %q, which replaces the whole "+
			"directory and deletes the runner binary (%s/bin/crucible-runner) and the "+
			"action library (%s/lib/actions.sh) from the running container. The Job then "+
			"fails at container init with:\n"+
			"  exec: \"crucible-runner\": executable file not found in $PATH\n"+
			"Mount the single config file with SubPath instead.",
			m.MountPath, installRoot, installRoot)
	}

	// Anything mounted *under* the install root must be a single file, otherwise
	// it shadows a subdirectory the image needs.
	if strings.HasPrefix(m.MountPath, installRoot+"/") && m.SubPath == "" {
		t.Errorf("mount at %q is under the install root but has no SubPath, so it "+
			"replaces that directory rather than adding a file to it", m.MountPath)
	}
}

// TestRunnerConfigMount_MatchesRunnerContract pins the mount to the path the
// runner binary actually opens. A mount that does not shadow anything but lands
// in the wrong place fails just as hard, only later and with a vaguer error.
func TestRunnerConfigMount_MatchesRunnerContract(t *testing.T) {
	job := provisionForTest(t, K8sConfig{
		Namespace:   "selfservice",
		RunnerImage: "img",
		RunnerNode:  "k3sv03",
		TrunkNIC:    "ens224",
		EngineURL:   "http://engine:8081",
	})
	m := job.Spec.Template.Spec.Containers[0].VolumeMounts[0]

	if m.MountPath != runner.DefaultConfigPath {
		t.Errorf("config mounted at %q but the runner reads %q; the runner would exit "+
			"immediately with a missing-config error", m.MountPath, runner.DefaultConfigPath)
	}
	if !m.ReadOnly {
		t.Error("runner config mount should be read-only; it carries the callback token")
	}

	// SubPath selects a key inside the Secret. If it does not match the key the
	// engine writes, Kubernetes mounts an empty dir over the config path and the
	// runner reports "no such file" for a path that appears to exist -- a
	// genuinely confusing failure.
	if m.SubPath != runnerConfigSecretKey {
		t.Errorf("SubPath = %q but the Secret key is %q; these must match",
			m.SubPath, runnerConfigSecretKey)
	}
}
