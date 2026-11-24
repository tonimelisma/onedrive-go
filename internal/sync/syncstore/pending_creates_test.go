package syncstore

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-5.6.15
func TestCommitObservation_OwnPlaceholderIsAdoptedAsEmptySyncedContent(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t1", driveID))

	// Recorded as synced empty content: the local file reads as changed, so
	// the planner uploads by ID instead of a create_conflict that renamed
	// the user's file and downloaded the empty one.
	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found)
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
}

// Validates: R-5.6.15
//
// A folder replaced by a file: the folder's delete went through on OneDrive
// (its answer lost) and the file's create stopped after the empty item. The
// window that reports both still finds the folder's baseline row at the path,
// which only says what was synced, not what OneDrive holds. Treated as the
// path's occupant, it kept the placeholder from being adopted: the user's
// file became a conflict copy and the empty item was downloaded in its place
// (found by the chaos Graph layer, D-092).
func TestCommitObservation_PlaceholderReplacesTheBaselineOfAnItemOneDriveDeleted(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "folder", ParentID: "root", Path: "doc", ItemType: syncdomain.ItemTypeFolder, ETag: `"f,1"`,
	}}, "t1", driveID))
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, etag) VALUES ('folder', 'doc', 'root', 'folder', '"f,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{
			DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc", ItemType: syncdomain.ItemTypeFile,
			Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,1"`,
		},
		{DriveID: driveID, ItemID: "folder", ParentID: "root", Path: "doc", ItemType: syncdomain.ItemTypeFolder, IsDeleted: true},
	}, "t2", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "the placeholder is adopted")
	assert.Equal(t, "doc", path)
	assert.Zero(t, localSize)
	_, _, folderFound := readBaselineRow(t, mgr, "folder")
	assert.False(t, folderFound, "the deleted folder no longer holds the path")
}

// Validates: R-5.6.15
//
// A file renamed online while edited here: the edit is created as a new file
// at the old name, and the create stops after the empty item. The renamed
// item's baseline row still holds the old name, which OneDrive had freed;
// the placeholder is adopted there, and the renamed item stays in
// remote_state at its new name (chaos Graph layer, D-092, seed 15).
func TestCommitObservation_PlaceholderReplacesTheBaselineOfAnItemMovedAway(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "moved", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: "h", Size: 3, ETag: `"m,1"`,
	}}, "t1", driveID))
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('moved', 'doc.txt', 'root', 'file', 'h', 'h', 3, 3, '"m,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{
		{
			DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
			Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,1"`,
		},
		{
			DriveID: driveID, ItemID: "moved", ParentID: "root", Path: "renamed.txt", ItemType: syncdomain.ItemTypeFile,
			Hash: "h", Size: 3, ETag: `"m,2"`,
		},
	}, "t2", driveID))

	path, _, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "the placeholder is adopted")
	assert.Equal(t, "doc.txt", path)
	_, _, movedFound := readBaselineRow(t, mgr, "moved")
	assert.False(t, movedFound, "the moved item no longer holds the old name")
}

// Validates: R-5.6.15
//
// A baseline row whose item OneDrive still holds keeps the path: the
// placeholder is not adopted over it.
func TestCommitObservation_PlaceholderDoesNotDisplaceALiveBaselineItem(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "live", ParentID: "root", Path: "doc", ItemType: syncdomain.ItemTypeFile,
		Hash: "h", Size: 3, ETag: `"l,1"`,
	}}, "t1", driveID))
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('live', 'doc', 'root', 'file', 'h', 'h', 3, 3, '"l,1"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc", ParentID: "root", CreatedETag: `"p,1"`,
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t2", driveID))

	_, _, found := readBaselineRow(t, mgr, "placeholder")
	assert.False(t, found)
	path, _, liveFound := readBaselineRow(t, mgr, "live")
	require.True(t, liveFound)
	assert.Equal(t, "doc", path)
}

