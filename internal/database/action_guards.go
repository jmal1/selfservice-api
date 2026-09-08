package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// LibraryActionCallableConflict returns the slug of an existing library action
// that would generate the same shell function name as callable, or "" if there
// is none. excludeID skips one row so an update can ignore itself.
//
// New slugs are constrained to a shape that maps injectively onto callables, so
// this can only fire against a legacy row stored before that guard existed --
// `http_get` and `http-get` both render `http_get()`. That is worth catching,
// because buildActionLibrary refuses to render an ambiguous library at all and
// the resulting failure takes down every run rather than the new action.
func (q *Queries) LibraryActionCallableConflict(ctx context.Context, callable string, excludeID uuid.UUID) (string, error) {
	var slug string
	err := q.pool.QueryRow(ctx, `
		SELECT slug
		FROM actions
		WHERE is_library = true
		  AND slug IS NOT NULL
		  AND replace(slug, '-', '_') = $1
		  AND id <> $2
		LIMIT 1
	`, callable, excludeID).Scan(&slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check library action callable conflict: %w", err)
	}
	return slug, nil
}

// WorkflowsCallingAction returns the names of workflows whose stored script
// references the given callable as a whole word.
//
// Renaming an action's slug renames its generated function, so every workflow
// calling the old name breaks -- and it breaks at run time, in a student's
// assessment, not at edit time. The match is deliberately textual: a script is
// bash, and the callable is the only handle a workflow has on an action. There
// is no foreign key to consult.
func (q *Queries) WorkflowsCallingAction(ctx context.Context, callable string) ([]string, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT name
		FROM workflows
		WHERE script ~ ('\m' || $1 || '\M')
		ORDER BY name
		LIMIT 20
	`, callable)
	if err != nil {
		return nil, fmt.Errorf("find workflows calling %q: %w", callable, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan workflow calling %q: %w", callable, err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
