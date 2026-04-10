package syncstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
)

// schemaSQL is the canonical sync-store schema. The project has no launched
// users and no state-compatibility burden, so the store applies the final
// schema directly and rejects unknown existing schema versions.
//
//go:embed schema.sql
var schemaSQL string

const syncStoreSchemaVersion = 1

// ErrIncompatibleSchema marks a state DB that cannot be trusted under the
// current durable-intent schema. The state is rebuildable, but user intent is
// now part of the DB, so incompatible stores fail loudly instead of mutating.
var ErrIncompatibleSchema = errors.New("sync: incompatible sync store schema")

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin schema bootstrap: %w", err)
	}

	if err := ensureCompatibleSchemaVersion(ctx, tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("sync: check schema version: %w", err),
				fmt.Errorf("sync: rollback schema bootstrap: %w", rollbackErr),
			)
		}

		return fmt.Errorf("sync: check schema version: %w", err)
	}

	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("sync: apply schema bootstrap: %w", err),
				fmt.Errorf("sync: rollback schema bootstrap: %w", rollbackErr),
			)
		}
		return fmt.Errorf("sync: apply schema bootstrap: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(syncStoreSchemaVersion)); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("sync: set schema version: %w", err),
				fmt.Errorf("sync: rollback schema bootstrap: %w", rollbackErr),
			)
		}

		return fmt.Errorf("sync: set schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit schema bootstrap: %w", err)
	}

	return nil
}

func ensureCompatibleSchemaVersion(ctx context.Context, tx *sql.Tx) error {
	version, err := readSchemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version == syncStoreSchemaVersion {
		return nil
	}

	hasTables, err := hasUserTables(ctx, tx)
	if err != nil {
		return err
	}
	if version == 0 && !hasTables {
		return nil
	}

	return fmt.Errorf(
		"%w: found version %d, expected version %d; rebuild or delete the drive state DB and run sync again",
		ErrIncompatibleSchema,
		version,
		syncStoreSchemaVersion,
	)
}

func readSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("sync: read schema version: %w", err)
	}

	return version, nil
}

func hasUserTables(ctx context.Context, tx *sql.Tx) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'table'
		  AND name NOT LIKE 'sqlite_%'`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("sync: inspect schema tables: %w", err)
	}

	return count > 0, nil
}
