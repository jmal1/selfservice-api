package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

type briefingFixture struct {
	pool    *pgxpool.Pool
	q       *Queries
	ownerID uuid.UUID
}

func newBriefingFixture(t *testing.T) (context.Context, *briefingFixture) {
	t.Helper()
	dsn := requirePostgresDSN(t)
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	f := &briefingFixture{pool: pool, q: NewQueries(pool), ownerID: uuid.New()}
	f.exec(t, ctx, `INSERT INTO users (id, oidc_sub, username, email) VALUES ($1, $2, $2, $3)`,
		f.ownerID, f.ownerID.String(), f.ownerID.String()+"@example.invalid")
	return ctx, f
}

func (f *briefingFixture) exec(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *briefingFixture) template(t *testing.T, ctx context.Context, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, ctx, `INSERT INTO templates (id, name, vcenter_template, os_type) VALUES ($1, $2, $3, 'linux')`,
		id, name+"-"+id.String(), "src-"+id.String())
	return id
}

func (f *briefingFixture) blueprint(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, ctx, `INSERT INTO blueprints (id, name, created_by) VALUES ($1, $2, $3)`, id, "bp-"+id.String(), f.ownerID)
	return id
}

func (f *briefingFixture) save(t *testing.T, ctx context.Context, kind string, owner uuid.UUID, body string, base int) *models.BriefingRevision {
	t.Helper()
	rev, err := f.q.SaveBriefingRevision(ctx, kind, owner, f.ownerID, body, "msg", base)
	if err != nil {
		t.Fatalf("save base=%d: %v", base, err)
	}
	return rev
}

