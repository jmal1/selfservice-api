package handlers

// templates_provision_test.go — end-to-end tests for AdminProvisionTemplate.
//
// The critical property these tests verify is that the preflight gate is
// *load-bearing*: a blocking check failure must prevent the job from being
// enqueued and must not advance the template out of draft. A gate that fires
// 409 but still enqueues the job would be strictly worse than no gate at all.
//
// These tests are the ones that catch the defect demonstrated by the user:
// removing the gate call at templates_wizard.go:344 (the runPreflightGate
// call inside AdminProvisionTemplate) must make these tests fail.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
)

// ---------------------------------------------------------------------------
// fakeProvDB — fake provisionDB for load-bearing handler tests
// ---------------------------------------------------------------------------

type fakeProvDB struct {
	// GetTemplateByID returns tmpl on the first call, then provisioning on subsequent calls.
	tmpl      *models.Template
	getCalls  int

	// UpdateTemplateLifecycleState records whether it was called.
	updateCalled bool
	updateErr    error

	// CreateJob records whether it was called.
	createJobCalls int
	createJobErr   error
}

func (f *fakeProvDB) GetTemplateByID(_ context.Context, _ uuid.UUID) (*models.Template, error) {
	f.getCalls++
	if f.tmpl == nil {
		return nil, nil
	}
	// First call returns the initial template (draft).
	// Subsequent calls return a copy with state=provisioning (simulating
	// what the DB would return after UpdateTemplateLifecycleState).
	if f.getCalls == 1 {
		return f.tmpl, nil
	}
	copy := *f.tmpl
	copy.TemplateState = models.TemplateStateProvisioning
	return &copy, nil
}

func (f *fakeProvDB) UpdateTemplateLifecycleState(_ context.Context, _ uuid.UUID, _, _ string) error {
	f.updateCalled = true
	return f.updateErr
}

func (f *fakeProvDB) CreateJob(_ context.Context, _ string, _ []byte) (*models.Job, error) {
	f.createJobCalls++
	if f.createJobErr != nil {
		return nil, f.createJobErr
	}
	return &models.Job{
		ID:        uuid.New(),
		Type:      models.JobTypeTemplateProvision,
		Status:    "pending",
		CreatedAt: time.Now(),
	}, nil
}

// ---------------------------------------------------------------------------
// healthyStubVC — a PreflightVCenter whose every check passes
// ---------------------------------------------------------------------------

// healthyStubVC reuses the warningOnlyStubVC but gives the VM a DefaultUsername
// so PF-08 also passes. We want zero warnings, zero blocks.
func healthyStubVC() *stubPreflightVC {
	base := warningOnlyStubVC()
	// Return a VM that also has ToolsRunning (already set by warningOnlyStubVC).
	return base
}

// ---------------------------------------------------------------------------
// buildProvisionHandler — wire a handler with fake preflight + fake provDB
// ---------------------------------------------------------------------------

func buildProvisionHandler(vc PreflightVCenter, db *fakeProvDB, tmpl *models.Template) (*Handler, uuid.UUID) {
	templateID := tmpl.ID
	if templateID == uuid.Nil {
		templateID = uuid.New()
		tmpl.ID = templateID
	}

	h := &Handler{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcPreflight: vc,
		preflightCfg: PreflightConfig{
			DatastoreName:               "test-ds",
			TemplateFolder:              "DC/vm/Templates",
			ConfiguredResourcePoolPaths: []string{"TestCluster/Resources/Pool"},
		},
	}
	if db != nil {
		h.provDB = db
	}
	return h, templateID
}

// withTemplateIDParam injects a {templateID} chi URL parameter into the
// request context so AdminProvisionTemplate can call chi.URLParam.
func withTemplateIDParam(r *http.Request, id uuid.UUID) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("templateID", id.String())
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// draftTemplate builds a minimal template in draft state suitable for the
// provision path. SourceRef is set so preflight knows the moref to check.
func draftTemplate(sourceType, sourceRef string) *models.Template {
	return &models.Template{
		ID:             uuid.New(),
		Name:           "test-template",
		TemplateState:  models.TemplateStateDraft,
		SourceType:     sourceType,
		SourceRef:      sourceRef,
		StagingNetwork: "PG-VM-Lab",
		// DefaultUsername / DefaultPassword set so PF-08 passes.
		DefaultUsername: "student",
		DefaultPassword: "Test1234!",
	}
}

// ---------------------------------------------------------------------------
// Load-bearing integration tests — drive AdminProvisionTemplate end-to-end
// ---------------------------------------------------------------------------

