package database

import (
	"fmt"
	"strings"
	"testing"
)

func validateTemplateRevalidationDedupSQL(lockSQL, activeSQL string) error {
	lock := strings.ToUpper(lockSQL)
	if !strings.Contains(lock, "FOR UPDATE") {
		return fmt.Errorf("template lock query must use FOR UPDATE")
	}

	active := strings.ToLower(activeSQL)
	for _, status := range []string{"'pending'", "'claimed'", "'in_progress'"} {
		if !strings.Contains(active, status) {
			return fmt.Errorf("active-job query must include status %s", status)
		}
	}
	if !strings.Contains(active, "payload->>'template_id'") {
		return fmt.Errorf("active-job query must scope dedup by payload template_id")
	}
	return nil
}

// These tests bind the guard to the exact constants executed by production.
// Concurrent transaction behavior still needs the repository's planned
// real-Postgres CI lane; this suite deliberately adds no SQL mocking dependency.
func TestTemplateRevalidationDedupSQLContract(t *testing.T) {
	if err := validateTemplateRevalidationDedupSQL(
		lockTemplateForRevalidationSQL,
		activeTemplateRevalidationJobSQL,
	); err != nil {
		t.Fatal(err)
	}
}

func TestTemplateRevalidationDedupSQLContractRejectsSabotage(t *testing.T) {
	tests := []struct {
		name      string
		lockSQL   string
		activeSQL string
	}{
		{
			name:      "missing FOR UPDATE",
			lockSQL:   strings.Replace(lockTemplateForRevalidationSQL, "FOR UPDATE", "", 1),
			activeSQL: activeTemplateRevalidationJobSQL,
		},
		{
			name:      "missing pending status",
			lockSQL:   lockTemplateForRevalidationSQL,
			activeSQL: strings.Replace(activeTemplateRevalidationJobSQL, "'pending', ", "", 1),
		},
		{
			name:      "missing claimed status",
			lockSQL:   lockTemplateForRevalidationSQL,
			activeSQL: strings.Replace(activeTemplateRevalidationJobSQL, "'claimed', ", "", 1),
		},
		{
			name:      "missing in_progress status",
			lockSQL:   lockTemplateForRevalidationSQL,
			activeSQL: strings.Replace(activeTemplateRevalidationJobSQL, ", 'in_progress'", "", 1),
		},
		{
			name:      "missing template_id predicate",
			lockSQL:   lockTemplateForRevalidationSQL,
			activeSQL: strings.Replace(activeTemplateRevalidationJobSQL, "AND payload->>'template_id' = $2", "", 1),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTemplateRevalidationDedupSQL(tc.lockSQL, tc.activeSQL); err == nil {
				t.Fatal("sabotaged SQL unexpectedly satisfied the production dedup contract")
			}
		})
	}
}

func TestCredentialRevalidationTemplateSelectorIncludesAllCustomizedTemplates(t *testing.T) {
	predicate := strings.ToLower(credentialRevalidationTemplateWhereSQL)
	if !strings.Contains(predicate, "is_active = true") {
		t.Fatal("credential revalidation selector must only include active templates")
	}
	if !strings.Contains(predicate, "kind = 'clone_with_customize'") {
		t.Fatal("credential revalidation selector must include clone_with_customize templates")
	}
	if strings.Contains(predicate, "trust_tier") {
		t.Fatal("credential revalidation selector must not depend on trust_tier; derived/untrusted sandbox Windows templates need credential smoke validation too")
	}
}

func TestCredentialRevalidationTemplateSelectorRejectsSabotage(t *testing.T) {
	tests := []struct {
		name      string
		predicate string
	}{
		{
			name:      "missing active filter",
			predicate: strings.Replace(credentialRevalidationTemplateWhereSQL, "is_active = true", "true", 1),
		},
		{
			name:      "missing clone_with_customize filter",
			predicate: strings.Replace(credentialRevalidationTemplateWhereSQL, "AND kind = 'clone_with_customize'", "", 1),
		},
		{
			name:      "trust tier filter reintroduced",
			predicate: credentialRevalidationTemplateWhereSQL + "\n\t\t  AND trust_tier = 'l1'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			predicate := strings.ToLower(tc.predicate)
			valid := strings.Contains(predicate, "is_active = true") &&
				strings.Contains(predicate, "kind = 'clone_with_customize'") &&
				!strings.Contains(predicate, "trust_tier")
			if valid {
				t.Fatal("sabotaged credential revalidation selector unexpectedly satisfied the contract")
			}
		})
	}
}