func TestBriefingRevisionsPostgres(t *testing.T) {
	ctx, f := newBriefingFixture(t)
	tmpl := f.template(t, ctx, "briefing")
	kind := models.BriefingOwnerTemplate

	meta, err := f.q.GetBriefingMeta(ctx, kind, tmpl)
	if err != nil || meta.HeadRevision != 0 || meta.PublishedRevision != nil {
		t.Fatalf("fresh meta = %+v, %v", meta, err)
	}
	if r1 := f.save(t, ctx, kind, tmpl, "one", 0); r1.Revision != 1 || r1.AuthorID == nil || *r1.AuthorID != f.ownerID {
		t.Fatalf("first revision = %+v", r1)
	}

	var conflict *BriefingConflictError
	if _, err := f.q.SaveBriefingRevision(ctx, kind, tmpl, f.ownerID, "stale", "", 0); !errors.As(err, &conflict) || conflict.Head != 1 {
		t.Fatalf("stale base must conflict at head 1, got %v", err)
	}
	f.save(t, ctx, kind, tmpl, "two", 1)

	// A save racing another writer on the same base must wait for the row
	// lock and then report 409, not a unique-violation 500 or a silent win.
	other, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx)
	var briefingID, parentID uuid.UUID
	if err := other.QueryRow(ctx, `SELECT id FROM briefings WHERE template_id = $1 FOR UPDATE`, tmpl).Scan(&briefingID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var raceErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, raceErr = f.q.SaveBriefingRevision(ctx, kind, tmpl, f.ownerID, "race", "", 2)
	}()
	time.Sleep(300 * time.Millisecond) // let the save reach the lock
	if err := other.QueryRow(ctx, `SELECT id FROM briefing_revisions WHERE briefing_id = $1 AND revision = 2`, briefingID).Scan(&parentID); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Exec(ctx, `
		INSERT INTO briefing_revisions (briefing_id, revision, parent_id, body_md, body_sha256)
		VALUES ($1, 3, $2, 'other', 'x')
	`, briefingID, parentID); err != nil {
		t.Fatalf("the other writer lost the race, so the save did not wait for the row lock: %v", err)
	}
	if err := other.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if !errors.As(raceErr, &conflict) || conflict.Head != 3 {
		t.Fatalf("racing save must conflict at head 3, got %v", raceErr)
	}

	revs, err := f.q.ListBriefingRevisions(ctx, kind, tmpl)
	if err != nil || len(revs) != 3 || revs[0].Revision != 3 || revs[2].Revision != 1 || revs[0].BodyMD != "" {
		t.Fatalf("history = %+v, %v", revs, err)
	}

	if err := f.q.PublishBriefingRevision(ctx, kind, tmpl, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.q.PublishBriefingRevision(ctx, kind, tmpl, 99); !errors.Is(err, ErrBriefingRevisionNotFound) {
		t.Fatalf("publishing a missing revision: %v", err)
	}
	restored, err := f.q.RestoreBriefingRevision(ctx, kind, tmpl, f.ownerID, 1)
	if err != nil || restored.Revision != 4 || restored.RestoredFrom == nil || *restored.RestoredFrom != 1 {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	got, err := f.q.GetBriefingRevision(ctx, kind, tmpl, 4)
	if err != nil || got.BodyMD != "one" || got.IsPublished {
		t.Fatalf("restored revision = %+v, %v", got, err)
	}
	meta, err = f.q.GetBriefingMeta(ctx, kind, tmpl)
	if err != nil || meta.HeadRevision != 4 || meta.PublishedRevision == nil || *meta.PublishedRevision != 1 {
		t.Fatalf("restore must not change what is published: %+v, %v", meta, err)
	}

	// Wrong kind for an existing ID, and an unknown ID, are both "not found".
	if _, err := f.q.SaveBriefingRevision(ctx, models.BriefingOwnerBlueprint, tmpl, f.ownerID, "x", "", 0); !errors.Is(err, ErrBriefingOwnerNotFound) {
		t.Fatalf("template id under blueprint kind: %v", err)
	}
	if _, err := f.q.GetBriefingMeta(ctx, kind, uuid.New()); !errors.Is(err, ErrBriefingOwnerNotFound) {
		t.Fatalf("unknown owner: %v", err)
	}

	// Deleting the template removes its briefing and every revision.
	f.exec(t, ctx, `DELETE FROM templates WHERE id = $1`, tmpl)
	var left int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM briefing_revisions r JOIN briefings b ON b.id = r.briefing_id WHERE b.template_id = $1
	`, tmpl).Scan(&left); err != nil || left != 0 {
		t.Fatalf("revisions left after template delete = %d, %v", left, err)
	}
}

func TestPodBriefingSectionsPostgres(t *testing.T) {
	ctx, f := newBriefingFixture(t)
	bp := f.blueprint(t, ctx)
	kali := f.template(t, ctx, "a-kali")
	target := f.template(t, ctx, "b-target")
	unbriefed := f.template(t, ctx, "c-none")

	podID := uuid.New()
	// destroyed keeps the fixture clear of the active-VLAN unique index; the
	// resolver does not look at pod status.
	f.exec(t, ctx, `
		INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet, blueprint_id)
		VALUES ($1, $2, $3, 'destroyed', 3997, '10.253.253.0/24', $4)
	`, podID, f.ownerID, "briefing-"+podID.String(), bp)
	for _, vm := range []struct {
		tmpl         uuid.UUID
		name, status string
		boot         int
	}{
		{target, "Target", "running", 2},
		{kali, "Kali", "running", 1},
		{kali, "Kali-2", "running", 3},
		{kali, "Gone", "deleted", 4},
		{unbriefed, "Plain", "running", 5},
	} {
		f.exec(t, ctx, `
			INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status, boot_order)
			VALUES ($1, $2, $3, 1, 1024, 10, $4, $5)
		`, podID, vm.tmpl, vm.name, vm.status, vm.boot)
	}

	sections, err := f.q.ListPodBriefingSections(ctx, podID)
	if err != nil || len(sections) != 0 {
		t.Fatalf("nothing published yet: %+v, %v", sections, err)
	}

	f.save(t, ctx, models.BriefingOwnerBlueprint, bp, "lab", 0)
	f.save(t, ctx, models.BriefingOwnerTemplate, kali, "kali v1", 0)
	f.save(t, ctx, models.BriefingOwnerTemplate, kali, "kali draft", 1)
	f.save(t, ctx, models.BriefingOwnerTemplate, target, "target", 0)
	f.save(t, ctx, models.BriefingOwnerTemplate, unbriefed, "never published", 0)
	for _, p := range []struct {
		kind  string
		owner uuid.UUID
	}{{models.BriefingOwnerBlueprint, bp}, {models.BriefingOwnerTemplate, kali}, {models.BriefingOwnerTemplate, target}} {
		if err := f.q.PublishBriefingRevision(ctx, p.kind, p.owner, 1); err != nil {
			t.Fatal(err)
		}
	}

	sections, err = f.q.ListPodBriefingSections(ctx, podID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 3 {
		t.Fatalf("want blueprint + 2 templates, got %+v", sections)
	}
	if s := sections[0]; s.Scope != "blueprint" || s.BodyMD != "lab" {
		t.Fatalf("blueprint section must come first: %+v", s)
	}
	if s := sections[1]; s.Scope != "template" || *s.TemplateID != kali || s.BodyMD != "kali v1" ||
		len(s.VMNames) != 2 || s.VMNames[0] != "Kali" || s.VMNames[1] != "Kali-2" {
		t.Fatalf("kali section must show the published revision (not the draft) and only live VMs: %+v", s)
	}
	if s := sections[2]; *s.TemplateID != target || s.BodyMD != "target" {
		t.Fatalf("target section: %+v", s)
	}

	if err := f.q.UnpublishBriefing(ctx, models.BriefingOwnerTemplate, kali); err != nil {
		t.Fatal(err)
	}
	sections, err = f.q.ListPodBriefingSections(ctx, podID)
	if err != nil || len(sections) != 2 || sections[1].BodyMD != "target" {
		t.Fatalf("unpublished kali must disappear: %+v, %v", sections, err)
	}
}
