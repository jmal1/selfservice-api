package handlers

// playlists_test.go — handler-level tests for AdminListRuns.
//
// These tests exercise the full filter-parsing and response-serialisation path
// without a live database.  A fakeRunsDB records the RunsListFilter the handler
// built from query parameters and returns whatever run slice the test seeds.
//
// What is NOT tested here (requires a real DB):
//   - SQL WHERE clause generation inside ListAllRunsFiltered (database package)
//   - Actual row-set intersection for combined filters
//
// The route-level RBAC guard (student → 403) is exercised in
// internal/api/routes/routes_test.go → TestAdminRunsRequiresInstructorRole.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
)

// ---------------------------------------------------------------------------
// fakeRunsDB
// ---------------------------------------------------------------------------

type fakeRunsDB struct {
	receivedFilter database.RunsListFilter
	receivedUserID uuid.UUID
	receivedSince  time.Time
	runs           []models.Run
	err            error
}

func (f *fakeRunsDB) ListAllRunsFiltered(_ context.Context, filter database.RunsListFilter) ([]models.Run, error) {
	f.receivedFilter = filter
	return f.runs, f.err
}

func (f *fakeRunsDB) ListRunsForUser(_ context.Context, userID uuid.UUID, since time.Time) ([]models.Run, error) {
	f.receivedUserID = userID
	f.receivedSince = since
	return f.runs, f.err
}

// Compile-time check: fakeRunsDB must satisfy the runsListDB interface.
var _ runsListDB = (*fakeRunsDB)(nil)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func buildRunsHandler(fake *fakeRunsDB) *Handler {
	return &Handler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		runsDB: fake,
	}
}

func doAdminListRuns(t *testing.T, h *Handler, rawQuery string) (int, []models.Run) {
	t.Helper()
	target := "/api/v1/admin/runs"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.AdminListRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 OK (body: %s)", rec.Code, rec.Body.String())
	}
	var runs []models.Run
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return rec.Code, runs
}

// ---------------------------------------------------------------------------
// Filter-parsing tests: each asserts the handler passes the right filter field
// to the DB layer.
// ---------------------------------------------------------------------------

func TestAdminListRuns_FilterByTriggeredByUUID(t *testing.T) {
	uid := uuid.New()
	fake := &fakeRunsDB{
		runs: []models.Run{{ID: uuid.New(), TriggeredBy: uid}},
	}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "triggered_by="+uid.String())

	if fake.receivedFilter.TriggeredBy == nil {
		t.Fatal("filter.TriggeredBy is nil; want UUID pointer")
	}
	if *fake.receivedFilter.TriggeredBy != uid {
		t.Errorf("filter.TriggeredBy = %v; want %v", *fake.receivedFilter.TriggeredBy, uid)
	}
	if fake.receivedFilter.TriggeredByStr != "" {
		t.Errorf("filter.TriggeredByStr = %q; want empty (UUID took priority)", fake.receivedFilter.TriggeredByStr)
	}
	if len(got) != 1 {
		t.Errorf("got %d runs; want 1", len(got))
	}
}

func TestAdminListRuns_FilterByTriggeredBySubstring(t *testing.T) {
	fake := &fakeRunsDB{
		runs: []models.Run{{ID: uuid.New(), TriggeredByUsername: "alice"}},
	}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "triggered_by=alice")

	if fake.receivedFilter.TriggeredBy != nil {
		t.Errorf("filter.TriggeredBy = %v; want nil (not a UUID)", fake.receivedFilter.TriggeredBy)
	}
	if fake.receivedFilter.TriggeredByStr != "alice" {
		t.Errorf("filter.TriggeredByStr = %q; want %q", fake.receivedFilter.TriggeredByStr, "alice")
	}
	if len(got) != 1 {
		t.Errorf("got %d runs; want 1", len(got))
	}
}

