package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/database"
)

func TestGetWorkflowsForPlaylistExcludesEditedDrafts(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to an isolated PostgreSQL database to run engine workflow-status tests")
	}
	if err := database.RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	userID := uuid.New()
	workflowID := uuid.New()
	playlistID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, userID, userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM playlists WHERE id = $1`, playlistID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflows WHERE id = $1`, workflowID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflows (
			id, name, slug, execution_mode, script, status, creation_mode, created_by
		) VALUES ($1, 'Edited draft', $2, 'kali_runner', 'true', 'draft', 'script', $3)
	`, workflowID, "edited-draft-"+workflowID.String(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO playlists (id, name, slug, created_by)
		VALUES ($1, 'Workflow status test', $2, $3)
	`, playlistID, "workflow-status-"+playlistID.String(), userID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO playlist_workflows (playlist_id, workflow_id, execution_order)
		VALUES ($1, $2, 0)
	`, playlistID, workflowID); err != nil {
		t.Fatal(err)
	}

	q := NewQueries(pool)
	workflows, err := q.GetWorkflowsForPlaylist(ctx, playlistID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 0 {
		t.Fatalf("edited draft remained runnable: %+v", workflows)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'active' WHERE id = $1`, workflowID); err != nil {
		t.Fatal(err)
	}
	workflows, err = q.GetWorkflowsForPlaylist(ctx, playlistID)
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) != 1 || workflows[0].ID != workflowID {
		t.Fatalf("active workflow was not runnable: %+v", workflows)
	}
}
