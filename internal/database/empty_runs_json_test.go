package database_test

import (
	"encoding/json"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestEmptyRunsSliceJSONIsArrayNotNull(t *testing.T) {
	runs := make([]models.Run, 0)
	b, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("empty runs must encode as [], got %s", b)
	}
	var nilRuns []models.Run
	b, err = json.Marshal(nilRuns)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("control: nil slice should still be null, got %s", b)
	}
}
