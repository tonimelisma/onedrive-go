package sync

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *RemoteStateRow {
	t.Helper()

	var (
		row      RemoteStateRow
		parentID sql.NullString
		hash     sql.NullString
		size     sql.NullInt64
		mtime    sql.NullInt64
		etag     sql.NullString
		prevPath sql.NullString
	)

	configuredDriveID, driveErr := configuredDriveIDForDB(t.Context(), db)
	require.NoError(t, driveErr)

	err := db.QueryRowContext(t.Context(),
		`SELECT item_id, path, parent_id, item_type, hash, size, mtime, etag,
			previous_path
		FROM remote_state WHERE item_id = ?`,
		itemID,
	).Scan(
		&row.ItemID, &row.Path, &parentID, &row.ItemType,
		&hash, &size, &mtime, &etag,
		&prevPath,
	)
	if err == sql.ErrNoRows {
		return nil
	}

	require.NoError(t, err)

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

	return &row
}

func readObservationCursor(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()

	var token string
	err := db.QueryRowContext(t.Context(),
		`SELECT cursor FROM observation_state WHERE singleton_id = 1 AND configured_drive_id = ?`,
		driveID,
	).Scan(&token)
	if err == sql.ErrNoRows {
		return ""
	}

	require.NoError(t, err)
	return token
}

// Validates: R-2.2
func TestCommitObservation_NewItemCreatesMirrorRowAndToken(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	err := mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		ParentID: "root",
		Path:     "hello.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
		Size:     100,
		Mtime:    1000000,
		ETag:     "etag1",
	}}, "delta-token-1", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "hello.txt", row.Path)
	assert.Equal(t, "hash1", row.Hash)
	assert.Equal(t, int64(100), row.Size)
	assert.Equal(t, "etag1", row.ETag)
	assert.Equal(t, "delta-token-1", readObservationCursor(t, mgr.DB(), testDriveID))
}

// Validates: R-2.2
func TestCommitObservation_DeleteRemovesMirrorRow(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		ParentID: "root",
		Path:     "hello.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
	}}, "delta-token-1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:   driveID,
		ItemID:    "item1",
		Path:      "hello.txt",
		ItemType:  ItemTypeFile,
		IsDeleted: true,
	}}, "delta-token-2", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.DB(), "item1"))
	assert.Equal(t, "delta-token-2", readObservationCursor(t, mgr.DB(), testDriveID))
}

// Validates: R-2.2
func TestCommitObservation_MoveUpdatesPreviousPath(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		ParentID: "root",
		Path:     "old.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
	}}, "delta-token-1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		ParentID: "root",
		Path:     "new.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
	}}, "delta-token-2", driveID))

	row := readRemoteStateRow(t, mgr.DB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "new.txt", row.Path)
	assert.Equal(t, "old.txt", row.PreviousPath)
}
