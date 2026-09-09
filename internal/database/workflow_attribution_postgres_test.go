package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestListWorkflows_IncludesCreatorAndApprover(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run workflow attribution tests")
	}
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	creatorID := uuid.New()
	approverID := uuid.New()
	workflowID := uuid.New()
	slug := "attr-wf-" + workflowID.String()[:8]

	for _, userID := range []uuid.UUID{creatorID, approverID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO users (id, oidc_sub, username, email, display_name, role)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, userID, userID.String(), "user-"+userID.String()[:8], userID.String()+"@example.invalid",
			"Display "+userID.String()[:8], models.RoleInstructor); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflows WHERE id = $1`, workflowID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id IN ($1, $2)`, creatorID, approverID)
	})

	if _, err := pool.Exec(ctx, `
		INSERT INTO workflows (
			id, name, slug, execution_mode, script, status, creation_mode,
			created_by, approved_by
		) VALUES ($1, 'Attribution Workflow', $2, 'kali_runner', '', 'pending_review', 'visual', $3, $4)
	`, workflowID, slug, creatorID, approverID); err != nil {
		t.Fatal(err)
	}

	q := NewQueries(pool)
	workflows, err := q.ListWorkflows(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var found *models.Workflow
	for i := range workflows {
		if workflows[i].ID == workflowID {
			found = &workflows[i]
			break
		}
	}
	if found == nil {
		t.Fatal("workflow not returned by ListWorkflows")
	}
	if found.Creator == nil || found.Creator.ID != creatorID {
		t.Fatalf("Creator = %+v, want id %s", found.Creator, creatorID)
	}
	if found.Creator.Username != "user-"+creatorID.String()[:8] {
		t.Fatalf("Creator.Username = %q", found.Creator.Username)
	}
	if found.Approver == nil || found.Approver.ID != approverID {
		t.Fatalf("Approver = %+v, want id %s", found.Approver, approverID)
	}
}

func TestListWorkflows_NullApproverLeavesApproverNil(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run workflow attribution tests")
	}
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	creatorID := uuid.New()
	workflowID := uuid.New()
	slug := "attr-draft-" + workflowID.String()[:8]
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email, display_name, role)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, creatorID, creatorID.String(), "creator-"+creatorID.String()[:8],
		creatorID.String()+"@example.invalid", "Creator", models.RoleInstructor); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflows WHERE id = $1`, workflowID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, creatorID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflows (
			id, name, slug, execution_mode, script, status, creation_mode, created_by
		) VALUES ($1, 'Draft Attribution', $2, 'kali_runner', '', 'draft', 'visual', $3)
	`, workflowID, slug, creatorID); err != nil {
		t.Fatal(err)
	}

	q := NewQueries(pool)
	workflows, err := q.ListWorkflows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *models.Workflow
	for i := range workflows {
		if workflows[i].ID == workflowID {
			found = &workflows[i]
			break
		}
	}
	if found == nil {
		t.Fatal("workflow not returned")
	}
	if found.Creator == nil {
		t.Fatal("expected Creator")
	}
	if found.Approver != nil {
		t.Fatalf("Approver = %+v, want nil", found.Approver)
	}
}
