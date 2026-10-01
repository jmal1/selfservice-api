package database

import (
	"context"
	"testing"
)

func TestSingleVMOnlyTemplateNamesEmpty(t *testing.T) {
	names, err := (&Queries{}).SingleVMOnlyTemplateNames(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if names != nil {
		t.Fatalf("names = %v, want nil", names)
	}
}
