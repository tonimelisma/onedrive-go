package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// The tests here follow a pending-create record to its end: every way a row
// is written has an end that comes once its file can no longer receive this
// client's upload, and no record swallows a remote change or adopts an item.

// testPlaceholderID is the placeholder these tests record, and the eTags it
// has as it is created, then moved, then moved again.
const (
	testPlaceholderID      = "placeholder"
	placeholderCreatedETag = `"p,1"`
	placeholderMovedETag   = `"p,2"`
	placeholderLaterETag   = `"p,3"`
)

// renamedPath is where these tests rename or move a placeholder, and
// folderDeltaToken the token a mount-root test folder's delta returns.
const (
	renamedPath      = "renamed.txt"
	folderDeltaToken = "folder-token"
)

// zeroByteCreatedItem is what graph.UploadCreating returns for a zero-byte
// file: the created item is the whole file, and the create hook never runs.
func zeroByteCreatedItem() *graph.Item {
	return &graph.Item{ID: "empty-item", ETag: `"e,1"`, Size: 0, QuickXorHash: emptyQuickXor}
}

// newFileUploadAction is the planned upload of a new file at path in the
// drive's root.
func newFileUploadAction(path string) *Action {
	return &Action{Type: ActionUpload, Path: path, DriveID: driveid.New(testDriveID), View: &PathView{Path: path}}
}

func requireNoPendingCreateAt(t *testing.T, store *SyncStore, path, why string) {
	t.Helper()

	_, found, err := store.PendingCreateAt(t.Context(), path)
	require.NoError(t, err)
	require.False(t, found, why)
}

// adoptTestPlaceholder records the placeholder of a create at doc.txt and
// has observation adopt it, still empty, as synced empty content.
func adoptTestPlaceholder(t *testing.T, store *SyncStore) ObservedItem {
	t.Helper()

	const path = "doc.txt"
	driveID := driveid.New(testDriveID)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: path, ParentID: graphRootID, CreatedETag: placeholderCreatedETag,
	}))
	placeholder := ObservedItem{
		DriveID: driveID, ItemID: testPlaceholderID, ParentID: graphRootID, Path: path, ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: placeholderCreatedETag,
	}
	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{placeholder}, "t-adopt", driveID))
	_, _, adopted := readBaselineRow(t, store, testPlaceholderID)
	require.True(t, adopted, "the placeholder is adopted")

	return placeholder
}

// Validates: R-5.6.15
//
// A zero-byte file is complete as created: graph runs no create hook for it,
// so only its intent names the path, and the attempt clears the intent when
// it succeeds. A shutdown landing as the create succeeded skipped that clear
// (a canceled context keeps the intent of a create that may still land), so
// the intent outlived a file that was already synced. The clear after a
// success runs detached from the action's context, and the commit of the
// outcome drops whatever record its path or item still has.
func TestUploadNewFile_ZeroByteCreateLeavesNoPendingRow(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{name: "completed"},
		{name: "canceled as it completed", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			actionCtx, cancelAction := context.WithCancel(t.Context())
			defer cancelAction()
			ul := &creatingTestUploader{create: func(context.Context, graph.CreatedFunc) (*graph.Item, error) {
				if tc.canceled {
					cancelAction()
				}
				return zeroByteCreatedItem(), nil
			}}
			_, logger := newDebugLogBuffer()
			e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
			writeExecTestFile(t, syncRoot, "empty.txt", "")

			outcome := e.ExecuteUpload(actionCtx, newFileUploadAction("empty.txt"))
			requireOutcomeSuccess(t, &outcome)
			requireNoPendingCreateAt(t, store, "empty.txt", "the attempt that succeeded clears its intent")

			require.NoError(t, store.CommitMutation(t.Context(), MutationFromActionOutcome(&outcome)))
			requireNoPendingCreateAt(t, store, "empty.txt", "the committed outcome leaves no record")
		})
	}
}

// Validates: R-5.6.15
//
// In watch mode the remote observer can commit a new zero-byte file between
// OneDrive making it and the attempt's clear of its intent. Observation then
// claims the file for the intent, so the row names the item, and the clear,
// which drops only a row that names nothing, left it for good: every later
// change to the empty file was absorbed as the placeholder following its
// eTag. The commit of the zero-byte outcome drops the row naming its item.
func TestUploadNewFile_ZeroByteCreateClaimedByTheObserverLeavesNoPendingRow(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	var store *SyncStore
	ul := &creatingTestUploader{create: func(ctx context.Context, _ graph.CreatedFunc) (*graph.Item, error) {
		created := zeroByteCreatedItem()
		// The change batch that shows the new file is committed before the
		// create's answer is handled.
		if err := store.CommitObservation(ctx, []ObservedItem{{
			DriveID: driveID, ItemID: created.ID, ParentID: graphRootID, Path: "empty.txt", ItemType: ItemTypeFile,
			Hash: created.QuickXorHash, Size: 0, ETag: created.ETag,
		}}, "", driveID); err != nil {
			return nil, err
		}
		return created, nil
	}}
	_, logger := newDebugLogBuffer()
	e, executorStore, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	store = executorStore
	writeExecTestFile(t, syncRoot, "empty.txt", "")

	outcome := e.ExecuteUpload(t.Context(), newFileUploadAction("empty.txt"))
	requireOutcomeSuccess(t, &outcome)
	pending, found, err := store.PendingCreateAt(t.Context(), "empty.txt")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "empty-item", pending.ItemID, "observation claimed the new file for the intent")

	require.NoError(t, store.CommitMutation(t.Context(), MutationFromActionOutcome(&outcome)))
	requireNoPendingCreateAt(t, store, "empty.txt", "the committed zero-byte outcome drops the row naming its item")
}