// Validates: R-5.6.15
func TestCommitObservation_PlaceholderStateSeenAfterTheCommitIsNotARemoteChange(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))
	// The upload committed: baseline and remote_state hold the content.
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('item', 'doc.txt', 'file', 'h', 'h', 7, 7, '"p,2"')`)
	require.NoError(t, err)
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h", Size: 7, ETag: `"p,2"`,
	}}, "t1", driveID))
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))

	// A delta page captured mid-upload, applied after the commit.
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile, Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,1"`,
	}}, "t2", driveID))

	row := readRemoteStateRow(t, mgr.db, "item")
	require.NotNil(t, row)
	assert.Equal(t, int64(7), row.Size, "the stale empty state must not overwrite the committed content")
	assert.Equal(t, `"p,2"`, row.ETag)
}

// Validates: R-5.6.15
func TestCommitObservation_PendingCreateIsDroppedOnceTheItemChanges(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "item", DriveID: driveID, Path: "doc.txt", CreatedETag: `"p,1"`,
	}))
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "item", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h", Size: 7, ETag: `"p,2"`,
	}}, "t1", driveID))

	_, found, err := mgr.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found)
}

// Validates: R-5.6.15
//
// Opening and then canceling an upload session on the empty item moves its
// eTag although no content landed (live, e2e
// TestE2E_SyncWatch_FileModification). Matching on the creation eTag, sync
// stopped recognizing its own placeholder: observation dropped the record
// and the item read as someone else's file.
func TestCommitObservation_PlaceholderWhoseETagMovedWithoutContentIsStillAdopted(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"p,2"`,
	}}, "t1", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "placeholder")
	require.True(t, found, "still our empty placeholder: adopted")
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
}

// Validates: R-5.6.15
//
// The creating request was in flight when the process died: OneDrive made
// the empty item after the client was gone, so no hook recorded it (live,
// 2026-11-01). Only the intent, written before the request, knows the empty
// file at that path is ours; observation claims it and adopts it.
func TestCommitObservation_IntentClaimsTheEmptyFileACrashLeftUnrecorded(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{
		DriveID: driveID, Path: "doc.txt", ParentID: "root", LocalSize: 7, LocalHash: "local-hash",
	}))

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "orphan", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"o,1"`,
	}}, "t1", driveID))

	path, localSize, found := readBaselineRow(t, mgr, "orphan")
	require.True(t, found, "the unrecorded placeholder is adopted as synced empty content")
	assert.Equal(t, "doc.txt", path)
	assert.Zero(t, localSize)
	pending, found, err := mgr.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "orphan", pending.ItemID, "the record now names the item")
	assert.Equal(t, `"o,1"`, pending.CreatedETag)
}

