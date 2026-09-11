package vcenter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestEveryVMCreationPrimitiveUsesCanonicalPlacement(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	creationFunctions := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var primitives []string
			hasResolver := false
			hasHostField := false
			hasExplicitHostArgument := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && key.Name == "Host" {
						hasHostField = true
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					switch sel.Sel.Name {
					case "ResolvePlacement", "ResolveReplicaBuildTarget":
						hasResolver = true
					case "DefaultResourcePool":
						t.Errorf("%s calls DefaultResourcePool; unpinned fallback is prohibited", fn.Name.Name)
					case "Clone", "CreateVM", "ImportVApp":
						primitives = append(primitives, sel.Sel.Name)
						if (sel.Sel.Name == "CreateVM" || sel.Sel.Name == "ImportVApp") &&
							len(n.Args) >= 4 &&
							isPlacementHostSelector(n.Args[3]) {
							hasExplicitHostArgument = true
						}
					}
				}
				return true
			})
			if len(primitives) == 0 {
				continue
			}
			creationFunctions[fn.Name.Name] = struct{}{}
			if !hasResolver {
				t.Errorf("%s invokes %v without ResolvePlacement", fn.Name.Name, primitives)
			}
			if !hasHostField && !hasExplicitHostArgument {
				t.Errorf("%s invokes %v without an explicit placement host", fn.Name.Name, primitives)
			}
		}
	}

	got := make([]string, 0, len(creationFunctions))
	for name := range creationFunctions {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"ImportOVA",
		"StartReplicaBuildCanary",
		"StartReplicaBuildClone",
		"cloneForHealthCheckInner",
		"cloneTemplateSourceVMInner",
		"createBlankVMInner",
		"startCloneVMInner",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("VM creation primitive owners = %v, want %v; update the guard when adding a creation path", got, want)
	}
}

func TestEveryVMCloneBuilderAppliesVTPMPolicy(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	cloneBuilders := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var firstClone token.Pos
			var policyCall token.Pos
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch target := call.Fun.(type) {
				case *ast.SelectorExpr:
					if target.Sel.Name == "Clone" && firstClone == token.NoPos {
						firstClone = call.Pos()
					}
				case *ast.Ident:
					if target.Name == "applyVTPMClonePolicy" && policyCall == token.NoPos {
						policyCall = call.Pos()
					}
				}
				return true
			})
			if firstClone == token.NoPos {
				continue
			}
			cloneBuilders[fn.Name.Name] = struct{}{}
			if policyCall == token.NoPos {
				t.Errorf("%s submits CloneVM_Task without applyVTPMClonePolicy", fn.Name.Name)
			} else if policyCall > firstClone {
				t.Errorf("%s applies the vTPM policy after CloneVM_Task submission", fn.Name.Name)
			}
		}
	}

	got := make([]string, 0, len(cloneBuilders))
	for name := range cloneBuilders {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{
		"StartReplicaBuildCanary",
		"StartReplicaBuildClone",
		"cloneForHealthCheckInner",
		"cloneTemplateSourceVMInner",
		"startCloneVMInner",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("VM clone builders = %v, want %v; update the guard when adding a clone path", got, want)
	}
}

func isPlacementHostSelector(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Host"
}

func TestVCenterHostsIsOnlyHostScopeInput(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	hostInput := regexp.MustCompile(`VCENTER_[A-Z0-9_]*HOST[A-Z0-9_]*`)
	found := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "_bundle", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".go" && ext != ".yaml" && ext != ".yml" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range hostInput.FindAllString(string(body), -1) {
			found[match] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("vCenter host-scope inputs = %v, want only VCENTER_HOSTS", mapKeys(found))
	}
	if _, ok := found["VCENTER_HOSTS"]; !ok {
		t.Fatalf("vCenter host-scope inputs = %v, want VCENTER_HOSTS", mapKeys(found))
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
