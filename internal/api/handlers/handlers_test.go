package handlers

import (
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// templateDeleteStateRefusal gates AdminDeleteTemplate. The contract:
// only block when a worker job is currently mid-flight on the staging
// VM. Every other state must allow delete (with VM cleanup as a side
// effect) — otherwise operators have no clean way to recover from
// errored templates or abandoned drafts that still hold a moref.
func TestTemplateDeleteStateRefusal(t *testing.T) {
	cases := []struct {
		state     string
		wantBlock bool
	}{
		{models.TemplateStateDraft, false},
		{models.TemplateStateProvisioning, true},
		{models.TemplateStateConfiguring, false},
		{models.TemplateStateGeneralizing, true},
		{models.TemplateStateVerifying, true},
		{models.TemplateStateReady, false},
		{models.TemplateStateActive, false},
		{models.TemplateStateError, false},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			block, reason := templateDeleteStateRefusal(tc.state)
			if block != tc.wantBlock {
				t.Errorf("state %q: refusal=%v, want %v (reason=%q)",
					tc.state, block, tc.wantBlock, reason)
			}
			if block && reason == "" {
				t.Errorf("state %q: blocked but reason is empty", tc.state)
			}
			if !block && reason != "" {
				t.Errorf("state %q: not blocked but reason is %q (want empty)",
					tc.state, reason)
			}
		})
	}
}

// Verify the canonical state list above stays in lockstep with the
// lifecycle constants — i.e. nobody adds a new TemplateState* without
// updating templateDeleteStateRefusal and this test together.
func TestTemplateDeleteStateRefusalCoversAllStates(t *testing.T) {
	covered := map[string]bool{
		models.TemplateStateDraft:        true,
		models.TemplateStateProvisioning: true,
		models.TemplateStateConfiguring:  true,
		models.TemplateStateGeneralizing: true,
		models.TemplateStateVerifying:    true,
		models.TemplateStateReady:        true,
		models.TemplateStateActive:       true,
		models.TemplateStateError:        true,
	}
	for _, state := range models.AllTemplateStates {
		if !covered[state] {
			t.Errorf("TemplateState %q is in models.AllTemplateStates but not "+
				"exercised by TestTemplateDeleteStateRefusal — add a case there "+
				"and decide whether delete should be allowed", state)
		}
	}
}
