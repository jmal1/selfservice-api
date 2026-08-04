package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
)

var templateStateCensusOrder = models.AllTemplateStates

var templateStuckStates = []string{
	models.TemplateStateProvisioning,
	models.TemplateStateConfiguring,
	models.TemplateStateGeneralizing,
	models.TemplateStateVerifying,
}

// CountTemplateStates returns a full census of template lifecycle states.
// Known states are zero-filled so callers can publish a complete gauge family
// even when some states are absent in the database.
func (q *Queries) CountTemplateStates(ctx context.Context) (map[string]int, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT template_state, COUNT(*)
		FROM templates
		WHERE template_state = ANY($1)
		GROUP BY template_state
	`, templateStateCensusOrder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int, len(templateStateCensusOrder))
	for _, state := range templateStateCensusOrder {
		out[state] = 0
	}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// CountStuckTemplates returns how many templates have remained in an in-flight
// lifecycle state longer than olderThan. A non-zero value means the wizard
// pipeline wedged in provisioning/configuring/generalizing/verifying.
func (q *Queries) CountStuckTemplates(ctx context.Context, olderThan time.Duration) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM templates
		WHERE template_state = ANY($1)
		  AND updated_at < now() - $2::interval
	`, templateStuckStates, fmt.Sprintf("%d seconds", int(olderThan.Seconds()))).Scan(&n)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	return n, err
}
