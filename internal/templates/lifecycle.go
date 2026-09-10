// Package templates owns the template-creation-wizard lifecycle (T4):
// the state machine, the per-state guards, and the helpers that worker
// jobs / API handlers use to advance a template through draft →
// provisioning → configuring → generalizing → ready → active.
//
// Keeping the transition rules in one pure-Go module (no DB, no HTTP)
// means handlers and worker jobs can share the same authority, the
// rules are trivially unit-testable, and the database CHECK constraint
// in migration 000018 is mirrored 1:1 here.
package templates

import (
	"errors"
	"fmt"
	"sort"

	"github.com/jmal1/selfservice-api/internal/models"
)

// ErrInvalidStateTransition is returned by CanTransition when the caller
// asks to move between two states that the lifecycle does not allow.
// Handlers translate this into HTTP 409 Conflict so the UI can re-load
// and present the operator with the genuinely available actions.
var ErrInvalidStateTransition = errors.New("invalid template state transition")

// ErrUnknownState is returned when a state string isn't one of the
// recognised TemplateState* constants. Callers should treat this as a
// data-integrity bug (e.g. a row created by an older codebase) and
// surface it as HTTP 500.
var ErrUnknownState = errors.New("unknown template state")

// allowedTransitions encodes the state diagram from
// future/Template-Creation-Workflow.md (lines 110-145). Each key is the
// current state; the value is the set of states it may legally move to.
//
// Terminal states (no outbound transitions) are present as empty maps so
// CanTransition can still distinguish "no transitions" from "unknown
// state".
//
// The shape mirrors migration 000018's CHECK constraint exactly:
//   draft         → provisioning
//   provisioning  → configuring, error
//   configuring   → generalizing, draft (cancel)
//   generalizing  → ready, error
//   ready         → verifying, configuring (re-enter), draft (discard)
//   verifying     → active (smoke passed), ready (smoke failed, retryable), error
//   active        → ready (unpublish)
//   error         → draft (retry/cleanup), generalizing (re-run when a
//                   staging VM is still recorded — handler requires moref)
var allowedTransitions = map[string]map[string]struct{}{
	models.TemplateStateDraft: {
		models.TemplateStateProvisioning: {},
	},
	models.TemplateStateProvisioning: {
		models.TemplateStateConfiguring: {},
		models.TemplateStateError:       {},
	},
	models.TemplateStateConfiguring: {
		models.TemplateStateGeneralizing: {},
		// Cancel from configuring tears the VM down and returns the
		// row to draft so the wizard can be restarted without
		// orphaning a half-built template.
		models.TemplateStateDraft: {},
	},
	models.TemplateStateGeneralizing: {
		models.TemplateStateReady: {},
		models.TemplateStateError: {},
	},
	models.TemplateStateReady: {
		// Publish now goes through an automated smoke test first: the
		// verify worker clones the base-image, boots it, and only then
		// promotes to `active`. There is deliberately NO direct
		// ready→active edge so the smoke gate cannot be skipped.
		models.TemplateStateVerifying: {},
		// Re-enter configuration: destroys the base-image snapshot
		// and powers the VM back on for further edits.
		models.TemplateStateConfiguring: {},
		// Discard from ready: VM never published, free to throw away.
		models.TemplateStateDraft: {},
	},
	models.TemplateStateVerifying: {
		// Smoke test passed: promote to active (worker also flips is_active).
		models.TemplateStateActive: {},
		// Smoke test failed: back to ready so the instructor can fix the
		// image (re-configure/re-generalize) and verify again.
		models.TemplateStateReady: {},
		// Infrastructure failure during verify (clone/vCenter error).
		models.TemplateStateError: {},
	},
	models.TemplateStateActive: {
		// Unpublish: row is no longer offered to students but the
		// snapshot is preserved so we can re-publish later.
		models.TemplateStateReady: {},
	},
	models.TemplateStateError: {
		// Operator-driven recovery: cleans up any partial vCenter
		// state and resets the row to draft. Retry (no VM) and
		// Cancel (destroy leftover VM) both land here.
		models.TemplateStateDraft: {},
		// Re-run Generalize without destroying a healthy leftover
		// staging VM. GuestOps can fail closed (missing sentinel,
		// passwordless sudo) after the OS is already installed;
		// the handler refuses this edge when vcenter_vm_id is
		// empty so a pre-clone provision failure cannot skip
		// provision and jump to generalize.
		models.TemplateStateGeneralizing: {},
	},
}

// CanTransition reports whether moving a template from `from` to `to` is
// permitted by the lifecycle. It is pure: no DB lookups, no I/O.
//
// Returns:
//   - nil                          → transition is allowed
//   - ErrUnknownState (wrapped)    → either state is not a TemplateState* constant
//   - ErrInvalidStateTransition    → both states are valid but the move is rejected
//
// A "no-op" transition (from == to) is treated as INVALID. Idempotency
// at the API boundary is the handler's responsibility, not the state
// machine's; treating same-state as valid here would let a worker job
// stuck in `provisioning` silently re-mark itself `provisioning` on
// retry instead of surfacing the bug.
func CanTransition(from, to string) error {
	out, ok := allowedTransitions[from]
	if !ok {
		return fmt.Errorf("%w: from=%q", ErrUnknownState, from)
	}
	if _, ok := allowedTransitions[to]; !ok {
		return fmt.Errorf("%w: to=%q", ErrUnknownState, to)
	}
	if _, ok := out[to]; !ok {
		return fmt.Errorf("%w: %s → %s", ErrInvalidStateTransition, from, to)
	}
	return nil
}

// AllowedNextStates returns the set of states reachable from `from` in a
// single transition, sorted alphabetically for deterministic output.
// The UI calls this (via an API endpoint) to render only the buttons
// that will actually succeed — for a row in `configuring`, the UI
// should only offer "Generalize" and "Cancel", never "Publish". For a
// row in `error`, allowed next states are `draft` (Retry/Cancel) and
// `generalizing` (re-run Generalize when a staging VM is still
// recorded; the handler refuses that edge if vcenter_vm_id is empty).
//
// Returns ErrUnknownState if `from` is not a recognised state.
func AllowedNextStates(from string) ([]string, error) {
	out, ok := allowedTransitions[from]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownState, from)
	}
	states := make([]string, 0, len(out))
	for s := range out {
		states = append(states, s)
	}
	sort.Strings(states)
	return states, nil
}

// IsTerminal reports whether `state` has no outbound transitions. The
// current lifecycle has none — every state can move somewhere — but
// keeping this helper future-proofs the API in case we add a hard
// "deleted" or "archived" terminal state later.
func IsTerminal(state string) bool {
	out, ok := allowedTransitions[state]
	if !ok {
		return false
	}
	return len(out) == 0
}

// IsKnownState reports whether `state` is one of the recognised
// lifecycle states. Equivalent to "is it in models.AllTemplateStates"
// but lookup is O(1) here.
func IsKnownState(state string) bool {
	_, ok := allowedTransitions[state]
	return ok
}
