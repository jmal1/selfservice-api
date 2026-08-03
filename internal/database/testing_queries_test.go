package database

import (
	"strings"
	"testing"
)

func assertRunAttributionLeftJoins(t *testing.T, name, query string) {
	t.Helper()

	upper := strings.ToUpper(query)
	if strings.Contains(upper, "INNER JOIN") {
		t.Fatalf("%s uses INNER JOIN; expected LEFT JOINs for attribution", name)
	}

	for _, table := range []string{"USERS", "PODS", "PLAYLISTS"} {
		leftJoin := "LEFT JOIN " + table
		if !strings.Contains(upper, leftJoin) {
			t.Fatalf("%s missing %q", name, leftJoin)
		}
		for _, line := range strings.Split(upper, "\n") {
			if strings.Contains(line, "JOIN "+table) && !strings.Contains(line, leftJoin) {
				t.Fatalf("%s uses a bare JOIN for %s: %s", name, table, strings.TrimSpace(line))
			}
		}
	}
}

func TestRunAttributionQueriesUseLeftJoins(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "ListAllRuns", query: listAllRunsQuery},
		{name: "GetRunForAdmin", query: getRunForAdminQuery},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertRunAttributionLeftJoins(t, tc.name, tc.query)
		})
	}
}
