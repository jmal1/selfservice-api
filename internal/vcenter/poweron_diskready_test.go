package vcenter

// Unit coverage for the power-on disk-readiness retry.
//
// The bug these guard: the first ISO template build powered on 234ms after
// CreateVM_Task returned and failed with "The file specified is not a virtual
// disk". vmware.log showed the flat extent had realSize 0 while the descriptor
// claimed the full capacity — the NFS datastore had not finished materializing
// the disk. Powering the identical VM on minutes later succeeded with no other
// change, which is what proved it a race rather than a bad disk.
//
// retryWhileDiskNotReady is a free function taking its delay precisely so
// these tests can run it with delay=0. A test that waited out the real 3s
// backoff would be slow enough that someone would eventually delete it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The exact string vCenter returned for the production failure, wrapped the
// way PowerOnVM wraps it. Hard-coding the real message keeps the matcher
// honest: if someone narrows isDiskNotReadyErr, this is what stops it.
const prodDiskNotReadyErr = "power on vm-13429: ServerFaultCode: The file specified is not a virtual disk"

func TestIsDiskNotReadyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the production failure", errors.New(prodDiskNotReadyErr), true},
		{"cannot open the disk", errors.New("Cannot open the disk '/vmfs/volumes/x/y.vmdk' or one of the snapshot disks it depends on"), true},
		{"extent truncation", errors.New("Size of extent in descriptor file larger than real size"), true},
		{"nil", nil, false},
		// Deliberately NOT retried: another host holds the disk. Retrying
		// hides a real problem instead of waiting out a transient one.
		{"file locked by another host", errors.New("Failed to lock the file"), false},
		{"invalid config", errors.New("ServerFaultCode: InvalidArgument: spec.guestId"), false},
		{"no host", errors.New("No host is compatible with the virtual machine"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDiskNotReadyErr(tc.err); got != tc.want {
				t.Fatalf("isDiskNotReadyErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRetryWhileDiskNotReady_SucceedsOnceDiskAppears(t *testing.T) {
	calls := 0
	err := retryWhileDiskNotReady(context.Background(), nil, "vm-1", 5, 0, func() error {
		calls++
		if calls < 3 {
			return errors.New(prodDiskNotReadyErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success once the disk became readable, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("powered on %d times, want 3 (2 failures then success)", calls)
	}
}

// TestRetryWhileDiskNotReady_DoesNotRetryOtherErrors is the guard that keeps
// this from becoming a blanket retry. A misconfigured VM must fail on the
// first attempt with its own error, not after a minute of pointless retries
// reporting a disk problem it does not have.
func TestRetryWhileDiskNotReady_DoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	want := errors.New("ServerFaultCode: InvalidArgument: spec.guestId")
	err := retryWhileDiskNotReady(context.Background(), nil, "vm-1", 5, 0, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want the original error unwrapped and unretried", err)
	}
	if calls != 1 {
		t.Fatalf("attempted %d times, want exactly 1 for a non-disk error", calls)
	}
}

func TestRetryWhileDiskNotReady_GivesUpAndKeepsTheCause(t *testing.T) {
	calls := 0
	err := retryWhileDiskNotReady(context.Background(), nil, "vm-1", 4, 0, func() error {
		calls++
		return errors.New(prodDiskNotReadyErr)
	})
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
	if calls != 4 {
		t.Fatalf("attempted %d times, want 4", calls)
	}
	// The final message must still carry the underlying fault; a bare
	// "gave up" would put us back where this started, with a timeout whose
	// cause was thrown away.
	if !strings.Contains(err.Error(), "not a virtual disk") {
		t.Fatalf("give-up error lost the underlying cause: %v", err)
	}
	if !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("give-up error should say how hard it tried: %v", err)
	}
}

func TestRetryWhileDiskNotReady_HonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := retryWhileDiskNotReady(ctx, nil, "vm-1", 1000, 5*time.Millisecond, func() error {
		calls++
		return errors.New(prodDiskNotReadyErr)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if calls >= 1000 {
		t.Fatalf("ignored cancellation and burned every attempt (%d)", calls)
	}
}

// TestPowerOnDiskReadyBudget guards the constants themselves. The observed
// race resolved somewhere between 234ms and a few minutes; a budget under ~30s
// would be a coin flip on a busy NAS, and the whole point of retrying rather
// than sleeping was to cover the slow case.
func TestPowerOnDiskReadyBudget(t *testing.T) {
	budget := time.Duration(powerOnDiskReadyAttempts-1) * powerOnDiskReadyDelay
	if budget < 30*time.Second {
		t.Fatalf("power-on disk-ready budget is %s; too short to ride out a slow datastore allocation", budget)
	}
	if budget > 5*time.Minute {
		t.Fatalf("power-on disk-ready budget is %s; a genuinely bad disk should fail well before this", budget)
	}
}
