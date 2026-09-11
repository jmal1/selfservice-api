package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jmal1/selfservice-api/internal/models"
)

func TestDestroyRetryQueryExcludesManualCleanupFailures(t *testing.T) {
	query := strings.ToUpper(listRetryableDestroyFailedPodsSQL)
	for _, fragment := range []string{
		"STATUS = 'DESTROY_FAILED'",
		"LEFT(COALESCE(ERROR_MESSAGE, ''), LENGTH($1)) <> $1",
	} {
		if !strings.Contains(query, fragment) {
			t.Errorf("destroy retry query does not contain %q", fragment)
		}
	}
	if models.PodErrorManualCleanupRequiredPrefix != "manual_cleanup_required:" {
		t.Fatalf(
			"manual cleanup error prefix = %q; changing it would requeue blocked destroys",
			models.PodErrorManualCleanupRequiredPrefix,
		)
	}
}

func TestListRetryableDestroyFailedPodsExcludesManualCleanupFailures(t *testing.T) {
	dsn := requirePostgresDSN(t)
	if err := RunMigrations(dsn); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	ownerID := uuid.New()
	retryID := uuid.New()
	manualID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, oidc_sub, username, email)
		VALUES ($1, $2, $2, $3)
	`, ownerID, "destroy-retry-"+ownerID.String(), ownerID.String()+"@example.invalid"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM pods WHERE id IN ($1, $2)`, retryID, manualID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, ownerID)
	}()
	if _, err := pool.Exec(ctx, `
		INSERT INTO pods (id, owner_id, name, status, error_message, vlan_id, subnet)
		VALUES
			($1, $3, 'retryable-destroy', 'destroy_failed', 'temporary RemovePortGroup failure', 3900, '10.250.0.0/24'),
			($2, $3, 'manual-destroy', 'destroy_failed', $4, 3901, '10.250.1.0/24')
	`, retryID, manualID, ownerID, models.PodErrorManualCleanupRequiredPrefix+" ownership is ambiguous"); err != nil {
		t.Fatal(err)
	}

	pods, err := (&Queries{pool: pool}).ListRetryableDestroyFailedPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundRetry := false
	for _, pod := range pods {
		switch pod.ID {
		case retryID:
			foundRetry = true
		case manualID:
			t.Fatal("manual-cleanup pod was returned to the automated destroy sweeper")
		}
	}
	if !foundRetry {
		t.Fatal("retryable destroy failure was omitted from the automated sweep")
	}
	count, err := (&Queries{pool: pool}).CountDestroyFailedPods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("destroy_failed count = %d, want retryable and manual-cleanup pods", count)
	}
}
