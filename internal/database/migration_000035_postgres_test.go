package database

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration000035SkipsMalformedLegacyRollbackEvidence(t *testing.T) {
	baseDSN := requirePostgresDSN(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	basePool, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(basePool.Close)

	schema := migrationTestSchemaName(uuid.New())
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
	t.Cleanup(func() {
		_, _ = migrator.Close()
	})
	if err := migrator.Migrate(34); err != nil {
		t.Fatalf("migrate fixture to version 34: %v", err)
	}

	pool, err := pgxpool.New(ctx, scopedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ownerID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, ownerID, ownerID.String(), ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}

	validReceipt := portGroupReceiptJSON(t, "Pod-VLAN3999", 3999)
	validSteps, err := json.Marshal([]map[string]any{{
		"name": "portgroup_create",
		"data": json.RawMessage(validReceipt),
	}})
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name          string
		rollbackSteps string
		wantReceipt   bool
	}{
		{name: "absent-data", rollbackSteps: `[{"name":"portgroup_create"}]`},
		{name: "null-data", rollbackSteps: `[{"name":"portgroup_create","data":null}]`},
		{name: "scalar-data", rollbackSteps: `[{"name":"portgroup_create","data":"receipt"}]`},
		{name: "array-data", rollbackSteps: `[{"name":"portgroup_create","data":[]}]`},
		{name: "null-hosts", rollbackSteps: `[{"name":"portgroup_create","data":{"name":"Pod-VLAN3999","vlan_id":3999,"hosts":null}}]`},
		{name: "incomplete-host", rollbackSteps: `[{"name":"portgroup_create","data":{"name":"Pod-VLAN3999","vlan_id":3999,"hosts":[{"host_name":"esxi1.lab.jmal.io"}]}}]`},
		{name: "null-steps", rollbackSteps: `null`},
		{name: "object-steps", rollbackSteps: `{"name":"portgroup_create"}`},
		{name: "valid-legacy-receipt", rollbackSteps: string(validSteps), wantReceipt: true},
	}

	podIDs := make(map[string]uuid.UUID, len(fixtures))
	for i, fixture := range fixtures {
		podID := uuid.New()
		podIDs[fixture.name] = podID
		vlanID := 3000 + i
		subnet := fmt.Sprintf("10.253.%d.0/24", i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO pods (id, owner_id, name, status, vlan_id, subnet)
			VALUES ($1, $2, $3, 'error', $4, $5)
		`, podID, ownerID, fixture.name+"-"+podID.String(), vlanID, subnet); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO jobs (id, type, payload, status, rollback_steps)
			VALUES (
				$1,
				'pod_create',
				jsonb_build_object('pod_id', $2::text),
				'failed',
				$3::jsonb
			)
		`, uuid.New(), podID, fixture.rollbackSteps); err != nil {
			t.Fatalf("insert %s fixture: %v", fixture.name, err)
		}
	}

	if err := migrator.Migrate(35); err != nil {
		t.Fatalf("migrate fixture to version 35: %v", err)
	}
	version, dirty, err := migrator.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 35 || dirty {
		t.Fatalf("migration version = %d dirty=%t, want 35 clean", version, dirty)
	}

	for _, fixture := range fixtures {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pod_portgroup_receipts
			WHERE pod_id = $1
		`, podIDs[fixture.name]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if fixture.wantReceipt {
			want = 1
		}
		if count != want {
			t.Errorf("%s receipt count = %d, want %d", fixture.name, count, want)
		}
	}

	var (
		storedReceipt []byte
		state         string
	)
	if err := pool.QueryRow(ctx, `
		SELECT receipt, state
		FROM pod_portgroup_receipts
		WHERE pod_id = $1
	`, podIDs["valid-legacy-receipt"]).Scan(&storedReceipt, &state); err != nil {
		t.Fatal(err)
	}
	if state != PodPortGroupReceiptLegacy {
		t.Fatalf("valid legacy receipt state = %q, want %q", state, PodPortGroupReceiptLegacy)
	}
	var wantJSON, gotJSON any
	if err := json.Unmarshal(validReceipt, &wantJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(storedReceipt, &gotJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("migration changed valid legacy evidence:\n got: %s\nwant: %s", storedReceipt, validReceipt)
	}
}

func postgresDSNWithSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatalf("TEST_DATABASE_URL scheme = %q, want postgres or postgresql", parsed.Scheme)
	}
	if parsed.Host == "" {
		t.Fatalf("TEST_DATABASE_URL has no host: %s", dsn)
	}
	return parsed.String()
}

func migrationTestSchemaName(id uuid.UUID) string {
	return fmt.Sprintf("migration_000035_%s", id.String()[24:])
}
