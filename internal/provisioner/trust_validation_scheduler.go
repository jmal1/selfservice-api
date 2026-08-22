package provisioner

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type l1ValidationReconcileFunc func(context.Context) (L1TrustValidationCounts, error)

type l1ValidationSchedulerMetrics interface {
	ObserveRun(L1TrustValidationCounts, error)
	Push(context.Context) error
}

// L1TrustValidationScheduler turns startup state, leadership transitions, and a
// short periodic poll into serialized reconciliations. Persisted template state
// decides whether work is due; process uptime never does.
type L1TrustValidationScheduler struct {
	isLeader  func() bool
	reconcile l1ValidationReconcileFunc
	metrics   l1ValidationSchedulerMetrics
	logger    *slog.Logger

	mu        sync.Mutex
	running   bool
	rerun     bool
	stopped   bool
	cancelRun context.CancelFunc
	rerunCtx  context.Context
	wait      sync.WaitGroup
}

func NewL1TrustValidationScheduler(
	isLeader func() bool,
	reconcile l1ValidationReconcileFunc,
	metrics l1ValidationSchedulerMetrics,
	logger *slog.Logger,
) *L1TrustValidationScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &L1TrustValidationScheduler{
		isLeader:  isLeader,
		reconcile: reconcile,
		metrics:   metrics,
		logger:    logger.With("component", "l1_validation_scheduler"),
	}
}

// Start performs the startup state check. It does not depend on a Changes()
// notification, so a worker that acquired leadership before this scheduler was
// initialized still reconciles immediately.
func (s *L1TrustValidationScheduler) Start(ctx context.Context) {
	s.trigger(ctx, "startup")
}

// Tick polls persisted due state on the short scheduler interval.
func (s *L1TrustValidationScheduler) Tick(ctx context.Context) {
	if !s.leader() {
		s.cancelActiveRun()
		return
	}
	s.trigger(ctx, "periodic")
}

// LeadershipChanged starts an immediate pass on acquisition and cancels an
// in-flight pass on loss.
func (s *L1TrustValidationScheduler) LeadershipChanged(ctx context.Context, isLeader bool) {
	if !isLeader {
		s.cancelActiveRun()
		return
	}
	s.trigger(ctx, "leadership_acquired")
}

func (s *L1TrustValidationScheduler) trigger(ctx context.Context, reason string) {
	if ctx.Err() != nil || !s.leader() || s.reconcile == nil {
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
		s.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.running = true
	s.cancelRun = cancel
	s.wait.Add(1)
	s.mu.Unlock()

	go s.run(runCtx, reason)
}

func (s *L1TrustValidationScheduler) run(ctx context.Context, reason string) {
	defer s.wait.Done()

	if !s.leader() {
		s.finishRun()
		return
	}

	counts, runErr := s.reconcile(ctx)
	if s.metrics != nil {
		s.metrics.ObserveRun(counts, runErr)
		if err := s.metrics.Push(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("l1 validation scheduler metrics push failed", "error", err)
		}
	}
	if runErr != nil {
		if ctx.Err() == nil {
			s.logger.Error("l1 trust validation reconcile failed",
				"trigger", reason,
				"due", counts.Due,
				"enqueued", counts.Enqueued,
				"error", runErr)
		}
	} else {
		s.logger.Info("l1 trust validation reconcile complete",
			"trigger", reason,
			"l1_templates", counts.L1Templates,
			"due", counts.Due,
			"enqueued", counts.Enqueued)
	}
	s.finishRun()
}

func (s *L1TrustValidationScheduler) finishRun() {
	s.mu.Lock()
	s.running = false
	s.cancelRun = nil
	rerun := s.rerun && !s.stopped
	rerunCtx := s.rerunCtx
	s.rerun = false
	s.rerunCtx = nil
	s.mu.Unlock()

	if rerun && rerunCtx != nil {
		s.trigger(rerunCtx, "queued")
	}
}

func (s *L1TrustValidationScheduler) cancelActiveRun() {
	s.mu.Lock()
	s.rerun = false
	s.rerunCtx = nil
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *L1TrustValidationScheduler) leader() bool {
	return s.isLeader != nil && s.isLeader()
}

// Stop prevents new runs and cancels the current reconciliation.
func (s *L1TrustValidationScheduler) Stop() {
	s.mu.Lock()
	s.stopped = true
	s.rerun = false
	s.rerunCtx = nil
	cancel := s.cancelRun
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait blocks until all started reconciliations return.
func (s *L1TrustValidationScheduler) Wait() {
	s.wait.Wait()
}

// WaitTimeout blocks until all reconciliations return or the deadline elapses.
func (s *L1TrustValidationScheduler) WaitTimeout(timeout time.Duration) bool {
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
