package handlers

// End-to-end proof of the OVA-to-template path, against a real PostgreSQL
// server: an imported OVA becomes selectable, its moref becomes the source_ref
// of a source_type=ovf draft, the draft walks
// draft -> provisioning -> configuring -> generalizing -> ready -> verifying ->
// active, and deleting the template destroys exactly the staging VM.
//
// Why this test exists in this shape. The pieces were individually unit-tested
// with fakes and still could not be trusted together, because every fake also
// faked the state machine: migration 000018's transition CHECK, the wizard's
// two-stage draft INSERT/UPDATE (which needs h.db.Pool() and so is skipped
// entirely by a fake DB), and the publish credential contract are exactly the
// parts a fake replaces. Those are also the parts that would silently strand an
// appliance template.
//
// What it does NOT cover, deliberately: the vCenter side of provision and
// verify (clone, AttachNetworkAdapter, WaitForTools, base-image snapshot, smoke
// clone). Those need real vCenter — vcsim's WaitForTools does not complete, per
// the skip in internal/vcenter/template_ops_vcsim_test.go — so the worker's
// vCenter work is represented here by the same durable writes the worker makes
// on success (SetTemplateVCenterVM, UpdateTemplateLifecycleState). This proves
// every transition, gate, and enqueue that lives in this repo; a live OVA still
// has to be imported once to prove govmomi's half.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// destroyRecorderVC records DestroyVM so the residue assertion can name the
// exact moref. GetGuestInfo is implemented because the wizard polls it for
// every response once a staging VM exists. The embedded nil VCenterConsole
// leaves everything else a panic rather than a silent no-op: this path must
// not power, reset, or ticket anything.
type destroyRecorderVC struct {
	VCenterConsole
	destroyed []string
	err       error
}

func (v *destroyRecorderVC) DestroyVM(_ context.Context, moref string) error {
	if v.err != nil {
		return v.err
	}
	v.destroyed = append(v.destroyed, moref)
	return nil
}

func (v *destroyRecorderVC) GetGuestInfo(_ context.Context, moref string) (*vcenter.GuestInfo, error) {
	return &vcenter.GuestInfo{Name: moref, PoweredOn: true}, nil
}

func TestOVFTemplateLifecycleEndToEndPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run the OVF lifecycle e2e")
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
	q := database.NewQueries(pool)

	// The instructor performing the whole flow. created_by and every audit
	// row point at this user, so it is torn down last.
	userID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email, role)
		VALUES ($1, $2, $2, $3, 'instructor')
	`, userID, "ovf-e2e-"+userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}

	// templateID is zeroed once the delete step proves the row is gone;
	// cleanupTemplateID is not, so the jobs this test enqueued are always
	// reaped even on the happy path.
	var templateID, cleanupTemplateID uuid.UUID
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if cleanupTemplateID != uuid.Nil {
			_, _ = pool.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, cleanupTemplateID)
			_, _ = pool.Exec(cleanupCtx,
				`DELETE FROM jobs WHERE payload->>'template_id' = $1`, cleanupTemplateID.String())
			_, _ = pool.Exec(cleanupCtx,
				`DELETE FROM audit_log WHERE resource_id = $1`, cleanupTemplateID)
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM image_uploads WHERE uploaded_by = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM audit_log WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, userID)
	})

	vc := &destroyRecorderVC{}
	h := NewHandler(q, nil, vc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil).
		// WithImageStore is what wires imgDB (= h.db); the OVA catalog
		// returns 503 without it. The store itself is never called here.
		WithImageStore(&fakeImageStore{})

	authed := func(r *http.Request) *http.Request {
		return r.WithContext(middleware.WithUserID(r.Context(), userID))
	}

	// ── Step 1: an OVA finishes importing ────────────────────────────────
	// Driven through the real queries the worker uses, so the row reaches
	// `imported` with a moref exactly the way ImportImage leaves it.
	const importedMoref = "vm-90210"
	img := &models.ImageUpload{
		Filename:   "appliance-e2e.ova",
		Kind:       models.ImageKindOVA,
		ObjectKey:  "crucible/e2e/" + uuid.NewString() + ".ova",
		SizeBytes:  4096,
		UploadedBy: &userID,
	}
	if err := q.CreateImageUpload(ctx, img); err != nil {
		t.Fatalf("create image upload: %v", err)
	}
	if err := q.SetImageUploadUploaded(ctx, img.ID, 4096); err != nil {
		t.Fatalf("mark uploaded: %v", err)
	}
	if err := q.UpdateImageUploadStatus(ctx, img.ID,
		models.ImageUploadUploaded, models.ImageUploadImporting); err != nil {
		t.Fatalf("mark importing: %v", err)
	}
	if err := q.SetImageUploadImported(ctx, img.ID, "", importedMoref, ""); err != nil {
		t.Fatalf("mark imported: %v", err)
	}

	// ── Step 2: the OVA is discoverable, and hands back a usable source_ref ──
	// This is the join that did not exist before: an imported OVA is
	// excluded from the ISO picker by design, so without this surface the
	// moref below is unobtainable and an ovf draft cannot be authored.
	rec := httptest.NewRecorder()
	h.AdminListVCenterOVAs(rec, authed(httptest.NewRequest(http.MethodGet, "/admin/vcenter/ovas", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("list OVAs = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var catalog struct {
		OVAs       []OVAEntry `json:"ovas"`
		SourceType string     `json:"source_type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &catalog); err != nil {
		t.Fatalf("decode OVA catalog: %v", err)
	}
	if catalog.SourceType != models.TemplateSourceOVF {
		t.Errorf("catalog source_type = %q, want %q", catalog.SourceType, models.TemplateSourceOVF)
	}
	var sourceRef string
	for _, e := range catalog.OVAs {
		if e.ImageID != img.ID.String() {
			continue
		}
		if e.Disabled {
			t.Fatalf("imported OVA is not selectable: %s", e.Reason)
		}
		sourceRef = e.SourceRef
	}
	if sourceRef != importedMoref {
		t.Fatalf("catalog source_ref = %q, want the imported moref %q", sourceRef, importedMoref)
	}

	// ── Step 3: an ovf draft, skipping generalize, kind=clone_no_customize ──
	// All three together are the appliance case. Any one of them missing is
	// a template whose pods cannot be logged into.
	draftBody, err := json.Marshal(CreateTemplateDraftRequest{
		Name:            "ovf-e2e-" + uuid.NewString()[:8],
		OSType:          "linux",
		SourceType:      models.TemplateSourceOVF,
		SourceRef:       sourceRef,
		SkipGeneralize:  true,
		Kind:            models.TemplateKindCloneNoCustomize,
		DefaultUsername: "appliance",
		DefaultPassword: "ApplianceP@ss1",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.AdminCreateTemplateDraft(rec,
		authed(httptest.NewRequest(http.MethodPost, "/admin/templates/draft", bytes.NewReader(draftBody))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create draft = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var draft models.Template
	if err := json.Unmarshal(rec.Body.Bytes(), &draft); err != nil {
		t.Fatalf("decode draft: %v", err)
	}
	templateID = draft.ID
	cleanupTemplateID = draft.ID

	persisted, err := q.GetTemplateByID(ctx, templateID)
	if err != nil || persisted == nil {
		t.Fatalf("refetch draft: %v", err)
	}
	if persisted.TemplateState != models.TemplateStateDraft {
		t.Errorf("state = %q, want %q", persisted.TemplateState, models.TemplateStateDraft)
	}
	if persisted.SourceType != models.TemplateSourceOVF || persisted.SourceRef != importedMoref {
		t.Errorf("source = %q/%q, want %q/%q",
			persisted.SourceType, persisted.SourceRef, models.TemplateSourceOVF, importedMoref)
	}
	if !persisted.SkipGeneralize {
		t.Error("skip_generalize did not persist; generalize would try to run GuestOps against an appliance")
	}
	if persisted.Kind != models.TemplateKindCloneNoCustomize {
		t.Errorf("kind = %q, want %q; a clone_with_customize appliance would hang in `configuring` "+
			"waiting for a generated credential nothing inside it creates",
			persisted.Kind, models.TemplateKindCloneNoCustomize)
	}
	if persisted.IsActive {
		t.Error("a fresh draft is visible to students")
	}

	// jobAt asserts the state after a wizard step and returns the enqueued job.
	jobAt := func(step, wantState, wantJobType string) {
		t.Helper()
		var body struct {
			JobID uuid.UUID `json:"job_id"`
			State struct {
				TemplateState string `json:"template_state"`
			} `json:"state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode response: %v (%s)", step, err, rec.Body.String())
		}
		if body.State.TemplateState != wantState {
			t.Errorf("%s: response state = %q, want %q", step, body.State.TemplateState, wantState)
		}
		fresh, err := q.GetTemplateByID(ctx, templateID)
		if err != nil || fresh == nil {
			t.Fatalf("%s: refetch: %v", step, err)
		}
		if fresh.TemplateState != wantState {
			t.Fatalf("%s: persisted state = %q, want %q", step, fresh.TemplateState, wantState)
		}
		job, err := q.GetJob(ctx, body.JobID)
		if err != nil || job == nil {
			t.Fatalf("%s: enqueued job %s not found: %v", step, body.JobID, err)
		}
		if job.Type != wantJobType {
			t.Errorf("%s: job type = %q, want %q", step, job.Type, wantJobType)
		}
	}

	// ── Step 4: provision (draft -> provisioning) ────────────────────────
	rec = httptest.NewRecorder()
	h.AdminProvisionTemplate(rec, withTemplateIDParam(
		authed(httptest.NewRequest(http.MethodPost, "/provision", nil)), templateID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("provision = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	jobAt("provision", models.TemplateStateProvisioning, models.JobTypeTemplateProvision)

	// Worker: the clone landed. These are the only two durable writes
	// ProvisionTemplate makes on success.
	const stagingMoref = "vm-90211"
	if err := q.SetTemplateVCenterVM(ctx, templateID, stagingMoref); err != nil {
		t.Fatalf("worker SetTemplateVCenterVM: %v", err)
	}
	if err := q.UpdateTemplateLifecycleState(ctx, templateID,
		models.TemplateStateProvisioning, models.TemplateStateConfiguring); err != nil {
		t.Fatalf("worker provisioning->configuring: %v", err)
	}

	// ── Step 5: generalize with NO credentials in the body ───────────────
	// The point of the step. A non-skip template 400s here; an appliance
	// must not be asked for guest credentials it has no way to accept.
	rec = httptest.NewRecorder()
	h.AdminGeneralizeTemplate(rec, withTemplateIDParam(
		authed(httptest.NewRequest(http.MethodPost, "/generalize", nil)), templateID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("generalize with no credentials = %d, want 202 (skip_generalize must waive the "+
			"credential requirement): %s", rec.Code, rec.Body.String())
	}
	jobAt("generalize", models.TemplateStateGeneralizing, models.JobTypeTemplateGeneralize)

	// Worker: skipGeneralizeAndFinalize powered off, snapshotted, and
	// reached ready without running a single GuestOps script.
	if err := q.UpdateTemplateLifecycleState(ctx, templateID,
		models.TemplateStateGeneralizing, models.TemplateStateReady); err != nil {
		t.Fatalf("worker generalizing->ready: %v", err)
	}

	// ── Step 6: publish must go to verifying, never straight to active ───
	rec = httptest.NewRecorder()
	h.AdminPublishTemplate(rec, withTemplateIDParam(
		authed(httptest.NewRequest(http.MethodPost, "/publish", nil)), templateID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("publish = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	jobAt("publish", models.TemplateStateVerifying, models.JobTypeTemplateVerify)

	// The smoke gate is the whole reason publish is asynchronous: a
	// ready -> active shortcut would let an unbooted template reach students.
	if mid, err := q.GetTemplateByID(ctx, templateID); err != nil || mid == nil {
		t.Fatalf("refetch after publish: %v", err)
	} else if mid.IsActive {
		t.Fatal("publish set is_active while still `verifying`; the smoke gate is bypassed")
	}

	// Worker: the smoke clone booted and was destroyed; VerifyTemplate promotes.
	if err := q.UpdateTemplateLifecycleState(ctx, templateID,
		models.TemplateStateVerifying, models.TemplateStateActive); err != nil {
		t.Fatalf("worker verifying->active: %v", err)
	}
	if err := q.SetTemplateActive(ctx, templateID, true); err != nil {
		t.Fatalf("worker SetTemplateActive: %v", err)
	}
	active, err := q.GetTemplateByID(ctx, templateID)
	if err != nil || active == nil {
		t.Fatalf("refetch active: %v", err)
	}
	if active.TemplateState != models.TemplateStateActive || !active.IsActive {
		t.Fatalf("final state = %q is_active=%t, want active/true",
			active.TemplateState, active.IsActive)
	}
	if active.VCenterVMID != stagingMoref {
		t.Fatalf("staging moref = %q, want %q", active.VCenterVMID, stagingMoref)
	}

	// ── Step 7: delete with zero residue ─────────────────────────────────
	rec = httptest.NewRecorder()
	h.AdminDeleteTemplate(rec, withTemplateIDParam(
		authed(httptest.NewRequest(http.MethodDelete, "/", nil)), templateID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	// Exactly one destroy, of exactly the staging VM. Destroying anything
	// else — or nothing — is the residue failure this step exists to catch:
	// the imported OVA source VM must survive, since other templates and a
	// re-import both depend on it.
	if len(vc.destroyed) != 1 || vc.destroyed[0] != stagingMoref {
		t.Fatalf("destroyed = %v, want exactly [%s]", vc.destroyed, stagingMoref)
	}
	if gone, err := q.GetTemplateByID(ctx, templateID); err == nil && gone != nil {
		t.Fatal("template row survived deletion")
	}
	templateID = uuid.Nil // deleted; keep cleanup from re-deleting

	stillThere, err := q.GetImageUploadByID(ctx, img.ID)
	if err != nil || stillThere == nil {
		t.Fatalf("the imported OVA record was collateral damage of template deletion: %v", err)
	}
	if stillThere.VCenterVMID != importedMoref {
		t.Errorf("imported OVA moref = %q, want %q", stillThere.VCenterVMID, importedMoref)
	}

	// ── Every step must have left an audit trail ─────────────────────────
	// Asserted here because audit.Log swallows its own write errors: this
	// path logged five actions and persisted none of them, because
	// r.RemoteAddr carries a port and audit_log.ip_address is INET. Nothing
	// else in the suite reaches a real audit_log insert, so without this
	// assertion the regression is invisible again.
	// Keyed on resource_id, not user_id: audit.Log resolves the user from its
	// own context key, which middleware.WithUserID does not set, so these
	// rows legitimately carry a NULL user_id.
	var actions []string
	rows, err := pool.Query(ctx,
		`SELECT action FROM audit_log WHERE resource_id = $1 ORDER BY id`, cleanupTemplateID)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"template.draft.create", "template.provision",
		"template.generalize", "template.publish", "template.delete",
	} {
		found := false
		for _, got := range actions {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("audit action %q was never persisted (got %v)", want, actions)
		}
	}
}
