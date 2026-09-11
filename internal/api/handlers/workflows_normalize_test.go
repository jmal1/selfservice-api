package handlers

import (
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestNormalizeScriptLineEndings(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"already lf\n", "already lf\n"},
		{"crlf\r\nhere\r\n", "crlf\nhere\n"},
		{"bare\rcr\rsplits", "bare\ncr\nsplits"},
		{"mixed\r\nlf\nand\rcr", "mixed\nlf\nand\ncr"},
	}
	for _, c := range cases {
		if got := normalizeScriptLineEndings(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeScriptPtr_NilPassThrough(t *testing.T) {
	if got := normalizeScriptPtr(nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestNormalizeScriptPtr_StripsCR(t *testing.T) {
	in := "echo hi\r\n"
	got := normalizeScriptPtr(&in)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if *got != "echo hi\n" {
		t.Errorf("got %q, want %q", *got, "echo hi\n")
	}
	// The input pointer must not be mutated in place — handlers may still
	// reference the request body for logging.
	if in != "echo hi\r\n" {
		t.Errorf("input was mutated: %q", in)
	}
}

func TestNormalizeActionScripts_NormalizesEveryEntry(t *testing.T) {
	actions := []models.Action{
		{Name: "a", Script: "one\r\ntwo\r\n"},
		{Name: "b", Script: "no-cr-here\n"},
		{Name: "c", Script: ""},
	}
	got := normalizeActionScripts(actions)
	if len(got) != 3 {
		t.Fatalf("length changed: %d", len(got))
	}
	if got[0].Script != "one\ntwo\n" {
		t.Errorf("entry 0 not normalized: %q", got[0].Script)
	}
	if got[1].Script != "no-cr-here\n" {
		t.Errorf("entry 1 changed unexpectedly: %q", got[1].Script)
	}
	if got[2].Script != "" {
		t.Errorf("entry 2 empty case: %q", got[2].Script)
	}
}
