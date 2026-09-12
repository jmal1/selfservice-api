package database_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type syntheticQuota struct {
	vcpus int
	ramMB int
	pods  int
}

var (
	bootstrapSyntheticQuota = regexp.MustCompile(`(?s)'student',\s*--[^\n]*\n\s*(\d+),\s*(\d+),\s*(\d+),\s*true`)
	migrationSyntheticQuota = regexp.MustCompile(`(?s)max_vcpus = GREATEST\(max_vcpus,\s*(\d+)\),\s*max_ram_mb = GREATEST\(max_ram_mb,\s*(\d+)\),\s*max_pods = GREATEST\(max_pods,\s*(\d+)\)`)
	migrationSyntheticUser  = regexp.MustCompile(`(?s)WHERE\s+oidc_sub = 'synthetic-monitor-no-oidc'\s+OR\s+username = 'synthetic'`)
)

func TestSyntheticMonitorQuotaSourcesPreserveNoopHeadroom(t *testing.T) {
	const (
		minVCPUs = 3
		minRAMMB = 3072
		minPods  = 3
	)

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")
	cases := []struct {
		name    string
		path    string
		pattern *regexp.Regexp
	}{
		{"bootstrap", filepath.Join(repoRoot, "deploy", "sql", "synthetic-user.sql"), bootstrapSyntheticQuota},
		{"migration", filepath.Join(filepath.Dir(file), "migrations", "000042_synthetic_monitor_quota_floor.up.sql"), migrationSyntheticQuota},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			quota := parseSyntheticQuota(t, tc.pattern, string(body))
			if quota.vcpus < minVCPUs || quota.ramMB < minRAMMB || quota.pods < minPods {
				t.Fatalf("synthetic quota = %+v, want at least %d vCPU / %d MiB / %d pods for synthetic-noop create/cleanup headroom", quota, minVCPUs, minRAMMB, minPods)
			}
		})
	}

	bootstrap, err := os.ReadFile(cases[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bootstrap), "ON CONFLICT DO UPDATE SET") {
		t.Fatal("synthetic bootstrap must repair quotas on re-run")
	}

	migration, err := os.ReadFile(cases[1].path)
	if err != nil {
		t.Fatal(err)
	}
	if !migrationSyntheticUser.Match(migration) {
		t.Fatal("quota repair migration must cover both synthetic identity forms")
	}
}

func parseSyntheticQuota(t *testing.T, pattern *regexp.Regexp, body string) syntheticQuota {
	t.Helper()
	match := pattern.FindStringSubmatch(body)
	if len(match) != 4 {
		t.Fatal("synthetic quota assignment not found")
	}
	values := make([]int, 3)
	for i := range values {
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			t.Fatal(err)
		}
		values[i] = value
	}
	return syntheticQuota{vcpus: values[0], ramMB: values[1], pods: values[2]}
}
