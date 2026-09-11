package templates

import (
	"strings"

	"github.com/jmal1/selfservice-api/internal/models"
)

// ValidateTemplateCredentialContract checks the template fields that directly
// determine whether Crucible can hand a student usable credentials at publish
// time. It is mode-driven:
//
//   - clone_no_customize / registered_existing_vm must carry non-empty
//     default_username and default_password on any OS because
//     resolvePodVMCredentials surfaces those template defaults verbatim.
//   - clone_with_customize ignores the static template defaults, except on
//     Linux where the guest image must still align its cloud-init default user
//     with ExpectedLinuxDefaultUser.
func ValidateTemplateCredentialContract(t *models.Template) []ContractViolation {
	if t == nil {
		return nil
	}
	if !reliesOnCloudInitCustomization(t.Kind) {
		return validateStaticTemplateCredentialContract(t)
	}
	return ValidateLinuxTemplateContract(t)
}

func validateStaticTemplateCredentialContract(t *models.Template) []ContractViolation {
	var violations []ContractViolation
	kind := staticCredentialModeLabel(t.Kind)

	if strings.TrimSpace(t.DefaultUsername) == "" {
		violations = append(violations, ContractViolation{
			Field:   "default_username",
			Problem: "default_username is empty. " + kind + " surfaces the template's baked-in username directly to the student, so publishing this template would produce a VM with no usable username even though the VM boots cleanly.",
			Fix:     "Set default_username to the account baked into the source image for this " + kind + " template, or switch the template to clone_with_customize so Crucible injects per-pod credentials instead of relying on static defaults.",
		})
	}
	if strings.TrimSpace(t.DefaultPassword) == "" {
		violations = append(violations, ContractViolation{
			Field:   "default_password",
			Problem: "default_password is empty. " + kind + " surfaces the template's baked-in password directly to the student, so publishing this template would produce a VM with no usable password even though the VM boots cleanly.",
			Fix:     "Set default_password to the password baked into the source image for this " + kind + " template, or switch the template to clone_with_customize so Crucible injects a generated per-pod password.",
		})
	}

	return violations
}

func staticCredentialModeLabel(kind string) string {
	switch kind {
	case models.TemplateKindCloneNoCustomize, models.TemplateKindRegisteredExistingVM:
		return kind
	default:
		return "non-customized"
	}
}
