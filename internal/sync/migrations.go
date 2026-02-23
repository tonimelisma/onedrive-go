package sync

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations applies all pending schema migrations to the database.
// Uses the goose v3 Provider API (no global state, context-aware).
func runMigrations(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	// Strip the "migrations/" prefix so goose sees files at the root of the FS.
	subFS, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("sync: creating migration sub-filesystem: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectSQLite3, db, subFS)
	if err != nil {
		return fmt.Errorf("sync: creating migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("sync: running migrations: %w", err)
	}

	for _, r := range results {
		logger.Info("applied migration",
			slog.String("source", r.Source.Path),
			slog.Int64("duration_ms", r.Duration.Milliseconds()),
		)
	}

	return nil
}
