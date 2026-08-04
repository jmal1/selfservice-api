package database_test

// TestMigrationsContiguous guards against the class of production incident where
// golang-migrate silently skips a migration because its number is below the
// current schema version. golang-migrate tracks a single integer watermark and
// never re-applies a migration numbered below it. If lane A merges 000026 and
// lane B later adds 000025, migrate will run to version 26, then permanently
// skip 000025 — the columns never appear while the code assumes they do, with
// no error anywhere.
//
// This test catches that before it reaches the database by asserting:
//   1. every .up.sql has a matching .down.sql and vice versa
//   2. version numbers are contiguous from 1 with no gaps and no duplicates
//   3. every filename matches the canonical pattern \d{6}_[a-z0-9_]+\.(up|down)\.sql

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var reFilename = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "migrations")
}

func TestMigrationsContiguous(t *testing.T) {
	dir := migrationsDir(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("cannot read migrations directory %s: %v", dir, err)
	}

	type entry struct {
		name    string
		version int
		stem    string
		kind    string // "up" or "down"
	}

	var malformed []string
	var parsed []entry

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := reFilename.FindStringSubmatch(e.Name())
		if m == nil {
			malformed = append(malformed, e.Name())
			continue
		}
		v, _ := strconv.Atoi(m[1])
		parsed = append(parsed, entry{
			name:    e.Name(),
			version: v,
			stem:    m[2],
			kind:    m[3],
		})
	}

	if len(malformed) > 0 {
		t.Errorf("migrations with non-canonical filenames (must match %%06d_[a-z0-9_]+.(up|down).sql):\n  %s",
			strings.Join(malformed, "\n  "))
	}

	// Build maps: version → up-stem, version → down-stem.
	type pair struct{ up, down string }
	pairs := map[int]*pair{}
	for _, e := range parsed {
		if pairs[e.version] == nil {
			pairs[e.version] = &pair{}
		}
		switch e.kind {
		case "up":
			if pairs[e.version].up != "" {
				t.Errorf("duplicate up migration for version %06d: %s and %s",
					e.version, pairs[e.version].up, e.stem)
			}
			pairs[e.version].up = e.stem
		case "down":
			if pairs[e.version].down != "" {
				t.Errorf("duplicate down migration for version %06d: %s and %s",
					e.version, pairs[e.version].down, e.stem)
			}
			pairs[e.version].down = e.stem
		}
	}

	// Collect sorted version list.
	versions := make([]int, 0, len(pairs))
	for v := range pairs {
		versions = append(versions, v)
	}
	sort.Ints(versions)

	// Check pairing: every up must have a matching down with the same stem.
	for _, v := range versions {
		p := pairs[v]
		if p.up == "" {
			t.Errorf("version %06d has a .down.sql but no .up.sql", v)
		}
		if p.down == "" {
			t.Errorf("version %06d has a .up.sql but no .down.sql", v)
		}
		if p.up != "" && p.down != "" && p.up != p.down {
			t.Errorf("version %06d up/down stem mismatch: up=%q down=%q", v, p.up, p.down)
		}
	}

	// Check contiguity: must be 1, 2, 3, … N with no gaps and no duplicates.
	// Duplicates are already caught above; check gaps here.
	if len(versions) == 0 {
		t.Error("no migrations found — migrations directory appears empty")
		return
	}
	if versions[0] != 1 {
		t.Errorf("lowest migration version is %06d, expected 000001", versions[0])
	}
	for i := 1; i < len(versions); i++ {
		prev := versions[i-1]
		cur := versions[i]
		if cur != prev+1 {
			missing := make([]string, 0, cur-prev-1)
			for gap := prev + 1; gap < cur; gap++ {
				missing = append(missing, fmt.Sprintf("%06d", gap))
			}
			t.Errorf(
				"gap in migration sequence between %06d and %06d — missing: %s\n"+
					"\nWARNING: golang-migrate tracks a single integer watermark and will SILENTLY SKIP\n"+
					"any migration numbered below the current schema version. If %06d merges first\n"+
					"and %s is added later, migrate will run to version %d and then permanently\n"+
					"ignore %s. The columns will never exist in production while the code assumes\n"+
					"they do, with no error anywhere. Fix by renumbering so the sequence is contiguous.",
				prev, cur, strings.Join(missing, ", "),
				cur, strings.Join(missing, "/"), cur, strings.Join(missing, "/"),
			)
		}
	}
}
