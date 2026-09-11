package handlers

// pods_list_test.go — handler-level tests for ListPods role scoping.
//
// Instructors and admins must receive ListAllPods (every pod with owner);
// students must receive ListPodsByOwner only. A fakePodsDB records which
// method was called so the gate can be sabotage-proved without a live DB.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

type fakePodsDB struct {
	allPods       []models.Pod
	ownerPods     []models.Pod
	calledAll     bool
	calledByOwner bool
	ownerArg      uuid.UUID
	err           error
}

func (f *fakePodsDB) ListAllPods(context.Context) ([]models.Pod, error) {
	f.calledAll = true
	return f.allPods, f.err
}

func (f *fakePodsDB) ListPodsByOwner(_ context.Context, ownerID uuid.UUID) ([]models.Pod, error) {
	f.calledByOwner = true
	f.ownerArg = ownerID
	return f.ownerPods, f.err
}

var _ podsListDB = (*fakePodsDB)(nil)

func TestRoleSeesAllPods(t *testing.T) {
	tests := []struct {
		role string
		want bool
	}{
		{role: models.RoleAdmin, want: true},
		{role: models.RoleInstructor, want: true},
		{role: models.RoleStudent, want: false},
		{role: "", want: false},
		{role: "unknown", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.role, func(t *testing.T) {
			if got := roleSeesAllPods(tc.role); got != tc.want {
				t.Fatalf("roleSeesAllPods(%q) = %v, want %v", tc.role, got, tc.want)
			}
		})
	}
}

func TestListPods_InstructorSeesAllWithOwners(t *testing.T) {
	owner := &models.User{
		ID:          uuid.New(),
		Username:    "alice",
		DisplayName: "Alice Student",
		Role:        models.RoleStudent,
	}
	all := []models.Pod{{
		ID:      uuid.New(),
		OwnerID: owner.ID,
		Name:    "Alice Lab",
		Owner:   owner,
	}}
	fake := &fakePodsDB{allPods: all}
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		podsDB: fake,
	}

	caller := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req = withRoleAndUser(req, models.RoleInstructor, caller)
	rec := httptest.NewRecorder()
	h.ListPods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if !fake.calledAll || fake.calledByOwner {
		t.Fatalf("calledAll=%v calledByOwner=%v; want ListAllPods only", fake.calledAll, fake.calledByOwner)
	}

	var got []models.Pod
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(pods) = %d, want 1", len(got))
	}
	if got[0].Owner == nil || got[0].Owner.DisplayName != "Alice Student" {
		t.Fatalf("owner = %+v, want display_name Alice Student", got[0].Owner)
	}
}

func TestListPods_AdminSeesAll(t *testing.T) {
	fake := &fakePodsDB{allPods: []models.Pod{{ID: uuid.New(), Name: "any"}}}
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		podsDB: fake,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req = withRoleAndUser(req, models.RoleAdmin, uuid.New())
	rec := httptest.NewRecorder()
	h.ListPods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !fake.calledAll || fake.calledByOwner {
		t.Fatalf("calledAll=%v calledByOwner=%v; want ListAllPods only", fake.calledAll, fake.calledByOwner)
	}
}

func TestListPods_StudentSeesOwnOnly(t *testing.T) {
	caller := uuid.New()
	fake := &fakePodsDB{ownerPods: []models.Pod{{ID: uuid.New(), OwnerID: caller, Name: "mine"}}}
	h := &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		podsDB: fake,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req = withRoleAndUser(req, models.RoleStudent, caller)
	rec := httptest.NewRecorder()
	h.ListPods(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if fake.calledAll || !fake.calledByOwner {
		t.Fatalf("calledAll=%v calledByOwner=%v; want ListPodsByOwner only", fake.calledAll, fake.calledByOwner)
	}
	if fake.ownerArg != caller {
		t.Fatalf("ownerArg = %s, want %s", fake.ownerArg, caller)
	}
}

func TestListPods_SabotageAdminOnlyGateUsesOwnerPathForInstructor(t *testing.T) {
	// Counterfactual: if roleSeesAllPods excluded instructors, ListPods would
	// call ListPodsByOwner for an instructor. Keep this assertion so a
	// regression that re-gates on RoleAdmin alone fails loudly.
	if !roleSeesAllPods(models.RoleInstructor) {
		t.Fatal("roleSeesAllPods(instructor) = false; instructors must see all pods")
	}
}
