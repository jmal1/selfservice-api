package main

import (
	"strings"
	"sync"
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
		runner.StopAndWait(time.Second)
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

func TestJobRunnerShutdownIsBounded(t *testing.T) {
	runner := &jobRunner{}
	release := make(chan struct{})
	if !runner.Go(func() { <-release }) {
		t.Fatal("runner rejected work before shutdown")
	}
	started := time.Now()
	if runner.StopAndWait(20 * time.Millisecond) {
		t.Fatal("shutdown reported completion while work remained blocked")
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded shutdown took %s", elapsed)
	}
	close(release)
}

type blockingCloser struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (c *blockingCloser) Close() {
	c.once.Do(func() { close(c.started) })
	<-c.release
}

func TestCloseBeforeDeadlineIsBounded(t *testing.T) {
	resource := &blockingCloser{started: make(chan struct{}), release: make(chan struct{})}
	start := time.Now()
	if closeBeforeDeadline(resource, 20*time.Millisecond) {
		t.Fatal("close reported completion while resource remained blocked")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded close took %s", elapsed)
	}
	<-resource.started
	close(resource.release)
}