// Validates: R-5.6.15, R-2.4.4
//
// The whole life of a zero-byte file whose create answer was lost, on a drive
// that gives empty files no hash: the next pass claims the empty file for the
// intent and settles it, and nothing is left recording it. A later change on
// OneDrive that moves only its eTag and modification time is then a remote
// change and is applied. With the record kept, the change was absorbed as the
// placeholder following its eTag, and the local file never learned of it.
func TestRunOnce_ClaimedZeroByteFileThenRemoteMtimeChangeIsARemoteChange(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.hashlessEmptyFiles = true
	// A file both sides keep, so no pass sees one side emptied.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)
	require.NoError(t, os.WriteFile(filepath.Join(h.syncRoot, "empty.txt"), nil, 0o600))

	loseCreateAnswers(remote, true)
	_, err := h.eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	loseCreateAnswers(remote, false)
	created, ok := remote.itemAt("empty.txt")
	require.True(t, ok, "the create landed")
	pending, found, err := h.eng.baseline.PendingCreateAt(t.Context(), "empty.txt")
	require.NoError(t, err)
	require.True(t, found, "only the intent names the path")
	require.Empty(t, pending.ItemID)

	expireRetryWork(t, h.eng)
	runScenarioPass(t, h.eng)
	requireNoPendingCreateAt(t, h.eng.baseline, "empty.txt", "the settled zero-byte file keeps no record")

	remote.mu.Lock()
	touched := remote.items[created.ID]
	touched.ETag = remote.nextETag()
	touched.ModifiedAt = touched.ModifiedAt.Add(time.Hour)
	remote.touch(created.ID)
	remote.mu.Unlock()

	// Both sides still hold the same empty content, so the change is applied
	// as a baseline update; absorbed by a record, the pass had nothing to do.
	report := runScenarioPass(t, h.eng)
	assert.Equal(t, 1, report.Downloads+report.BaselineUpdates, "the remote change is planned and applied: %+v", report)
	h.beginRound(t)
	assertSyncInvariants(t, h)
}

// Validates: R-5.6.15
//
// An adopted placeholder whose local file is empty too has nothing left to
// upload: both sides hold the same empty file, so the pass plans nothing and
// commits nothing, and the record stayed for good, absorbing every later move
// of the item's eTag. The step after planning drops it.
func TestRunOnce_AdoptedPlaceholderWithAnEmptyLocalFileLosesItsRecord(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	// A file both sides keep, so no pass sees one side emptied.
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	h := newSyncHarness(t, remote)
	runScenarioPass(t, h.eng)

	// A zero-byte file's create landed and its answer was lost: OneDrive
	// holds the empty file and only the intent names its path.
	require.NoError(t, os.WriteFile(filepath.Join(h.syncRoot, "empty.txt"), nil, 0o600))
	require.NoError(t, h.eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: remote.driveID, Path: "empty.txt", ParentID: "root",
	}))
	remote.mu.Lock()
	remote.addFile(t, "orphan", "root", "empty.txt", "")
	remote.mu.Unlock()
	h.beginRound(t)

	report := runScenarioPass(t, h.eng)

	assert.Zero(t, report.Uploads+report.Downloads, "both sides hold the same empty file: %+v", report)
	_, _, adopted := readBaselineRow(t, h.eng.baseline, "orphan")
	require.True(t, adopted, "observation claimed and adopted the empty file")
	requireNoPendingCreateAt(t, h.eng.baseline, "empty.txt", "nothing is left to upload into the placeholder")
	assertSyncInvariants(t, h)
}

// Validates: R-5.6.15
//
// The step after planning ends the record of an adopted placeholder only when
// nothing is left to upload into it and no echo can be mistaken: its baseline
// row and the local file at its path hold the same empty content. A local
// file with content still has to go into the item by ID, a local file that is
// gone still has its placeholder to delete, and an upload with content that
// committed still needs the record for the echo of the empty state.
func TestPrunePendingCreates_DropsTheRecordOfASettledEmptyPlaceholder(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		local     *LocalStateRow
		committed bool // an upload with content committed into the item
		wantKept  bool
	}{
		{name: "local file empty", local: &LocalStateRow{Path: "doc.txt", ItemType: ItemTypeFile, Hash: emptyQuickXor}},
		{
			name:  "local file holds content",
			local: &LocalStateRow{Path: "doc.txt", ItemType: ItemTypeFile, Hash: "my-hash", Size: 9}, wantKept: true,
		},
		{name: "local file gone", wantKept: true},
		{
			name:  "upload with content committed",
			local: &LocalStateRow{Path: "doc.txt", ItemType: ItemTypeFile, Hash: emptyQuickXor}, committed: true, wantKept: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			ctx := t.Context()
			adoptTestPlaceholder(t, store)
			if tc.committed {
				require.NoError(t, store.CommitMutation(ctx, &BaselineMutation{
					Action: ActionUpload, Success: true, Path: "doc.txt", DriveID: driveid.New(testDriveID),
					ItemID: testPlaceholderID, ParentID: graphRootID, ItemType: ItemTypeFile,
					LocalHash: "my-hash", RemoteHash: "my-hash", LocalSize: 9, LocalSizeKnown: true,
					RemoteSize: 9, RemoteSizeKnown: true, ETag: placeholderMovedETag,
				}))
			}
			var scanned []LocalStateRow
			if tc.local != nil {
				scanned = append(scanned, *tc.local)
			}
			require.NoError(t, store.ReplaceLocalState(ctx, scanned))

			require.NoError(t, store.PrunePendingCreates(ctx, PassRefreshedLocalOnly, func(string) bool { return true }))

			_, found, err := store.PendingCreateAt(ctx, "doc.txt")
			require.NoError(t, err)
			assert.Equal(t, tc.wantKept, found)
		})
	}
}

// Validates: R-5.6.15
//
// An outcome that removes an item from the baseline ends the record naming
// that item: a delete of the placeholder on either side, or another item's
// row taking its path. The item can no longer receive this client's upload,
// and a record of it would only have an echo of the deleted item adopted.
func TestCommitMutation_DeleteOutcomeDropsTheRecordOfItsItem(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	for _, tc := range []struct {
		name     string
		mutation BaselineMutation
	}{
		{
			name: "deleted on OneDrive",
			mutation: BaselineMutation{
				Action: ActionRemoteDelete, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: testPlaceholderID,
				ItemType: ItemTypeFile,
			},
		},
		{
			name: "deleted locally",
			mutation: BaselineMutation{
				Action: ActionLocalDelete, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: testPlaceholderID,
				ItemType: ItemTypeFile,
			},
		},
		{
			name: "another item takes its path",
			mutation: BaselineMutation{
				Action: ActionDownload, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: "theirs",
				ParentID: graphRootID, ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
				LocalSize: 3, LocalSizeKnown: true, RemoteSize: 3, RemoteSizeKnown: true, ETag: `"t,1"`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			adoptTestPlaceholder(t, store)

			mutation := tc.mutation
			require.NoError(t, store.CommitMutation(t.Context(), &mutation))

			requireNoPendingCreateAt(t, store, "doc.txt", "the item left the baseline, and its record with it")
		})
	}
}