// Validates: R-5.6.15
//
// A folder was replaced by a file of its name: sync deleted the folder on
// OneDrive and then created the file, whose answer was lost. The next delta
// reports the folder's deletion and the empty file together, in either
// order. The folder's deletion at the path is not the file the intent
// describes and says nothing of it, so the intent stays and claims the empty
// file. Dropping it on the folder's tombstone made the file read as someone
// else's: the user's file became a conflict copy and the empty one was
// downloaded in its place (chaos seed 63).
func TestCommitObservation_AFolderDeletedAtThePathKeepsTheIntent(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)
	folderGone := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: "old-folder", ParentID: "root", Path: "x", ItemType: syncdomain.ItemTypeFolder,
		IsDeleted: true,
	}
	placeholder := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: "new-file", ParentID: "root", Path: "x", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"n,1"`,
	}
	for name, items := range map[string][]syncdomain.ObservedItem{
		"the tombstone first": {folderGone, placeholder},
		"the file first":      {placeholder, folderGone},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mgr := newTestStore(t)
			require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{
				DriveID: driveID, Path: "x", ParentID: "root", LocalSize: 7, LocalHash: "local-hash",
			}))
			require.NoError(t, mgr.CommitObservation(t.Context(), items, "t1", driveID))

			pending, found, err := mgr.PendingCreateAt(t.Context(), "x")
			require.NoError(t, err)
			require.True(t, found, "the folder's tombstone does not drop the file's intent")
			assert.Equal(t, "new-file", pending.ItemID, "the intent claims the empty file")
		})
	}
}

// Validates: R-5.6.15
//
// An intent claims only an empty file, and only while fresh: a file with
// content at the path is someone's real file, and an intent older than the
// TTL could meet a stranger's empty file long after the attempt. Both drop
// the intent.
func TestCommitObservation_IntentDoesNotClaimContentOrOutliveItsTTL(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(syncfixture.TestDriveID)

	withContent := newTestStore(t)
	require.NoError(t, withContent.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{DriveID: driveID, Path: "doc.txt", ParentID: "root"}))
	require.NoError(t, withContent.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "theirs", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: "their-hash", Size: 9, ETag: `"t,1"`,
	}}, "t1", driveID))
	_, _, found := readBaselineRow(t, withContent, "theirs")
	assert.False(t, found, "a file with content is not adopted")
	_, found, err := withContent.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found, "the intent is dropped: the path holds real content")

	expired := newTestStore(t)
	base := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	expired.nowFunc = func() time.Time { return base }
	require.NoError(t, expired.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{DriveID: driveID, Path: "doc.txt", ParentID: "root"}))
	expired.nowFunc = func() time.Time { return base.Add(syncdomain.PendingCreateIntentTTL + time.Minute) }
	require.NoError(t, expired.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "late", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"l,1"`,
	}}, "t1", driveID))
	_, _, found = readBaselineRow(t, expired, "late")
	assert.False(t, found, "an expired intent claims nothing")
	_, found, err = expired.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.False(t, found, "and is dropped")
}

