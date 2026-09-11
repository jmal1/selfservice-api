// Package schemaguard cross-checks the SQL embedded in Go source against the
// schema the migrations actually produce.
//
// It exists because of a real production outage: internal/database queried
// blueprint_vms.slot, a column that has never existed. Postgres answered
// SQLSTATE 42703 and GET /pods/{id}/testing returned 500 for every pod, for
// the entire life of the endpoint. Nothing caught it -- the code compiled,
// unit tests passed (they never hit Postgres), and the one synthetic covering
// that route probes a nonexistent pod, so it 404s before the query runs.
//
// The narrower predecessor of this package, internal/engine's
// schema_consistency_test.go, could not have caught it either: it scans only
// its own package directory and validates columns for only the pods table.
//
// Design notes:
//
//   - The schema is built by replaying migrations as a position-ordered edit
//     list, not by parsing each file independently. Migration 000003 drops and
//     re-adds pods.vlan_id within a single file, so order within a file matters
//     as much as order between files.
//
//   - CREATE TABLE bodies are extracted with balanced-paren scanning rather
//     than a regex, because column definitions contain nested parens
//     (NUMERIC(10,2), CHECK (x IN ('a','b')), DEFAULT gen_random_uuid()).
//
//   - SQL is lifted from Go string literals via go/parser rather than by
//     text-scanning, so comments (including doc comments that quote the old
//     broken SQL) are excluded for free.
//
//   - Checking is deliberately conservative: a reference is only reported when
//     its alias can be bound to a known table. CTEs, derived tables and the
//     EXCLUDED/OLD/NEW pseudo-tables are skipped rather than guessed at. A
//     guard that cries wolf gets muted, which is worse than no guard.
package schemaguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Schema maps table name -> set of column names.
type Schema map[string]map[string]bool

// Tables returns the known table names, sorted.
func (s Schema) Tables() []string {
	out := make([]string, 0, len(s))
	for t := range s {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Finding is a single SQL reference that does not match the schema.
type Finding struct {
	File    string
	Table   string
	Alias   string
	Column  string
	Snippet string
}

func (f Finding) String() string {
	where := f.Table
	if f.Alias != "" && f.Alias != f.Table {
		where = fmt.Sprintf("%s (aliased %q)", f.Table, f.Alias)
	}
	return fmt.Sprintf("%s: column %q does not exist on table %s\n    in: %s",
		f.File, f.Column, where, f.Snippet)
}

// Stats records how much work a scan actually did, so callers can fail a
// vacuous run. A guard that silently scanned zero queries is indistinguishable
// from a guard that found no problems.
type Stats struct {
	GoFiles       int
	SQLLiterals   int
	RefsChecked   int
	TablesInScope int
}

// ---------------------------------------------------------------------------
// Migration replay
// ---------------------------------------------------------------------------

var (
	reLineComment  = regexp.MustCompile(`--[^\n]*`)
	reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

	reCreateTable = regexp.MustCompile(`(?is)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s*\(`)
	reDropTable   = regexp.MustCompile(`(?is)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)
	reAddColumn   = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)
	reDropColumn  = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)
	reRenameCol   = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s+RENAME\s+(?:COLUMN\s+)?"?([a-z_][a-z0-9_]*)"?\s+TO\s+"?([a-z_][a-z0-9_]*)"?`)
	reRenameTable = regexp.MustCompile(`(?is)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?\s+RENAME\s+TO\s+"?([a-z_][a-z0-9_]*)"?`)
)

type edit struct {
	pos   int
	apply func(Schema)
}

// LoadSchema replays every *.up.sql in dir, in filename order, and returns the
// resulting schema.
func LoadSchema(dir string) (Schema, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no *.up.sql migrations found in %s", dir)
	}
	sort.Strings(files)

	schema := Schema{}
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		applyMigration(schema, stripSQLComments(string(raw)))
	}
	return schema, nil
}

func stripSQLComments(s string) string {
	s = reBlockComment.ReplaceAllStringFunc(s, blankOut)
	s = reLineComment.ReplaceAllStringFunc(s, blankOut)
	return s
}

// blankOut preserves length (and newlines) so byte offsets stay meaningful.
func blankOut(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
	return string(b)
}

