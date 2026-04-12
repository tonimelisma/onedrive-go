// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - ListRemoteState:          current remote mirror rows
//   - GetRemoteStateByPath:     point lookup by path + drive
//   - GetRemoteStateByID:       point lookup by drive + item ID
//   - queryRemoteStateRows:     shared multi-row remote_state scanner
//   - scanRemoteStateRowWithQuerier: shared single-row remote_state scanner
package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	sqlGetRemoteStateByPath = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, is_filtered, observed_at,
		filter_generation, filter_reason
		FROM remote_state
		WHERE path = ? AND drive_id = ?`

	sqlGetRemoteStateByID = `SELECT drive_id, item_id, path, parent_id, item_type,
		hash, size, mtime, etag, previous_path, is_filtered, observed_at,
		filter_generation, filter_reason
		FROM remote_state
		WHERE drive_id = ? AND item_id = ?`
)

// ListRemoteState returns the current remote mirror rows.
func (m *SyncStore) ListRemoteState(ctx context.Context) ([]synctypes.RemoteStateRow, error) {
	return m.queryRemoteStateRows(ctx,
		`SELECT drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path, is_filtered, observed_at, filter_generation, filter_reason
		FROM remote_state`,
	)
}

func (m *SyncStore) queryRemoteStateRows(ctx context.Context, query string, args ...any) ([]synctypes.RemoteStateRow, error) {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close()

	var result []synctypes.RemoteStateRow

	for rows.Next() {
		var (
			row      synctypes.RemoteStateRow
			parentID sql.NullString
			hash     sql.NullString
			size     sql.NullInt64
			mtime    sql.NullInt64
			etag     sql.NullString
			prevPath sql.NullString
			filtered int
			reason   sql.NullString
		)

		if err := rows.Scan(
			&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
			&hash, &size, &mtime, &etag,
			&prevPath, &filtered, &row.ObservedAt, &row.FilterGeneration, &reason,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning remote_state row: %w", err)
		}

		row.ParentID = parentID.String
		row.Hash = hash.String
		row.ETag = etag.String
		row.PreviousPath = prevPath.String
		row.IsFiltered = filtered != 0
		row.FilterReason = synctypes.RemoteFilterReason(reason.String)

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

// GetRemoteStateByPath looks up the current remote_state row for a path+drive
// combination.
func (m *SyncStore) GetRemoteStateByPath(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (*synctypes.RemoteStateRow, bool, error) {
	row, err := scanRemoteStateRowWithQuerier(
		func(dest ...any) error {
			return m.db.QueryRowContext(ctx, sqlGetRemoteStateByPath, path, driveID.String()).Scan(dest...)
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: GetRemoteStateByPath %s: %w", path, err)
	}

	return row, true, nil
}

// GetRemoteStateByID looks up the exact remote_state row for a drive+item ID.
func (m *SyncStore) GetRemoteStateByID(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) (*synctypes.RemoteStateRow, bool, error) {
	row, err := scanRemoteStateRowWithQuerier(
		func(dest ...any) error {
			return m.db.QueryRowContext(ctx, sqlGetRemoteStateByID, driveID.String(), itemID).Scan(dest...)
		},
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("sync: GetRemoteStateByID %s: %w", itemID, err)
	}

	return row, true, nil
}

func scanRemoteStateRowWithQuerier(
	scan func(dest ...any) error,
) (*synctypes.RemoteStateRow, error) {
	var (
		row          synctypes.RemoteStateRow
		parentID     sql.NullString
		hash         sql.NullString
		size         sql.NullInt64
		mtime        sql.NullInt64
		etag         sql.NullString
		prevPath     sql.NullString
		filterReason sql.NullString
		isFiltered   int
	)

	if err := scan(
		&row.DriveID, &row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath, &isFiltered, &row.ObservedAt, &row.FilterGeneration, &filterReason,
	); err != nil {
		return nil, err
	}

	row.ParentID = parentID.String
	row.Hash = hash.String
	row.ETag = etag.String
	row.PreviousPath = prevPath.String
	row.IsFiltered = isFiltered != 0
	row.FilterReason = synctypes.RemoteFilterReason(filterReason.String)

	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row, nil
}
