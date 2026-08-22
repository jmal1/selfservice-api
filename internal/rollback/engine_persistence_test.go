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

type failingPersister struct {
	err error
}

func (p *failingPersister) SaveRollbackSteps(context.Context, uuid.UUID, []Step) error {
	return p.err
}

func TestRecordPersistenceFailureSynchronouslyUndoesResource(t *testing.T) {
	persistErr := errors.New("lease lost")
	engine := New(
		uuid.New(),
		&failingPersister{err: persistErr},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	var undone string
	engine.RegisterUndo("created", func(_ context.Context, data json.RawMessage) error {
		var step struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &step); err != nil {
			return err
		}
		undone = step.ID
		return nil
	})

	err := engine.Record(context.Background(), "created", map[string]string{"id": "resource-1"})
	if !errors.Is(err, persistErr) {
		t.Fatalf("Record error = %v, want persistence failure", err)
	}
	if undone != "resource-1" {
		t.Fatalf("synchronous undo target = %q, want exact resource", undone)
	}
	if len(engine.Steps()) != 0 {
		t.Fatalf("successfully undone unpersisted step remained in memory: %+v", engine.Steps())
	}
}
