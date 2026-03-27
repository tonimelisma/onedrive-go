package syncstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// UpsertShortcut inserts or updates a shortcut in the shortcuts table.
// Uses ON CONFLICT to explicitly control which columns are updated,
// preserving discovered_at on updates. The read_only column is always
// written as 0 (ignored — permission state lives in sync_failures).
func (m *SyncStore) UpsertShortcut(ctx context.Context, sc *synctypes.Shortcut) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO shortcuts
			(item_id, remote_drive, remote_item, local_path, drive_type, observation, read_only, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(item_id) DO UPDATE SET
			remote_drive = excluded.remote_drive,
			remote_item  = excluded.remote_item,
			local_path   = excluded.local_path,
			drive_type   = excluded.drive_type,
			observation  = excluded.observation`,
		sc.ItemID, sc.RemoteDrive, sc.RemoteItem, sc.LocalPath,
		sc.DriveType, sc.Observation, sc.DiscoveredAt,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting shortcut %s: %w", sc.ItemID, err)
	}

	return nil
}

// GetShortcut returns a shortcut by item ID.
func (m *SyncStore) GetShortcut(ctx context.Context, itemID string) (*synctypes.Shortcut, bool, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts WHERE item_id = ?`, itemID)

	var sc synctypes.Shortcut

	err := row.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
		&sc.DriveType, &sc.Observation, &sc.DiscoveredAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("sync: getting shortcut %s: %w", itemID, err)
	}

	return &sc, true, nil
}

// ListShortcuts returns all registered shortcuts.
func (m *SyncStore) ListShortcuts(ctx context.Context) ([]synctypes.Shortcut, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts: %w", err)
	}
	defer rows.Close()

	var shortcuts []synctypes.Shortcut

	for rows.Next() {
		var sc synctypes.Shortcut
		if err := rows.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
			&sc.DriveType, &sc.Observation, &sc.DiscoveredAt); err != nil {
			return nil, fmt.Errorf("sync: scanning shortcut row: %w", err)
		}

		shortcuts = append(shortcuts, sc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate shortcut rows: %w", err)
	}

	return shortcuts, nil
}

// DeleteShortcut removes a shortcut by item ID. No error if not found.
func (m *SyncStore) DeleteShortcut(ctx context.Context, itemID string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM shortcuts WHERE item_id = ?`, itemID)
	if err != nil {
		return fmt.Errorf("sync: deleting shortcut %s: %w", itemID, err)
	}

	return nil
}