// Validates: R-5.6.15
//
// An upload with content into a placeholder keeps the record: a delta page
// captured mid-upload still shows the empty item, and only the record tells
// that echo from a remote change. Observation drops the record once it sees
// the content.
func TestCommitMutation_UploadWithContentKeepsTheRecordForItsEcho(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	placeholder := adoptTestPlaceholder(t, store)
	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action: ActionUpload, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: testPlaceholderID,
		ParentID: graphRootID, ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 7, LocalSizeKnown: true, RemoteSize: 7, RemoteSizeKnown: true, ETag: placeholderMovedETag,
	}))
	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "kept for the echo")
	require.Equal(t, testPlaceholderID, pending.ItemID)

	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{placeholder}, "t-echo", driveID))
	row := readRemoteStateRow(t, store.rawDB(), testPlaceholderID)
	require.NotNil(t, row)
	assert.Equal(t, int64(7), row.Size, "the echo of the empty state is not a remote change")
}

// Validates: R-5.6.14, R-5.6.15
//
// The failure that names a created item says whether the cleanup removed it.
// A removed item can never receive the content, so the attempt drops its
// record at once, also when a shutdown canceled it: nothing on OneDrive is
// left to recognize. An item the cleanup left in place keeps its record.
func TestUploadNewFile_RemovedPlaceholderLeavesNoRecord(t *testing.T) {
	t.Parallel()

	created := func() *graph.Item { return &graph.Item{ID: "new-item", ETag: `"c,1"`} }
	refused := errors.New("access denied")
	for _, tc := range []struct {
		name     string
		canceled bool
		answer   func(context.Context, graph.CreatedFunc) (*graph.Item, error)
		wantKept bool
	}{
		{
			name: "graph removed it",
			answer: func(_ context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
				item := created()
				if err := onCreated(item); err != nil {
					return nil, &graph.CreatedItemError{Item: item, Err: err}
				}
				return nil, &graph.CreatedItemError{Item: item, Err: refused, Removed: true}
			},
		},
		{
			name: "graph removed it and the action was canceled", canceled: true,
			answer: func(ctx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
				item := created()
				if err := onCreated(item); err != nil {
					return nil, &graph.CreatedItemError{Item: item, Err: err}
				}
				return nil, &graph.CreatedItemError{Item: item, Err: ctx.Err(), Removed: true}
			},
		},
		{
			name: "graph removed it before it was recorded",
			answer: func(context.Context, graph.CreatedFunc) (*graph.Item, error) {
				return nil, &graph.CreatedItemError{Item: created(), Err: refused, Removed: true}
			},
		},
		{
			name: "the transfer manager removed it after a final fragment that did not land",
			answer: func(_ context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
				item := created()
				if err := onCreated(item); err != nil {
					return nil, &graph.CreatedItemError{Item: item, Err: err}
				}
				return nil, &graph.CreatedItemError{Item: item, Err: &graph.FinalFragmentError{Err: errors.New(scenarioConnectionReset)}}
			},
		},
		{
			name: "left in place", wantKept: true,
			answer: func(_ context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
				item := created()
				if err := onCreated(item); err != nil {
					return nil, &graph.CreatedItemError{Item: item, Err: err}
				}
				return nil, &graph.CreatedItemError{Item: item, Err: refused}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			actionCtx, cancelAction := context.WithCancel(t.Context())
			defer cancelAction()
			ul := &creatingTestUploader{
				create: func(ctx context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
					if tc.canceled {
						cancelAction()
					}
					return tc.answer(ctx, onCreated)
				},
				getItem: func(itemID string) (*graph.Item, error) {
					// Still empty: the content did not land.
					return &graph.Item{ID: itemID, ETag: `"c,2"`, QuickXorHash: emptyQuickXor}, nil
				},
			}
			_, logger := newDebugLogBuffer()
			e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
			local := writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

			_, uploadErr := e.uploadNewFile(actionCtx, &Action{Type: ActionUpload, Path: "doc.txt"},
				driveid.New(testDriveID), graphRootID, "doc.txt", local)
			require.Error(t, uploadErr)

			pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			require.Equal(t, tc.wantKept, found)
			if tc.wantKept {
				assert.Equal(t, "new-item", pending.ItemID)
			}
		})
	}
}

// Validates: R-5.6.15, R-2.4.16
//
// An observation of a pending item whose size Graph did not report arrives as
// size 0. Read as "still empty", it was dropped as a stale echo whenever the
// upload into the item had committed: a remote change swallowed because a
// record existed. It is observed like any item, and the record stays.
func TestCommitObservation_UnknownSizeOfAPendingItemIsNotAStaleEcho(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	placeholder := adoptTestPlaceholder(t, store)
	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action: ActionUpload, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: testPlaceholderID,
		ParentID: graphRootID, ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 7, LocalSizeKnown: true, RemoteSize: 7, RemoteSizeKnown: true, ETag: placeholderMovedETag,
	}))

	sizeless := placeholder
	sizeless.Hash, sizeless.ETag, sizeless.SizeUnknown = "", placeholderLaterETag, true
	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{sizeless}, "t-sizeless", driveID))

	row := readRemoteStateRow(t, store.rawDB(), testPlaceholderID)
	require.NotNil(t, row)
	assert.Equal(t, placeholderLaterETag, row.ETag, "the observation is applied like any item's")
	assert.False(t, row.SizeKnown)
	_, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	assert.True(t, found, "an observation that cannot say whether the item is empty ends nothing")
}

