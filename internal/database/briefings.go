package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmal1/selfservice-api/internal/models"
)

var (
	ErrBriefingOwnerNotFound    = errors.New("briefing owner not found")
	ErrBriefingRevisionNotFound = errors.New("briefing revision not found")
)

// BriefingConflictError is returned when a save's base revision is no longer
// the head, so the editor can show the instructor what changed.
type BriefingConflictError struct{ Head int }

func (e *BriefingConflictError) Error() string {
	return fmt.Sprintf("briefing head is revision %d", e.Head)
}

// Every owner lookup below uses the same predicate: $1 is the owner UUID and
// $2 is models.BriefingOwnerTemplate or models.BriefingOwnerBlueprint.

func (q *Queries) briefingOwnerExists(ctx context.Context, kind string, ownerID uuid.UUID) error {
	var exists bool
	err := q.pool.QueryRow(ctx, `
		SELECT CASE WHEN $2 = 'template'
		            THEN EXISTS (SELECT 1 FROM templates WHERE id = $1)
		            ELSE EXISTS (SELECT 1 FROM blueprints WHERE id = $1)
		       END
	`, ownerID, kind).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return ErrBriefingOwnerNotFound
	}
	return nil
}

// GetBriefingMeta returns head/published revision numbers. An owner that has
// never been briefed returns HeadRevision 0.
func (q *Queries) GetBriefingMeta(ctx context.Context, kind string, ownerID uuid.UUID) (*models.BriefingMeta, error) {
	if err := q.briefingOwnerExists(ctx, kind, ownerID); err != nil {
		return nil, err
	}
	meta := &models.BriefingMeta{OwnerKind: kind, OwnerID: ownerID}
	var updatedAt time.Time
	err := q.pool.QueryRow(ctx, `
		SELECT b.updated_at,
		       COALESCE((SELECT max(r.revision) FROM briefing_revisions r WHERE r.briefing_id = b.id), 0),
		       pr.revision
		FROM briefings b
		LEFT JOIN briefing_revisions pr ON pr.id = b.published_revision_id
		WHERE ($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1)
	`, ownerID, kind).Scan(&updatedAt, &meta.HeadRevision, &meta.PublishedRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return meta, nil
	}
	if err != nil {
		return nil, err
	}
	meta.UpdatedAt = &updatedAt
	return meta, nil
}

const briefingRevisionColumns = `
	r.revision, r.body_sha256, r.message, r.author_id, COALESCE(u.username, ''),
	r.restored_from, r.id IS NOT DISTINCT FROM b.published_revision_id, r.created_at`

func scanBriefingRevision(row pgx.Row, rev *models.BriefingRevision, extra ...any) error {
	return row.Scan(append([]any{
		&rev.Revision, &rev.BodySHA256, &rev.Message, &rev.AuthorID, &rev.Author,
		&rev.RestoredFrom, &rev.IsPublished, &rev.CreatedAt,
	}, extra...)...)
}