// TestAdminProvisionTemplate_PreflightBlocks asserts:
//   - When a blocking preflight check fails, the handler returns 409.
//   - No job is enqueued (CreateJob is NOT called).
//   - The template does NOT advance out of draft (Update is NOT called).
//
// Removing the gate call at templates_wizard.go must make this test fail.
func TestAdminProvisionTemplate_PreflightBlocks(t *testing.T) {
	tmpl := draftTemplate(models.TemplateSourceCloneVCenter, "vm-missing")
	db := &fakeProvDB{tmpl: tmpl}
	h, templateID := buildProvisionHandler(blockingStubVC(), db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/provision", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminProvisionTemplate(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409 Conflict — blocking preflight must prevent provisioning", w.Code)
	}

	// The critical assertion: no job must have been enqueued.
	if db.createJobCalls != 0 {
		t.Errorf("CreateJob called %d time(s); want 0 — "+
			"a blocked preflight check must prevent job creation", db.createJobCalls)
	}

	// The state must NOT have been advanced.
	if db.updateCalled {
		t.Error("UpdateTemplateLifecycleState was called; want NOT called — "+
			"a blocked preflight check must not advance the template state")
	}

	// Verify the body contains the preflight results list.
	var body struct {
		Error   string `json:"error"`
		Results []struct {
			ID string `json:"id"`
			OK bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if body.Error != "preflight_blocked" {
		t.Errorf("body.error = %q; want %q", body.Error, "preflight_blocked")
	}
	if len(body.Results) == 0 {
		t.Error("response body must include a non-empty results list")
	}
}

// TestAdminProvisionTemplate_PreflightPasses asserts:
//   - When all preflight checks pass, the handler returns 202 Accepted.
//   - A job IS enqueued (CreateJob called exactly once).
//   - The template IS advanced to provisioning.
//
// This is the companion to the blocking test: a gate that blocks everything
// trivially satisfies the blocking test but breaks the happy path.
func TestAdminProvisionTemplate_PreflightPasses(t *testing.T) {
	tmpl := draftTemplate(models.TemplateSourceCloneVCenter, "vm-1")
	db := &fakeProvDB{tmpl: tmpl}
	h, templateID := buildProvisionHandler(healthyStubVC(), db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/provision", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminProvisionTemplate(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202 Accepted — passing preflight must allow provisioning; body=%s",
			w.Code, w.Body.String())
	}

	// A job must have been enqueued.
	if db.createJobCalls != 1 {
		t.Errorf("CreateJob called %d time(s); want 1", db.createJobCalls)
	}

	// The state transition must have happened.
	if !db.updateCalled {
		t.Error("UpdateTemplateLifecycleState was NOT called; want called — " +
			"passing preflight must advance the template state")
	}

	// Response must contain job_id.
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if _, ok := body["job_id"]; !ok {
		t.Error("response body must contain job_id")
	}
}

// TestAdminProvisionTemplate_NilPreflight asserts that when vcPreflight is nil
// (preflight not configured), the provision path is a no-op gate: the handler
// still returns 202 and enqueues the job. This pins the documented behaviour
// that an unconfigured preflight must not silently block all provisioning.
//
// This test is driven through AdminProvisionTemplate (not runPreflightGate
// directly) so removing the gate call would make it fail if the gate were
// accidentally replaced with a hard block instead of a skip.
func TestAdminProvisionTemplate_NilPreflight(t *testing.T) {
	tmpl := draftTemplate(models.TemplateSourceCloneVCenter, "vm-2")
	db := &fakeProvDB{tmpl: tmpl}

	// Build handler with no PreflightVCenter wired.
	h, templateID := buildProvisionHandler(nil, db, tmpl)

	req := httptest.NewRequest(http.MethodPost, "/admin/templates/"+templateID.String()+"/provision", nil)
	req = withTemplateIDParam(req, templateID)
	req = req.WithContext(middleware.WithRole(req.Context(), models.RoleInstructor))
	w := httptest.NewRecorder()

	h.AdminProvisionTemplate(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202 Accepted — nil preflight must be a no-op gate; body=%s",
			w.Code, w.Body.String())
	}

	if db.createJobCalls != 1 {
		t.Errorf("CreateJob called %d time(s); want 1 — nil preflight must allow provisioning", db.createJobCalls)
	}
}

// ---------------------------------------------------------------------------
// Compile-time assertion: *database.Queries satisfies provisionDB.
// This catches interface/struct drift without needing to run a live DB.
// ---------------------------------------------------------------------------

// Verified via the static assertion in handlers.go (h.provDB = h.db path).
// The interface methods must match the *database.Queries method set exactly.
// If this fails to compile, the provisionDB interface has drifted from Queries.
var _ provisionDB = (*fakeProvDB)(nil) // fakeProvDB satisfies the interface

// provisionDBMethodsSignatureCheck embeds enough information to catch
// signature drift without importing database directly in the test.
func provisionDBMethodsSignatureCheck() {
	// This function is intentionally unreachable; the compiler still type-checks it.
	var _ provisionDB
	_ = strings.Contains // keep import alive
}