// Validates: R-5.6.15, R-2.4.16
//
// A pending item of unknown size that the baseline does not hold yet was
// adopted as synced empty content on both sides: the next upload by ID could
// then overwrite content the item may hold by now. It is observed like any
// item, nothing is adopted, and the record stays for an observation that
// knows the size: a record naming the item, and an intent at its path, which
// such an observation neither claims nor drops (it may be the file of the
// intent's create). The next observation of the empty item claims and adopts.
func TestCommitObservation_UnknownSizeOfAPendingItemIsNotAdopted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		intentOnly   bool
		wantRecorded string
	}{
		{name: "a record naming the item", wantRecorded: testPlaceholderID},
		{name: "an intent at its path", intentOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			driveID := driveid.New(testDriveID)
			pending := &PendingCreate{DriveID: driveID, Path: "doc.txt", ParentID: graphRootID}
			if tc.intentOnly {
				require.NoError(t, store.RecordPendingCreateIntent(t.Context(), pending))
			} else {
				pending.ItemID, pending.CreatedETag = testPlaceholderID, placeholderCreatedETag
				require.NoError(t, store.RecordPendingCreate(t.Context(), pending))
			}
			observed := ObservedItem{
				DriveID: driveID, ItemID: testPlaceholderID, ParentID: graphRootID, Path: "doc.txt", ItemType: ItemTypeFile,
				ETag: placeholderMovedETag, SizeUnknown: true,
			}

			require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{observed}, "t1", driveID))

			_, _, adopted := readBaselineRow(t, store, testPlaceholderID)
			assert.False(t, adopted, "nothing is adopted on a size nobody knows")
			require.NotNil(t, readRemoteStateRow(t, store.rawDB(), testPlaceholderID), "the item is observed like any other")
			kept, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			require.True(t, found, "the record stays")
			assert.Equal(t, tc.wantRecorded, kept.ItemID)

			observed.SizeUnknown, observed.Hash, observed.ETag = false, emptyQuickXor, placeholderLaterETag
			require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{observed}, "t2", driveID))
			_, _, adopted = readBaselineRow(t, store, testPlaceholderID)
			assert.True(t, adopted, "known to be empty, the placeholder is adopted")
		})
	}
}

// Validates: R-5.6.15
//
// An adopted placeholder renamed online while still empty kept its record at
// the old path. A later create at the old path that met 409 then found the
// record and uploaded into the item at its new path. The record follows the
// item instead. It is not dropped: an upload by ID into the item may already
// be on its way (the eTag it is conditional on is checked when its session
// opens), and once that content commits, a window captured before it shows
// the empty item again, which only the record tells from a remote change.
func TestCommitObservation_AdoptedPlaceholderRenamedWhileEmptyTakesItsRecordAlong(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	placeholder := adoptTestPlaceholder(t, store)

	renamed := placeholder
	renamed.Path, renamed.ETag = renamedPath, placeholderMovedETag
	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{renamed}, "t-renamed", driveID))

	requireNoPendingCreateAt(t, store, "doc.txt", "a create at the old path finds no placeholder of its own")
	pending, found, err := store.PendingCreateAt(t.Context(), renamedPath)
	require.NoError(t, err)
	require.True(t, found, "the record follows the item")
	require.Equal(t, testPlaceholderID, pending.ItemID)

	// The upload by ID that was on its way commits its content at the path it
	// was planned for, and the echo of the renamed empty item arrives after it.
	require.NoError(t, store.CommitMutation(t.Context(), &BaselineMutation{
		Action: ActionUpload, Success: true, Path: "doc.txt", DriveID: driveID, ItemID: testPlaceholderID,
		ParentID: graphRootID, ItemType: ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalSize: 7, LocalSizeKnown: true, RemoteSize: 7, RemoteSizeKnown: true, ETag: placeholderLaterETag,
	}))
	echo := renamed
	echo.ETag = `"p,2b"`
	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{echo}, "t-echo", driveID))
	row := readRemoteStateRow(t, store.rawDB(), testPlaceholderID)
	require.NotNil(t, row)
	assert.Equal(t, int64(7), row.Size, "the echo of the empty item is not a remote change")
}

// Validates: R-5.6.15
//
// A placeholder renamed or moved online before observation adopted it is not
// the create its record describes any more: the record ends and the item is
// observed like any other, at its new path, without being adopted.
func TestCommitObservation_PlaceholderMovedBeforeItWasAdoptedLosesItsRecord(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	driveID := driveid.New(testDriveID)
	require.NoError(t, store.RecordPendingCreate(t.Context(), &PendingCreate{
		ItemID: testPlaceholderID, DriveID: driveID, Path: "doc.txt", ParentID: graphRootID, CreatedETag: placeholderCreatedETag,
	}))

	require.NoError(t, store.CommitObservation(t.Context(), []ObservedItem{{
		DriveID: driveID, ItemID: testPlaceholderID, ParentID: graphRootID, Path: renamedPath, ItemType: ItemTypeFile,
		Hash: emptyQuickXor, Size: 0, ETag: placeholderMovedETag,
	}}, "t1", driveID))

	requireNoPendingCreateAt(t, store, "doc.txt", "the record of a create at doc.txt ends")
	requireNoPendingCreateAt(t, store, renamedPath, "and does not follow an item it never adopted")
	_, _, adopted := readBaselineRow(t, store, testPlaceholderID)
	assert.False(t, adopted)
	row := readRemoteStateRow(t, store.rawDB(), testPlaceholderID)
	require.NotNil(t, row)
	assert.Equal(t, renamedPath, row.Path)
}

// Validates: R-5.6.15, R-5.5.2
//
// An upload that completed without returning its item is looked up by the
// created item's ID. A lookup that answered 404 came back without the create
// mark, and the executor read it as the create's own 404: the parent folder
// gone (remoteCreateParentGone). The failure keeps the mark, so it is an
// unknown outcome of this create, and the record of the created item stays.
func TestExecuteUpload_LookupFailureAfterACreateIsNotAGoneParent(t *testing.T) {
	t.Parallel()

	ul := &creatingTestUploader{
		create: func(_ context.Context, onCreated graph.CreatedFunc) (*graph.Item, error) {
			item := &graph.Item{ID: "new-item", ETag: `"c,1"`}
			if err := onCreated(item); err != nil {
				return nil, &graph.CreatedItemError{Item: item, Err: err}
			}
			return nil, &graph.CreatedItemError{
				Item: item, Err: fmt.Errorf("graph: uploading chunk at offset 0: %w", graph.ErrUploadCompletedWithoutItem),
			}
		},
		getItem: func(string) (*graph.Item, error) { return nil, graph.ErrNotFound },
	}
	_, logger := newDebugLogBuffer()
	e, store, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")

	outcome := e.ExecuteUpload(t.Context(), newFileUploadAction("doc.txt"))

	requireOutcomeFailure(t, &outcome)
	var gone *RemoteItemGoneError
	require.NotErrorAs(t, outcome.Error, &gone, "a failed lookup of the created item says nothing about its parent")
	var created *graph.CreatedItemError
	require.ErrorAs(t, outcome.Error, &created)
	assert.Equal(t, "new-item", created.Item.ID)
	pending, found, err := store.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the outcome is unknown: the created item may hold the content")
	assert.Equal(t, "new-item", pending.ItemID)
}

