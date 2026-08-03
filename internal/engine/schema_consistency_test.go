package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEngineSQLReferencesOnlyRealTables is a schema-consistency guard.
//
// It exists because of a production incident: GetPodVLANTag queried a table
// named `vlans` that has never existed in any migration (the real table is
// `vlan_pool`). Go's compiler cannot see inside a SQL string, no unit test
// touched a database, and the failure only surfaced at runtime — as
// `relation "vlans" does not exist` — which meant *every* kali_runner
// assessment failed the moment it tried to provision a runner. The feature
// had never worked in production and nothing detected it.
//
// Rather than guard that single query, this test extracts every table
// referenced by FROM/JOIN/INSERT INTO/UPDATE/DELETE FROM across the package's
// SQL literals and asserts each one is actually created by a migration. That
// catches the whole class: typos, renames, and queries written against a
// schema that only ever existed in someone's head.
func TestEngineSQLReferencesOnlyRealTables(t *testing.T) {
	known := tablesFromMigrations(t)
	if len(known) == 0 {
		t.Fatal("parsed zero tables from migrations — the parser or the path is wrong, " +
			"which would make this guard silently vacuous")
	}
	t.Logf("known tables from migrations (%d): %s", len(known), strings.Join(sortedKeys(known), ", "))

	referenced := tablesReferencedInGoSQL(t, ".")
	if len(referenced) == 0 {
		t.Fatal("found zero table references in package SQL — the extractor is broken, " +
			"which would make this guard silently vacuous")
	}

	for _, ref := range sortedKeys(referenced) {
		if !known[ref] {
			t.Errorf("SQL references table %q, which no migration creates.\n"+
				"  Referenced in: %s\n"+
				"  Known tables: %s",
				ref, strings.Join(referenced[ref], ", "), strings.Join(sortedKeys(known), ", "))
		}
	}
}

// TestGetPodVLANTagReadsPodsDirectly pins the specific fix. pods.vlan_id holds
// the VLAN *tag*, not a foreign key, so this query must not join a lookup
// table — a join would reintroduce the original bug in a subtler form by
// matching a tag against a serial primary key.
func TestGetPodVLANTagReadsPodsDirectly(t *testing.T) {
	src := readFile(t, "queries.go")

	fn := funcBody(t, src, "func (q *Queries) GetPodVLANTag(")
	if fn == "" {
		t.Fatal("could not locate GetPodVLANTag — was it renamed? Update this test.")
	}

	sql := strings.ToLower(fn)
	if strings.Contains(sql, "join") {
		t.Errorf("GetPodVLANTag must not JOIN: pods.vlan_id already stores the VLAN tag, "+
			"so any join matches a tag against a different column's key space.\nGot:\n%s", fn)
	}
	if !strings.Contains(sql, "select vlan_id from pods") {
		t.Errorf("GetPodVLANTag should read pods.vlan_id directly.\nGot:\n%s", fn)
	}
	if strings.Contains(sql, "vlans ") || strings.Contains(sql, "join vlans") {
		t.Errorf("GetPodVLANTag references a table named `vlans`, which does not exist "+
			"(the real table is `vlan_pool`).\nGot:\n%s", fn)
	}
}

// TestEnginePodsColumnsExist guards column references on `pods`, the table the
// runner path depends on most.
//
// The `vlans` incident came with a second defect in the same function:
// `COALESCE(p.pod_index, 0)`, where pods.pod_index had been dropped by
// migration 000003 years earlier. A table-level guard would not have caught
// it. Columns are harder to track in general (ALTERs, renames, per-table
// aliasing), so this deliberately covers one high-value table rather than
// pretending to cover all of them.
func TestEnginePodsColumnsExist(t *testing.T) {
	cols := podsColumnsFromMigrations(t)
	if len(cols) == 0 {
		t.Fatal("parsed zero columns for `pods` — parser broken, guard would be vacuous")
	}
	t.Logf("pods columns from migrations (%d): %s", len(cols), strings.Join(sortedKeys(cols), ", "))

	// Sanity-check the parser against columns we know exist, so a regression in
	// the parser cannot silently turn this guard into a no-op.
	for _, must := range []string{"id", "owner_id", "vlan_id", "subnet", "status"} {
		if !cols[must] {
			t.Fatalf("parser did not find known pods column %q — fix the parser before trusting this test", must)
		}
	}
	if cols["pod_index"] {
		t.Error("parser thinks pods.pod_index exists, but migration 000003 drops it — " +
			"DROP COLUMN handling is broken")
	}

	// p.<col> / pods.<col> references inside SQL literals.
	re := regexp.MustCompile(`(?i)\b(?:p|pods)\.([a-z_][a-z0-9_]*)`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		for _, lit := range rawStringLiterals(readFile(t, name)) {
			if !looksLikeSQL(lit) || !mentionsPodsTable(lit) {
				continue
			}
			for _, m := range re.FindAllStringSubmatch(lit, -1) {
				col := strings.ToLower(m[1])
				checked++
				if !cols[col] {
					t.Errorf("%s: SQL references pods.%s, which no migration creates.\nSQL:\n%s",
						name, col, strings.TrimSpace(lit))
				}
			}
		}
	}
	t.Logf("checked %d pods.<column> references", checked)
}

