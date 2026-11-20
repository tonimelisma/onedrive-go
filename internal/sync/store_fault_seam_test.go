package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A fault-injecting database/sql driver around the real SQLite driver. A
// store opened through it behaves exactly like production until a test arms
// one failure: the next statement containing a SQL fragment, or the next
// transaction commit, fails with the armed error. Every
// `fmt.Errorf("sync: …: %w")` branch of the store was otherwise unreachable,
// and the invariant that a failed write leaves the database and the
// in-memory baseline cache agreeing was unproven.

// newFaultStore opens a fresh store through the fault driver and returns the
// fault that governs its connections.
func newFaultStore(t *testing.T) (*SyncStore, *sqlFault) {
	t.Helper()

	registerFaultDriver(t)
	dbPath := filepath.Join(t.TempDir(), "fault.db")
	store, err := OpenSyncStoreWithDriverForTest(synctest.TestContext(t), dbPath, synctest.TestLogger(t), true, faultDriverName)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.Background())) })

	return store, faultForPath(dbPath)
}

func faultSeedOutcome(path, itemID, localHash string) *BaselineMutation {
	return MutationFromActionOutcome(&ActionOutcome{
		Action: ActionDownload, Success: true, Path: path,
		DriveID: driveid.New(engineTestDriveID), ItemID: itemID,
		ParentID: "root", ItemType: ItemTypeFile,
		LocalHash: localHash, RemoteHash: localHash,
		LocalSize: 100, LocalSizeKnown: true, RemoteSize: 100, RemoteSizeKnown: true,
	})
}

// Validates: R-6.5.1, R-6.5.2
//
// A statement failing inside an outcome commit, and a commit itself failing,
// each leave the database and the in-memory baseline cache agreeing, with
// the failed outcome in neither; the same outcome commits once the fault is
// gone, so nothing is lost and nothing is double-applied.
func TestFault_CommitMutationFailureLeavesCacheAndDatabaseAgreeing(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome("a.txt", "item-a", "hash-a")))

	for _, tc := range []struct {
		name  string
		match string
		path  string
		item  string
	}{
		{name: "statement", match: "INTO baseline", path: "b.txt", item: "item-b"},
		{name: "commit", match: faultCommit, path: "c.txt", item: "item-c"},
	} {
		fault.arm(tc.match, errInjectedFault)
		err := store.CommitMutation(ctx, faultSeedOutcome(tc.path, tc.item, "hash-"+tc.item))
		require.ErrorIs(t, err, errInjectedFault, tc.name)
		require.Equal(t, 1, fault.fired(), "%s: the fault fired once and disarmed", tc.name)
		fault.hits = 0

		mismatches, checkErr := store.CheckCacheConsistency(ctx)
		require.NoError(t, checkErr, tc.name)
		assert.Zero(t, mismatches, "%s: cache and database agree after the failed commit", tc.name)
		_, cached := store.baseline.GetByPath(tc.path)
		assert.False(t, cached, "%s: the failed outcome is not in the cache", tc.name)

		require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome(tc.path, tc.item, "hash-"+tc.item)), tc.name)
		entry, ok := store.baseline.GetByPath(tc.path)
		require.True(t, ok, "%s: the retried outcome lands", tc.name)
		assert.Equal(t, "hash-"+tc.item, entry.LocalHash)
		mismatches, checkErr = store.CheckCacheConsistency(ctx)
		require.NoError(t, checkErr)
		assert.Zero(t, mismatches, tc.name)
	}
}

// Validates: R-6.5.2
//
// A local-baseline refresh whose commit fails leaves the old row in both the
// database and the cache: no half-refreshed row, no cache ahead of the disk.
func TestFault_RefreshLocalBaselineCommitFailureLeavesTheOldRow(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.CommitMutation(ctx, faultSeedOutcome("a.txt", "item-a", "hash-old")))

	fault.arm(faultCommit, errInjectedFault)
	err := store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "item-a", ItemType: ItemTypeFile,
		LocalHash: "hash-new", LocalSize: 101, LocalSizeKnown: true, LocalMtime: 42,
	})
	require.ErrorIs(t, err, errInjectedFault)

	entry, ok := store.baseline.GetByPath("a.txt")
	require.True(t, ok)
	assert.Equal(t, "hash-old", entry.LocalHash, "the cache was not updated ahead of the failed commit")
	mismatches, checkErr := store.CheckCacheConsistency(ctx)
	require.NoError(t, checkErr)
	assert.Zero(t, mismatches, "the database holds the same old row")

	require.NoError(t, store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(engineTestDriveID), ItemID: "item-a", ItemType: ItemTypeFile,
		LocalHash: "hash-new", LocalSize: 101, LocalSizeKnown: true, LocalMtime: 42,
	}))
	entry, _ = store.baseline.GetByPath("a.txt")
	assert.Equal(t, "hash-new", entry.LocalHash)
}

// Validates: R-6.5.2
//
// A failed read is returned, not answered with an empty result: a store that
// cannot read its held deletes must not report "nothing held".
func TestFault_ReadFailureIsReturnedNotEmpty(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	require.NoError(t, store.ReplaceHeldDeletes(ctx, []HeldDelete{{Path: "x.txt", Side: DeleteSafetySideRemote, Reason: DeleteSafetyReasonTooMany, ItemType: ItemTypeFile}}))

	fault.arm("FROM held_deletes", errInjectedFault)
	held, err := store.ListHeldDeletes(ctx)
	require.ErrorIs(t, err, errInjectedFault)
	assert.Nil(t, held)

	held, err = store.ListHeldDeletes(ctx)
	require.NoError(t, err)
	assert.Len(t, held, 1, "the read works again once the fault is gone")
}

// Validates: R-2.5.2
//
// A retry-work prune whose delete fails in the middle of its run deletes
// nothing: the rows it had already deleted come back with the rollback.
func TestFault_RetryWorkPruneThatFailsMidwayDeletesNothing(t *testing.T) {
	t.Parallel()

	store, fault := newFaultStore(t)
	ctx := t.Context()
	for _, path := range []string{"a.txt", "b.txt", "c.txt"} {
		require.NoError(t, store.UpsertRetryWork(ctx, testDelayedRetryWorkRow(path, "", ActionUpload, 1, 1)))
	}

	fault.armAfter(sqlDeleteRetryWorkByPathType, 1, errInjectedFault)
	err := store.PruneRetryWorkToCurrentActions(ctx, nil)
	require.ErrorIs(t, err, errInjectedFault)
	require.Equal(t, 1, fault.fired(), "the second delete failed")

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "no row is gone")

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, nil))
	rows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "the prune completes once the fault is gone")
}
