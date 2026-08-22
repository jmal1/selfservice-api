package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type recordingPersister struct {
	steps []Step
}

func (p *recordingPersister) SaveRollbackSteps(_ context.Context, _ uuid.UUID, steps []Step) error {
	p.steps = append([]Step(nil), steps...)
	return nil
}

func TestRollbackPersistsOnlyFailedStepsForRetry(t *testing.T) {
	persister := &recordingPersister{}
	engine := New(uuid.New(), persister, slog.New(slog.NewTextHandler(io.Discard, nil)))
	failB := true
	var calls []string
	engine.RegisterUndo("a", func(context.Context, json.RawMessage) error {
		calls = append(calls, "a")
		return nil
	})
	engine.RegisterUndo("b", func(context.Context, json.RawMessage) error {
		calls = append(calls, "b")
		if failB {
			return errors.New("temporary cleanup failure")
		}
		return nil
	})
	if err := engine.Record(context.Background(), "a", map[string]string{"id": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Record(context.Background(), "b", map[string]string{"id": "b"}); err != nil {
		t.Fatal(err)
	}

	if errs := engine.Rollback(context.Background()); len(errs) != 1 {
		t.Fatalf("first rollback errors = %v, want one", errs)
	}
	if len(persister.steps) != 1 || persister.steps[0].Name != "b" {
		t.Fatalf("persisted steps = %+v, want only failed step b", persister.steps)
	}

	failB = false
	calls = nil
	if errs := engine.Rollback(context.Background()); len(errs) != 0 {
		t.Fatalf("retry rollback errors = %v", errs)
	}
	if len(calls) != 1 || calls[0] != "b" {
		t.Fatalf("retry calls = %v, want only b", calls)
	}
	if len(persister.steps) != 0 {
		t.Fatalf("persisted steps after successful retry = %+v, want empty", persister.steps)
	}
}

func TestRecordRetainsDistinctRecoveredResourceIdentities(t *testing.T) {
	persister := &recordingPersister{}
	engine := New(uuid.New(), persister, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.LoadSteps([]Step{{Name: "vm_clone_0", Data: json.RawMessage(`{"moref":"vm-old"}`)}})

	if err := engine.Record(context.Background(), "vm_clone_0", map[string]string{"moref": "vm-new"}); err != nil {
		t.Fatal(err)
	}
	if len(persister.steps) != 2 {
		t.Fatalf("persisted steps = %+v, want both clone identities", persister.steps)
	}
	if string(persister.steps[0].Data) != `{"moref":"vm-old"}` ||
		string(persister.steps[1].Data) != `{"moref":"vm-new"}` {
		t.Fatalf("persisted identities = %+v, want vm-old then vm-new", persister.steps)
	}

	if err := engine.Record(context.Background(), "vm_clone_0", map[string]string{"moref": "vm-new"}); err != nil {
		t.Fatal(err)
	}
	if len(engine.Steps()) != 2 {
		t.Fatalf("identical retry duplicated rollback step: %+v", engine.Steps())
	}

	var rolledBack []string
	engine.RegisterUndo("vm_clone_0", func(_ context.Context, data json.RawMessage) error {
		var step struct {
			Moref string `json:"moref"`
		}
		if err := json.Unmarshal(data, &step); err != nil {
			return err
		}
		rolledBack = append(rolledBack, step.Moref)
		return nil
	})
	if errs := engine.Rollback(context.Background()); len(errs) != 0 {
		t.Fatalf("rollback errors = %v", errs)
	}
	if len(rolledBack) != 2 || rolledBack[0] != "vm-new" || rolledBack[1] != "vm-old" {
		t.Fatalf("rollback order = %v, want [vm-new vm-old]", rolledBack)
	}
}
