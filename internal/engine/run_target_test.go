package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// funcStringLiterals returns every string literal inside the named top-level
// function in the given file.
//
// It parses the AST with comments deliberately NOT included (parser mode 0) and
// looks only at *ast.BasicLit nodes. That matters here: the doc comments on
// GetRunTargetInfo and on this test explain the bug by quoting the old
// `ORDER BY created_at` clause verbatim. A guard built on strings.Contains over
// the raw file would fire on correct code, and a guard that fires on correct
// code gets deleted the first time it is inconvenient — taking the explanation
// of the bug with it.
func funcStringLiterals(t *testing.T, file, fnName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var lits []string
	var found bool
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != fnName {
			continue
		}
		found = true
		ast.Inspect(fn, func(n ast.Node) bool {
			if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				lits = append(lits, bl.Value)
			}
			return true
		})
	}
	if !found {
		t.Fatalf("could not find func %s in %s — was it renamed? Update this guard "+
			"rather than deleting it.", fnName, file)
	}
	return lits
}

// TestGetRunTargetInfo_OrdersDeterministically guards the target-selection fix.
//
// The production defect: target selection was `ORDER BY pv.created_at ASC LIMIT 1`
// with no tiebreaker. CreatePod inserts all of a pod's VMs in a single statement,
// so a multi-VM pod's rows carry an identical created_at to the microsecond, and
// boot_order defaults to 0 for every one of them. Postgres plans that as an
// unstable quicksort (verified on the live database: "Sort Method: quicksort",
// two rows, equal keys), so "the pod's primary VM" was whichever row the sort
// happened to emit first.
//
// Consequence if this regresses: the same pod, playlist and code can grade a
// DIFFERENT machine between runs. A student's web-server workflow silently runs
// against their database VM and reports a failure they cannot reproduce. It is
// invisible rather than loud, because the run itself succeeds.
//
// pv.id is a unique primary key, so requiring it in the ORDER BY makes the
// ordering total regardless of what the other keys do.
func TestGetRunTargetInfo_OrdersDeterministically(t *testing.T) {
	lits := funcStringLiterals(t, "queries.go", "GetRunTargetInfo")

	var orderBy string
	for _, lit := range lits {
		lower := strings.ToLower(lit)
		if strings.Contains(lower, "from pod_vms") && strings.Contains(lower, "order by") {
			orderBy = lit
			break
		}
	}
	// Assert the premise. If the query is ever restructured so that no literal
	// matches, this guard would otherwise pass while checking nothing at all.
	if orderBy == "" {
		t.Fatal("could not find the pod_vms target-selection query in GetRunTargetInfo. " +
			"This guard is now vacuous — fix it rather than leaving it green.")
	}

	lower := strings.ToLower(orderBy)
	idx := strings.Index(lower, "order by")
	clause := lower[idx:]
	if end := strings.Index(clause, "limit"); end != -1 {
		clause = clause[:end]
	}

	if !strings.Contains(clause, "pv.id") {
		t.Errorf("GetRunTargetInfo's ORDER BY has no unique-key tiebreaker.\n"+
			"  Got: %s\n"+
			"  A pod's VMs are inserted in one statement, so created_at is identical\n"+
			"  across them and boot_order defaults to 0 for all. Without pv.id the\n"+
			"  ordering is not total, Postgres sorts it with an unstable quicksort,\n"+
			"  and which VM gets graded is undefined between runs.",
			strings.TrimSpace(clause))
	}
}

// TestGetRunTargetInfo_SelectsTargetIdentity guards the other half: the query
// must actually read the identity of the row it picked.
//
// Before this change it selected only ip/os/username/password, so the engine
// never learned WHICH pod_vms row it had chosen. Nothing downstream could record
// it, which is why a completed assessment left no trace of the machine it
// graded — the only evidence was incidental, e.g. nmap printing the scanned IP
// into its own stdout.
func TestGetRunTargetInfo_SelectsTargetIdentity(t *testing.T) {
	lits := funcStringLiterals(t, "queries.go", "GetRunTargetInfo")

	var query string
	for _, lit := range lits {
		if strings.Contains(strings.ToLower(lit), "from pod_vms") {
			query = strings.ToLower(lit)
			break
		}
	}
	if query == "" {
		t.Fatal("could not find the pod_vms query in GetRunTargetInfo — guard is vacuous")
	}

	selectPart := query
	if end := strings.Index(selectPart, "from pod_vms"); end != -1 {
		selectPart = selectPart[:end]
	}
	for _, col := range []string{"pv.id", "pv.display_name"} {
		if !strings.Contains(selectPart, col) {
			t.Errorf("GetRunTargetInfo does not select %s.\n"+
				"  Got SELECT list: %s\n"+
				"  Without it the engine cannot name the VM it graded, and the run\n"+
				"  record has no target attribution at all.",
				col, strings.TrimSpace(selectPart))
		}
	}
}

// TestExecuteRun_RecordsTargetVM is a dead-wiring guard.
//
// This repo has now shipped the same class of defect at least seven times: a
// component that is correct, unit-tested and reviewed, but never actually called
// from the code path that runs in production (see internal/ci/wiring_test.go).
// SetRunTarget has exactly that shape — nothing else calls it, its own unit test
// would pass whether or not the engine invokes it, and the symptom of forgetting
// is silent: runs simply keep having no target recorded, which is
// indistinguishable from the bug this change fixes.
func TestExecuteRun_RecordsTargetVM(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}

	var callsGetTarget, callsSetTarget bool
	ast.Inspect(parsed, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "GetRunTargetInfo":
			callsGetTarget = true
		case "SetRunTarget":
			callsSetTarget = true
		}
		return true
	})

	// Premise check: if the engine no longer resolves a target at all, this guard
	// is meaningless and should fail loudly rather than pass.
	if !callsGetTarget {
		t.Fatal("engine.go never calls GetRunTargetInfo — this guard is vacuous. " +
			"If target resolution moved, move this guard with it.")
	}
	if !callsSetTarget {
		t.Error("engine.go resolves a target VM but never calls SetRunTarget.\n" +
			"  Every assessment run would then complete with target_pod_vm_id NULL\n" +
			"  and target_vm_name empty, so neither the UI nor an instructor could\n" +
			"  tell which VM a student was graded on. Nothing errors — the runs just\n" +
			"  silently carry no attribution, exactly as they did before this fix.")
	}
}
