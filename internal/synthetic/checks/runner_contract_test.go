package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/synthetic"
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
	// terminal. A missing entry does not fail fast — it hangs until RunTimeout
	// and then reports a timeout, which reads like the Kali runner hung when
	// the run in fact finished promptly.
	//
	// runner_smoke classifies by exclusion (see nonTerminalRunStatuses), so
	// this loop is really asserting that none of these ever drifts INTO the
	// non-terminal set.
	for _, want := range []string{
		models.RunStatusCompleted,
		models.RunStatusFailed,
		models.RunStatusCancelled,
		models.RunStatusTimeout,
	} {
		if !isTerminalRunStatus(want) {
			t.Errorf("run status %q is terminal in the API but runner_smoke does not "+
				"recognise it as terminal; the check would poll until RunTimeout and "+
				"report a misleading timeout instead of the real outcome", want)
		}
	}

	// Conversely, a NON-terminal status must never be treated as terminal, or
	// the check would grade a run that has not finished.
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

	// The non-terminal set is the whole contract now, so pin it exactly. It
	// must be precisely the in-progress statuses in models, no more and no
	// less: an extra entry means a finished run is polled forever, a missing
	// one means an unfinished run is graded.
	got := append([]string(nil), nonTerminalRunStatuses...)
	want := []string{
		models.RunStatusPending,
		models.RunStatusProvisioning,
		models.RunStatusRunning,
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("nonTerminalRunStatuses = %v, want exactly %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("nonTerminalRunStatuses = %v, want exactly %v", got, want)
		}
	}
}

// TestRunnerSmoke_UnknownStatusIsTerminalNotHung asserts the classification is
// by EXCLUSION, which is the property that makes this check degrade sensibly.
//
// A poller with an allowlist of terminal statuses does not report a status it
// does not know about — it WAITS on it, burns the full RunTimeout, and then
// says "timed out waiting for terminal run status". That message sends the
// reader to the runner, the node and the image pull, when the run actually
// finished immediately. Classifying by exclusion means a status added by a
// future migration is handled on the day it ships and is reported by name.
func TestRunnerSmoke_UnknownStatusIsTerminalNotHung(t *testing.T) {
	for _, s := range []string{"error", "aborted", "superseded", "expired"} {
		if !isTerminalRunStatus(s) {
			t.Errorf("unknown run status %q treated as non-terminal; runner_smoke "+
				"would poll it until RunTimeout and report a misleading timeout "+
				"instead of surfacing the real status", s)
		}
	}

	// An absent status is the one case that must NOT be called terminal: a
	// malformed or partial payload should keep polling, not be graded as a
	// finished run that failed.
	if isTerminalRunStatus("") {
		t.Error("empty run status treated as terminal; a partial or malformed " +
			"response would be graded as a finished run rather than polled again")
	}
}

// TestRunnerSmoke_EmptyTemplateNameFailsFast is the companion to
// TestRunnerSmoke_EmptyPlaylistID.
//
// main.go registers runner_smoke even when its env vars are unset, because an
// unregistered check's Prometheus series ceases to exist and therefore cannot
// match `1 - crucible_synthetic_check_success > 0` — it would be invisible
// rather than red. That design choice makes THIS error message the only thing
// that tells an operator what to fix, so it must name the env var and it must
// fire before any network call or pod is created.
func TestRunnerSmoke_EmptyTemplateNameFailsFast(t *testing.T) {
	// A server that answers every request with a teapot: reaching it at all
	// is the bug, and 418 is a status no check under test ever accepts.
	var reached int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reached, 1)
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	cfg := RunnerSmokeConfig{
		TemplateName:   "", // deliberately empty
		PlaylistID:     "0000-uuid",
		ReadyTimeout:   2 * time.Second,
		RunTimeout:     2 * time.Second,
		DestroyTimeout: 2 * time.Second,
		PreCleanMaxAge: time.Minute,
	}
	_, err := RunnerSmoke(cfg).Run(context.Background(), synthetic.NewClient(srv.URL, ""))
	if err == nil {
		t.Fatal("empty TemplateName must fail the check")
	}
	if !strings.Contains(err.Error(), "SYNTHETIC_RUNNER_TEMPLATE") {
		t.Errorf("error %q must name the env var; it is the only thing telling an "+
			"operator what to fix, since the check is registered regardless", err)
	}
	if n := atomic.LoadInt32(&reached); n != 0 {
		t.Errorf("made %d HTTP request(s) before failing: a missing env var must be "+
			"caught locally, not after a round-trip that yields a confusing "+
			"'template not found'", n)
	}
}
