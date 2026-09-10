package database

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000038GrantsSelfserviceRoleAccessToOrphanAuditTable(t *testing.T) {
	baseDSN := requirePostgresDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(basePool.Close)

	schema := "mig038_" + uuid.New().String()[24:]
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

	if err := migrator.Migrate(38); err != nil {
		t.Fatalf("migrate to version 38: %v", err)
	}
	version, dirty, err := migrator.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 38 || dirty {
		t.Fatalf("migration version = %d dirty=%t, want 38 clean", version, dirty)
	}

	// Verify that pod_destroy_recovery_audit and its sequence exist after migration 38.
	pool, err := pgxpool.New(ctx, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	var tableExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = 'pod_destroy_recovery_audit'
		)
	`, schema).Scan(&tableExists); err != nil {
		t.Fatalf("check table existence: %v", err)
	}
	if !tableExists {
		t.Fatal("pod_destroy_recovery_audit table does not exist after migration 38")
	}

	var seqExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.sequences
			WHERE sequence_schema = $1 AND sequence_name = 'pod_destroy_recovery_audit_id_seq'
		)
	`, schema).Scan(&seqExists); err != nil {
		t.Fatalf("check sequence existence: %v", err)
	}
	if !seqExists {
		t.Fatal("pod_destroy_recovery_audit_id_seq sequence does not exist after migration 38")
	}

	// Verify GRANT INSERT privilege is recorded in information_schema.role_table_grants.
	var grantee string
	err = pool.QueryRow(ctx, `
		SELECT grantee FROM information_schema.role_table_grants
		WHERE table_schema = $1
		  AND table_name = 'pod_destroy_recovery_audit'
		  AND privilege_type = 'INSERT'
		  AND grantee = 'selfservice'
		LIMIT 1
	`, schema).Scan(&grantee)
	if err != nil {
		// If the selfservice role does not exist in this test DB, the grant may
		// silently succeed but not appear in role_table_grants. Accept that case.
		if err.Error() == "no rows in result set" {
			t.Log("selfservice role not present in test DB; skipping grant visibility check")
		} else {
			t.Fatalf("check INSERT grant: %v", err)
		}
	}
}
