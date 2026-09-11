package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestTimeoutWatchdog_CapturesDiagnosticsBeforeCleanup guards an ordering
// invariant that nothing else can see.
//
// CleanupRunner deletes the runner Job, and deleting a Job deletes its pods.
// The pods are the only record of why a run never reported. If the
// DescribeRunner call is ever moved below the CleanupRunner call — an entirely
// natural-looking edit, since "clean up, then report" reads fine — the capture
// silently degrades to "job not found" and every timeout goes back to being
// indistinguishable from every other timeout. Nothing fails, nothing logs, and
// the loss is only discovered the next time someone tries to diagnose a
// timeout, weeks later.
//
// This is an AST walk rather than a text scan so that a comment mentioning the
// two functions (this file's own doc comments, for instance) cannot trip it.
// A guard that fires on correct code gets deleted the first time it is
// inconvenient.
func TestTimeoutWatchdog_CapturesDiagnosticsBeforeCleanup(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}

	fn := findFuncDecl(file, "checkForTimeouts")
	if fn == nil {
		t.Fatal("checkForTimeouts not found in engine.go — if it was renamed, " +
			"move this guard with it rather than deleting it")
	}

	describePos, cleanupPos := token.NoPos, token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "DescribeRunner":
			if !describePos.IsValid() {
				describePos = call.Pos()
			}
		case "CleanupRunner":
			if !cleanupPos.IsValid() {
				cleanupPos = call.Pos()
			}
		}
		return true
	})

	if !describePos.IsValid() {
		t.Fatal("checkForTimeouts no longer calls DescribeRunner: every timed-out run " +
			"will record the same generic message, and the pods carrying the reason " +
			"are deleted moments later by CleanupRunner")
	}
	if !cleanupPos.IsValid() {
		t.Fatal("checkForTimeouts no longer calls CleanupRunner: runner Jobs and their " +
			"Secrets will leak, and a leaked Secret holds a live callback token")
	}
	if describePos > cleanupPos {
		t.Fatalf("DescribeRunner (line %d) runs AFTER CleanupRunner (line %d). "+
			"CleanupRunner deletes the Job and therefore the pods, so the capture "+
			"will find nothing and every timeout becomes un-diagnosable again.",
			fset.Position(describePos).Line, fset.Position(cleanupPos).Line)
	}
}

// TestTimeoutWatchdog_ClosesOrphanedWorkflowResults guards the second half of
// the fix. A run that never reported leaves its workflow_results in 'pending'
// forever: the run row says timeout while its children claim to still be
// queued, so the testing UI renders a permanently in-progress assessment and
// UpdateRunCounts (which counts only fail/error/timeout as failures) reports
// zero failed workflows for a run that completed nothing.
func TestTimeoutWatchdog_ClosesOrphanedWorkflowResults(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatalf("parse engine.go: %v", err)
	}

	fn := findFuncDecl(file, "checkForTimeouts")
	if fn == nil {
		t.Fatal("checkForTimeouts not found in engine.go")
	}

	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
				sel.Sel.Name == "TimeoutPendingWorkflowResults" {
				found = true
			}
		}
		return true
	})

	if !found {
		t.Fatal("checkForTimeouts no longer calls TimeoutPendingWorkflowResults: a " +
			"terminal run will keep non-terminal workflow results, which the testing " +
			"UI renders as an assessment that is still running and which make " +
			"failed_workflows read 0 for a run that produced nothing")
	}
}

func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}
