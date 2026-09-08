package handlers

// Coverage for the three authoring create handlers, against a real PostgreSQL
// server. Before this file, AdminCreateAction, AdminCreateWorkflow and
// AdminCreatePlaylist had no tests at all.
//
// Postgres rather than a fake because the interesting failures are database
// failures: the CHECK constraints on execution_mode and creation_mode, the
// unique index on actions.slug, and the callable-conflict and slug-rename
// guards, all of which query real rows. A fake DB would assert only that the
// handler calls the method it obviously calls.

import (
	"bytes"
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

type authoringFixture struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *database.Queries
	h      *Handler
	userID uuid.UUID
	// suffix keeps slugs unique per run so a crashed earlier run's leftovers
	// cannot turn a real failure into a confusing duplicate-key error.
	suffix string
}

func newAuthoringFixture(t *testing.T) *authoringFixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run authoring create coverage")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	userID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email, role)
		VALUES ($1, $2, $2, $3, 'instructor')
	`, userID, "authoring-"+userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}

	f := &authoringFixture{
		t:      t,
		ctx:    ctx,
		pool:   pool,
		q:      database.NewQueries(pool),
		userID: userID,
		suffix: strings.ReplaceAll(uuid.NewString()[:8], "-", ""),
	}
	f.h = NewHandler(f.q, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM actions WHERE slug LIKE '%'||$1`, f.suffix)
		_, _ = pool.Exec(c, `DELETE FROM playlists WHERE created_by = $1`, userID)
		_, _ = pool.Exec(c, `DELETE FROM workflows WHERE created_by = $1`, userID)
		_, _ = pool.Exec(c, `DELETE FROM audit_log WHERE user_id = $1`, userID)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, userID)
	})
	return f
}

// slug returns a per-run-unique kebab slug ending in the fixture suffix.
func (f *authoringFixture) slug(base string) string { return base + "-" + f.suffix }

func (f *authoringFixture) post(handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/admin/x", bytes.NewReader(raw))
	r = r.WithContext(middleware.WithUserID(r.Context(), f.userID))
	rec := httptest.NewRecorder()
	handler(rec, r)
	return rec
}

func (f *authoringFixture) patchAction(id uuid.UUID, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPatch, "/admin/actions/"+id.String(), bytes.NewReader(raw))
	r = r.WithContext(middleware.WithUserID(r.Context(), f.userID))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("actionID", id.String())
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	f.h.AdminUpdateAction(rec, r)
	return rec
}

// A well-formed action round-trips and is stored with the documented defaults.
func TestAdminCreateAction_HappyPathAppliesDefaults(t *testing.T) {
	f := newAuthoringFixture(t)
	slug := f.slug("probe-thing")

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name":   "Probe Thing",
		"slug":   slug,
		"script": "local host=\"\"\nwhile [[ $# -gt 0 ]]; do\n  case \"$1\" in\n    --host) host=\"$2\"; shift 2;;\n    *) shift;;\n  esac\ndone\nreturn 0",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var got models.Action
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.IsLibrary {
		t.Error("created action is not marked is_library")
	}
	if got.ActionType != "command" || got.ActionCategory != "general" {
		t.Errorf("defaults not applied: type=%q category=%q", got.ActionType, got.ActionCategory)
	}
	if string(got.SupportedPlatforms) != `["any"]` {
		t.Errorf("supported_platforms = %s, want [\"any\"]", got.SupportedPlatforms)
	}

	stored, err := f.q.GetLibraryAction(f.ctx, got.ID)
	if err != nil {
		t.Fatalf("action was not persisted: %v", err)
	}
	if stored.Slug == nil || *stored.Slug != slug {
		t.Errorf("stored slug = %v, want %q", stored.Slug, slug)
	}
}

