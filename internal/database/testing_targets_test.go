package database

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func testingQueryFuncSource(t *testing.T, fnName string) string {
	t.Helper()
	raw, err := os.ReadFile("testing_queries.go")
	if err != nil {
		t.Fatalf("read testing_queries.go: %v", err)
	}
	src := string(raw)
	needle := "func (q *Queries) " + fnName + "("
	idx := strings.Index(src, needle)
	if idx < 0 {
		t.Fatalf("%s not found", fnName)
	}
	body := src[idx:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	return body
}

// TestGetTestingTargetsForPod_ListsPerVM ensures the dashboard query walks
// pod_vms rather than collapsing to a pod-level DISTINCT playlist list.
func TestGetTestingTargetsForPod_ListsPerVM(t *testing.T) {
	body := testingQueryFuncSource(t, "GetTestingTargetsForPod")
	if !strings.Contains(body, "FROM pod_vms") && !strings.Contains(strings.ToLower(body), "from pod_vms") {
		// SQL is in a string literal; check via AST literals.
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, "testing_queries.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name == nil || fn.Name.Name != "GetTestingTargetsForPod" {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				bl, ok := n.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					return true
				}
				if strings.Contains(strings.ToLower(bl.Value), "from pod_vms") {
					found = true
				}
				return true
			})
		}
		if !found {
			t.Fatal("GetTestingTargetsForPod does not query pod_vms")
		}
	}
	if !strings.Contains(body, "GetPlaylistsForPodVM") {
		t.Error("GetTestingTargetsForPod must attach playlists per VM via GetPlaylistsForPodVM")
	}
}

// TestCreateRun_PersistsTargetVM ensures create writes target columns so
// attribution exists before the engine runs.
func TestCreateRun_PersistsTargetVM(t *testing.T) {
	body := testingQueryFuncSource(t, "CreateRun")
	for _, col := range []string{"target_pod_vm_id", "target_vm_name", "target_vm_ip"} {
		if !strings.Contains(body, col) {
			t.Errorf("CreateRun INSERT no longer writes %s", col)
		}
	}
}
