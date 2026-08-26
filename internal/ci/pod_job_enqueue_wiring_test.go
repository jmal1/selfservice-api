package ci

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This is the exhaustive production producer inventory for jobs that target a
// pod or pod VM. vm_suspend has no enqueue producer today; it remains protected
// because the worker still accepts the durable job type.
var expectedSerializedPodJobProducerInventory = map[string][]string{
	"pod_create": {
		"internal/api/handlers/handlers.go:CreatePodCreateJobTx",
		"internal/api/handlers/blueprints.go:CreatePodCreateJobTx",
	},
	"pod_destroy": {
		"internal/api/handlers/handlers.go:CreatePodDestroyJob",
		"internal/provisioner/expiration.go:CreateExpiredPodDestroyJob",
		"internal/provisioner/destroy.go:RequeueFailedPodDestroyJob",
		"internal/provisioner/vm_ops.go:CreateEmptyPodDestroyJob",
	},
	"vm_add":             {"internal/api/handlers/handlers.go:CreateVMAddJob"},
	"vm_destroy":         {"internal/api/handlers/handlers.go:CreateVMJob"},
	"vm_start":           {"internal/api/handlers/handlers.go:CreateVMJob"},
	"vm_stop":            {"internal/api/handlers/handlers.go:CreateVMJob"},
	"vm_restart":         {"internal/api/handlers/handlers.go:CreateVMJob"},
	"vm_reset":           {"internal/api/handlers/handlers.go:CreateVMJob"},
	"vm_snapshot":        {"internal/api/handlers/snapshots.go:CreateVMJob"},
	"vm_revert":          {"internal/api/handlers/snapshots.go:CreateVMJob"},
	"vm_snapshot_delete": {"internal/api/handlers/snapshots.go:CreateVMJob"},
	"vm_suspend":         {},
}

var expectedSerializedBoundaryCalls = map[string]map[string]int{
	"internal/api/handlers/handlers.go": {
		"CreatePodCreateJobTx": 1,
		"CreatePodDestroyJob":  1,
		"CreateVMAddJob":       1,
		"CreateVMJob":          2,
		"ExtendPod":            2,
	},
	"internal/api/handlers/blueprints.go": {
		"CreatePodCreateJobTx": 1,
	},
	"internal/api/handlers/snapshots.go": {
		"CreateVMJob": 4,
	},
	"internal/provisioner/expiration.go": {
		"CreateExpiredPodDestroyJob": 1,
	},
	"internal/provisioner/destroy.go": {
		"RequeueFailedPodDestroyJob": 1,
		"PreparePodDestroy":          1,
	},
	"internal/provisioner/vm_ops.go": {
		"CreateEmptyPodDestroyJob": 1,
	},
}

func TestSerializedPodJobProducerInventoryUsesOnlySharedBoundary(t *testing.T) {
	root := findRepoRoot(t)
	for jobType, producers := range expectedSerializedPodJobProducerInventory {
		for _, producer := range producers {
			parts := strings.SplitN(producer, ":", 2)
			if len(parts) != 2 {
				t.Fatalf("invalid %s producer inventory entry %q", jobType, producer)
			}
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(parts[0])))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), parts[1]+"(") {
				t.Errorf("%s producer %s no longer uses %s", jobType, parts[0], parts[1])
			}
		}
	}

	actual := make(map[string]map[string]int)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, method := range []string{
			"CreatePodCreateJobTx",
			"CreatePodDestroyJob",
			"CreateExpiredPodDestroyJob",
			"CreateEmptyPodDestroyJob",
			"RequeueFailedPodDestroyJob",
			"PreparePodDestroy",
			"CreateVMAddJob",
			"CreateVMJob",
			"ExtendPod",
		} {
			count := strings.Count(string(body), "."+method+"(")
			if count == 0 {
				continue
			}
			if actual[rel] == nil {
				actual[rel] = make(map[string]int)
			}
			actual[rel][method] = count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(expectedSerializedBoundaryCalls) {
		t.Fatalf("serialized boundary producer files = %v, want %v", actual, expectedSerializedBoundaryCalls)
	}
	for file, methods := range expectedSerializedBoundaryCalls {
		if len(actual[file]) != len(methods) {
			t.Errorf("%s serialized boundary calls = %v, want exactly %v", file, actual[file], methods)
		}
		for method, want := range methods {
			if got := actual[file][method]; got != want {
				t.Errorf("%s has %d %s calls, want %d; update the exhaustive producer inventory intentionally", file, got, method, want)
			}
		}
	}
}

func TestPodExtensionPersistenceUsesOnlySerializedBoundary(t *testing.T) {
	root := findRepoRoot(t)
	attestationInsert := regexp.MustCompile(`(?is)\binsert\s+into\s+pod_attestations\b`)
	expiryUpdate := regexp.MustCompile(`(?is)\bupdate\s+pods\s+set\s+expires_at\b`)
	actual := make(map[string]int)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), ".CreatePodAttestation(") ||
			strings.Contains(string(body), ".UpdatePodExpiry(") {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s bypasses the serialized pod extension boundary", filepath.ToSlash(rel))
		}
		count := len(attestationInsert.FindAll(body, -1)) + len(expiryUpdate.FindAll(body, -1))
		if count == 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(rel)] = count
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExactFileCounts(t, "pod extension persistence", actual, map[string]int{
		"internal/database/pod_jobs.go": 2,
	})
}

func TestProductionGenericAndRawJobInsertionInventoryIsStable(t *testing.T) {
	root := findRepoRoot(t)
	insertJobsPattern := regexp.MustCompile(`(?is)\binsert\s+into\s+jobs\b`)
	expectedGeneric := map[string]int{
		"internal/api/handlers/images.go":           2,
		"internal/api/handlers/templates_wizard.go": 1,
	}
	expectedRawSQL := map[string]int{
		"internal/database/queries.go":                 2,
		"internal/database/template_health.go":         1,
		"internal/database/template_replica_builds.go": 2,
	}
	expectedPrivateInsertHelper := map[string]int{
		"internal/database/queries.go":  3,
		"internal/database/pod_jobs.go": 4,
	}
	actualGeneric := make(map[string]int)
	actualRawSQL := make(map[string]int)
	actualPrivateInsertHelper := make(map[string]int)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if count := strings.Count(string(body), ".CreateJob(") + strings.Count(string(body), ".CreateJobTx("); count > 0 {
			actualGeneric[rel] = count
		}
		if count := len(insertJobsPattern.FindAll(body, -1)); count > 0 {
			actualRawSQL[rel] = count
		}
		if count := strings.Count(string(body), "createJob("); count > 0 {
			actualPrivateInsertHelper[rel] = count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExactFileCounts(t, "generic job insertion", actualGeneric, expectedGeneric)
	assertExactFileCounts(t, "raw jobs SQL", actualRawSQL, expectedRawSQL)
	assertExactFileCounts(t, "private createJob helper", actualPrivateInsertHelper, expectedPrivateInsertHelper)
}

func assertExactFileCounts(t *testing.T, name string, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s files = %v, want %v", name, got, want)
	}
	for file, count := range want {
		if got[file] != count {
			t.Errorf("%s count for %s = %d, want %d", name, file, got[file], count)
		}
	}
}
