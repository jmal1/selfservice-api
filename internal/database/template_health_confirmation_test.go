package database

import (
	"testing"

	"github.com/google/uuid"
)

func TestTemplateHealthConfirmationJobIDIsStablePerFailure(t *testing.T) {
	templateID := uuid.New()
	payload := []byte(`{"template_id":"` + templateID.String() + `","pending_failure_at":"2026-08-20T05:00:00Z"}`)

	first := templateHealthConfirmationJobID(templateID, payload)
	for i := 0; i < 20; i++ {
		if got := templateHealthConfirmationJobID(templateID, payload); got != first {
			t.Fatalf("same pending failure produced job %s, want stable %s", got, first)
		}
	}

	newFailure := []byte(`{"template_id":"` + templateID.String() + `","pending_failure_at":"2026-08-20T06:00:00Z"}`)
	if got := templateHealthConfirmationJobID(templateID, newFailure); got == first {
		t.Fatal("a new pending failure reused the previous confirmation job ID")
	}
}