func TestAdminListRuns_FilterByPodOwnerUUID(t *testing.T) {
	ownerID := uuid.New()
	fake := &fakeRunsDB{
		runs: []models.Run{{ID: uuid.New(), PodOwnerID: &ownerID}},
	}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "pod_owner="+ownerID.String())

	if fake.receivedFilter.PodOwner == nil {
		t.Fatal("filter.PodOwner is nil; want UUID pointer")
	}
	if *fake.receivedFilter.PodOwner != ownerID {
		t.Errorf("filter.PodOwner = %v; want %v", *fake.receivedFilter.PodOwner, ownerID)
	}
	if fake.receivedFilter.PodOwnerStr != "" {
		t.Errorf("filter.PodOwnerStr = %q; want empty (UUID took priority)", fake.receivedFilter.PodOwnerStr)
	}
	if len(got) != 1 {
		t.Errorf("got %d runs; want 1", len(got))
	}
}

func TestAdminListRuns_FilterByPodOwnerSubstring(t *testing.T) {
	fake := &fakeRunsDB{
		runs: []models.Run{{ID: uuid.New(), PodOwnerUsername: "bob"}},
	}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "pod_owner=bob")

	if fake.receivedFilter.PodOwner != nil {
		t.Errorf("filter.PodOwner = %v; want nil (not a UUID)", fake.receivedFilter.PodOwner)
	}
	if fake.receivedFilter.PodOwnerStr != "bob" {
		t.Errorf("filter.PodOwnerStr = %q; want %q", fake.receivedFilter.PodOwnerStr, "bob")
	}
	if len(got) != 1 {
		t.Errorf("got %d runs; want 1", len(got))
	}
}

func TestAdminListRuns_FilterByStatus(t *testing.T) {
	fake := &fakeRunsDB{
		runs: []models.Run{{ID: uuid.New(), Status: "completed"}},
	}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "status=completed")

	if fake.receivedFilter.Status != "completed" {
		t.Errorf("filter.Status = %q; want %q", fake.receivedFilter.Status, "completed")
	}
	if len(got) != 1 {
		t.Errorf("got %d runs; want 1", len(got))
	}
}

func TestAdminListRuns_FilterByFromDate(t *testing.T) {
	from := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	fake := &fakeRunsDB{runs: []models.Run{{ID: uuid.New()}}}
	h := buildRunsHandler(fake)
	doAdminListRuns(t, h, "from="+url.QueryEscape(from.Format(time.RFC3339)))

	if fake.receivedFilter.From == nil {
		t.Fatal("filter.From is nil; want time pointer")
	}
	if !fake.receivedFilter.From.Equal(from) {
		t.Errorf("filter.From = %v; want %v", *fake.receivedFilter.From, from)
	}
}

func TestAdminListRuns_FilterByToDate(t *testing.T) {
	to := time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC)
	fake := &fakeRunsDB{runs: []models.Run{{ID: uuid.New()}}}
	h := buildRunsHandler(fake)
	doAdminListRuns(t, h, "to="+url.QueryEscape(to.Format(time.RFC3339)))

	if fake.receivedFilter.To == nil {
		t.Fatal("filter.To is nil; want time pointer")
	}
	if !fake.receivedFilter.To.Equal(to) {
		t.Errorf("filter.To = %v; want %v", *fake.receivedFilter.To, to)
	}
}

// ---------------------------------------------------------------------------
// Combined filter: handler must pass BOTH fields to the DB (intersection).
// ---------------------------------------------------------------------------

func TestAdminListRuns_TwoFiltersCombined(t *testing.T) {
	uid := uuid.New()
	// The fake simulates the DB returning only the intersection: one run
	// that matches both triggered_by=uid AND status=completed.
	matchingRun := models.Run{ID: uuid.New(), TriggeredBy: uid, Status: "completed"}
	fake := &fakeRunsDB{runs: []models.Run{matchingRun}}
	h := buildRunsHandler(fake)
	_, got := doAdminListRuns(t, h, "triggered_by="+uid.String()+"&status=completed")

	// Both filter fields must be forwarded to the DB.
	if fake.receivedFilter.TriggeredBy == nil || *fake.receivedFilter.TriggeredBy != uid {
		t.Errorf("filter.TriggeredBy = %v; want %v", fake.receivedFilter.TriggeredBy, uid)
	}
	if fake.receivedFilter.Status != "completed" {
		t.Errorf("filter.Status = %q; want %q", fake.receivedFilter.Status, "completed")
	}
	if len(got) != 1 || got[0].ID != matchingRun.ID {
		t.Errorf("got runs = %v; want only the matching run %v", got, matchingRun.ID)
	}
}

