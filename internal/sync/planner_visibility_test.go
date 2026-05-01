package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3, R-2.4.1
func TestReplacePlannerVisibleStateTx_PrunesRemoteDescendantsWhenBaselineFolderMissingRemotely(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash)
		VALUES
			('folder-docs', 'docs', 'folder', '', ''),
			('file-docs', 'docs/file.txt', 'file', 'hash', 'hash')`)
	require.NoError(t, err)

	visible := plannerVisibleRowsForTest(t, store, nil, []RemoteStateRow{
		{
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "file-docs",
			Path:     "docs/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
		{
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "file-docs2",
			Path:     "docs2/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
	})

	assert.Equal(t, []string{"docs2/file.txt"}, remoteStatePaths(visible.Remote))
}

// Validates: R-2.1.3, R-2.4.1
func TestReplacePlannerVisibleStateTx_PrunesLocalDescendantsWhenBaselineFolderMissingLocally(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash)
		VALUES
			('folder-docs', 'docs', 'folder', '', ''),
			('file-docs', 'docs/file.txt', 'file', 'hash', 'hash')`)
	require.NoError(t, err)

	visible := plannerVisibleRowsForTest(t, store, []LocalStateRow{
		{
			Path:     "docs/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
		{
			Path:     "docs2/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
	}, nil)

	assert.Equal(t, []string{"docs2/file.txt"}, localStatePaths(visible.Local))
}

func plannerVisibleRowsForTest(
	t *testing.T,
	store *SyncStore,
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
) plannerVisibleRows {
	t.Helper()

	ctx := t.Context()
	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	visible, err := replacePlannerVisibleStateTx(
		ctx,
		tx,
		localRows,
		remoteRows,
		driveid.New(engineTestDriveID),
	)
	require.NoError(t, err)

	return visible
}

func localStatePaths(rows []LocalStateRow) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}
	return paths
}

func remoteStatePaths(rows []RemoteStateRow) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}
	return paths
}
