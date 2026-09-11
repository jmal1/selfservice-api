package workflowvalidation

import (
	"strings"
	"testing"
)

func TestValidateRunActionCallsRejectsMissingCallable(t *testing.T) {
	script := "source /opt/crucible/lib/actions.sh\nset -euo pipefail\nrun_action \"demo-http-service-reachable\"\n"
	err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"})
	if err == nil {
		t.Fatal("expected malformed production call to be rejected")
	}
	if !strings.Contains(err.Error(), "line 3") ||
		!strings.Contains(err.Error(), "missing its command") ||
		!strings.Contains(err.Error(), "demo_http_service_reachable") {
		t.Fatalf("error is not instructor-actionable: %v", err)
	}
}

func TestValidateRunActionCallsRejectsKebabCaseLibrarySlug(t *testing.T) {
	err := ValidateRunActionCalls(
		`run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable --url "$CRUCIBLE_TARGET_IP"`,
		[]string{"demo-http-service-reachable"},
	)
	if err == nil {
		t.Fatal("expected kebab-case library slug to be rejected")
	}
	if !strings.Contains(err.Error(), `"demo-http-service-reachable"`) ||
		!strings.Contains(err.Error(), `"demo_http_service_reachable"`) {
		t.Fatalf("error must name both slug and callable: %v", err)
	}
}

func TestValidateRunActionCallsAllowsVisualBuilderLibraryCall(t *testing.T) {
	script := `run_action "Demo HTTP service reachable" demo_http_service_reachable --url "$CRUCIBLE_TARGET_IP"`
	if err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"}); err != nil {
		t.Fatalf("valid visual-builder output was rejected: %v", err)
	}
}

func TestValidateRunActionCallsAllowsInlineCommand(t *testing.T) {
	script := `run_action "HTTP probe" bash -c 'curl -fsS --max-time 5 "$CRUCIBLE_TARGET_IP"'`
	if err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"}); err != nil {
		t.Fatalf("valid inline command was rejected: %v", err)
	}
}

func TestValidateRunActionCallsOnlyTreatsKnownLibrarySlugsAsCallables(t *testing.T) {
	script := `run_action "Custom tool" locally-installed-tool --check`
	if err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"}); err != nil {
		t.Fatalf("hyphenated inline command was mistaken for a library slug: %v", err)
	}
}

func TestValidateRunActionCallsIgnoresQuotedTextAndComments(t *testing.T) {
	script := "echo 'run_action \"not a call\"'\n# run_action \"also not a call\"\nprintf '%s\\n' run_action\n"
	if err := ValidateRunActionCalls(script, nil); err != nil {
		t.Fatalf("non-call text was rejected: %v", err)
	}
}

func TestValidateRunActionCallsIgnoresHeredocBodies(t *testing.T) {
	script := `cat >"$CRUCIBLE_WORKDIR/example.sh" <<'SCRIPT'
run_action "documentation example"
run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable
SCRIPT
run_action "Real probe" bash -c 'true'
`
	if err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"}); err != nil {
		t.Fatalf("heredoc text was mistaken for an executable call: %v", err)
	}
}

func TestValidateRunActionCallsRejectsMissingCallableBeforeRedirection(t *testing.T) {
	for _, script := range []string{
		`run_action "HTTP probe" >"$CRUCIBLE_WORKDIR/result"`,
		`run_action "HTTP probe" 2>/dev/null`,
		`run_action "HTTP probe" > "$CRUCIBLE_WORKDIR/result"`,
	} {
		if err := ValidateRunActionCalls(script, nil); err == nil {
			t.Fatalf("missing callable hidden by redirection was accepted: %s", script)
		}
	}
}

func TestValidateRunActionCallsAllowsRedirectionsAroundCallable(t *testing.T) {
	for _, script := range []string{
		`run_action "HTTP probe" bash -c 'true' >"$CRUCIBLE_WORKDIR/result"`,
		`run_action "HTTP probe" >"$CRUCIBLE_WORKDIR/result" bash -c 'true'`,
	} {
		if err := ValidateRunActionCalls(script, nil); err != nil {
			t.Fatalf("valid redirected call was rejected: %v\n%s", err, script)
		}
	}
}

func TestValidateRunActionCallsDoesNotTreatArithmeticShiftAsHeredoc(t *testing.T) {
	script := "value=$((1 << 2))\nrun_action \"demo-http-service-reachable\"\n"
	err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"})
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("call after arithmetic shift was not validated: %v", err)
	}
}

func TestValidateRunActionCallsRejectsUnavailableLibraryAction(t *testing.T) {
	err := ValidateRunActionCallsWithCatalog(
		`run_action "Windows probe" windows_probe`,
		[]LibraryAction{{
			Slug:              "windows-probe",
			UnavailableReason: "supported_platforms includes windows",
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "unavailable to this runner") {
		t.Fatalf("unavailable library action was accepted: %v", err)
	}
}

func TestValidateRunActionCallsRejectsRunnerCallableCollision(t *testing.T) {
	err := ValidateRunActionCallsWithCatalog(
		`run_action "Probe" probe_action`,
		[]LibraryAction{
			{Slug: "probe-action", RunnerCallable: true},
			{Slug: "probe_action", RunnerCallable: true},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "both generate function") {
		t.Fatalf("colliding runner callables were accepted: %v", err)
	}
}

func TestValidateRunActionCallsRejectsLeadingRedirectionBypass(t *testing.T) {
	script := `>/dev/null run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable`
	err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"})
	if err == nil || !strings.Contains(err.Error(), "demo_http_service_reachable") {
		t.Fatalf("leading redirection bypassed validation: %v", err)
	}
}

func TestValidateRunActionCallsDoesNotConfuseBashSyntaxWithCommands(t *testing.T) {
	script := `case "$value" in
  run_action) echo "pattern only" ;;
esac
(( shifted = 1 << 2 ))
cat <<\EOF
run_action "heredoc example"
EOF
run_action "Real probe" bash -c 'true'
`
	if err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"}); err != nil {
		t.Fatalf("valid compound Bash was rejected: %v", err)
	}
}

func TestValidateRunActionCallsRejectsMalformedCallAfterArithmeticShift(t *testing.T) {
	script := "(( shifted = 1 << 2 ))\nrun_action \"DEMO - HTTP Service Reachable\" demo-http-service-reachable\n"
	err := ValidateRunActionCalls(script, []string{"demo-http-service-reachable"})
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("arithmetic shift hid malformed call: %v", err)
	}
}
