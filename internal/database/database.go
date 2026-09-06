package database

import (
	"context"
	"embed"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/config"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Connect creates a connection pool to PostgreSQL.
//
// Pool sizing: the default MaxConns (20) is intentionally conservative so that
// a single process does not exhaust the server. Tune it per deployment via
// DB_MAX_CONNS. The platform-wide ceiling with the current configuration is:
//   api-gateway (20) + crucible-engine (20) + 4 × workers (5 + 1 leader conn) = 64
// against max_connections = 100, leaving ~36 headroom for admin tools and
// migrations. Adjust this comment when the fleet composition changes.
//
// When cfg.CredentialsFile is set, each new connection re-reads username and
// password from that file (Vault Agent template) so dynamic DB leases rotate
// without restarting the process.
func Connect(ctx context.Context, cfg config.DatabaseConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}

	// Default pool size. Override per-process via DB_MAX_CONNS (cfg.MaxConns).
	const defaultMaxConns = 20
	maxConns := int32(defaultMaxConns)
	if cfg.MaxConns > 0 {
		maxConns = int32(cfg.MaxConns)
	}
	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = 2
	if maxConns < 4 {
		// Avoid keeping more idle connections than the pool can hold.
		poolCfg.MinConns = 1
	}
	poolCfg.MaxConnLifetime = 30 * time.Minute

	if cfg.CredentialsFile != "" {
		credPath := cfg.CredentialsFile
		poolCfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
			user, pass, err := readCredentialsFile(credPath)
			if err != nil {
				return err
			}
			cc.User = user
			cc.Password = pass
			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// RunMigrations applies all pending database migrations.
func RunMigrations(dsn string) error {
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

// MigrateWithDriver runs migrations using an existing database connection.
func MigrateWithDriver(dsn string) error {
	// golang-migrate needs a stdlib *sql.DB, so we use the DSN directly.
	// The postgres driver handles connection internally.
	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("create migration source: %w", err)
	}
	_ = postgres.Postgres{} // ensure import

	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

// readCredentialsFile loads dynamic DB credentials. Format: username on line 1,
// password on line 2 (Vault Agent template output). Never log the contents.
func readCredentialsFile(path string) (user, password string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read DB credentials file: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return "", "", fmt.Errorf("DB credentials file %s must contain username and password lines", path)
	}
	user = strings.TrimSpace(lines[0])
	password = strings.TrimSpace(lines[1])
	if user == "" || password == "" {
		return "", "", fmt.Errorf("DB credentials file %s has empty username or password", path)
	}
	return user, password, nil
}
