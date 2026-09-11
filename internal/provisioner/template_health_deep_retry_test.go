package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// clonePlatformFault reproduces the exact vCenter fault string that broke the
// first production template-health cycle. internal/vcenter/template_ops.go
// documents this fault (Round 11, 2026-08-03) as intermittent and
// environmental: a clone of student-ubuntu-2404 failed and the identical clone
// succeeded 68 seconds later. It is not a property of the source VM.
const clonePlatformFault = "health-check clone task: The virtual disk is either corrupted or not a supported format."

// TestDeepCheckRetriesTransientCloneFailure is the regression test for the
// first production run of template health, which reported "Ubuntu 24.04
// Server" as failing its deep check 389ms after starting — while that same
// template had provisioned 72 student VMs successfully, including one earlier
// the same day.
//
// The cause was that the structural check was wrapped in retryWithBackoff but
// the deep check was a bare call, so the single most failure-prone operation
// in the codebase had no retry at all. That directly contradicts the feature's
// stated purpose of not alerting erroneously, and the resulting
// crucible_template_health_status{check_type="deep"}=0 would have fired
// CrucibleTemplateHealthFailing on a perfectly healthy template.
func TestDeepCheckRetriesTransientCloneFailure(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()

	// Fail twice with the real fault, then succeed — the documented shape.
	vc.cloneTransient = []error{
		errors.New(clonePlatformFault),
		errors.New(clonePlatformFault),
	}

	cfg := defaultCfg()
	counts, err := reconcileTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if !counts.DeepChecked {
		t.Fatal("expected a deep check to have run")
	}

	if vc.cloneCalls != 3 {
		t.Errorf("expected 3 clone attempts (2 transient failures + 1 success), got %d", vc.cloneCalls)
	}

	// The decisive assertion: a template that is genuinely healthy must not be
	// recorded as failing just because the platform blipped.
	state := db.states[tmpl.ID]
	if state == nil {
		t.Fatal("expected health state to be recorded")
	}
	if state.ConsecutiveFailures != 0 {
		t.Errorf("transient clone fault must not count as a failure, got consecutive_failures=%d (last_error=%v)",
			state.ConsecutiveFailures, derefErr(state.LastError))
	}
	if state.HealthStatus != "healthy" {
		t.Errorf("expected health_status=healthy after a successful retry, got %q", state.HealthStatus)
	}
}

// TestDeepCheckStillFailsWhenFaultIsPersistent guards the other direction: the
// retry must not paper over a genuinely broken template. If every attempt
// fails, the failure has to be recorded, otherwise the deep check can never
// detect anything and the whole feature is decorative.
func TestDeepCheckStillFailsWhenFaultIsPersistent(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)

	cfg := defaultCfg()
	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), cfg); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	if vc.cloneCalls != cfg.MaxRetries {
		t.Errorf("expected %d clone attempts before giving up, got %d", cfg.MaxRetries, vc.cloneCalls)
	}

	state := db.states[tmpl.ID]
	if state == nil {
		t.Fatal("expected health state to be recorded")
	}
	if state.ConsecutiveFailures == 0 {
		t.Error("a persistently failing deep check must be recorded as a failure")
	}
	if state.LastError == nil || !strings.Contains(*state.LastError, "corrupted or not a supported format") {
		t.Errorf("expected the underlying fault to be preserved in last_error, got %v", derefErr(state.LastError))
	}
}

// TestDeepCheckFailureIsLogged verifies the failure reason reaches the logs.
// On the first production cycle the log line said only `passed:false`, and the
// actual reason had to be dug out of Postgres by hand. A failure an operator
// cannot diagnose from logs is a failure they will misdiagnose.
func TestDeepCheckFailureIsLogged(t *testing.T) {
	tmpl := makeTemplate("11111111-1111-1111-1111-111111111111", "vm-101")
	db := newFakeHealthDB([]models.Template{tmpl})
	vc := newFakeHealthVC()
	vc.cloneErr = errors.New(clonePlatformFault)

	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if _, err := reconcileTemplateHealth(context.Background(), db, vc, nil, log, defaultCfg()); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "corrupted or not a supported format") {
		t.Errorf("deep check failure reason must appear in the logs; got:\n%s", out)
	}
}

func derefErr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

var _ = uuid.Nil
