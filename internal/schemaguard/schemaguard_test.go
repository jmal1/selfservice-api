package schemaguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (go.mod)")
	return ""
}

func loadRepoSchema(t *testing.T) Schema {
	t.Helper()
	schema, err := LoadSchema(filepath.Join(repoRoot(t), "internal", "database", "migrations"))
	if err != nil {
		t.Fatalf("LoadSchema: %v", err)
	}
	return schema
}

// TestLoadSchema_ParsesRealMigrations is the sanity floor. If migration parsing
// silently produced an empty or tiny schema, every other check here would pass
// vacuously -- which is precisely the failure mode this package exists to stop.
func TestLoadSchema_ParsesRealMigrations(t *testing.T) {
	schema := loadRepoSchema(t)

	if len(schema) < 15 {
		t.Fatalf("only %d tables parsed from migrations (%v); expected the full schema. "+
			"A near-empty schema makes every consistency check pass vacuously.",
			len(schema), schema.Tables())
	}

	// Spot-check tables and columns that the guard's value depends on.
	want := map[string][]string{
		"pods":                   {"id", "name", "owner_id", "vlan_id", "status"},
		"blueprint_vms":          {"id", "blueprint_id", "template_id", "boot_order", "quantity"},
		"blueprint_vm_playlists": {"blueprint_id", "vm_slot", "playlist_id"},
		"template_playlists":     {"template_id", "playlist_id"},
		"templates":              {"id", "name", "template_state"},
	}
	for table, cols := range want {
		got, ok := schema[table]
		if !ok {
			t.Errorf("table %q missing from parsed schema", table)
			continue
		}
		for _, c := range cols {
			if !got[c] {
				t.Errorf("table %q is missing expected column %q", table, c)
			}
		}
	}

	// The defect that motivated this package: blueprint_vms has no `slot`.
	if schema["blueprint_vms"]["slot"] {
		t.Error(`schema says blueprint_vms.slot exists; it never has. ` +
			`If a migration genuinely added it, update this test deliberately.`)
	}
}

// TestLoadSchema_HandlesDropAndReAddInOneFile pins the ordering behaviour that
// a naive per-file parse gets wrong. Migration 000003 drops pods.vlan_id and
// re-adds it later in the same file; processing DROP after ADD would leave the
// column missing and produce a flood of false positives.
func TestLoadSchema_HandlesDropAndReAddInOneFile(t *testing.T) {
	schema := Schema{}
	applyMigration(schema, `
		CREATE TABLE t (id UUID PRIMARY KEY, keep TEXT, doomed TEXT);
		ALTER TABLE t DROP COLUMN doomed;
		ALTER TABLE t ADD COLUMN doomed INT;
		ALTER TABLE t DROP COLUMN keep;
	`)
	if !schema["t"]["doomed"] {
		t.Error("doomed should exist: it was dropped then re-added later in the same file")
	}
	if schema["t"]["keep"] {
		t.Error("keep should be gone: it was dropped after creation")
	}
}