// ListBriefingRevisions returns history newest first, without bodies.
func (q *Queries) ListBriefingRevisions(ctx context.Context, kind string, ownerID uuid.UUID) ([]models.BriefingRevision, error) {
	if err := q.briefingOwnerExists(ctx, kind, ownerID); err != nil {
		return nil, err
	}
	rows, err := q.pool.Query(ctx, `
		SELECT `+briefingRevisionColumns+`
		FROM briefings b
		JOIN briefing_revisions r ON r.briefing_id = b.id
		LEFT JOIN users u ON u.id = r.author_id
		WHERE ($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1)
		ORDER BY r.revision DESC
	`, ownerID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.BriefingRevision{}
	for rows.Next() {
		var rev models.BriefingRevision
		if err := scanBriefingRevision(rows, &rev); err != nil {
			return nil, err
		}
		out = append(out, rev)
	}
	return out, rows.Err()
}

// GetBriefingRevision returns one revision including its body.
func (q *Queries) GetBriefingRevision(ctx context.Context, kind string, ownerID uuid.UUID, revision int) (*models.BriefingRevision, error) {
	if err := q.briefingOwnerExists(ctx, kind, ownerID); err != nil {
		return nil, err
	}
	var rev models.BriefingRevision
	err := scanBriefingRevision(q.pool.QueryRow(ctx, `
		SELECT `+briefingRevisionColumns+`, r.body_md
		FROM briefings b
		JOIN briefing_revisions r ON r.briefing_id = b.id
		LEFT JOIN users u ON u.id = r.author_id
		WHERE (($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1))
		  AND r.revision = $3
	`, ownerID, kind, revision), &rev, &rev.BodyMD)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBriefingRevisionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

// SaveBriefingRevision appends a new head revision. baseRevision must equal
// the current head (0 for the first save) or a *BriefingConflictError is
// returned. The briefings row is created on first save.
func (q *Queries) SaveBriefingRevision(ctx context.Context, kind string, ownerID, authorID uuid.UUID, body, message string, baseRevision int) (*models.BriefingRevision, error) {
	return q.appendBriefingRevision(ctx, kind, ownerID, authorID, &baseRevision, nil, body, message)
}

// RestoreBriefingRevision copies an old revision's body into a new head. It
// never rewrites history and does not change what is published.
func (q *Queries) RestoreBriefingRevision(ctx context.Context, kind string, ownerID, authorID uuid.UUID, revision int) (*models.BriefingRevision, error) {
	return q.appendBriefingRevision(ctx, kind, ownerID, authorID, nil, &revision, "", fmt.Sprintf("Restore revision %d", revision))
}

func (q *Queries) appendBriefingRevision(ctx context.Context, kind string, ownerID, authorID uuid.UUID, baseRevision, restoreFrom *int, body, message string) (*models.BriefingRevision, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO briefings (template_id, blueprint_id)
		VALUES (CASE WHEN $2 = 'template' THEN $1::uuid END,
		        CASE WHEN $2 = 'blueprint' THEN $1::uuid END)
		ON CONFLICT DO NOTHING
	`, ownerID, kind); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return nil, ErrBriefingOwnerNotFound
		}
		return nil, err
	}
	// The row lock serializes concurrent saves, so revision numbers stay
	// contiguous and exactly one of two racing saves on the same base wins.
	var briefingID uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT b.id FROM briefings b
		WHERE ($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1)
		FOR UPDATE
	`, ownerID, kind).Scan(&briefingID); err != nil {
		return nil, err
	}
	var parentID *uuid.UUID
	head := 0
	err = tx.QueryRow(ctx, `
		SELECT r.id, r.revision FROM briefing_revisions r
		WHERE r.briefing_id = $1 ORDER BY r.revision DESC LIMIT 1
	`, briefingID).Scan(&parentID, &head)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if baseRevision != nil && *baseRevision != head {
		return nil, &BriefingConflictError{Head: head}
	}
	if restoreFrom != nil {
		err := tx.QueryRow(ctx, `
			SELECT r.body_md FROM briefing_revisions r WHERE r.briefing_id = $1 AND r.revision = $2
		`, briefingID, *restoreFrom).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBriefingRevisionNotFound
		}
		if err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256([]byte(body))
	rev := &models.BriefingRevision{
		Revision:     head + 1,
		BodyMD:       body,
		BodySHA256:   hex.EncodeToString(sum[:]),
		Message:      message,
		RestoredFrom: restoreFrom,
	}
	if authorID != uuid.Nil {
		rev.AuthorID = &authorID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO briefing_revisions
			(briefing_id, revision, parent_id, body_md, body_sha256, message, author_id, restored_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING created_at
	`, briefingID, rev.Revision, parentID, rev.BodyMD, rev.BodySHA256, rev.Message, rev.AuthorID, rev.RestoredFrom,
	).Scan(&rev.CreatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE briefings SET updated_at = now() WHERE id = $1`, briefingID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return rev, nil
}

// PublishBriefingRevision points students at an existing revision. Publishing
// an older revision is how an instructor rolls back.
func (q *Queries) PublishBriefingRevision(ctx context.Context, kind string, ownerID uuid.UUID, revision int) error {
	if err := q.briefingOwnerExists(ctx, kind, ownerID); err != nil {
		return err
	}
	tag, err := q.pool.Exec(ctx, `
		UPDATE briefings b
		SET published_revision_id = r.id, updated_at = now()
		FROM briefing_revisions r
		WHERE r.briefing_id = b.id AND r.revision = $3
		  AND (($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1))
	`, ownerID, kind, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBriefingRevisionNotFound
	}
	return nil
}

// UnpublishBriefing hides the briefing from students. It is idempotent.
func (q *Queries) UnpublishBriefing(ctx context.Context, kind string, ownerID uuid.UUID) error {
	if err := q.briefingOwnerExists(ctx, kind, ownerID); err != nil {
		return err
	}
	_, err := q.pool.Exec(ctx, `
		UPDATE briefings b SET published_revision_id = NULL, updated_at = now()
		WHERE (($2 = 'template' AND b.template_id = $1) OR ($2 = 'blueprint' AND b.blueprint_id = $1))
		  AND b.published_revision_id IS NOT NULL
	`, ownerID, kind)
	return err
}

// ListPodBriefingSections resolves the published briefings for a pod: the
// blueprint's first (if any), then one per distinct template of its live VMs.
// Only published revisions are ever read here.
func (q *Queries) ListPodBriefingSections(ctx context.Context, podID uuid.UUID) ([]models.BriefingSection, error) {
	sections := []models.BriefingSection{}

	var bp models.BriefingSection
	err := q.pool.QueryRow(ctx, `
		SELECT bl.name, r.body_md, r.revision, r.created_at
		FROM pods p
		JOIN blueprints bl ON bl.id = p.blueprint_id
		JOIN briefings b ON b.blueprint_id = p.blueprint_id
		JOIN briefing_revisions r ON r.id = b.published_revision_id
		WHERE p.id = $1
	`, podID).Scan(&bp.Title, &bp.BodyMD, &bp.Revision, &bp.UpdatedAt)
	switch {
	case err == nil:
		bp.Scope = models.BriefingOwnerBlueprint
		sections = append(sections, bp)
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}

	rows, err := q.pool.Query(ctx, `
		SELECT t.id, t.name, array_agg(pv.display_name ORDER BY pv.boot_order, pv.created_at),
		       r.body_md, r.revision, r.created_at
		FROM pod_vms pv
		JOIN templates t ON t.id = pv.template_id
		JOIN briefings b ON b.template_id = pv.template_id
		JOIN briefing_revisions r ON r.id = b.published_revision_id
		WHERE pv.pod_id = $1 AND pv.status != 'deleted'
		GROUP BY t.id, t.name, r.body_md, r.revision, r.created_at
		ORDER BY min(pv.boot_order), t.name
	`, podID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s := models.BriefingSection{Scope: models.BriefingOwnerTemplate}
		var templateID uuid.UUID
		if err := rows.Scan(&templateID, &s.TemplateName, &s.VMNames, &s.BodyMD, &s.Revision, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.TemplateID = &templateID
		s.Title = s.TemplateName
		sections = append(sections, s)
	}
	return sections, rows.Err()
}
