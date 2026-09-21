package models

import "testing"

func TestIdleSuspendApplies_AllRoles(t *testing.T) {
	t.Parallel()
	for _, role := range []string{RoleStudent, RoleInstructor, RoleAdmin, "anything"} {
		if !IdleSuspendApplies(role) {
			t.Fatalf("IdleSuspendApplies(%q) = false, want true for every owner role", role)
		}
	}
}

func TestLimitsForRole_IdleSuspendHoursOnEveryRole(t *testing.T) {
	t.Parallel()
	for _, role := range []string{RoleStudent, RoleInstructor, RoleAdmin} {
		lim, err := LimitsForRole(role)
		if err != nil {
			t.Fatalf("LimitsForRole(%q): %v", role, err)
		}
		if !lim.IdleSuspendApplies {
			t.Fatalf("LimitsForRole(%q).IdleSuspendApplies = false, want true", role)
		}
		if lim.IdleSuspendHours == nil {
			t.Fatalf("LimitsForRole(%q).IdleSuspendHours = nil, want hours", role)
		}
		want := DefaultIdleTimeoutSeconds / 3600
		if *lim.IdleSuspendHours != want {
			t.Fatalf("LimitsForRole(%q).IdleSuspendHours = %d, want %d", role, *lim.IdleSuspendHours, want)
		}
	}
}
