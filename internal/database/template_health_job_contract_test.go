package database

import (
	"strings"
	"testing"
)

// TestTemplateHealthConfirmationEnqueueSQLIsRaceSafe pins the exact SQL used
// by production. Sabotage: remove ON CONFLICT or the failed-job repair clause
// and this fails while the higher-level fake scheduler test still passes.
func TestTemplateHealthConfirmationEnqueueSQLIsRaceSafe(t *testing.T) {
	required := []string{
		"next_attempt_at",
		"ON CONFLICT (id) DO UPDATE",
		"WHERE jobs.status = 'failed'",
		"retry_count = 0",
	}
	for _, fragment := range required {
		if !strings.Contains(createTemplateHealthConfirmationJobSQL, fragment) {
			t.Errorf("production confirmation enqueue SQL is missing %q", fragment)
		}
	}
}
