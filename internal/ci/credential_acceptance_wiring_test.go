package ci

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func credentialAcceptanceCallSites(createSrc, vmOpsSrc string) error {
	if strings.Count(createSrc, "enforcePodVMCredentialAcceptance(") != 2 {
		return errors.New("pod_create must gate both fresh and already-active recovery paths")
	}
	if strings.Count(vmOpsSrc, "enforcePodVMCredentialAcceptance(") != 2 {
		return errors.New("vm_add must gate both fresh and already-running recovery paths")
	}
	return nil
}

func removeNth(src, needle string, n int) string {
	offset := 0
	for i := 0; i <= n; i++ {
		index := strings.Index(src[offset:], needle)
		if index < 0 {
			return src
		}
		offset += index
		if i == n {
			return src[:offset] + "removedCredentialAcceptance(" + src[offset+len(needle):]
		}
		offset += len(needle)
	}
	return src
}

func TestCredentialAcceptanceProductionCallSitesAreLoadBearing(t *testing.T) {
	root := findRepoRoot(t)
	createBody, err := os.ReadFile(filepath.Join(root, "internal", "provisioner", "create.go"))
	if err != nil {
		t.Fatal(err)
	}
	vmOpsBody, err := os.ReadFile(filepath.Join(root, "internal", "provisioner", "vm_ops.go"))
	if err != nil {
		t.Fatal(err)
	}
	createSrc := string(createBody)
	vmOpsSrc := string(vmOpsBody)
	if err := credentialAcceptanceCallSites(createSrc, vmOpsSrc); err != nil {
		t.Fatal(err)
	}

	for name, sources := range map[string][2]string{
		"pod-create-fresh-call-removed": {
			removeNth(createSrc, "enforcePodVMCredentialAcceptance(", 0),
			vmOpsSrc,
		},
		"pod-create-recovery-call-removed": {
			removeNth(createSrc, "enforcePodVMCredentialAcceptance(", 1),
			vmOpsSrc,
		},
		"vm-add-recovery-call-removed": {
			createSrc,
			removeNth(vmOpsSrc, "enforcePodVMCredentialAcceptance(", 0),
		},
		"vm-add-fresh-call-removed": {
			createSrc,
			removeNth(vmOpsSrc, "enforcePodVMCredentialAcceptance(", 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := credentialAcceptanceCallSites(sources[0], sources[1]); err == nil {
				t.Fatal("credential acceptance wiring invariant survived production call-site sabotage")
			}
		})
	}
}

func queryFunctionBody(src, name string) string {
	start := strings.Index(src, "func (q *Queries) "+name+"(")
	if start < 0 {
		return ""
	}
	rest := src[start:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

func cloneIdentityClearInvariant(fn string) error {
	for _, marker := range []string{
		"guest_credentials_verified_at",
		"guest_credentials_verified_vm_id",
		"NULL",
	} {
		if !strings.Contains(fn, marker) {
			return errors.New("clone identity change does not clear " + marker)
		}
	}
	return nil
}

func TestCredentialAcceptanceClearsOnCloneIdentityChanges(t *testing.T) {
	root := findRepoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "database", "queries.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, name := range []string{
		"UpdatePodVM",
		"UpdatePodVMFrom",
		"AdoptPodVMClone",
		"CompleteVMCloneCleanup",
		"ClearPodVMVCenterReference",
	} {
		t.Run(name, func(t *testing.T) {
			fn := queryFunctionBody(src, name)
			if fn == "" {
				t.Fatalf("%s not found", name)
			}
			if err := cloneIdentityClearInvariant(fn); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			sabotaged := strings.ReplaceAll(fn, "guest_credentials_verified_at", "removed_acceptance_marker")
			if err := cloneIdentityClearInvariant(sabotaged); err == nil {
				t.Fatalf("%s marker-clear invariant survived sabotage", name)
			}
		})
	}

	mark := queryFunctionBody(src, "MarkPodVMCredentialsVerified")
	for _, required := range []string{
		"guest_credentials_verified_vm_id = $2",
		"vcenter_vm_id = $2",
		"generated_username = $3",
		"generated_password = $4",
	} {
		if !strings.Contains(mark, required) {
			t.Fatalf("credential acceptance marker is not bound to exact clone and pair: missing %q", required)
		}
	}

	updateCredentials := queryFunctionBody(src, "UpdatePodVMCredentials")
	for _, required := range []string{
		"guest_credentials_verified_at = CASE",
		"guest_credentials_verified_vm_id = CASE",
		"ELSE NULL",
	} {
		if !strings.Contains(updateCredentials, required) {
			t.Fatalf("credential pair change leaves partial acceptance state: missing %q", required)
		}
	}
}

func TestTemplateAcceptanceInvalidatesOnCredentialOrSourceChanges(t *testing.T) {
	root := findRepoRoot(t)
	queryBody, err := os.ReadFile(filepath.Join(root, "internal", "database", "queries.go"))
	if err != nil {
		t.Fatal(err)
	}
	replicaBody, err := os.ReadFile(filepath.Join(root, "internal", "database", "placements.go"))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"UpdateTemplate":              queryFunctionBody(string(queryBody), "UpdateTemplate"),
		"SetTemplateVCenterVM":        queryFunctionBody(string(queryBody), "SetTemplateVCenterVM"),
		"CreateTemplateSourceReplica": queryFunctionBody(string(replicaBody), "CreateTemplateSourceReplica"),
		"DeleteTemplateSourceReplica": queryFunctionBody(string(replicaBody), "DeleteTemplateSourceReplica"),
	} {
		if !strings.Contains(body, "guest_credentials_verified_at") ||
			!strings.Contains(body, "NULL") {
			t.Fatalf("%s does not invalidate template credential acceptance", name)
		}
	}
}
