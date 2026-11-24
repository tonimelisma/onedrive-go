// Remote mirror reads of the sync store.
//
// Contents:
//   - ListRemoteState:                current remote mirror rows
//   - GetRemoteStateByPath:           point lookup by path
//   - GetRemoteStateByID:             point lookup by item ID
//   - queryRemoteStateRowsWithRunner: shared multi-row remote_state scanner
//   - scanRemoteStateRowWithQuerier:  shared single-row remote_state scanner

package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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

	sqlListRemoteState = `SELECT ` + sqlSelectRemoteStateCols + ` FROM remote_state`
)

// ListRemoteState returns the current remote mirror rows.
func (s *SyncStore) ListRemoteState(ctx context.Context) ([]syncdomain.RemoteStateRow, error) {
	contentDriveID, err := s.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("syncstore: reading content drive for remote_state: %w", err)
	}

	return queryRemoteStateRowsWithRunner(ctx, s.db, sqlListRemoteState, contentDriveID)
}

func queryRemoteStateRowsWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
	query string,
	contentDriveID driveid.ID,
) ([]syncdomain.RemoteStateRow, error) {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("syncstore: querying remote_state: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var result []syncdomain.RemoteStateRow

	for rows.Next() {
		var (
			rawDriveID string
			row        syncdomain.RemoteStateRow
			hash       sql.NullString
			size       sql.NullInt64
			mtime      sql.NullInt64
			etag       sql.NullString
		)

		if err := rows.Scan(
			&rawDriveID, &row.ItemID, &row.Path, &row.ItemType,
			&hash, &size, &mtime, &etag,
		); err != nil {
			return nil, fmt.Errorf("syncstore: scanning remote_state row: %w", err)
		}

		row.DriveID = remoteStateDriveID(rawDriveID, contentDriveID)
		row.Hash = hash.String
		row.ETag = etag.String

		if size.Valid {
			row.Size = size.Int64
			row.SizeKnown = true
		}
		if mtime.Valid {
			row.Mtime = mtime.Int64
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("syncstore: iterating remote_state rows: %w", err)
	}

	return result, nil
}

func (s *SyncStore) getRemoteStateRow(
	ctx context.Context,
	driveID driveid.ID,
	query string,
	arg string,
	contextLabel string,
) (*syncdomain.RemoteStateRow, bool, error) {
	contentDriveID, err := s.contentDriveIDForRead(ctx, driveID)
	if err != nil {
		return nil, false, fmt.Errorf("syncstore: reading content drive for %s: %w", contextLabel, err)
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
		return nil, false, fmt.Errorf("syncstore: %s %s: %w", contextLabel, arg, err)
	}

	return row, true, nil
}

// GetRemoteStateByPath looks up the current remote_state row for a path.
func (s *SyncStore) GetRemoteStateByPath(
	ctx context.Context,
	path string,
	driveID driveid.ID,
) (*syncdomain.RemoteStateRow, bool, error) {
	return s.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByPath, path, "GetRemoteStateByPath")
}

// GetRemoteStateByID looks up the exact remote_state row for an item ID.
func (s *SyncStore) GetRemoteStateByID(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
) (*syncdomain.RemoteStateRow, bool, error) {
	return s.getRemoteStateRow(ctx, driveID, sqlGetRemoteStateByID, itemID, "GetRemoteStateByID")
}

func scanRemoteStateRowWithQuerier(
	fallbackDriveID driveid.ID,
	scan func(dest ...any) error,
) (*syncdomain.RemoteStateRow, error) {
	var (
		rawDriveID string
		row        syncdomain.RemoteStateRow
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
		row.SizeKnown = true
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
