package provisioner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

type memoryJobLease struct {
	mu        sync.Mutex
	owner     string
	claimedAt time.Time
	status    string
}

func (m *memoryJobLease) RenewJobLease(_ context.Context, _ uuid.UUID, workerID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner != workerID || (m.status != models.JobStatusClaimed && m.status != models.JobStatusInProgress) {
		return false, nil
	}
	m.claimedAt = time.Now()
	return true, nil
}

func (m *memoryJobLease) recoverExpired(staleBefore time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.status != models.JobStatusClaimed && m.status != models.JobStatusInProgress {
		return false
	}
	if !m.claimedAt.IsZero() && !m.claimedAt.Before(staleBefore) {
		return false
	}
	m.status = models.JobStatusPending
	m.owner = ""
	m.claimedAt = time.Time{}
	return true
}

func (m *memoryJobLease) transfer(owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner = owner
	m.claimedAt = time.Now()
	m.status = models.JobStatusClaimed
}

func TestLeaseRecoveryDoesNotStealFreshForeignJob(t *testing.T) {
	store := &memoryJobLease{
		owner:     "worker-a",
		claimedAt: time.Now(),
		status:    models.JobStatusInProgress,
	}
	if store.recoverExpired(time.Now().Add(-time.Minute)) {
		t.Fatal("worker-b startup stole worker-a's fresh in-progress job")
	}
}

func TestLeaseRecoveryRecoversExpiredForeignJob(t *testing.T) {
	store := &memoryJobLease{
		owner:     "worker-a",
		claimedAt: time.Now().Add(-2 * time.Minute),
		status:    models.JobStatusInProgress,
	}
	if !store.recoverExpired(time.Now().Add(-time.Minute)) {
		t.Fatal("expired foreign claim was not recovered")
	}
}

func TestLeaseHeartbeatPreventsRecovery(t *testing.T) {
	const (
		heartbeat = 5 * time.Millisecond
		lease     = 30 * time.Millisecond
	)
	store := &memoryJobLease{
		owner:     "worker-a",
		claimedAt: time.Now(),
		status:    models.JobStatusInProgress,
	}
	parent, stop := context.WithCancel(context.Background())
	leaseCtx, cancelLease := context.WithCancelCause(parent)
	done := make(chan struct{})
	initialClaimedAt := store.claimedAt
	go func() {
		defer close(done)
		maintainJobLease(
			leaseCtx,
			store,
			uuid.New(),
			"worker-a",
			initialClaimedAt,
			heartbeat,
			lease,
			heartbeat,
			cancelLease,
		)
	}()

	time.Sleep(4 * lease)
	if store.recoverExpired(time.Now().Add(-lease)) {
		t.Fatal("fresh heartbeat was recovered as expired")
	}
	stop()
	<-done
}

func TestLeaseOwnershipLossCancelsExecution(t *testing.T) {
	store := &memoryJobLease{
		owner:     "worker-a",
		claimedAt: time.Now(),
		status:    models.JobStatusInProgress,
	}
	leaseCtx, cancelLease := context.WithCancelCause(context.Background())
	done := make(chan struct{})
	initialClaimedAt := store.claimedAt
	go func() {
		defer close(done)
		maintainJobLease(
			leaseCtx,
			store,
			uuid.New(),
			"worker-a",
			initialClaimedAt,
			5*time.Millisecond,
			time.Minute,
			5*time.Millisecond,
			cancelLease,
		)
	}()
	store.transfer("worker-b")

	select {
	case <-leaseCtx.Done():
		if !errors.Is(context.Cause(leaseCtx), database.ErrJobLeaseLost) {
			t.Fatalf("lease cancellation cause = %v, want ErrJobLeaseLost", context.Cause(leaseCtx))
		}
	case <-time.After(time.Second):
		t.Fatal("ownership loss did not cancel execution")
	}
	<-done
}

