package sync

import (
	"context"
	"database/sql"
	"fmt"
)

// UpsertShortcut inserts or updates a shortcut in the shortcuts table.
// Uses ON CONFLICT to explicitly control which columns are updated,
// preserving read_only and discovered_at on updates.
func (m *SyncStore) UpsertShortcut(ctx context.Context, sc *Shortcut) error {
	_, err := m.db.ExecContext(ctx,
		`INSERT INTO shortcuts
			(item_id, remote_drive, remote_item, local_path, drive_type, observation, read_only, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
			remote_drive = excluded.remote_drive,
			remote_item  = excluded.remote_item,
			local_path   = excluded.local_path,
			drive_type   = excluded.drive_type,
			observation  = excluded.observation`,
		sc.ItemID, sc.RemoteDrive, sc.RemoteItem, sc.LocalPath,
		sc.DriveType, sc.Observation, sc.ReadOnly, sc.DiscoveredAt,
	)
	if err != nil {
		return fmt.Errorf("sync: upserting shortcut %s: %w", sc.ItemID, err)
	}

	return nil
}

// GetShortcut returns a shortcut by item ID, or nil if not found.
func (m *SyncStore) GetShortcut(ctx context.Context, itemID string) (*Shortcut, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, read_only, discovered_at
		FROM shortcuts WHERE item_id = ?`, itemID)

	var sc Shortcut

	err := row.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
		&sc.DriveType, &sc.Observation, &sc.ReadOnly, &sc.DiscoveredAt)
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
		`SELECT item_id, remote_drive, remote_item, local_path, drive_type, observation, read_only, discovered_at
		FROM shortcuts ORDER BY item_id`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing shortcuts: %w", err)
	}
	defer rows.Close()

	var shortcuts []Shortcut

	for rows.Next() {
		var sc Shortcut
		if err := rows.Scan(&sc.ItemID, &sc.RemoteDrive, &sc.RemoteItem, &sc.LocalPath,
			&sc.DriveType, &sc.Observation, &sc.ReadOnly, &sc.DiscoveredAt); err != nil {
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

// SetShortcutReadOnly updates the read_only flag for a shortcut.
func (m *SyncStore) SetShortcutReadOnly(ctx context.Context, itemID string, readOnly bool) error {
	_, err := m.db.ExecContext(ctx,
		`UPDATE shortcuts SET read_only = ? WHERE item_id = ?`, readOnly, itemID)
	if err != nil {
		return fmt.Errorf("sync: setting shortcut %s read_only=%v: %w", itemID, readOnly, err)
	}

	return nil
}