// ---------------------------------------------------------------------------
// Unmatched filter: the MOST IMPORTANT test.
//
// The failure mode we guard against: a filter silently ignored causes
// every run to be returned (200 OK with a large list).  A status-only
// assertion would pass that broken behaviour.  We decode the body and
// assert it is exactly "[]", not just "not 500".
// ---------------------------------------------------------------------------

func TestAdminListRuns_UnmatchedFilterReturnsEmptyArray(t *testing.T) {
	// Fake returns nil — no DB row matched the filter.
	fake := &fakeRunsDB{runs: nil, err: nil}
	h := buildRunsHandler(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/runs?triggered_by="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	h.AdminListRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 OK", rec.Code)
	}

	// Read the raw JSON body and trim whitespace/newline added by json.Encoder.
	// We must assert the body is "[]", not "null":
	//   - "null"  means the nil-guard in the handler is missing and the filter
	//     could be silently ignored (a caller receiving null instead of [] would
	//     break UI code expecting an array).
	//   - A non-empty array means a filter was silently ignored and every run
	//     was returned — the failure mode a status-only assertion would miss.
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "[]" {
		t.Errorf("body = %q; want exactly [] — got something else, which means either "+
			"the nil-guard is missing (null) or a filter was silently ignored (non-empty array)", trimmed)
	}
}

// Companion: fake returns a non-nil empty slice — result is still "[]".
func TestAdminListRuns_EmptySliceReturnsEmptyArray(t *testing.T) {
	fake := &fakeRunsDB{runs: []models.Run{}, err: nil}
	h := buildRunsHandler(fake)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/runs", nil)
	rec := httptest.NewRecorder()
	h.AdminListRuns(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 OK", rec.Code)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("body = %q; want []", strings.TrimSpace(string(body)))
	}
}

// ---------------------------------------------------------------------------
// Wildcard characters in substring filter must pass through unescaped.
// The task spec confirms this is intentional for an instructor-only endpoint.
// ---------------------------------------------------------------------------

func TestAdminListRuns_TriggeredBySubstringWithPercent(t *testing.T) {
	// URL-encode the value: %25 → %. The handler must receive "alice%world"
	// and pass it as-is to the DB (not double-escaped).
	fake := &fakeRunsDB{runs: nil}
	h := buildRunsHandler(fake)
	doAdminListRuns(t, h, "triggered_by=alice%25world")

	if fake.receivedFilter.TriggeredByStr != "alice%world" {
		t.Errorf("filter.TriggeredByStr = %q; want %q (percent unescaped from URL)", fake.receivedFilter.TriggeredByStr, "alice%world")
	}
	if fake.receivedFilter.TriggeredBy != nil {
		t.Errorf("filter.TriggeredBy = %v; want nil (not a UUID)", fake.receivedFilter.TriggeredBy)
	}
}

// ---------------------------------------------------------------------------
// Non-empty result: handler serialises the slice the DB returned.
// ---------------------------------------------------------------------------

func TestAdminListRuns_ReturnsRunsFromDB(t *testing.T) {
	run1 := models.Run{ID: uuid.New(), Status: "completed"}
	run2 := models.Run{ID: uuid.New(), Status: "failed"}
	fake := &fakeRunsDB{runs: []models.Run{run1, run2}}
	h := buildRunsHandler(fake)

	_, got := doAdminListRuns(t, h, "")
	if len(got) != 2 {
		t.Fatalf("got %d runs; want 2", len(got))
	}
	if got[0].ID != run1.ID || got[1].ID != run2.ID {
		t.Errorf("run IDs mismatch: got [%v, %v]; want [%v, %v]",
			got[0].ID, got[1].ID, run1.ID, run2.ID)
	}
}
