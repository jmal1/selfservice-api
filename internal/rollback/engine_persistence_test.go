package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type failingPersister struct {
	err error
}

func (p *failingPersister) SaveRollbackSteps(context.Context, uuid.UUID, []Step) error {
	return p.err
}

func TestRecordPersistenceFailureDoesNotUndoWithoutDurableReceipt(t *testing.T) {
	persistErr := errors.New("database transport unavailable")
	engine := New(
		uuid.New(),
		&failingPersister{err: persistErr},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	undoCalls := 0
	engine.RegisterUndo("created", func(_ context.Context, data json.RawMessage) error {
		undoCalls++
		return nil
	})

	err := engine.Record(context.Background(), "created", map[string]string{"id": "resource-1"})
	if !errors.Is(err, persistErr) {
		t.Fatalf("Record error = %v, want persistence failure", err)
	}
	if undoCalls != 0 {
		t.Fatalf("ambiguous owner performed %d destructive undo calls", undoCalls)
	}
	if len(engine.Steps()) != 1 {
		t.Fatalf("unpersisted receipt was discarded from memory: %+v", engine.Steps())
	}
}

type leaseLostPersister struct {
	handoffs        []Step
	handoffCalls    int
	handoffFailures int
	saveErr         error
}

func (p *leaseLostPersister) SaveRollbackSteps(context.Context, uuid.UUID, []Step) error {
	if p.saveErr != nil {
		return p.saveErr
	}
	return ErrOwnershipLost
}

func (p *leaseLostPersister) HandoffRollbackStep(_ context.Context, _ uuid.UUID, step Step) error {
	p.handoffCalls++
	if p.handoffFailures > 0 {
		p.handoffFailures--
		return errors.New("transient handoff failure")
	}
	p.handoffs = append(p.handoffs, step)
	return nil
}

func TestRecordOwnershipLossHandsReceiptToSuccessorWithoutUndo(t *testing.T) {
	persister := &leaseLostPersister{handoffFailures: 1}
	engine := New(
		uuid.New(),
		persister,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	undoCalls := 0
	engine.RegisterUndo("created", func(context.Context, json.RawMessage) error {
		undoCalls++
		return nil
	})

	err := engine.Record(context.Background(), "created", map[string]string{"id": "resource-1"})
	if !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("Record error = %v, want ownership loss", err)
	}
	if undoCalls != 0 {
		t.Fatalf("stale owner performed %d destructive undo calls", undoCalls)
	}
	if len(persister.handoffs) != 1 || persister.handoffs[0].Name != "created" {
		t.Fatalf("handoff receipts = %+v", persister.handoffs)
	}
	if persister.handoffCalls != 2 {
		t.Fatalf("handoff calls = %d, want transient failure then success", persister.handoffCalls)
	}

	successor := New(uuid.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	successor.LoadSteps(persister.handoffs)
	successor.RegisterUndo("created", func(context.Context, json.RawMessage) error {
		undoCalls++
		return nil
	})
	if errs := successor.Rollback(context.Background()); len(errs) != 0 {
		t.Fatalf("successor rollback errors = %v", errs)
	}
	if undoCalls != 1 {
		t.Fatalf("successor undo calls = %d, want 1", undoCalls)
	}
}

func TestRecordCanceledPersistenceHandsReceiptOffWithoutUndo(t *testing.T) {
	persistErr := errors.New("database transport unavailable")
	persister := &leaseLostPersister{saveErr: persistErr}
	engine := New(
		uuid.New(),
		persister,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	undoCalls := 0
	engine.RegisterUndo("created", func(context.Context, json.RawMessage) error {
		undoCalls++
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := engine.Record(ctx, "created", map[string]string{"id": "resource-1"})
	if !errors.Is(err, persistErr) {
		t.Fatalf("Record error = %v, want transport failure", err)
	}
	if undoCalls != 0 {
		t.Fatalf("ownership-ambiguous writer performed %d destructive undo calls", undoCalls)
	}
	if len(persister.handoffs) != 1 || persister.handoffs[0].Name != "created" {
		t.Fatalf("handoff receipts = %+v", persister.handoffs)
	}
}

type rollbackFencePersister struct {
	err   error
	steps []Step
}

func (p *rollbackFencePersister) SaveRollbackSteps(_ context.Context, _ uuid.UUID, steps []Step) error {
	p.steps = append([]Step(nil), steps...)
	return p.err
}

func TestRollbackDoesNotUndoWithoutOwnershipFence(t *testing.T) {
	leaseErr := fmt.Errorf("%w: superseded claim", ErrOwnershipLost)
	persister := &rollbackFencePersister{err: leaseErr}
	engine := New(
		uuid.New(),
		persister,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	engine.LoadSteps([]Step{{Name: "created", Data: json.RawMessage(`{"id":"resource-1"}`)}})
	undoCalls := 0
	engine.RegisterUndo("created", func(context.Context, json.RawMessage) error {
		undoCalls++
		return nil
	})

	errs := engine.Rollback(context.Background())
	if len(errs) != 1 || !errors.Is(errs[0], ErrOwnershipLost) {
		t.Fatalf("Rollback errors = %v, want ownership loss", errs)
	}
	if undoCalls != 0 {
		t.Fatalf("stale owner performed %d destructive undo calls", undoCalls)
	}
	if len(engine.Steps()) != 1 {
		t.Fatalf("rollback receipt was discarded after failed fence: %+v", engine.Steps())
	}
}
