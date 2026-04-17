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
			Path:            "alpha.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-a",
			Size:            1,
			Mtime:           11,
			ContentIdentity: "hash-a",
			ObservedAt:      100,
		},
		{
			Path:       "folder",
			ItemType:   ItemTypeFolder,
			ObservedAt: 100,
		},
	}))

	require.NoError(t, store.ReplaceLocalState(ctx, []LocalStateRow{
		{
			Path:            "beta.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-b",
			Size:            2,
			Mtime:           22,
			ContentIdentity: "hash-b",
			ObservedAt:      200,
		},
	}))

	rows, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "beta.txt", rows[0].Path)
	assert.Equal(t, "hash-b", rows[0].Hash)
	assert.Equal(t, int64(200), rows[0].ObservedAt)
}

// Validates: R-2.1.3
func TestBuildLocalStateRows_ReconstructsCurrentSnapshotFromBaselineAndScan(t *testing.T) {
	t.Parallel()

	rows := buildLocalStateRows(localStateBaselineFixture(), ScanResult{
		Events: localStateEventFixture(),
	}, 999)

	require.Len(t, rows, 4)
	assert.Equal(t, expectedLocalStateRows(), rows)
}

func localStateBaselineFixture() *Baseline {
	return baselineWith(
		&BaselineEntry{
			Path:           "kept.txt",
			ItemID:         "item-kept",
			ItemType:       ItemTypeFile,
			LocalHash:      "hash-kept",
			LocalSize:      10,
			LocalSizeKnown: true,
			LocalMtime:     101,
		},
		&BaselineEntry{
			Path:           "deleted.txt",
			ItemID:         "item-deleted",
			ItemType:       ItemTypeFile,
			LocalHash:      "hash-deleted",
			LocalSize:      20,
			LocalSizeKnown: true,
			LocalMtime:     202,
		},
		&BaselineEntry{
			Path:           "old-name.txt",
			ItemID:         "item-moved",
			ItemType:       ItemTypeFile,
			LocalHash:      "hash-moved",
			LocalSize:      30,
			LocalSizeKnown: true,
			LocalMtime:     303,
		},
	)
}

func localStateEventFixture() []ChangeEvent {
	return []ChangeEvent{
		{
			Source:   SourceLocal,
			Type:     ChangeDelete,
			Path:     "deleted.txt",
			ItemType: ItemTypeFile,
		},
		{
			Source:   SourceLocal,
			Type:     ChangeMove,
			Path:     "new-name.txt",
			OldPath:  "old-name.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-moved",
			Size:     30,
			Mtime:    404,
		},
		{
			Source:   SourceLocal,
			Type:     ChangeCreate,
			Path:     "fresh.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash-fresh",
			Size:     40,
			Mtime:    505,
		},
		{
			Source:   SourceLocal,
			Type:     ChangeCreate,
			Path:     "folder",
			ItemType: ItemTypeFolder,
			Mtime:    606,
		},
	}
}

func expectedLocalStateRows() []LocalStateRow {
	return []LocalStateRow{
		{
			Path:       "folder",
			ItemType:   ItemTypeFolder,
			ObservedAt: 999,
			Mtime:      606,
		},
		{
			Path:            "fresh.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-fresh",
			Size:            40,
			Mtime:           505,
			ContentIdentity: "hash-fresh",
			ObservedAt:      999,
		},
		{
			Path:            "kept.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-kept",
			Size:            10,
			Mtime:           101,
			ContentIdentity: "hash-kept",
			ObservedAt:      999,
		},
		{
			Path:            "new-name.txt",
			ItemType:        ItemTypeFile,
			Hash:            "hash-moved",
			Size:            30,
			Mtime:           404,
			ContentIdentity: "hash-moved",
			ObservedAt:      999,
		},
	}
}
