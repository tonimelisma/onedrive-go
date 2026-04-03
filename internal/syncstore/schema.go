package syncstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
)

// schemaSQL is the canonical sync-store schema. The project has no launched
// users and no state-compatibility burden, so the store applies the final
// schema directly and keeps only one narrow legacy baseline repair path.
//
//go:embed schema.sql
var schemaSQL string

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin schema bootstrap: %w", err)
	}

	if err := repairLegacyBaselineTable(ctx, tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return errors.Join(
				fmt.Errorf("sync: repair legacy baseline: %w", err),
				fmt.Errorf("sync: rollback schema bootstrap: %w", rollbackErr),
			)
		}

		return fmt.Errorf("sync: repair legacy baseline: %w", err)
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

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit schema bootstrap: %w", err)
	}

	return nil
}

func repairLegacyBaselineTable(ctx context.Context, tx *sql.Tx) error {
	cols, err := tableColumns(ctx, tx, "baseline")
	if err != nil {
		return err
	}

	if len(cols) == 0 || cols["local_size"] {
		return nil
	}

	if !cols["size"] || !cols["mtime"] {
		return fmt.Errorf("baseline table missing required legacy columns for repair")
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE baseline_new (
			drive_id        TEXT    NOT NULL,
			item_id         TEXT    NOT NULL,
			path            TEXT    NOT NULL UNIQUE,
			parent_id       TEXT,
			item_type       TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
			local_hash      TEXT,
			remote_hash     TEXT,
			local_size      INTEGER,
			remote_size     INTEGER,
			local_mtime     INTEGER,
			remote_mtime    INTEGER,
			synced_at       INTEGER NOT NULL CHECK(synced_at > 0),
			etag            TEXT,
			PRIMARY KEY (drive_id, item_id)
		)
	`); err != nil {
		return fmt.Errorf("create migrated baseline table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO baseline_new (
			drive_id, item_id, path, parent_id, item_type,
			local_hash, remote_hash, local_size, remote_size,
			local_mtime, remote_mtime, synced_at, etag
		)
		SELECT
			drive_id, item_id, path, parent_id, item_type,
			local_hash, remote_hash, size, NULL,
			mtime, NULL, synced_at, etag
		FROM baseline
	`); err != nil {
		return fmt.Errorf("copy baseline rows into migrated table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE baseline`); err != nil {
		return fmt.Errorf("drop legacy baseline table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE baseline_new RENAME TO baseline`); err != nil {
		return fmt.Errorf("rename migrated baseline table: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_baseline_parent ON baseline(parent_id)`); err != nil {
		return fmt.Errorf("recreate baseline parent index: %w", err)
	}

	return nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, tableName string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, tableName))
	if err != nil {
		return nil, fmt.Errorf("query table info for %s: %w", tableName, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)

		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan table info for %s: %w", tableName, err)
		}

		columns[strings.ToLower(name)] = true
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table info for %s: %w", tableName, err)
	}

	return columns, nil
}
