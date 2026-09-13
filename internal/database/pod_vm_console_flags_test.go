package database

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPodVMQueries_SelectConsoleHelperFlags fails if any template-joined
// pod_vms loader forgets assign_ip / skip_generalize — partial drift would
// leave the console helper unable to gate the network section.
func TestPodVMQueries_SelectConsoleHelperFlags(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "queries.go"))
	if err != nil {
		t.Fatalf("read queries.go: %v", err)
	}
	body := string(src)

	needles := []string{
		"COALESCE(t.assign_ip, true)",
		"COALESCE(t.skip_generalize, false)",
	}
	for _, n := range needles {
		count := strings.Count(body, n)
		// GetPodByID, listPodVMsActive, ListPodVMs, GetPodVM
		if count < 4 {
			t.Errorf("%q appears %d times in queries.go; want at least 4 (one per pod-VM loader)", n, count)
		}
	}
}
