package workflowvalidation

import (
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/jmal1/selfservice-api/internal/actionlibrary"
)

// RunActionError reports an activation-blocking workflow authoring error.
type RunActionError struct {
	Line    int
	Message string
}

func (e *RunActionError) Error() string {
	if e.Line <= 0 {
		return e.Message
	}
	return fmt.Sprintf("line %d: %s", e.Line, e.Message)
}

// LibraryAction describes whether a catalog action is materialized as a Bash
// function in runner pods.
type LibraryAction struct {
	Slug              string
	RunnerCallable    bool
	UnavailableReason string
}

// ValidateRunActionCalls checks actual Bash command nodes against the runner's
// label-plus-command calling convention.
func ValidateRunActionCalls(script string, librarySlugs []string) error {
	actions := make([]LibraryAction, 0, len(librarySlugs))
	for _, slug := range librarySlugs {
		actions = append(actions, LibraryAction{Slug: slug, RunnerCallable: true})
	}
	return ValidateRunActionCallsWithCatalog(script, actions)
}

// ValidateRunActionCallsWithCatalog validates calls against the exact action
// catalog available to the Bash runner.
func ValidateRunActionCallsWithCatalog(script string, libraryActions []LibraryAction) error {
	type callableResult struct {
		name              string
		err               error
		runnerCallable    bool
		unavailableReason string
	}
	callables := make(map[string]callableResult, len(libraryActions))
	callablesByName := make(map[string]callableResult, len(libraryActions))
	runnerSlugsByCallable := make(map[string]string, len(libraryActions))
	for _, action := range libraryActions {
		callable, err := actionlibrary.CallableName(action.Slug)
		result := callableResult{
			name:              callable,
			err:               err,
			runnerCallable:    action.RunnerCallable,
			unavailableReason: action.UnavailableReason,
		}
		callables[action.Slug] = result
		if err == nil {
			callablesByName[callable] = result
		}
		if err != nil || !action.RunnerCallable {
			continue
		}
		if previous, exists := runnerSlugsByCallable[callable]; exists {
			return &RunActionError{Message: fmt.Sprintf(
				"runner action library is ambiguous: slugs %q and %q both generate function %q",
				previous, action.Slug, callable,
			)}
		}
		runnerSlugsByCallable[callable] = action.Slug
	}

	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).
		Parse(strings.NewReader(script), "workflow")
	if err != nil {
		line := 1
		if langErr, ok := err.(syntax.LangError); ok {
			line = int(langErr.Pos.Line())
		}
		return &RunActionError{
			Line:    line,
			Message: "workflow script is not valid Bash: " + err.Error(),
		}
	}

	var validationErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		if validationErr != nil {
			return false
		}
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		command, static := staticWord(call.Args[0])
		if !static || command != "run_action" {
			return true
		}

		line := int(call.Pos().Line())
		if len(call.Args) < 3 {
			label := ""
			if len(call.Args) == 2 {
				label, _ = staticWord(call.Args[1])
			}
			message := "run_action requires a display label followed by a command or library function"
			if label != "" {
				message = fmt.Sprintf("run_action is missing its command or library function after display label %q", label)
				if callable, ok := callables[label]; ok {
					if callable.err != nil {
						message += fmt.Sprintf("; library action %q has no valid runner callable: %v", label, callable.err)
					} else if !callable.runnerCallable {
						message += fmt.Sprintf("; library action %q is unavailable to this runner: %s", label, callable.unavailableReason)
					} else {
						message += fmt.Sprintf("; call this library action as `run_action %q %s`", label, callable.name)
					}
				}
			}
			validationErr = &RunActionError{Line: line, Message: message}
			return false
		}

		commandArg, static := staticWord(call.Args[2])
		if !static {
			return true
		}
		callable, isLibrarySlug := callables[commandArg]
		if !isLibrarySlug {
			if byName, exists := callablesByName[commandArg]; exists && !byName.runnerCallable {
				validationErr = unavailableLibraryActionError(line, commandArg, byName.unavailableReason)
				return false
			}
			return true
		}
		if callable.err != nil {
			validationErr = &RunActionError{
				Line:    line,
				Message: fmt.Sprintf("run_action references library slug %q, which has no valid runner callable: %v", commandArg, callable.err),
			}
			return false
		}
		if !callable.runnerCallable {
			validationErr = unavailableLibraryActionError(line, commandArg, callable.unavailableReason)
			return false
		}
		if commandArg == callable.name {
			return true
		}
		label, _ := staticWord(call.Args[1])
		validationErr = &RunActionError{
			Line: line,
			Message: fmt.Sprintf(
				"run_action uses library slug %q as the callable; keep the first argument as the display label and use generated function %q as the second argument (for example: `run_action %q %s`)",
				commandArg,
				callable.name,
				label,
				callable.name,
			),
		}
		return false
	})
	return validationErr
}

func unavailableLibraryActionError(line int, action, reason string) error {
	if reason == "" {
		reason = "it is not materialized in the Bash action library"
	}
	return &RunActionError{
		Line:    line,
		Message: fmt.Sprintf("run_action references library action %q, but it is unavailable to this runner: %s", action, reason),
	}
}

func staticWord(word *syntax.Word) (string, bool) {
	var value strings.Builder
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit:
			value.WriteString(part.Value)
		case *syntax.SglQuoted:
			value.WriteString(part.Value)
		case *syntax.DblQuoted:
			nested, ok := staticWord(&syntax.Word{Parts: part.Parts})
			if !ok {
				return "", false
			}
			value.WriteString(nested)
		default:
			return "", false
		}
	}
	return value.String(), true
}
