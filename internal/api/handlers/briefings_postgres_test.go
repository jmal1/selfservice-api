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

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestBriefingHandlersPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if testing.Short() || dsn == "" {
		t.Skip("set TEST_DATABASE_URL (and drop -short) to run briefing handler persistence tests")
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
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	owner, stranger, instructor := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, stranger, instructor} {
		exec(`INSERT INTO users (id, oidc_sub, username, email) VALUES ($1, $2, $2, $3)`, id, id.String(), id.String()+"@example.invalid")
	}
	tmpl, podID := uuid.New(), uuid.New()
	exec(`INSERT INTO templates (id, name, vcenter_template, os_type) VALUES ($1, $2, $3, 'linux')`, tmpl, "h-"+tmpl.String(), "s-"+tmpl.String())
	exec(`INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet) VALUES ($1, $2, $3, 'destroyed', 3996, '10.253.252.0/24')`, podID, owner, "h-"+podID.String())
	exec(`INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status) VALUES ($1, $2, 'Kali', 1, 1024, 10, 'running')`, podID, tmpl)

	h := NewHandler(database.NewQueries(pool), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	call := func(fn http.HandlerFunc, method string, user uuid.UUID, role, body string, params map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		c := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		c = middleware.WithRole(middleware.WithUserID(c, user), role)
		rec := httptest.NewRecorder()
		fn(rec, req.WithContext(c))
		return rec
	}
	tp := map[string]string{"templateID": tmpl.String()}

	if rec := call(h.AdminSaveBriefingRevision, http.MethodPost, instructor, models.RoleInstructor, `{"body_md":"# Hi"}`, tp); rec.Code != http.StatusBadRequest {
		t.Fatalf("save without base_revision = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.AdminSaveBriefingRevision, http.MethodPost, instructor, models.RoleInstructor, `{"body_md":"# Hi","base_revision":0}`, tp); rec.Code != http.StatusCreated {
		t.Fatalf("first save = %d %s", rec.Code, rec.Body)
	}
	rec := call(h.AdminSaveBriefingRevision, http.MethodPost, instructor, models.RoleInstructor, `{"body_md":"stale","base_revision":0}`, tp)
	var conflict struct {
		HeadRevision int `json:"head_revision"`
	}
	if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &conflict) != nil || conflict.HeadRevision != 1 {
		t.Fatalf("stale save = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.AdminPublishBriefing, http.MethodPost, instructor, models.RoleInstructor, `{"revision":1}`, tp); rec.Code != http.StatusOK {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if rec := call(h.AdminGetBriefing, http.MethodGet, instructor, models.RoleInstructor, "", map[string]string{"blueprintID": tmpl.String()}); rec.Code != http.StatusNotFound {
		t.Fatalf("a template id on the blueprint route = %d, want 404", rec.Code)
	}

	pp := map[string]string{"podID": podID.String()}
	for _, tc := range []struct {
		name string
		user uuid.UUID
		role string
		want int
	}{
		{"owner", owner, models.RoleStudent, http.StatusOK},
		{"other student", stranger, models.RoleStudent, http.StatusForbidden},
		{"instructor", instructor, models.RoleInstructor, http.StatusOK},
	} {
		rec := call(h.GetPodBriefing, http.MethodGet, tc.user, tc.role, "", pp)
		if rec.Code != tc.want {
			t.Fatalf("%s: GET pod briefing = %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
		}
		if tc.want == http.StatusOK && !strings.Contains(rec.Body.String(), `"body_md":"# Hi"`) {
			t.Fatalf("%s: published body missing: %s", tc.name, rec.Body)
		}
	}
	if rec := call(h.GetPodBriefing, http.MethodGet, owner, models.RoleStudent, "", map[string]string{"podID": uuid.NewString()}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown pod = %d, want 404", rec.Code)
	}
}
