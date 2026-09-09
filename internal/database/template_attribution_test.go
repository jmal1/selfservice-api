package database

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

type fakeUserLookup struct {
	users map[uuid.UUID]*models.User
	err   error
	calls int
}

func (f *fakeUserLookup) GetUserByID(_ context.Context, id uuid.UUID) (*models.User, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.users[id], nil
}

func TestAttachTemplateCreators_PopulatesAndCaches(t *testing.T) {
	creatorID := uuid.New()
	otherID := uuid.New()
	full := &models.User{
		ID:          creatorID,
		Username:    "jane",
		Email:       "jane@example.invalid",
		DisplayName: "Jane Instructor",
		Role:        models.RoleInstructor,
		MaxVCPUs:    20,
	}
	lookup := &fakeUserLookup{users: map[uuid.UUID]*models.User{creatorID: full}}

	templates := []models.Template{
		{Name: "draft-a", CreatedBy: &creatorID},
		{Name: "legacy", CreatedBy: nil},
		{Name: "draft-b", CreatedBy: &creatorID},
		{Name: "orphan", CreatedBy: &otherID},
	}
	if err := attachTemplateCreators(context.Background(), lookup, templates); err != nil {
		t.Fatalf("attachTemplateCreators: %v", err)
	}

	if lookup.calls != 2 {
		t.Fatalf("lookup calls = %d, want 2 (one per distinct created_by)", lookup.calls)
	}
	if templates[0].Creator == nil || templates[0].Creator.DisplayName != "Jane Instructor" {
		t.Fatalf("templates[0].Creator = %+v", templates[0].Creator)
	}
	if templates[0].Creator.MaxVCPUs != 0 {
		t.Fatalf("Creator should be slim attribution fields only; MaxVCPUs=%d", templates[0].Creator.MaxVCPUs)
	}
	if templates[1].Creator != nil {
		t.Fatalf("legacy nil created_by should leave Creator unset")
	}
	if templates[2].Creator != templates[0].Creator {
		t.Fatalf("same created_by should reuse cached Creator pointer")
	}
	if templates[3].Creator != nil {
		t.Fatalf("missing user should leave Creator unset, got %+v", templates[3].Creator)
	}
}

func TestAttachTemplateCreators_LookupError(t *testing.T) {
	id := uuid.New()
	lookup := &fakeUserLookup{err: errors.New("db down")}
	templates := []models.Template{{CreatedBy: &id}}
	if err := attachTemplateCreators(context.Background(), lookup, templates); err == nil {
		t.Fatal("expected lookup error")
	}
}

func TestSlimAttributionUser_Nil(t *testing.T) {
	if slimAttributionUser(nil) != nil {
		t.Fatal("nil in → nil out")
	}
}
