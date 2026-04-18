// Package sync persists sync baseline, observation, failure, scope-block, and metadata state.
//
// sync_failures read paths stay separate from mutation paths so query-heavy
// status/retry helpers do not hide behind write-prefixed filenames.
package sync

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// ListSyncFailures returns all sync_failures rows ordered by last_seen_at DESC.
func (m *SyncStore) ListSyncFailures(ctx context.Context) ([]SyncFailureRow, error) {
	configuredDriveID, err := m.configuredDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("sync: reading configured drive for sync failures: %w", err)
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows, configuredDriveID)
}

type syncFailureScanner interface {
	Scan(dest ...any) error
}

func scanSyncFailureRow(scanner syncFailureScanner, row *SyncFailureRow, configuredDriveID driveid.ID) error {
	if row == nil {
		return fmt.Errorf("sync: scanning sync failure row: nil destination")
	}

	var wireScopeKey string
	if err := scanner.Scan(
		&row.Path, &row.Direction, &row.ActionType, &row.Role, &row.Category,
		&row.IssueType, &row.ItemID,
		&row.FailureCount, &row.NextRetryAt,
		&row.LastError, &row.HTTPStatus,
		&row.FirstSeenAt, &row.LastSeenAt,
		&row.FileSize, &row.LocalHash,
		&wireScopeKey,
	); err != nil {
		return fmt.Errorf("sync: scanning sync failure row: %w", err)
	}

	row.DriveID = configuredDriveID
	row.ScopeKey = ParseScopeKey(wireScopeKey)
	return nil
}

// scanSyncFailureRows scans multiple sync_failures rows from a query result.
func scanSyncFailureRows(rows *sql.Rows, configuredDriveID driveid.ID) ([]SyncFailureRow, error) {
	var result []SyncFailureRow

	for rows.Next() {
		var r SyncFailureRow
		if scanErr := scanSyncFailureRow(rows, &r, configuredDriveID); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning sync failure row: %w", scanErr)
		}
		result = append(result, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating sync failure rows: %w", err)
	}

	return result, nil
}
