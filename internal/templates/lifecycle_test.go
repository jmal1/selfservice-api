package templates

import (
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TestCanTransition_AllowedMoves enumerates every allowed transition
// from the design doc and asserts each one is accepted. If this test
// fails, either lifecycle.go's allowedTransitions map or the design
// doc has drifted — never silently change one without the other.
func TestCanTransition_AllowedMoves(t *testing.T) {
	cases := []struct {
		from, to string
	}{
		{models.TemplateStateDraft, models.TemplateStateProvisioning},
		{models.TemplateStateProvisioning, models.TemplateStateConfiguring},
		{models.TemplateStateProvisioning, models.TemplateStateError},
		{models.TemplateStateConfiguring, models.TemplateStateGeneralizing},
		{models.TemplateStateConfiguring, models.TemplateStateDraft},
		{models.TemplateStateGeneralizing, models.TemplateStateReady},
		{models.TemplateStateGeneralizing, models.TemplateStateError},
		{models.TemplateStateReady, models.TemplateStateVerifying},
		{models.TemplateStateReady, models.TemplateStateConfiguring},
		{models.TemplateStateReady, models.TemplateStateDraft},
		{models.TemplateStateVerifying, models.TemplateStateActive},
		{models.TemplateStateVerifying, models.TemplateStateReady},
		{models.TemplateStateVerifying, models.TemplateStateError},
		{models.TemplateStateActive, models.TemplateStateReady},
		{models.TemplateStateError, models.TemplateStateDraft},
		{models.TemplateStateError, models.TemplateStateGeneralizing},
	}
	for _, c := range cases {
		t.Run(c.from+"→"+c.to, func(t *testing.T) {
			if err := CanTransition(c.from, c.to); err != nil {
				t.Errorf("CanTransition(%q, %q) returned %v; want nil", c.from, c.to, err)
			}
		})
	}
}

// TestCanTransition_RejectsSameState locks in the design choice that
// no-op transitions (from == to) are INVALID. Idempotency lives in the
// API layer, not here.
func TestCanTransition_RejectsSameState(t *testing.T) {
	for _, s := range models.AllTemplateStates {
		err := CanTransition(s, s)
		if !errors.Is(err, ErrInvalidStateTransition) {
			t.Errorf("CanTransition(%q, %q) = %v; want ErrInvalidStateTransition", s, s, err)
		}
	}
}

// TestCanTransition_RejectsUnknownStates covers the data-integrity
// guards: a row state we don't recognise must surface ErrUnknownState
// (not silently get rejected as a 409). Distinguishes "this row is
// corrupt" from "you asked for an illegal move".
func TestCanTransition_RejectsUnknownStates(t *testing.T) {
	t.Run("unknown from", func(t *testing.T) {
		err := CanTransition("never-existed", models.TemplateStateDraft)
		if !errors.Is(err, ErrUnknownState) {
			t.Errorf("got %v; want ErrUnknownState", err)
		}
	})
	t.Run("unknown to", func(t *testing.T) {
		err := CanTransition(models.TemplateStateDraft, "never-existed")
		if !errors.Is(err, ErrUnknownState) {
			t.Errorf("got %v; want ErrUnknownState", err)
		}
	})
	t.Run("empty from", func(t *testing.T) {
		err := CanTransition("", models.TemplateStateDraft)
		if !errors.Is(err, ErrUnknownState) {
			t.Errorf("got %v; want ErrUnknownState", err)
		}
	})
}

// TestCanTransition_RejectsAllNonAllowedMoves is the negative-case
// sweep: for every pair (from, to) where from != to and the pair is
// not in the allowed-moves list, CanTransition must reject with
// ErrInvalidStateTransition. This guards against accidentally widening
// the state machine in a future refactor.
func TestCanTransition_RejectsAllNonAllowedMoves(t *testing.T) {
	allowed := map[[2]string]bool{}
	for from, outs := range allowedTransitions {
		for to := range outs {
			allowed[[2]string{from, to}] = true
		}
	}
	for _, from := range models.AllTemplateStates {
		for _, to := range models.AllTemplateStates {
			if from == to {
				continue // covered by the same-state test above
			}
			if allowed[[2]string{from, to}] {
				continue // covered by the allowed-moves test above
			}
			t.Run(from+"→"+to+"_should_reject", func(t *testing.T) {
				err := CanTransition(from, to)
				if !errors.Is(err, ErrInvalidStateTransition) {
					t.Errorf("CanTransition(%q, %q) = %v; want ErrInvalidStateTransition", from, to, err)
				}
			})
		}
	}
}

// TestAllowedNextStates_ReturnsSortedSlice asserts AllowedNextStates is
// deterministic so handler responses don't churn between calls (which
// would defeat any cache layer the UI builds in front of /admin/templates/:id).
func TestAllowedNextStates_ReturnsSortedSlice(t *testing.T) {
	cases := []struct {
		from string
		want []string
	}{
		{models.TemplateStateDraft, []string{models.TemplateStateProvisioning}},
		{models.TemplateStateProvisioning, []string{models.TemplateStateConfiguring, models.TemplateStateError}},
		{models.TemplateStateConfiguring, []string{models.TemplateStateDraft, models.TemplateStateGeneralizing}},
		{models.TemplateStateGeneralizing, []string{models.TemplateStateError, models.TemplateStateReady}},
		{models.TemplateStateReady, []string{models.TemplateStateConfiguring, models.TemplateStateDraft, models.TemplateStateVerifying}},
		{models.TemplateStateVerifying, []string{models.TemplateStateActive, models.TemplateStateError, models.TemplateStateReady}},
		{models.TemplateStateActive, []string{models.TemplateStateReady}},
		{models.TemplateStateError, []string{models.TemplateStateDraft, models.TemplateStateGeneralizing}},
	}
	for _, c := range cases {
		t.Run(c.from, func(t *testing.T) {
			got, err := AllowedNextStates(c.from)
			if err != nil {
				t.Fatalf("AllowedNextStates(%q) error: %v", c.from, err)
			}
			// The function promises sorted output; we assert it.
			if !sort.StringsAreSorted(got) {
				t.Errorf("expected sorted slice, got %v", got)
			}
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("AllowedNextStates(%q) = %v; want %v", c.from, got, want)
			}
		})
	}
}

