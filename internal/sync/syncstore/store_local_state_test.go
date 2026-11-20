package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.1.3
func TestReplaceLocalState_ReplacesWholeSnapshot(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "alpha.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-a",
			Size:     1,
			Mtime:    11,
		},
		{
			Path:     "folder",
			ItemType: syncdomain.ItemTypeFolder,
		},
	}))

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:     "beta.txt",
			ItemType: syncdomain.ItemTypeFile,
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

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{
			Path:             "folder",
			ItemType:         syncdomain.ItemTypeFolder,
			LocalDevice:      101,
			LocalInode:       202,
			LocalHasIdentity: true,
		},
		{
			Path:             "folder/file.txt",
			ItemType:         syncdomain.ItemTypeFile,
			Hash:             "hash-a",
			Size:             12,
			Mtime:            34,
			LocalDevice:      303,
			LocalInode:       404,
			LocalBirthTime:   505,
			LocalHasIdentity: true,
		},
	}))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, syncdomain.LocalStateRow{
		Path:             "folder",
		ItemType:         syncdomain.ItemTypeFolder,
		LocalDevice:      101,
		LocalInode:       202,
		LocalHasIdentity: true,
	}, rows[0])
	assert.Equal(t, syncdomain.LocalStateRow{
		Path:             "folder/file.txt",
		ItemType:         syncdomain.ItemTypeFile,
		Hash:             "hash-a",
		Size:             12,
		Mtime:            34,
		LocalDevice:      303,
		LocalInode:       404,
		LocalBirthTime:   505,
		LocalHasIdentity: true,
	}, rows[1])
}

// Validates: R-2.17.1
func TestReplaceLocalState_BackfillsBaselineIdentityForUnchangedItems(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	// Rows written before baselines recorded identity.
	_, err := store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, local_size, local_mtime) VALUES
			('item-folder', 'folder', 'folder', NULL, NULL, NULL),
			('item-same', 'folder/same.txt', 'file', 'hash-a', 12, 34),
			('item-edited', 'folder/edited.txt', 'file', 'hash-old', 12, 34),
			('item-unhashed', 'folder/unhashed.txt', 'file', NULL, 12, 34),
			('item-swapped', 'swapped', 'folder', NULL, NULL, NULL)`)
	require.NoError(t, err)
	_, err = store.Load(ctx)
	require.NoError(t, err)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "folder", ItemType: syncdomain.ItemTypeFolder, LocalDevice: 1, LocalInode: 10, LocalHasIdentity: true},
		{
			Path: "folder/same.txt", ItemType: syncdomain.ItemTypeFile, Hash: "hash-a", Size: 12, Mtime: 34,
			LocalDevice: 1, LocalInode: 11, LocalHasIdentity: true,
		},
		{
			Path: "folder/edited.txt", ItemType: syncdomain.ItemTypeFile, Hash: "hash-new", Size: 12, Mtime: 34,
			LocalDevice: 1, LocalInode: 12, LocalHasIdentity: true,
		},
		{
			Path: "folder/unhashed.txt", ItemType: syncdomain.ItemTypeFile, Size: 12, Mtime: 34,
			LocalDevice: 1, LocalInode: 13, LocalHasIdentity: true,
		},
		{
			Path: "swapped", ItemType: syncdomain.ItemTypeFile, Hash: "hash-s", Size: 1, Mtime: 2,
			LocalDevice: 1, LocalInode: 14, LocalHasIdentity: true,
		},
	}))

	// The cache reflects the backfill without a reload by the caller.
	cached := store.cachedBaseline()
	require.NotNil(t, cached)
	identity := func(path string) (uint64, bool) {
		entry, ok := cached.GetByPath(path)
		require.True(t, ok, path)
		return entry.LocalInode, entry.LocalHasIdentity
	}

	inode, has := identity("folder")
	assert.True(t, has)
	assert.Equal(t, uint64(10), inode)
	inode, has = identity("folder/same.txt")
	assert.True(t, has)
	assert.Equal(t, uint64(11), inode)
	for _, path := range []string{"folder/edited.txt", "folder/unhashed.txt", "swapped"} {
		_, has = identity(path)
		assert.False(t, has, "%s must not adopt an identity its content was not proven against", path)
	}

	fresh, err := LoadBaselineWithRunner(ctx, store.RawDBForTest(), driveid.ID{})
	require.NoError(t, err)
	folder, ok := fresh.GetByPath("folder")
	require.True(t, ok)
	assert.True(t, folder.LocalHasIdentity)
	assert.Equal(t, uint64(10), folder.LocalInode)
}

// Validates: R-2.8.8
func TestScopedLocalStateMutation_UpsertReadAndDeleteExactPath(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "kept.txt",
		ItemType: syncdomain.ItemTypeFile,
		Hash:     "old",
		Size:     1,
	}}))

	require.NoError(t, store.UpsertLocalStateRows(ctx, []syncdomain.LocalStateRow{
		{
			Path:             "kept.txt",
			ItemType:         syncdomain.ItemTypeFile,
			Hash:             "new",
			Size:             2,
			Mtime:            22,
			LocalDevice:      33,
			LocalInode:       44,
			LocalHasIdentity: true,
		},
		{
			Path:     "added.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "added",
			Size:     3,
		},
	}))

	kept, found, err := store.GetLocalStateByPath(ctx, "kept.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, kept)
	assert.Equal(t, syncdomain.LocalStateRow{
		Path:             "kept.txt",
		ItemType:         syncdomain.ItemTypeFile,
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

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "docs/a.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "docs/nested", ItemType: syncdomain.ItemTypeFolder},
		{Path: "docs/nested/b.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Docs/a.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "docs sibling.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "docs2/a.txt", ItemType: syncdomain.ItemTypeFile},
	}))

	require.NoError(t, store.DeleteLocalStatePrefix(ctx, "docs"))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.LocalStateRow{
		{Path: "Docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "Docs/a.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "docs sibling.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "docs2/a.txt", ItemType: syncdomain.ItemTypeFile},
	}, rows)
}

// Validates: R-2.8.8
func TestDeleteLocalStatePrefix_EscapesSQLWildcards(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: `100%_done`, ItemType: syncdomain.ItemTypeFolder},
		{Path: `100%_done/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `100xxdone/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `100%_done_else/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `slash\folder`, ItemType: syncdomain.ItemTypeFolder},
		{Path: `slash\folder/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `slashXfolder/file.txt`, ItemType: syncdomain.ItemTypeFile},
	}))

	require.NoError(t, store.DeleteLocalStatePrefix(ctx, `100%_done`))
	require.NoError(t, store.DeleteLocalStatePrefix(ctx, `slash\folder`))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	assert.Equal(t, []syncdomain.LocalStateRow{
		{Path: `100%_done_else/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `100xxdone/file.txt`, ItemType: syncdomain.ItemTypeFile},
		{Path: `slashXfolder/file.txt`, ItemType: syncdomain.ItemTypeFile},
	}, rows)
}