// applyMigration collects every DDL edit in the file with its byte offset,
// sorts by offset, then applies. Order within a file matters: migration 000003
// drops pods.vlan_id and re-adds it a few statements later.
func applyMigration(schema Schema, sql string) {
	var edits []edit

	for _, m := range reCreateTable.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		body, ok := balancedParenBody(sql, m[1]-1)
		if !ok {
			continue
		}
		cols := parseColumnNames(body)
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			set := map[string]bool{}
			for _, c := range cols {
				set[c] = true
			}
			s[table] = set
		}})
	}

	for _, m := range reAddColumn.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		col := strings.ToLower(sql[m[4]:m[5]])
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			if s[table] == nil {
				s[table] = map[string]bool{}
			}
			s[table][col] = true
		}})
	}

	for _, m := range reDropColumn.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		col := strings.ToLower(sql[m[4]:m[5]])
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			if s[table] != nil {
				delete(s[table], col)
			}
		}})
	}

	for _, m := range reRenameCol.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		from := strings.ToLower(sql[m[4]:m[5]])
		to := strings.ToLower(sql[m[6]:m[7]])
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			if s[table] != nil {
				delete(s[table], from)
				s[table][to] = true
			}
		}})
	}

	for _, m := range reRenameTable.FindAllStringSubmatchIndex(sql, -1) {
		from := strings.ToLower(sql[m[2]:m[3]])
		to := strings.ToLower(sql[m[4]:m[5]])
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			if cols, ok := s[from]; ok {
				s[to] = cols
				delete(s, from)
			}
		}})
	}

	for _, m := range reDropTable.FindAllStringSubmatchIndex(sql, -1) {
		table := strings.ToLower(sql[m[2]:m[3]])
		edits = append(edits, edit{pos: m[0], apply: func(s Schema) {
			delete(s, table)
		}})
	}

	sort.SliceStable(edits, func(i, j int) bool { return edits[i].pos < edits[j].pos })
	for _, e := range edits {
		e.apply(schema)
	}
}

// balancedParenBody returns the text between the paren at open and its match.
func balancedParenBody(s string, open int) (string, bool) {
	if open < 0 || open >= len(s) || s[open] != '(' {
		return "", false
	}
	depth := 0
	inSingle := false
	for i := open; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return s[open+1 : i], true
			}
		}
	}
	return "", false
}

var constraintLeaders = map[string]bool{
	"primary": true, "foreign": true, "unique": true, "check": true,
	"constraint": true, "exclude": true, "like": true, "deferrable": true,
}

// parseColumnNames splits a CREATE TABLE body on top-level commas and takes the
// leading identifier of each part, skipping table-level constraints.
func parseColumnNames(body string) []string {
	var cols []string
	depth := 0
	inSingle := false
	start := 0
	parts := []string{}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case c == '\'':
			inSingle = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, body[start:i])
			start = i + 1
		}
	}
	parts = append(parts, body[start:])

	for _, p := range parts {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(strings.Trim(f[0], `"`))
		if constraintLeaders[name] {
			continue
		}
		if name == "" || !isIdent(name) {
			continue
		}
		cols = append(cols, name)
	}
	return cols
}

func isIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if i == 0 && (c >= '0' && c <= '9') {
			return false
		}
		if !ok {
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// Go source scanning
// ---------------------------------------------------------------------------

var (
	reSQLish = regexp.MustCompile(`(?is)\b(SELECT\b.*\bFROM|INSERT\s+INTO|UPDATE\b.*\bSET|DELETE\s+FROM)\b`)

	// Binds a table to an optional alias. The identifier requirement means
	// "FROM (" (a derived table) simply does not match, which is what we want.
	reTableBinding = regexp.MustCompile(`(?is)\b(?:FROM|JOIN|UPDATE|INTO)\s+"?([a-z_][a-z0-9_]*)"?(?:\s+(?:AS\s+)?"?([a-z_][a-z0-9_]*)"?)?`)

	reQualifiedRef = regexp.MustCompile(`\b([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)\b`)

	reCTEName = regexp.MustCompile(`(?is)\b([a-z_][a-z0-9_]*)\s+AS\s*\(`)
)

// sqlKeywords are words that can follow a table name but are not an alias.
var sqlKeywords = map[string]bool{
	"on": true, "where": true, "set": true, "group": true, "order": true,
	"limit": true, "offset": true, "using": true, "left": true, "right": true,
	"inner": true, "outer": true, "full": true, "cross": true, "join": true,
	"values": true, "as": true, "returning": true, "select": true, "from": true,
	"union": true, "having": true, "and": true, "or": true, "with": true,
	"lateral": true, "natural": true, "for": true, "into": true, "do": true,
	"conflict": true, "not": true, "exists": true, "in": true, "is": true,
	"null": true, "case": true, "when": true, "then": true, "else": true,
	"end": true, "distinct": true, "all": true, "desc": true, "asc": true,
	"window": true, "fetch": true, "only": true,
}

// pseudoTables are qualifiers that are valid SQL but are not real tables.
var pseudoTables = map[string]bool{
	"excluded": true, "old": true, "new": true,
}

// LooksLikeSQL reports whether a string literal is worth checking.
func LooksLikeSQL(s string) bool {
	if len(s) < 20 {
		return false
	}
	return reSQLish.MatchString(s)
}

// CheckDir walks dir for non-test Go files and validates the qualified column
// references in their SQL literals against schema.
func CheckDir(dir string, schema Schema) ([]Finding, Stats, error) {
	stats := Stats{TablesInScope: len(schema)}
	var findings []Finding

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "vendor" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		stats.GoFiles++

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file we cannot parse is not a schema problem; skip it.
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil || !LooksLikeSQL(val) {
				return true
			}
			stats.SQLLiterals++
			fs, refs := checkSQL(val, schema)
			stats.RefsChecked += refs
			for i := range fs {
				fs[i].File = fmt.Sprintf("%s:%d", path, fset.Position(lit.Pos()).Line)
				findings = append(findings, fs[i])
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, stats, err
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Column < findings[j].Column
	})
	return findings, stats, nil
}

// checkSQL validates one SQL string, returning findings and the number of
// references it was actually able to check.
func checkSQL(sql string, schema Schema) ([]Finding, int) {
	aliases := map[string]string{} // alias (or table) -> table
	for _, m := range reTableBinding.FindAllStringSubmatch(sql, -1) {
		table := strings.ToLower(m[1])
		if _, known := schema[table]; !known {
			continue
		}
		aliases[table] = table
		if alias := strings.ToLower(m[2]); alias != "" && !sqlKeywords[alias] {
			aliases[alias] = table
		}
	}

	// Anything introduced as a CTE shadows a real table of the same name and
	// has columns we cannot know, so drop it from consideration entirely.
	for _, m := range reCTEName.FindAllStringSubmatch(sql, -1) {
		delete(aliases, strings.ToLower(m[1]))
	}

	if len(aliases) == 0 {
		return nil, 0
	}

	var findings []Finding
	checked := 0
	seen := map[string]bool{}

	for _, m := range reQualifiedRef.FindAllStringSubmatch(sql, -1) {
		qual := strings.ToLower(m[1])
		col := strings.ToLower(m[2])
		if pseudoTables[qual] || sqlKeywords[qual] {
			continue
		}
		table, ok := aliases[qual]
		if !ok {
			continue // unknown binding: a CTE, a subquery alias, or a schema prefix
		}
		cols := schema[table]
		if len(cols) == 0 {
			continue
		}
		checked++
		if cols[col] {
			continue
		}
		key := qual + "." + col
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, Finding{
			Table:   table,
			Alias:   qual,
			Column:  col,
			Snippet: snippet(sql, key),
		})
	}
	return findings, checked
}

func snippet(sql, needle string) string {
	flat := strings.Join(strings.Fields(sql), " ")
	idx := strings.Index(strings.ToLower(flat), needle)
	if idx < 0 {
		if len(flat) > 120 {
			return flat[:120] + "..."
		}
		return flat
	}
	start := idx - 50
	if start < 0 {
		start = 0
	}
	end := idx + 70
	if end > len(flat) {
		end = len(flat)
	}
	out := flat[start:end]
	if start > 0 {
		out = "..." + out
	}
	if end < len(flat) {
		out += "..."
	}
	return out
}
