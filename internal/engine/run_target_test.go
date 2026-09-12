package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

func podVMsQueryLiteral(t *testing.T, fnName string) string {
	t.Helper()
	lits := funcStringLiterals(t, "queries.go", fnName)
	for _, lit := range lits {
		lower := strings.ToLower(lit)
		if strings.Contains(lower, "from pod_vms") {
			return lower
		}
	}
	t.Fatalf("could not find the pod_vms query in %s — guard is vacuous", fnName)
	return ""
}

// TestGetRunTargetInfo_PinsExactPodVMID guards the twin-VM selection contract.
//
// Same-template twins used to share one playlist offer and the engine always
// graded "primary" via ORDER BY … LIMIT 1. Selecting VM B in the UI still
// graded VM A. The query must bind pv.id to the run's target_pod_vm_id ($2).
func TestGetRunTargetInfo_PinsExactPodVMID(t *testing.T) {
	query := podVMsQueryLiteral(t, "GetRunTargetInfo")
	if !strings.Contains(query, "pv.id = $2") {
		t.Error("GetRunTargetInfo no longer pins pv.id = $2.\n" +
			"  Twin VMs from the same template would again grade whichever row\n" +
			"  an ORDER BY / LIMIT 1 picked, ignoring the student's selection.")
	}
	if strings.Contains(query, "order by") || strings.Contains(query, "limit 1") {
		t.Error("GetRunTargetInfo still uses ORDER BY / LIMIT 1 primary selection.\n" +
			"  Exact target_pod_vm_id selection must not fall back to primary.")
	}
}

// TestGetRunTargetInfo_SkipsUnreachableVMs guards the empty-target.ip incident.
//
// Live failure (2026-09-12): a deleted rebuild residue could be selected and
// ship target.ip="", hanging the run as status=running for 10 minutes.
func TestGetRunTargetInfo_SkipsUnreachableVMs(t *testing.T) {
	query := podVMsQueryLiteral(t, "GetRunTargetInfo")

	if !strings.Contains(query, "deleted") {
		t.Error("GetRunTargetInfo no longer excludes status='deleted'.\n" +
			"  Deleted rebuild residue could be selected again, target.ip would\n" +
			"  be empty, and the runner would crash then hang for 10 minutes.")
	}
	if !strings.Contains(query, "ip_address") || !strings.Contains(query, "<> ''") {
		t.Error("GetRunTargetInfo no longer requires a non-empty ip_address.\n" +
			"  The runner rejects empty target.ip at startup; filtering here fails\n" +
			"  the run at claim time instead of shipping a doomed Job.")
	}
}

// TestGetRunTargetInfo_SelectsTargetIdentity guards the other half: the query
// must actually read the identity of the row it picked.
func TestGetRunTargetInfo_SelectsTargetIdentity(t *testing.T) {
	query := podVMsQueryLiteral(t, "GetRunTargetInfo")

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

// TestGetVMwareToolsTarget_PinsExactPodVMID keeps guest-ops on the same VM the
// student selected for kali_runner grading.
func TestGetVMwareToolsTarget_PinsExactPodVMID(t *testing.T) {
	query := podVMsQueryLiteral(t, "GetVMwareToolsTarget")
	if !strings.Contains(query, "pv.id = $2") {
		t.Error("GetVMwareToolsTarget no longer pins pv.id = $2.\n" +
			"  Mixed-mode playlists would guest-ops the oldest VM while kali\n" +
			"  graded the selected twin.")
	}
}

// TestExecuteRun_RequiresTargetPodVMID guards fail-closed create→engine contract.
func TestExecuteRun_RequiresTargetPodVMID(t *testing.T) {
	raw, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	src := string(raw)
	idx := strings.Index(src, "func (e *Engine) executeRun(")
	if idx < 0 {
		t.Fatal("executeRun not found")
	}
	body := src[idx:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "TargetPodVMID") || !strings.Contains(body, "has no target_pod_vm_id") {
		t.Error("executeRun no longer fail-closes when TargetPodVMID is nil.\n" +
			"  Legacy/synthetic runs without a selected VM would again pick an\n" +
			"  ambiguous primary target.")
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
