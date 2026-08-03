package checks

import (
	"sort"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TestRunnerSmoke_TerminalStatusesMatchTheAPIContract pins the run-status
// strings runner_smoke polls for to the constants the API actually emits.
//
// runner_smoke deliberately hardcodes these as literals rather than importing
// models.RunStatus*, because it is a black-box prober: if it simply followed
// an internal rename it would stop noticing a breaking change to the run API
// that every real client — the UI included — would also hit.
//
// The failure mode that makes this test worth having is quiet. If a terminal
// status is renamed, waitForRunTerminal never matches it, so the check burns
// its full RunTimeout and reports "timed out waiting for terminal run status".
// That reads as "the Kali runner is broken" and would send someone to k3sv03,
// Multus and the image pull, when the runner in fact worked perfectly. This
// test converts that into a compile-time failure naming the real cause.
//
// If you are changing a run status on purpose, update BOTH the model constant
// and the literals in runner.go.
func TestRunnerSmoke_TerminalStatusesMatchTheAPIContract(t *testing.T) {
	if successRunStatus != models.RunStatusCompleted {
		t.Errorf("successRunStatus = %q but models.RunStatusCompleted = %q.\n"+
			"runner_smoke only treats this exact value as a pass, so a mismatch makes "+
			"every successful run look like a failed one.",
			successRunStatus, models.RunStatusCompleted)
	}

	// Every status the API can END on must be one runner_smoke recognises as
	// terminal. A missing entry does not fail fast — it hangs until timeout.
	//
	// "error" has no models.RunStatus* constant (only ResultStatusError exists
	// in models). It is included here because the engine may set it on
	// unexpected infra failures; treating it as terminal-bad prevents a
	// misleading "timed out" error when the run ended immediately.
	wantTerminal := []string{
		models.RunStatusCompleted,
		models.RunStatusFailed,
		models.RunStatusCancelled,
		models.RunStatusTimeout,
		"error", // no RunStatusError constant for runs; engine may set this on unexpected failure
	}
	for _, want := range wantTerminal {
		if !isTerminalRunStatus(want) {
			t.Errorf("run status %q is terminal in the API but runner_smoke does not "+
				"recognise it as terminal; the check would poll until RunTimeout and "+
				"report a misleading timeout instead of the real outcome", want)
		}
	}

	// Conversely, a NON-terminal status must never be treated as terminal, or
	// the check would grade a run that is still provisioning.
	for _, notTerminal := range []string{
		models.RunStatusPending,
		models.RunStatusProvisioning,
		models.RunStatusRunning,
	} {
		if isTerminalRunStatus(notTerminal) {
			t.Errorf("run status %q is NOT terminal but runner_smoke treats it as "+
				"terminal; the check would assert on a run that has not finished",
				notTerminal)
		}
	}

	// Guard against the list silently growing stale in the other direction:
	// an entry here that the API can never emit is dead weight that hides the
	// fact nobody re-checked this list.
	got := append([]string(nil), terminalRunStatuses...)
	want := append([]string(nil), wantTerminal...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("terminalRunStatuses = %v, want exactly %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("terminalRunStatuses = %v, want exactly %v", got, want)
		}
	}
}