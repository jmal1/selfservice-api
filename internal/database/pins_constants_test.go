package database_test

import (
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/database"
)

// TestTemplatePinOrderClauseConstant verifies the canonical template pin ordering constant.
// This test proves that removing or changing the ORDER BY pinned DESC clause will break the assertion.
func TestTemplatePinOrderClauseConstant(t *testing.T) {
	if !strings.Contains(database.TemplatePinOrderClause, "pinned DESC") {
		t.Fatalf("TemplatePinOrderClause missing 'pinned DESC': %s", database.TemplatePinOrderClause)
	}
	if !strings.Contains(database.TemplatePinOrderClause, "pin_order") {
		t.Fatalf("TemplatePinOrderClause missing 'pin_order': %s", database.TemplatePinOrderClause)
	}
	if !strings.Contains(database.TemplatePinOrderClause, "pinned_at") {
		t.Fatalf("TemplatePinOrderClause missing 'pinned_at': %s", database.TemplatePinOrderClause)
	}
}

// TestBlueprintPinOrderClauseConstant verifies the canonical blueprint pin ordering constant.
func TestBlueprintPinOrderClauseConstant(t *testing.T) {
	if !strings.Contains(database.BlueprintPinOrderClause, "pinned DESC") {
		t.Fatalf("BlueprintPinOrderClause missing 'pinned DESC': %s", database.BlueprintPinOrderClause)
	}
	if !strings.Contains(database.BlueprintPinOrderClause, "pin_order") {
		t.Fatalf("BlueprintPinOrderClause missing 'pin_order': %s", database.BlueprintPinOrderClause)
	}
	if !strings.Contains(database.BlueprintPinOrderClause, "pinned_at") {
		t.Fatalf("BlueprintPinOrderClause missing 'pinned_at': %s", database.BlueprintPinOrderClause)
	}
}
