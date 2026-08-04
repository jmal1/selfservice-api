package database

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jmal1/selfservice-api/internal/models"
)

// TemplateHealthState is the per-template health state row from
// template_health_state (migration 000031).
type TemplateHealthState struct {
	TemplateID             uuid.UUID  `json:"template_id"`
	TemplateName           string     `json:"template_name"`
	HealthStatus           string     `json:"health_status"`
	ConsecutiveFailures    int        `json:"consecutive_failures"`
	LastStructuralCheckAt  *time.Time `json:"last_structural_check_at,omitempty"`
	LastDeepCheckAt        *time.Time `json:"last_deep_check_at,omitempty"`
	LastError              *string    `json:"last_error,omitempty"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// StudentVisibleTemplateFilter is the WHERE predicate defining "a template a
// student can actually see". It is a const rather than inline SQL so a test can
// assert on it directly — asserting by reading the .go file off disk is
// path-fragile and cannot pass on both Windows and Linux CI.
//
// Every predicate here must mirror the student branch of ListTemplatesForUser.
const StudentVisibleTemplateFilter = `
		  is_active = true
		  AND is_internal = false
		  AND template_state = 'active'
		  AND visibility = 'public'`

// ListStudentVisibleTemplates returns all templates that are student-facing,
// per StudentVisibleTemplateFilter.
//
// This is the canonical query used by the health checker to decide which
// templates to check.
//
// The visibility predicate is deliberately explicit. An earlier revision of
// this comment claimed the query "automatically respects any future visibility
// controls"; that was never true of raw SQL, and when migration 000029 added
// the visibility column this query silently kept health-checking
// instructor_only templates. Alerting on a template no student can reach is
// exactly the noise the health checker exists to avoid.
//
// Callers must not add their own is_internal / template_state / visibility
// filtering on top of this result — the filter is already applied here.
func (q *Queries) ListStudentVisibleTemplates(ctx context.Context) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates
		WHERE `+StudentVisibleTemplateFilter+`
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []models.Template
	for rows.Next() {
		var t models.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, rows.Err()
}

// UpsertTemplateHealthState inserts or updates the health state row for a
// template. updated_at is always set to now(). Only the supplied fields are
// written; callers must provide all fields they intend to update.
func (q *Queries) UpsertTemplateHealthState(ctx context.Context, state TemplateHealthState) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO template_health_state
		    (template_id, health_status, consecutive_failures,
		     last_structural_check_at, last_deep_check_at, last_error, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (template_id) DO UPDATE SET
		    health_status           = EXCLUDED.health_status,
		    consecutive_failures    = EXCLUDED.consecutive_failures,
		    last_structural_check_at = EXCLUDED.last_structural_check_at,
		    last_deep_check_at      = EXCLUDED.last_deep_check_at,
		    last_error              = EXCLUDED.last_error,
		    updated_at              = now()
	`,
		state.TemplateID,
		state.HealthStatus,
		state.ConsecutiveFailures,
		state.LastStructuralCheckAt,
		state.LastDeepCheckAt,
		state.LastError,
	)
	return err
}

// GetTemplateHealthState returns the health state for a single template,
// or nil if no row exists yet (first cycle has not run).
func (q *Queries) GetTemplateHealthState(ctx context.Context, templateID uuid.UUID) (*TemplateHealthState, error) {
	var s TemplateHealthState
	err := q.pool.QueryRow(ctx, `
		SELECT ths.template_id, t.name,
		       ths.health_status, ths.consecutive_failures,
		       ths.last_structural_check_at, ths.last_deep_check_at,
		       ths.last_error, ths.updated_at
		FROM template_health_state ths
		JOIN templates t ON t.id = ths.template_id
		WHERE ths.template_id = $1
	`, templateID).Scan(
		&s.TemplateID, &s.TemplateName,
		&s.HealthStatus, &s.ConsecutiveFailures,
		&s.LastStructuralCheckAt, &s.LastDeepCheckAt,
		&s.LastError, &s.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetNewestTemplateHealthCheckTime returns the most recent structural check
// timestamp across all templates, or nil when no cycle has ever completed.
//
// This exists so the worker can decide, on leader acquisition, whether a cycle
// is actually due. The reconciler is driven by a 12h time.Ticker that restarts
// from zero every time the worker process restarts; this platform deploys
// several times a day, so a ticker-only reconciler would in practice never
// fire at all. Persisted state is the only restart-durable clock available.
//
// Structural (not deep) is the right column: the structural pass runs for every
// template on every cycle, so its max is the true "when did a cycle last run",
// whereas last_deep_check_at only advances for the one round-robin template.
func (q *Queries) GetNewestTemplateHealthCheckTime(ctx context.Context) (*time.Time, error) {
	var newest *time.Time
	err := q.pool.QueryRow(ctx, `
		SELECT MAX(last_structural_check_at) FROM template_health_state
	`).Scan(&newest)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return newest, nil
}

// ListTemplateHealthStates returns health state rows for all student-visible
// templates, joined with the template name. Templates that have never been
// checked yet are returned with health_status='unknown' and zero timestamps.
// The join is a LEFT JOIN so newly-published templates without a health row
// still appear in the response.
func (q *Queries) ListTemplateHealthStates(ctx context.Context) ([]TemplateHealthState, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT t.id, t.name,
		       COALESCE(ths.health_status, 'unknown'),
		       COALESCE(ths.consecutive_failures, 0),
		       ths.last_structural_check_at,
		       ths.last_deep_check_at,
		       ths.last_error,
		       COALESCE(ths.updated_at, t.updated_at)
		FROM templates t
		LEFT JOIN template_health_state ths ON ths.template_id = t.id
		WHERE t.is_active = true
		  AND t.is_internal = false
		  AND t.template_state = 'active'
		ORDER BY t.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var states []TemplateHealthState
	for rows.Next() {
		var s TemplateHealthState
		if err := rows.Scan(
			&s.TemplateID, &s.TemplateName,
			&s.HealthStatus, &s.ConsecutiveFailures,
			&s.LastStructuralCheckAt, &s.LastDeepCheckAt,
			&s.LastError, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		states = append(states, s)
	}
	return states, rows.Err()
}

// GetLeastRecentlyDeepCheckedTemplate returns the student-visible template
// that was least recently deep-checked (NULL last_deep_check_at sorts first).
// Returns nil if no student-visible templates exist.
//
// is_internal templates are excluded because the deep check clones the VM,
// and cloning synthetic-noop on every cycle would be pointless overhead.
func (q *Queries) GetLeastRecentlyDeepCheckedTemplate(ctx context.Context) (*models.Template, error) {
	var t models.Template
	err := scanTemplate(q.pool.QueryRow(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates t
		WHERE t.is_active = true
		  AND t.is_internal = false
		  AND t.template_state = 'active'
		ORDER BY (
		    SELECT ths.last_deep_check_at
		    FROM template_health_state ths
		    WHERE ths.template_id = t.id
		) ASC NULLS FIRST,
		t.name ASC
		LIMIT 1
	`), &t)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}
