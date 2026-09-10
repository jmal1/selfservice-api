package database

import (
	"os"
	"testing"
)

// requirePostgresDSN skips under -short (CI fail-fast / local ci-fast) and when
// TEST_DATABASE_URL is unset. Full suite runs without -short still exercise
// these integration tests when the URL is present.
func requirePostgresDSN(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("postgres integration skipped under -short; covered by full suite")
	}
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database")
	}
	return dsn
}
