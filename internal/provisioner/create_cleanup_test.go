package provisioner

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreatePodPayloadCleanupOnlyRoundTrip(t *testing.T) {
	want := CreatePodPayload{PodID: uuid.New(), CleanupOnly: true}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got CreatePodPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CleanupOnly || got.PodID != want.PodID {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestPodCreateCleanupRetryTypeCannotBeSpoofedByMessage(t *testing.T) {
	typed := newPodCreateCleanupRetryError("rollback", []error{errors.New("cleanup failed")})
	if !isPodCreateCleanupRetry(typed) {
		t.Fatal("typed cleanup error was not recognized")
	}
	if isPodCreateCleanupRetry(errors.New(typed.Error())) {
		t.Fatal("plain error text was allowed to mark a retry cleanup-only")
	}
}
