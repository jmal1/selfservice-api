package actionlibrary

import (
	"fmt"
	"regexp"
	"strings"
)

var callableNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// CallableName converts a kebab-case library action slug into the shell
// function name exported to workflow scripts.
func CallableName(slug string) (string, error) {
	name := strings.ReplaceAll(strings.TrimSpace(slug), "-", "_")
	if !callableNamePattern.MatchString(name) {
		return "", fmt.Errorf("library action slug %q does not yield a legal shell function name (got %q)", slug, name)
	}
	return name, nil
}
