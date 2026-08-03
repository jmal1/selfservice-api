package provisioner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestGeneralizeTemplate_IssuesTheShutdownItself is the companion guard to
// TestGeneralizeScript_LinuxMustNotPowerItselfOff.
//
// Those two tests are a pair, and neither is sufficient alone. Removing the
// shutdown from the guest script (which we must, so the guestinfo sentinel
// survives long enough to be read) moves the responsibility for powering the
// guest off onto the worker. If that half is ever dropped, the Linux script
// exits cleanly, the sentinel confirms, and then GeneralizeTemplate sits in
// waitForPowerOff until powerOffTimeout expires - reporting a timeout on a
// template whose cleanup ran perfectly, which is precisely the class of
// misleading failure this whole change exists to remove.
//
// This is the dead-wiring class that has now bitten this codebase repeatedly:
// a component that is correct, tested, and simply never called. Unit tests
// cannot see it because they exercise the piece, not the caller.
//
// Parsed as an AST rather than scanned as text on purpose. The doc comments in
// template_jobs.go quote `shutdown -h now` several times while explaining why
// the *script* must not contain it, so a strings.Contains guard would fire on
// correct code - and a guard that fires on correct code gets deleted, taking
// the documentation with it.
func TestGeneralizeTemplate_IssuesTheShutdownItself(t *testing.T) {
	const (
		file = "template_jobs.go"
		fn   = "GeneralizeTemplate"
		want = "shutdown -h now"
	)

	fset := token.NewFileSet()
	// Mode 0: do not parse comments, so prose about the bug cannot satisfy
	// (or trip) this guard.
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var decl *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if ok && fd.Name != nil && fd.Name.Name == fn {
			decl = fd
			return false
		}
		return true
	})
	if decl == nil {
		t.Fatalf("premise broken: %s no longer declares %s, so this guard is testing nothing", file, fn)
	}

	var literals int
	var found bool
	ast.Inspect(decl, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		literals++
		s, uErr := strconv.Unquote(lit.Value)
		if uErr != nil {
			return true
		}
		if strings.Contains(s, want) {
			found = true
		}
		return true
	})

	// Hollow-test guard: if we inspected nothing, "not found" would be
	// meaningless.
	if literals == 0 {
		t.Fatalf("premise broken: found no string literals inside %s; the guard scanned nothing", fn)
	}
	if !found {
		t.Errorf("%s contains no %q command.\n"+
			"The Linux generalize script deliberately no longer powers the guest off, because\n"+
			"vCenter clears guest-written guestinfo on power-off and that erased the completion\n"+
			"sentinel before the worker could read it. The worker must therefore issue the\n"+
			"shutdown itself after confirming the sentinel. Without it the guest stays powered\n"+
			"on and waitForPowerOff times out, failing a template whose cleanup succeeded.",
			fn, want)
	}
}
