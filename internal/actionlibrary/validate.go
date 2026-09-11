package actionlibrary

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// slugPattern is the kebab-case shape a library action slug must have.
//
// It is deliberately narrower than the callable pattern in names.go. Because
// the slug-to-callable map only rewrites `-` as `_`, restricting slugs to this
// shape makes the map injective: two distinct conforming slugs can never
// produce the same shell function. Slug uniqueness in the database is then
// sufficient to guarantee callable uniqueness, which is the property
// buildActionLibrary depends on.
var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidateSlug rejects a library action slug that cannot be safely rendered
// into the generated action library.
func ValidateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("slug %q must be lowercase kebab-case: letters and digits separated by single hyphens, starting with a letter (for example \"ufw-rule-exists\")", slug)
	}
	return nil
}

// ValidateBody parses an action body the way the engine will render it into the
// generated library: wrapped in `callable() { ... }`.
//
// The wrapping is not cosmetic. Bodies use `local` and `return`, which are only
// legal inside a function, so parsing a body on its own would reject valid
// input. More importantly the whole library is sourced as ONE file, so a single
// unparseable body does not fail its own action -- it fails `source` and takes
// down every action in every run on that pod. Catching it at authoring time is
// the difference between one instructor seeing a 400 and every student's
// assessment going red.
//
// Parsing is done in-process with mvdan.cc/sh rather than shelling out to
// `bash -n`, because the API pod is not guaranteed to have bash and a missing
// binary would silently degrade this from a guard into a no-op.
func ValidateBody(callable, script string) error {
	wrapped := callable + "() {\n" + script + "\n}\n"
	_, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(wrapped), "action")
	if err == nil {
		return nil
	}
	// Positions are reported against the wrapped source, whose first line is the
	// generated `callable() {`. Subtracting it puts the line number back on the
	// body the author actually typed.
	var parseErr syntax.ParseError
	if errors.As(err, &parseErr) {
		return fmt.Errorf("action body is not valid bash at line %d: %s", parseErr.Pos.Line()-1, parseErr.Text)
	}
	var langErr syntax.LangError
	if errors.As(err, &langErr) {
		return fmt.Errorf("action body is not valid bash at line %d: %s", langErr.Pos.Line()-1, langErr.Feature)
	}
	return fmt.Errorf("action body is not valid bash: %w", err)
}
