package handlers

import (
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestIsTerminalRunStatus(t *testing.T) {
	terminal := []string{
		models.RunStatusCompleted,
		models.RunStatusFailed,
		models.RunStatusCancelled,
		models.RunStatusTimeout,
	}
	nonTerminal := []string{
		models.RunStatusPending,
		models.RunStatusProvisioning,
		models.RunStatusRunning,
		"",
		"unknown",
	}
	for _, s := range terminal {
		if !isTerminalRunStatus(s) {
			t.Errorf("expected %s to be terminal", s)
		}
	}
	for _, s := range nonTerminal {
		if isTerminalRunStatus(s) {
			t.Errorf("expected %s to NOT be terminal", s)
		}
	}
}

func TestTerminalEventTypes_ContainsExpected(t *testing.T) {
	want := []string{"completed", "failed", "cancelled", "timeout"}
	for _, w := range want {
		if _, ok := terminalEventTypes[w]; !ok {
			t.Errorf("expected terminalEventTypes to include %q", w)
		}
	}
	// Non-terminal events MUST NOT be in the map or the WS would close prematurely.
	for _, w := range []string{"workflow_start", "workflow_complete", "action_complete", "running", "provisioning", "snapshot"} {
		if _, ok := terminalEventTypes[w]; ok {
			t.Errorf("did not expect terminalEventTypes to include %q (would close WS too early)", w)
		}
	}
}
