package main

import (
	"strings"
	"testing"
	"time"
)

func TestNewJobClaimIDFencesEachClaimGeneration(t *testing.T) {
	const workerID = "worker-a"
	first := newJobClaimID(workerID)
	second := newJobClaimID(workerID)
	if first == second {
		t.Fatal("two claims from one worker process shared a fencing token")
	}
	for _, claimID := range []string{first, second} {
		if !strings.HasPrefix(claimID, workerID+":") {
			t.Fatalf("claim ID %q does not retain process identity %q", claimID, workerID)
		}
	}
}

func TestJobRunnerShutdownWaitsForInFlightHandoff(t *testing.T) {
	runner := &jobRunner{}
	started := make(chan struct{})
	release := make(chan struct{})
	if !runner.Go(func() {
		close(started)
		<-release
	}) {
		t.Fatal("runner rejected work before shutdown")
	}
	<-started

	stopped := make(chan struct{})
	go func() {
		runner.StopAndWait()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("shutdown returned before in-flight handoff completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after in-flight handoff completed")
	}
	if runner.Go(func() {}) {
		t.Fatal("runner accepted new work after shutdown began")
	}
}
