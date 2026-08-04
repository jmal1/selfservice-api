package handlers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// This guard exists because the preflight framework shipped completely dead.
//
// `WithPreflightVCenter` was defined, documented, unit-tested and merged — and
// never called from any `cmd/` binary. Both consumers degrade *silently and
// permissively* when it is not called:
//
//	runPreflightGate     -> h.vcPreflight == nil -> returns blocked=false
//	AdminPreflightTemplate -> h.vcPreflight == nil -> 503
//
// So provisioning proceeded completely ungated while every unit test passed,
// because the tests construct a Handler with a fake vCenter already injected.
// That is the same failure mode as the `crucible_template_stuck` gauge: the
// unit under test is the piece we wrote, not the seam where it attaches.
//
// Any exported `With*` method on *Handler is, by naming convention, an opt-in
// wiring seam whose absence is silent. This test fails when one of them has no
// call site outside the handlers package and outside tests — i.e. when nothing
// in cmd/ actually turns it on.

func TestHandlerWiringMethodsAreCalledFromMain(t *testing.T) {
	root := handlerGuardRepoRoot(t)

	declared, err := exportedWithMethodsOnHandler(root)
	if err != nil {
		t.Fatalf("scan handler methods: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("found no With* methods on *Handler — the guard is not scanning anything")
	}

	called, err := withMethodCallSitesOutsideHandlers(root)
	if err != nil {
		t.Fatalf("scan call sites: %v", err)
	}

	// Stale allowlist entries are themselves a bug: they silence a name that no
	// longer exists, and would silence a *new* method that happens to reuse it.
	for name := range testOnlyWiringSeams {
		if _, ok := declared[name]; !ok {
			t.Errorf("testOnlyWiringSeams lists %q, which is no longer a With* method on *Handler — remove it", name)
		}
	}

	var unwired []string
	for name := range declared {
		if _, ok := called[name]; ok {
			continue
		}
		if _, exempt := testOnlyWiringSeams[name]; exempt {
			continue
		}
		unwired = append(unwired, name)
	}
	sort.Strings(unwired)

	if len(unwired) != 0 {
		t.Fatalf("handler wiring methods with no production call site outside internal/api/handlers: %s\n\n"+
			"These are opt-in seams that fail OPEN when never called — the feature is dead in "+
			"production while its unit tests pass. Wire them from the relevant cmd/ binary, or, if "+
			"the seam is genuinely test-only AND its absence fails CLOSED, add it to "+
			"testOnlyWiringSeams with a justification.", strings.Join(unwired, ", "))
	}
}

// testOnlyWiringSeams are With* methods that are deliberately never called from
// a cmd/ binary. Each entry must state why the seam is safe when unwired — the
// bar is that its absence fails **closed** (the real dependency is used), not
// open (the feature silently disappears).
//
// Keep this map as small as possible. Adding a name here is how the preflight
// bug would have been re-introduced, so an entry needs a real reason.
var testOnlyWiringSeams = map[string]string{
	"WithProvisionDB": "Injects a fake provisionDB so AdminProvisionTemplate can be driven without a " +
		"live pgxpool. provisionStore() falls back to h.db when nil, so an unwired seam uses the " +
		"real database — it fails closed.",
}

// TestHandlerWiringGuardDetectsAnUnwiredMethod is the guard's own negative
// control: it proves the detection logic reports a method that nothing calls,
// rather than silently finding nothing.
func TestHandlerWiringGuardDetectsAnUnwiredMethod(t *testing.T) {
	declared := map[string]struct{}{
		"WithRealThing":  {},
		"WithGhostThing": {},
	}
	called := map[string]struct{}{
		"WithRealThing": {},
	}

	var unwired []string
	for name := range declared {
		if _, ok := called[name]; !ok {
			unwired = append(unwired, name)
		}
	}
	sort.Strings(unwired)

	if len(unwired) != 1 || unwired[0] != "WithGhostThing" {
		t.Fatalf("unwired = %v, want [WithGhostThing]", unwired)
	}
}

func exportedWithMethodsOnHandler(root string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	dir := filepath.Join(root, "internal", "api", "handlers")

	err := walkNonTestGoFiles(dir, func(_ string, file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !ast.IsExported(fn.Name.Name) {
				continue
			}
			if !strings.HasPrefix(fn.Name.Name, "With") {
				continue
			}
			if isHandlerReceiver(fn.Recv.List) {
				out[fn.Name.Name] = struct{}{}
			}
		}
	})
	return out, err
}

func withMethodCallSitesOutsideHandlers(root string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	handlersDir := filepath.Join(root, "internal", "api", "handlers")

	err := walkNonTestGoFiles(root, func(path string, file *ast.File) {
		// A call from inside the handlers package proves nothing: the point is
		// that a cmd/ binary (or some other package) actually turns the feature on.
		if strings.HasPrefix(path, handlersDir+string(filepath.Separator)) {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if strings.HasPrefix(sel.Sel.Name, "With") {
				out[sel.Sel.Name] = struct{}{}
			}
			return true
		})
	})
	return out, err
}

func isHandlerReceiver(list []*ast.Field) bool {
	if len(list) == 0 {
		return false
	}
	switch recv := list[0].Type.(type) {
	case *ast.StarExpr:
		id, ok := recv.X.(*ast.Ident)
		return ok && id.Name == "Handler"
	case *ast.Ident:
		return recv.Name == "Handler"
	default:
		return false
	}
}

func walkNonTestGoFiles(root string, visit func(path string, file *ast.File)) error {
	fset := token.NewFileSet()
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		visit(path, file)
		return nil
	})
}

func handlerGuardRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
}
