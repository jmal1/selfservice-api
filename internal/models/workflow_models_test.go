package models

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRunJSONIncludesAdminAttributionKeys(t *testing.T) {
	playlistID := uuid.New()
	run := Run{
		ID:                     uuid.New(),
		PodID:                  uuid.New(),
		PlaylistID:             &playlistID,
		TriggeredBy:            uuid.New(),
		TargetVMName:           "vm-01",
		TargetVMIP:             "192.0.2.50",
		TriggeredByUsername:    "instructor1",
		TriggeredByDisplayName: "Instructor One",
		PodName:                "pod-01",
		PodStatus:              "destroyed",
		PlaylistName:           "baseline-assessment",
		Status:                 RunStatusCompleted,
		CreatedAt:              time.Now().UTC(),
		UpdatedAt:              time.Now().UTC(),
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantKeys := []string{
		"triggered_by_username",
		"triggered_by_display_name",
		"pod_name",
		"pod_status",
		"playlist_name",
	}
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing JSON key %q; got keys %v", key, got)
		}
	}
}
