package syncstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
)

// migrationFS is the authoritative sync-store schema history. New stores apply
// all embedded migrations; existing stores must already be goose-managed so we
// never guess at durable user-intent shape.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

const currentMigrationVersion = int64(1)

// ErrIncompatibleSchema marks a state DB that cannot be trusted under the
// current durable-intent schema. The state is rebuildable, but user intent is
// now part of the DB, so incompatible stores fail loudly instead of mutating.
var ErrIncompatibleSchema = errors.New("sync: incompatible sync store schema")

func applySchema(ctx context.Context, db *sql.DB) error {
	if err := ensureGooseManagedOrEmpty(ctx, db); err != nil {
		return fmt.Errorf("sync: check migration state: %w", err)
	}

	migrations, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("sync: open embedded migrations: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectSQLite3,
		db,
		migrations,
		goose.WithLogger(goose.NopLogger()),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("sync: configure migrations: %w", err)
	}
	if _, upErr := provider.Up(ctx); upErr != nil {
		return fmt.Errorf("sync: apply migrations: %w", upErr)
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("sync: read migration version: %w", err)
	}
	if version != currentMigrationVersion {
		return fmt.Errorf(
			"%w: found migration version %d, expected version %d; rebuild or migrate the drive state DB and run sync again",
			ErrIncompatibleSchema,
			version,
			currentMigrationVersion,
		)
	}

	return nil
}

func ensureGooseManagedOrEmpty(ctx context.Context, db *sql.DB) error {
	hasGooseVersion, err := tableExists(ctx, db, goose.DefaultTablename)
	if err != nil {
		return err
	}
	if hasGooseVersion {
		return nil
	}

	hasTables, err := hasNonGooseUserTables(ctx, db)
	if err != nil {
		return err
	}
	if !hasTables {
		return nil
	}

	return fmt.Errorf(
		"%w: existing sync store has no goose migration history; rebuild or migrate the drive state DB and run sync again",
		ErrIncompatibleSchema,
	)
}

func tableExists(ctx context.Context, db *sql.DB, tableName string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table'
		  AND name = ?`, tableName).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("sync: inspect table %s: %w", tableName, err)
	}

	return count > 0, nil
}

func hasNonGooseUserTables(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'
		  AND name <> ?`, goose.DefaultTablename).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("sync: inspect schema tables: %w", err)
	}

	return count > 0, nil
}
