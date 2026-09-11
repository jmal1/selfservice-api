package preflight_test

import (
	"context"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter/preflight"
)

// These tests lock in the regression fix: a clone_template draft whose source
// could not be resolved to a live moref (SourceResolveError set) must make
// PF-01 block with the resolver's reason, and the moref-dependent checks must
// cascade to "Fix PF-01 first" rather than showing a misleading green
// "skipped". The original bug fed the Crucible templates.id UUID straight to
// vCenter, so every check failed with a raw ServerFaultCode.

func TestPF01_Fail_SourceResolveError(t *testing.T) {
	f := newFake()
	p := preflight.Params{
		SourceType:         models.TemplateSourceCloneTemplate,
		SourceResolveError: "source template 41e00994 has neither vcenter_vm_id nor vcenter_template set",
	}
	r := preflight.RunAll(context.Background(), f, p)

	pf01 := findResult(t, r, "PF-01")
	if pf01.OK {
		t.Fatal("PF-01 must fail when source resolution failed")
	}
	if pf01.Severity != "block" {
		t.Errorf("PF-01 severity = %q; want block", pf01.Severity)
	}
	if !containsAny(pf01.Detail, "neither vcenter_vm_id nor vcenter_template") {
		t.Errorf("PF-01 Detail should surface the resolver reason; got %q", pf01.Detail)
	}
}

// When resolution fails, SourceMoref is empty. The moref-dependent block checks
// must NOT report a green skip for a clone source — they must fail so the
// instructor is pointed at PF-01. This is the anti-"silent green" guard.
func TestClonePreflight_MorefDependentChecksCascade_OnResolveFailure(t *testing.T) {
	f := newFake()
	p := preflight.Params{
		SourceType:         models.TemplateSourceCloneTemplate,
		DatastoreName:      "NAS-vmstore",
		StagingPortGroup:   "PG-VM-Lab",
		SourceResolveError: "load source template: not found",
	}
	r := preflight.RunAll(context.Background(), f, p)

	// Every block-severity, moref-dependent check must be failing (not a green
	// skip) for an unresolved clone source.
	for _, id := range []string{"PF-02", "PF-03", "PF-04", "PF-05", "PF-06", "PF-07"} {
		res := findResult(t, r, id)
		if res.OK {
			t.Errorf("%s should fail (cascade) when clone source is unresolved; got green Detail=%q", id, res.Detail)
		}
	}
	if !preflight.AnyBlockFailed(r) {
		t.Fatal("AnyBlockFailed must be true when the clone source cannot be resolved")
	}
}

// Sabotage control: with a resolvable clone source (moref present, live VM in
// the fake), PF-01 passes — proving the failure above is caused by the
// unresolved source and not by something unconditional.
func TestPF01_Pass_CloneTemplate_Resolved(t *testing.T) {
	f := newFake()
	f.vmProps["vm-42"] = vmWithHost("host-1")
	p := preflight.Params{
		SourceType:  models.TemplateSourceCloneTemplate,
		SourceMoref: "vm-42", // handler resolved the UUID to this moref
	}
	r := preflight.RunAll(context.Background(), f, p)
	pf01 := findResult(t, r, "PF-01")
	if !pf01.OK {
		t.Fatalf("PF-01 should pass for a resolved clone_template source; got Detail=%q", pf01.Detail)
	}
}

// ISO sources legitimately have no source moref: the moref-dependent checks
// skip green, and PF-01 passes (there is nothing to resolve). This guards
// against the cascade change accidentally breaking the ISO path.
func TestISO_MorefChecksSkipGreen(t *testing.T) {
	f := newFake()
	p := preflight.Params{
		SourceType:    models.TemplateSourceISO,
		DatastoreName: "NAS-vmstore",
	}
	r := preflight.RunAll(context.Background(), f, p)
	for _, id := range []string{"PF-01", "PF-02", "PF-03", "PF-04", "PF-05", "PF-06", "PF-07"} {
		res := findResult(t, r, id)
		if !res.OK {
			t.Errorf("%s should be a green skip for an ISO source; got Detail=%q Fix=%q", id, res.Detail, res.Fix)
		}
	}
}