// Validates: R-5.6.15, R-5.5.2
//
// The fault end to end: the 404 from the lookup of the created item reached
// the engine as the parent proven gone, and the parent folder and its subtree
// were forgotten from remote truth, so the plan that followed read the folder
// as deleted on OneDrive. The folder stays in remote truth, the attempt fails
// as an unknown outcome, and nothing is deleted.
func TestRunOnce_LookupFailureAfterACreateKeepsItsParentInRemoteTruth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("folder-docs", "root", "docs")
	remote.addFile(t, "item-keep", "folder-docs", "keep.txt", "keep\n")
	client := remote.client(t)
	client.uploadCreatingFn = func(
		_ context.Context, _, _ string, _ io.ReaderAt, _ int64, onCreated graph.CreatedFunc,
	) (*graph.Item, error) {
		// The content went up and the completion carried no item, and the
		// item cannot be read back: OneDrive does not list it.
		item := &graph.Item{ID: "unreadable-item", ETag: `"u,1"`}
		if err := onCreated(item); err != nil {
			return nil, &graph.CreatedItemError{Item: item, Err: err}
		}
		return nil, &graph.CreatedItemError{
			Item: item, Err: fmt.Errorf("graph: uploading chunk at offset 0: %w", graph.ErrUploadCompletedWithoutItem),
		}
	}
	eng, syncRoot := newTestEngine(t, client)
	runScenarioPass(t, eng)
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "docs", "new.txt"), []byte("my words\n"), 0o600))

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	assert.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Zero(t, report.Superseded, "a failed lookup proves nothing about the plan")
	assert.Zero(t, report.LocalDeletes+report.HeldLocalDeletes)
	require.NotNil(t, readRemoteStateRow(t, eng.baseline.rawDB(), "folder-docs"), "the parent folder stays in remote truth")
	require.NotNil(t, readRemoteStateRow(t, eng.baseline.rawDB(), "item-keep"), "and so does its subtree")
	assert.FileExists(t, filepath.Join(syncRoot, "docs", "keep.txt"))
}

// finishIntoPlaceholderCase is an item a refused create reads back by the ID
// its record names, and whether the create finishes into it.
type finishIntoPlaceholderCase struct {
	name       string
	path       string // the create's path
	parentID   string // the create's parent, "root" for the drive root
	item       graph.Item
	wantFinish bool
}

// Validates: R-5.6.15
//
// A placeholder is finished only while it is an empty live file at the
// create's own path: a folder, a deleted item, a package or an item of
// unknown size also reads as size 0 or below, and an item renamed or moved
// online (before observation took its record along) is no longer where the
// create puts the user's file. The upload by ID would write the user's file
// into something that is not this create's placeholder; nothing is sent.
// The drive root's alias is resolved through the item's parent path.
func TestFinishIntoPlaceholder_RefusesAnItemThatIsNotAnEmptyLiveFile(t *testing.T) {
	t.Parallel()

	inDocs := func(item graph.Item) graph.Item {
		item.ID, item.Name, item.ParentID = testPlaceholderID, "doc.txt", "folder-docs"
		return item
	}
	atRoot := func(name, parentID, parentPath string) graph.Item {
		return graph.Item{
			ID: testPlaceholderID, Name: name, ParentID: parentID, ParentPath: parentPath, ParentPathKnown: true,
			ETag: `"r,1"`,
		}
	}
	for _, tc := range []finishIntoPlaceholderCase{
		{name: "folder", path: "docs/doc.txt", parentID: "folder-docs", item: inDocs(graph.Item{ETag: `"f,1"`, IsFolder: true})},
		{name: "deleted", path: "docs/doc.txt", parentID: "folder-docs", item: inDocs(graph.Item{ETag: `"d,1"`, IsDeleted: true})},
		{name: "package", path: "docs/doc.txt", parentID: "folder-docs", item: inDocs(graph.Item{ETag: `"k,1"`, IsPackage: true})},
		{name: "unknown size", path: "docs/doc.txt", parentID: "folder-docs", item: inDocs(graph.Item{ETag: `"u,1"`, Size: -1})},
		{
			name: "renamed online", path: "docs/doc.txt", parentID: "folder-docs",
			item: graph.Item{ID: testPlaceholderID, Name: renamedPath, ParentID: "folder-docs", ETag: `"n,1"`},
		},
		{
			name: "moved to another folder", path: "docs/doc.txt", parentID: "folder-docs",
			item: graph.Item{ID: testPlaceholderID, Name: "doc.txt", ParentID: "folder-other", ETag: `"m,1"`},
		},
		{name: "moved out of the drive root", path: "doc.txt", parentID: graphRootID, item: atRoot("doc.txt", "folder-other", "other")},
		{name: "still in its folder", path: "docs/doc.txt", parentID: "folder-docs", item: inDocs(graph.Item{ETag: `"s,1"`}), wantFinish: true},
		{name: "still at the drive root", path: "doc.txt", parentID: graphRootID, item: atRoot("doc.txt", "root-id", ""), wantFinish: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			finished, uploads := runFinishIntoPlaceholderCase(t, &tc)

			assert.Equal(t, tc.wantFinish, finished)
			if !tc.wantFinish {
				assert.Zero(t, uploads, "nothing is written into an item that is not this create's placeholder")
			}
		})
	}
}

