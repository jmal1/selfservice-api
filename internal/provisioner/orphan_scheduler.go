package provisioner

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type orphanReconcileFunc func(context.Context) (ReconcileCounts, error)

// OrphanLeadershipState is an atomic snapshot of the worker's current
// leadership lease.
type OrphanLeadershipState struct {
	IsLeader   bool
	Generation int64
	Context    context.Context
}

// OrphanReconcilerScheduler serializes startup, leadership, and periodic
// triggers so an orphan pass never overlaps another pass from this worker.
type OrphanReconcilerScheduler struct {
	leadership func() OrphanLeadershipState
	reconcile  orphanReconcileFunc
	logger     *slog.Logger

	mu                 sync.Mutex
	started            bool
	leaderObserved     bool
	wasLeader          bool
	observedGeneration int64
	running            bool
	rerun              bool
	stopped            bool
	cancelRun          context.CancelFunc
	rerunCtx           context.Context
	rerunLeadership    OrphanLeadershipState
	wait               sync.WaitGroup
}

func NewOrphanReconcilerScheduler(
	leadership func() OrphanLeadershipState,
	reconcile orphanReconcileFunc,
	logger *slog.Logger,
) *OrphanReconcilerScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &OrphanReconcilerScheduler{
		leadership: leadership,
		reconcile:  reconcile,
		logger:     logger.With("component", "vcenter_orphan_reconciler_scheduler"),
	}
}

// Start observes current leadership directly so a worker that acquired
// leadership before scheduler initialization still reconciles immediately.
func (s *OrphanReconcilerScheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	state := s.currentLeadership()
	s.leaderObserved = true
	s.wasLeader = state.IsLeader
	s.observedGeneration = state.Generation
	s.mu.Unlock()

	if state.IsLeader {
		s.trigger(ctx, state, "startup")
	}
}

// Tick runs the normal periodic pass when this worker is leader.
func (s *OrphanReconcilerScheduler) Tick(ctx context.Context) {
	state := s.currentLeadership()
	if !state.IsLeader {
		s.observeLeadershipLoss()
		return
	}

	s.mu.Lock()
	s.leaderObserved = true
	s.wasLeader = true
	s.observedGeneration = state.Generation
	s.mu.Unlock()
	s.trigger(ctx, state, "periodic")
}

// LeadershipChanged starts an immediate pass on acquisition and cancels an
// in-flight pass on loss. A startup acquisition event is ignored when Start
// already observed the same held leadership. It samples authoritative state
// rather than trusting the lossy Changes payload.
func (s *OrphanReconcilerScheduler) LeadershipChanged(ctx context.Context) {
	state := s.currentLeadership()
	s.mu.Lock()
	newLeadership := !s.leaderObserved ||
		!s.wasLeader ||
		s.observedGeneration != state.Generation
	s.leaderObserved = true
	s.wasLeader = state.IsLeader
	s.observedGeneration = state.Generation
	if !state.IsLeader {
		s.rerun = false
		s.rerunCtx = nil
		s.rerunLeadership = OrphanLeadershipState{}
		cancel := s.cancelRun
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	s.mu.Unlock()

	if newLeadership {
		s.trigger(ctx, state, "leadership_acquired")
	}
}

func (s *OrphanReconcilerScheduler) trigger(
	ctx context.Context,
	state OrphanLeadershipState,
	reason string,
) {
	if ctx.Err() != nil ||
		!state.IsLeader ||
		state.Context == nil ||
		state.Context.Err() != nil ||
		s.reconcile == nil {
		return
	}

	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.running {
		s.rerun = true
		s.rerunCtx = ctx
		s.rerunLeadership = state
		s.mu.Unlock()
		return
	}
	runCtx, cancelCtx := context.WithCancel(state.Context)
	stopWorkerWatch := context.AfterFunc(ctx, cancelCtx)
	cancel := func() {
		stopWorkerWatch()
		cancelCtx()
	}
	s.running = true
	s.cancelRun = cancel
	s.wait.Add(1)
	s.mu.Unlock()

	go s.run(runCtx, cancel, reason)
}

func (s *OrphanReconcilerScheduler) run(
	ctx context.Context,
	cancel context.CancelFunc,
	reason string,
) {
	defer s.wait.Done()
	defer cancel()

	if ctx.Err() != nil {
		s.finishRun()
		return
	}

	_, err := s.reconcile(ctx)
	if err != nil && ctx.Err() == nil {
		s.logger.Error("orphan reconcile failed", "trigger", reason, "error", err)
	}
	s.finishRun()
}

func (s *OrphanReconcilerScheduler) finishRun() {
	s.mu.Lock()
	s.running = false
	s.cancelRun = nil
	rerun := s.rerun && !s.stopped
	rerunCtx := s.rerunCtx
	rerunLeadership := s.rerunLeadership
	s.rerun = false
	s.rerunCtx = nil
	s.rerunLeadership = OrphanLeadershipState{}
	s.mu.Unlock()

	if rerun && rerunCtx != nil {
		s.trigger(rerunCtx, rerunLeadership, "queued")
	}
}

func (s *OrphanReconcilerScheduler) observeLeadershipLoss() {
	s.mu.Lock()
	s.leaderObserved = true
	s.wasLeader = false
	s.rerun = false
	s.rerunCtx = nil
	s.rerunLeadership = OrphanLeadershipState{}
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Stop prevents new runs and cancels the current reconciliation.
func (s *OrphanReconcilerScheduler) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.rerun = false
	s.rerunCtx = nil
	s.rerunLeadership = OrphanLeadershipState{}
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait blocks until all started reconciliations return.
func (s *OrphanReconcilerScheduler) Wait() {
	s.wait.Wait()
}

// WaitTimeout blocks until all reconciliations return or the deadline elapses.
func (s *OrphanReconcilerScheduler) WaitTimeout(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.wait.Wait()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (s *OrphanReconcilerScheduler) currentLeadership() OrphanLeadershipState {
	if s.leadership == nil {
		return OrphanLeadershipState{}
	}
	return s.leadership()
}