// Validates: R-2.8.8
func TestDeleteLocalStatePrefix_UsesCharacterLengthForUnicodePrefix(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{
		{Path: "é", ItemType: syncdomain.ItemTypeFolder},
		{Path: "é/docs", ItemType: syncdomain.ItemTypeFolder},
		{Path: "é/docs/file.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "é/docs/nested", ItemType: syncdomain.ItemTypeFolder},
		{Path: "é/docs/nested/file.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "é/docs2/file.txt", ItemType: syncdomain.ItemTypeFile},
		{Path: "e/docs/file.txt", ItemType: syncdomain.ItemTypeFile},
	}))

	require.NoError(t, store.DeleteLocalStatePrefix(ctx, "é/docs"))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, row.Path)
	}
	assert.ElementsMatch(t, []string{
		"é",
		"é/docs2/file.txt",
		"e/docs/file.txt",
	}, paths)
}

// Validates: R-2.8.8
func TestReplaceLocalState_MarksLocalTruthComplete(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, store.MarkLocalTruthSuspect(ctx, syncdomain.LocalTruthRecoveryDroppedEvents))
	state := readObservationStateForTest(t, store, ctx)
	assert.False(t, state.LocalTruthComplete)
	assert.Equal(t, syncdomain.LocalTruthRecoveryDroppedEvents, state.LocalTruthRecoveryReason)

	require.NoError(t, store.ReplaceLocalState(ctx, []syncdomain.LocalStateRow{{
		Path:     "fresh.txt",
		ItemType: syncdomain.ItemTypeFile,
	}}))

	state = readObservationStateForTest(t, store, ctx)
	assert.True(t, state.LocalTruthComplete)
	assert.Empty(t, state.LocalTruthRecoveryReason)
}

// Validates: R-2.1.3
func TestBuildLocalStateRows_UsesDirectSnapshotRows(t *testing.T) {
	t.Parallel()

	rows := syncobserve.BuildLocalStateRows(syncobserve.ScanResult{
		Rows: []syncdomain.LocalStateRow{
			{
				Path:     "folder",
				ItemType: syncdomain.ItemTypeFolder,
				Mtime:    606,
			},
			{
				Path:     "fresh.txt",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "hash-fresh",
				Size:     40,
				Mtime:    505,
			},
			{
				Path:     "kept.txt",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "hash-kept",
				Size:     10,
				Mtime:    101,
			},
			{
				Path:     "new-name.txt",
				ItemType: syncdomain.ItemTypeFile,
				Hash:     "hash-moved",
				Size:     30,
				Mtime:    404,
			},
		},
	})

	require.Len(t, rows, 4)
	assert.Equal(t, expectedLocalStateRows(), rows)
}

func expectedLocalStateRows() []syncdomain.LocalStateRow {
	return []syncdomain.LocalStateRow{
		{
			Path:     "folder",
			ItemType: syncdomain.ItemTypeFolder,
			Mtime:    606,
		},
		{
			Path:     "fresh.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-fresh",
			Size:     40,
			Mtime:    505,
		},
		{
			Path:     "kept.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-kept",
			Size:     10,
			Mtime:    101,
		},
		{
			Path:     "new-name.txt",
			ItemType: syncdomain.ItemTypeFile,
			Hash:     "hash-moved",
			Size:     30,
			Mtime:    404,
		},
	}
}

// Validates: R-2.8.8
func TestDeleteLocalStatePrefix_SeeksThePrimaryKey(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	plan := queryPlanText(t, store.RawDBForTest(), sqlDeleteLocalStateSubtree, localStateSubtreeArgs("docs")...)

	assert.Regexp(t, `SEARCH local_state USING COVERING INDEX sqlite_autoindex_local_state_\d+ \(path=\?\)`, plan,
		"the delete must find the path itself by the primary key; query plan:\n%s", plan)
	assert.Regexp(t, `SEARCH local_state USING COVERING INDEX sqlite_autoindex_local_state_\d+ \(path>\? AND path<\?\)`, plan,
		"the delete must range-scan the rows below the path; query plan:\n%s", plan)
	assert.NotContains(t, plan, "SCAN local_state", "the delete must not read every row; query plan:\n%s", plan)
}
