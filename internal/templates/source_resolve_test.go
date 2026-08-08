package templates

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// fakeGetter is a minimal TemplateByIDGetter. It returns the mapped template,
// or (nil, nil) for an unknown id, or a wired error.
type fakeGetter struct {
	byID map[uuid.UUID]*models.Template
	err  error
}

func (g *fakeGetter) GetTemplateByID(_ context.Context, id uuid.UUID) (*models.Template, error) {
	if g.err != nil {
		return nil, g.err
	}
	return g.byID[id], nil
}

// fakeResolver is a minimal VMNameResolver mapping inventory name -> moref.
type fakeResolver struct {
	byName map[string]string
	err    error
}

func (r *fakeResolver) ResolveVMByName(_ context.Context, name string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.byName[name], nil
}

func TestResolveCloneSourceMoref_ISO(t *testing.T) {
	moref, err := ResolveCloneSourceMoref(context.Background(), nil, nil, models.TemplateSourceISO, "[ds] w/win.iso")
	if err != nil {
		t.Fatalf("iso should not error: %v", err)
	}
	if moref != "" {
		t.Fatalf("iso should resolve to empty moref, got %q", moref)
	}
}

func TestResolveCloneSourceMoref_CloneVCenter_Passthrough(t *testing.T) {
	moref, err := ResolveCloneSourceMoref(context.Background(), nil, nil, models.TemplateSourceCloneVCenter, "vm-1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moref != "vm-1234" {
		t.Fatalf("clone_vcenter moref = %q; want vm-1234", moref)
	}
}

func TestResolveCloneSourceMoref_CloneVCenter_EmptyRef(t *testing.T) {
	_, err := ResolveCloneSourceMoref(context.Background(), nil, nil, models.TemplateSourceCloneVCenter, "")
	if err == nil {
		t.Fatal("expected error for empty source_ref")
	}
}

// The core bug: clone_template's source_ref is a Crucible templates.id UUID,
// not a moref. It must be resolved via the source template row's vcenter_vm_id.
func TestResolveCloneSourceMoref_CloneTemplate_ViaVMID(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id, VCenterVMID: "vm-987"},
	}}
	moref, err := ResolveCloneSourceMoref(context.Background(), g, nil, models.TemplateSourceCloneTemplate, id.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moref != "vm-987" {
		t.Fatalf("moref = %q; want vm-987", moref)
	}
}

// Fallback path: no vcenter_vm_id, resolve via vcenter_template name.
func TestResolveCloneSourceMoref_CloneTemplate_ViaName(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id, VCenterTemplate: "Win11-Golden"},
	}}
	r := &fakeResolver{byName: map[string]string{"Win11-Golden": "vm-555"}}
	moref, err := ResolveCloneSourceMoref(context.Background(), g, r, models.TemplateSourceCloneTemplate, id.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moref != "vm-555" {
		t.Fatalf("moref = %q; want vm-555", moref)
	}
}

// vcenter_vm_id must win over vcenter_template when both are set, matching
// models.Template.VCenterRef() precedence.
func TestResolveCloneSourceMoref_CloneTemplate_VMIDPreferred(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id, VCenterVMID: "vm-1", VCenterTemplate: "Golden"},
	}}
	r := &fakeResolver{byName: map[string]string{"Golden": "vm-2"}}
	moref, err := ResolveCloneSourceMoref(context.Background(), g, r, models.TemplateSourceCloneTemplate, id.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if moref != "vm-1" {
		t.Fatalf("moref = %q; want vm-1 (vcenter_vm_id preferred)", moref)
	}
}

func TestResolveCloneSourceMoref_CloneTemplate_BadUUID(t *testing.T) {
	_, err := ResolveCloneSourceMoref(context.Background(), &fakeGetter{}, nil, models.TemplateSourceCloneTemplate, "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for non-UUID source_ref")
	}
}

func TestResolveCloneSourceMoref_CloneTemplate_NotFound(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{}} // empty -> nil
	_, err := ResolveCloneSourceMoref(context.Background(), g, nil, models.TemplateSourceCloneTemplate, id.String())
	if err == nil {
		t.Fatal("expected error when source template row is missing")
	}
}

func TestResolveCloneSourceMoref_CloneTemplate_NoLinkage(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id}, // neither VCenterVMID nor VCenterTemplate
	}}
	_, err := ResolveCloneSourceMoref(context.Background(), g, nil, models.TemplateSourceCloneTemplate, id.String())
	if err == nil {
		t.Fatal("expected error when source template has no vcenter linkage")
	}
}

// Name resolution that returns an empty moref (source VM renamed/deleted) must
// be an error, not a silent empty pass-through.
func TestResolveCloneSourceMoref_CloneTemplate_NameResolvesEmpty(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id, VCenterTemplate: "Ghost"},
	}}
	r := &fakeResolver{byName: map[string]string{}} // "Ghost" -> ""
	_, err := ResolveCloneSourceMoref(context.Background(), g, r, models.TemplateSourceCloneTemplate, id.String())
	if err == nil {
		t.Fatal("expected error when name resolves to empty moref")
	}
}

func TestResolveCloneSourceMoref_CloneTemplate_ResolverError(t *testing.T) {
	id := uuid.New()
	g := &fakeGetter{byID: map[uuid.UUID]*models.Template{
		id: {ID: id, VCenterTemplate: "Win11-Golden"},
	}}
	r := &fakeResolver{err: errors.New("vcenter down")}
	_, err := ResolveCloneSourceMoref(context.Background(), g, r, models.TemplateSourceCloneTemplate, id.String())
	if err == nil {
		t.Fatal("expected error when the resolver fails")
	}
}

func TestResolveCloneSourceMoref_CloneTemplate_NilGetter(t *testing.T) {
	id := uuid.New()
	_, err := ResolveCloneSourceMoref(context.Background(), nil, nil, models.TemplateSourceCloneTemplate, id.String())
	if err == nil {
		t.Fatal("expected error when no template store is configured")
	}
}

func TestResolveCloneSourceMoref_UnknownType(t *testing.T) {
	_, err := ResolveCloneSourceMoref(context.Background(), nil, nil, "bogus", "x")
	if err == nil {
		t.Fatal("expected error for unknown source_type")
	}
}
