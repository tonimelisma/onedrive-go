// Package sync persists sync baseline, observation, failure, scope-block, and metadata state.
//
// Contents:
//   - ListRemoteState:          current remote mirror rows
//   - GetRemoteStateByPath:     point lookup by path
//   - GetRemoteStateByID:       point lookup by item ID
//   - queryRemoteStateRows:     shared multi-row remote_state scanner
//   - scanRemoteStateRowWithQuerier: shared single-row remote_state scanner
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	sqlGetRemoteStateByPath = `SELECT item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path
		FROM remote_state
		WHERE path = ?`

	sqlGetRemoteStateByID = `SELECT item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path
		FROM remote_state
		WHERE item_id = ?`
)

// ListRemoteState returns the current remote mirror rows.
func (m *SyncStore) ListRemoteState(ctx context.Context) ([]RemoteStateRow, error) {
	configuredDriveID, err := m.configuredDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("sync: reading configured drive for remote_state: %w", err)
	}

	return queryRemoteStateRowsWithRunner(ctx, m.db,
		`SELECT item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path
		FROM remote_state`,
		configuredDriveID,
	)
}

func queryRemoteStateRowsWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
	query string,
	configuredDriveID driveid.ID,
	args ...any,
) ([]RemoteStateRow, error) {
	rows, err := runner.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close()

	var result []RemoteStateRow

	for rows.Next() {
		var (
			row      RemoteStateRow
			parentID sql.NullString
			hash     sql.NullString
			size     sql.NullInt64
			mtime    sql.NullInt64
			etag     sql.NullString
			prevPath sql.NullString
		)

		if err := rows.Scan(
			&row.ItemID, &row.Path, &parentID, &row.ItemType,
			&hash, &size, &mtime, &etag,
			&prevPath,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning remote_state row: %w", err)
		}

		row.DriveID = configuredDriveID
		row.ParentID = parentID.String
		row.Hash = hash.String
		row.ETag = etag.String
		row.PreviousPath = prevPath.String

		if size.Valid {
			row.Size = size.Int64
		}
		if mtime.Valid {
			row.Mtime = mtime.Int64
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating remote_state rows: %w", err)
	}

	return result, nil
}

func (m *SyncStore) getRemoteStateRow(
	ctx context.Context,
	driveID driveid.ID,
	query string,
	arg string,
	contextLabel string,
) (*RemoteStateRow, bool, error) {
	configuredDriveID, err := m.configuredDriveIDForRead(ctx, driveID)
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading configured drive for %s: %w", contextLabel, err)
	}
	if matchErr := ensureMatchingConfiguredDriveID(driveID, configuredDriveID); matchErr != nil {
		return nil, false, matchErr
	}

	row, err := scanRemoteStateRowWithQuerier(
		configuredDriveID,
		func(dest ...any) error {
			return m.db.QueryRowContext(ctx, query, arg).Scan(dest...)
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: %s %s: %w", contextLabel, arg, err)
	}

	return row, true, nil
}

// GetRemoteStateByPath looks up the current remote_state row for a path.
func (m *SyncStore) GetRemoteStateByPath(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (*RemoteStateRow, bool, error) {
	return m.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByPath, path, "GetRemoteStateByPath")
}

// GetRemoteStateByID looks up the exact remote_state row for an item ID.
func (m *SyncStore) GetRemoteStateByID(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) (*RemoteStateRow, bool, error) {
	return m.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByID, itemID, "GetRemoteStateByID")
}

func scanRemoteStateRowWithQuerier(
	configuredDriveID driveid.ID,
	scan func(dest ...any) error,
) (*RemoteStateRow, error) {
	var (
		row      RemoteStateRow
		parentID sql.NullString
		hash     sql.NullString
		size     sql.NullInt64
		mtime    sql.NullInt64
		etag     sql.NullString
		prevPath sql.NullString
	)

	if err := scan(
		&row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath,
	); err != nil {
		return nil, err
	}

	row.DriveID = configuredDriveID
	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String

	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row, nil
}