// Validates: R-5.6.15
//
// A zero-byte file is complete as created, so its create hook never runs and
// only the intent names the path. When that intent outlives the create (the
// create succeeded as a shutdown canceled the attempt, which keeps a canceled
// attempt's intent, or the clear failed), the next observation of the file
// claimed it as a placeholder although the baseline already held it as
// synced: the record then named an empty file that stays empty, and nothing
// ever dropped it. A file the baseline holds is not a lost create's, and the
// intent is dropped instead.
func TestCommitObservation_IntentDoesNotClaimAFileTheBaselineHolds(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{DriveID: driveID, Path: "empty.txt", ParentID: "root"}))
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash, local_size, remote_size, etag)
		VALUES ('created', 'empty.txt', 'root', 'file', ?, ?, 0, 0, '"c,1"')`, syncfixture.EmptyQuickXor, syncfixture.EmptyQuickXor)
	require.NoError(t, err)

	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{{
		DriveID: driveID, ItemID: "created", ParentID: "root", Path: "empty.txt", ItemType: syncdomain.ItemTypeFile,
		Hash: syncfixture.EmptyQuickXor, Size: 0, ETag: `"c,1"`,
	}}, "t1", driveID))

	_, found, err := mgr.PendingCreateAt(t.Context(), "empty.txt")
	require.NoError(t, err)
	assert.False(t, found, "the intent of a create that completed is dropped, not turned into a placeholder record")
	path, _, synced := readBaselineRow(t, mgr, "created")
	require.True(t, synced)
	assert.Equal(t, "empty.txt", path)
}

// createIntentPruneCase is one intent and what the pass that prunes it saw.
type createIntentPruneCase struct {
	name          string
	path          string
	localFile     bool          // the local scan found a file at path
	readDenied    string        // the scan could not read this folder
	remoteDenied  string        // OneDrive refused to list this folder
	suspect       bool          // local truth was marked suspect after the scan
	unscanned     bool          // the content filter or a shortcut root keeps path out of the scan
	localOnlyPass bool          // a watch replan: local truth refreshed, OneDrive not observed
	namesItem     bool          // the row records the item the create made
	prunedAfter   time.Duration // since the intent was recorded
	wantKept      bool
	why           string
}

// Validates: R-5.6.15
//
// An intent without an item that no create will ever retry stayed for good:
// the create ended without an answer, the user renamed the file away, and no
// item appeared at the path (model test, chaos seeds 9 and 11). While fresh
// it could still claim an empty file a stranger made there, and with the
// local file gone the next pass deleted that file. Pruned after planning, such
// an intent is dropped once a pass that observed OneDrive and then a complete
// local scan finds nothing at its path, and any intent past its TTL is dropped
// whatever the pass saw. A path the scan could not see or OneDrive would not
// list, local truth marked suspect, a pass that did not observe OneDrive
// (whose next change batch may still show the create's empty file), and a row
// that names an item all keep the row.
func TestPruneAbandonedCreateIntents_DropsWhatNoCreateWillClaim(t *testing.T) {
	t.Parallel()

	const deniedFolder = "Private"
	fresh, expired := time.Minute, syncdomain.PendingCreateIntentTTL+time.Minute
	for _, tc := range []createIntentPruneCase{
		{
			name: "no file at its path", path: "doc.txt", prunedAfter: fresh,
			why: "nothing at the path will retry the create",
		},
		{
			name: "past its TTL with its file still there and local truth suspect", path: "doc.txt",
			localFile: true, suspect: true, prunedAfter: expired, why: "an expired intent claims nothing",
		},
		{
			name: "past its TTL in a pass that did not observe OneDrive", path: "doc.txt",
			localOnlyPass: true, prunedAfter: expired, why: "expiry needs no observation",
		},
		{
			name: "its file still there", path: "doc.txt", localFile: true, prunedAfter: fresh,
			wantKept: true, why: "the create of that file is retried",
		},
		{
			name: "below a folder the scan could not read", path: deniedFolder + "/doc.txt", readDenied: deniedFolder,
			prunedAfter: fresh, wantKept: true, why: "unreadable is not absent",
		},
		{
			name: "below a folder OneDrive would not list", path: deniedFolder + "/doc.txt", remoteDenied: deniedFolder,
			prunedAfter: fresh, wantKept: true, why: "an unobservable remote path may hold the create's empty file",
		},
		{
			name: "a pass that did not observe OneDrive", path: "doc.txt", localOnlyPass: true, prunedAfter: fresh,
			wantKept: true, why: "the change batch that shows a landed create claims it",
		},
		{
			name: "local truth suspect", path: "doc.txt", suspect: true, prunedAfter: fresh,
			wantKept: true, why: "a suspect scan proves no absence",
		},
		{
			name: "a path the scan does not cover", path: "doc.txt", unscanned: true, prunedAfter: fresh,
			wantKept: true, why: "a path the scan never visits proves no absence",
		},
		{
			name: "a row that names its item", path: "doc.txt", namesItem: true, prunedAfter: expired,
			wantKept: true, why: "a row naming its item at a scanned path ends through observation or an outcome, not its file's absence",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runCreateIntentPruneCase(t, &tc)
		})
	}
}

// runCreateIntentPruneCase records tc's intent, the local and remote truth
// its pass saw, prunes, and checks whether the row is still there.
func runCreateIntentPruneCase(t *testing.T, tc *createIntentPruneCase) {
	t.Helper()

	intendedAt := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	store.nowFunc = func() time.Time { return intendedAt }
	pending := &syncdomain.PendingCreate{DriveID: driveID, Path: tc.path, ParentID: "root"}
	require.NoError(t, store.RecordPendingCreateIntent(ctx, pending))
	if tc.namesItem {
		pending.ItemID, pending.CreatedETag = "placeholder", `"p,1"`
		require.NoError(t, store.RecordPendingCreate(ctx, pending))
	}
	scanned := []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile, Hash: "keep-hash", Size: 5}}
	if tc.localFile {
		scanned = append(scanned, syncdomain.LocalStateRow{Path: tc.path, ItemType: syncdomain.ItemTypeFile, Hash: "my-hash", Size: 9})
	}
	require.NoError(t, store.ReplaceLocalState(ctx, scanned))
	if tc.readDenied != "" {
		seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
			Path: tc.readDenied, DriveID: driveID, IssueType: syncdomain.IssueLocalReadDenied, ScopeKey: syncdomain.SKPermLocalRead(tc.readDenied),
		})
	}
	if tc.remoteDenied != "" {
		seedObservationIssueRowForTest(t, store, &syncdomain.ObservationIssue{
			Path: tc.remoteDenied, DriveID: driveID, IssueType: syncdomain.IssueRemoteReadDenied, ScopeKey: syncdomain.SKPermRemoteRead(tc.remoteDenied),
		})
	}
	if tc.suspect {
		require.NoError(t, store.MarkLocalTruthSuspect(ctx, "watch overflow"))
	}
	store.nowFunc = func() time.Time { return intendedAt.Add(tc.prunedAfter) }
	observed := PassObservedRemoteThenLocal
	if tc.localOnlyPass {
		observed = PassRefreshedLocalOnly
	}

	require.NoError(t, store.PrunePendingCreates(ctx, observed, func(path string) bool {
		return !tc.unscanned || path != tc.path
	}))

	kept, found, err := store.PendingCreateAt(ctx, tc.path)
	require.NoError(t, err)
	require.Equal(t, tc.wantKept, found, tc.why)
	if tc.namesItem {
		assert.Equal(t, "placeholder", kept.ItemID)
	}
}

// Validates: R-5.6.15
//
// An attempt that ends without creating anything (the create itself failed)
// clears its intent, and an intent at the path of a placeholder already
// recorded keeps the placeholder's ID.
func TestPendingCreateIntent_ClearedWhenNothingWasCreatedAndKeepsAKnownItem(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{DriveID: driveID, Path: "a.txt", ParentID: "root"}))
	require.NoError(t, mgr.ClearPendingCreateIntent(t.Context(), "a.txt"))
	_, found, err := mgr.PendingCreateAt(t.Context(), "a.txt")
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "b.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))
	require.NoError(t, mgr.RecordPendingCreateIntent(t.Context(), &syncdomain.PendingCreate{DriveID: driveID, Path: "b.txt", ParentID: "root"}))
	pending, found, err := mgr.PendingCreateAt(t.Context(), "b.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "placeholder", pending.ItemID, "a retry's intent does not forget the placeholder it will upload into")
	require.NoError(t, mgr.ClearPendingCreateIntent(t.Context(), "b.txt"))
	_, found, err = mgr.PendingCreateAt(t.Context(), "b.txt")
	require.NoError(t, err)
	assert.True(t, found, "clearing an intent never drops a recorded item")
}

var _ driveops.ItemReader = (*syncfixture.CreatingTestUploader)(nil)

// Validates: R-5.6.15
//
// On a drive that gives empty files no hash (zero-byte files on Business),
// the eTag and modification time are all the comparison has for an adopted
// placeholder. Both move without content when an upload session is opened
// and canceled on the item, and with the local file changed that read as an
// edit on both sides: the user's file renamed to a conflict copy and the
// empty file downloaded in its place. The adopted row follows them while the
// item stays empty, so the plan is still the upload into it by ID.
func TestCommitObservation_AdoptedHashlessPlaceholderWithMovedETagIsNotAConflict(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 7, LocalHash: "local-hash",
	}))
	createdAt := time.Date(2026, 11, 10, 18, 0, 48, 0, time.UTC)
	placeholder := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Size: 0, ETag: `"p,1"`, Mtime: createdAt.UnixNano(),
	}
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{placeholder}, "t1", driveID))
	_, err := mgr.db.ExecContext(t.Context(), `INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES ('doc.txt', 'file', 'local-hash', 7, 7)`)
	require.NoError(t, err)
	// Cached now, so the check at the end reads the in-memory copy.
	_, err = mgr.Load(t.Context())
	require.NoError(t, err)

	moved := placeholder
	moved.ETag = `"p,2"`
	touched := moved
	touched.ETag, touched.Mtime = `"p,3"`, createdAt.Add(time.Second).UnixNano()
	for i, observed := range []syncdomain.ObservedItem{moved, touched} {
		require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{observed}, fmt.Sprintf("t%d", i+2), driveID))
		rows, queryErr := queryReconciliationState(t.Context(), mgr)
		require.NoError(t, queryErr)
		assert.Equal(t, "upload", reconciliationKindsByPath(rows)["doc.txt"], "observation %d is no remote edit", i)
	}

	bl, err := mgr.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByID("placeholder")
	require.True(t, ok)
	assert.Equal(t, `"p,3"`, entry.ETag, "the in-memory baseline follows the row")
	assert.Zero(t, entry.RemoteSize)
}

// Validates: R-5.6.15, R-2.4.16
//
// Following an adopted placeholder's eTag says "still empty at this eTag",
// and the next upload into it is conditional on exactly that eTag. An
// observation whose size Graph did not report cannot say that: the item may
// hold someone's content by now, so its row keeps the eTag it was adopted
// with and the moved eTag stays visible as a remote change.
func TestCommitObservation_AdoptedPlaceholderOfUnknownSizeIsNotFollowed(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	driveID := driveid.New(syncfixture.TestDriveID)
	require.NoError(t, mgr.RecordPendingCreate(t.Context(), &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "doc.txt", ParentID: "root", CreatedETag: `"p,1"`,
	}))
	placeholder := syncdomain.ObservedItem{
		DriveID: driveID, ItemID: "placeholder", ParentID: "root", Path: "doc.txt", ItemType: syncdomain.ItemTypeFile,
		Size: 0, ETag: `"p,1"`,
	}
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{placeholder}, "t1", driveID))

	sizeless := placeholder
	sizeless.ETag, sizeless.SizeUnknown = `"p,moved"`, true
	require.NoError(t, mgr.CommitObservation(t.Context(), []syncdomain.ObservedItem{sizeless}, "t2", driveID))

	bl, err := mgr.Load(t.Context())
	require.NoError(t, err)
	entry, ok := bl.GetByID("placeholder")
	require.True(t, ok)
	assert.Equal(t, `"p,1"`, entry.ETag, "an item not known to be empty keeps the eTag it was adopted with")
}

// An intent recorded while the clock reads a time before 1970 is a time like
// any other: only zero is "no time".
//
// Validates: R-5.6.15
func TestPendingCreate_AnIntentRecordedBefore1970KeepsItsTimeAndStaysFresh(t *testing.T) {
	t.Parallel()

	mgr := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.TestDriveID)
	intended := time.Date(1969, 7, 20, 20, 17, 0, 0, time.UTC)
	mgr.nowFunc = func() time.Time { return intended }
	require.NoError(t, mgr.RecordPendingCreateIntent(ctx, &syncdomain.PendingCreate{
		DriveID: driveID, Path: "moon.txt", ParentID: "root", LocalSize: 3, LocalHash: "local-hash",
	}))

	mgr.nowFunc = func() time.Time { return intended.Add(time.Hour) }
	require.NoError(t, mgr.RecordPendingCreate(ctx, &syncdomain.PendingCreate{
		ItemID: "placeholder", DriveID: driveID, Path: "moon.txt", ParentID: "root", CreatedETag: `"p,1"`,
		LocalSize: 3, LocalHash: "local-hash",
	}))

	pending, found, err := mgr.PendingCreateAt(ctx, "moon.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, syncdomain.StoredTimeNano(intended), pending.IntendedAt, "the second record keeps the first intent's time")
	assert.True(t, pending.IntentIsFresh(intended.Add(2*time.Hour)), "inside its lifetime the intent is fresh")
	assert.False(t, pending.IntentIsFresh(intended.Add(syncdomain.PendingCreateIntentTTL+time.Second)))
}
