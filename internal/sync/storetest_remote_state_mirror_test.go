package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *RemoteStateRow {
	t.Helper()

	var (
		rawDriveID string
		row        RemoteStateRow
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		`SELECT `+SqlSelectRemoteStateCols+`
		FROM remote_state WHERE item_id = ?`,
		itemID,
	).Scan(
		&rawDriveID, &row.ItemID, &row.Path, &row.ItemType,
		&hash, &size, &mtime, &etag,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err)

	row.DriveID = RemoteStateDriveID(rawDriveID, driveid.ID{})
	row.Hash = hash.String
	row.ETag = etag.String
	if size.Valid {
		row.Size = size.Int64
	}
	if mtime.Valid {
		row.Mtime = mtime.Int64
	}

	return &row
}