// An unparseable body is the failure with the widest blast radius: the library
// is one sourced file, so it would break every action in every run. It has to
// be a 400 here, not a runtime surprise.
func TestAdminCreateAction_RejectsUnsourceableBody(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name":   "Broken",
		"slug":   f.slug("broken-body"),
		"script": "if [ -z \"$x\" ]; then\n  return 1\n", // no fi
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not valid bash") {
		t.Errorf("error does not name the problem: %s", rec.Body.String())
	}

	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT count(*) FROM actions WHERE slug LIKE '%'||$1`, f.suffix).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("rejected action was still written: %d rows", n)
	}
}

func TestAdminCreateAction_RejectsUnusableSlugs(t *testing.T) {
	f := newAuthoringFixture(t)
	for _, slug := range []string{"Bad Slug", "bad_slug", "-leading", "2fast", "double--hyphen"} {
		t.Run(slug, func(t *testing.T) {
			rec := f.post(f.h.AdminCreateAction, map[string]any{
				"name":   "X",
				"slug":   slug,
				"script": "return 0",
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create %q = %d, want 400: %s", slug, rec.Code, rec.Body.String())
			}
		})
	}
}

// A PowerShell body must not be bash-parsed: windows actions are excluded from
// the bash library entirely, so rejecting them would make the six shipped
// win-* actions unauthorable.
func TestAdminCreateAction_WindowsBodyIsNotBashChecked(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name":                "Win Service Running",
		"slug":                f.slug("win-thing"),
		"supported_platforms": []string{"windows"},
		"script":              "$svc = Get-Service -Name $Name\nif ($svc.Status -ne 'Running') { exit 1 }",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// Two slugs that render the same shell function make buildActionLibrary refuse
// to render at all, which fails every run rather than this one action. New
// slugs cannot collide with each other, but they can collide with a legacy row
// stored before the slug rule existed.
func TestAdminCreateAction_RejectsCallableCollisionWithLegacySlug(t *testing.T) {
	f := newAuthoringFixture(t)
	legacy := "legacy_probe_" + f.suffix

	// Inserted directly: the handler would (correctly) refuse this slug.
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO actions (name, slug, script, is_library)
		VALUES ('Legacy Probe', $1, 'return 0', true)
	`, legacy); err != nil {
		t.Fatal(err)
	}

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name":   "Legacy Probe",
		"slug":   "legacy-probe-" + f.suffix,
		"script": "return 0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), legacy) {
		t.Errorf("error does not name the conflicting action: %s", rec.Body.String())
	}
}

// Renaming a slug renames the generated function, and every workflow calling
// the old name then dies with exit 127 inside a student's assessment. Nothing
// else in the system would notice: there is no foreign key from a workflow's
// bash to the action catalog.
func TestAdminUpdateAction_BlocksSlugRenameThatBreaksWorkflows(t *testing.T) {
	f := newAuthoringFixture(t)
	slug := f.slug("renameable-check")

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name": "Renameable Check", "slug": slug, "script": "return 0",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create action = %d: %s", rec.Code, rec.Body.String())
	}
	var action models.Action
	if err := json.Unmarshal(rec.Body.Bytes(), &action); err != nil {
		t.Fatal(err)
	}
	callable := strings.ReplaceAll(slug, "-", "_")

	wf := f.post(f.h.AdminCreateWorkflow, map[string]any{
		"name": "Uses The Check", "slug": f.slug("uses-the-check"),
		"script": "source /opt/crucible/lib/actions.sh\nrun_action \"Check\" " + callable + "\n",
	})
	if wf.Code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", wf.Code, wf.Body.String())
	}

	renamed := f.slug("renamed-check")
	blocked := f.patchAction(action.ID, map[string]any{"slug": renamed})
	if blocked.Code != http.StatusConflict {
		t.Fatalf("rename = %d, want 409: %s", blocked.Code, blocked.Body.String())
	}
	if !strings.Contains(blocked.Body.String(), "Uses The Check") {
		t.Errorf("error does not name the affected workflow: %s", blocked.Body.String())
	}

	stored, err := f.q.GetLibraryAction(f.ctx, action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Slug == nil || *stored.Slug != slug {
		t.Errorf("slug changed despite the 409: %v", stored.Slug)
	}
}

