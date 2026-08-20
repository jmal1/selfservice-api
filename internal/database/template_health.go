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
	TemplateID                    uuid.UUID  `json:"template_id"`
	TemplateName                  string     `json:"template_name"`
	HealthStatus                  string     `json:"health_status"`
	ConsecutiveFailures           int        `json:"consecutive_failures"`
	StructuralConsecutiveFailures int        `json:"structural_consecutive_failures"`
	DeepConsecutiveFailures       int        `json:"deep_consecutive_failures"`
	LastStructuralCheckAt         *time.Time `json:"last_structural_check_at,omitempty"`
	LastStructuralPassed          *bool      `json:"last_structural_passed,omitempty"`
	LastStructuralError           *string    `json:"last_structural_error,omitempty"`
	LastStructuralFaultClass      *string    `json:"last_structural_fault_class,omitempty"`
	LastStructuralDurationSeconds *float64   `json:"last_structural_duration_seconds,omitempty"`
	LastDeepCheckAt               *time.Time `json:"last_deep_check_at,omitempty"`
	LastDeepPassed                *bool      `json:"last_deep_passed,omitempty"`
	LastDeepError                 *string    `json:"last_deep_error,omitempty"`
	LastDeepFaultClass            *string    `json:"last_deep_fault_class,omitempty"`
	LastDeepDurationSeconds       *float64   `json:"last_deep_duration_seconds,omitempty"`
	PendingDeepFailureAt          *time.Time `json:"pending_deep_failure_at,omitempty"`
	DeepConfirmationDueAt         *time.Time `json:"deep_confirmation_due_at,omitempty"`
	LastError                     *string    `json:"last_error,omitempty"`
	UpdatedAt                     time.Time  `json:"updated_at"`
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

