package provisioner

import (
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestResolveTemplateKind(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty defaults to clone_with_customize", "", models.TemplateKindCloneWithCustomize},
		{"clone_with_customize passthrough", models.TemplateKindCloneWithCustomize, models.TemplateKindCloneWithCustomize},
		{"clone_no_customize passthrough", models.TemplateKindCloneNoCustomize, models.TemplateKindCloneNoCustomize},
		{"registered_existing_vm passthrough", models.TemplateKindRegisteredExistingVM, models.TemplateKindRegisteredExistingVM},
		{"typo falls back to default", "clone-no-customize", models.TemplateKindCloneWithCustomize},
		{"capital letters fall back", "Clone_With_Customize", models.TemplateKindCloneWithCustomize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTemplateKind(tc.in); got != tc.want {
				t.Fatalf("resolveTemplateKind(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestShouldGenerateGuestPassword(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		osType string
		want   bool
	}{
		{"linux clone_with_customize", models.TemplateKindCloneWithCustomize, "linux", true},
		{"windows clone_with_customize", models.TemplateKindCloneWithCustomize, "windows", true},
		{"unknown os clone_with_customize", models.TemplateKindCloneWithCustomize, "other", false},
		{"linux clone_no_customize", models.TemplateKindCloneNoCustomize, "linux", false},
		{"windows registered_existing_vm", models.TemplateKindRegisteredExistingVM, "windows", false},
		{"empty kind treated as default", "", "linux", true},
		{"typo kind falls back to default", "bogus", "linux", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldGenerateGuestPassword(tc.kind, tc.osType); got != tc.want {
				t.Fatalf("shouldGenerateGuestPassword(%q,%q) = %v, want %v", tc.kind, tc.osType, got, tc.want)
			}
		})
	}
}

func TestResolvePodVMCredentials(t *testing.T) {
	tmplWithDefaults := &models.Template{
		DefaultUsername: "admin",
		DefaultPassword: "BakedIn1!",
	}
	tmplBlank := &models.Template{}

	tests := []struct {
		name        string
		kind        string
		osType      string
		generated   string
		tmpl        *models.Template
		wantUser    string
		wantPass    string
	}{
		{
			name:      "clone_with_customize linux uses generated",
			kind:      models.TemplateKindCloneWithCustomize,
			osType:    "linux",
			generated: "Gen3rated!",
			tmpl:      tmplWithDefaults,
			wantUser:  "student",
			wantPass:  "Gen3rated!",
		},
		{
			name:      "clone_with_customize windows capitalizes username",
			kind:      models.TemplateKindCloneWithCustomize,
			osType:    "windows",
			generated: "WinGen1@",
			tmpl:      nil,
			wantUser:  "Student",
			wantPass:  "WinGen1@",
		},
		{
			name:      "clone_no_customize uses template defaults",
			kind:      models.TemplateKindCloneNoCustomize,
			osType:    "linux",
			generated: "ignored",
			tmpl:      tmplWithDefaults,
			wantUser:  "admin",
			wantPass:  "BakedIn1!",
		},
		{
			name:      "registered_existing_vm uses template defaults",
			kind:      models.TemplateKindRegisteredExistingVM,
			osType:    "windows",
			generated: "ignored",
			tmpl:      tmplWithDefaults,
			wantUser:  "admin",
			wantPass:  "BakedIn1!",
		},
		{
			name:      "registered_existing_vm with nil template returns empty",
			kind:      models.TemplateKindRegisteredExistingVM,
			osType:    "linux",
			generated: "ignored",
			tmpl:      nil,
			wantUser:  "",
			wantPass:  "",
		},
		{
			name:      "no-customize with blank-defaults template returns empty (UI surfaces inline hint)",
			kind:      models.TemplateKindCloneNoCustomize,
			osType:    "linux",
			generated: "ignored",
			tmpl:      tmplBlank,
			wantUser:  "",
			wantPass:  "",
		},
		{
			name:      "unknown kind falls back to clone_with_customize policy",
			kind:      "bogus",
			osType:    "linux",
			generated: "Fall6ack!",
			tmpl:      nil,
			wantUser:  "student",
			wantPass:  "Fall6ack!",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, pass := resolvePodVMCredentials(tc.kind, tc.osType, tc.generated, tc.tmpl)
			if user != tc.wantUser {
				t.Errorf("user = %q, want %q", user, tc.wantUser)
			}
			if pass != tc.wantPass {
				t.Errorf("pass = %q, want %q", pass, tc.wantPass)
			}
		})
	}
}
