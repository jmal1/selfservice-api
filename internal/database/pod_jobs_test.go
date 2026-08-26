package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestSerializedPodJobTypeInventoryIsExhaustive(t *testing.T) {
	want := []string{
		models.JobTypePodCreate,
		models.JobTypePodDestroy,
		models.JobTypeVMAdd,
		models.JobTypeVMDestroy,
		models.JobTypeVMStart,
		models.JobTypeVMStop,
		models.JobTypeVMRestart,
		models.JobTypeVMReset,
		models.JobTypeVMSnapshot,
		models.JobTypeVMRevert,
		models.JobTypeVMSnapshotDelete,
		models.JobTypeVMSuspend,
	}
	if len(serializedPodJobTypes) != len(want) {
		t.Fatalf("serialized pod job inventory = %v, want %v", serializedPodJobTypes, want)
	}
	for i := range want {
		if serializedPodJobTypes[i] != want[i] {
			t.Fatalf("serialized pod job inventory[%d] = %q, want %q", i, serializedPodJobTypes[i], want[i])
		}
	}
}

func TestGenericCreateJobRejectsEverySerializedPodJobType(t *testing.T) {
	q := &Queries{}
	payload, err := json.Marshal(map[string]string{"pod_id": uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	for _, jobType := range serializedPodJobTypes {
		t.Run(jobType, func(t *testing.T) {
			_, err := q.CreateJob(context.Background(), jobType, payload)
			if !errors.Is(err, ErrPodJobRequiresSerialization) {
				t.Fatalf("CreateJob(%q) error = %v, want %v", jobType, err, ErrPodJobRequiresSerialization)
			}
			_, err = q.CreateJobTx(context.Background(), nil, jobType, payload)
			if !errors.Is(err, ErrPodJobRequiresSerialization) {
				t.Fatalf("CreateJobTx(%q) error = %v, want %v", jobType, err, ErrPodJobRequiresSerialization)
			}
		})
	}
}

func TestTerminalPodStatusInventoryRejectsMutatorJobs(t *testing.T) {
	for _, status := range []string{
		models.PodStatusDestroying,
		models.PodStatusDestroyFailed,
		models.PodStatusDestroyed,
		models.PodStatusError,
		"cancelled",
	} {
		if !podRejectsMutatorJob(status) {
			t.Errorf("status %q unexpectedly accepts mutator jobs", status)
		}
	}
	for _, status := range []string{
		models.PodStatusPending,
		models.PodStatusProvisioning,
		models.PodStatusActive,
	} {
		if podRejectsMutatorJob(status) {
			t.Errorf("status %q unexpectedly rejects mutator jobs", status)
		}
	}
}