// CompareAndSwapTemplateHealthState inserts a new row or replaces an existing
// row only when updated_at still matches the version the caller read. Health
// confirmations run on the normal job workers while the elected leader runs
// reconciliation, so an unconditional full-row upsert could restore a stale
// pending token after a confirmation had atomically cleared it.
func (q *Queries) CompareAndSwapTemplateHealthState(ctx context.Context, state TemplateHealthState) (bool, error) {
	if state.UpdatedAt.IsZero() {
		tag, err := q.pool.Exec(ctx, `
		INSERT INTO template_health_state
		    (template_id, health_status, consecutive_failures,
		     structural_consecutive_failures, deep_consecutive_failures,
		     last_structural_check_at, last_structural_passed,
		     last_structural_error, last_structural_fault_class,
		     last_structural_duration_seconds, last_deep_check_at,
		     last_deep_passed, last_deep_error, last_deep_fault_class,
		     last_deep_duration_seconds, pending_deep_failure_at,
		     deep_confirmation_due_at, last_error, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, clock_timestamp())
		ON CONFLICT (template_id) DO NOTHING
	`,
			state.TemplateID,
			state.HealthStatus,
			state.ConsecutiveFailures,
			state.StructuralConsecutiveFailures,
			state.DeepConsecutiveFailures,
			state.LastStructuralCheckAt,
			state.LastStructuralPassed,
			state.LastStructuralError,
			state.LastStructuralFaultClass,
			state.LastStructuralDurationSeconds,
			state.LastDeepCheckAt,
			state.LastDeepPassed,
			state.LastDeepError,
			state.LastDeepFaultClass,
			state.LastDeepDurationSeconds,
			state.PendingDeepFailureAt,
			state.DeepConfirmationDueAt,
			state.LastError,
		)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() == 1, nil
	}

	tag, err := q.pool.Exec(ctx, `
		UPDATE template_health_state SET
		    health_status = $2,
		    consecutive_failures = $3,
		    structural_consecutive_failures = $4,
		    deep_consecutive_failures = $5,
		    last_structural_check_at = $6,
		    last_structural_passed = $7,
		    last_structural_error = $8,
		    last_structural_fault_class = $9,
		    last_structural_duration_seconds = $10,
		    last_deep_check_at = $11,
		    last_deep_passed = $12,
		    last_deep_error = $13,
		    last_deep_fault_class = $14,
		    last_deep_duration_seconds = $15,
		    pending_deep_failure_at = $16,
		    deep_confirmation_due_at = $17,
		    last_error = $18,
		    updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')
		WHERE template_id = $1 AND updated_at = $19
	`,
		state.TemplateID,
		state.HealthStatus,
		state.ConsecutiveFailures,
		state.StructuralConsecutiveFailures,
		state.DeepConsecutiveFailures,
		state.LastStructuralCheckAt,
		state.LastStructuralPassed,
		state.LastStructuralError,
		state.LastStructuralFaultClass,
		state.LastStructuralDurationSeconds,
		state.LastDeepCheckAt,
		state.LastDeepPassed,
		state.LastDeepError,
		state.LastDeepFaultClass,
		state.LastDeepDurationSeconds,
		state.PendingDeepFailureAt,
		state.DeepConfirmationDueAt,
		state.LastError,
		state.UpdatedAt,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// GetTemplateHealthState returns the health state for a single template,
// or nil if no row exists yet (first cycle has not run).
func (q *Queries) GetTemplateHealthState(ctx context.Context, templateID uuid.UUID) (*TemplateHealthState, error) {
	var s TemplateHealthState
	err := q.pool.QueryRow(ctx, `
		SELECT ths.template_id, t.name,
		       ths.health_status, ths.consecutive_failures,
		       ths.structural_consecutive_failures, ths.deep_consecutive_failures,
		       ths.last_structural_check_at, ths.last_structural_passed,
		       ths.last_structural_error, ths.last_structural_fault_class,
		       ths.last_structural_duration_seconds,
		       ths.last_deep_check_at, ths.last_deep_passed,
		       ths.last_deep_error, ths.last_deep_fault_class,
		       ths.last_deep_duration_seconds,
		       ths.pending_deep_failure_at, ths.deep_confirmation_due_at,
		       ths.last_error, ths.updated_at
		FROM template_health_state ths
		JOIN templates t ON t.id = ths.template_id
		WHERE ths.template_id = $1
	`, templateID).Scan(
		&s.TemplateID, &s.TemplateName,
		&s.HealthStatus, &s.ConsecutiveFailures,
		&s.StructuralConsecutiveFailures, &s.DeepConsecutiveFailures,
		&s.LastStructuralCheckAt, &s.LastStructuralPassed,
		&s.LastStructuralError, &s.LastStructuralFaultClass,
		&s.LastStructuralDurationSeconds,
		&s.LastDeepCheckAt, &s.LastDeepPassed,
		&s.LastDeepError, &s.LastDeepFaultClass,
		&s.LastDeepDurationSeconds,
		&s.PendingDeepFailureAt, &s.DeepConfirmationDueAt,
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

// GetLastTemplateHealthCycleCompletedAt returns the durable completion marker
// written only after all state changes and the replacement snapshot succeed.
// Per-template timestamps cannot serve as this clock because a failed cycle may
// have updated only its first few templates.
func (q *Queries) GetLastTemplateHealthCycleCompletedAt(ctx context.Context) (*time.Time, error) {
	var completedAt *time.Time
	err := q.pool.QueryRow(ctx, `
		SELECT last_completed_at
		FROM template_health_reconcile_state
		WHERE singleton = TRUE
	`).Scan(&completedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return completedAt, nil
}

// MarkTemplateHealthCycleCompleted advances the durable cycle clock after the
// full reconciliation and complete Pushgateway replacement have succeeded.
func (q *Queries) MarkTemplateHealthCycleCompleted(ctx context.Context, completedAt time.Time) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO template_health_reconcile_state (singleton, last_completed_at)
		VALUES (TRUE, $1)
		ON CONFLICT (singleton) DO UPDATE
		SET last_completed_at = EXCLUDED.last_completed_at
	`, completedAt)
	return err
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
		       COALESCE(ths.structural_consecutive_failures, 0),
		       COALESCE(ths.deep_consecutive_failures, 0),
		       ths.last_structural_check_at,
		       ths.last_structural_passed,
		       ths.last_structural_error,
		       ths.last_structural_fault_class,
		       ths.last_structural_duration_seconds,
		       ths.last_deep_check_at,
		       ths.last_deep_passed,
		       ths.last_deep_error,
		       ths.last_deep_fault_class,
		       ths.last_deep_duration_seconds,
		       ths.pending_deep_failure_at,
		       ths.deep_confirmation_due_at,
		       ths.last_error,
		       COALESCE(ths.updated_at, t.updated_at)
		FROM templates t
		LEFT JOIN template_health_state ths ON ths.template_id = t.id
		WHERE `+StudentVisibleTemplateFilter+`
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
			&s.StructuralConsecutiveFailures, &s.DeepConsecutiveFailures,
			&s.LastStructuralCheckAt, &s.LastStructuralPassed,
			&s.LastStructuralError, &s.LastStructuralFaultClass,
			&s.LastStructuralDurationSeconds,
			&s.LastDeepCheckAt, &s.LastDeepPassed,
			&s.LastDeepError, &s.LastDeepFaultClass,
			&s.LastDeepDurationSeconds,
			&s.PendingDeepFailureAt, &s.DeepConfirmationDueAt,
			&s.LastError, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		states = append(states, s)
	}
	return states, rows.Err()
}

const createTemplateHealthConfirmationJobSQL = `
	INSERT INTO jobs (id, type, payload, next_attempt_at)
	VALUES ($1, 'template_health_confirm', $2, $3)
	ON CONFLICT (id) DO UPDATE SET
	    status = 'pending',
	    claimed_by = NULL,
	    claimed_at = NULL,
	    started_at = NULL,
	    completed_at = NULL,
	    result = NULL,
	    retry_count = 0,
	    next_attempt_at = EXCLUDED.next_attempt_at
	WHERE jobs.status = 'failed'
	RETURNING id
`

// CreateTemplateHealthConfirmationJob schedules one durable confirmation for a
// particular observed deep failure. The deterministic ID and ON CONFLICT make
// startup, ticker, and leader-failover schedulers race safely.
func (q *Queries) CreateTemplateHealthConfirmationJob(
	ctx context.Context,
	templateID uuid.UUID,
	payload []byte,
	nextAt time.Time,
) (bool, error) {
	jobID := templateHealthConfirmationJobID(templateID, payload)
	var inserted uuid.UUID
	err := q.pool.QueryRow(ctx, createTemplateHealthConfirmationJobSQL, jobID, payload, nextAt).Scan(&inserted)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func templateHealthConfirmationJobID(templateID uuid.UUID, payload []byte) uuid.UUID {
	return uuid.NewSHA1(templateID, payload)
}

// ApplyTemplateHealthDeepConfirmation consumes a pending failure only when the
// job still matches it. A success or a newer deep cycle makes an older job a
// harmless no-op.
func (q *Queries) ApplyTemplateHealthDeepConfirmation(
	ctx context.Context,
	templateID uuid.UUID,
	expectedFailureAt time.Time,
	passed bool,
	checkedAt time.Time,
	durationSeconds float64,
	errMsg *string,
	faultClass *string,
) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE template_health_state
		SET last_deep_check_at = $4,
		    last_deep_passed = $3,
		    last_deep_error = $6,
		    last_deep_fault_class = $7,
		    last_deep_duration_seconds = $5,
		    deep_consecutive_failures = CASE WHEN $3 THEN 0 ELSE 2 END,
		    pending_deep_failure_at = NULL,
		    deep_confirmation_due_at = NULL,
		    consecutive_failures = CASE
		        WHEN $3 THEN structural_consecutive_failures
		        ELSE GREATEST(structural_consecutive_failures, 2)
		    END,
		    health_status = CASE
		        WHEN NOT $3 OR structural_consecutive_failures >= 2 THEN 'unhealthy'
		        ELSE 'healthy'
		    END,
		    last_error = CASE
		        WHEN NOT $3 THEN $6
		        WHEN structural_consecutive_failures > 0 THEN last_structural_error
		        ELSE NULL
		    END,
		    updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')
		WHERE template_id = $1
		  AND pending_deep_failure_at = $2
	`, templateID, expectedFailureAt, passed, checkedAt, durationSeconds, errMsg, faultClass)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClearTemplateHealthDeepPending discards a confirmation schedule only when it
// still matches the failure token carried by the job. This is used when the
// target is no longer student-visible; it does not invent a passing attempt or
// change confirmed health.
func (q *Queries) ClearTemplateHealthDeepPending(
	ctx context.Context,
	templateID uuid.UUID,
	expectedFailureAt time.Time,
) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE template_health_state
		SET pending_deep_failure_at = NULL,
		    deep_confirmation_due_at = NULL,
		    updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')
		WHERE template_id = $1
		  AND pending_deep_failure_at = $2
	`, templateID, expectedFailureAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
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
		WHERE `+StudentVisibleTemplateFilter+`
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
