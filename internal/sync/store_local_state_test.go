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