func TestLeaseShutdownHandoffBecomesRecoverable(t *testing.T) {
	store := &memoryJobLease{
		owner:     "worker-a",
		claimedAt: time.Now(),
		status:    models.JobStatusInProgress,
	}
	parent, stop := context.WithCancel(context.Background())
	leaseCtx, cancelLease := context.WithCancelCause(parent)
	done := make(chan struct{})
	initialClaimedAt := store.claimedAt
	go func() {
		defer close(done)
		maintainJobLease(
			leaseCtx,
			store,
			uuid.New(),
			"worker-a",
			initialClaimedAt,
			time.Hour,
			time.Minute,
			time.Second,
			cancelLease,
		)
	}()
	stop()
	<-done

	if !store.recoverExpired(time.Now().Add(time.Nanosecond)) {
		t.Fatal("shutdown claim did not remain recoverable after lease expiry cutoff")
	}
}

type ownershipGuardDB struct {
	mu        sync.Mutex
	owner     string
	statuses  []string
	completed bool
}

func (d *ownershipGuardDB) UpdateJobStatus(
	_ context.Context,
	_ uuid.UUID,
	workerID string,
	status string,
	_ []byte,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if workerID != d.owner {
		return database.ErrJobLeaseLost
	}
	d.statuses = append(d.statuses, status)
	if status == models.JobStatusCompleted {
		d.completed = true
	}
	return nil
}

func (d *ownershipGuardDB) RetryJob(
	context.Context,
	uuid.UUID,
	time.Time,
	bool,
	[]byte,
	string,
) error {
	return database.ErrJobLeaseLost
}

func TestOwnershipLossBlocksStaleFinalization(t *testing.T) {
	workerA := "worker-a"
	claimedAt := time.Now()
	job := &models.Job{
		ID:        uuid.New(),
		Type:      models.JobTypePodDestroy,
		ClaimedBy: &workerA,
		ClaimedAt: &claimedAt,
	}
	db := &ownershipGuardDB{owner: workerA}

	result := processJobLifecycle(
		context.Background(),
		db,
		nil,
		job,
		nil,
		func(context.Context, *models.Job) error {
			db.mu.Lock()
			db.owner = "worker-b"
			db.mu.Unlock()
			return nil
		},
	)
	if !errors.Is(result, database.ErrJobLeaseLost) {
		t.Fatalf("stale finalization result = %v, want ErrJobLeaseLost", result)
	}
	if db.completed {
		t.Fatal("superseded worker finalized a reclaimed job")
	}
}

func TestLeaseConstantsMatchPlanBounds(t *testing.T) {
	if JobLeaseDuration != 90*time.Second {
		t.Fatalf("JobLeaseDuration = %s, want 90s liveness window", JobLeaseDuration)
	}
	if JobLeaseHeartbeatInterval != 15*time.Second {
		t.Fatalf("JobLeaseHeartbeatInterval = %s, want 15s", JobLeaseHeartbeatInterval)
	}
	if JobLeaseRecoveryInterval != 15*time.Second {
		t.Fatalf("JobLeaseRecoveryInterval = %s, want 15s", JobLeaseRecoveryInterval)
	}
	if JobLeaseDuration < 4*JobLeaseHeartbeatInterval {
		t.Fatal("lease duration must cover multiple missed heartbeats")
	}
}

func TestStaleOwnerCannotFinalizeAfterReclaim(t *testing.T) {
	// Counterfactual: A pauses past expiry, B claims with a new token, A resumes.
	workerA := "worker-a:claim-1"
	workerB := "worker-b:claim-2"
	claimedAt := time.Now()
	job := &models.Job{
		ID:        uuid.New(),
		Type:      models.JobTypePodDestroy,
		ClaimedBy: &workerA,
		ClaimedAt: &claimedAt,
	}
	db := &ownershipGuardDB{owner: workerA}

	result := processJobLifecycle(
		context.Background(),
		db,
		nil,
		job,
		nil,
		func(context.Context, *models.Job) error {
			db.mu.Lock()
			db.owner = workerB
			db.mu.Unlock()
			return nil
		},
	)
	if !errors.Is(result, database.ErrJobLeaseLost) {
		t.Fatalf("stale finalization result = %v, want ErrJobLeaseLost", result)
	}
	if db.completed {
		t.Fatal("superseded claim token finalized a reclaimed job")
	}
}
