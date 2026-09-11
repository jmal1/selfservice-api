package database

import (
	"strings"
	"testing"
)

// The template health checker and the student-facing template list are separate
// SQL statements over the same concept: "templates a student can see". When
// migration 000029 added the visibility column, only the student list was
// updated, so the health checker silently kept checking (and could alert on)
// instructor_only templates that no student can reach.
//
// These assert on the exported const rather than reading the .go source off
// disk: a filepath-based assertion passes on Windows and can never pass on
// Linux CI, because backslashes are literal filename characters on POSIX.
func TestStudentVisibleTemplateFilterPredicates(t *testing.T) {
	for _, want := range []string{
		"is_active = true",
		"is_internal = false",
		"template_state = 'active'",
		"visibility = 'public'",
	} {
		if !strings.Contains(StudentVisibleTemplateFilter, want) {
			t.Errorf("StudentVisibleTemplateFilter is missing %q; the health checker would check templates students cannot see", want)
		}
	}
}

// The health checker must never widen past the student list. If this filter
// ever gains an OR, a single true disjunct could pull instructor_only or
// inactive templates back into the checked set.
func TestStudentVisibleTemplateFilterHasNoDisjunction(t *testing.T) {
	if strings.Contains(strings.ToUpper(StudentVisibleTemplateFilter), " OR ") {
		t.Error("StudentVisibleTemplateFilter contains OR; the student-visibility property no longer holds by construction")
	}
}

// The student branch of ListTemplatesForUser applies visibility = 'public'.
// Pin ordering must never be part of the filter - it is ordering, not scope.
func TestStudentVisibleTemplateFilterIsFilterNotOrdering(t *testing.T) {
	if strings.Contains(strings.ToUpper(StudentVisibleTemplateFilter), "ORDER BY") {
		t.Error("StudentVisibleTemplateFilter contains ORDER BY; it is spliced into a WHERE clause")
	}
}