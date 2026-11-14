package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// The tests here follow a pending-create record when its item or its path
// changes place: a rename online, a move the client commits, a path another
// placeholder takes, a path the scan stops visiting. The record is keyed by
// path and must stay with its item, or end, never stay behind at a path the
// item left.

// Validates: R-5.6.15
//
// A record names an item at a path the full local scan does not visit any
// more (the content filter hides it, or a shortcut root reserves it): this
// engine uploads nothing there, so the record can never be used. One whose
// item no observation of known size ever adopted stayed for good. The step
// after planning ends it. An intent at such a path is kept: its absence from
// the scan proves nothing.
func TestPrunePendingCreates_DropsTheRecordOfAnItemAtAPathTheScanDoesNotVisit(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(testDriveID)
	require.NoError(t, store.RecordPendingCreate(ctx, &PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: "hidden.txt", ParentID: graphRootID,
		CreatedETag: placeholderCreatedETag,
	}))
	require.NoError(t, store.RecordPendingCreateIntent(ctx, &PendingCreate{
		DriveID: driveID, Path: "hidden-intent.txt", ParentID: graphRootID,
	}))
	require.NoError(t, store.ReplaceLocalState(ctx, nil))
	scanned := func(path string) bool { return path != "hidden.txt" && path != "hidden-intent.txt" }

	require.NoError(t, store.prunePendingCreates(ctx, passObservedRemoteThenLocal, scanned))

	requireNoPendingCreateAt(t, store, "hidden.txt", "a record nothing will upload into ends")
	_, found, err := store.PendingCreateAt(ctx, "hidden-intent.txt")
	require.NoError(t, err)
	assert.True(t, found, "an intent at a path the scan does not visit is kept")
}

// Validates: R-5.6.15
//
// The content filter starts hiding the path of a create whose content step
// failed after its placeholder was recorded. Observation still adopts the
// placeholder, the plan removes the hidden baseline row, and that outcome
// ends the record of the item it removes from the baseline.
func TestRunOnce_PlaceholderAtAPathTheFilterHidesLosesItsRecord(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	// A file both sides keep, so no pass sees one side emptied.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("my words\n"), 0o600))

	remote.mu.Lock()
	remote.interruptCreates = true
	remote.mu.Unlock()
	_, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	pending, found, err := eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, pending.ItemID, "the placeholder is recorded")

	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	eng.contentFilter = ContentFilterConfig{IgnoredPaths: []string{"doc.txt"}}
	expireRetryWork(t, eng)
	runScenarioPass(t, eng)

	requireNoPendingCreateAt(t, eng.baseline, "doc.txt", "the record ends with the hidden baseline row")
	_, _, inBaseline := readBaselineRow(t, eng.baseline, pending.ItemID)
	assert.False(t, inBaseline)
}

// Validates: R-5.6.15
//
// An adopted placeholder renamed online onto a path that already holds
// another record (the intent of a create there) cannot take its record
// along: the record at the new path stays, and the renamed item's record
// ends rather than stay at the path the item left, where a create meeting
// 409 would finish into the item at its new path.
func TestCommitObservation_AdoptedPlaceholderRenamedOntoARecordedPathLeavesNoRecordBehind(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	placeholder := adoptTestPlaceholder(t, store)
	require.NoError(t, store.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveID, Path: renamedPath, ParentID: graphRootID,
	}))

	renamed := placeholder
	renamed.Path, renamed.ETag = renamedPath, placeholderMovedETag
	require.NoError(t, store.CommitObservation(t.Context(), []observedItem{renamed}, "t-renamed", driveID))

	requireNoPendingCreateAt(t, store, "doc.txt", "no record stays at the path the item left")
	pending, found, err := store.PendingCreateAt(t.Context(), renamedPath)
	require.NoError(t, err)
	require.True(t, found, "the intent already there stays")
	assert.Empty(t, pending.ItemID)
}

// Validates: R-5.6.15
//
// A new placeholder takes a path whose baseline row names an item OneDrive no
// longer holds there: an adopted placeholder renamed online, whose record
// followed it. Its baseline row is dropped so the new placeholder is
// adopted, and its record goes with it: kept, the record named an item with
// no baseline row, which the next observation would adopt at its new path,
// and with no local file there the plan would delete it online.
func TestCommitObservation_APlaceholderTakingAFreedPathEndsTheRecordOfTheItemThatLeftIt(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	left := adoptTestPlaceholder(t, store)
	left.Path, left.ETag = renamedPath, placeholderMovedETag
	require.NoError(t, store.CommitObservation(t.Context(), []observedItem{left}, "t-renamed", driveID))
	_, followed, err := store.PendingCreateAt(t.Context(), renamedPath)
	require.NoError(t, err)
	require.True(t, followed, "the record followed the renamed item")

	require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: "new-placeholder", DriveID: driveID, Path: "doc.txt", ParentID: graphRootID, CreatedETag: `"n,1"`,
	}))
	require.NoError(t, store.CommitObservation(t.Context(), []observedItem{{
		DriveID: driveID, ItemID: "new-placeholder", ParentID: graphRootID, Path: "doc.txt", ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: `"n,1"`,
	}}, "t-new", driveID))

	_, _, adopted := readBaselineRow(t, store, "new-placeholder")
	require.True(t, adopted, "the new placeholder takes the freed path")
	requireNoPendingCreateAt(t, store, renamedPath, "the record of the item that left the path ends")
	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "new-placeholder", pending.ItemID)
}