func TestAllowedNextStates_UnknownState(t *testing.T) {
	if _, err := AllowedNextStates("nonsense"); !errors.Is(err, ErrUnknownState) {
		t.Errorf("got %v; want ErrUnknownState", err)
	}
}

func TestIsKnownState(t *testing.T) {
	for _, s := range models.AllTemplateStates {
		if !IsKnownState(s) {
			t.Errorf("IsKnownState(%q) = false; want true", s)
		}
	}
	if IsKnownState("not-a-state") {
		t.Error("IsKnownState(\"not-a-state\") = true; want false")
	}
}

// TestIsTerminal documents the current invariant that the lifecycle
// has NO terminal states. If you add one (e.g. "archived"), update
// this test to match — don't just delete the assertion.
func TestIsTerminal_NoCurrentTerminalStates(t *testing.T) {
	for _, s := range models.AllTemplateStates {
		if IsTerminal(s) {
			t.Errorf("state %q is unexpectedly terminal; if intentional, update this test", s)
		}
	}
	if IsTerminal("unknown") {
		t.Error("unknown states must not be reported terminal")
	}
}

// TestLifecycleCoversAllModelStates is the cross-check between the
// state-machine map and models.AllTemplateStates: every state the
// model package exposes must be a node in the lifecycle graph, and
// vice versa. Catches the failure mode where someone adds a state to
// models.go but forgets the transition rules.
func TestLifecycleCoversAllModelStates(t *testing.T) {
	modelStates := map[string]bool{}
	for _, s := range models.AllTemplateStates {
		modelStates[s] = true
	}
	for s := range allowedTransitions {
		if !modelStates[s] {
			t.Errorf("allowedTransitions has %q but models.AllTemplateStates does not", s)
		}
	}
	for s := range modelStates {
		if _, ok := allowedTransitions[s]; !ok {
			t.Errorf("models.AllTemplateStates has %q but allowedTransitions does not", s)
		}
	}
}

// TestTemplateLifecycle_NoDirectReadyToActive is the anti-brick regression
// guard for the publish path.
//
// ready -> active must NOT be a legal edge. Publishing has to route through
// verifying, which clones the template, boots the clone, waits for Tools and
// an IP, and destroys it again. That gate is the only thing standing between
// a silently-broken template and every student who provisions from it: a
// template whose unattend password was scrubbed, or whose cloud-init never
// runs, looks perfectly healthy in vCenter and fails only at first login.
//
// TestAllowedNextStates_ReturnsSortedSlice would also catch this by exact-set
// equality, but it reads as a formatting assertion. This test states the
// intent, so anyone tempted to add a "publish now" shortcut finds out why the
// edge is missing rather than assuming it was an oversight.
func TestTemplateLifecycle_NoDirectReadyToActive(t *testing.T) {
	if err := CanTransition(models.TemplateStateReady, models.TemplateStateActive); err == nil {
		t.Error("ready -> active is allowed; publish must route through verifying " +
			"so the smoke test can reject a broken template before students clone it")
	}

	// The legal route must stay open, otherwise nothing can ever publish.
	if err := CanTransition(models.TemplateStateReady, models.TemplateStateVerifying); err != nil {
		t.Errorf("ready -> verifying must be allowed: %v", err)
	}
	if err := CanTransition(models.TemplateStateVerifying, models.TemplateStateActive); err != nil {
		t.Errorf("verifying -> active must be allowed: %v", err)
	}

	// A failed smoke test has to be able to send the template back rather
	// than stranding it in verifying forever.
	if err := CanTransition(models.TemplateStateVerifying, models.TemplateStateReady); err != nil {
		t.Errorf("verifying -> ready must be allowed so a failed verify can unwind: %v", err)
	}
}

// TestTemplateLifecycle_ErrorCanRerunGeneralize is the keep-the-VM recovery
// guard. GuestOps can return success without stamping
// guestinfo.crucible.generalize.job (passwordless sudo broken mid-script);
// the leftover staging VM is still the thing to generalize. error→draft
// via Retry refuses while a moref is set, and Cancel destroys the VM, so
// error→generalizing is the only edge that re-runs the existing job
// without throwing the guest away. The handler still requires
// vcenter_vm_id so a pre-clone provision failure cannot skip provision.
func TestTemplateLifecycle_ErrorCanRerunGeneralize(t *testing.T) {
	if err := CanTransition(models.TemplateStateError, models.TemplateStateGeneralizing); err != nil {
		t.Errorf("error -> generalizing must be allowed so a healthy leftover staging VM can re-run Generalize: %v", err)
	}
	// The destroy path must stay distinct; re-generalize must not replace it.
	if err := CanTransition(models.TemplateStateError, models.TemplateStateDraft); err != nil {
		t.Errorf("error -> draft must stay allowed for Cancel/Retry: %v", err)
	}
	// Do not invent error→configuring: the wizard already offers Generalize
	// once allowed_next_states contains generalizing, and configuring would
	// need a second handler the UI does not call from error.
	if err := CanTransition(models.TemplateStateError, models.TemplateStateConfiguring); err == nil {
		t.Error("error -> configuring is allowed; re-run Generalize from error instead of adding a resume-to-configuring hop")
	}
}
