package provisioner

import (
	"context"
	"errors"
	"testing"
	"time"

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

func TestStagingCloneCredentials(t *testing.T) {
	tests := []struct {
		name     string
		tmpl     *models.Template
		wantOS   string
		wantPass string
	}{
		{
			name: "clone_with_customize linux injects default_password",
			tmpl: &models.Template{
				Kind:            models.TemplateKindCloneWithCustomize,
				OSType:          "linux",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "linux",
			wantPass: "Changeme123!",
		},
		{
			name: "clone_with_customize windows injects default_password",
			tmpl: &models.Template{
				Kind:            models.TemplateKindCloneWithCustomize,
				OSType:          "windows",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "windows",
			wantPass: "Changeme123!",
		},
		{
			name: "empty kind defaults to customized and injects",
			tmpl: &models.Template{
				Kind:            "",
				OSType:          "linux",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "linux",
			wantPass: "Changeme123!",
		},
		{
			name: "clone_no_customize does not inject (source carries real creds)",
			tmpl: &models.Template{
				Kind:            models.TemplateKindCloneNoCustomize,
				OSType:          "linux",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "",
			wantPass: "",
		},
		{
			name: "registered_existing_vm does not inject",
			tmpl: &models.Template{
				Kind:            models.TemplateKindRegisteredExistingVM,
				OSType:          "windows",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "",
			wantPass: "",
		},
		{
			name: "unknown OS does not inject",
			tmpl: &models.Template{
				Kind:            models.TemplateKindCloneWithCustomize,
				OSType:          "freebsd",
				DefaultPassword: "Changeme123!",
			},
			wantOS:   "",
			wantPass: "",
		},
		{
			name:     "nil template is safe",
			tmpl:     nil,
			wantOS:   "",
			wantPass: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOS, gotPass := stagingCloneCredentials(tc.tmpl)
			if gotOS != tc.wantOS || gotPass != tc.wantPass {
				t.Fatalf("stagingCloneCredentials() = (%q,%q), want (%q,%q)",
					gotOS, gotPass, tc.wantOS, tc.wantPass)
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
		name      string
		kind      string
		osType    string
		generated string
		tmpl      *models.Template
		wantUser  string
		wantPass  string
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

func TestProvisionedPodVMCredentials_ReusesPersistedGeneratedPassword(t *testing.T) {
	for _, osType := range []string{"linux", "windows"} {
		t.Run(osType, func(t *testing.T) {
			podVM := &models.PodVM{
				GeneratedUsername: "stale-name",
				GeneratedPassword: "AlreadyInjected1!",
			}
			tmpl := &models.Template{
				Kind:            models.TemplateKindCloneWithCustomize,
				OSType:          osType,
				DefaultPassword: "Changeme123!",
			}
			generateCalls := 0
			user, password, err := provisionedPodVMCredentials(
				tmpl.Kind,
				tmpl.OSType,
				podVM,
				tmpl,
				func(int) string {
					generateCalls++
					return "WrongRetryPassword2!"
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			wantUser := "student"
			if osType == "windows" {
				wantUser = "Student"
			}
			if user != wantUser || password != podVM.GeneratedPassword {
				t.Fatalf("credentials = (%q, %q), want (%q, persisted password)", user, password, wantUser)
			}
			if generateCalls != 0 {
				t.Fatalf("retry generated %d replacement passwords", generateCalls)
			}
			if password == tmpl.DefaultPassword {
				t.Fatal("customized clone silently fell back to the template bootstrap password")
			}
		})
	}
}

func TestProvisionedPodVMCredentials_StaticKindsNeverGenerate(t *testing.T) {
	for _, kind := range []string{
		models.TemplateKindCloneNoCustomize,
		models.TemplateKindRegisteredExistingVM,
	} {
		t.Run(kind, func(t *testing.T) {
			tmpl := &models.Template{
				Kind:            kind,
				OSType:          "windows",
				DefaultUsername: "Administrator",
				DefaultPassword: "StaticOnly1!",
			}
			generateCalls := 0
			user, password, err := provisionedPodVMCredentials(
				kind,
				tmpl.OSType,
				&models.PodVM{},
				tmpl,
				func(int) string {
					generateCalls++
					return "MustNotGenerate2!"
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if user != tmpl.DefaultUsername || password != tmpl.DefaultPassword {
				t.Fatalf("credentials = (%q, %q), want static template credentials", user, password)
			}
			if generateCalls != 0 {
				t.Fatalf("static kind generated %d passwords", generateCalls)
			}
		})
	}
}

func TestProvisionedPodVMCredentials_StaticRetryIgnoresChangedTemplateDefaults(t *testing.T) {
	podVM := &models.PodVM{
		GeneratedUsername: "original-admin",
		GeneratedPassword: "OriginalStatic1!",
	}
	tmpl := &models.Template{
		Kind:            models.TemplateKindCloneNoCustomize,
		OSType:          "linux",
		DefaultUsername: "changed-admin",
		DefaultPassword: "ChangedStatic2!",
	}
	user, password, err := provisionedPodVMCredentials(
		tmpl.Kind,
		tmpl.OSType,
		podVM,
		tmpl,
		func(int) string { return "MustNotGenerate3!" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if user != podVM.GeneratedUsername || password != podVM.GeneratedPassword {
		t.Fatalf("static retry credentials = (%q, %q), want persisted pair", user, password)
	}
}

func TestProvisionedPodVMCredentials_CustomizedCloneNeverUsesBootstrapPassword(t *testing.T) {
	tmpl := &models.Template{
		Kind:            models.TemplateKindCloneWithCustomize,
		OSType:          "linux",
		DefaultPassword: "Changeme123!",
	}
	_, password, err := provisionedPodVMCredentials(
		tmpl.Kind,
		tmpl.OSType,
		&models.PodVM{},
		tmpl,
		func(int) string { return "PerPodGenerated1!" },
	)
	if err != nil {
		t.Fatal(err)
	}
	if password != "PerPodGenerated1!" || password == tmpl.DefaultPassword {
		t.Fatalf("customized password = %q, want fresh per-pod credential", password)
	}
}

type fakeGuestCredentialValidator struct {
	errs  []error
	calls int
	moref string
	user  string
	pass  string
}

func (f *fakeGuestCredentialValidator) ValidateGuestCredentials(
	_ context.Context,
	moref, user, pass string,
) error {
	f.calls++
	f.moref = moref
	f.user = user
	f.pass = pass
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func TestWaitForPodVMCredentialReady_RequiresGuestAuthentication(t *testing.T) {
	for _, tc := range []struct {
		osType string
		user   string
	}{
		{osType: "linux", user: "student"},
		{osType: "windows", user: "Student"},
	} {
		t.Run(tc.osType, func(t *testing.T) {
			validator := &fakeGuestCredentialValidator{
				errs: []error{errors.New("tools not ready"), nil},
			}
			err := waitForPodVMCredentialReady(
				context.Background(),
				validator,
				models.TemplateKindCloneWithCustomize,
				tc.osType,
				"vm-123",
				tc.user,
				"Generated1!",
				time.Second,
				time.Millisecond,
			)
			if err != nil {
				t.Fatal(err)
			}
			if validator.calls != 2 ||
				validator.moref != "vm-123" ||
				validator.user != tc.user ||
				validator.pass != "Generated1!" {
				t.Fatalf("validator calls=%d moref=%q user=%q pass=%q",
					validator.calls, validator.moref, validator.user, validator.pass)
			}
		})
	}
}

func TestWaitForPodVMCredentialReady_SabotagedConsumptionFails(t *testing.T) {
	sabotage := errors.New("InvalidGuestLogin")
	validator := &fakeGuestCredentialValidator{errs: []error{sabotage}}
	err := waitForPodVMCredentialReady(
		context.Background(),
		validator,
		models.TemplateKindCloneWithCustomize,
		"linux",
		"vm-123",
		"student",
		"Generated1!",
		0,
		0,
	)
	if !errors.Is(err, sabotage) {
		t.Fatalf("sabotaged guest consumption error = %v, want %v", err, sabotage)
	}
	if validator.calls != 1 {
		t.Fatalf("sabotaged validator calls = %d, want 1", validator.calls)
	}
}

func TestWaitForPodVMCredentialReady_StaticKindsBypassGeneratedCredentialGate(t *testing.T) {
	for _, kind := range []string{
		models.TemplateKindCloneNoCustomize,
		models.TemplateKindRegisteredExistingVM,
	} {
		t.Run(kind, func(t *testing.T) {
			validator := &fakeGuestCredentialValidator{
				errs: []error{errors.New("must not be called")},
			}
			if err := waitForPodVMCredentialReady(
				context.Background(),
				validator,
				kind,
				"windows",
				"vm-123",
				"Administrator",
				"StaticOnly1!",
				0,
				0,
			); err != nil {
				t.Fatal(err)
			}
			if validator.calls != 0 {
				t.Fatalf("static kind made %d generated-credential validation calls", validator.calls)
			}
		})
	}
}
