package templates

import (
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestValidateTemplateCredentialContract_ModeDriven(t *testing.T) {
	tests := []struct {
		name                string
		tmpl                *models.Template
		wantFields          []string
		wantProblemContains map[string]string
		wantFixContains     map[string]string
	}{
		{
			name: "windows clone_no_customize with empty default_username is blocked",
			tmpl: &models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "",
				DefaultPassword: "BakedIn1!",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": models.TemplateKindCloneNoCustomize,
			},
			wantFixContains: map[string]string{
				"default_username": models.TemplateKindCloneWithCustomize,
			},
		},
		{
			name: "linux clone_no_customize with empty default_username is blocked",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "",
				DefaultPassword: "BakedIn1!",
			},
			wantFields: []string{"default_username"},
			wantProblemContains: map[string]string{
				"default_username": models.TemplateKindCloneNoCustomize,
			},
		},
		{
			name: "windows clone_no_customize with static credentials is accepted",
			tmpl: &models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "Administrator",
				DefaultPassword: "BakedIn1!",
			},
		},
		{
			name: "linux clone_no_customize with static credentials is accepted",
			tmpl: &models.Template{
				OSType:          "linux",
				Kind:            models.TemplateKindCloneNoCustomize,
				DefaultUsername: "student",
				DefaultPassword: "BakedIn1!",
			},
		},
		{
			name: "clone_with_customize with empty default_username stays accepted on windows",
			tmpl: &models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
		},
		{
			name: "manual windows template matching production shape stays accepted",
			tmpl: &models.Template{
				SourceType:      models.TemplateSourceManual,
				OSType:          "windows",
				Kind:            models.TemplateKindCloneWithCustomize,
				DefaultUsername: "",
			},
		},
		{
			name: "windows registered_existing_vm with empty default_password is blocked",
			tmpl: &models.Template{
				OSType:          "windows",
				Kind:            models.TemplateKindRegisteredExistingVM,
				DefaultUsername: "Administrator",
				DefaultPassword: "",
			},
			wantFields: []string{"default_password"},
			wantProblemContains: map[string]string{
				"default_password": models.TemplateKindRegisteredExistingVM,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateTemplateCredentialContract(tc.tmpl)

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
					t.Errorf("violation for %q has an empty Fix", field)
				}
				if strings.TrimSpace(v.Problem) == "" {
					t.Errorf("violation for %q has an empty Problem", field)
				}
				if want := tc.wantProblemContains[field]; want != "" && !strings.Contains(strings.ToLower(v.Problem), strings.ToLower(want)) {
					t.Errorf("violation for %q Problem = %q; want it to contain %q", field, v.Problem, want)
				}
				if want := tc.wantFixContains[field]; want != "" && !strings.Contains(strings.ToLower(v.Fix), strings.ToLower(want)) {
					t.Errorf("violation for %q Fix = %q; want it to contain %q", field, v.Fix, want)
				}
			}
		})
	}
}
