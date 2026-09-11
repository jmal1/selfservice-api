package provisioner

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

type parsedGoFile struct {
	name string
	file *ast.File
}

func TestPipelineMetricsMethodsHaveProductionCallSites(t *testing.T) {
	root := repoRoot(t)
	files, err := loadParsedGoFiles(root)
	if err != nil {
		t.Fatalf("load Go files: %v", err)
	}

	missing := missingPipelineMetricsMethods(files)
	if len(missing) != 0 {
		t.Fatalf("unwired PipelineMetrics methods: %s", strings.Join(missing, ", "))
	}
}

func TestPipelineMetricsGuardFlagsMissingMethod(t *testing.T) {
	files := []parsedGoFile{
		mustParseGoFile(t, "pipeline_metrics.go", `package provisioner
type PipelineMetrics struct{}
func (m *PipelineMetrics) RecordGhost() {}
`),
		mustParseGoFile(t, "use.go", `package provisioner
func use() {}
`),
	}

	missing := missingPipelineMetricsMethods(files)
	if len(missing) != 1 || missing[0] != "RecordGhost" {
		t.Fatalf("missing methods = %v, want [RecordGhost]", missing)
	}
}

func missingPipelineMetricsMethods(files []parsedGoFile) []string {
	methods := collectPipelineMetricsMethods(files)
	seen := collectPipelineMetricsCallSites(files)

	var missing []string
	for name := range methods {
		if _, ok := seen[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func collectPipelineMetricsMethods(files []parsedGoFile) map[string]struct{} {
	methods := make(map[string]struct{})
	for _, pf := range files {
		if strings.HasSuffix(pf.name, "_test.go") {
			continue
		}
		for _, decl := range pf.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !ast.IsExported(fn.Name.Name) {
				continue
			}
			if isPipelineMetricsReceiver(fn.Recv.List) {
				methods[fn.Name.Name] = struct{}{}
			}
		}
	}
	return methods
}

func collectPipelineMetricsCallSites(files []parsedGoFile) map[string]struct{} {
	seen := make(map[string]struct{})
	for _, pf := range files {
		if strings.HasSuffix(pf.name, "_test.go") {
			continue
		}
		ast.Inspect(pf.file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Push" && !isPipelineMetricsPushCall(pf.name, sel.X) {
				return true
			}
			seen[sel.Sel.Name] = struct{}{}
			return true
		})
	}
	return seen
}

func isPipelineMetricsPushCall(file string, recv ast.Expr) bool {
	// Push is the only ambiguous method name here — other reconcilers and
	// metrics helpers in the repo also have a Push method — so restrict the
	// count to the known PipelineMetrics call sites in provisioner code.
	if !strings.Contains(file, string(filepath.Separator)+"internal"+string(filepath.Separator)+"provisioner"+string(filepath.Separator)) {
		return false
	}
	id, ok := recv.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "m" || id.Name == "metrics"
}

func isPipelineMetricsReceiver(list []*ast.Field) bool {
	if len(list) == 0 {
		return false
	}
	switch recv := list[0].Type.(type) {
	case *ast.StarExpr:
		id, ok := recv.X.(*ast.Ident)
		return ok && id.Name == "PipelineMetrics"
	case *ast.Ident:
		return recv.Name == "PipelineMetrics"
	default:
		return false
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func loadParsedGoFiles(root string) ([]parsedGoFile, error) {
	var files []parsedGoFile
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files = append(files, parsedGoFile{name: path, file: file})
		return nil
	})
	return files, err
}

func mustParseGoFile(t *testing.T, name, src string) parsedGoFile {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return parsedGoFile{name: name, file: file}
}
