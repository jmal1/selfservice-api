package provisioner

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func credentialProvisioningWiringInvariant(createSrc, vmOpsSrc string) error {
	createResolve := strings.Index(createSrc, "provisionedPodVMCredentials(")
	createPersist := strings.Index(createSrc, "p.db.UpdatePodVMCredentials(")
	createClone := strings.Index(createSrc, "executeDurableVMClone(")
	createReady := strings.Index(createSrc, "waitForPodVMCredentialReady(")
	if createResolve < 0 || createPersist < 0 || createClone < 0 ||
		createResolve > createPersist || createPersist > createClone {
		return errors.New("pod_create must resolve and persist the reusable credential before clone submission")
	}
	if createReady < 0 ||
		strings.Index(createSrc[createReady:], "models.VMStatusRunning") < 0 {
		return errors.New("pod_create must authenticate the generated credential before marking the VM running")
	}
	if !strings.Contains(createSrc, "Password:     customizationPassword") {
		return errors.New("pod_create clone params are not constrained to the customization-only password")
	}

	vmResolve := strings.Index(vmOpsSrc, "provisionedPodVMCredentials(")
	vmPersist := strings.Index(vmOpsSrc, "p.db.UpdatePodVMCredentials(")
	vmClone := strings.Index(vmOpsSrc, "executeDurableVMClone(")
	vmReady := strings.Index(vmOpsSrc, "waitForPodVMCredentialReady(")
	if vmResolve < 0 || vmPersist < 0 || vmClone < 0 ||
		vmResolve > vmPersist || vmPersist > vmClone {
		return errors.New("vm_add must resolve and persist the reusable credential before clone submission")
	}
	if vmReady < 0 ||
		strings.Index(vmOpsSrc[vmReady:], "models.VMStatusRunning") < 0 {
		return errors.New("vm_add must authenticate the generated credential before marking the VM running")
	}
	if !strings.Contains(vmOpsSrc, "Password:     customizationPassword") {
		return errors.New("vm_add clone params are not constrained to the customization-only password")
	}
	return nil
}

func TestCredentialProvisioningWiringIsLoadBearing(t *testing.T) {
	createBody, err := os.ReadFile("create.go")
	if err != nil {
		t.Fatal(err)
	}
	vmOpsBody, err := os.ReadFile("vm_ops.go")
	if err != nil {
		t.Fatal(err)
	}
	createSrc := string(createBody)
	vmOpsSrc := string(vmOpsBody)
	if err := credentialProvisioningWiringInvariant(createSrc, vmOpsSrc); err != nil {
		t.Fatal(err)
	}

	sabotages := map[string][2]string{
		"pod-create-persistence": {
			strings.Replace(createSrc, "p.db.UpdatePodVMCredentials(", "removedCredentialPersistence(", 1),
			vmOpsSrc,
		},
		"pod-create-readiness": {
			strings.Replace(createSrc, "waitForPodVMCredentialReady(", "removedCredentialReadiness(", 1),
			vmOpsSrc,
		},
		"vm-add-persistence": {
			createSrc,
			strings.Replace(vmOpsSrc, "p.db.UpdatePodVMCredentials(", "removedCredentialPersistence(", 1),
		},
		"vm-add-readiness": {
			createSrc,
			strings.Replace(vmOpsSrc, "waitForPodVMCredentialReady(", "removedCredentialReadiness(", 1),
		},
	}
	for name, src := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := credentialProvisioningWiringInvariant(src[0], src[1]); err == nil {
				t.Fatal("credential invariant survived sabotage")
			}
		})
	}
}
