package templates

import (
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// hasViolation reports whether any violation targets the given field, and
// returns the first matching one so callers can assert on its contents.
func hasViolation(vs []ContractViolation, field string) (ContractViolation, bool) {
	for _, v := range vs {
		if v.Field == field {
			return v, true
		}
	}
	return ContractViolation{}, false
}

// TestValidateLinuxTemplateContract exercises the guest-image contract
// validator against the failure modes it exists to catch. Each case
// asserts on the *contents* of the returned violations (field + a non-
// empty, actionable Fix), not just the count, so a regression that
// silently changes which field is flagged is caught.
func TestValidateLinuxTemplateContract(t *testing.T) {
	tests := []struct {
		name string
		tmpl *models.Template
		// wantFields is the set of fields we expect a violation for.
		// Empty means "no violations at all".
		wantFields []string
		// wantProblemContains, keyed by field, asserts a substring is
		// present in that violation's Problem so the explanation stays
		// meaningful.
		wantProblemContains map[string]string
	}{
		{
			name: "empty default_username is the live violation",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": "empty",
			},
		},
		{
			name: "whitespace-only default_username is not silently accepted",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "   \t  ",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": "empty",
			},
		},
		{
			name: "student default_username satisfies the contract",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "student",
			},
			wantFields: nil,
		},
		{
			name: "non-student user on a customized template breaks password injection",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "ubuntu",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				// The explanation must call out the cloud-init default-user coupling.
				"default_username": "default user",
			},
		},
		{
			name: "empty kind canonicalizes to customized, so non-student user is flagged",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            "",
				DefaultUsername: "ubuntu",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": "default user",
			},
		},
		{
			name: "static non-student user is fine on a registered_existing_vm template",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindRegisteredExistingVM,
				DefaultUsername: "admin",
				DefaultPassword: "BakedIn1!",
			},
			wantFields: nil,
		},
		{
			name: "static non-student user is fine on a clone_no_customize template",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "admin",
				DefaultPassword: "BakedIn1!",
			},
			wantFields: nil,
		},
		{
			name: "non-customized template with a username but no password locks the student out",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindRegisteredExistingVM,
				DefaultUsername: "admin",
				DefaultPassword: "",
			},
			wantFields: []string{"default_password"},
			wantProblemContains: map[string]string{
				"default_password": "password",
			},
		},
		{
			name: "windows template with empty default_username fires no linux violations",
			tmpl: &models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
			wantFields: nil,
		},
		{
			name: "os_type is matched case- and space-insensitively",
			tmpl: &models.Template{
				OSType:          "  Linux ",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": "empty",
			},
		},
		{
			name:       "nil template returns no violations",
			tmpl:       nil,
			wantFields: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateLinuxTemplateContract(tc.tmpl)

			if len(got) != len(tc.wantFields) {
				t.Fatalf("got %d violations %+v; want %d for fields %v",
					len(got), got, len(tc.wantFields), tc.wantFields)
			}

			for _, field := range tc.wantFields {
				v, ok := hasViolation(got, field)
				if !ok {
					t.Fatalf("expected a violation for field %q; got %+v", field, got)
				}
				if strings.TrimSpace(v.Fix) == "" {
					t.Errorf("violation for %q has an empty Fix; every violation must be actionable", field)
				}
				if strings.TrimSpace(v.Problem) == "" {
					t.Errorf("violation for %q has an empty Problem", field)
				}
				if want, checkProblem := tc.wantProblemContains[field]; checkProblem {
					if !strings.Contains(strings.ToLower(v.Problem), strings.ToLower(want)) {
						t.Errorf("violation for %q Problem = %q; want it to contain %q",
							field, v.Problem, want)
					}
				}
			}
		})
	}
}

// TestValidateLinuxTemplateContract_StudentUserNeverFlagged is a focused
// regression guard for the happy path: a fully-formed customized Linux
// template whose default user is "student" must produce zero violations.
func TestValidateLinuxTemplateContract_StudentUserNeverFlagged(t *testing.T) {
	tmpl := &models.Template{
		OSType:          "linux",
		Kind:            models.TemplateKindCloneWithCustomize,
		DefaultUsername: ExpectedLinuxDefaultUser,
	}
	if got := ValidateLinuxTemplateContract(tmpl); len(got) != 0 {
		t.Fatalf("a contract-compliant template produced violations: %+v", got)
	}
}
