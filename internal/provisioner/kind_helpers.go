package provisioner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
)

// resolveTemplateKind canonicalizes the kind value coming off a VMSpec.
// Empty string (pre-T3 payloads still in flight when the API gateway is
// upgraded) is treated as the legacy clone_with_customize behavior. Unknown
// values fall back to clone_with_customize too — the caller is expected to
// log a warning so the operator can fix the upstream payload, but the pod
// creation should not fail closed on a typo.
func resolveTemplateKind(kind string) string {
	switch kind {
	case models.TemplateKindCloneWithCustomize,
		models.TemplateKindCloneNoCustomize,
		models.TemplateKindRegisteredExistingVM:
		return kind
	default:
		return models.TemplateKindCloneWithCustomize
	}
}

// shouldGenerateGuestPassword reports whether the provisioner should
// generate a fresh password and inject it via guestinfo during the clone.
// Only clone_with_customize on a known OS gets a generated password; the
// other kinds preserve whatever credentials are baked into the source VM.
func shouldGenerateGuestPassword(kind, osType string) bool {
	if resolveTemplateKind(kind) != models.TemplateKindCloneWithCustomize {
		return false
	}
	return osType == "linux" || osType == "windows"
}

// stagingCloneCredentials returns the (osType, password) that the template
// wizard's staging clone should inject as first-boot guest customization.
//
// Only clone_with_customize sources on a known OS get an injected password:
// their cloud-init / cloudbase-init `student` account has NO baked-in password
// (it is set per-clone via guestinfo), so without injection the staging VM
// boots with no usable login and the instructor cannot log in with the
// credentials the wizard surfaces. Non-customized sources carry real
// credentials in the image already, so both return values are empty and the
// clone copies the source verbatim.
//
// Pure so it can be unit-tested without a database or vCenter.
func stagingCloneCredentials(tmpl *models.Template) (osType, password string) {
	if tmpl == nil {
		return "", ""
	}
	if !shouldGenerateGuestPassword(tmpl.Kind, tmpl.OSType) {
		return "", ""
	}
	return tmpl.OSType, tmpl.DefaultPassword
}

// should be persisted on the pod_vms row after a successful clone.
//
//   - clone_with_customize: the generated password is authoritative; the
//     username is "student" on Linux and "Student" on Windows to match the
//     accounts cloud-init / cloudbase-init expects.
//   - clone_no_customize / registered_existing_vm: the template's static
//     default_username / default_password are surfaced to the student. The
//     publish gate and provisioning path both require a complete pair.
//
// The function is pure so it can be unit-tested without a database or
// vCenter; the caller threads the template through after looking it up.
func resolvePodVMCredentials(kind, osType, generatedPassword string, tmpl *models.Template) (string, string) {
	switch resolveTemplateKind(kind) {
	case models.TemplateKindCloneWithCustomize:
		user := "student"
		if osType == "windows" {
			user = "Student"
		}
		return user, generatedPassword
	default:
		if tmpl == nil {
			return "", ""
		}
		return tmpl.DefaultUsername, tmpl.DefaultPassword
	}
}

// provisionedPodVMCredentials returns the credential pair that must be both
// injected into and displayed for a pod VM. A previously persisted generated
// password always wins so a recovered job cannot display a newly generated
// password for an already-configured clone.
func provisionedPodVMCredentials(
	kind, osType string,
	podVM *models.PodVM,
	tmpl *models.Template,
	generate func(int) string,
) (string, string, error) {
	if podVM == nil {
		return "", "", fmt.Errorf("pod VM is required to resolve credentials")
	}

	if resolveTemplateKind(kind) == models.TemplateKindCloneWithCustomize {
		if !shouldGenerateGuestPassword(kind, osType) {
			return "", "", fmt.Errorf(
				"%s requires guest credential customization, but OS %q is unsupported",
				models.TemplateKindCloneWithCustomize,
				osType,
			)
		}
		user, _ := resolvePodVMCredentials(kind, osType, "", tmpl)
		password := podVM.GeneratedPassword
		if password == "" {
			password = generate(12)
		}
		if password == "" {
			return "", "", fmt.Errorf("generated guest password is empty")
		}
		return user, password, nil
	}

	if podVM.GeneratedUsername != "" || podVM.GeneratedPassword != "" {
		if strings.TrimSpace(podVM.GeneratedUsername) == "" ||
			strings.TrimSpace(podVM.GeneratedPassword) == "" {
			return "", "", fmt.Errorf(
				"%s has incomplete persisted static credentials",
				resolveTemplateKind(kind),
			)
		}
		return podVM.GeneratedUsername, podVM.GeneratedPassword, nil
	}
	user, password := resolvePodVMCredentials(kind, osType, "", tmpl)
	if strings.TrimSpace(user) == "" || strings.TrimSpace(password) == "" {
		return "", "", fmt.Errorf(
			"%s requires non-empty static template credentials",
			resolveTemplateKind(kind),
		)
	}
	return user, password, nil
}

type guestCredentialValidator interface {
	ValidateGuestCredentials(ctx context.Context, moref, guestUser, guestPassword string) error
}

const (
	podGuestCredentialReadyTimeout  = 6 * time.Minute
	podGuestCredentialRetryInterval = 15 * time.Second
)

// waitForPodVMCredentialReady is the production readiness gate for customized
// pod clones. Static-credential kinds deliberately bypass guest customization
// and this generated-credential check.
func waitForPodVMCredentialReady(
	ctx context.Context,
	validator guestCredentialValidator,
	kind, osType, moref, username, password string,
	timeout, interval time.Duration,
) error {
	if resolveTemplateKind(kind) != models.TemplateKindCloneWithCustomize {
		return nil
	}
	if !shouldGenerateGuestPassword(kind, osType) {
		return fmt.Errorf(
			"%s cannot verify generated credentials for unsupported OS %q",
			models.TemplateKindCloneWithCustomize,
			osType,
		)
	}
	if moref == "" || username == "" || password == "" {
		return fmt.Errorf("customized VM credential readiness requires a VM identity, username, and password")
	}
	if err := pollGuestCredentials(ctx, func(attemptCtx context.Context) error {
		return validator.ValidateGuestCredentials(attemptCtx, moref, username, password)
	}, timeout, interval); err != nil {
		return fmt.Errorf(
			"guest customization did not install the generated credential for %q: %w",
			username,
			err,
		)
	}
	return nil
}
