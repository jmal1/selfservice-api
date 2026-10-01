package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestRejectSharedAssessment(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods/x/testing/run", nil)
	if rejectSharedAssessment(rec, req, &models.Pod{NetworkMode: models.NetworkModeIsolated}) {
		t.Fatal("isolated pod was refused")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	if !rejectSharedAssessment(rec, req, &models.Pod{NetworkMode: models.NetworkModeShared}) {
		t.Fatal("shared pod was accepted")
	}
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}