// The guard must not become a lock: an action nobody calls is still renameable.
func TestAdminUpdateAction_AllowsSlugRenameWithNoCallers(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name": "Unused Check", "slug": f.slug("unused-check"), "script": "return 0",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var action models.Action
	if err := json.Unmarshal(rec.Body.Bytes(), &action); err != nil {
		t.Fatal(err)
	}

	renamed := f.slug("still-unused-check")
	if got := f.patchAction(action.ID, map[string]any{"slug": renamed}); got.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200: %s", got.Code, got.Body.String())
	}
	stored, err := f.q.GetLibraryAction(f.ctx, action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Slug == nil || *stored.Slug != renamed {
		t.Errorf("slug = %v, want %q", stored.Slug, renamed)
	}
}

// A partial PATCH must validate the merged row, not the request: a new script
// has to be checked even though the request carries no slug.
func TestAdminUpdateAction_ValidatesScriptAgainstExistingSlug(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreateAction, map[string]any{
		"name": "Patchable", "slug": f.slug("patchable"), "script": "return 0",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var action models.Action
	if err := json.Unmarshal(rec.Body.Bytes(), &action); err != nil {
		t.Fatal(err)
	}

	got := f.patchAction(action.ID, map[string]any{"script": "while true; do\n  return 0\n"})
	if got.Code != http.StatusBadRequest {
		t.Fatalf("patch = %d, want 400: %s", got.Code, got.Body.String())
	}
	stored, err := f.q.GetLibraryAction(f.ctx, action.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Script != "return 0" {
		t.Errorf("body was overwritten despite the 400: %q", stored.Script)
	}
}

func TestAdminCreateWorkflow_RejectsInvalidEnumsWithoutA500(t *testing.T) {
	f := newAuthoringFixture(t)

	cases := map[string]map[string]any{
		"bad execution_mode": {"execution_mode": "kali-runner"},
		"bad creation_mode":  {"creation_mode": "wizard"},
		"bad slug":           {"slug": "Not A Slug"},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"name": "WF", "slug": f.slug("wf-enum")}
			for k, v := range override {
				body[k] = v
			}
			rec := f.post(f.h.AdminCreateWorkflow, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("create = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdminCreateWorkflow_StartsInDraft(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreateWorkflow, map[string]any{
		"name": "Draft WF", "slug": f.slug("draft-wf"),
		"execution_mode": models.ExecModeVMwareTools,
		"creation_mode":  models.CreationModeScript,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var wf models.Workflow
	if err := json.Unmarshal(rec.Body.Bytes(), &wf); err != nil {
		t.Fatal(err)
	}
	// Only `active` may join a playlist run, so a create that returned
	// anything else here would put unreviewed content in front of students.
	if wf.Status != models.WorkflowStatusDraft {
		t.Errorf("status = %q, want %q", wf.Status, models.WorkflowStatusDraft)
	}
	if wf.TimeoutSeconds != 300 {
		t.Errorf("timeout_seconds = %d, want the documented default 300", wf.TimeoutSeconds)
	}
}

// Accepting scoring_mode=points would return 201 and then silently grade
// pass/fail, because nothing reads actions.points. Refusing is the honest
// answer until the engine can total a score.
func TestAdminCreatePlaylist_RejectsUnimplementedPointsScoring(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreatePlaylist, map[string]any{
		"name": "Points PL", "slug": f.slug("points-pl"),
		"scoring_mode": models.ScoringModePoints,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not implemented") {
		t.Errorf("error does not explain why: %s", rec.Body.String())
	}
}

func TestAdminCreatePlaylist_DefaultsToPassFail(t *testing.T) {
	f := newAuthoringFixture(t)

	rec := f.post(f.h.AdminCreatePlaylist, map[string]any{
		"name": "Plain PL", "slug": f.slug("plain-pl"),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var pl models.Playlist
	if err := json.Unmarshal(rec.Body.Bytes(), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.ScoringMode != models.ScoringModePassFail {
		t.Errorf("scoring_mode = %q, want %q", pl.ScoringMode, models.ScoringModePassFail)
	}
}
