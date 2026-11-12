package sync

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func readRemoteStateRow(t *testing.T, db *sql.DB, itemID string) *remoteStateRow {
	t.Helper()

	var (
		rawDriveID string
		row        remoteStateRow
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
		`SELECT cursor FROM observation_state WHERE content_drive_id = ? LIMIT 1`,
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

	err := mgr.CommitObservation(ctx, []observedItem{{
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

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		Path:     "hello.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
	}}, "delta-token-1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
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
	contentDriveID := driveid.New(testDriveID)
	sharedDriveID := driveid.New("shared-drive-id")

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
		DriveID:  sharedDriveID,
		ItemID:   "shared-item",
		Path:     "Shared/inside.txt",
		ItemType: ItemTypeFile,
		Hash:     "shared-hash",
		Size:     64,
		Mtime:    1234,
		ETag:     "shared-etag",
	}}, "delta-token-shared", contentDriveID))

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.Equal(t, contentDriveID, state.ContentDriveID)

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

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item1",
		Path:     "same.txt",
		ItemType: ItemTypeFile,
		Hash:     "hash1",
		Size:     100,
		Mtime:    1000000,
		ETag:     "etag1",
	}}, "delta-token-1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
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

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
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
func TestCommitObservation_PersistsJunkRowsAsRawRemoteTruth(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := context.Background()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
		DriveID:  driveID,
		ItemID:   "item-junk",
		Path:     ".DS_Store",
		ItemType: ItemTypeFile,
		Hash:     "hash-junk",
	}}, "delta-token-junk", driveID))

	require.NotNil(t, readRemoteStateRow(t, mgr.rawDB(), "item-junk"))
	assert.Equal(t, "delta-token-junk", readObservationCursor(t, mgr.rawDB(), testDriveID))
}

// Regression: delta reports a renamed folder as one item. Its children kept
// their old remote_state paths, so after the rename was applied locally they
// looked moved back on OneDrive, and sync moved them back into a re-created
// old folder (live: "Moved locally links-renamed/x -> links/x").
//
// Validates: R-2.17.3
func TestCommitObservation_FolderRenameRepathsDescendants(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "dir", Path: "links", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "sub", Path: "links/sub", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "a", Path: "links/a.txt", ItemType: ItemTypeFile, Hash: "ha", Size: 1},
		{DriveID: driveID, ItemID: "b", Path: "links/sub/b.txt", ItemType: ItemTypeFile, Hash: "hb", Size: 1},
		{DriveID: driveID, ItemID: "sib", Path: "links-other/c.txt", ItemType: ItemTypeFile, Hash: "hc", Size: 1},
	}, "t1", driveID))

	// The next delta carries only the folder.
	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "dir", Path: "links-renamed", ItemType: ItemTypeFolder, ETag: "e2"},
	}, "t2", driveID))

	for id, want := range map[string]string{
		"dir": "links-renamed",
		"sub": "links-renamed/sub",
		"a":   "links-renamed/a.txt",
		"b":   "links-renamed/sub/b.txt",
		"sib": "links-other/c.txt",
	} {
		row := readRemoteStateRow(t, mgr.rawDB(), id)
		require.NotNil(t, row, id)
		assert.Equal(t, want, row.Path, id)
	}
}

// Regression: delta reports a deleted folder as one item. A child that was
// never downloaded (its name was too long for the local filesystem) stayed in
// remote_state, and status showed "Remote changes: 1 item" and "Syncing"
// forever for a folder that no longer existed.
//
// Validates: R-2.17.2
func TestCommitObservation_FolderDeleteRemovesDescendants(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "dir", Path: "probe", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "sub", Path: "probe/len", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "kid", Path: "probe/len/long.txt", ItemType: ItemTypeFile, Hash: "h", Size: 1},
		{DriveID: driveID, ItemID: "sib", Path: "probe-other/keep.txt", ItemType: ItemTypeFile, Hash: "h2", Size: 1},
	}, "t1", driveID))

	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "dir", Path: "probe", ItemType: ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	for _, id := range []string{"dir", "sub", "kid"} {
		assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), id), id)
	}
	require.NotNil(t, readRemoteStateRow(t, mgr.rawDB(), "sib"), "a sibling with the same name prefix stays")
}

// A remote_state row that cannot be read is a failed read, not an absent row.
// Taken for absent, the delete of that item committed as a no-op and its row
// stayed for good; a live observation of it tried to insert it a second time.
//
// Validates: R-2.2
func TestCommitObservation_ARemoteRowThatCannotBeReadFailsTheWindow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		deleted bool
	}{
		{name: "its delete", deleted: true},
		{name: "a change to it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			ctx := t.Context()
			driveID := driveid.New(testDriveID)
			require.NoError(t, mgr.CommitObservation(ctx, []observedItem{{
				DriveID: driveID, ItemID: "item1", Path: "hello.txt", ItemType: ItemTypeFile, Hash: "hash1", Size: 5,
			}}, "delta-token-1", driveID))
			// A size the column can no longer be read back as.
			_, err := mgr.rawDB().ExecContext(ctx, `UPDATE remote_state SET size = 'five' WHERE item_id = 'item1'`)
			require.NoError(t, err)

			err = mgr.CommitObservation(ctx, []observedItem{{
				DriveID: driveID, ItemID: "item1", Path: "hello.txt", ItemType: ItemTypeFile,
				Hash: "hash2", Size: 6, IsDeleted: tc.deleted,
			}}, "delta-token-2", driveID)
			require.ErrorContains(t, err, "reading remote_state of item1")
			assert.Equal(t, "delta-token-1", readObservationCursor(t, mgr.rawDB(), testDriveID), "the window is not committed")
		})
	}
}

// ForgetRemoteItem removes a folder's row with its subtree. A row whose item
// type cannot be read fails the removal: taken for a file, the folder's
// subtree stayed behind.
//
// Validates: R-2.2
func TestForgetRemoteItem_RemovesTheSubtreeAndFailsOnAnUnreadableItemType(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	require.NoError(t, mgr.CommitObservation(ctx, []observedItem{
		{DriveID: driveID, ItemID: "dir", Path: "docs", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "kid", Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "h", Size: 1},
		{DriveID: driveID, ItemID: "odd", Path: "odd", ItemType: ItemTypeFolder},
		{DriveID: driveID, ItemID: "odd-kid", Path: "odd/b.txt", ItemType: ItemTypeFile, Hash: "h2", Size: 1},
	}, "t1", driveID))

	require.NoError(t, mgr.ForgetRemoteItem(ctx, "dir"))
	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "dir"))
	assert.Nil(t, readRemoteStateRow(t, mgr.rawDB(), "kid"), "the subtree goes with the folder")

	db := mgr.rawDB()
	for _, statement := range []string{
		`PRAGMA ignore_check_constraints = ON`,
		`UPDATE remote_state SET item_type = 'bundle' WHERE item_id = 'odd'`,
		`PRAGMA ignore_check_constraints = OFF`,
	} {
		_, err := db.ExecContext(ctx, statement)
		require.NoError(t, err, statement)
	}
	require.ErrorContains(t, mgr.ForgetRemoteItem(ctx, "odd"), `"bundle"`)
	assert.NotNil(t, readRemoteStateRow(t, mgr.rawDB(), "odd-kid"), "nothing is half removed")
}
