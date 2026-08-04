// retry_reconcile_test.go -- unit tests for reconcileRetryPending.
//
// Design mirrors image_reconcile_test.go: narrow fake implementations,
// no real DB or Pushgateway, pure function tested directly.
package provisioner

import (
	"context"
	"errors"
	"testing"
)

// --------------------------------------------------------------------------
// Fakes
// --------------------------------------------------------------------------

type fakeRetryPendingDB struct {
	count    int
	queryErr error
}

func (f *fakeRetryPendingDB) CountRetryPendingJobs(_ context.Context) (int, error) {
	if f.queryErr != nil {
		return 0, f.queryErr
	}
	return f.count, nil
}

type fakeRetryPendingMetrics struct {
	setPending  []int
	pushErr     error
	pushCalls   int
}

func (f *fakeRetryPendingMetrics) SetJobRetryPending(n int) {
	f.setPending = append(f.setPending, n)
}

func (f *fakeRetryPendingMetrics) Push(_ context.Context) error {
	f.pushCalls++
	return f.pushErr
}

// --------------------------------------------------------------------------
// Tests
// --------------------------------------------------------------------------

// PublishesCount: reconciler pushes the count from the DB.
func TestReconcileRetryPending_PublishesCount(t *testing.T) {
	db := &fakeRetryPendingDB{count: 5}
	m := &fakeRetryPendingMetrics{}

	if err := reconcileRetryPending(context.Background(), db, m, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.setPending) != 1 || m.setPending[0] != 5 {
		t.Errorf("SetJobRetryPending called with %v, want [5]", m.setPending)
	}
	if m.pushCalls != 1 {
		t.Errorf("Push called %d times, want 1", m.pushCalls)
	}
}

// ZeroIsPublished: a count of 0 is published once the reconciler has run
// (zero is an honest value; the gauge must not be suppressed after first call).
func TestReconcileRetryPending_ZeroIsPublished(t *testing.T) {
	db := &fakeRetryPendingDB{count: 0}
	m := &fakeRetryPendingMetrics{}

	if err := reconcileRetryPending(context.Background(), db, m, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.setPending) != 1 || m.setPending[0] != 0 {
		t.Errorf("SetJobRetryPending called with %v, want [0]", m.setPending)
	}
}

// PushFailureDoesNotFail: a push failure is logged but does not make
// reconcileRetryPending return an error (Pushgateway down must not stop the worker).
func TestReconcileRetryPending_PushFailureDoesNotFail(t *testing.T) {
	db := &fakeRetryPendingDB{count: 2}
	m := &fakeRetryPendingMetrics{pushErr: errors.New("pushgateway down")}

	if err := reconcileRetryPending(context.Background(), db, m, nil); err != nil {
		t.Errorf("push failure should not propagate: got %v", err)
	}
	// SetJobRetryPending must still have been called.
	if len(m.setPending) != 1 {
		t.Errorf("SetJobRetryPending not called despite push failure")
	}
}

// QueryErrorDoesNotPublish: if the DB query fails, SetJobRetryPending must
// NOT be called (we must not publish a stale 0).
func TestReconcileRetryPending_QueryErrorDoesNotPublish(t *testing.T) {
	db := &fakeRetryPendingDB{queryErr: errors.New("db error")}
	m := &fakeRetryPendingMetrics{}

	err := reconcileRetryPending(context.Background(), db, m, nil)
	if err == nil {
		t.Fatal("expected error from DB failure")
	}
	if len(m.setPending) != 0 {
		t.Errorf("SetJobRetryPending called despite DB error")
	}
	if m.pushCalls != 0 {
		t.Errorf("Push called despite DB error")
	}
}

// RespectsContextCancel: a cancelled context propagates to the DB query.
func TestReconcileRetryPending_RespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling

	db := &fakeRetryPendingDB{queryErr: ctx.Err()}
	m := &fakeRetryPendingMetrics{}

	err := reconcileRetryPending(ctx, db, m, nil)
	if err == nil {
		t.Error("expected error on cancelled context")
	}
	if len(m.setPending) != 0 {
		t.Error("SetJobRetryPending called after context cancel")
	}
}
