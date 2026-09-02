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
	sqlSelectRemoteStateCols = `drive_id, item_id, path, item_type,
		hash, size, mtime, etag`
	sqlGetRemoteStateByPath = `SELECT ` + sqlSelectRemoteStateCols + `
		FROM remote_state
		WHERE path = ?`

	sqlGetRemoteStateByID = `SELECT ` + sqlSelectRemoteStateCols + `
		FROM remote_state
		WHERE item_id = ?`
)

// ListRemoteState returns the current remote mirror rows.
func (s *SyncStore) ListRemoteState(ctx context.Context) ([]remoteStateRow, error) {
	contentDriveID, err := s.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("sync: reading content drive for remote_state: %w", err)
	}

	return queryRemoteStateRowsWithRunner(ctx, s.db,
		`SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`,
		contentDriveID,
	)
}

func queryRemoteStateRowsWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
	query string,
	contentDriveID driveid.ID,
) ([]remoteStateRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying remote_state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []remoteStateRow

	for rows.Next() {
		var (
			rawDriveID string
			row        remoteStateRow
			hash       sql.NullString
			size       sql.NullInt64
			mtime      sql.NullInt64
			etag       sql.NullString
		)

		if err := rows.Scan(
			&rawDriveID, &row.ItemID, &row.Path, &row.ItemType,
			&hash, &size, &mtime, &etag,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning remote_state row: %w", err)
		}

		row.DriveID = remoteStateDriveID(rawDriveID, contentDriveID)
		row.Hash = hash.String
		row.ETag = etag.String

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

func (s *SyncStore) getRemoteStateRow(
	ctx context.Context,
	driveID driveid.ID,
	query string,
	arg string,
	contextLabel string,
) (*remoteStateRow, bool, error) {
	contentDriveID, err := s.contentDriveIDForRead(ctx, driveID)
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading content drive for %s: %w", contextLabel, err)
	}
	if matchErr := ensureMatchingContentDriveID(driveID, contentDriveID); matchErr != nil {
		return nil, false, matchErr
	}

	row, err := scanRemoteStateRowWithQuerier(
		contentDriveID,
		func(dest ...any) error {
			return s.db.QueryRowContext(ctx, query, arg).Scan(dest...)
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
func (s *SyncStore) GetRemoteStateByPath(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (*remoteStateRow, bool, error) {
	return s.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByPath, path, "GetRemoteStateByPath")
}

// GetRemoteStateByID looks up the exact remote_state row for an item ID.
func (s *SyncStore) GetRemoteStateByID(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) (*remoteStateRow, bool, error) {
	return s.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByID, itemID, "GetRemoteStateByID")
}

func scanRemoteStateRowWithQuerier(
	fallbackDriveID driveid.ID,
	scan func(dest ...any) error,
) (*remoteStateRow, error) {
	var (
		rawDriveID string
		row        remoteStateRow
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	if err := scan(
		&rawDriveID, &row.ItemID, &row.Path, &row.ItemType,
		&hash, &size, &mtime, &etag,
	); err != nil {
		return nil, err
	}

	row.DriveID = remoteStateDriveID(rawDriveID, fallbackDriveID)
	row.Hash = hash.String
	row.ETag = etag.String

	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row, nil
}

func remoteStateDriveID(rawDriveID string, fallback driveid.ID) driveid.ID {
	if rawDriveID != "" {
		return driveid.New(rawDriveID)
	}

	return fallback
}
