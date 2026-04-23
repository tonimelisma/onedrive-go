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
		rawDriveID string
		row        RemoteStateRow
		hash       sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := db.QueryRowContext(t.Context(),
		`SELECT `+sqlSelectRemoteStateCols+`
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

	row.DriveID = remoteStateDriveID(rawDriveID, driveid.ID{})
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

func readObservationCursor(t *testing.T, db *sql.DB, driveID string) string {
	t.Helper()

	var token string
	err := db.QueryRowContext(t.Context(),
		`SELECT cursor FROM observation_state WHERE mount_drive_id = ? LIMIT 1`,
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
		Path:     "hello.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
		Size:     100,
		Mtime:    1000000,
		ETag:     "etag1",
	}}, "delta-token-1", driveID)
	require.NoError(t, err)

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "hello.txt", row.Path)
	assert.Equal(t, "hash1", row.Hash)
	assert.Equal(t, int64(100), row.Size)
	assert.Equal(t, "etag1", row.ETag)
	assert.Equal(t, "delta-token-1", readObservationCursor(t, mgr.rawDB(), testDriveID))
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

	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "item1"))
	assert.Equal(t, "delta-token-2", readObservationCursor(t, mgr.rawDB(), testDriveID))
}

// Validates: R-2.2
func TestCommitObservation_PreservesObservedDriveIDPerRemoteStateRow(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	mountDriveID := driveid.New(testDriveID)
	sharedDriveID := driveid.New("shared-drive-id")

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  sharedDriveID,
		ItemID:   "shared-item",
		Path:     "Shared/inside.txt",
		ItemType: ItemTypeFile,
		Hash:     "shared-hash",
		Size:     64,
		Mtime:    1234,
		ETag:     "shared-etag",
	}}, "delta-token-shared", mountDriveID))

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, mountDriveID, state.MountDriveID)

	row := readRemoteStateRow(t, mgr.rawDB(), "shared-item")
	require.NotNil(t, row)
	assert.Equal(t, sharedDriveID, row.DriveID)
	assert.Equal(t, "Shared/inside.txt", row.Path)

	rows, err := mgr.ListRemoteState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, sharedDriveID, rows[0].DriveID)
}

// Validates: R-2.2
func TestCommitObservation_UnchangedItemDoesNotRewriteStateOrCursor(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		Path:     "same.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
		Size:     100,
		Mtime:    1000000,
		ETag:     "etag1",
	}}, "delta-token-1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		Path:     "same.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
		Size:     100,
		Mtime:    1000000,
		ETag:     "etag1",
	}}, "", driveID))

	row := readRemoteStateRow(t, mgr.rawDB(), "item1")
	require.NotNil(t, row)
	assert.Equal(t, "same.txt", row.Path)
	assert.Equal(t, "delta-token-1", readObservationCursor(t, mgr.rawDB(), testDriveID))
}

// Validates: R-2.2
func TestCommitObservation_DeleteMissingItemOnlyAdvancesCursor(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:   driveID,
		ItemID:    "missing-item",
		Path:      "gone.txt",
		ItemType:  ItemTypeFile,
		IsDeleted: true,
	}}, "delta-token-delete", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "missing-item"))
	assert.Equal(t, "delta-token-delete", readObservationCursor(t, mgr.rawDB(), testDriveID))
}

// Validates: R-2.11
func TestCommitObservation_IgnoresSymmetricJunkRows(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []ObservedItem{{
		DriveID:  driveID,
		ItemID:   "item-junk",
		Path:     ".DS_Store",
		ItemType: ItemTypeFile,
		Hash:     "hash-junk",
	}}, "delta-token-junk", driveID))

	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "item-junk"))
	assert.Equal(t, "delta-token-junk", readObservationCursor(t, mgr.rawDB(), testDriveID))
}