// mentionsPodsTable reports whether a SQL literal actually queries `pods`, so
// that a `p.` alias bound to some other table isn't checked against pods'
// columns.
func mentionsPodsTable(sql string) bool {
	re := regexp.MustCompile(`(?is)\b(?:FROM|JOIN|UPDATE|INSERT\s+INTO)\s+pods\b`)
	return re.MatchString(sql)
}

// podsColumnsFromMigrations replays CREATE TABLE pods / ADD COLUMN / DROP
// COLUMN across migrations in filename order.
func podsColumnsFromMigrations(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "database", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	createRe := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?pods\s*\((.*?)\n\s*\);`)
	addRe := regexp.MustCompile(`(?is)ALTER\s+TABLE\s+pods\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?["']?([a-z_][a-z0-9_]*)["']?`)
	dropRe := regexp.MustCompile(`(?is)ALTER\s+TABLE\s+pods\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?["']?([a-z_][a-z0-9_]*)["']?`)
	renameRe := regexp.MustCompile(`(?is)ALTER\s+TABLE\s+pods\s+RENAME\s+COLUMN\s+["']?([a-z_][a-z0-9_]*)["']?\s+TO\s+["']?([a-z_][a-z0-9_]*)["']?`)
	colRe := regexp.MustCompile(`(?im)^\s*["']?([a-z_][a-z0-9_]*)["']?\s+[a-z]`)

	cols := map[string]bool{}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sql := stripSQLComments(string(body))

		if m := createRe.FindStringSubmatch(sql); m != nil {
			for _, line := range strings.Split(m[1], "\n") {
				trimmed := strings.TrimSpace(strings.ToUpper(line))
				// Skip table-level constraints, which are not columns.
				if strings.HasPrefix(trimmed, "PRIMARY KEY") || strings.HasPrefix(trimmed, "FOREIGN KEY") ||
					strings.HasPrefix(trimmed, "UNIQUE") || strings.HasPrefix(trimmed, "CHECK") ||
					strings.HasPrefix(trimmed, "CONSTRAINT") {
					continue
				}
				if cm := colRe.FindStringSubmatch(line); cm != nil {
					cols[strings.ToLower(cm[1])] = true
				}
			}
		}

		// ALTERs must be replayed in source order: migration 000003 drops
		// pods.vlan_id and then re-adds it in the same file, so applying all
		// DROPs after all ADDs would wrongly conclude the column is gone.
		type edit struct {
			pos  int
			kind string
			a, b string
		}
		var edits []edit
		for _, m := range addRe.FindAllStringSubmatchIndex(sql, -1) {
			edits = append(edits, edit{pos: m[0], kind: "add", a: strings.ToLower(sql[m[2]:m[3]])})
		}
		for _, m := range dropRe.FindAllStringSubmatchIndex(sql, -1) {
			edits = append(edits, edit{pos: m[0], kind: "drop", a: strings.ToLower(sql[m[2]:m[3]])})
		}
		for _, m := range renameRe.FindAllStringSubmatchIndex(sql, -1) {
			edits = append(edits, edit{
				pos: m[0], kind: "rename",
				a: strings.ToLower(sql[m[2]:m[3]]),
				b: strings.ToLower(sql[m[4]:m[5]]),
			})
		}
		sort.Slice(edits, func(i, j int) bool { return edits[i].pos < edits[j].pos })
		for _, e := range edits {
			switch e.kind {
			case "add":
				cols[e.a] = true
			case "drop":
				delete(cols, e.a)
			case "rename":
				delete(cols, e.a)
				cols[e.b] = true
			}
		}
	}
	return cols
}

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["']?([a-z_][a-z0-9_]*)["']?`)
	alterRenameRe = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+["']?([a-z_][a-z0-9_]*)["']?\s+RENAME\s+TO\s+["']?([a-z_][a-z0-9_]*)["']?`)
	dropTableRe   = regexp.MustCompile(`(?is)DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?["']?([a-z_][a-z0-9_]*)["']?`)

	// Table references inside SQL. Deliberately conservative: only matches an
	// identifier directly after the keyword, so expressions and subqueries
	// (`FROM (`) are skipped rather than producing false positives.
	tableRefRe = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|INSERT\s+INTO|UPDATE)\s+([a-z_][a-z0-9_]*)`)
)

// sqlKeywords are words that can follow FROM/UPDATE etc. but are not tables.
// `skip` matters in particular: `FOR UPDATE SKIP LOCKED` otherwise parses as
// an UPDATE against a table called "skip".
var sqlKeywords = map[string]bool{
	"select": true, "values": true, "set": true, "where": true, "lateral": true,
	"only": true, "unnest": true, "generate_series": true, "dual": true,
	"skip": true, "nowait": true, "of": true,
}

func tablesFromMigrations(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join("..", "database", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir %s: %v", dir, err)
	}

	// Apply migrations in filename order so renames/drops resolve correctly.
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	tables := map[string]bool{}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sql := stripSQLComments(string(body))

		for _, m := range createTableRe.FindAllStringSubmatch(sql, -1) {
			tables[strings.ToLower(m[1])] = true
		}
		for _, m := range alterRenameRe.FindAllStringSubmatch(sql, -1) {
			delete(tables, strings.ToLower(m[1]))
			tables[strings.ToLower(m[2])] = true
		}
		for _, m := range dropTableRe.FindAllStringSubmatch(sql, -1) {
			delete(tables, strings.ToLower(m[1]))
		}
	}
	return tables
}

// tablesReferencedInGoSQL scans raw-string literals in non-test Go files for
// SQL table references, returning table -> files that reference it.
func tablesReferencedInGoSQL(t *testing.T, dir string) map[string][]string {
	t.Helper()
	refs := map[string][]string{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src := readFile(t, name)
		for _, lit := range rawStringLiterals(src) {
			if !looksLikeSQL(lit) {
				continue
			}
			for _, m := range tableRefRe.FindAllStringSubmatch(lit, -1) {
				tbl := strings.ToLower(m[1])
				if sqlKeywords[tbl] {
					continue
				}
				if !contains(refs[tbl], name) {
					refs[tbl] = append(refs[tbl], name)
				}
			}
		}
	}
	return refs
}

func looksLikeSQL(s string) bool {
	u := strings.ToUpper(s)
	return strings.Contains(u, "SELECT ") || strings.Contains(u, "INSERT INTO") ||
		strings.Contains(u, "UPDATE ") || strings.Contains(u, "DELETE FROM")
}

// rawStringLiterals returns the contents of every backtick-quoted literal,
// after removing Go comments. Comments matter: a doc comment that quotes the
// old, broken SQL in backticks would otherwise be scanned as live code and
// re-report the very bug it documents.
func rawStringLiterals(src string) []string {
	src = stripGoComments(src)
	var out []string
	for {
		i := strings.Index(src, "`")
		if i < 0 {
			return out
		}
		rest := src[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		src = rest[j+1:]
	}
}

// stripGoComments removes // line comments and /* */ block comments. It is
// intentionally simple; it only needs to be right for this repo's source, and
// over-removal would at worst make the guard skip a query (which the
// "zero references" fatal check would catch if it ever became total).
func stripGoComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		if strings.HasPrefix(src[i:], "//") {
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				break
			}
			b.WriteByte('\n')
			i += j + 1
			continue
		}
		if strings.HasPrefix(src[i:], "/*") {
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				break
			}
			i += 2 + j + 2
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func funcBody(t *testing.T, src, sig string) string {
	t.Helper()
	i := strings.Index(src, sig)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
