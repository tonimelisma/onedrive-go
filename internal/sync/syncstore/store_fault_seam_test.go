package syncstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
func newFaultStore(t *testing.T) (*SyncStore, *syncfixture.SqlFault) {
	t.Helper()

	registerFaultDriver(t)
	dbPath := filepath.Join(t.TempDir(), "fault.db")
	store, err := OpenSyncStoreWithDriverForTest(synctest.TestContext(t), dbPath, synctest.TestLogger(t), true, syncfixture.FaultDriverName)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, store.Close(context.Background())) })

	return store, syncfixture.FaultForPath(dbPath)
}

func faultSeedOutcome(path, itemID, localHash string) *syncdomain.BaselineMutation {
	return syncdomain.MutationFromActionOutcome(&syncdomain.ActionOutcome{
		Action: syncdomain.ActionDownload, Success: true, Path: path,
		DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: itemID,
		ParentID: "root", ItemType: syncdomain.ItemTypeFile,
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
		{name: "commit", match: syncfixture.FaultCommit, path: "c.txt", item: "item-c"},
	} {
		fault.Arm(tc.match, syncfixture.ErrInjectedFault)
		err := store.CommitMutation(ctx, faultSeedOutcome(tc.path, tc.item, "hash-"+tc.item))
		require.ErrorIs(t, err, syncfixture.ErrInjectedFault, tc.name)
		require.Equal(t, 1, fault.Fired(), "%s: the fault fired once and disarmed", tc.name)
		fault.Hits = 0

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

	fault.Arm(syncfixture.FaultCommit, syncfixture.ErrInjectedFault)
	err := store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: "item-a", ItemType: syncdomain.ItemTypeFile,
		LocalHash: "hash-new", LocalSize: 101, LocalSizeKnown: true, LocalMtime: 42,
	})
	require.ErrorIs(t, err, syncfixture.ErrInjectedFault)

	entry, ok := store.baseline.GetByPath("a.txt")
	require.True(t, ok)
	assert.Equal(t, "hash-old", entry.LocalHash, "the cache was not updated ahead of the failed commit")
	mismatches, checkErr := store.CheckCacheConsistency(ctx)
	require.NoError(t, checkErr)
	assert.Zero(t, mismatches, "the database holds the same old row")

	require.NoError(t, store.RefreshLocalBaseline(ctx, &localBaselineRefresh{
		Path: "a.txt", DriveID: driveid.New(syncfixture.EngineTestDriveID), ItemID: "item-a", ItemType: syncdomain.ItemTypeFile,
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
	require.NoError(t, store.ReplaceHeldDeletes(ctx, []syncdomain.HeldDelete{{Path: "x.txt", Side: syncdomain.DeleteSafetySideRemote, Reason: syncdomain.DeleteSafetyReasonTooMany, ItemType: syncdomain.ItemTypeFile}}))

	fault.Arm("FROM held_deletes", syncfixture.ErrInjectedFault)
	held, err := store.ListHeldDeletes(ctx)
	require.ErrorIs(t, err, syncfixture.ErrInjectedFault)
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
		require.NoError(t, store.UpsertRetryWork(ctx, syncfixture.TestDelayedRetryWorkRow(path, "", syncdomain.ActionUpload, 1, 1)))
	}

	fault.ArmAfter(sqlDeleteRetryWorkByPathType, 1, syncfixture.ErrInjectedFault)
	err := store.PruneRetryWorkToCurrentActions(ctx, nil)
	require.ErrorIs(t, err, syncfixture.ErrInjectedFault)
	require.Equal(t, 1, fault.Fired(), "the second delete failed")

	rows, err := store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Len(t, rows, 3, "no row is gone")

	require.NoError(t, store.PruneRetryWorkToCurrentActions(ctx, nil))
	rows, err = store.ListRetryWork(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows, "the prune completes once the fault is gone")
}
