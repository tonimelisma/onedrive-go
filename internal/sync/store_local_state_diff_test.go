package sync

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installLocalStateWriteCounter counts every row written to or deleted from
// local_state, across all pooled connections, through ordinary triggers.
func installLocalStateWriteCounter(t *testing.T, store *SyncStore) func() int {
	t.Helper()

	ctx := t.Context()
	for _, stmt := range []string{
		`CREATE TABLE test_local_state_writes (n INTEGER NOT NULL)`,
		`INSERT INTO test_local_state_writes (n) VALUES (0)`,
		`CREATE TRIGGER test_count_local_state_insert AFTER INSERT ON local_state
			BEGIN UPDATE test_local_state_writes SET n = n + 1; END`,
		`CREATE TRIGGER test_count_local_state_update AFTER UPDATE ON local_state
			BEGIN UPDATE test_local_state_writes SET n = n + 1; END`,
		`CREATE TRIGGER test_count_local_state_delete AFTER DELETE ON local_state
			BEGIN UPDATE test_local_state_writes SET n = n + 1; END`,
	} {
		_, err := store.db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	return func() int {
		var n int
		require.NoError(t, store.db.QueryRowContext(context.Background(),
			`SELECT n FROM test_local_state_writes`).Scan(&n))
		return n
	}
}

func localSnapshotRows() []localStateRow {
	return []localStateRow{
		{Path: "docs", ItemType: ItemTypeFolder, LocalDevice: 1, LocalInode: 10, LocalHasIdentity: true},
		{Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "ha", Size: 3, Mtime: 1_700_000_000_000_000_000, LocalDevice: 1, LocalInode: 11, LocalHasIdentity: true},
		{Path: "docs/b.txt", ItemType: ItemTypeFile, Hash: "hb", Size: 4, Mtime: 1_700_000_001_000_000_000, LocalDevice: 1, LocalInode: 12, LocalHasIdentity: true},
		{Path: "empty.txt", ItemType: ItemTypeFile, Size: 0},
	}
}

// Validates: R-2.8.17
func TestReplaceLocalState_UnchangedSnapshotWritesNoRows(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.ReplaceLocalState(ctx, localSnapshotRows()))
	writes := installLocalStateWriteCounter(t, store)

	changed, err := store.ReplaceLocalStateReportingChange(ctx, localSnapshotRows())
	require.NoError(t, err)

	assert.False(t, changed)
	assert.Zero(t, writes(), "an idle safety scan must not rewrite local_state")
}

// Validates: R-2.8.17
func TestReplaceLocalState_WritesOnlyTheDifference(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.ReplaceLocalState(ctx, localSnapshotRows()))
	writes := installLocalStateWriteCounter(t, store)

	next := localSnapshotRows()
	next[1].Hash = "ha2" // changed
	next = next[:3]      // empty.txt removed
	next = append(next, localStateRow{Path: "new.txt", ItemType: ItemTypeFile, Hash: "hn", Size: 1})

	changed, err := store.ReplaceLocalStateReportingChange(ctx, next)
	require.NoError(t, err)
	assert.True(t, changed)
	// One REPLACE (counted as one insert; SQLite fires delete triggers for
	// REPLACE only with recursive_triggers), one insert, one delete.
	assert.LessOrEqual(t, writes(), 4, "only the changed, added and removed rows are written")

	got, err := store.ListLocalState(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, next, got, "the stored snapshot equals the new one")
}

// Validates: R-2.8.17
func TestReplaceLocalState_RestoringCompleteTruthReportsChange(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.ReplaceLocalState(ctx, localSnapshotRows()))
	require.NoError(t, store.MarkLocalTruthSuspect(ctx, localTruthRecoveryWatcherError))

	changed, err := store.ReplaceLocalStateReportingChange(ctx, localSnapshotRows())
	require.NoError(t, err)
	assert.True(t, changed, "work held while local truth was suspect must replan once truth is complete again")

	state, err := store.ReadObservationState(ctx)
	require.NoError(t, err)
	assert.True(t, state.LocalTruthComplete)

	changed, err = store.ReplaceLocalStateReportingChange(ctx, localSnapshotRows())
	require.NoError(t, err)
	assert.False(t, changed, "an unchanged snapshot over complete truth is still no change")
}
