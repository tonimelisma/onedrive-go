package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.1.3
func TestReplaceLocalState_ReplacesWholeSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:     "alpha.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-a",
			Size:     1,
			Mtime:    11,
		},
		{
			Path:     "folder",
			ItemType: ItemTypeFolder,
		},
	}))

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:     "beta.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-b",
			Size:     2,
			Mtime:    22,
		},
	}))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "beta.txt", rows[0].Path)
	assert.Equal(t, "hash-b", rows[0].Hash)
}

// Validates: R-2.1.3
func TestReplaceLocalState_PersistsFilesystemIdentity(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:             "folder",
			ItemType:         ItemTypeFolder,
			LocalDevice:      101,
			LocalInode:       202,
			LocalHasIdentity: true,
		},
		{
			Path:             "folder/file.txt",
			ItemType:         ItemTypeFile,
			Hash:             "hash-a",
			Size:             12,
			Mtime:            34,
			LocalDevice:      303,
			LocalInode:       404,
			LocalHasIdentity: true,
		},
	}))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, LocalStateRow{
		Path:             "folder",
		ItemType:         ItemTypeFolder,
		LocalDevice:      101,
		LocalInode:       202,
		LocalHasIdentity: true,
	}, rows[0])
	assert.Equal(t, LocalStateRow{
		Path:             "folder/file.txt",
		ItemType:         ItemTypeFile,
		Hash:             "hash-a",
		Size:             12,
		Mtime:            34,
		LocalDevice:      303,
		LocalInode:       404,
		LocalHasIdentity: true,
	}, rows[1])
}

// Validates: R-2.8.8
func TestScopedLocalStateMutation_UpsertReadAndDeleteExactPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "kept.txt",
		ItemType: ItemTypeFile,
		Hash:     "old",
		Size:     1,
	}}))

	require.NoError(t, store.UpsertLocalStateRows(ctx, []LocalStateRow{
		{
			Path:             "kept.txt",
			ItemType:         ItemTypeFile,
			Hash:             "new",
			Size:             2,
			Mtime:            22,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
		{
			Path:     "added.txt",
			ItemType: ItemTypeFile,
			Hash:     "added",
			Size:     3,
		},
	}))

	kept, found, err := store.GetLocalStateByPath(ctx, "kept.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, kept)
	assert.Equal(t, LocalStateRow{
		Path:             "kept.txt",
		ItemType:         ItemTypeFile,
		Hash:             "new",
		Size:             2,
		Mtime:            22,
		LocalDevice:      33,
		LocalInode:       44,
		LocalHasIdentity: true,
	}, *kept)

	require.NoError(t, store.DeleteLocalStatePath(ctx, "kept.txt"))
	kept, found, err = store.GetLocalStateByPath(ctx, "kept.txt")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, kept)

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "added.txt", rows[0].Path)
}

// Validates: R-2.8.8
func TestDeleteLocalStatePrefix_DeletesDirectoryAndDescendantsOnly(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{Path: "docs", ItemType: ItemTypeFolder},
		{Path: "docs/a.txt", ItemType: ItemTypeFile},
		{Path: "docs/nested", ItemType: ItemTypeFolder},
		{Path: "docs/nested/b.txt", ItemType: ItemTypeFile},
		{Path: "Docs", ItemType: ItemTypeFolder},
		{Path: "Docs/a.txt", ItemType: ItemTypeFile},
		{Path: "docs sibling.txt", ItemType: ItemTypeFile},
		{Path: "docs2/a.txt", ItemType: ItemTypeFile},
	}))

	require.NoError(t, store.DeleteLocalStatePrefix(ctx, "docs"))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{
		{Path: "Docs", ItemType: ItemTypeFolder},
		{Path: "Docs/a.txt", ItemType: ItemTypeFile},
		{Path: "docs sibling.txt", ItemType: ItemTypeFile},
		{Path: "docs2/a.txt", ItemType: ItemTypeFile},
	}, rows)
}

// Validates: R-2.8.8
func TestDeleteLocalStatePrefix_EscapesSQLWildcards(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{Path: `100%_done`, ItemType: ItemTypeFolder},
		{Path: `100%_done/file.txt`, ItemType: ItemTypeFile},
		{Path: `100xxdone/file.txt`, ItemType: ItemTypeFile},
		{Path: `100%_done_else/file.txt`, ItemType: ItemTypeFile},
		{Path: `slash\folder`, ItemType: ItemTypeFolder},
		{Path: `slash\folder/file.txt`, ItemType: ItemTypeFile},
		{Path: `slashXfolder/file.txt`, ItemType: ItemTypeFile},
	}))

	require.NoError(t, store.DeleteLocalStatePrefix(ctx, `100%_done`))
	require.NoError(t, store.DeleteLocalStatePrefix(ctx, `slash\folder`))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	assert.Equal(t, []LocalStateRow{
		{Path: `100%_done_else/file.txt`, ItemType: ItemTypeFile},
		{Path: `100xxdone/file.txt`, ItemType: ItemTypeFile},
		{Path: `slashXfolder/file.txt`, ItemType: ItemTypeFile},
	}, rows)
}

// Validates: R-2.8.8
func TestReplaceLocalState_MarksLocalTruthComplete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.MarkLocalTruthSuspect(ctx, LocalTruthRecoveryDroppedEvents))
	state := readObservationStateForTest(t, store, ctx)
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{{
		Path:     "fresh.txt",
		ItemType: ItemTypeFile,
	}}))

	state = readObservationStateForTest(t, store, ctx)
	assert.True(t, state.LocalTruthComplete)
	assert.Empty(t, state.LocalTruthRecoveryReason)
}

// Validates: R-2.1.3
func TestBuildLocalStateRows_UsesDirectSnapshotRows(t *testing.T) {
	t.Parallel()

	rows := buildLocalStateRows(ScanResult{
		Rows: []LocalStateRow{
			{
				Path:     "folder",
				ItemType: ItemTypeFolder,
				Mtime:    606,
			},
			{
				Path:     "fresh.txt",
				ItemType: ItemTypeFile,
				Hash:     "hash-fresh",
				Size:     40,
				Mtime:    505,
			},
			{
				Path:     "kept.txt",
				ItemType: ItemTypeFile,
				Hash:     "hash-kept",
				Size:     10,
				Mtime:    101,
			},
			{
				Path:     "new-name.txt",
				ItemType: ItemTypeFile,
				Hash:     "hash-moved",
				Size:     30,
				Mtime:    404,
			},
		},
	})

	require.Len(t, rows, 4)
	assert.Equal(t, expectedLocalStateRows(), rows)
}

func expectedLocalStateRows() []LocalStateRow {
	return []LocalStateRow{
		{
			Path:     "folder",
			ItemType: ItemTypeFolder,
			Mtime:    606,
		},
		{
			Path:     "fresh.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-fresh",
			Size:     40,
			Mtime:    505,
		},
		{
			Path:     "kept.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-kept",
			Size:     10,
			Mtime:    101,
		},
		{
			Path:     "new-name.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-moved",
			Size:     30,
			Mtime:    404,
		},
	}
}
