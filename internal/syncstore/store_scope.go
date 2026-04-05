package syncstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// UpsertSyncMetadataEntries writes arbitrary sync_metadata keys in one
// transaction. Engine-owned scope snapshot persistence uses this helper so the
// durable key/value store remains the single authority for watch/run metadata.
func (m *SyncStore) UpsertSyncMetadataEntries(ctx context.Context, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync metadata begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const upsertSQL = `INSERT INTO sync_metadata (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`

	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, upsertSQL, key, entries[key]); err != nil {
			return fmt.Errorf("sync metadata upsert %s: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sync metadata upsert: %w", err)
	}

	return nil
}

// ApplyRemoteScope marks already-known remote_state rows as filtered when they
// fall outside the current effective sync scope. Re-entry is handled by the
// next in-scope remote observation, which moves filtered rows back into the
// normal pending/synced state machine.
func (m *SyncStore) ApplyRemoteScope(ctx context.Context, snapshot syncscope.Snapshot) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: begin remote scope tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT drive_id, item_id, path, sync_status
		FROM remote_state
		WHERE sync_status NOT IN (?, ?, ?, ?)`,
		synctypes.SyncStatusDeleted,
		synctypes.SyncStatusPendingDelete,
		synctypes.SyncStatusDownloading,
		synctypes.SyncStatusDeleting,
	)
	if err != nil {
		return fmt.Errorf("sync: query remote scope rows: %w", err)
	}
	defer rows.Close()

	type scopeRow struct {
		driveID string
		itemID  string
		path    string
		status  synctypes.SyncStatus
	}

	var updates []scopeRow
	for rows.Next() {
		var row scopeRow
		if err := rows.Scan(&row.driveID, &row.itemID, &row.path, &row.status); err != nil {
			return fmt.Errorf("sync: scan remote scope row: %w", err)
		}

		if snapshot.AllowsPath(row.path) || row.status == synctypes.SyncStatusFiltered {
			continue
		}

		updates = append(updates, row)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("sync: iterate remote scope rows: %w", err)
	}

	for _, row := range updates {
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ? WHERE drive_id = ? AND item_id = ?`,
			synctypes.SyncStatusFiltered, row.driveID, row.itemID,
		); err != nil {
			return fmt.Errorf("sync: apply remote scope %s: %w", row.path, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit remote scope tx: %w", err)
	}

	return nil
}
