package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeUploadDB implements imageUploadsDB for tests.
type fakeUploadDB struct {
	count int
	err   error
}

func (f *fakeUploadDB) CountStuckImageUploads(_ context.Context, _ time.Duration) (int, error) {
	return f.count, f.err
}

// fakeUploadMetrics implements imageUploadsMetrics for tests. It records the
// last value passed to SetImageUploadsStuck and a call count so tests can
// assert both what was published and whether the publish happened at all,
// plus the number of Pushes so tests can prove the gauge actually left the
// process rather than merely being set in memory.
type fakeUploadMetrics struct {
	lastValue int
	calls     int
	pushes    int
	pushErr   error
}

func (f *fakeUploadMetrics) SetImageUploadsStuck(n int) {
	f.calls++
	f.lastValue = n
}

func (f *fakeUploadMetrics) Push(_ context.Context) error {
	f.pushes++
	return f.pushErr
}

// TestReconcileStuckUploads_PublishesCount verifies that when the DB reports N
// stuck uploads, the gauge is set to exactly N. A wrong value here would cause
// the alert threshold to fire incorrectly or silently under-count a leak.
func TestReconcileStuckUploads_PublishesCount(t *testing.T) {
	db := &fakeUploadDB{count: 5}
	m := &fakeUploadMetrics{}

	n, err := reconcileStuckUploads(context.Background(), db, m, 30*time.Minute, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Errorf("returned count = %d, want 5", n)
	}
	if m.calls != 1 {
		t.Errorf("SetImageUploadsStuck called %d times, want 1", m.calls)
	}
	if m.lastValue != 5 {
		t.Errorf("gauge value = %d, want 5; exact count is required for alert accuracy", m.lastValue)
	}
	if m.pushes != 1 {
		t.Errorf("Push called %d times, want 1; setting the gauge without pushing leaves it inside the worker and the metric never reaches Prometheus", m.pushes)
	}
}

// TestReconcileStuckUploads_ZeroIsPublished verifies that a zero count is
// pushed even when there are no stuck uploads. Skipping the push when count==0
// would leave the gauge at its last non-zero value and keep the alert firing
// indefinitely after the problem is resolved.
func TestReconcileStuckUploads_ZeroIsPublished(t *testing.T) {
	db := &fakeUploadDB{count: 0}
	m := &fakeUploadMetrics{}

	n, err := reconcileStuckUploads(context.Background(), db, m, 30*time.Minute, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("returned count = %d, want 0", n)
	}
	if m.calls != 1 {
		t.Errorf("SetImageUploadsStuck must be called even when count is 0; called %d times", m.calls)
	}
	if m.lastValue != 0 {
		t.Errorf("gauge value = %d, want 0", m.lastValue)
	}
	if m.pushes != 1 {
		t.Errorf("Push called %d times, want 1; the zero must be transmitted or the alert never clears", m.pushes)
	}
}

// TestReconcileStuckUploads_PushFailureDoesNotFailPass verifies that a
// Pushgateway outage is logged but does not fail the reconcile pass. The
// count is still correct and callers should not treat a metrics-transport
// problem as a reconcile failure — that would turn a monitoring blip into a
// worker error log storm.
func TestReconcileStuckUploads_PushFailureDoesNotFailPass(t *testing.T) {
	db := &fakeUploadDB{count: 3}
	m := &fakeUploadMetrics{pushErr: errors.New("pushgateway 502")}

	n, err := reconcileStuckUploads(context.Background(), db, m, 30*time.Minute, nil)
	if err != nil {
		t.Fatalf("push failure must not fail the reconcile pass, got %v", err)
	}
	if n != 3 {
		t.Errorf("returned count = %d, want 3", n)
	}
	if m.pushes != 1 {
		t.Errorf("Push called %d times, want 1", m.pushes)
	}
}

// TestReconcileStuckUploads_QueryErrorDoesNotPublish verifies that a DB error
// leaves the gauge untouched. Publishing 0 on a DB failure would make a
// transient outage look like "no stuck uploads", masking an ongoing leak and
// silently clearing a firing alert.
func TestReconcileStuckUploads_QueryErrorDoesNotPublish(t *testing.T) {
	db := &fakeUploadDB{err: errors.New("db timeout")}
	m := &fakeUploadMetrics{}

	_, err := reconcileStuckUploads(context.Background(), db, m, 30*time.Minute, nil)
	if err == nil {
		t.Fatal("expected error when query fails, got nil")
	}
	if m.calls != 0 {
		t.Errorf("SetImageUploadsStuck must NOT be called on query error; called %d times", m.calls)
	}
	if m.pushes != 0 {
		t.Errorf("Push must NOT be called on query error; called %d times — pushing a stale or zero gauge would clear a firing alert during a DB outage", m.pushes)
	}
}
