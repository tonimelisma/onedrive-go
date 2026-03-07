package sync

import (
	"context"
	"database/sql"
	"fmt"
)

// UpsertShortcut inserts or updates a shortcut in the shortcuts table.
// Uses ON CONFLICT to explicitly control which columns are updated,
// preserving discovered_at on updates. The read_only column is always
// written as 0 (ignored — permission state lives in local_issues).
func (m *SyncStore) UpsertShortcut(ctx context.Context, sc *Shortcut) error {
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

// GetShortcut returns a shortcut by item ID, or nil if not found.
func (m *SyncStore) GetShortcut(ctx context.Context, itemID string) (*Shortcut, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts WHERE item_id = ?`, itemID)

	var sc Shortcut

	err := row.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
		&sc.DriveType, &sc.Observation, &sc.DiscoveredAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("sync: getting shortcut %s: %w", itemID, err)
	}

	return &sc, nil
}

// ListShortcuts returns all registered shortcuts.
func (m *SyncStore) ListShortcuts(ctx context.Context) ([]Shortcut, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts: %w", err)
	}
	defer rows.Close()

	var shortcuts []Shortcut

	for rows.Next() {
		var sc Shortcut
		if err := rows.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
			&sc.DriveType, &sc.Observation, &sc.DiscoveredAt); err != nil {
			return nil, fmt.Errorf("sync: scanning shortcut row: %w", err)
		}

		shortcuts = append(shortcuts, sc)
	}

	return shortcuts, rows.Err()
}

// DeleteShortcut removes a shortcut by item ID. No error if not found.
func (m *SyncStore) DeleteShortcut(ctx context.Context, itemID string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM shortcuts WHERE item_id = ?`, itemID)
	if err != nil {
		return fmt.Errorf("sync: deleting shortcut %s: %w", itemID, err)
	}

	return nil
}
