package database

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestListOwnedTemplateFolderMorefs_SQLUsesRealReplicaBuildTable locks the
// table name against the live schema. Migration 000034 created
// template_source_replica_builds; a typo of template_replica_builds compiles
// and unit-tests green with fakes, then fails every leader tick in prod
// (SQLSTATE 42P01) — observed on Helm 207 after api#254.
func TestListOwnedTemplateFolderMorefs_SQLUsesRealReplicaBuildTable(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "template_orphan.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "FROM template_source_replica_builds") {
		t.Fatal("ListOwnedTemplateFolderMorefs must query template_source_replica_builds")
	}
	if strings.Contains(body, "FROM template_replica_builds") {
		t.Fatal("ListOwnedTemplateFolderMorefs must not query the non-existent template_replica_builds relation")
	}
}
