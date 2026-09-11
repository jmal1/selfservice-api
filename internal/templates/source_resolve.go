package templates

// source_resolve.go — the single, canonical rule for turning a template
// draft's (source_type, source_ref) pair into a live vCenter VM moref.
//
// Why this lives in one place
// ---------------------------
// The provision worker (internal/provisioner) and the preflight checks
// (internal/vcenter/preflight, driven from internal/api/handlers) BOTH need to
// resolve the clone source before they act. They MUST agree: if one treats
// source_ref as a raw moref while the other resolves a clone_template UUID,
// the two disagree and the wizard fails preflight on sources it can actually
// clone. That exact bug shipped once — the worker resolved clone_template
// correctly (parse the Crucible templates.id UUID, load the row, use its
// vcenter_vm_id / vcenter_template) while preflight fed the UUID straight to
// vCenter as a moref and every check cascaded to failure. Both callers now go
// through ResolveCloneSourceMoref so they cannot drift apart again.

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TemplateByIDGetter loads a template row by its Crucible ID. Satisfied by
// *database.Queries in production; fakes implement it in tests.
type TemplateByIDGetter interface {
	GetTemplateByID(ctx context.Context, id uuid.UUID) (*models.Template, error)
}

// VMNameResolver resolves a vCenter inventory name to a managed-object
// reference ("vm-NNN"). Satisfied by *vcenter.Client.
type VMNameResolver interface {
	ResolveVMByName(ctx context.Context, name string) (string, error)
}

// ResolveCloneSourceMoref maps a template draft's (sourceType, sourceRef) to a
// live vCenter VM moref, applying the rules the worker and preflight must share:
//
//   - clone_vcenter: sourceRef IS the moref; returned unchanged.
//   - ovf: sourceRef IS the moref of an already-imported OVA; returned
//     unchanged (same contract as clone_vcenter).
//   - clone_template: sourceRef is a Crucible templates.id UUID. The source
//     template row is loaded and resolved to a moref via its vcenter_vm_id
//     (preferred — set by wizard-published templates) or, failing that, its
//     vcenter_template name (the legacy/manually-registered path) via the
//     resolver.
//   - iso: there is no source VM to clone; returns ("", nil).
//
// A non-nil error means the source cannot be resolved and the caller must not
// proceed. The error text is phrased for an operator/instructor and is safe to
// surface in a preflight Result.
func ResolveCloneSourceMoref(
	ctx context.Context,
	getter TemplateByIDGetter,
	resolver VMNameResolver,
	sourceType, sourceRef string,
) (string, error) {
	switch sourceType {
	case models.TemplateSourceISO:
		return "", nil

	case models.TemplateSourceCloneVCenter, models.TemplateSourceOVF:
		if sourceRef == "" {
			return "", fmt.Errorf("source_ref is required for source_type=%s", sourceType)
		}
		return sourceRef, nil

	case models.TemplateSourceCloneTemplate:
		if sourceRef == "" {
			return "", fmt.Errorf("source_ref is required for source_type=%s", sourceType)
		}
		srcID, err := uuid.Parse(sourceRef)
		if err != nil {
			return "", fmt.Errorf("source_ref %q is not a valid Crucible template UUID for source_type=clone_template: %w", sourceRef, err)
		}
		if getter == nil {
			return "", fmt.Errorf("cannot resolve clone_template source %s: no template store configured", srcID)
		}
		src, err := getter.GetTemplateByID(ctx, srcID)
		if err != nil {
			return "", fmt.Errorf("load source template %s: %w", srcID, err)
		}
		if src == nil {
			return "", fmt.Errorf("source template %s not found", srcID)
		}
		switch {
		case src.VCenterVMID != "":
			return src.VCenterVMID, nil
		case src.VCenterTemplate != "":
			if resolver == nil {
				return "", fmt.Errorf("cannot resolve source template %q to a vCenter moref: no vCenter resolver configured", src.VCenterTemplate)
			}
			moref, rerr := resolver.ResolveVMByName(ctx, src.VCenterTemplate)
			if rerr != nil {
				return "", fmt.Errorf("resolve source template %q to a vCenter moref: %w", src.VCenterTemplate, rerr)
			}
			if moref == "" {
				return "", fmt.Errorf("source template %q resolved to an empty vCenter moref (the source VM may have been renamed or deleted)", src.VCenterTemplate)
			}
			return moref, nil
		default:
			return "", fmt.Errorf("source template %s has neither vcenter_vm_id nor vcenter_template set; cannot resolve to a vCenter VM", srcID)
		}

	default:
		return "", fmt.Errorf("unknown source_type %q (must be one of clone_template, clone_vcenter, iso, ovf)", sourceType)
	}
}
