package provisioner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// TestCloneFirstBootToolsTimeoutIsGenerous pins the deadline that broke on
// 2026-08-04.
//
// Provisioning a clone of `Ubuntu 24.04 Server` (tpl-ubuntu-aacc6a, vm-13702)
// failed with `timed out after 5m0s waiting for VMware Tools`, and the VM was
// found reporting guestToolsRunning shortly after. A too-short deadline turns a
// healthy template into a hard failure *and* prints a diagnosis that is simply
// wrong ("install open-vm-tools on the source VM").
//
// Waiting longer is free on the happy path: WaitForTools returns the moment
// tools report. So the deadline must stay generous.
func TestCloneFirstBootToolsTimeoutIsGenerous(t *testing.T) {
	const minimum = 15 * time.Minute
	if cloneFirstBootToolsTimeout < minimum {
		t.Errorf("cloneFirstBootToolsTimeout = %s, want >= %s.\n"+
			"A clone's first boot includes a guest-customization reboot on NFS-backed storage; "+
			"5 minutes was measured as too short on vm-13702.",
			cloneFirstBootToolsTimeout, minimum)
	}
	// Both are "ordinary first boot" waits and should not drift apart silently.
	if cloneFirstBootToolsTimeout != isoInstalledBootTimeout {
		t.Errorf("cloneFirstBootToolsTimeout (%s) != isoInstalledBootTimeout (%s); "+
			"these bound the same kind of wait — if they should now differ, say why here",
			cloneFirstBootToolsTimeout, isoInstalledBootTimeout)
	}
}

// TestWaitForToolsCallsUseNamedConstants is the structural half of the guard.
//
// Pinning the constant is not enough: the original bug was a *literal*
// `5*time.Minute` at the call site, which no assertion about a constant can
// catch. This walks template_jobs.go and fails if any WaitForTools call passes
// an inline duration expression rather than a named constant, so the only way
// to change a deadline is to change a documented constant.
func TestWaitForToolsCallsUseNamedConstants(t *testing.T) {
	const file = "template_jobs.go"

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var offenders []string
	var found int

	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WaitForTools" {
			return true
		}
		found++
		// WaitForTools(ctx, moref, timeout)
		if len(call.Args) != 3 {
			return true
		}
		switch arg := call.Args[2].(type) {
		case *ast.Ident:
			// A named constant or variable — acceptable.
			_ = arg
		default:
			offenders = append(offenders, exprText(call.Args[2]))
		}
		return true
	})

	if found == 0 {
		t.Fatalf("found no WaitForTools calls in %s — this guard is not scanning anything", file)
	}
	if len(offenders) != 0 {
		t.Errorf("WaitForTools called with an inline timeout in %s: %s\n\n"+
			"Use a named, documented constant (e.g. cloneFirstBootToolsTimeout) so the deadline "+
			"has a written rationale and one place to change. An inline 5*time.Minute here is the "+
			"exact regression that broke vm-13702.",
			file, strings.Join(offenders, ", "))
	}
}

// TestWaitForToolsGuardFlagsInlineDuration is the guard's own negative control.
func TestWaitForToolsGuardFlagsInlineDuration(t *testing.T) {
	src := `package provisioner
func f(vc any) {
	vc.WaitForTools(ctx, moref, 5*time.Minute)
	vc.WaitForTools(ctx, moref, namedTimeout)
}
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	var offenders int
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WaitForTools" || len(call.Args) != 3 {
			return true
		}
		if _, isIdent := call.Args[2].(*ast.Ident); !isIdent {
			offenders++
		}
		return true
	})

	if offenders != 1 {
		t.Fatalf("offenders = %d, want 1 (the inline 5*time.Minute)", offenders)
	}
}

func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BinaryExpr:
		return exprText(v.X) + v.Op.String() + exprText(v.Y)
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.Ident:
		return v.Name
	default:
		return "<expr>"
	}
}