// Validates: R-5.6.15
//
// A move the client commits for an adopted placeholder (the local file
// renamed, the item moved online to match, or a folder above it moved)
// takes the record to the item's new path in the same transaction. Left at
// the old path, the record was found by a create there that met 409, which
// uploaded into the item at its new path.
func TestCommitMutation_MoveOutcomeTakesTheRecordOfItsItemAlong(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	// renameOfPlaceholder is the committed move of the adopted placeholder
	// from doc.txt to renamedPath: ActionRemoteMove when this client moved the
	// item online to match a local rename, ActionLocalMove when it moved the
	// local file to match a rename made online.
	renameOfPlaceholder := func(action actionType) *BaselineMutation {
		return &BaselineMutation{
			Action: action, Success: true, Path: renamedPath, OldPath: "doc.txt", DriveID: driveID,
			ItemID: testPlaceholderID, ParentID: graphRootID, ItemType: ItemTypeFile,
			LocalSize: 0, LocalSizeKnown: true, RemoteSize: 0, RemoteSizeKnown: true, ETag: placeholderMovedETag,
		}
	}
	requirePlaceholderRecordAtRenamedPath := func(t *testing.T, store *SyncStore) {
		t.Helper()

		requireNoPendingCreateAt(t, store, "doc.txt", "nothing is left at the old path")
		pending, found, err := store.PendingCreateAt(t.Context(), renamedPath)
		require.NoError(t, err)
		require.True(t, found, "the record follows the item")
		assert.Equal(t, testPlaceholderID, pending.ItemID)
	}

	t.Run("the item itself", func(t *testing.T) {
		t.Parallel()

		store := newTestStore(t)
		adoptTestPlaceholder(t, store)

		require.NoError(t, store.CommitMutation(t.Context(), renameOfPlaceholder(ActionRemoteMove)))

		requirePlaceholderRecordAtRenamedPath(t, store)
	})

	// A rename made online put the item where the baseline holds another
	// adopted placeholder, which the commit displaces from the baseline. That
	// item's record ends, and the moved item's record takes the path: the
	// displaced item's record is dropped before the move looks for a free path,
	// not after it found the path taken.
	t.Run("onto the path of an item that has a record of its own", func(t *testing.T) {
		t.Parallel()

		const displacedID, displacedETag = "displaced", `"d,1"`
		store := newTestStore(t)
		adoptTestPlaceholder(t, store)
		require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
			ItemID: displacedID, DriveID: driveID, Path: renamedPath, ParentID: graphRootID, CreatedETag: displacedETag,
		}))
		require.NoError(t, store.CommitObservation(t.Context(), []observedItem{{
			DriveID: driveID, ItemID: displacedID, ParentID: graphRootID, Path: renamedPath, ItemType: ItemTypeFile,
			Hash: emptyQuickXor, Size: 0, ETag: displacedETag,
		}}, "t-displaced", driveID))
		_, _, adopted := readBaselineRow(t, store, displacedID)
		require.True(t, adopted, "the second placeholder is adopted at the path the move takes")

		require.NoError(t, store.CommitMutation(t.Context(), renameOfPlaceholder(ActionLocalMove)))

		_, _, kept := readBaselineRow(t, store, displacedID)
		require.False(t, kept, "the move takes the path from the item it displaces")
		requirePlaceholderRecordAtRenamedPath(t, store)
	})

	t.Run("a folder above it", func(t *testing.T) {
		t.Parallel()

		store := newStoreWithDocsFolderMovedToPapers(t, "docs/draft.txt")

		for _, moved := range []string{"papers/doc.txt", "papers/draft.txt"} {
			_, found, findErr := store.PendingCreateAt(t.Context(), moved)
			require.NoError(t, findErr)
			assert.True(t, found, "the record below the folder moves with it: %s", moved)
		}
		requireNoPendingCreateAt(t, store, "docs/doc.txt", "nothing is left below the old path")
		requireNoPendingCreateAt(t, store, "docs/draft.txt", "nothing is left below the old path")
	})

	// The place below the new folder path already holds a row. That row is
	// kept, and the row below the old path ends instead of staying under a
	// folder that is no longer there.
	t.Run("a folder above it, onto a path that holds a row", func(t *testing.T) {
		t.Parallel()

		store := newStoreWithDocsFolderMovedToPapers(t, "papers/doc.txt")

		kept, found, findErr := store.PendingCreateAt(t.Context(), "papers/doc.txt")
		require.NoError(t, findErr)
		require.True(t, found, "the row the new path held is kept")
		assert.Empty(t, kept.ItemID, "it is still the intent, not the moved row")
		requireNoPendingCreateAt(t, store, "docs/doc.txt", "nothing is left below the old path")
	})
}