// runFinishIntoPlaceholderCase reads tc's item back as the placeholder of a
// create at tc's path and reports whether the create finished into it and
// how many uploads by ID it sent.
func runFinishIntoPlaceholderCase(t *testing.T, tc *finishIntoPlaceholderCase) (finished bool, uploads int) {
	t.Helper()

	ul := &creatingTestUploader{}
	ul.uploadToItemFn = func(
		context.Context, driveid.ID, string, string, io.ReaderAt, int64, time.Time, graph.ProgressFunc,
	) (*graph.Item, error) {
		uploads++
		return &graph.Item{ID: testPlaceholderID}, nil
	}
	_, logger := newDebugLogBuffer()
	e, _, syncRoot := newPendingCreateTestExecutor(t, ul, logger)
	e.items = &executorMockItemClient{getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
		item := tc.item
		return &item, nil
	}}
	local := writeExecTestFile(t, syncRoot, tc.path, "my words\n")

	_, finished, err := e.finishIntoPlaceholder(t.Context(), &Action{Type: ActionUpload, Path: tc.path},
		driveid.New(testDriveID), tc.parentID, testPlaceholderID, tc.path, local)
	require.NoError(t, err)

	return finished, uploads
}

// Validates: R-2.8.7, R-5.6.15
//
// The read of the placeholder a refused create would finish into is a read
// made while resolving the 409, and one that fails is that read's failure,
// classified by its own answer (an outage, a throttle). Taken for "not my
// placeholder", it fell through to the folder listing and came back as a
// stale plan under the 409's status. A placeholder that is gone (404) is
// still no placeholder: the listing decides.
func TestFinishIntoPlaceholder_AReadThatFailsIsThatReadsFailure(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(testDriveID)
	newExecutor := func(t *testing.T, readErr error) (*Executor, string) {
		t.Helper()

		_, logger := newDebugLogBuffer()
		e, _, syncRoot := newPendingCreateTestExecutor(t, &creatingTestUploader{}, logger)
		e.items = &executorMockItemClient{getItemFn: func(context.Context, driveid.ID, string) (*graph.Item, error) {
			return nil, readErr
		}}

		return e, writeExecTestFile(t, syncRoot, "doc.txt", "my words\n")
	}

	for _, read := range followUpReadFailures() {
		t.Run(read.name, func(t *testing.T) {
			t.Parallel()

			e, local := newExecutor(t, read.err)
			_, finished, err := e.finishIntoPlaceholder(t.Context(), &Action{Type: ActionUpload, Path: "doc.txt"},
				driveID, graphRootID, testPlaceholderID, "doc.txt", local)

			assert.True(t, finished, "the failed read settles the attempt")
			requireClassifiedAsTheRead(t, read, ActionUpload, driveID, uploadTransferError(err))
		})
	}

	t.Run("gone", func(t *testing.T) {
		t.Parallel()

		e, local := newExecutor(t, graph.ErrNotFound)
		_, finished, err := e.finishIntoPlaceholder(t.Context(), &Action{Type: ActionUpload, Path: "doc.txt"},
			driveID, graphRootID, testPlaceholderID, "doc.txt", local)

		require.NoError(t, err)
		assert.False(t, finished, "a placeholder that is gone is no placeholder")
	})
}

// dispatchTestCreate queues the create of the new file doc.txt as action id
// and lets rt's loop dispatch it to a worker.
func dispatchTestCreate(t *testing.T, rt *watchRuntime, id int64) *TrackedAction {
	t.Helper()

	const path = "doc.txt"
	return dispatchTestAction(t, rt, id, &Action{
		Type: ActionUpload, Path: path, DriveID: rt.engine.driveID, View: &PathView{Path: path},
	})
}

// dispatchTestAction queues action as action id and lets rt's loop dispatch
// it to a worker, which leaves it running.
func dispatchTestAction(t *testing.T, rt *watchRuntime, id int64, action *Action) *TrackedAction {
	t.Helper()

	queued := rt.sched.graph.Add(action, id, nil)
	require.NotNil(t, queued)
	rt.sched.markQueued(queued)
	rt.replaceOutbox([]*TrackedAction{queued})
	_, err := rt.runNonDrainingWatchStep(t.Context(), &watchPipeline{runtime: rt}, nil)
	require.NoError(t, err)
	select {
	case dispatched := <-rt.sched.dispatchCh:
		require.Equal(t, queued.ID, dispatched.ID)
	default:
		require.Fail(t, "the create was not dispatched")
	}

	return queued
}

// failTestCreate completes the dispatched create as one whose answer was
// lost, a second after clock's now, and returns when it ended.
func failTestCreate(t *testing.T, rt *watchRuntime, bl *Baseline, clock *manualClock, create *TrackedAction) time.Time {
	t.Helper()

	clock.Advance(time.Second)
	endedAt := clock.Now()
	require.NoError(t, rt.handleWatchActionCompletion(t.Context(), &watchPipeline{bl: bl}, &ActionCompletion{
		Path: create.Action.Path, DriveID: rt.engine.driveID, ActionType: ActionUpload, ActionID: create.ID,
		Err: scenarioCreateAnswerLost(filepath.Base(create.Action.Path)), ErrMsg: "create answer lost",
	}))

	return endedAt
}

// pollWatchTest is a watch runtime past one synced pass, with keep.txt on
// both sides and a fresh intent at doc.txt whose file the user removed.
type pollWatchTest struct {
	remote *moveScenarioRemote
	eng    *testEngine
	rt     *watchRuntime
	bl     *Baseline
	clock  *manualClock
}

func newPollWatchTest(t *testing.T) *pollWatchTest {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, _ := newTestEngine(t, remote.client(t))
	clock := newManualClock(time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	runScenarioPass(t, eng)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = NewDirtyBuffer(eng.logger)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	require.NoError(t, eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: remote.driveID, Path: "doc.txt", ParentID: "root",
	}))

	return &pollWatchTest{remote: remote, eng: eng, rt: rt, bl: bl, clock: clock}
}

