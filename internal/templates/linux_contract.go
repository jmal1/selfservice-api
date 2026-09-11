// This file adds the Linux guest-image contract validator (C0). A Linux
// template that violates the contract still clones and boots, but the
// student is silently unable to log in — the failure surfaces only when
// the student tries and can't. ValidateLinuxTemplateContract turns those
// silent violations into explicit, actionable findings so the wizard's
// publish gate (and a future synthetic check) can block or warn before a
// broken image ever reaches a student.
//
// The function is deliberately pure — no DB, no vCenter — so it can be
// called from the publish handler, a worker job, or a unit test with an
// in-memory *models.Template. The only knobs it reads are fields that
// actually exist on models.Template; it never guesses at guest-internal
// state (that belongs to the synthetic checks, which boot the image).
package templates

import (
	"strings"

	"github.com/jmal1/selfservice-api/internal/models"
)

// ExpectedLinuxDefaultUser is the cloud-init default user that Crucible's
// guest customization targets. internal/vcenter/client.go writes a bare
// top-level `password:` key into the #cloud-config guestinfo payload,
// which cloud-init applies to the *default user* only; and
// provisioner.resolvePodVMCredentials hands the student the username
// "student" for a customized Linux pod. The template's cloud-init default
// user must therefore be exactly this value, or the injected password
// lands on an account the student is never told about.
const ExpectedLinuxDefaultUser = "student"

// ContractViolation is a single, actionable finding from
// ValidateLinuxTemplateContract. Field names the offending template
// attribute (using the db/json column name so it matches the wizard form
// and the DB row), Problem explains the silent failure it causes, and Fix
// is a concrete remediation the instructor can apply.
type ContractViolation struct {
	Field   string
	Problem string
	Fix     string
}

// ValidateLinuxTemplateContract checks a Linux template against the guest
// contract Crucible relies on for per-pod password injection and SSH
// access. It returns one ContractViolation per problem found, or nil when
// the template satisfies the contract.
//
// It only inspects Linux templates: for a nil template, or any os_type
// other than "linux" (case/space-insensitive), it returns nil so a
// Windows or "other" template is never flagged with Linux-specific
// findings. This mirrors the OS gate the provisioner applies
// (strings.ToLower(osType) == "linux") in template_jobs.go / client.go.
//
// The checks it performs, all determinable from models.Template alone:
//
//   - default_username is empty or whitespace-only. This is the live
//     violation found on the "Ubuntu 24.04 Server" row. For a non-
//     customized kind the empty value is surfaced verbatim to the student
//     (resolvePodVMCredentials returns tmpl.DefaultUsername), leaving them
//     with no username; for a customized kind an empty value means the
//     row can't be reconciled against the cloud-init default user the
//     contract requires.
//
//   - default_username is set to something other than "student" on a
//     template that relies on cloud-init customization. client.go injects
//     a bare `password:` that cloud-init only applies to the default user,
//     and resolvePodVMCredentials tells the student to log in as
//     "student"; any other default user silently receives no password.
//
//   - default_password is empty on a non-customized kind. Those kinds
//     surface the template's static default_password to the student
//     (resolvePodVMCredentials returns tmpl.DefaultPassword); an empty
//     value leaves the student with a username but no way to authenticate.
//     Customized kinds are exempt because the password is generated and
//     injected per pod, so a blank static password is expected there.
func ValidateLinuxTemplateContract(t *models.Template) []ContractViolation {
	if t == nil {
		return nil
	}
	if strings.ToLower(strings.TrimSpace(t.OSType)) != "linux" {
		return nil
	}

	var violations []ContractViolation

	username := strings.TrimSpace(t.DefaultUsername)
	customized := reliesOnCloudInitCustomization(t.Kind)

	switch {
	case username == "":
		violations = append(violations, ContractViolation{
			Field:   "default_username",
			Problem: "default_username is empty. Crucible has no username to hand the student, so they cannot log in even though the pod boots cleanly — a silent failure the student discovers only when SSH/console login is refused.",
			Fix:     "Set default_username to \"" + ExpectedLinuxDefaultUser + "\" and make sure the guest's cloud-init default user matches (system_info.default_user.name: " + ExpectedLinuxDefaultUser + " in /etc/cloud/cloud.cfg.d/99-crucible.cfg).",
		})
	case customized && username != ExpectedLinuxDefaultUser:
		violations = append(violations, ContractViolation{
			Field:   "default_username",
			Problem: "default_username is \"" + t.DefaultUsername + "\", but this template is customized per pod. The #cloud-config guestinfo payload sets a bare top-level `password:`, which cloud-init applies only to the guest's default user, and the student is told to log in as \"" + ExpectedLinuxDefaultUser + "\". Any other default user silently receives no password, so login fails.",
			Fix:     "Change the cloud-init default user to \"" + ExpectedLinuxDefaultUser + "\" (system_info.default_user.name in /etc/cloud/cloud.cfg.d/99-crucible.cfg) and set default_username to \"" + ExpectedLinuxDefaultUser + "\", or switch the template to a non-customized kind with real static credentials.",
		})
	}

	if !customized && strings.TrimSpace(t.DefaultPassword) == "" {
		violations = append(violations, ContractViolation{
			Field:   "default_password",
			Problem: "default_password is empty on a non-customized template. Crucible surfaces the template's static credentials verbatim for this kind, so the student gets a username but no password and cannot authenticate.",
			Fix:     "Set default_password to the static password baked into the source image, or switch the template to clone_with_customize so Crucible injects a generated per-pod password.",
		})
	}

	return violations
}

// reliesOnCloudInitCustomization reports whether a template kind causes
// Crucible to run guest customization (cloud-init) and inject a generated
// per-pod password. It mirrors provisioner.resolveTemplateKind /
// shouldGenerateGuestPassword: only the two explicitly non-customized
// kinds return false; empty and unrecognized kinds canonicalize to
// clone_with_customize, so they rely on customization.
func reliesOnCloudInitCustomization(kind string) bool {
	switch kind {
	case models.TemplateKindCloneNoCustomize, models.TemplateKindRegisteredExistingVM:
		return false
	default:
		return true
	}
}
