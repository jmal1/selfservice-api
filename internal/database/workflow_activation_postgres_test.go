package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/workflowvalidation"
)

func TestActivateWorkflowValidatesPersistedScriptBeforeTransition(t *testing.T) {
	dsn := requirePostgresDSN(t)

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

	userID := uuid.New()
	workflowID := uuid.New()
	actionID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, userID, userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflows WHERE id = $1`, workflowID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM actions WHERE id = $1`, actionID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO actions (
			id, name, slug, action_type, action_category, script, is_library
		) VALUES ($1, 'Demo HTTP service reachable', 'demo-http-service-reachable',
			'command', 'network', 'return 0', true)
	`, actionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflows (
			id, name, slug, execution_mode, script, status, creation_mode, created_by
		) VALUES ($1, 'Malformed activation test', $2, 'kali_runner',
			'run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable',
			'approved', 'script', $3)
	`, workflowID, "malformed-activation-"+workflowID.String(), userID); err != nil {
		t.Fatal(err)
	}

	q := NewQueries(pool)
	err = q.ActivateWorkflow(ctx, workflowID)
	var validationErr *workflowvalidation.RunActionError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ActivateWorkflow error = %v, want RunActionError", err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, workflowID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.WorkflowStatusApproved {
		t.Fatalf("malformed workflow status = %q, want approved", status)
	}

	if _, err := pool.Exec(ctx, `
		UPDATE workflows
		SET script = 'run_action "Demo HTTP service reachable" demo_http_service_reachable'
		WHERE id = $1
	`, workflowID); err != nil {
		t.Fatal(err)
	}
	if err := q.ActivateWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("valid visual-builder call was rejected: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM workflows WHERE id = $1`, workflowID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != models.WorkflowStatusActive {
		t.Fatalf("valid workflow status = %q, want active", status)
	}
}

func TestUpdateWorkflowReturnsReviewedContentToDraft(t *testing.T) {
	dsn := requirePostgresDSN(t)
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
	for _, userID := range []uuid.UUID{creatorID, approverID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO users (id, oidc_sub, username, email)
			VALUES ($1, $2, $2, $3)
		`, userID, userID.String(), userID.String()+"@example.invalid"); err != nil {
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
		) VALUES ($1, 'Reviewed workflow', $2, 'kali_runner',
			'run_action "HTTP probe" bash -c "true"', 'active', 'script', $3, $4)
	`, workflowID, "reviewed-update-"+workflowID.String(), creatorID, approverID); err != nil {
		t.Fatal(err)
	}

	malformed := `run_action "DEMO - HTTP Service Reachable" demo-http-service-reachable`
	if err := NewQueries(pool).UpdateWorkflow(
		ctx, workflowID, nil, nil, nil, &malformed, nil, nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}

	var status string
	var approvedBy *uuid.UUID
	var script string
	if err := pool.QueryRow(ctx, `
		SELECT status, approved_by, script
		FROM workflows
		WHERE id = $1
	`, workflowID).Scan(&status, &approvedBy, &script); err != nil {
		t.Fatal(err)
	}
	if status != models.WorkflowStatusDraft {
		t.Fatalf("edited workflow status = %q, want draft", status)
	}
	if approvedBy != nil {
		t.Fatalf("edited workflow approved_by = %s, want NULL", *approvedBy)
	}
	if script != malformed {
		t.Fatalf("updated script = %q, want %q", script, malformed)
	}
}

func TestActivateWorkflowAllowsVMwareToolsGuestScript(t *testing.T) {
	dsn := requirePostgresDSN(t)
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

	userID := uuid.New()
	workflowID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, userID, userID.String(), userID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflows WHERE id = $1`, workflowID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	if _, err := pool.Exec(ctx, `
		INSERT INTO workflows (
			id, name, slug, execution_mode, script, status, creation_mode,
			guest_interpreter, created_by
		) VALUES ($1, 'PowerShell guest check', $2, 'vmware_tools',
			'Get-Service -Name sshd | Where-Object { $_.Status -eq "Running" }',
			'approved', 'script', 'powershell.exe', $3)
	`, workflowID, "powershell-activation-"+workflowID.String(), userID); err != nil {
		t.Fatal(err)
	}

	if err := NewQueries(pool).ActivateWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("vmware_tools guest script was parsed as Bash: %v", err)
	}
}