// newStoreWithDocsFolderMovedToPapers is a store whose baseline folder docs
// held the record of a created item at docs/doc.txt and an intent at
// intentPath, after the committed move of that folder to papers.
func newStoreWithDocsFolderMovedToPapers(t *testing.T, intentPath string) *SyncStore {
	t.Helper()

	driveID := driveid.New(testDriveID)
	store := newTestStore(t)
	_, err := store.rawDB().ExecContext(t.Context(), `INSERT INTO baseline (item_id, path, parent_id, item_type, etag)
		VALUES ('folder-docs', 'docs', 'root', 'folder', '"f,1"')`)
	require.NoError(t, err)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: "docs/doc.txt", ParentID: "folder-docs",
		CreatedETag: placeholderCreatedETag,
	}))
	require.NoError(t, store.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveID, Path: intentPath, ParentID: "folder-docs",
	}))
	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action: ActionRemoteMove, Success: true, Path: "papers", OldPath: "docs", DriveID: driveID,
		ItemID: "folder-docs", ParentID: graphRootID, ItemType: ItemTypeFolder, ETag: `"f,2"`,
	}))

	return store
}

// Validates: R-5.6.15, R-2.4.4
//
// An adopted placeholder on a drive that gives empty files no hash, renamed
// online while still empty while the user's content waits to go into it: the
// rename moves the item's eTag and modification time, which are all the
// comparison of a hashless empty file has. The plan follows the rename and
// uploads the user's file into the item by ID; it never reads the moved
// eTag as an edit on both sides, which made a conflict copy.
func TestRunOnce_AdoptedHashlessPlaceholderRenamedOnlineIsFinishedWithoutAConflict(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.hashlessEmptyFiles = true
	// A file both sides keep, so no pass sees one side emptied.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	require.NoError(t, os.WriteFile(filepath.Join(h.syncRoot, "doc.txt"), []byte("my words\n"), 0o600))

	// The create stops after its empty item; a pass that uploads nothing
	// adopts it.
	remote.mu.Lock()
	remote.interruptCreates = true
	remote.mu.Unlock()
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	remote.mu.Lock()
	remote.interruptCreates = false
	remote.mu.Unlock()
	placeholder, ok := remote.itemAt("doc.txt")
	require.True(t, ok)
	_, err = h.eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	_, _, adopted := readBaselineRow(t, h.eng.baseline, placeholder.ID)
	require.True(t, adopted, "the placeholder is adopted")

	// Someone renames the empty item online.
	remote.mu.Lock()
	remote.moveLocked(placeholder.ID, "root", renamedPath)
	renamed := remote.items[placeholder.ID]
	renamed.ETag = remote.nextETag()
	renamed.ModifiedAt = renamed.ModifiedAt.Add(time.Hour)
	remote.touch(placeholder.ID)
	remote.mu.Unlock()
	expireRetryWork(t, h.eng)
	h.beginRound(t)
	report := runScenarioPass(t, h.eng)

	assert.Zero(t, report.ConflictCopies, "no conflict copy: %+v", report)
	finished, ok := remote.itemAt(renamedPath)
	require.True(t, ok, "the user's file follows the rename: %v", remote.tree())
	assert.Equal(t, placeholder.ID, finished.ID, "the content went into the placeholder by ID")
	assert.Equal(t, map[string]string{"keep.txt": "keep\n", renamedPath: "my words\n"}, remote.tree())
	assertSyncInvariants(t, h)
}

// Validates: R-5.6.15
//
// The scenario fake creates a new file the way graph.UploadCreating does: a
// zero-byte file is complete as created, and the create hook never runs for
// it. Running it there made every scenario and chaos test record a zero-byte
// create's item, a path production never takes.
func TestMoveScenarioRemote_ZeroByteCreateRunsNoCreateHook(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	hooked := 0
	hook := func(*graph.Item) error {
		hooked++
		return nil
	}

	empty, err := remote.uploadCreating(t, "root", "empty.txt", strings.NewReader(""), 0, hook)
	require.NoError(t, err)
	assert.Zero(t, hooked, "a zero-byte create runs no hook")
	assert.Zero(t, empty.Size)

	_, err = remote.uploadCreating(t, "root", "full.txt", strings.NewReader("words\n"), int64(len("words\n")), hook)
	require.NoError(t, err)
	assert.Equal(t, 1, hooked, "a create with content runs it once")
}