// poll applies a batch whose poll began at startedAt, the kind a poll that
// returned no items sends or, withEvents, one that carries another change,
// and returns the store transactions it cost.
func (w *pollWatchTest) poll(t *testing.T, startedAt time.Time, withEvents bool) int {
	t.Helper()

	batch := RemoteObservationBatch{Source: RemoteObservationBatchPrimaryWatch, PollStartedAt: startedAt, PollOnly: true}
	if withEvents {
		keep, ok := w.remote.itemAt("keep.txt")
		require.True(t, ok)
		batch = RemoteObservationBatch{
			Source: RemoteObservationBatchPrimaryWatch, PollStartedAt: startedAt,
			Observed: []ObservedItem{{
				DriveID: w.remote.driveID, ItemID: keep.ID, ParentID: "root", Path: "keep.txt",
				ItemType: ItemTypeFile, Hash: keep.QuickXorHash, Size: keep.Size,
				Mtime: keep.ModifiedAt.UnixNano(), ETag: `"keep-touched"`,
			}},
		}
	}
	collector := perf.NewCollector(nil)
	require.NoError(t, w.rt.handleRemoteObservationBatch(perf.WithCollector(t.Context(), collector), &batch))

	return collector.Snapshot().DBTransactionCount
}

func (w *pollWatchTest) intentKept(t *testing.T) bool {
	t.Helper()

	pending, found, err := w.eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, !found || pending.ItemID == "", "the row stays an intent")

	return found
}

// Validates: R-5.6.15
//
// A watch replan observes the local tree only, so the fresh intent of a
// create whose file the user removed stayed until the next start or the
// 72-hour expiry, and in that window an empty file another device made at the
// path was claimed and then deleted. Once a remote poll that began after the
// last create attempt at the path ended has been applied without claiming
// anything there, every create of that attempt that landed has been seen, and
// the intent is dropped: after a poll that returned no items as after one
// with other changes. A poll that returned no items costs no replan and no
// store write unless it drops an intent. Only the intent of the attempt is
// judged: one at a path no attempt of this process ended at stays for the
// start pass or its TTL.
func TestWatch_FreshIntentOfAGoneFileIsDroppedAfterTheNextRemotePoll(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		withEvents bool
	}{
		{name: "zero-event poll"},
		{name: "poll with other events", withEvents: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newPollWatchTest(t)
			require.NoError(t, w.eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
				DriveID: w.remote.driveID, Path: "elsewhere.txt", ParentID: "root",
			}))
			endedAt := failTestCreate(t, w.rt, w.bl, w.clock, dispatchTestCreate(t, w.rt, 1))
			before := pendingCreateCount(t, w.eng.baseline)

			transactions := w.poll(t, endedAt.Add(time.Second), tc.withEvents)

			assert.False(t, w.intentKept(t), "dropped once a poll that began after the attempt was applied")
			assert.Equal(t, before-1, pendingCreateCount(t, w.eng.baseline), "exactly one record is dropped")
			_, elsewhere, err := w.eng.baseline.PendingCreateAt(t.Context(), "elsewhere.txt")
			require.NoError(t, err)
			assert.True(t, elsewhere, "an intent no attempt of this process ended at is not judged")
			assert.Empty(t, w.rt.loop.createAttempts.endedAt, "the dropped path is forgotten")
			if !tc.withEvents {
				assert.Positive(t, transactions, "the drop is the poll's one store write")
				assert.Nil(t, w.rt.resources.dirtyBuf.FlushImmediate(), "and it replans nothing")
			}
		})
	}
}

// Validates: R-5.6.15
//
// A poll that began after the attempt ended judges its intent, and keeps it
// while the local tree still holds the file: its create may still be retried.
// The path stays in memory, so a later poll judges it again and drops the
// intent once the file is gone.
func TestWatch_PollKeepsTheIntentOfAFileThatIsStillThere(t *testing.T) {
	t.Parallel()

	w := newPollWatchTest(t)
	require.NoError(t, w.eng.baseline.UpsertLocalStateRows(t.Context(), []LocalStateRow{{
		Path: "doc.txt", ItemType: ItemTypeFile, Hash: "my-hash", Size: 9,
	}}))
	endedAt := failTestCreate(t, w.rt, w.bl, w.clock, dispatchTestCreate(t, w.rt, 1))

	assert.Positive(t, w.poll(t, endedAt.Add(time.Second), false), "the intent is judged")
	assert.True(t, w.intentKept(t), "and kept: the file is still there")
	assert.Contains(t, w.rt.loop.createAttempts.endedAt, "doc.txt", "its path stays for a later poll")

	require.NoError(t, w.eng.baseline.DeleteLocalStatePath(t.Context(), "doc.txt"))
	w.poll(t, endedAt.Add(2*time.Second), false)
	assert.False(t, w.intentKept(t), "a later poll drops it once the file is gone")
	assert.Empty(t, w.rt.loop.createAttempts.endedAt)
}

// Validates: R-5.6.15
//
// The step after planning drops the record of an adopted placeholder whose
// local file is empty too. In watch mode it runs with a replan, and a replan
// waits until no action is running: an upload by ID into the placeholder
// still on its way while the local file reads empty keeps the record its
// stale echo needs until the upload's outcome is committed, which ends or
// keeps the record by what it committed.
func TestWatch_ReplanWaitsWhileAnUploadRunsBeforeItPrunesPendingCreates(t *testing.T) {
	t.Parallel()

	w := newPollWatchTest(t)
	require.NoError(t, w.eng.baseline.ClearPendingCreateIntent(t.Context(), "doc.txt"))
	adoptTestPlaceholder(t, w.eng.baseline)
	require.NoError(t, w.eng.baseline.UpsertLocalStateRows(t.Context(), []LocalStateRow{{
		Path: "doc.txt", ItemType: ItemTypeFile, Hash: emptyQuickXor, Size: 0,
	}}))
	upload := dispatchTestAction(t, w.rt, 1, &Action{
		Type: ActionUpload, Path: "doc.txt", DriveID: w.rt.engine.driveID, ItemID: testPlaceholderID,
		View: &PathView{Path: "doc.txt"},
	})

	w.rt.queuePendingReplan(DirtyBatch{})
	replanned, err := w.rt.runPendingWatchReplan(t.Context(), &watchPipeline{runtime: w.rt, bl: w.bl})
	require.NoError(t, err)
	assert.False(t, replanned, "no replan, so no prune, while the upload runs")
	assert.True(t, w.rt.loop.hasPendingReplan, "the replan waits for the upload")
	pending, found, err := w.eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
	require.NoError(t, err)
	require.True(t, found, "the record stays while the upload runs")
	assert.Equal(t, testPlaceholderID, pending.ItemID)

	// The record was exposed: the prune a replan runs once nothing is
	// running drops it.
	w.rt.sched.markFinished(upload)
	require.True(t, w.rt.canPrepareNow())
	require.NoError(t, w.eng.baseline.PrunePendingCreates(t.Context(), PassRefreshedLocalOnly, w.eng.localScanCoverage()))
	requireNoPendingCreateAt(t, w.eng.baseline, "doc.txt", "the settled empty placeholder's record goes with the next replan")
}

