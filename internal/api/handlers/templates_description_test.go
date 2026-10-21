package handlers

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/audit"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

func descriptionBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The cap is enforced before any DB access, so these run without Postgres.
func TestTemplateDescription_CreatePathsRejectOverCap(t *testing.T) {
	h := &Handler{logger: noopLogger(t)}
	tooLong := strings.Repeat("a", models.TemplateDescriptionMaxBytes+1)
	cases := []struct {
		name string
		fn   http.HandlerFunc
		body string
	}{
		{"create", h.AdminCreateTemplate, descriptionBody(t, map[string]any{
			"name": "t", "vcenter_template": "v", "os_type": "linux", "description": tooLong})},
		{"wizard draft", h.AdminCreateTemplateDraft, descriptionBody(t, map[string]any{
			"name": "t", "os_type": "linux", "description": tooLong})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.fn(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)))
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body)
			}
		})
	}
}

func TestTemplateDescription_UpdateRejectsOversizedBody(t *testing.T) {
	h := &Handler{logger: noopLogger(t)}
	body := `{"description":"` + strings.Repeat("a", templateRequestMaxBytes) + `"}`
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
	req = withTemplateIDParam(req, uuid.New())
	rec := httptest.NewRecorder()
	h.AdminUpdateTemplate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body)
	}
}

func TestTemplateDescriptionUpdatePostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if testing.Short() || dsn == "" {
		t.Skip("set TEST_DATABASE_URL (and drop -short) to run template description persistence tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	instructor := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, oidc_sub, username, email) VALUES ($1, $2, $2, $3)`,
		instructor, instructor.String(), instructor.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	legacy := strings.Repeat("L", models.TemplateDescriptionMaxBytes+10)
	tmplID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO templates (id, name, vcenter_template, os_type, description) VALUES ($1, $2, $3, 'linux', $4)`,
		tmplID, "d-"+tmplID.String(), "s-"+tmplID.String(), legacy); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(database.NewQueries(pool), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	patch := func(fields map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(descriptionBody(t, fields)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("templateID", tmplID.String())
		c := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		c = middleware.WithRole(middleware.WithUserID(audit.WithUserID(c, instructor), instructor), models.RoleInstructor)
		rec := httptest.NewRecorder()
		h.AdminUpdateTemplate(rec, req.WithContext(c))
		return rec
	}

	// A legacy over-cap description echoed back unchanged must not block other edits.
	if rec := patch(map[string]any{"name": "renamed-" + tmplID.String(), "description": legacy}); rec.Code != http.StatusOK {
		t.Fatalf("unchanged legacy description = %d %s", rec.Code, rec.Body)
	}
	// A new over-cap description is refused and nothing is written.
	if rec := patch(map[string]any{"description": legacy + "x"}); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("new over-cap description = %d %s", rec.Code, rec.Body)
	}

	rec := patch(map[string]any{"description": "## Kali\nAttack box."})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit description = %d %s", rec.Code, rec.Body)
	}
	var got models.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Description != "## Kali\nAttack box." {
		t.Fatalf("response description = %q (err %v)", got.Description, err)
	}

	// Optimistic concurrency: a save based on an older read is a 409.
	if rec := patch(map[string]any{"description": "stale", "expected_updated_at": got.UpdatedAt.Add(-time.Hour)}); rec.Code != http.StatusConflict {
		t.Fatalf("stale description save = %d %s", rec.Code, rec.Body)
	}

	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'template.update' AND resource_id = $1 AND user_id = $2`,
		tmplID, instructor).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatalf("template.update audit rows = %d, want 2 (one per successful save)", audits)
	}
}