// TestParseColumnNames_HandlesNestedParens guards the reason this uses balanced
// scanning instead of a regex.
func TestParseColumnNames_HandlesNestedParens(t *testing.T) {
	body := `
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		price NUMERIC(10,2) NOT NULL,
		status TEXT NOT NULL CHECK (status IN ('a','b','c')),
		label TEXT DEFAULT 'has, a comma',
		CONSTRAINT uq UNIQUE (id, status),
		PRIMARY KEY (id)
	`
	got := parseColumnNames(body)
	want := []string{"id", "price", "status", "label"}
	if len(got) != len(want) {
		t.Fatalf("got columns %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("column %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestCheckSQL_CatchesTheBlueprintVMsSlotBug is the regression test for the
// outage. It reconstructs the exact query shape that shipped and asserts the
// guard flags it against the real migration-derived schema.
func TestCheckSQL_CatchesTheBlueprintVMsSlotBug(t *testing.T) {
	schema := loadRepoSchema(t)

	broken := `
		SELECT p.id, bvp.playlist_id
		FROM pod_vms pv
		JOIN blueprint_vms bv ON bv.blueprint_id = p.blueprint_id
		JOIN blueprint_vm_playlists bvp ON bv.slot = bvp.vm_slot
		WHERE pv.pod_id = $1
	`
	findings, checked := checkSQL(broken, schema)
	if checked == 0 {
		t.Fatal("checked 0 references; the guard would pass vacuously")
	}
	var found bool
	for _, f := range findings {
		if f.Table == "blueprint_vms" && f.Column == "slot" {
			found = true
		}
	}
	if !found {
		t.Errorf("guard did not flag blueprint_vms.slot. findings=%v", findings)
	}

	// The corrected query must be clean, otherwise the guard is just noisy.
	fixed := strings.Replace(broken, "bv.slot", "bv.boot_order", 1)
	findings, _ = checkSQL(fixed, schema)
	for _, f := range findings {
		if f.Table == "blueprint_vms" {
			t.Errorf("corrected query still flagged: %v", f)
		}
	}
}

// TestCheckSQL_SkipsConstructsItCannotResolve documents the deliberate blind
// spots. Reporting these would produce false positives, and a guard that cries
// wolf gets muted -- which costs more than the coverage is worth.
func TestCheckSQL_SkipsConstructsItCannotResolve(t *testing.T) {
	schema := Schema{
		"pods": {"id": true, "name": true},
	}
	cases := []struct {
		name string
		sql  string
	}{
		{"upsert EXCLUDED pseudo-table", `INSERT INTO pods (id, name) VALUES ($1,$2) ON CONFLICT (id) DO UPDATE SET name = excluded.name`},
		{"CTE shadowing a real name", `WITH pods AS (SELECT 1 AS anything) SELECT pods.anything FROM pods`},
		{"derived table alias", `SELECT x.whatever FROM (SELECT 1 AS whatever) x`},
		{"unknown table entirely", `SELECT z.nope FROM some_other_service_table z`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings, _ := checkSQL(tc.sql, schema)
			if len(findings) != 0 {
				t.Errorf("expected no findings, got %v", findings)
			}
		})
	}
}

// TestCheckSQL_ResolvesAliasesAndBareTables covers the binding forms actually
// used in this repo.
func TestCheckSQL_ResolvesAliasesAndBareTables(t *testing.T) {
	schema := Schema{"pods": {"id": true, "owner_id": true}}

	bad := []string{
		`SELECT p.nonexistent FROM pods p WHERE p.id = $1`,
		`SELECT p.nonexistent FROM pods AS p WHERE p.id = $1`,
		`SELECT pods.nonexistent FROM pods WHERE pods.id = $1`,
		`UPDATE pods SET x = 1 WHERE pods.nonexistent = $1`,
	}
	for _, sql := range bad {
		findings, checked := checkSQL(sql, schema)
		if checked == 0 {
			t.Errorf("checked nothing for: %s", sql)
		}
		if len(findings) == 0 {
			t.Errorf("expected a finding for: %s", sql)
		}
	}

	good := `SELECT p.id, p.owner_id FROM pods p JOIN pods p2 ON p2.id = p.owner_id`
	if findings, _ := checkSQL(good, schema); len(findings) != 0 {
		t.Errorf("false positive on valid SQL: %v", findings)
	}
}

// TestRepoSQLMatchesMigrations is the repo-wide guard. It is the check that
// would have caught blueprint_vms.slot before it reached production.
func TestRepoSQLMatchesMigrations(t *testing.T) {
	root := repoRoot(t)
	schema := loadRepoSchema(t)

	findings, stats, err := CheckDir(filepath.Join(root, "internal"), schema)
	if err != nil {
		t.Fatalf("CheckDir: %v", err)
	}
	t.Logf("scanned %d Go files, %d SQL literals, %d qualified refs against %d tables",
		stats.GoFiles, stats.SQLLiterals, stats.RefsChecked, stats.TablesInScope)

	// Fail a vacuous run loudly. If a refactor moved the queries elsewhere, a
	// silently-zero scan must not read as "all clear".
	if stats.SQLLiterals < 20 {
		t.Fatalf("only %d SQL literals found; expected many. The scanner is probably broken, "+
			"and a broken scanner reports success.", stats.SQLLiterals)
	}
	if stats.RefsChecked < 50 {
		t.Fatalf("only %d qualified column references checked; expected many. "+
			"Alias resolution is probably broken.", stats.RefsChecked)
	}

	if len(findings) > 0 {
		var b strings.Builder
		b.WriteString("SQL references columns that the migrations do not define:\n\n")
		for _, f := range findings {
			b.WriteString(f.String())
			b.WriteString("\n\n")
		}
		b.WriteString("Each of these will fail at runtime with SQLSTATE 42703 (undefined_column).\n" +
			"This is exactly how GET /pods/{id}/testing 500'd for every pod.\n" +
			"Do NOT silence this by loosening the guard -- fix the query or the migration.")
		t.Fatal(b.String())
	}
}
