package syncstore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
)

// schemaSQL is the canonical sync-store schema. The project has no launched
// users and no state-compatibility burden, so the store applies the final
// schema directly instead of carrying an incremental migration chain.
//
//go:embed schema.sql
var schemaSQL string

func applySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin schema bootstrap: %w", err)
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