// Validates: R-5.6.15, R-2.8.5
//
// A mount-root engine's watch polls its folder on its own goroutine. Its
// batches carry when their poll began, as the primary watch's do, so the
// fresh intent of a create whose file the user removed is dropped there too;
// unstamped, a create attempt in a mount-root engine never ended. A poll
// that saw no events hands over a batch as well (its findings clear the
// read-denied issue), and ends the attempt the same way.
func TestWatch_MountRootPollDropsTheFreshIntentOfAGoneFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		withEvents bool
	}{
		{name: "zero-event poll"},
		{name: "poll with events", withEvents: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := newMountRootWatchTest(t, tc.withEvents)
			endedAt := failTestCreate(t, w.rt, w.bl, w.clock, dispatchTestCreate(t, w.rt, 1))
			w.clock.Advance(time.Second)
			polledAt := w.clock.Now()

			batches := make(chan RemoteObservationBatch)
			cancel, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
				return w.rt.watchMountRootRemote(ctx, w.bl, batches, MinPollInterval)
			})
			batch := receiveWithin(t, batches, "the mount-root poll's batch")

			assert.Equal(t, RemoteObservationBatchMountRoot, batch.Source)
			assert.Equal(t, polledAt, batch.PollStartedAt, "stamped when the poll began")
			assert.True(t, batch.PollStartedAt.After(endedAt))
			assert.NotNil(t, batch.ApplyAck, "a whole batch, whose apply the poll waits for")
			require.NoError(t, w.rt.handleRemoteObservationBatch(t.Context(), &batch))
			stopWatch(t, cancel, done)

			_, found, err := w.eng.baseline.PendingCreateAt(t.Context(), "doc.txt")
			require.NoError(t, err)
			assert.False(t, found, "dropped once the poll that began after the attempt was applied")
		})
	}
}

// mountRootWatchTest is a mount-root engine's watch runtime past one synced
// pass of an empty folder, with a fresh intent at doc.txt whose file the
// user removed.
type mountRootWatchTest struct {
	eng   *testEngine
	rt    *watchRuntime
	bl    *Baseline
	clock *manualClock
}

// newMountRootWatchTest builds a mountRootWatchTest whose folder's polls
// after the first report a new folder when withEvents, and nothing otherwise.
func newMountRootWatchTest(t *testing.T, withEvents bool) *mountRootWatchTest {
	t.Helper()

	const mountRoot = "mount-root-root"
	driveID := driveid.New(engineTestDriveID)
	var polls atomic.Int32
	mock := &engineMockClient{
		folderDeltaFn: func(_ context.Context, _ driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			polls.Add(1)
			assert.Equal(t, mountRoot, folderID)
			if token == "" || !withEvents {
				return nil, folderDeltaToken, nil
			}
			return []graph.Item{{
				ID: "folder-new", Name: "new", ParentID: mountRoot, DriveID: driveID, IsFolder: true, ETag: `"n,1"`,
			}}, folderDeltaToken, nil
		},
	}
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.RemoteRootItemID = mountRoot
		inputs.RemoteRootDeltaCapable = true
	})
	clock := newManualClock(time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	runScenarioPass(t, eng)
	require.Positive(t, polls.Load(), "the pass read the folder")
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	rt.resources.dirtyBuf = NewDirtyBuffer(eng.logger)
	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	require.NoError(t, eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveID, Path: "doc.txt", ParentID: mountRoot,
	}))

	return &mountRootWatchTest{eng: eng, rt: rt, bl: bl, clock: clock}
}

// pendingCreateCount is the number of pending-create rows in store.
func pendingCreateCount(t *testing.T, store *SyncStore) int {
	t.Helper()

	var rows int
	require.NoError(t, store.rawDB().QueryRowContext(t.Context(), `SELECT count(*) FROM pending_creates`).Scan(&rows))

	return rows
}

// Validates: R-5.6.15
//
// A poll proves the create of an attempt was seen only when it began after
// the attempt ended and while no other create at the path was running: one
// that began earlier, or while a retry was in flight, may have missed it. Such
// a poll keeps the intent, and costs no store write and no replan; the next
// poll that began after the last attempt ended drops it.
func TestWatch_PollThatMayHaveMissedTheCreateKeepsTheIntent(t *testing.T) {
	t.Parallel()

	t.Run("began before the attempt ended", func(t *testing.T) {
		t.Parallel()

		w := newPollWatchTest(t)
		endedAt := failTestCreate(t, w.rt, w.bl, w.clock, dispatchTestCreate(t, w.rt, 1))

		assert.Zero(t, w.poll(t, endedAt.Add(-time.Second), false), "nothing to judge: no store access")
		assert.Nil(t, w.rt.resources.dirtyBuf.FlushImmediate(), "and no replan")
		assert.True(t, w.intentKept(t))

		w.poll(t, endedAt.Add(time.Second), false)
		assert.False(t, w.intentKept(t), "the next poll that began after the attempt drops it")
	})

	t.Run("another create at the path in flight", func(t *testing.T) {
		t.Parallel()

		w := newPollWatchTest(t)
		failTestCreate(t, w.rt, w.bl, w.clock, dispatchTestCreate(t, w.rt, 1))
		retry := dispatchTestCreate(t, w.rt, 2)

		assert.Zero(t, w.poll(t, w.clock.Now().Add(time.Second), false), "nothing to judge: no store access")
		assert.True(t, w.intentKept(t))

		endedAt := failTestCreate(t, w.rt, w.bl, w.clock, retry)
		w.poll(t, endedAt.Add(time.Second), false)
		assert.False(t, w.intentKept(t), "dropped once the retry ended and a later poll was applied")
	})
}
