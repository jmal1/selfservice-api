package handlers

import (
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestHideUnreadyPodCredentials(t *testing.T) {
	verifiedAt := time.Now()
	verifiedMoref := "vm-verified"
	pod := &models.Pod{
		VMs: []models.PodVM{
			{
				Status:            models.VMStatusConfiguring,
				TemplateKind:      models.TemplateKindCloneWithCustomize,
				DefaultUsername:   "student",
				DefaultPassword:   "REPLACE_WITH_BUILD_PASSWORD",
				GeneratedUsername: "student",
				GeneratedPassword: "NotYetVerified1!",
			},
			{
				Status:                       models.VMStatusRunning,
				TemplateKind:                 models.TemplateKindCloneWithCustomize,
				GuestCredentialsVerifiedAt:   &verifiedAt,
				VCenterVMID:                  &verifiedMoref,
				GuestCredentialsVerifiedVMID: &verifiedMoref,
				DefaultUsername:              "student",
				DefaultPassword:              "REPLACE_WITH_BUILD_PASSWORD",
				GeneratedUsername:            "student",
				GeneratedPassword:            "VerifiedPerPod1!",
			},
			{
				Status:            models.VMStatusError,
				TemplateKind:      models.TemplateKindCloneNoCustomize,
				DefaultUsername:   "Administrator",
				DefaultPassword:   "StaticOnly1!",
				GeneratedUsername: "Administrator",
				GeneratedPassword: "StaticOnly1!",
			},
		},
	}

	hideUnreadyPodCredentials(pod)

	for _, index := range []int{0, 2} {
		vm := pod.VMs[index]
		if vm.DefaultUsername != "" || vm.DefaultPassword != "" ||
			vm.GeneratedUsername != "" || vm.GeneratedPassword != "" {
			t.Fatalf("VM %d exposed credentials in state %q: %+v", index, vm.Status, vm)
		}
	}

	running := pod.VMs[1]
	if running.GeneratedUsername != "student" ||
		running.GeneratedPassword != "VerifiedPerPod1!" {
		t.Fatalf("running VM credentials were redacted: %+v", running)
	}
}

func TestHideUnreadyPodCredentialsFromList(t *testing.T) {
	verifiedAt := time.Now()
	verifiedMoref := "vm-list-verified"
	pods := []models.Pod{
		{
			VMs: []models.PodVM{{
				Status:            models.VMStatusPending,
				TemplateKind:      models.TemplateKindCloneWithCustomize,
				GeneratedUsername: "student",
				GeneratedPassword: "Pending1!",
			}},
		},
		{
			VMs: []models.PodVM{{
				Status:                       models.VMStatusRunning,
				TemplateKind:                 models.TemplateKindCloneWithCustomize,
				GuestCredentialsVerifiedAt:   &verifiedAt,
				VCenterVMID:                  &verifiedMoref,
				GuestCredentialsVerifiedVMID: &verifiedMoref,
				GeneratedUsername:            "Student",
				GeneratedPassword:            "Verified1!",
			}},
		},
	}

	hideUnreadyPodCredentialsFromList(pods)

	if pods[0].VMs[0].GeneratedPassword != "" {
		t.Fatal("list response exposed credentials for an unready VM")
	}
	if pods[1].VMs[0].GeneratedPassword != "Verified1!" {
		t.Fatal("list response redacted credentials for a running VM")
	}
}

func TestHideCredentialsWhenAcceptanceBelongsToDestroyedClone(t *testing.T) {
	oldMoref := "vm-old"
	replacementMoref := "vm-replacement"
	verifiedAt := time.Now()
	pod := &models.Pod{VMs: []models.PodVM{{
		Status:                       models.VMStatusRunning,
		TemplateKind:                 models.TemplateKindCloneWithCustomize,
		VCenterVMID:                  &replacementMoref,
		GuestCredentialsVerifiedAt:   &verifiedAt,
		GuestCredentialsVerifiedVMID: &oldMoref,
		GeneratedUsername:            "student",
		GeneratedPassword:            "SamePassword1!",
	}}}

	hideUnreadyPodCredentials(pod)

	if pod.VMs[0].GeneratedPassword != "" {
		t.Fatal("replacement clone exposed credentials accepted only by the destroyed clone")
	}
}

func TestHideUnverifiedLegacyRunningCustomizedCredentials(t *testing.T) {
	pod := &models.Pod{VMs: []models.PodVM{{
		Status:            models.VMStatusRunning,
		TemplateKind:      models.TemplateKindCloneWithCustomize,
		GeneratedUsername: "student",
		GeneratedPassword: "LegacyUnverified1!",
	}}}

	hideUnreadyPodCredentials(pod)

	if pod.VMs[0].GeneratedUsername != "" || pod.VMs[0].GeneratedPassword != "" {
		t.Fatal("legacy running customized VM exposed credentials without durable acceptance")
	}
}

func TestStaticRunningCredentialsDoNotRequireGeneratedAcceptance(t *testing.T) {
	pod := &models.Pod{VMs: []models.PodVM{{
		Status:            models.VMStatusRunning,
		TemplateKind:      models.TemplateKindCloneNoCustomize,
		GeneratedUsername: "Administrator",
		GeneratedPassword: "Static1!",
	}}}

	hideUnreadyPodCredentials(pod)

	if pod.VMs[0].GeneratedPassword != "Static1!" {
		t.Fatal("static VM credentials were incorrectly gated on generated acceptance")
	}
}
