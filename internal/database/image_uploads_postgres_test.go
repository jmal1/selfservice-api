package database

// First Postgres-backed coverage for image_uploads. The table shipped in
// migration 000023 with two CHECK constraints and a unique object_key index,
// and migration 000039 added the `ovf` source type and skip_generalize, but
// none of it was ever exercised against a real server: every existing image
// test uses in-memory fakes. A CHECK that silently disagrees with the Go
// constants is invisible until an insert fails in production.
//
// These tests also pin the stuck-upload query, whose status list is the only
// thing that decides whether an abandoned upload is ever noticed.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

// imageUploadsTestPool migrates an isolated schema to the current head and
// returns a pool scoped to it.
func imageUploadsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	baseDSN := os.Getenv("TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run image_uploads regressions")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(basePool.Close)

	schema := "imgup_" + uuid.New().String()[24:]
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := basePool.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = basePool.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE")
	})

	scopedDSN := postgresDSNWithSearchPath(t, baseDSN, schema)
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrator, err := migrate.NewWithSourceInstance("iofs", source, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = migrator.Close() })

	if err := migrator.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	if _, dirty, verr := migrator.Version(); verr != nil || dirty {
		t.Fatalf("migration version err=%v dirty=%t, want clean", verr, dirty)
	}

	pool, err := pgxpool.New(ctx, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func insertImageUpload(t *testing.T, pool *pgxpool.Pool, kind, status, objectKey string) (uuid.UUID, error) {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `
		INSERT INTO image_uploads (filename, kind, status, object_key)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, "probe-"+objectKey, kind, status, objectKey).Scan(&id)
	return id, err
}

// TestImageUploads_StatusCheckMatchesGoConstants is the assertion that
// matters most: the CHECK constraint and the models.ImageUpload* constants
// must describe the same set. A constant the CHECK rejects fails at INSERT in
// production; a status the CHECK allows but Go never writes is dead surface
// that lifecycle code will not handle.
func TestImageUploads_StatusCheckMatchesGoConstants(t *testing.T) {
	pool := imageUploadsTestPool(t)

	goStatuses := []string{
		models.ImageUploadPending,
		models.ImageUploadUploading,
		models.ImageUploadUploaded,
		models.ImageUploadImporting,
		models.ImageUploadImported,
		models.ImageUploadError,
	}
	for i, status := range goStatuses {
		if _, err := insertImageUpload(t, pool, models.ImageKindISO, status,
			"crucible/status-"+uuid.NewString()); err != nil {
			t.Errorf("status constant %q (index %d) is rejected by the CHECK constraint: %v",
				status, i, err)
		}
	}

	// The DB must not accept anything outside that set.
	if _, err := insertImageUpload(t, pool, models.ImageKindISO, "finished",
		"crucible/bogus-status"); err == nil {
		t.Error("status 'finished' was accepted; the CHECK constraint is not enforcing the lifecycle")
	}

	// And the constraint must not have quietly grown a status Go never sets.
	var allowed string
	if err := pool.QueryRow(context.Background(), `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'image_uploads'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%status%'
	`).Scan(&allowed); err != nil {
		t.Fatalf("read status CHECK definition: %v", err)
	}
	for _, status := range goStatuses {
		if !strings.Contains(allowed, "'"+status+"'") {
			t.Errorf("CHECK %q does not mention status constant %q", allowed, status)
		}
	}
}

func TestImageUploads_KindCheckRejectsAnythingButISOAndOVA(t *testing.T) {
	pool := imageUploadsTestPool(t)

	for _, kind := range []string{models.ImageKindISO, models.ImageKindOVA} {
		if _, err := insertImageUpload(t, pool, kind, models.ImageUploadPending,
			"crucible/kind-"+uuid.NewString()); err != nil {
			t.Errorf("kind constant %q is rejected by the CHECK constraint: %v", kind, err)
		}
	}
	// The upload handler derives kind from the file extension precisely so a
	// caller cannot relabel an executable as installer media; the CHECK is
	// the backstop for that control.
	for _, kind := range []string{"exe", "vmdk", "ISO", ""} {
		if _, err := insertImageUpload(t, pool, kind, models.ImageUploadPending,
			"crucible/kind-"+uuid.NewString()); err == nil {
			t.Errorf("kind %q was accepted; only iso and ova may exist", kind)
		}
	}
}

// TestImageUploads_ObjectKeyIsUnique pins the index that stops two rows from
// claiming the same MinIO object. Without it, a successful import would
// delete the object out from under the other row, whose retry would then fail
// forever with an unexplained missing object.
func TestImageUploads_ObjectKeyIsUnique(t *testing.T) {
	pool := imageUploadsTestPool(t)

	const key = "crucible/shared/mint.iso"
	if _, err := insertImageUpload(t, pool, models.ImageKindISO, models.ImageUploadUploaded, key); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := insertImageUpload(t, pool, models.ImageKindISO, models.ImageUploadPending, key); err == nil {
		t.Fatal("a second row reused the same object_key; idx_image_uploads_object_key is missing")
	}
}

// TestImageUploads_UploaderDeletionKeepsTheRow covers the FK's ON DELETE SET
// NULL. Deleting a departed instructor must not cascade away the record of a
// multi-GB object still sitting in MinIO.
func TestImageUploads_UploaderDeletionKeepsTheRow(t *testing.T) {
	pool := imageUploadsTestPool(t)
	ctx := context.Background()

	probe := uuid.NewString()
	var userID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (oidc_sub, username, email, role)
		VALUES ($1, $2, $3, 'instructor') RETURNING id
	`, "oidc-"+probe, "img-probe-"+probe[:8], probe+"@example.invalid").Scan(&userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	var imageID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO image_uploads (filename, kind, status, object_key, uploaded_by)
		VALUES ('mint.iso', 'iso', 'uploaded', 'crucible/fk/mint.iso', $1)
		RETURNING id
	`, userID).Scan(&imageID); err != nil {
		t.Fatalf("create image upload: %v", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var uploadedBy *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT uploaded_by FROM image_uploads WHERE id = $1`, imageID).Scan(&uploadedBy); err != nil {
		t.Fatalf("row did not survive its uploader being deleted: %v", err)
	}
	if uploadedBy != nil {
		t.Errorf("uploaded_by = %v; want NULL after ON DELETE SET NULL", uploadedBy)
	}
}

