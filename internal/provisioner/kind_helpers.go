package provisioner

import "github.com/jmal1/selfservice-api/internal/models"

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

// resolvePodVMCredentials returns the (username, password) tuple that
// should be persisted on the pod_vms row after a successful clone.
//
//   - clone_with_customize: the generated password is authoritative; the
//     username is "student" on Linux and "Student" on Windows to match the
//     accounts cloud-init / cloudbase-init expects.
//   - clone_no_customize / registered_existing_vm: the template's static
//     default_username / default_password are surfaced to the student. If
//     the template has no defaults configured, both fields come back empty
//     and the UI shows a "credentials managed inside the VM" hint.
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
