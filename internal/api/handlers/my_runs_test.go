package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestListMyRuns_ReturnsCallerRuns(t *testing.T) {
	userID := uuid.New()
	want := []models.Run{{ID: uuid.New(), Status: "running", TriggeredBy: userID}}
	fake := &fakeRunsDB{runs: want}
	h := buildRunsHandler(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	req = req.WithContext(middleware.WithUserID(req.Context(), userID))
	rec := httptest.NewRecorder()
	h.ListMyRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fake.receivedUserID != userID {
		t.Fatalf("ListRunsForUser user = %s, want %s", fake.receivedUserID, userID)
	}
	if time.Since(fake.receivedSince) < 50*time.Minute || time.Since(fake.receivedSince) > 70*time.Minute {
		t.Fatalf("lookback since = %s, want ~1h ago", fake.receivedSince)
	}
	var got []models.Run
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != want[0].ID {
		t.Fatalf("runs = %+v, want %+v", got, want)
	}
}

func TestListMyRuns_EmptySliceNotNull(t *testing.T) {
	h := buildRunsHandler(&fakeRunsDB{runs: nil})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	req = req.WithContext(middleware.WithUserID(req.Context(), uuid.New()))
	rec := httptest.NewRecorder()
	h.ListMyRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.Bytes()
	if string(body) == "null\n" || string(body) == "null" {
		t.Fatalf("body = %q, want []", body)
	}
	var got []models.Run
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got == nil {
		t.Fatal("decoded nil slice")
	}
}

func TestListMyRuns_UnauthorizedWithoutUser(t *testing.T) {
	h := buildRunsHandler(&fakeRunsDB{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	rec := httptest.NewRecorder()
	h.ListMyRuns(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
