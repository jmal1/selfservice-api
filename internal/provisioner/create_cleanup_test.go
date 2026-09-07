package provisioner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func neverAppliedCreateFinalizeOrderOK(commonCleanupBody string) bool {
	markVMTerminal := strings.Index(commonCleanupBody, "p.db.UpdatePodVMStatusFrom(")
	finalizeNeverApplied := strings.Index(commonCleanupBody, "FinalizeNeverAppliedPodCreate(")
	markCompensation := strings.Index(commonCleanupBody, "MarkJobCompensationCompleted(")
	return markVMTerminal >= 0 &&
		finalizeNeverApplied >= 0 &&
		markCompensation >= 0 &&
		finalizeNeverApplied > markVMTerminal &&
		markCompensation > finalizeNeverApplied
}

func TestNeverAppliedCreateFinalizeWiring(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "create.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	commonCleanupStart := strings.Index(src, "func (p *Provisioner) cleanupPodCreateResources(")
	if commonCleanupStart < 0 {
		t.Fatal("cleanupPodCreateResources not found")
	}
	commonCleanupEnd := strings.Index(src[commonCleanupStart+1:], "\nfunc ")
	if commonCleanupEnd < 0 {
		t.Fatal("could not isolate cleanupPodCreateResources")
	}
	commonCleanupBody := src[commonCleanupStart : commonCleanupStart+1+commonCleanupEnd]
	if !neverAppliedCreateFinalizeOrderOK(commonCleanupBody) {
		t.Fatal("never-applied create finalize wiring is missing or out of order")
	}

	sabotaged := strings.ReplaceAll(commonCleanupBody, "FinalizeNeverAppliedPodCreate(", "RemovedNeverAppliedFinalize(")
	if neverAppliedCreateFinalizeOrderOK(sabotaged) {
		t.Fatal("removing FinalizeNeverAppliedPodCreate must fail the wiring check")
	}
}