// TestCountStuckImageUploads_CountsAbandonedPending is the direct check on the
// blind spot: nothing in the codebase ever writes status 'uploading', so an
// abandoned upload sits in 'pending' forever. While the query only looked at
// uploading/importing it could not see any of them, which made the
// crucible_image_uploads_stuck gauge read zero while MinIO leaked.
func TestCountStuckImageUploads_CountsAbandonedPending(t *testing.T) {
	pool := imageUploadsTestPool(t)
	ctx := context.Background()
	q := NewQueries(pool)

	// Fresh rows in every state, plus aged rows in each non-terminal state.
	mustInsert := func(status string, ageHours int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO image_uploads (filename, kind, status, object_key, created_at, updated_at)
			VALUES ($1, 'iso', $2, $3, now() - make_interval(hours => $4), now() - make_interval(hours => $4))
		`, status+".iso", status, "crucible/"+status+"-"+uuid.NewString(), ageHours); err != nil {
			t.Fatalf("insert %s aged %dh: %v", status, ageHours, err)
		}
	}

	// Aged and non-terminal: all three must be counted.
	mustInsert(models.ImageUploadPending, 3)
	mustInsert(models.ImageUploadUploading, 3)
	mustInsert(models.ImageUploadImporting, 3)
	// Aged but terminal: never counted, however old.
	mustInsert(models.ImageUploadImported, 72)
	mustInsert(models.ImageUploadError, 72)
	// Fresh and non-terminal: an upload in progress must not alert.
	mustInsert(models.ImageUploadPending, 0)
	mustInsert(models.ImageUploadImporting, 0)

	n, err := q.CountStuckImageUploads(ctx, time.Hour)
	if err != nil {
		t.Fatalf("CountStuckImageUploads: %v", err)
	}
	if n != 3 {
		t.Fatalf("stuck count = %d, want 3 (aged pending + uploading + importing); "+
			"a count of 2 means 'pending' was dropped from the status list and abandoned "+
			"uploads are invisible again", n)
	}

	// A threshold longer than every row's age must report all clear, proving
	// the query is genuinely age-sensitive rather than counting by status.
	if n, err = q.CountStuckImageUploads(ctx, 48*time.Hour); err != nil {
		t.Fatalf("CountStuckImageUploads(48h): %v", err)
	}
	if n != 0 {
		t.Errorf("stuck count with a 48h threshold = %d, want 0", n)
	}
}

// TestTemplates_OVFSourceTypeAndSkipGeneralize covers migration 000039. The
// wizard's whole OVF path depends on the templates CHECK admitting 'ovf'; if
// the constraint and models.TemplateSourceOVF disagree, every OVA draft fails
// at INSERT with a constraint violation rather than a useful error.
func TestTemplates_OVFSourceTypeAndSkipGeneralize(t *testing.T) {
	pool := imageUploadsTestPool(t)
	ctx := context.Background()

	newTemplate := func(sourceType string, skipGeneralize bool) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO templates (name, vcenter_template, os_type, source_type, source_ref, skip_generalize, kind)
			VALUES ($1, '', 'linux', $2, 'vm-123', $3, $4)
		`, "probe-"+uuid.NewString()[:8], sourceType, skipGeneralize, models.TemplateKindCloneNoCustomize)
		return err
	}

	for _, sourceType := range []string{
		models.TemplateSourceOVF,
		models.TemplateSourceCloneVCenter,
		models.TemplateSourceISO,
		models.TemplateSourceCloneTemplate,
		models.TemplateSourceManual,
	} {
		if err := newTemplate(sourceType, false); err != nil {
			t.Errorf("source_type %q is rejected by the templates CHECK: %v", sourceType, err)
		}
	}
	if err := newTemplate("ova", false); err == nil {
		t.Error("source_type 'ova' was accepted; the wizard's value is 'ovf' and the CHECK must say so")
	}

	// skip_generalize must persist. The DB does not police which source types
	// may set it — models.ValidateSkipGeneralize does — so the column only has
	// to round-trip.
	if err := newTemplate(models.TemplateSourceOVF, true); err != nil {
		t.Fatalf("ovf + skip_generalize=true insert failed: %v", err)
	}
	var skipped int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM templates WHERE source_type = $1 AND skip_generalize
	`, models.TemplateSourceOVF).Scan(&skipped); err != nil {
		t.Fatalf("count skip_generalize rows: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skip_generalize rows = %d, want 1; the column did not persist", skipped)
	}

	// kind=clone_no_customize is what an appliance OVA template must be, so
	// the kind CHECK has to admit it.
	if err := newTemplate(models.TemplateSourceOVF, false); err != nil {
		t.Errorf("kind=%s is rejected by the templates CHECK: %v", models.TemplateKindCloneNoCustomize, err)
	}
}
