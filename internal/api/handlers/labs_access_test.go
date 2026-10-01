package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestWithLabsRequireGrant(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil)
	if h.WithLabsRequireGrant(false).labsRequireGrant {
		t.Fatal("labsRequireGrant = true, want false")
	}
	if !h.WithLabsRequireGrant(true).labsRequireGrant {
		t.Fatal("labsRequireGrant = false, want true")
	}
}

func TestIsolatedLabsDeniedFor(t *testing.T) {
	cases := []struct {
		name         string
		role         string
		labsEnabled  bool
		requireGrant bool
		want         bool
	}{
		{name: "grant off", role: models.RoleStudent, requireGrant: false, want: false},
		{name: "student granted", role: models.RoleStudent, labsEnabled: true, requireGrant: true, want: false},
		{name: "student denied", role: models.RoleStudent, requireGrant: true, want: true},
		{name: "unknown role denied", role: "guest", requireGrant: true, want: true},
		{name: "unknown role granted", role: "guest", labsEnabled: true, requireGrant: true, want: false},
		{name: "instructor", role: models.RoleInstructor, requireGrant: true, want: false},
		{name: "admin", role: models.RoleAdmin, requireGrant: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isolatedLabsDeniedFor(tc.role, tc.labsEnabled, tc.requireGrant)
			if got != tc.want {
				t.Fatalf("isolatedLabsDeniedFor() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRejectIsolatedLabs(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil).WithLabsRequireGrant(true)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods", nil)

	allowed := httptest.NewRecorder()
	if h.rejectIsolatedLabs(allowed, req, &models.User{Role: models.RoleInstructor}) {
		t.Fatal("instructor was rejected")
	}
	if allowed.Code != http.StatusOK {
		t.Fatalf("status = %d, want the response left unwritten", allowed.Code)
	}

	missing := httptest.NewRecorder()
	if h.rejectIsolatedLabs(missing, req, nil) {
		t.Fatal("nil user was rejected")
	}

	denied := httptest.NewRecorder()
	if !h.rejectIsolatedLabs(denied, req, &models.User{Role: models.RoleStudent}) {
		t.Fatal("student without labs was admitted")
	}
	if denied.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", denied.Code)
	}
	if body := denied.Body.String(); !strings.Contains(body, isolatedLabsDeniedMessage) {
		t.Fatalf("body = %q, want %q", body, isolatedLabsDeniedMessage)
	}
}

func TestRejectSingleVMOnly(t *testing.T) {
	h := NewHandler(nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/pods", nil)

	plain := httptest.NewRecorder()
	if h.rejectSingleVMOnly(plain, req, []*models.Template{
		nil,
		{Name: "Ubuntu", SingleVMOnly: false},
	}) {
		t.Fatal("ordinary template was rejected")
	}

	denied := httptest.NewRecorder()
	if !h.rejectSingleVMOnly(denied, req, []*models.Template{
		{Name: "Desktop", SingleVMOnly: true},
		{Name: "Desktop", SingleVMOnly: true},
		{Name: "Windows", SingleVMOnly: true},
	}) {
		t.Fatal("single-vm-only templates were admitted")
	}
	if denied.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", denied.Code)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(denied.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	want := singleVMOnlyMessage([]string{"Desktop", "Windows"})
	if payload.Error != want {
		t.Fatalf("error = %q, want %q", payload.Error, want)
	}
}

func TestTemplateIDs(t *testing.T) {
	if got := templateIDs(nil); len(got) != 0 {
		t.Fatalf("templateIDs(nil) = %v", got)
	}
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := templateIDs([]models.BlueprintVM{{TemplateID: id}})
	if len(got) != 1 || got[0] != id {
		t.Fatalf("templateIDs() = %v", got)
	}
}

func TestBlueprintSingleVMOnlyMessage(t *testing.T) {
	got := blueprintSingleVMOnlyMessage("Desktop", []string{"Red Team", "Blue Team"})
	want := `template "Desktop" is used by blueprints: Red Team, Blue Team`
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestAccessPatchError(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		max    int
		status int
	}{
		{name: "student zero", role: models.RoleStudent, max: 0},
		{name: "student cap", role: models.RoleStudent, max: 3},
		{name: "instructor", role: models.RoleInstructor, status: http.StatusConflict},
		{name: "admin", role: models.RoleAdmin, status: http.StatusConflict},
		{name: "below", role: models.RoleStudent, max: -1, status: http.StatusBadRequest},
		{name: "above", role: models.RoleStudent, max: 4, status: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, msg := accessPatchError(tc.role, tc.max)
			if status != tc.status {
				t.Fatalf("status = %d, want %d (%s)", status, tc.status, msg)
			}
			if status == 0 && msg != "" {
				t.Fatalf("allowed patch returned %q", msg)
			}
			if status != 0 && msg == "" {
				t.Fatal("refused patch returned an empty message")
			}
		})
	}
}
