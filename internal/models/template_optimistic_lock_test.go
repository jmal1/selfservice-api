package models_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TestTemplateJSON_IncludesUpdatedAt locks the wire shape that the
// admin UI relies on for optimistic concurrency. The UI takes the
// updated_at it received on the most recent GET and echoes it back as
// expected_updated_at on the next PUT to detect concurrent edits.
//
// If this test fails the admin UI's edit form will appear to work but
// silently overwrite concurrent admin edits.
func TestTemplateJSON_IncludesUpdatedAt(t *testing.T) {
	ts := time.Date(2026, 6, 7, 12, 34, 56, 0, time.UTC)
	tmpl := models.Template{
		ID:        uuid.New(),
		Name:      "ubuntu-server",
		Kind:      models.TemplateKindCloneWithCustomize,
		AssignIP:  true,
		IsActive:  true,
		CreatedAt: ts,
		UpdatedAt: ts.Add(time.Hour),
	}

	b, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got := string(b)
	if !strings.Contains(got, `"updated_at":"2026-06-07T13:34:56Z"`) {
		t.Errorf("Template JSON must include updated_at; got %s", got)
	}

	// And round-trip back into the struct.
	var round models.Template
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !round.UpdatedAt.Equal(tmpl.UpdatedAt) {
		t.Errorf("UpdatedAt round-trip lost precision: got %v want %v", round.UpdatedAt, tmpl.UpdatedAt)
	}
}

// TestUpdateTemplateRequest_OmitsExpectedUpdatedAtWhenNil ensures
// clients that don't yet opt into optimistic locking (older UIs, CLI
// scripts, integration tests) do NOT send expected_updated_at on the
// wire — leaving the column unset means the DB-level guard is bypassed
// and writes still succeed. This preserves backwards compatibility with
// every caller that existed before migration 000017.
func TestUpdateTemplateRequest_OmitsExpectedUpdatedAtWhenNil(t *testing.T) {
	name := "renamed-template"
	req := models.UpdateTemplateRequest{Name: &name}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "expected_updated_at") {
		t.Errorf("expected_updated_at must be omitempty when nil; got %s", b)
	}
}

// TestUpdateTemplateRequest_SendsExpectedUpdatedAtWhenSet ensures the
// optimistic-locking guard is round-trippable when callers DO opt in.
func TestUpdateTemplateRequest_SendsExpectedUpdatedAtWhenSet(t *testing.T) {
	ts := time.Date(2026, 6, 7, 12, 34, 56, 0, time.UTC)
	name := "renamed-template"
	req := models.UpdateTemplateRequest{Name: &name, ExpectedUpdatedAt: &ts}

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"expected_updated_at":"2026-06-07T12:34:56Z"`) {
		t.Errorf("expected_updated_at must round-trip; got %s", got)
	}

	var back models.UpdateTemplateRequest
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ExpectedUpdatedAt == nil || !back.ExpectedUpdatedAt.Equal(ts) {
		t.Errorf("ExpectedUpdatedAt round-trip lost precision: got %v want %v", back.ExpectedUpdatedAt, ts)
	}
}
