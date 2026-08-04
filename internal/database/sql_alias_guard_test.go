package database

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This file guards a bug class that shipped to production twice, both times as
// a total 500 on a list endpoint, and both times invisible to CI because there
// is no Postgres in the test environment.
//
//	1. `blueprintSelectCols` was an UNQUALIFIED column list ("id, name, ...")
//	   used in two queries that JOIN a table which also has an `id`
//	   (blueprint_access, users). Postgres rejects the whole statement with
//	   `column reference "id" is ambiguous`, so GET /api/v1/blueprints and
//	   GET /api/v1/admin/blueprints returned 500 for every caller.
//
//	2. `ListAllTemplates` ordered by `t.pinned` while selecting `FROM templates`
//	   with no alias -> `missing FROM-clause entry for table "t"`.
//
//	3. `ListBlueprintsForUser` combined SELECT DISTINCT with a pin ORDER BY
//	   built from CASE expressions, which Postgres rejects because a DISTINCT
//	   query may only order by expressions present in its select list.
//
// The blast radius was wider than "blueprints are broken": the new-pod wizard
// loads templates and blueprints in one Promise.all, so a single 500 on
// blueprints also took out template selection, and the create_and_destroy UI
// synthetic went red with a misleading "template card not visible".
//
// Shared column-list and ORDER BY consts are what make this class so easy to
// reintroduce -- a const is written against one query's FROM clause and then
// reused by a second query whose joins make it invalid.

// sharedSQLConsts lets the extractor resolve the identifiers that queries.go
// concatenates into its SQL. Anything not listed here renders as a
// placeholder, which is harmless: placeholders contain no alias references.
func sharedSQLConsts() map[string]string {
	return map[string]string{
		"templateSelectCols":      templateSelectCols,
		"blueprintSelectCols":     blueprintSelectCols,
		"TemplatePinOrderClause":  TemplatePinOrderClause,
		"BlueprintPinOrderClause": BlueprintPinOrderClause,
	}
}

// sqlLiterals returns every SQL statement built in queries.go, with shared
// consts expanded, so the checks below see what Postgres will actually get.
//
// It parses the Go source rather than scanning for backticks: a naive backtick
// split also captures the ordinary Go code sitting between two literals, which
// produces nonsense findings.
//
// Reading "queries.go" from the package's own directory is portable -- go test
// runs with the package dir as the working directory. (The path-fragility that
// previously broke this repo's CI was a "..\\..\\database\\queries.go" literal,
// whose backslashes are ordinary filename characters on Linux.)
func sqlLiterals(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "queries.go", nil, 0)
	if err != nil {
		t.Fatalf("parse queries.go: %v", err)
	}

	consts := sharedSQLConsts()

	// eval renders an expression as the SQL text it contributes. Non-constant
	// parts (variables such as the computed visibilityFilter) become a
	// placeholder rather than being dropped, so nothing is silently ignored.
	var eval func(ast.Expr) string
	eval = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return ""
			}
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return ""
			}
			return s
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return ""
			}
			return eval(v.X) + eval(v.Y)
		case *ast.Ident:
			if s, ok := consts[v.Name]; ok {
				return s
			}
			return " /*expr*/ "
		case *ast.ParenExpr:
			return eval(v.X)
		}
		return " /*expr*/ "
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// Every SQL statement reaches the driver through one of these.
		switch sel.Sel.Name {
		case "Query", "QueryRow", "Exec":
		default:
			return true
		}
		for _, arg := range call.Args {
			s := eval(arg)
			u := strings.ToUpper(s)
			if strings.Contains(u, "SELECT ") || strings.Contains(u, "UPDATE ") ||
				strings.Contains(u, "INSERT ") || strings.Contains(u, "DELETE ") {
				out = append(out, s)
			}
		}
		return true
	})

	if len(out) < 20 {
		t.Fatalf("extracted only %d SQL statements from queries.go; the extractor is broken, not the SQL", len(out))
	}
	return out
}

// aliasDecl finds table aliases introduced by a FROM/JOIN clause.
var aliasDecl = regexp.MustCompile(`(?is)\b(?:FROM|JOIN)\s+([a-z_]+)\s+(?:AS\s+)?([a-z][a-z0-9]{0,2})\b`)

// aliasUse finds qualified column references such as `b.id`.
var aliasUse = regexp.MustCompile(`\b([a-z][a-z0-9]{0,2})\.[a-z_]+`)

// sqlReserved are words aliasUse can match that are not table aliases.
var sqlReserved = map[string]bool{
	"is": true, "as": true, "on": true, "or": true, "and": true, "by": true,
	"in": true, "no": true, "to": true, "set": true, "not": true, "all": true,
}

// TestSQLAliasesAreDeclared guards defects 1 and 2: every alias a statement
// uses must be one that the same statement declares. An undeclared alias is
// either a "missing FROM-clause entry" error or, when the bare column also
// exists on a joined table, the nastier ambiguous-column error.
func TestSQLAliasesAreDeclared(t *testing.T) {
	for _, q := range sqlLiterals(t) {
		declared := map[string]bool{}
		for _, m := range aliasDecl.FindAllStringSubmatch(q, -1) {
			declared[strings.ToLower(m[2])] = true
		}
		reported := map[string]bool{}
		for _, m := range aliasUse.FindAllStringSubmatch(q, -1) {
			a := strings.ToLower(m[1])
			if sqlReserved[a] || declared[a] || reported[a] {
				continue
			}
			reported[a] = true
			t.Errorf("SQL uses undeclared table alias %q — Postgres rejects this at runtime:\n%s\n", a, strings.TrimSpace(q))
		}
	}
}

// TestSelectColConstsAreQualified is the direct guard for defect 1. A shared
// column-list const is reused across queries with different JOINs, so it is
// only ever safe if every column carries its table alias. It is easy to get
// wrong precisely because an unqualified list works fine until the first query
// that joins.
func TestSelectColConstsAreQualified(t *testing.T) {
	qualified := regexp.MustCompile(`^[a-z][a-z0-9]{0,2}\.`)
	for name, cols := range map[string]string{
		"blueprintSelectCols": blueprintSelectCols,
	} {
		for _, col := range strings.Split(cols, ",") {
			col = strings.TrimSpace(col)
			if col == "" || strings.Contains(col, "(") {
				continue
			}
			if !qualified.MatchString(col) {
				t.Errorf("%s: column %q is unqualified. It is used in queries that JOIN a table carrying the same column name, which fails with 'column reference is ambiguous'.", name, col)
			}
		}
	}
}

// TestNoDistinctWithPinOrdering guards defect 3. The pin clauses order by CASE
// expressions, and Postgres requires every ORDER BY expression in a DISTINCT
// query to appear in the select list, which a CASE expression cannot. Where
// de-duplication is needed, express access with EXISTS instead of a JOIN.
func TestNoDistinctWithPinOrdering(t *testing.T) {
	for _, q := range sqlLiterals(t) {
		u := strings.ToUpper(q)
		if !strings.Contains(u, "SELECT DISTINCT") {
			continue
		}
		if strings.Contains(u, "ORDER BY") && strings.Contains(u, "CASE WHEN") {
			t.Errorf("SELECT DISTINCT with a CASE-expression ORDER BY is rejected by Postgres; rewrite the join as EXISTS:\n%s\n", strings.TrimSpace(q))
		}
	}
}
