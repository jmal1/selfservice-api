package handlers

import (
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestHideUnreadyPodCredentials(t *testing.T) {
	pod := &models.Pod{
		VMs: []models.PodVM{
			{
				Status:            models.VMStatusConfiguring,
				DefaultUsername:   "student",
				DefaultPassword:   "Changeme123!",
				GeneratedUsername: "student",
				GeneratedPassword: "NotYetVerified1!",
			},
			{
				Status:            models.VMStatusRunning,
				DefaultUsername:   "student",
				DefaultPassword:   "Changeme123!",
				GeneratedUsername: "student",
				GeneratedPassword: "VerifiedPerPod1!",
			},
			{
				Status:            models.VMStatusError,
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
	pods := []models.Pod{
		{
			VMs: []models.PodVM{{
				Status:            models.VMStatusPending,
				GeneratedUsername: "student",
				GeneratedPassword: "Pending1!",
			}},
		},
		{
			VMs: []models.PodVM{{
				Status:            models.VMStatusRunning,
				GeneratedUsername: "Student",
				GeneratedPassword: "Verified1!",
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
